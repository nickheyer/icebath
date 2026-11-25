package homeassistant

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"

	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/settings"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	httputil "github.com/nickheyer/icebath/pkg/utils/http"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	templateutil "github.com/nickheyer/icebath/pkg/utils/template"
	textutil "github.com/nickheyer/icebath/pkg/utils/text"
	websocketutil "github.com/nickheyer/icebath/pkg/utils/websocket"
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
	if !anyutil.IsTrue(token) {
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
	return strings.TrimSpace(text) != "", nil
}

// Domain of an entity id, empty without a dot, a TypeError for a value that is not text
func domainOf(entityID any) (string, error) {
	id, ok := entityID.(string)
	if !ok {
		return "", &kindErr{"TypeError", fmt.Sprintf("an entity id must be text, got %s", errchain.Kind(entityID))}
	}
	if !strings.Contains(id, ".") {
		return "", nil
	}
	head, _, _ := strings.Cut(id, ".")
	return head, nil
}

// Bearer and JSON content headers of a token
func authHeaders(token any) map[string]string {
	return map[string]string{"Authorization": "Bearer " + anyutil.String(token), "Content-Type": "application/json"}
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

	hassURL        string
	hassToken      any
	watchDomains   map[string]bool
	watchEntities  map[string]bool
	ignoreEntities map[string]bool
	watchAll       bool
	cooldown       time.Duration

	mu            sync.Mutex
	msgID         int64
	ws            *websocketutil.Conn
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

// Set of the items of a list
func textSet(items []string) map[string]bool {
	out := make(map[string]bool, len(items))
	for _, item := range items {
		out[item] = true
	}
	return out
}

// Adapter with the URL and token of the connection and the filters and cooldown of the profile served
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
	if !anyutil.IsTrue(rawURL) {
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
	a.hassURL = strings.TrimRight(urlText, "/")
	a.hassToken = config.Token
	if !anyutil.IsTrue(a.hassToken) {
		scoped, err := gateway.GetScopedSecret(ctx, "HASS_TOKEN", "", false)
		if err != nil {
			return nil, err
		}
		a.hassToken = scoped
	}
	sctx := gateway.SettingsContext(ctx)
	a.watchDomains = textSet(settings.HomeassistantWatchDomains.Strings(sctx))
	a.watchEntities = textSet(settings.HomeassistantWatchEntities.Strings(sctx))
	a.ignoreEntities = textSet(settings.HomeassistantIgnoreEntities.Strings(sctx))
	a.watchAll = settings.HomeassistantWatchAll.Get(sctx)
	a.cooldown = settings.HomeassistantCooldown.Get(sctx)
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
	if !anyutil.IsTrue(a.hassToken) {
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
	if len(a.watchDomains) == 0 && len(a.watchEntities) == 0 && !a.watchAll {
		pkgLog().Warn(fmt.Sprintf("[%s] No watch_domains, watch_entities, or watch_all configured. "+
			"All state_changed events will be dropped. Set homeassistant.watch_domains, "+
			"homeassistant.watch_entities or homeassistant.watch_all to receive events.", a.Name()))
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
	opts := websocketutil.Options{PingInterval: 30 * time.Second, PingTimeout: 15 * time.Second, CloseTimeout: 30 * time.Second,
		MaxMessageSize: 4 * 1024 * 1024}
	if gateway.GatewayTrustEnv(ctx) {
		opts.Proxy = httputil.Proxy
	}
	const connectLimit = 30 * time.Second
	connectCtx, cancel := context.WithTimeout(ctx, connectLimit)
	conn, err := websocketutil.Connect(connectCtx, wsURL, opts)
	timedOut := errors.Is(connectCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil
	cancel()
	if err != nil {
		if timedOut {
			return false, &timeoutError{what: "websocket connect to " + wsURL, limit: connectLimit}
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
	kind, err := lookup(msg, "type", nil)
	if err != nil {
		return false, err
	}
	if kind != "auth_required" {
		return a.handshakeFailed(ctx, fmt.Sprintf("Expected auth_required, got: %s", anyutil.String(kind)))
	}
	if err := sendJSON(conn, jsonutil.ObjectOf("type", "auth", "access_token", a.hassToken)); err != nil {
		return false, err
	}
	if msg, err = receiveJSON(ctx, conn); err != nil {
		return false, err
	}
	if kind, err = lookup(msg, "type", nil); err != nil {
		return false, err
	}
	if kind != "auth_ok" {
		return a.handshakeFailed(ctx, "Auth failed: "+anyutil.String(msg))
	}
	if err := sendJSON(conn, jsonutil.ObjectOf("id", a.nextID(), "type", "subscribe_events", "event_type", "state_changed")); err != nil {
		return false, err
	}
	if msg, err = receiveJSON(ctx, conn); err != nil {
		return false, err
	}
	success, err := lookup(msg, "success", nil)
	if err != nil {
		return false, err
	}
	if !anyutil.IsTrue(success) {
		return a.handshakeFailed(ctx, "Failed to subscribe to events: "+anyutil.String(msg))
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
		conn.CloseWith(ctx, websocketutil.CloseNormal, "")
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

// Handles event messages until the WebSocket closes or fails, skipping binary messages and invalid JSON
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
			var closed *websocketutil.CloseError
			if errors.As(err, &closed) {
				return nil
			}
			return err
		}
		if msg.Type != websocketutil.Text {
			continue
		}
		text := string(msg.Data)
		data, err := jsonutil.Decode(msg.Data)
		if err != nil {
			pkgLog().Debug(fmt.Sprintf("Invalid JSON from HA WS: %s", textutil.First(text, 200)))
			continue
		}
		kind, err := lookup(data, "type", nil)
		if err != nil {
			return err
		}
		if kind != "event" {
			continue
		}
		event, err := lookup(data, "event", jsonutil.NewObject())
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
	domain, err := domainOf(entityID)
	if err != nil {
		return false, err
	}
	id := entityID.(string)
	if a.ignoreEntities[id] {
		return false, nil
	}
	if len(a.watchDomains) > 0 || len(a.watchEntities) > 0 {
		return a.watchDomains[domain] || a.watchEntities[id], nil
	}
	return a.watchAll, nil
}

// Forwards one state_changed event as a channel message after the filters and the per entity cooldown
func (a *HomeAssistantAdapter) handleHAEvent(ctx context.Context, event any) error {
	eventData, err := lookup(event, "data", jsonutil.NewObject())
	if err != nil {
		return err
	}
	entityID, err := lookup(eventData, "entity_id", "")
	if err != nil {
		return err
	}
	if !anyutil.IsTrue(entityID) {
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
	if now-lastSeconds < a.cooldown.Seconds() {
		return nil
	}
	a.mu.Lock()
	err = a.lastEventTime.set(entityID, now)
	a.mu.Unlock()
	if err != nil {
		return err
	}
	oldState, err := lookup(eventData, "old_state", jsonutil.NewObject())
	if err != nil {
		return err
	}
	newState, err := lookup(eventData, "new_state", jsonutil.NewObject())
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
	messageID := fmt.Sprintf("ha_%s_%d", anyutil.String(entityID), int64(now))
	messageEvent := gateway.NewMessageEvent(message)
	messageEvent.MessageType, messageEvent.Source = gateway.MessageTypeText, source
	messageEvent.MessageID = &messageID
	messageEvent.Timestamp = time.Now()
	return a.HandleMessage(ctx, messageEvent)
}

// Value of a key of a state mapping, the default when the key is absent or holds null
func presentOr(v any, key string, def any) (any, error) {
	val, err := lookup(v, key, def)
	if err == nil && val == nil {
		return def, nil
	}
	return val, err
}

// Human readable description of a state change, false when the new state is empty or unchanged
func formatStateChange(entityID, oldState, newState any) (string, bool, error) {
	if !anyutil.IsTrue(newState) {
		return "", false, nil
	}
	var oldVal any = "unknown"
	if anyutil.IsTrue(oldState) {
		v, err := presentOr(oldState, "state", "unknown")
		if err != nil {
			return "", false, err
		}
		oldVal = v
	}
	newVal, err := presentOr(newState, "state", "unknown")
	if err != nil {
		return "", false, err
	}
	if anyutil.Equal(oldVal, newVal) {
		return "", false, nil
	}
	attrs, err := lookup(newState, "attributes", jsonutil.NewObject())
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
	attr := func(key string, def any) (any, error) { return presentOr(attrs, key, def) }
	name, err := attr("friendly_name", entityID)
	if err != nil {
		return "", false, err
	}
	temp, _ := attr("current_temperature", "?")
	target, _ := attr("temperature", "?")
	unit, _ := attr("unit_of_measurement", "")
	onOff := "off"
	if newVal == "on" {
		onOff = "on"
	}
	triggered := func(v any) string {
		if v == "on" {
			return "triggered"
		}
		return "cleared"
	}
	fields := map[string]any{"name": name, "entity_id": entityID, "old": oldVal, "new": newVal, "temp": temp,
		"target": target, "unit": unit, "on_off": onOff, "new_trig": triggered(newVal), "old_trig": triggered(oldVal)}
	text, err := templateutil.Format(template, fields)
	if err != nil {
		return "", false, err
	}
	return text, true, nil
}

// Posts the reply as a persistent notification over REST so the WebSocket listener is never raced
func (a *HomeAssistantAdapter) Send(ctx context.Context, chatID, content, replyTo string, metadata map[string]any) (gateway.SendResult, error) {
	target := a.hassURL + "/api/services/persistent_notification/create"
	payload := jsonutil.ObjectOf("title", notificationTitle, "message", textutil.First(content, maxMessageLength))
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
	return gateway.SendResult{Success: false, Error: fmt.Sprintf("HTTP %d: %s", resp.status, resp.text())}, nil
}

// Chat info of the event channel with the instance URL
func (a *HomeAssistantAdapter) GetChatInfo(ctx context.Context, chatID string) (map[string]any, error) {
	return map[string]any{"name": "Home Assistant Events", "type": "channel", "url": a.hassURL}, nil
}
