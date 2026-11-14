package approval

import (
	"context"
	"log/slog"
	"os"
	"slices"
	"time"

	"github.com/nickheyer/icebath/internal/tool"
)

type waiter struct {
	prompt       tool.ApprovalPrompt
	timeout      int
	done         chan struct{}
	closed       bool
	result       string
	reason       string
	cancelled    string
	expired      bool
	acknowledged bool
	settle       func(string)
	observers    []func(string)
}

func (w *waiter) wake() {
	if !w.closed {
		w.closed = true
		close(w.done)
	}
}

func (g *Gate) Resolve(sessionKey, requestID string, c tool.Choice, all bool) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	queue := g.queues[sessionKey]
	if len(queue) == 0 {
		return 0
	}
	var targets []*waiter
	switch {
	case requestID != "":
		var kept []*waiter
		for _, w := range queue {
			if w.prompt.ID == requestID {
				targets = append(targets, w)
			} else {
				kept = append(kept, w)
			}
		}
		if len(targets) == 0 {
			return 0
		}
		queue = kept
	case all:
		targets, queue = queue, nil
	default:
		targets, queue = queue[:1], queue[1:]
	}
	if len(queue) == 0 {
		delete(g.queues, sessionKey)
	} else {
		g.queues[sessionKey] = queue
	}
	for _, w := range targets {
		w.result = c.Answer
		if c.Reason != "" {
			w.reason = c.Reason
		}
		w.wake()
	}
	return len(targets)
}

func (g *Gate) Withdraw(sessionKey, requestID, cause string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	queue := g.queues[sessionKey]
	k := slices.IndexFunc(queue, func(w *waiter) bool { return w.prompt.ID == requestID })
	if k < 0 {
		return false
	}
	w := queue[k]
	g.removeWaiterLocked(sessionKey, w)
	w.cancelled = cause
	w.wake()
	return true
}

func (g *Gate) removeWaiterLocked(sessionKey string, w *waiter) bool {
	queue := g.queues[sessionKey]
	k := slices.Index(queue, w)
	if k >= 0 {
		queue = slices.Delete(queue, k, k+1)
	}
	if len(queue) == 0 {
		delete(g.queues, sessionKey)
	} else {
		g.queues[sessionKey] = queue
	}
	return k >= 0
}

func (g *Gate) Pending(sessionKey string) []tool.ApprovalPrompt {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []tool.ApprovalPrompt
	for _, w := range g.queues[sessionKey] {
		p := w.prompt
		p.PatternKeys = slices.Clone(p.PatternKeys)
		out = append(out, p)
	}
	return out
}

func (g *Gate) find(sessionKey, requestID string, fn func(w *waiter)) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, w := range g.queues[sessionKey] {
		if w.prompt.ID == requestID {
			fn(w)
			return true
		}
	}
	return false
}

func (g *Gate) Settle(sessionKey, requestID string, fn func(reason string)) bool {
	return g.find(sessionKey, requestID, func(w *waiter) { w.settle = fn })
}

func (g *Gate) Observe(sessionKey, requestID string, fn func(reason string)) bool {
	return g.find(sessionKey, requestID, func(w *waiter) { w.observers = append(w.observers, fn) })
}

func (g *Gate) Ack(sessionKey, requestID string) bool {
	return g.find(sessionKey, requestID, func(w *waiter) { w.acknowledged = true })
}

func (g *Gate) HasBlocking(sessionKey string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.queues[sessionKey]) > 0
}

func (g *Gate) PendingCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := 0
	for _, q := range g.queues {
		n += len(q)
	}
	return n
}

func (g *Gate) WakeAll(sessionKey string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, w := range g.queues[sessionKey] {
		w.wake()
	}
	delete(g.queues, sessionKey)
}

type gatewayDecision struct {
	resolved     bool
	choice       string
	reason       string
	cancelled    string
	notifyFailed bool
}

const (
	waitSet         = "set"
	waitInterrupted = "interrupted"
	waitTimeout     = "timeout"
)

func (g *Gate) poll(ctx context.Context, c *tool.Call, key string, done <-chan struct{}, timeout int, interruptLog string) string {
	deadline := time.Now().Add(time.Duration(max(timeout, 0)) * time.Second)
	hb := newHeartbeat(c, "waiting for user approval")
	exit := g.humanWait(ctx, key)
	defer exit()
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			slog.InfoContext(ctx, interruptLog, "session", key)
			return waitInterrupted
		}
		if !time.Now().Before(deadline) {
			return waitTimeout
		}
		select {
		case <-ctx.Done():
			slog.InfoContext(ctx, interruptLog, "session", key)
			return waitInterrupted
		case <-done:
			return waitSet
		case <-timer.C:
			return waitTimeout
		case <-ticker.C:
			hb.beat()
		}
	}
}

func (g *Gate) cancelCause(ctx context.Context, state string, w *waiter) string {
	switch {
	case state == waitInterrupted:
		if reason, ok := tool.InterruptReason(ctx); ok && reason != "" {
			return reason
		}
		return "turn interrupted"
	case state == waitSet:
		g.mu.Lock()
		defer g.mu.Unlock()
		if w.result == "" {
			if w.cancelled != "" {
				return w.cancelled
			}
			return "the turn ended before the prompt was answered"
		}
	}
	return ""
}

func hookChoice(resolved bool, choice, cancelled string) string {
	switch {
	case cancelled != "":
		return "cancelled"
	case resolved && choice != "":
		return choice
	}
	return "timeout"
}

func (g *Gate) awaitLeader(ctx context.Context, c *tool.Call, key string, leader *waiter, payload tool.ApprovalPrompt) (gatewayDecision, bool) {
	pre := approvalHook(c, payload, "")
	pre.Coalesced = true
	g.o.Hooks.Approval(ctx, "pre_approval_request", pre)
	state := g.poll(ctx, c, key, leader.done, leader.timeout, "Coalesced approval wait interrupted — returning deny")
	g.mu.Lock()
	if state == waitSet && leader.result == "" && leader.expired {
		state = waitTimeout
	}
	result, reason := leader.result, leader.reason
	g.mu.Unlock()
	cancelled := g.cancelCause(ctx, state, leader)
	choice, resolved := "", false
	switch state {
	case waitInterrupted:
		choice, resolved = "deny", true
	case waitTimeout:
	default:
		choice, resolved = result, result != ""
	}
	if choice == "once" {
		return gatewayDecision{}, false
	}
	post := approvalHook(c, payload, hookChoice(resolved, choice, cancelled))
	post.Coalesced = true
	post.Cancelled = cancelled
	g.o.Hooks.Approval(ctx, "post_approval_response", post)
	return gatewayDecision{resolved: resolved, choice: choice, reason: reason, cancelled: cancelled}, true
}

func (g *Gate) dropWaiter(ctx context.Context, key string, w *waiter, state string) string {
	g.mu.Lock()
	choice := w.result
	g.removeWaiterLocked(key, w)
	settle, observers := w.settle, w.observers
	w.settle, w.observers = nil, nil
	if choice == "" && (state == waitTimeout || state == "notify_failed") {
		w.expired = state == waitTimeout
		if state == "notify_failed" {
			w.cancelled = "the approval prompt could not be delivered"
		}
		w.wake()
	}
	g.mu.Unlock()
	if settle == nil && len(observers) == 0 {
		return choice
	}
	reason := state
	switch {
	case state == waitTimeout && choice != "":
		reason = "resolved"
	case state == waitSet && choice != "":
		reason = "resolved"
	case state == waitSet:
		reason = "session_closed"
	}
	callbacks := observers
	if settle != nil {
		callbacks = append([]func(string){settle}, observers...)
	}
	for _, fn := range callbacks {
		fn(reason)
	}
	return choice
}

func (g *Gate) awaitGateway(ctx context.Context, c *tool.Call, data tool.ApprovalPrompt, surface string) gatewayDecision {
	key := sessionKey(c)
	if surface == "gateway" {
		surface = gatewaySurface(c.Origin)
	}
	keys := slices.Clone(data.PatternKeys)
	payloadKeys := keys
	if payloadKeys == nil {
		payloadKeys = []string{data.PatternKey}
	}
	payload := tool.ApprovalPrompt{Command: data.Command, Description: data.Description, PatternKey: data.PatternKey,
		PatternKeys: payloadKeys, SessionKey: key, Surface: surface}
	timeout := approvalTimeout(ctx, g.o.Config)
	data.ID = newRequestID()
	data.SessionKey = key
	data.Timeout = time.Duration(timeout) * time.Second
	w := &waiter{prompt: data, timeout: timeout, done: make(chan struct{})}
	slot := batchSlot(ctx)
	for {
		g.mu.Lock()
		var leader *waiter
		if slot == nil || !slot.Preparing() {
			for _, e := range g.queues[key] {
				if e.prompt.Command == data.Command && slices.Equal(e.prompt.PatternKeys, keys) {
					leader = e
					break
				}
			}
		}
		if leader == nil {
			if slot != nil {
				if err := slot.Register(func() { g.withdrawDeny(key, w) }); err != nil {
					g.mu.Unlock()
					return gatewayDecision{cancelled: err.Error()}
				}
			}
			g.queues[key] = append(g.queues[key], w)
			g.mu.Unlock()
			break
		}
		g.mu.Unlock()
		if adopted, ok := g.awaitLeader(ctx, c, key, leader, payload); ok {
			return adopted
		}
	}
	var res gatewayDecision
	g.humanInput(ctx, c, "approval", payload.Command, key, func() string {
		g.o.Hooks.Approval(ctx, "pre_approval_request", approvalHook(c, payload, ""))
		if err := c.Surface.NotifyApproval(ctx, w.prompt); err != nil {
			slog.WarnContext(ctx, "Gateway approval notify failed", "error", err)
			g.dropWaiter(ctx, key, w, "notify_failed")
			g.o.Hooks.Approval(ctx, "post_approval_response", approvalHook(c, payload, "notify_failed"))
			res = gatewayDecision{notifyFailed: true}
			return "notify_failed"
		}
		if slot != nil {
			slot.Published()
		}
		state := g.poll(ctx, c, key, w.done, timeout, "Approval wait interrupted — returning deny")
		cancelled := g.cancelCause(ctx, state, w)
		if state == waitInterrupted {
			g.mu.Lock()
			w.cancelled = cancelled
			w.wake()
			g.mu.Unlock()
		}
		choice := g.dropWaiter(ctx, key, w, state)
		if state == waitInterrupted {
			choice = "deny"
		}
		resolved := state != waitTimeout || choice != ""
		g.mu.Lock()
		reason := w.reason
		g.mu.Unlock()
		outcome := hookChoice(resolved, choice, cancelled)
		post := approvalHook(c, payload, outcome)
		post.Cancelled = cancelled
		g.o.Hooks.Approval(ctx, "post_approval_response", post)
		res = gatewayDecision{resolved: resolved, choice: choice, reason: reason, cancelled: cancelled}
		return outcome
	})
	return res
}

func (g *Gate) withdrawDeny(key string, w *waiter) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.removeWaiterLocked(key, w) {
		w.result = "deny"
		w.wake()
	}
}

func (g *Gate) humanInput(ctx context.Context, c *tool.Call, kind, prompt, key string, wait func() string) {
	g.HumanInput(ctx, c, kind, prompt, key, wait)
}

func (g *Gate) HumanInput(ctx context.Context, c *tool.Call, kind, prompt, sessionKey string, wait func() string) {
	platform := os.Getenv("ICEBATH_PLATFORM")
	if platform == "" {
		platform = c.Origin.Platform
	}
	if platform == "" {
		platform = c.Origin.Source
	}
	if platform == "" {
		platform = "cli"
	}
	request := tool.HumanInput{Kind: kind, RequestID: newRequestID(), SessionID: c.SessionID, SessionKey: sessionKey,
		Platform: platform, Prompt: g.o.Redactor.Approval(ctx, prompt, true, false)}
	g.o.Hooks.HumanInput(ctx, "on_human_input_request", request)
	outcome := ""
	defer func() {
		if outcome == "" {
			outcome = "error"
		}
		request.Outcome = outcome
		g.o.Hooks.HumanInput(ctx, "on_human_input_resolved", request)
	}()
	outcome = wait()
}

func approvalHook(c *tool.Call, p tool.ApprovalPrompt, choice string) tool.ApprovalHook {
	return tool.ApprovalHook{Prompt: p, Choice: choice, TurnID: c.TurnID, ToolCallID: c.ID, SessionID: c.SessionID}
}
