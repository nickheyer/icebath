package plugins

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

var pluginMessageTimeout = 30 * time.Second

const noSessionDetail = "no session is bound in the calling context (a tool call, slash command or hook fired for a session)"

// Refusal result with an error code and a detail only when one is given
func messageErr(code, detail string) map[string]any {
	out := map[string]any{"ok": false, "error": code}
	if detail != "" {
		out["detail"] = detail
	}
	return out
}

// One gateway operation still running past the wait, its outcome landing when done closes
type messageOp struct {
	done   chan struct{}
	result map[string]any
	err    error
}

// Whether the operation has not finished yet
func (op *messageOp) running() bool {
	if op == nil {
		return false
	}
	select {
	case <-op.done:
		return false
	default:
		return true
	}
}

// Operation attached to the handle, nil when none ran
func (h *PluginMessage) pendingOp() *messageOp {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.pending
}

// Handle of one posted message whose edits replace it in place and stay ordered behind a slow operation
type PluginMessage struct {
	mu        sync.Mutex
	pluginID  string
	target    any
	result    map[string]any
	messageID any
	pending   *messageOp
}

// Handle over a resolved target or nil for a refusal, its message id read from the result
func newPluginMessage(pluginID string, target any, result map[string]any) *PluginMessage {
	return &PluginMessage{pluginID: pluginID, target: target, result: result, messageID: result["message_id"]}
}

// Result of the last operation on the handle
func (h *PluginMessage) Result() map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.result
}

// Message id the handle follows, nil before any message landed
func (h *PluginMessage) MessageID() any {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.messageID
}

// Whether the last operation succeeded
func (h *PluginMessage) OK() bool { return pyrepr.Truthy(h.Result()["ok"]) }

// Error code of the last operation, nil after a success
func (h *PluginMessage) ErrorCode() any { return h.Result()["error"] }

// Whether an operation the gateway has not finished is still attached
func (h *PluginMessage) Pending() bool { return h.pendingOp().running() }

func (h *PluginMessage) String() string {
	return fmt.Sprintf("<PluginMessage ok=%s message_id=%s error=%s>", pyrepr.Repr(h.OK()), pyrepr.Repr(h.MessageID()), pyrepr.Repr(h.ErrorCode()))
}

// Takes over the target, result and message id of another handle
func (h *PluginMessage) adopt(other *PluginMessage) {
	other.mu.Lock()
	target, result, id := other.target, other.result, other.messageID
	other.mu.Unlock()
	h.mu.Lock()
	h.target, h.result, h.messageID = target, result, id
	h.mu.Unlock()
}

// Refusal for a handle without a conversation carrying its original error and detail
func (h *PluginMessage) refused() map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.target != nil {
		return nil
	}
	code := "no_conversation"
	if e := h.result["error"]; pyrepr.Truthy(e) {
		code = pyrepr.Str(e)
	}
	detail := ""
	if d, ok := h.result["detail"]; ok {
		detail = pyrepr.Str(d)
	}
	return messageErr(code, detail)
}

// Records an operation result and follows a new message id when it succeeded with one
func (h *PluginMessage) settle(result map[string]any) map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.result = result
	if pyrepr.Truthy(result["ok"]) && pyrepr.Truthy(result["message_id"]) {
		h.messageID = result["message_id"]
	}
	return result
}

func (h *PluginMessage) setResult(result map[string]any) {
	h.mu.Lock()
	h.result = result
	h.mu.Unlock()
}

// Gateway edit of the followed message unless the handle was refused
func (h *PluginMessage) edit(ctx context.Context, text string) (map[string]any, error) {
	if refused := h.refused(); refused != nil {
		return refused, nil
	}
	fn, err := needService("EditPluginMessage", func(s *Services) func(ctx context.Context, target, messageID any, text, pluginID string) map[string]any {
		return s.EditPluginMessage
	})
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	target, id := h.target, h.messageID
	h.mu.Unlock()
	return h.settle(fn(ctx, target, id, text, h.pluginID)), nil
}

// Edit for code already running on the gateway loop, waiting for a pending operation first
func (h *PluginMessage) AEdit(ctx context.Context, text string) (map[string]any, error) {
	if op := h.pendingOp(); op.running() {
		<-op.done
		if op.err != nil {
			return nil, op.err
		}
	}
	return h.edit(ctx, text)
}

// Replaces the message text in place or posts anew where the surface cannot edit, never raising
func (h *PluginMessage) Edit(ctx context.Context, text string) map[string]any {
	if onGatewayLoop(ctx) {
		return messageErr("on_event_loop", "called from the gateway event loop: use await handle.aedit(...)")
	}
	if op := h.pendingOp(); op.running() {
		select {
		case <-op.done:
			if op.err != nil {
				logDebug("plugin_message: previous operation failed", "error", op.err)
			}
		case <-time.After(pluginMessageTimeout):
			return messageErr("timeout", "the previous operation on this message has not finished")
		}
	}
	if refused := h.refused(); refused != nil {
		return refused
	}
	return runOnGatewayLoop(ctx, func(lctx context.Context) (map[string]any, error) { return h.edit(lctx, text) }, h)
}

// Live gateway runner and whether ctx runs on its loop, nil when no gateway runs in this process
func gatewayLoop(ctx context.Context) (any, bool) {
	s := installedServices()
	if s == nil || s.GatewayLoop == nil {
		return nil, false
	}
	return s.GatewayLoop(ctx)
}

// Whether ctx runs on the live gateway loop
func onGatewayLoop(ctx context.Context) bool {
	runner, on := gatewayLoop(ctx)
	return runner != nil && on
}

// Runs op as the gateway operation of the handle, waiting the timeout before leaving it pending
func runOnGatewayLoop(ctx context.Context, op func(context.Context) (map[string]any, error), h *PluginMessage) map[string]any {
	runner, on := gatewayLoop(ctx)
	if runner == nil {
		return messageErr("gateway_unavailable", "no live gateway is running in this process")
	}
	if on {
		return messageErr("on_event_loop", "called from the gateway event loop: use the async form (await ...)")
	}
	pending := &messageOp{done: make(chan struct{})}
	h.mu.Lock()
	h.pending = pending
	h.mu.Unlock()
	loopCtx := context.WithoutCancel(ctx)
	go func() {
		defer close(pending.done)
		v, err := guardedValue(func() (any, error) { return op(loopCtx) })
		pending.result, _ = v.(map[string]any)
		pending.err = err
	}()
	select {
	case <-pending.done:
		if pending.err != nil {
			logWarn("plugin_message: gateway hand-off failed", "error", pending.err)
			return messageErr("send_failed", pystr.Slice(pending.err.Error(), 0, 512))
		}
		return pending.result
	case <-time.After(pluginMessageTimeout):
		result := messageErr("timeout", fmt.Sprintf("the gateway did not answer within %.0fs; the operation "+
			"is still running and its result will land on this handle", pluginMessageTimeout.Seconds()))
		h.setResult(result)
		return result
	}
}

// Resolves the session conversation and posts the first message, a refusal handle when it cannot
func postFor(ctx context.Context, c *Context, text, sessionKey string, route any) (*PluginMessage, error) {
	pluginID := c.PluginID()
	runner, _ := gatewayLoop(ctx)
	var result map[string]any
	switch {
	case sessionKey == "":
		result = messageErr("no_conversation", noSessionDetail)
	case runner == nil:
		result = messageErr("gateway_unavailable", "no live gateway is running in this process")
	default:
		resolve, err := needService("ResolvePluginMessageTarget", func(s *Services) func(context.Context, any, string, any, string, string) (any, map[string]any, error) {
			return s.ResolvePluginMessageTarget
		})
		if err != nil {
			return nil, err
		}
		post, err := needService("PostPluginMessage", func(s *Services) func(context.Context, any, string, string) map[string]any {
			return s.PostPluginMessage
		})
		if err != nil {
			return nil, err
		}
		target, refusal, err := resolve(ctx, runner, sessionKey, route, c.manager.home, pluginID)
		if err != nil {
			return nil, err
		}
		if target == nil {
			result = refusal
			if len(refusal) == 0 {
				result = messageErr("no_conversation", "")
			}
		} else {
			result = post(ctx, target, text, pluginID)
			if pyrepr.Truthy(result["ok"]) {
				return newPluginMessage(pluginID, target, result), nil
			}
		}
	}
	logInfo(fmt.Sprintf("plugin_message plugin=%s op=post refused: %s", pluginID, pyrepr.Str(result["error"])))
	return newPluginMessage(pluginID, nil, result), nil
}

// Session key and live route bound to ctx by the gateway, empty and nil when none is bound
func boundMessageRoute(ctx context.Context) (string, any) {
	s := installedServices()
	if s == nil {
		return "", nil
	}
	key := ""
	if s.BoundSessionKey != nil {
		key = s.BoundSessionKey(ctx)
	}
	var route any
	if s.BoundRoute != nil {
		route = s.BoundRoute(ctx)
	}
	return key, route
}

// Post for code already running on the gateway loop
func apostMessage(ctx context.Context, c *Context, text string) (*PluginMessage, error) {
	key, route := boundMessageRoute(ctx)
	return postFor(ctx, c, text, key, route)
}

// Posts text to the calling session from a worker, the session read on the caller before the gateway send
func postMessage(ctx context.Context, c *Context, text string) *PluginMessage {
	key, route := boundMessageRoute(ctx)
	if key == "" {
		return newPluginMessage(c.PluginID(), nil, messageErr("no_conversation", noSessionDetail))
	}
	handle := newPluginMessage(c.PluginID(), nil, messageErr("timeout", ""))
	result := runOnGatewayLoop(ctx, func(lctx context.Context) (map[string]any, error) {
		other, err := postFor(lctx, c, text, key, route)
		if err != nil {
			return nil, err
		}
		handle.adopt(other)
		return handle.Result(), nil
	}, handle)
	handle.mu.Lock()
	refusedNow := handle.target == nil
	handle.mu.Unlock()
	if refusedNow && !handle.Pending() {
		handle.setResult(result)
	}
	return handle
}
