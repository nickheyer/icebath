package gateway

import (
	"context"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// tui_gateway/server.py _consume_typed_stop_phrase without emit, assigned in cmd/icebath/wiring.go to the tuiserver port
var ConsumeTypedStopPhrase func(ctx context.Context, text, sessionID string) (bool, error)

// Stop submissions this owner consumed, keyed by subject, profile, session and request id, oldest first
type consumedVoiceStops struct {
	sync.Mutex
	order []voiceStopKey
	seen  map[voiceStopKey]bool
}

type voiceStopKey struct{ subject, profile, session, request string }

var voiceStopsByAuthority sync.Map

// Busy preference and correction verbs of a connection keyed by method name
func busyControlHandlers(c *AuthorityConnection) map[string]controlHandler {
	return map[string]controlHandler{
		"config.get": func(ctx context.Context, ref SessionRef, params *pyjson.Object) (any, error) {
			return busyConfig(ctx, c.authority, c.actor, ref, params, false)
		},
		"config.set": func(ctx context.Context, ref SessionRef, params *pyjson.Object) (any, error) {
			return busyConfig(ctx, c.authority, c.actor, ref, params, true)
		},
		"session.steer": func(ctx context.Context, ref SessionRef, params *pyjson.Object) (any, error) {
			return correct(ctx, c, ref, params, "steer")
		},
		"session.redirect": func(ctx context.Context, ref SessionRef, params *pyjson.Object) (any, error) {
			return correct(ctx, c, ref, params, "redirect")
		},
	}
}

// Agent seam taking a redirect of the running turn
type RedirectReceiver interface {
	Redirect(text string) bool
}

// Steers or redirects the running claim, latching for an agent not yet built and refusing managed workers
func correct(ctx context.Context, c *AuthorityConnection, ref SessionRef, params *pyjson.Object, verb string) (*pyjson.Object, error) {
	text, isStr := params.GetOr("text", nil).(string)
	echoed := params.GetOr("execution_generation", nil)
	exact, isInt := strictInt(echoed)
	if paramsOutside(params, "session_id", "profile", "text", "execution_generation") || !isStr || pystr.Strip(text) == "" || !isInt {
		return nil, runtimeRefusal("invalid_params")
	}
	generation := int64(absentGeneration)
	if exact.IsInt64() {
		generation = exact.Int64()
	}
	a := c.authority
	if err := authorizeBusy(ctx, a, c.actor, ref, params, "session:control"); err != nil {
		return nil, err
	}
	stopped, err := c.typedVoiceStop(ctx, ref, params)
	if err != nil {
		return nil, err
	}
	if stopped {
		return pyjson.ObjectOf("voice_stopped", true), nil
	}
	live, err := liveSession(a, ref.SessionID)
	if err != nil {
		return nil, err
	}
	live.EventStream.Lock.Lock()
	defer live.EventStream.Lock.Unlock()
	if err := a.checkApprovalGeneration(ctx, ref.SessionID, generation); err != nil {
		return nil, err
	}
	a.mu.Lock()
	_, managed := a.managedWorkers[ref.SessionID]
	a.mu.Unlock()
	if managed {
		return nil, runtimeRefusal("unsupported_control")
	}
	var latch *string
	if verb == "steer" {
		latch = &text
	}
	agent, latched, err := a.SteerAgent(ctx, ref, generation, latch)
	if err != nil {
		return nil, err
	}
	if latched {
		return pyjson.ObjectOf("status", "queued", "text", text, "execution_generation", echoed, "authority_epoch", a.Epoch), nil
	}
	if agent == nil {
		return pyjson.ObjectOf("status", "rejected", "text", text, "execution_generation", echoed,
			"authority_epoch", a.Epoch, "delivered", false), nil
	}
	var accepted bool
	switch verb {
	case "steer":
		receiver, ok := agent.(SteerReceiver)
		if !ok {
			return nil, runtimeRefusal("execution_not_ready")
		}
		accepted = receiver.Steer(text)
	default:
		receiver, ok := agent.(RedirectReceiver)
		if !ok {
			return nil, runtimeRefusal("execution_not_ready")
		}
		accepted = receiver.Redirect(text)
	}
	status := "rejected"
	if accepted {
		status = map[string]string{"steer": "queued", "redirect": "redirected"}[verb]
	}
	return pyjson.ObjectOf("status", status, "text", text, "execution_generation", echoed, "authority_epoch", a.Epoch), nil
}

// Identity of a typed stop submission, false when the frame names no submission or input id
func (c *AuthorityConnection) voiceStopKey(ref SessionRef, params *pyjson.Object) (voiceStopKey, bool) {
	requestID := params.GetOr("submission_id", nil)
	if !pyrepr.Truthy(requestID) {
		requestID = params.GetOr("input_id", nil)
	}
	id, ok := requestID.(string)
	if !ok || id == "" {
		return voiceStopKey{}, false
	}
	return voiceStopKey{c.actor.Subject, ref.ProfileID, ref.SessionID, id}, true
}

// Consumed stops of an authority, created on first use
func authorityVoiceStops(a *SessionAuthority) *consumedVoiceStops {
	stops, _ := voiceStopsByAuthority.LoadOrStore(a, &consumedVoiceStops{seen: map[voiceStopKey]bool{}})
	return stops.(*consumedVoiceStops)
}

// True for a retry of a stop this owner already consumed, answered again by its request identity
func (c *AuthorityConnection) voiceStopReplay(ctx context.Context, ref SessionRef, params *pyjson.Object) (bool, error) {
	key, ok := c.voiceStopKey(ref, params)
	if !ok {
		return false, nil
	}
	stops := authorityVoiceStops(c.authority)
	stops.Lock()
	consumed := stops.seen[key]
	stops.Unlock()
	if !consumed || !c.subscribed(ref.SessionID) {
		return false, nil
	}
	return true, c.authority.Authorize(ctx, c.actor, ref, "session:submit")
}

// Remembers a consumed stop submission, the newest 256 kept
func (c *AuthorityConnection) recordVoiceStop(ref SessionRef, params *pyjson.Object) {
	key, ok := c.voiceStopKey(ref, params)
	if !ok {
		return
	}
	stops := authorityVoiceStops(c.authority)
	stops.Lock()
	defer stops.Unlock()
	if stops.seen[key] {
		for i, k := range stops.order {
			if k == key {
				stops.order = append(stops.order[:i], stops.order[i+1:]...)
				break
			}
		}
	}
	stops.seen[key] = true
	stops.order = append(stops.order, key)
	for len(stops.order) > 256 {
		delete(stops.seen, stops.order[0])
		stops.order = stops.order[1:]
	}
}

// Whether this connection is attached to the session
func (c *AuthorityConnection) subscribed(sessionID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.subscriptions[sessionID]
	return ok
}

// A bare stop phrase typed while a local session's voice chat is on ends the chat and is never admitted
func (c *AuthorityConnection) typedVoiceStop(ctx context.Context, ref SessionRef, params *pyjson.Object) (bool, error) {
	text, ok := params.GetOr("text", nil).(string)
	if !ok || !c.subscribed(ref.SessionID) {
		return false, nil
	}
	if err := c.authority.Authorize(ctx, c.actor, ref, "session:submit"); err != nil {
		return false, err
	}
	live, held := c.authority.live(ref.SessionID)
	if !held || live.Source == nil || live.Source.Platform != PlatformLocal {
		return false, nil
	}
	enabled, err := LocalVoiceEnabled(ref.SessionID)
	if err != nil {
		return false, err
	}
	if enabled == nil || !*enabled {
		return false, nil
	}
	return forwardOffThread(ctx, func() (bool, error) { return ConsumeTypedStopPhrase(ctx, text, ref.SessionID) })
}
