package homeassistant

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"

	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyaiohttpws"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const (
	platformName     = "homeassistant"
	maxMessageLength = 4096
	// Title of the persistent notification outbound replies are posted as
	notificationTitle = "Icebath Agent"
)

// Reconnect backoff in seconds, the last value repeating
var backoffSteps = []int{5, 10, 30, 60}

// Description template of each domain over the shared field set
var (
	turnedTemplate  = "[Home Assistant] {name}: turned {on_off}"
	domainTemplates = map[string]string{
		"climate": "[Home Assistant] {name}: HVAC mode changed from " +
			"'{old}' to '{new}' (current: {temp}, target: {target})",
		"sensor":              "[Home Assistant] {name}: changed from {old}{unit} to {new}{unit}",
		"binary_sensor":       "[Home Assistant] {name}: {new_trig} (was {old_trig})",
		"light":               turnedTemplate,
		"switch":              turnedTemplate,
		"fan":                 turnedTemplate,
		"alarm_control_panel": "[Home Assistant] {name}: alarm state changed from '{old}' to '{new}'",
	}
	defaultTemplate = "[Home Assistant] {name} ({entity_id}): changed from '{old}' to '{new}'"
)

// The HTTP and WebSocket clients are compiled in
func checkHARequirements() bool { return true }

// Whether the config token or the scoped HASS_TOKEN is non blank
func validateHAConfig(ctx context.Context, config *gateway.PlatformConfig) (bool, error) {
	var token any
	if config != nil {
		token = config.Token
	}
	if !pyrepr.Truthy(token) {
		scoped, err := gateway.GetScopedSecret(ctx, "HASS_TOKEN", "", false)
		if err != nil {
			return false, err
		}
		token = scoped
	}
	text, ok := token.(string)
	if !ok {
		return false, attrErr(token, "the Home Assistant token", "text")
	}
	return pystr.Strip(text) != "", nil
}

// Domain of an entity id, empty without a dot, the TypeError of a value that is not a str
func domainOf(entityID any) (string, error) {
	id, ok := entityID.(string)
	if !ok {
		return "", &pyErr{"TypeError", fmt.Sprintf("an entity id must be text, got %s", errchain.Kind(entityID))}
	}
	if !strings.Contains(id, ".") {
		return "", nil
	}
	head, _, _ := strings.Cut(id, ".")
	return head, nil
}

// Bearer and JSON content headers of a token
func authHeaders(token any) map[string]string {
	return map[string]string{"Authorization": "Bearer " + pyrepr.Str(token), "Content-Type": "application/json"}
}

// Error text with the macOS Local Network Privacy remedy for a launchd gateway the LAN refused
func connectErrorDetail(err error) string {
	text := err.Error()
	if runtime.GOOS == "darwin" && gateway.IsSupervisedGatewayLaunch(nil) && errors.Is(err, syscall.EHOSTUNREACH) {
		return text + " — macOS Local Network Privacy is blocking this launchd gateway from the LAN. " +
			"Run `icebath gateway install` to regenerate the launchd job, then `icebath gateway restart`. " +
			"The regenerated job starts the gateway through /usr/bin/osascript, an Apple platform binary whose " +
			"child processes macOS lets reach the LAN."
	}
	return text
}

// Gateway adapter turning state_changed events into messages and replies into persistent notifications
type HomeAssistantAdapter struct {
	*gateway.BasePlatformAdapter

	hassURL         string
	hassToken       any
	watchDomains    pySet
	watchEntities   pySet
	ignoreEntities  pySet
	watchAll        bool
	cooldownSeconds *big.Int

	mu            sync.Mutex
	msgID         int64
	ws            *pyaiohttpws.Conn
	restSession   *http.Client
	listenTask    *gateway.AdapterTask
	lastEventTime keyedMap
}

// Class attributes the adapter declares over the base defaults
func adapterTraits() gateway.AdapterTraits {
	traits := gateway.DefaultAdapterTraits()
	traits.MaxMessageLength = maxMessageLength
	return traits
}

// Extra value of a key with the default for a missing key, the dict get of Python
func extraOr(extra map[string]any, key string, def any) any {
	if v, ok := extra[key]; ok {
		return v
	}
	return def
}

// Adapter reading URL, token, filters and cooldown from the config extra and the scoped env
func NewHomeAssistantAdapter(ctx context.Context, config *gateway.PlatformConfig) (*HomeAssistantAdapter, error) {
	if config == nil {
		config = gateway.NewPlatformConfig()
	}
	a := &HomeAssistantAdapter{lastEventTime: keyedMap{}}
	a.BasePlatformAdapter = gateway.NewBasePlatformAdapter(config, gateway.Platform(platformName), a)
	a.Traits = adapterTraits()
	extra := config.Extra
	if extra == nil {
		extra = map[string]any{}
	}
	var rawURL any = extra["url"]
	if !pyrepr.Truthy(rawURL) {
		scoped, err := gateway.GetScopedSecret(ctx, "HASS_URL", defaultHassURL, false)
		if err != nil {
			return nil, err
		}
		rawURL = scoped
	}
	urlText, ok := rawURL.(string)
	if !ok {
		return nil, attrErr(rawURL, "the Home Assistant url", "text")
	}
	a.hassURL = pystr.RstripChars(urlText, "/")
	a.hassToken = config.Token
	if !pyrepr.Truthy(a.hassToken) {
		scoped, err := gateway.GetScopedSecret(ctx, "HASS_TOKEN", "", false)
		if err != nil {
			return nil, err
		}
		a.hassToken = scoped
	}
	var err error
	if a.watchDomains, err = newPySet(extraOr(extra, "watch_domains", []any{})); err != nil {
		return nil, err
	}
	if a.watchEntities, err = newPySet(extraOr(extra, "watch_entities", []any{})); err != nil {
		return nil, err
	}
	if a.ignoreEntities, err = newPySet(extraOr(extra, "ignore_entities", []any{})); err != nil {
		return nil, err
	}
	a.watchAll = pyrepr.Truthy(extraOr(extra, "watch_all", false))
	if a.cooldownSeconds, err = pyIntOf(extraOr(extra, "cooldown_seconds", 30)); err != nil {
		return nil, err
	}
	return a, nil
}

// Next WebSocket command id, counting across reconnects
func (a *HomeAssistantAdapter) nextID() int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.msgID++
	return a.msgID
}

// Validates the token and URL, opens the event subscription and starts the listener with its REST session
func (a *HomeAssistantAdapter) Connect(ctx context.Context, isReconnect bool) (bool, error) {
	if !pyrepr.Truthy(a.hassToken) {
		pkgLog().Warn(fmt.Sprintf("[%s] No HASS_TOKEN configured", a.Name()))
		return false, nil
	}
	if problem := urlSchemeProblem(a.hassURL); problem != "" {
		pkgLog().Error(fmt.Sprintf("[%s] %s", a.Name(), problem))
		a.SetFatalError(ctx, "homeassistant_url_scheme", problem, false)
		return false, nil
	}
	ok, err := a.wsConnect(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		pkgLog().Error(fmt.Sprintf("[%s] Failed to connect: %s", a.Name(), connectErrorDetail(err)))
		return false, nil
	}
	if !ok {
		return false, nil
	}
	session := newSession(gateway.GatewayTrustEnv(ctx))
	a.mu.Lock()
	a.restSession = session
	a.mu.Unlock()
	if !(a.watchDomains.truthy() || a.watchEntities.truthy() || a.watchAll) {
		pkgLog().Warn(fmt.Sprintf("[%s] No watch_domains, watch_entities, or watch_all configured. "+
			"All state_changed events will be dropped. Configure filters in "+
			"your HA platform config to receive events.", a.Name()))
	}
	a.SetRunning(true)
	task := gateway.SpawnAdapterTask(ctx, a.listenLoop)
	a.mu.Lock()
	a.listenTask = task
	a.mu.Unlock()
	pkgLog().Info(fmt.Sprintf("[%s] Connected to %s", a.Name(), a.hassURL))
	a.WirePluginHandlers(ctx, nil)
	return true, nil
}

// Opens the WebSocket, authenticates and subscribes to state_changed, false after a logged handshake failure
func (a *HomeAssistantAdapter) wsConnect(ctx context.Context) (bool, error) {
	wsURL := wsBaseURL(a.hassURL) + "/api/websocket"
	connectCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	connectCtx, handshake := withTLSTrace(connectCtx)
	conn, err := pyaiohttpws.Connect(connectCtx, wsURL, pyaiohttpws.Options{
		Heartbeat: 30 * time.Second, CloseTimeout: 30 * time.Second, TrustEnv: gateway.GatewayTrustEnv(ctx),
		MaxMsgSize: 4 * 1024 * 1024})
	timedOut := errors.Is(connectCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil
	cancel()
	if err != nil {
		if timedOut {
			return false, &timeoutError{}
		}
		if tlsErr := handshake.get(); tlsErr != nil && ctx.Err() == nil {
			return false, sslConnectError(wsURL, tlsErr)
		}
		return false, err
	}
	a.mu.Lock()
	a.ws = conn
	a.mu.Unlock()
	msg, err := receiveJSON(ctx, conn)
	if err != nil {
		return false, err
	}
	kind, err := pyGet(msg, "type", nil)
	if err != nil {
		return false, err
	}
	if !pyrepr.Equal(kind, "auth_required") {
		return a.handshakeFailed(ctx, fmt.Sprintf("Expected auth_required, got: %s", pyrepr.Str(kind)))
	}
	if err := sendJSON(conn, pyjson.ObjectOf("type", "auth", "access_token", a.hassToken)); err != nil {
		return false, err
	}
	if msg, err = receiveJSON(ctx, conn); err != nil {
		return false, err
	}
	if kind, err = pyGet(msg, "type", nil); err != nil {
		return false, err
	}
	if !pyrepr.Equal(kind, "auth_ok") {
		return a.handshakeFailed(ctx, "Auth failed: "+pyrepr.Str(msg))
	}
	if err := sendJSON(conn, pyjson.ObjectOf("id", a.nextID(), "type", "subscribe_events", "event_type", "state_changed")); err != nil {
		return false, err
	}
	if msg, err = receiveJSON(ctx, conn); err != nil {
		return false, err
	}
	success, err := pyGet(msg, "success", nil)
	if err != nil {
		return false, err
	}
	if !pyrepr.Truthy(success) {
		return a.handshakeFailed(ctx, "Failed to subscribe to events: "+pyrepr.Str(msg))
	}
	return true, nil
}

// Logs the handshake failure, closes the WebSocket and reports the connect as failed
func (a *HomeAssistantAdapter) handshakeFailed(ctx context.Context, message string) (bool, error) {
	pkgLog().Error(message)
	a.cleanupWS(ctx)
	return false, nil
}

// Closes and forgets the WebSocket
func (a *HomeAssistantAdapter) cleanupWS(ctx context.Context) {
	a.mu.Lock()
	conn := a.ws
	a.ws = nil
	a.mu.Unlock()
	if conn != nil && !conn.Closed() {
		conn.Close(ctx)
	}
}

// Stops the listener, closes the WebSocket and the REST session
func (a *HomeAssistantAdapter) Disconnect(ctx context.Context) error {
	a.SetRunning(false)
	a.mu.Lock()
	task := a.listenTask
	a.mu.Unlock()
	if task != nil {
		task.Cancel()
		if err := task.Wait(ctx); err != nil && !errors.Is(err, context.Canceled) {
			return err
		}
		a.mu.Lock()
		a.listenTask = nil
		a.mu.Unlock()
	}
	a.cleanupWS(ctx)
	a.mu.Lock()
	session := a.restSession
	a.restSession = nil
	a.mu.Unlock()
	if session != nil {
		session.CloseIdleConnections()
	}
	pkgLog().Info(fmt.Sprintf("[%s] Disconnected", a.Name()))
	return nil
}

// Reads events and reconnects with backoff until the adapter stops
func (a *HomeAssistantAdapter) listenLoop(ctx context.Context) error {
	backoffIdx := 0
	for a.IsConnected() {
		if err := a.readEvents(ctx); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			pkgLog().Warn(fmt.Sprintf("[%s] WebSocket error: %s", a.Name(), connectErrorDetail(err)))
		}
		if ctx.Err() != nil || !a.IsConnected() {
			return nil
		}
		delay := backoffSteps[min(backoffIdx, len(backoffSteps)-1)]
		pkgLog().Info(fmt.Sprintf("[%s] Reconnecting in %ds...", a.Name(), delay))
		timer := time.NewTimer(time.Duration(delay) * time.Second)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return nil
		}
		backoffIdx++
		a.cleanupWS(ctx)
		ok, err := a.wsConnect(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			pkgLog().Warn(fmt.Sprintf("[%s] Reconnection failed: %s", a.Name(), connectErrorDetail(err)))
			continue
		}
		if ok {
			backoffIdx = 0
			pkgLog().Info(fmt.Sprintf("[%s] Reconnected", a.Name()))
		}
	}
	return nil
}

// Handles event messages until the WebSocket closes or errors, skipping non text frames and invalid JSON
func (a *HomeAssistantAdapter) readEvents(ctx context.Context) error {
	a.mu.Lock()
	conn := a.ws
	a.mu.Unlock()
	if conn == nil || conn.Closed() {
		return nil
	}
	for {
		msg, err := conn.Receive(ctx)
		if err != nil {
			return err
		}
		switch msg.Type {
		case pyaiohttpws.Close, pyaiohttpws.Closing, pyaiohttpws.Closed, pyaiohttpws.Error:
			return nil
		case pyaiohttpws.Text:
		default:
			continue
		}
		text, _ := msg.Data.(string)
		data, err := pyjson.Loads(text)
		if err != nil {
			pkgLog().Debug(fmt.Sprintf("Invalid JSON from HA WS: %s", pystr.Slice(text, 0, 200)))
			continue
		}
		kind, err := pyGet(data, "type", nil)
		if err != nil {
			return err
		}
		if !pyrepr.Equal(kind, "event") {
			continue
		}
		event, err := pyGet(data, "event", pyjson.NewObject())
		if err != nil {
			return err
		}
		if err := a.handleHAEvent(ctx, event); err != nil {
			return err
		}
	}
}

// Whether an entity passes the ignore list and the domain or entity watch lists, closed by default
func (a *HomeAssistantAdapter) passesFilters(entityID any) (bool, error) {
	ignored, err := a.ignoreEntities.has(entityID)
	if err != nil || ignored {
		return false, err
	}
	if a.watchDomains.truthy() || a.watchEntities.truthy() {
		domain, err := domainOf(entityID)
		if err != nil {
			return false, err
		}
		if watched, err := a.watchDomains.has(domain); err != nil || watched {
			return watched, err
		}
		return a.watchEntities.has(entityID)
	}
	return a.watchAll, nil
}

// Forwards one state_changed event as a channel message after the filters and the per entity cooldown
func (a *HomeAssistantAdapter) handleHAEvent(ctx context.Context, event any) error {
	eventData, err := pyGet(event, "data", pyjson.NewObject())
	if err != nil {
		return err
	}
	entityID, err := pyGet(eventData, "entity_id", "")
	if err != nil {
		return err
	}
	if !pyrepr.Truthy(entityID) {
		return nil
	}
	if passes, err := a.passesFilters(entityID); err != nil || !passes {
		return err
	}
	now := epochSeconds()
	a.mu.Lock()
	last, seen, err := a.lastEventTime.get(entityID)
	a.mu.Unlock()
	if err != nil {
		return err
	}
	lastSeconds := 0.0
	if seen {
		lastSeconds = last.(float64)
	}
	cooldown, _ := new(big.Float).SetInt(a.cooldownSeconds).Float64()
	if now-lastSeconds < cooldown {
		return nil
	}
	a.mu.Lock()
	err = a.lastEventTime.set(entityID, now)
	a.mu.Unlock()
	if err != nil {
		return err
	}
	oldState, err := pyGet(eventData, "old_state", pyjson.NewObject())
	if err != nil {
		return err
	}
	newState, err := pyGet(eventData, "new_state", pyjson.NewObject())
	if err != nil {
		return err
	}
	message, ok, err := formatStateChange(entityID, oldState, newState)
	if err != nil || !ok || message == "" {
		return err
	}
	chatName, userID, userName := "Home Assistant Events", "homeassistant", "Home Assistant"
	source := a.BuildSource(ctx, "ha_events", gateway.SourceFields{ChatName: &chatName, ChatType: "channel", UserID: &userID,
		UserName: &userName})
	messageID := fmt.Sprintf("ha_%s_%d", pyrepr.Str(entityID), int64(now))
	messageEvent := gateway.NewMessageEvent(message)
	messageEvent.MessageType, messageEvent.Source = gateway.MessageTypeText, source
	messageEvent.MessageID = &messageID
	messageEvent.Timestamp = time.Now()
	return a.HandleMessage(ctx, messageEvent)
}

// Human readable description of a state change, false when the new state is empty or unchanged
func formatStateChange(entityID, oldState, newState any) (string, bool, error) {
	if !pyrepr.Truthy(newState) {
		return "", false, nil
	}
	var oldVal any = "unknown"
	if pyrepr.Truthy(oldState) {
		v, err := pyGet(oldState, "state", "unknown")
		if err != nil {
			return "", false, err
		}
		oldVal = v
	}
	newVal, err := pyGet(newState, "state", "unknown")
	if err != nil {
		return "", false, err
	}
	if pyrepr.Equal(oldVal, newVal) {
		return "", false, nil
	}
	attrs, err := pyGet(newState, "attributes", pyjson.NewObject())
	if err != nil {
		return "", false, err
	}
	domain, err := domainOf(entityID)
	if err != nil {
		return "", false, err
	}
	template, ok := domainTemplates[domain]
	if !ok {
		template = defaultTemplate
	}
	attr := func(key string, def any) (any, error) { return pyGet(attrs, key, def) }
	name, err := attr("friendly_name", entityID)
	if err != nil {
		return "", false, err
	}
	temp, _ := attr("current_temperature", "?")
	target, _ := attr("temperature", "?")
	unit, _ := attr("unit_of_measurement", "")
	onOff := "off"
	if pyrepr.Equal(newVal, "on") {
		onOff = "on"
	}
	triggered := func(v any) string {
		if pyrepr.Equal(v, "on") {
			return "triggered"
		}
		return "cleared"
	}
	fields := map[string]any{"name": name, "entity_id": entityID, "old": oldVal, "new": newVal, "temp": temp,
		"target": target, "unit": unit, "on_off": onOff, "new_trig": triggered(newVal), "old_trig": triggered(oldVal)}
	text, err := pyrepr.FormatMap(template, fields)
	if err != nil {
		return "", false, err
	}
	return text, true, nil
}

// Posts the reply as a persistent notification over REST so the WebSocket listener is never raced
func (a *HomeAssistantAdapter) Send(ctx context.Context, chatID, content, replyTo string, metadata map[string]any) (gateway.SendResult, error) {
	target := a.hassURL + "/api/services/persistent_notification/create"
	payload := pyjson.ObjectOf("title", notificationTitle, "message", pystr.Slice(content, 0, maxMessageLength))
	a.mu.Lock()
	session := a.restSession
	a.mu.Unlock()
	if session == nil {
		session = newSession(gateway.GatewayTrustEnv(ctx))
		defer session.CloseIdleConnections()
	}
	resp, err := postJSON(ctx, session, target, authHeaders(a.hassToken), payload, 10*time.Second)
	if err != nil {
		if ctx.Err() != nil {
			return gateway.SendResult{}, ctx.Err()
		}
		var timeout *timeoutError
		if errors.As(err, &timeout) {
			return gateway.SendResult{Success: false, Error: "Timeout sending notification to HA"}, nil
		}
		return gateway.SendResult{Success: false, Error: err.Error()}, nil
	}
	if resp.status < 300 {
		return gateway.SendResult{Success: true, MessageID: strings.ReplaceAll(uuid.NewString(), "-", "")[:12]}, nil
	}
	text, err := resp.text()
	if err != nil {
		return gateway.SendResult{Success: false, Error: err.Error()}, nil
	}
	return gateway.SendResult{Success: false, Error: fmt.Sprintf("HTTP %d: %s", resp.status, text)}, nil
}

// Chat info of the event channel with the instance URL
func (a *HomeAssistantAdapter) GetChatInfo(ctx context.Context, chatID string) (map[string]any, error) {
	return map[string]any{"name": "Home Assistant Events", "type": "channel", "url": a.hassURL}, nil
}
