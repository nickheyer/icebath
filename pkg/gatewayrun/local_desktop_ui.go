package gatewayrun

import (
	"errors"
	"maps"
	"reflect"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Read and action deadlines of the desktop window bridges in seconds
const (
	desktopTerminalReadTimeoutS          = 30
	desktopPreviewReadTimeoutS           = 45
	desktopWindowReadTimeoutS            = 30
	desktopPreviewActionTimeoutS         = 45
	desktopPreviewActionProbeTimeoutS    = 10
	desktopPreviewActionReprobeCooldownS = 30
	desktopThemeTimeoutS                 = 15
	desktopThemePickTimeoutS             = 300
	desktopTourTimeoutS                  = 45
	desktopTourProbeTimeoutS             = 10
)

var desktopPreviewActionBridgeUnavailable = mustDumpsJSON(pyjson.ObjectOf("success", false, "error",
	"No Hermes Desktop window answered the preview action request. The drive_preview / "+
		"annotate_preview bridge is served by the desktop app's renderer, which updates "+
		"separately from this backend, so an app build older than the tool has nothing "+
		"listening. Update the Hermes Desktop app, open a page with open_preview, and try "+
		"again in this session after a short cooldown."))

var desktopTourBridgeUnavailable = mustDumpsJSON(pyjson.ObjectOf("success", false, "error",
	"No Hermes Desktop window answered the tour request. The tour is driven by the desktop app's "+
		"renderer, which updates separately from this backend, so an app build older than the tour tool "+
		"has nothing listening. Update the Hermes Desktop app and start a new session. Do not retry tour "+
		"in this session."))

func desktopThemeUnanswered() *pyjson.Object {
	return pyjson.ObjectOf("success", false, "error", "No Hermes Desktop window answered the theme request.")
}

func mustDumpsJSON(v any) string {
	out, err := pyjson.Dumps(v)
	if err != nil {
		panic(err)
	}
	return out
}

// Publish function of one live local viewer turn, compared by identity on release
type desktopTurnPublish struct {
	publish func(eventType string, payload map[string]any) bool
}

// Preview action bridge state of one session, answered or unanswered with its reprobe time
type desktopPreviewBridge struct {
	state   string
	retryAt float64
}

// Authority and canonical session that last ran a turn under a process session key
type desktopProcessRoute struct {
	authority *gateway.SessionAuthority
	sessionID string
}

var desktopUI = struct {
	mu                    sync.Mutex
	turns                 map[string]*desktopTurnPublish
	previewBridge         map[string]*desktopPreviewBridge
	tourBridge            map[string]string
	processRoutes         map[string]desktopProcessRoute
	emitterInstalled      bool
	processSinksInstalled bool
	connectionHook        func(op *tools.ConnectionOperation, change, snapshot *pyjson.Object)
}{turns: map[string]*desktopTurnPublish{}, previewBridge: map[string]*desktopPreviewBridge{},
	tourBridge: map[string]string{}, processRoutes: map[string]desktopProcessRoute{}}

func desktopMonotonic() float64 { return float64(time.Now().UnixNano()) / 1e9 }

// Publishes a desktop_ui event on the stream of the turn that raised it, an error outside a local viewer turn
func desktopEmit(env *tools.Env, sid, event string, payload map[string]any) error {
	keys := []string{sid, tools.GetSessionEnv(env, "ICEBATH_SESSION_ID", "")}
	desktopUI.mu.Lock()
	var target *desktopTurnPublish
	for _, key := range keys {
		if key == "" {
			continue
		}
		if found, ok := desktopUI.turns[key]; ok {
			target = found
			break
		}
	}
	desktopUI.mu.Unlock()
	if target == nil {
		return errors.New("no Hermes desktop window is attached to this turn")
	}
	copied := maps.Clone(payload)
	if copied == nil {
		copied = map[string]any{}
	}
	target.publish(event, copied)
	return nil
}

func installDesktopEmitter() {
	desktopUI.mu.Lock()
	installed := desktopUI.emitterInstalled
	desktopUI.emitterInstalled = true
	desktopUI.mu.Unlock()
	if !installed {
		tools.SetDesktopEmitter(desktopEmit)
	}
}

// Publishes a process event on the stream of the session that owns the process
func desktopPublishProcess(session *tools.ProcessSession, eventType string, payload map[string]any) error {
	key := ""
	if session != nil {
		key = session.SessionKey
	}
	desktopUI.mu.Lock()
	route, ok := desktopUI.processRoutes[key]
	desktopUI.mu.Unlock()
	if key == "" || !ok {
		return errors.New("no Hermes desktop window owns this process")
	}
	live, ok := route.authority.Live(route.sessionID)
	if !ok || live == nil {
		return errors.New("the session that owns this process is no longer live")
	}
	live.EventStream.Publish(route.sessionID, payload, eventType, nil)
	return nil
}

// Installs the live output and tab close sinks of background processes once
func installDesktopProcessSinks() {
	desktopUI.mu.Lock()
	if desktopUI.processSinksInstalled {
		desktopUI.mu.Unlock()
		return
	}
	desktopUI.processSinksInstalled = true
	desktopUI.mu.Unlock()
	registry := tools.DefaultProcessRegistry()
	if registry.OnOutput == nil {
		registry.OnOutput = func(s *tools.ProcessSession, chunk string) {
			_ = desktopPublishProcess(s, "agent.terminal.output", map[string]any{"process_id": s.ID, "chunk": chunk})
		}
	}
	if registry.OnClose == nil {
		registry.OnClose = func(s *tools.ProcessSession, pid string) error {
			return desktopPublishProcess(s, "terminal.close", map[string]any{"process_id": pid})
		}
	}
}

func funcIdentity(fn any) uintptr {
	if fn == nil || reflect.ValueOf(fn).IsNil() {
		return 0
	}
	return reflect.ValueOf(fn).Pointer()
}

// Routes connection updates of operations our turns opened to the owning session, others to the wrapped hook
func installDesktopConnectionHook() {
	previous := tools.OperationOnChange
	desktopUI.mu.Lock()
	ours := desktopUI.connectionHook
	desktopUI.mu.Unlock()
	if ours != nil && previous != nil && funcIdentity(previous) == funcIdentity(ours) {
		return
	}
	var hook func(op *tools.ConnectionOperation, change, snapshot *pyjson.Object)
	hook = func(op *tools.ConnectionOperation, change, snapshot *pyjson.Object) {
		desktopUI.mu.Lock()
		route, ok := desktopUI.processRoutes[op.SessionKey]
		desktopUI.mu.Unlock()
		var live *gateway.LiveSession
		if ok {
			live, _ = route.authority.Live(route.sessionID)
		}
		if live == nil || home.Key(route.authority.ProfileID) != op.ProfileKey {
			if previous != nil {
				previous(op, change, snapshot)
			}
			return
		}
		payload := pyjson.NewObject()
		if snapshot != nil {
			for _, key := range snapshot.Keys {
				payload.Set(key, snapshot.Vals[key])
			}
		}
		settled := false
		if snapshot != nil {
			settled = snapshot.GetOr("settled_at", nil) != nil
		}
		payload.Set("settled", settled)
		if change != nil {
			for _, key := range change.Keys {
				payload.Set(key, change.Vals[key])
			}
		}
		payload.Set("owner", pyjson.ObjectOf("type", "session", "session_id", route.sessionID))
		live.EventStream.Publish(route.sessionID, tuigateway.ConnectorUIPayload(payload), "connection.update", nil)
	}
	desktopUI.mu.Lock()
	desktopUI.connectionHook = hook
	desktopUI.mu.Unlock()
	tools.OperationOnChange = hook
}

// Events and session of the turn owner, nil when the session is no longer live
func desktopOwnerStream(t *TurnRunner) (*gateway.SessionEvents, string) {
	owner := t.approvalOwner
	live, ok := owner.authority.Live(owner.sessionID)
	if !ok || live == nil {
		return nil, owner.sessionID
	}
	return live.EventStream, owner.sessionID
}

// One structured server request to the attached viewers, nil when none answered
func desktopAskResult(t *TurnRunner, a *agent.AIAgent, method string, params *pyjson.Object, timeout float64) *pyjson.Object {
	events, sessionID := desktopOwnerStream(t)
	if events == nil {
		return nil
	}
	return gateway.AskViewerResult(events, sessionID, method, params, a.InterruptRequested, &timeout, nil)
}

// One value request to the attached viewers that is never replayed to a reconnecting viewer, empty when unanswered
func desktopAsk(t *TurnRunner, a *agent.AIAgent, method string, params *pyjson.Object, timeout float64) string {
	events, sessionID := desktopOwnerStream(t)
	if events == nil {
		return ""
	}
	return gateway.AskViewer(events, sessionID, method, params, a.InterruptRequested, &timeout, false)
}

func payloadObject(payload map[string]any) *pyjson.Object {
	out := pyjson.NewObject()
	for _, key := range sortedKeys(payload) {
		out.Set(key, payload[key])
	}
	return out
}

// Applies theme values or opens a picker card that waits for the user
func desktopTheme(t *TurnRunner, a *agent.AIAgent, payload map[string]any) any {
	if _, picker := payload["picker"]; !picker {
		answer := desktopAsk(t, a, "theme.apply", payloadObject(payload), desktopThemeTimeoutS)
		if answer == "" {
			return desktopThemeUnanswered()
		}
		parsed, err := pyjson.Loads(answer)
		if err != nil {
			return pyjson.ObjectOf("success", true, "text", answer)
		}
		return parsed
	}
	started := desktopMonotonic()
	result := desktopAskResult(t, a, "theme.pick", payloadObject(payload), desktopThemePickTimeoutS)
	if result == nil {
		if desktopMonotonic()-started >= desktopThemePickTimeoutS {
			return pyjson.ObjectOf("timed_out", true)
		}
		return pyjson.ObjectOf("cancelled", true)
	}
	if picked, ok := result.GetOr("picked", nil).(*pyjson.Object); ok {
		return pyjson.ObjectOf("success", true, "picked", picked)
	}
	if said, ok := result.GetOr("said", nil).(string); ok && pystr.Strip(said) != "" {
		return pyjson.ObjectOf("said", pystr.Strip(said))
	}
	return pyjson.ObjectOf("cancelled", true)
}

// Probes the tour bridge once per session, then waits the full deadline or fails fast
func desktopTour(t *TurnRunner, a *agent.AIAgent, payload map[string]any) string {
	sessionID := t.approvalOwner.sessionID
	desktopUI.mu.Lock()
	state := desktopUI.tourBridge[sessionID]
	desktopUI.mu.Unlock()
	if state == "unanswered" {
		return desktopTourBridgeUnavailable
	}
	timeout := float64(desktopTourProbeTimeoutS)
	if state == "answered" {
		timeout = desktopTourTimeoutS
	}
	answer := desktopAsk(t, a, "tour", payloadObject(payload), timeout)
	desktopUI.mu.Lock()
	if answer != "" {
		desktopUI.tourBridge[sessionID] = "answered"
	} else if state != "answered" && !a.InterruptRequested() {
		desktopUI.tourBridge[sessionID] = "unanswered"
	}
	desktopUI.mu.Unlock()
	if answer == "" {
		return desktopTourBridgeUnavailable
	}
	return answer
}

// Short probe until the renderer answered once, then the full deadline, failing fast during a cooldown
func desktopPreviewAction(t *TurnRunner, a *agent.AIAgent, payload map[string]any) string {
	sessionID := t.approvalOwner.sessionID
	now := desktopMonotonic()
	desktopUI.mu.Lock()
	bridge, ok := desktopUI.previewBridge[sessionID]
	if !ok {
		bridge = &desktopPreviewBridge{}
		desktopUI.previewBridge[sessionID] = bridge
	}
	state := bridge.state
	if state == "unanswered" && now < bridge.retryAt {
		desktopUI.mu.Unlock()
		return desktopPreviewActionBridgeUnavailable
	}
	if state == "unanswered" {
		bridge.retryAt = now + desktopPreviewActionReprobeCooldownS
	}
	desktopUI.mu.Unlock()
	timeout := float64(desktopPreviewActionProbeTimeoutS)
	if state == "answered" {
		timeout = desktopPreviewActionTimeoutS
	}
	answer := desktopAsk(t, a, "preview.act", payloadObject(payload), timeout)
	desktopUI.mu.Lock()
	if answer != "" {
		bridge.state = "answered"
	} else if bridge.state != "answered" && !a.InterruptRequested() {
		bridge.state = "unanswered"
		bridge.retryAt = desktopMonotonic() + desktopPreviewActionReprobeCooldownS
	}
	desktopUI.mu.Unlock()
	if answer == "" {
		return desktopPreviewActionBridgeUnavailable
	}
	return answer
}

// Read request taking an optional start line and count
func desktopReadBlock(t *TurnRunner, a *agent.AIAgent, method string, timeout float64) func(start, count *int) string {
	return func(start, count *int) string {
		params := pyjson.NewObject()
		if start != nil {
			params.Set("start", *start)
		}
		if count != nil {
			params.Set("count", *count)
		}
		return desktopAsk(t, a, method, params, timeout)
	}
}

// Registry entry of one wired turn that release drops
type desktopUIToken struct {
	key     string
	publish *desktopTurnPublish
}

// Installs the window bridges for one local viewer turn on the sinks, nil without a session owner
func wireLocalDesktopUI(t *TurnRunner, a *agent.AIAgent, sinks *agent.LocalViewerSinks) *desktopUIToken {
	owner := t.approvalOwner
	if owner == nil {
		return nil
	}
	installDesktopEmitter()
	installDesktopProcessSinks()
	installDesktopConnectionHook()
	key := t.ctx.sessionID
	if key == "" {
		key = owner.sessionID
	}
	publish := &desktopTurnPublish{publish: t.publishExecution}
	processKey := t.ctx.sessionKey
	desktopUI.mu.Lock()
	desktopUI.turns[key] = publish
	if processKey != "" {
		desktopUI.processRoutes[processKey] = desktopProcessRoute{authority: owner.authority, sessionID: owner.sessionID}
	}
	desktopUI.mu.Unlock()
	sinks.ReadTerminal = desktopReadBlock(t, a, "terminal.read", desktopTerminalReadTimeoutS)
	sinks.ReadPreview = desktopReadBlock(t, a, "preview.read", desktopPreviewReadTimeoutS)
	sinks.DrivePreview = func(payload map[string]any) string { return desktopPreviewAction(t, a, maps.Clone(payload)) }
	sinks.ReadWindowBelow = func() string { return desktopAsk(t, a, "window.read", pyjson.NewObject(), desktopWindowReadTimeoutS) }
	sinks.Theme = func(payload map[string]any) any { return desktopTheme(t, a, maps.Clone(payload)) }
	sinks.Tour = func(payload map[string]any) string { return desktopTour(t, a, maps.Clone(payload)) }
	sinks.SetupBeat = func(payload map[string]any) *pyjson.Object {
		return desktopAskResult(t, a, "setup_beat", payloadObject(payload), tools.OnboardingCardTimeout)
	}
	sinks.Connection = func(payload map[string]any) { publish.publish("connection.request", maps.Clone(payload)) }
	return &desktopUIToken{key: key, publish: publish}
}

// Drops the turn routing entry unless a newer turn of the same session replaced it
func releaseLocalDesktopUI(token *desktopUIToken) {
	if token == nil {
		return
	}
	desktopUI.mu.Lock()
	defer desktopUI.mu.Unlock()
	if desktopUI.turns[token.key] == token.publish {
		delete(desktopUI.turns, token.key)
	}
}
