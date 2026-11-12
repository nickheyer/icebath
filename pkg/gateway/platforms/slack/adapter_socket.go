package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	slackapi "github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"
)

// Acknowledges a Socket Mode envelope with an optional response payload
type ackFunc func(payload map[string]any) error

// Event listener receiving the inner event and the outer envelope payload
type eventListener func(ctx context.Context, event, body map[string]any)

// Slash command listener receiving its ack and the command payload
type commandListener func(ctx context.Context, ack ackFunc, command map[string]any)

// Block action listener receiving its ack, the interaction payload and the matched action
type actionListener func(ctx context.Context, ack ackFunc, body, action map[string]any)

// Action matcher, an exact id, a pattern or Bolt constraints on the action and payload fields
type actionMatcher struct {
	id          string
	pattern     *regexp.Regexp
	constraints map[string]actionMatcher
}

// Whether the matcher accepts the value
func (m actionMatcher) matches(value string) bool {
	if m.pattern != nil {
		return m.pattern.MatchString(value)
	}
	return m.id == value
}

// Whether the matcher accepts the action of the payload, constraints checked field by field
func (m actionMatcher) matchesAction(body, action map[string]any) bool {
	if m.constraints == nil {
		return m.matches(strOrEmpty(action["action_id"]))
	}
	for key, c := range m.constraints {
		var value string
		switch key {
		case "type":
			value = strOrEmpty(body["type"])
		case "callback_id":
			value = strOrEmpty(body["callback_id"])
		default:
			value = strOrEmpty(action[key])
		}
		if !c.matches(value) {
			return false
		}
	}
	return true
}

// Socket Mode app routing events, commands and actions to the first matching listener
type slackApp struct {
	client    *webClient
	botUserID string
	botID     string

	mu       sync.Mutex
	events   []eventRoute
	commands []commandRoute
	actions  []actionRoute
}

type eventRoute struct {
	eventType string
	pattern   *regexp.Regexp
	listener  eventListener
}

type commandRoute struct {
	pattern  *regexp.Regexp
	listener commandListener
}

type actionRoute struct {
	matcher  actionMatcher
	listener actionListener
}

// Registers a listener for an event type
func (app *slackApp) event(eventType string, listener eventListener) {
	app.mu.Lock()
	app.events = append(app.events, eventRoute{eventType: eventType, listener: listener})
	app.mu.Unlock()
}

// Registers a listener for every event type the pattern matches
func (app *slackApp) eventPattern(pattern *regexp.Regexp, listener eventListener) {
	app.mu.Lock()
	app.events = append(app.events, eventRoute{pattern: pattern, listener: listener})
	app.mu.Unlock()
}

// Registers a listener for slash commands the pattern matches
func (app *slackApp) command(pattern *regexp.Regexp, listener commandListener) {
	app.mu.Lock()
	app.commands = append(app.commands, commandRoute{pattern: pattern, listener: listener})
	app.mu.Unlock()
}

// Registers a listener for block actions the matcher accepts
func (app *slackApp) action(matcher actionMatcher, listener actionListener) {
	app.mu.Lock()
	app.actions = append(app.actions, actionRoute{matcher: matcher, listener: listener})
	app.mu.Unlock()
}

// Acting user id of a payload, read from user, user_id, then the nested event or message
func extractUserID(payload map[string]any) string {
	if user, ok := payload["user"]; ok && user != nil {
		if s, ok := user.(string); ok {
			return s
		}
		if m, ok := user.(map[string]any); ok {
			if _, has := m["id"]; has {
				return strOrEmpty(m["id"])
			}
		}
	}
	if v, ok := payload["user_id"]; ok {
		return strOrEmpty(v)
	}
	for _, key := range []string{"event", "message", "previous_message"} {
		if nested, ok := payload[key].(map[string]any); ok {
			return extractUserID(nested)
		}
	}
	return ""
}

// Whether an envelope is the app's own event, which the app drops before any listener
func (app *slackApp) isSelfEvent(body map[string]any) bool {
	event := asMap(body["event"])
	switch event["type"] {
	case "member_joined_channel", "member_left_channel":
		return false
	}
	userID := extractUserID(body)
	botID := strOrEmpty(event["bot_id"])
	return (userID != "" && userID == app.botUserID) || (botID != "" && botID == app.botID)
}

// Routes an Events API envelope to the first listener of its event type
func (app *slackApp) dispatchEvent(ctx context.Context, body map[string]any) {
	if app.isSelfEvent(body) {
		return
	}
	event := asMap(body["event"])
	eventType := strOrEmpty(event["type"])
	app.mu.Lock()
	routes := append([]eventRoute(nil), app.events...)
	app.mu.Unlock()
	for _, r := range routes {
		if (r.pattern == nil && r.eventType == eventType) || (r.pattern != nil && r.pattern.MatchString(eventType)) {
			r.listener(ctx, event, body)
			return
		}
	}
}

// Routes a slash command to the first listener its name matches, acking it when none does
func (app *slackApp) dispatchCommand(ctx context.Context, ack ackFunc, command map[string]any) {
	name := strOrEmpty(command["command"])
	app.mu.Lock()
	routes := append([]commandRoute(nil), app.commands...)
	app.mu.Unlock()
	for _, r := range routes {
		if r.pattern.MatchString(name) {
			r.listener(ctx, ack, command)
			return
		}
	}
	_ = ack(nil)
}

// Routes a block action payload to the first listener its action id matches, acking it when none does
func (app *slackApp) dispatchAction(ctx context.Context, ack ackFunc, body map[string]any) {
	actions := asList(body["actions"])
	if len(actions) == 0 {
		_ = ack(nil)
		return
	}
	action := asMap(actions[0])
	app.mu.Lock()
	routes := append([]actionRoute(nil), app.actions...)
	app.mu.Unlock()
	for _, r := range routes {
		if r.matcher.matchesAction(body, action) {
			r.listener(ctx, ack, body, action)
			return
		}
	}
	_ = ack(nil)
}

// Socket Mode connection with its observed transport state and last ping time
type socketModeHandler struct {
	client       *socketmode.Client
	pingInterval time.Duration

	mu           sync.Mutex
	connected    *bool
	lastPingPong float64
	hasPingPong  bool
}

// slack-go debug log sink that records pings and logs the rest at debug level
type socketLogSink struct{ h *socketModeHandler }

func (s socketLogSink) Output(_ int, line string) error {
	if strings.Contains(line, "WebSocket ping message received") {
		s.h.mu.Lock()
		s.h.lastPingPong, s.h.hasPingPong = wallTime(), true
		s.h.mu.Unlock()
		return nil
	}
	slog.Debug("[Slack] socketmode: " + strings.TrimSpace(line))
	return nil
}

// Expected interval between Slack's websocket pings
const socketPingInterval = 30 * time.Second

// Socket Mode handler for the app token, the app's client carrying the API URL and proxy
func newSocketModeHandler(app *slackApp, appToken string) *socketModeHandler {
	h := &socketModeHandler{pingInterval: socketPingInterval}
	api := slackapi.New(app.client.token,
		slackapi.OptionAppLevelToken(appToken),
		slackapi.OptionAPIURL(app.client.apiURL),
		slackapi.OptionHTTPClient(app.client.http))
	h.client = socketmode.New(api,
		socketmode.OptionLog(socketLogSink{h}),
		socketmode.OptionDebug(true),
		socketmode.OptionPingInterval(h.pingInterval))
	return h
}

// Records the transport state the connection events report
func (h *socketModeHandler) setConnected(on bool) {
	h.mu.Lock()
	h.connected = &on
	h.mu.Unlock()
}

// Transport state, nil before the first connection event
func (h *socketModeHandler) isConnected() *bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.connected
}

// Runs the connection and dispatches its envelopes to the app until ctx ends or the client fails
func (h *socketModeHandler) run(ctx context.Context, app *slackApp) error {
	errc := make(chan error, 1)
	go func() { errc <- h.client.RunContext(ctx) }()
	for {
		select {
		case err := <-errc:
			h.setConnected(false)
			return err
		case evt, ok := <-h.client.Events:
			if !ok {
				return <-errc
			}
			h.handle(ctx, app, evt)
		}
	}
}

// Handles one Socket Mode event, acking envelopes and routing them on their own goroutine
func (h *socketModeHandler) handle(ctx context.Context, app *slackApp, evt socketmode.Event) {
	var req socketmode.Request
	switch evt.Type {
	case socketmode.EventTypeConnected, socketmode.EventTypeHello:
		h.setConnected(true)
		return
	case socketmode.EventTypeConnecting, socketmode.EventTypeDisconnect, socketmode.EventTypeConnectionError:
		h.setConnected(false)
		return
	case socketmode.EventTypeEventsAPI, socketmode.EventTypeInteractive, socketmode.EventTypeSlashCommand:
		if evt.Request == nil {
			return
		}
		req = *evt.Request
	case socketmode.EventTypeErrorBadMessage:
		raw, ok := rawEnvelope(evt)
		if !ok {
			return
		}
		req = raw
	default:
		return
	}
	body := map[string]any{}
	if decoded, err := pyjson.Loads(string(req.Payload)); err == nil {
		body = asMap(pyjson.Plain(decoded))
	}
	var once sync.Once
	ack := func(payload map[string]any) error {
		var err error
		once.Do(func() {
			if payload == nil {
				err = h.client.AckCtx(ctx, req.EnvelopeID, nil)
			} else {
				err = h.client.AckCtx(ctx, req.EnvelopeID, payload)
			}
		})
		return err
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error(fmt.Sprintf("[Slack] listener panicked: %v", r))
				_ = ack(nil)
			}
		}()
		switch req.Type {
		case socketmode.RequestTypeEventsAPI:
			_ = ack(nil)
			app.dispatchEvent(ctx, body)
		case socketmode.RequestTypeSlashCommands:
			app.dispatchCommand(ctx, ack, body)
		case socketmode.RequestTypeInteractive:
			if body["type"] == "block_actions" {
				app.dispatchAction(ctx, ack, body)
				return
			}
			_ = ack(nil)
		}
	}()
}

// Envelope slack-go could not type, recovered from its raw frame so it is acked and routed as Bolt routes every envelope
func rawEnvelope(evt socketmode.Event) (socketmode.Request, bool) {
	var req socketmode.Request
	bad, ok := evt.Data.(*socketmode.ErrorBadMessage)
	if !ok || json.Unmarshal(bad.Message, &req) != nil || req.EnvelopeID == "" {
		return req, false
	}
	switch req.Type {
	case socketmode.RequestTypeEventsAPI, socketmode.RequestTypeSlashCommands, socketmode.RequestTypeInteractive:
		return req, true
	}
	return req, false
}

// Starts the Socket Mode goroutine for the current app and app token
func (a *SlackAdapter) startSocketModeHandler(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.app == nil || a.appToken == "" {
		return errors.New("Socket Mode requires an initialized app and app token")
	}
	handler := newSocketModeHandler(a.app, a.appToken)
	app := a.app
	a.handler = handler
	task := startSocketTask(context.WithoutCancel(ctx), func(taskCtx context.Context) error {
		return handler.run(taskCtx, app)
	}, func(done *socketTask) { a.onSocketModeTaskDone(context.WithoutCancel(ctx), done, done.err) })
	a.socketModeTask = task
	a.socketHandlerStarted = monotonic()
	return nil
}

// Stops the Socket Mode goroutine before dropping the handler so nothing reconnects after close
func (a *SlackAdapter) stopSocketModeHandler() {
	a.mu.Lock()
	task := a.socketModeTask
	a.handler, a.socketModeTask = nil, nil
	a.mu.Unlock()
	cancelSocketTasks([]*socketTask{task})
}

// Transport state of the current handler, nil when unknown
func (a *SlackAdapter) socketTransportConnected() *bool {
	a.mu.Lock()
	h := a.handler
	a.mu.Unlock()
	if h == nil {
		return nil
	}
	return h.isConnected()
}

// Whether no ping arrived within the stale window, or none within the grace after connecting
func (a *SlackAdapter) socketPingPongStale() bool {
	a.mu.Lock()
	h, started := a.handler, a.socketHandlerStarted
	a.mu.Unlock()
	if h == nil {
		return false
	}
	interval := h.pingInterval.Seconds()
	if interval <= 0 {
		return false
	}
	h.mu.Lock()
	last, has := h.lastPingPong, h.hasPingPong
	h.mu.Unlock()
	if !has {
		grace := max(a.socketFirstPingGrace, interval*2)
		return started != 0 && monotonic()-started > grace
	}
	return wallTime()-last > interval*a.socketPingStaleFactor
}

// Reconnects Socket Mode without rebuilding adapter state
func (a *SlackAdapter) restartSocketMode(ctx context.Context, reason string) {
	if !a.running() {
		return
	}
	a.socketReconnectMu.Lock()
	defer a.socketReconnectMu.Unlock()
	a.mu.Lock()
	ready := a.app != nil && a.appToken != ""
	a.mu.Unlock()
	if !a.running() || !ready {
		return
	}
	slog.Warn(fmt.Sprintf("[Slack] Socket Mode unhealthy (%s); reconnecting", reason))
	a.stopSocketModeHandler()
	if err := a.startSocketModeHandler(ctx); err != nil {
		slog.Error(fmt.Sprintf("[Slack] Socket Mode reconnect failed: %v", err))
	}
}

// Watches Socket Mode and reconnects when the task stops, the transport drops or pings go stale
func (a *SlackAdapter) socketWatchdogLoop(ctx context.Context) error {
	for a.running() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(a.socketWatchdogInterval):
		}
		if !a.running() {
			break
		}
		a.mu.Lock()
		task := a.socketModeTask
		a.mu.Unlock()
		switch {
		case task == nil:
			a.restartSocketMode(ctx, "socket task missing")
		case task.isDone():
			a.restartSocketMode(ctx, "socket task stopped")
		default:
			if connected := a.socketTransportConnected(); connected != nil && !*connected {
				a.restartSocketMode(ctx, "transport disconnected")
			} else if a.socketPingPongStale() {
				a.restartSocketMode(ctx, "ping/pong stale")
			}
		}
	}
	return nil
}

// Restarts the watchdog when it exits while the adapter still runs
func (a *SlackAdapter) onSocketWatchdogDone(ctx context.Context, task *socketTask) {
	a.mu.Lock()
	current := a.socketWatchdogTask
	a.mu.Unlock()
	if task != current || errors.Is(task.err, context.Canceled) || !a.running() {
		return
	}
	if task.err != nil {
		slog.Warn(fmt.Sprintf("[Slack] Socket Mode watchdog exited with error; restarting: %v", task.err))
	} else {
		slog.Warn("[Slack] Socket Mode watchdog exited; restarting")
	}
	a.mu.Lock()
	a.socketWatchdogTask = nil
	a.mu.Unlock()
	a.ensureSocketWatchdog(ctx)
}

// Cancels and awaits the watchdog, its failure debug logged
func (a *SlackAdapter) cancelSocketWatchdog(failureMsg string) {
	a.mu.Lock()
	task := a.socketWatchdogTask
	a.socketWatchdogTask = nil
	a.mu.Unlock()
	if task == nil || task.isDone() {
		return
	}
	task.cancel()
	<-task.done
	if task.err != nil && !errors.Is(task.err, context.Canceled) {
		slog.Debug(fmt.Sprintf("%s: %v", failureMsg, task.err))
	}
}

// Starts the watchdog unless a live one exists
func (a *SlackAdapter) ensureSocketWatchdog(ctx context.Context) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.socketWatchdogTask != nil && !a.socketWatchdogTask.isDone() {
		return
	}
	a.socketWatchdogTask = startSocketTask(context.WithoutCancel(ctx), a.socketWatchdogLoop,
		func(done *socketTask) { a.onSocketWatchdogDone(context.WithoutCancel(ctx), done) })
}

// Schedules a reconnect when the current Socket Mode task exits unexpectedly
func (a *SlackAdapter) onSocketModeTaskDone(ctx context.Context, task *socketTask, err error) {
	<-task.done
	a.mu.Lock()
	current := a.socketModeTask
	a.mu.Unlock()
	if task != current || errors.Is(err, context.Canceled) || !a.running() {
		return
	}
	if err != nil {
		slog.Warn(fmt.Sprintf("[Slack] Socket Mode task exited with error: %v", err))
	} else {
		slog.Warn("[Slack] Socket Mode task exited unexpectedly")
	}
	a.restartSocketMode(ctx, "socket task exited")
}

// Authenticates one bot token and registers its workspace client and identity, the first token primary
func (a *SlackAdapter) authenticateWorkspace(ctx context.Context, token, proxyURL string) error {
	client := newWebClient(token, proxyURL)
	auth, err := client.call(ctx, "auth.test", nil)
	if err != nil {
		return err
	}
	teamID := strOrEmpty(getOr(auth.Data, "team_id", ""))
	botUserID := strOrEmpty(getOr(auth.Data, "user_id", ""))
	botName := pyrepr.Str(getOr(auth.Data, "user", "unknown"))
	teamName := pyrepr.Str(getOr(auth.Data, "team", "unknown"))
	a.mu.Lock()
	a.teamClients.Set(teamID, client)
	a.teamBotUserIDs[teamID] = botUserID
	a.teamBotNames[teamID] = botName
	if a.botUserID == nil {
		a.botUserID = &botUserID
		if a.app != nil {
			a.app.botUserID = botUserID
			a.app.botID = strOrEmpty(auth.Data["bot_id"])
		}
	}
	if a.botDisplayName == nil {
		a.botDisplayName = &botName
	}
	a.mu.Unlock()
	slog.Info(fmt.Sprintf("[Slack] Authenticated as @%s in workspace %s (team: %s)", botName, teamName, teamID))
	a.warnIfMissingGroupDMScopes(auth.Headers, teamName)
	a.warnIfNotBotToken(auth.Data, teamName)
	a.warnIfInchannelWithoutFlatReply(teamName)
	return nil
}

// Connects through Socket Mode under the app token lock, replacing any previous handler
func (a *SlackAdapter) Connect(ctx context.Context, isReconnect bool) (bool, error) {
	rawToken := strOrEmpty(a.Config.Token)
	appToken, _ := gateway.GetScopedSecret(ctx, "SLACK_APP_TOKEN", "", false)
	for _, pair := range [][2]string{{"SLACK_BOT_TOKEN", rawToken}, {"SLACK_APP_TOKEN", appToken}} {
		if pair[1] == "" {
			a.fatalMissingEnv(ctx, pair[0])
			return false, nil
		}
	}
	proxyURL := resolveSlackProxyURL(ctx)
	if proxyURL != "" {
		slog.Info(fmt.Sprintf("[Slack] Using proxy for Slack transport: %s", gateway.SafeURLForLog(proxyURL, 80)))
	}
	botTokens := loadSlackBotTokens(ctx, rawToken, false)
	acquired, err := a.AcquirePlatformLock(ctx, "slack-app-token", appToken, "Slack app token")
	if err != nil {
		slog.Error(fmt.Sprintf("[Slack] Connection failed: %v", err))
		return false, nil
	}
	if !acquired {
		return false, nil
	}
	ok := a.connectLocked(ctx, appToken, proxyURL, botTokens)
	if !ok {
		a.ReleasePlatformLock()
	}
	return ok, nil
}

// Body of Connect run while the app token lock is held, false on any failure
func (a *SlackAdapter) connectLocked(ctx context.Context, appToken, proxyURL string, botTokens []string) bool {
	a.SetRunning(false)
	a.cancelSocketWatchdog("[Slack] Prior watchdog task failed while stopping")
	a.stopSocketModeHandler()
	a.closeWorkspaceClients()
	a.mu.Lock()
	a.app = nil
	a.appToken = appToken
	a.proxyURL = proxyURL
	a.botUserID, a.botDisplayName = nil, nil
	a.teamClients = pydict.New[string, *webClient]()
	a.teamBotUserIDs, a.teamBotNames = map[string]string{}, map[string]string{}
	a.app = &slackApp{client: newWebClient(botTokens[0], proxyURL)}
	a.mu.Unlock()
	for _, token := range botTokens {
		if err := a.authenticateWorkspace(ctx, token, proxyURL); err != nil {
			slog.Error(fmt.Sprintf("[Slack] Connection failed: %v", err))
			return false
		}
	}
	if err := a.registerBoltHandlers(ctx); err != nil {
		slog.Error(fmt.Sprintf("[Slack] Connection failed: %v", err))
		return false
	}
	if err := a.startSocketModeHandler(ctx); err != nil {
		a.SetRunning(false)
		a.stopSocketModeHandler()
		slog.Error(fmt.Sprintf("[Slack] Connection failed: %v", err))
		return false
	}
	a.SetRunning(true)
	a.ensureSocketWatchdog(ctx)
	a.mu.Lock()
	n := a.teamClients.Len()
	a.mu.Unlock()
	slog.Info(fmt.Sprintf("[Slack] Socket Mode connected (%d workspace(s))", n))
	a.hintAllowBots(ctx)
	return true
}

// Logs and records the permanent config error of a missing Slack token
func (a *SlackAdapter) fatalMissingEnv(ctx context.Context, envName string) {
	slog.Error(fmt.Sprintf("[Slack] %s not set — this is a permanent config error; set %s via `icebath "+
		"gateway setup` or in the active profile's ICEBATH_HOME/.env file, then restart the "+
		"gateway.", envName, envName))
	a.SetFatalError(ctx, "missing_"+strings.ToLower(envName),
		envName+" not configured. Use `icebath gateway setup` "+
			"or add it to your active profile's ICEBATH_HOME/.env file, then restart the gateway.", false)
}

// Startup hint for the plumbing bot to bot interop also needs when allow_bots is on
func (a *SlackAdapter) hintAllowBots(ctx context.Context) {
	if policy := a.slackAllowBots(ctx); policy != "none" {
		slog.Info(fmt.Sprintf("[Slack] allow_bots=%s — for bot-to-bot interop also ensure: (a) the Slack "+
			"app manifest subscribes to message.channels / message.groups / message.im as "+
			"appropriate (run 'icebath slack manifest' if unsure), and (b) the other bot's "+
			"Slack user id is in SLACK_ALLOWED_USERS or GATEWAY_ALLOW_ALL_USERS=true. "+
			"Without these, bot events are silently dropped upstream of the allow_bots "+
			"gate.", policy))
	}
}

// Posts a seed message and returns its ts as the handoff thread id
func (a *SlackAdapter) CreateHandoffThread(ctx context.Context, parentChatID, name string) (string, bool, error) {
	if a.currentApp() == nil {
		return "", false, nil
	}
	client := a.getClient(parentChatID, "")
	if client == nil {
		return "", false, nil
	}
	if name == "" {
		name = i18n.T(ctx, "platform.shared.handoff.default_name", "", nil)
	}
	seed := i18n.T(ctx, "platform.slack.handoff.seed", "", map[string]any{"name": pystr.Slice(pystr.Strip(name), 0, 80)})
	result, err := client.call(ctx, "chat.postMessage", map[string]any{"channel": parentChatID, "text": seed})
	if err != nil {
		slog.Warn(fmt.Sprintf("[%s] Handoff thread: seed-post failed for channel %s: %v", a.Name(), parentChatID, err))
		return "", false, nil
	}
	ts := strOrEmpty(result.get("ts"))
	return ts, ts != "", nil
}

// Disconnects, sealing live streams and releasing the app token lock
func (a *SlackAdapter) Disconnect(ctx context.Context) error {
	a.SetRunning(false)
	a.mu.Lock()
	var keys []threadKey
	var streams []*draftStream
	for _, k := range a.activeStreams.Keys() {
		s, _ := a.activeStreams.Get(k)
		keys, streams = append(keys, k), append(streams, s)
	}
	a.mu.Unlock()
	for i, k := range keys {
		a.sealStream(ctx, k, streams[i], "")
	}
	a.mu.Lock()
	a.activeStreams.Clear()
	a.mu.Unlock()
	a.cancelSocketWatchdog("[Slack] Watchdog task raised during disconnect")
	a.mu.Lock()
	cards := map[threadKey]*nativeTaskCardStream{}
	for k, s := range a.nativeTaskCardStreams {
		cards[k] = s
	}
	a.mu.Unlock()
	for k, s := range cards {
		a.stopNativeTaskCardStream(ctx, k, s)
	}
	a.stopSocketModeHandler()
	a.closeWorkspaceClients()
	a.mu.Lock()
	a.app, a.appToken, a.proxyURL, a.botUserID = nil, "", "", nil
	a.teamClients = pydict.New[string, *webClient]()
	a.teamBotUserIDs = map[string]string{}
	a.channelTeam = pydict.New[string, string]()
	a.dmConversationCache = pydict.New[string, string]()
	a.mu.Unlock()
	a.ReleasePlatformLock()
	slog.Info("[Slack] Disconnected")
	return nil
}
