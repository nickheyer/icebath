package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nickheyer/icebath/internal/model/wire/codex"
	"github.com/nickheyer/icebath/internal/state"
	"github.com/nickheyer/icebath/internal/tool"
)

type InterruptRequest struct {
	Message    string
	Hard       bool
	ToolReason string
	Generation *uint64
}

const (
	ReasonUserInterrupt   = tool.ReasonUserInterrupt
	ReasonNewMessage      = tool.ReasonNewMessage
	ReasonHardStop        = tool.ReasonHardStop
	ReasonGatewayShutdown = tool.ReasonGatewayShutdown
	ReasonHostCancel      = tool.ReasonHostCancelled

	abortInterrupt = "interrupt_abort"
	abortRedirect  = "redirect_abort"

	redirectJoin = "\n\n[Additional user correction]\n"
)

func UserInterruptReason(reason string) bool {
	return reason == ReasonHardStop || reason == ReasonNewMessage || reason == ReasonUserInterrupt
}

type AbortError struct {
	Reason string
}

func (e *AbortError) Error() string {
	return "model request aborted: " + e.Reason
}

type Activity struct {
	LastAt        time.Time
	Description   string
	Provenance    state.ActivityProvenance
	CurrentTool   string
	APICalls      int
	MaxIterations int
	Generation    uint64
	Running       bool
	Budget        BudgetState
}

func (a Activity) SecondsSince(now time.Time) (float64, bool) {
	if a.LastAt.IsZero() {
		return 0, false
	}
	return float64(now.Sub(a.LastAt).Round(100*time.Millisecond)) / float64(time.Second), true
}

type steerPart struct {
	ticket string
	text   string
}

type steerDrain struct {
	text  string
	parts []steerPart
	set   bool
}

type control struct {
	redirectMu  sync.Mutex
	requested   bool
	message     string
	toolReason  string
	hard        bool
	redirect    string
	modelActive bool
	abort       context.CancelCauseFunc

	requestedFlag atomic.Bool
	hardFlag      atomic.Bool

	steerMu   sync.Mutex
	steers    []steerPart
	fates     map[string]bool
	lastDrain steerDrain

	bindMu         sync.Mutex
	turnInterrupt  *tool.Interrupt
	signalPending  bool
	signalled      bool
	executingTools bool
	codexSession   *codex.Session
}

type activityState struct {
	mu          sync.Mutex
	generation  uint64
	lastAt      time.Time
	description string
	provenance  state.ActivityProvenance
	abortClaim  *uint64
	lastPersist time.Time
	currentTool string
	apiCalls    int
	running     bool
	runStarted  time.Time
	graceCall   bool
}

func (c *control) executing() bool {
	c.bindMu.Lock()
	defer c.bindMu.Unlock()
	return c.executingTools
}

func (c *control) setExecuting(on bool) {
	c.bindMu.Lock()
	c.executingTools = on
	c.bindMu.Unlock()
}

func (c *control) codex() *codex.Session {
	c.bindMu.Lock()
	defer c.bindMu.Unlock()
	return c.codexSession
}

func (a *Agent) bindCodex(s *codex.Session) {
	a.control.bindMu.Lock()
	a.control.codexSession = s
	a.control.bindMu.Unlock()
}

func (a *Agent) bindTurnInterrupt(in *tool.Interrupt) {
	c := &a.control
	c.redirectMu.Lock()
	reason := c.toolReason
	c.redirectMu.Unlock()
	c.bindMu.Lock()
	c.turnInterrupt = in
	c.signalled = false
	pending := c.signalPending && in != nil
	if pending {
		c.signalPending = false
		c.signalled = true
	}
	c.bindMu.Unlock()
	if pending {
		in.Stop(reason)
	}
}

func (a *Agent) beginModelRequest(ctx context.Context) (context.Context, func()) {
	c := &a.control
	rctx, cancel := context.WithCancelCause(ctx)
	c.redirectMu.Lock()
	c.modelActive = true
	c.abort = cancel
	c.redirectMu.Unlock()
	return rctx, func() {
		c.redirectMu.Lock()
		c.modelActive = false
		c.abort = nil
		c.redirectMu.Unlock()
		cancel(nil)
	}
}

func (c *control) abortRequest(reason string) {
	c.redirectMu.Lock()
	abort := c.abort
	c.redirectMu.Unlock()
	if abort != nil {
		abort(&AbortError{Reason: reason})
	}
}

func (c *control) signalTools(reason string) {
	c.bindMu.Lock()
	in := c.turnInterrupt
	if in == nil {
		c.signalPending = true
	} else {
		c.signalPending = false
		c.signalled = true
	}
	c.bindMu.Unlock()
	if in != nil {
		in.Stop(reason)
	}
}

func (a *Agent) boundTurnInterrupt() *tool.Interrupt {
	c := &a.control
	c.bindMu.Lock()
	defer c.bindMu.Unlock()
	return c.turnInterrupt
}

func (a *Agent) Interrupt(r InterruptRequest) bool {
	c := &a.control
	if r.Generation != nil {
		a.activity.mu.Lock()
		if a.activity.generation != *r.Generation {
			a.activity.mu.Unlock()
			return false
		}
		claim := *r.Generation
		a.activity.abortClaim = &claim
		a.activity.mu.Unlock()
	}

	reason := r.ToolReason
	switch {
	case reason != "":
	case r.Hard:
		reason = ReasonHardStop
	case r.Message != "":
		reason = ReasonNewMessage
	default:
		reason = ReasonUserInterrupt
	}

	publish := func() {
		if !(r.ToolReason == ReasonGatewayShutdown && c.requested && UserInterruptReason(c.toolReason)) {
			c.message = r.Message
			c.toolReason = reason
		}
		c.requested = true
		c.requestedFlag.Store(true)
		a.userIntervened.Store(true)
		kind := "soft"
		if r.Hard {
			kind = "hard"
			c.hard = true
			c.hardFlag.Store(true)
		}
		slog.Info(fmt.Sprintf("Interrupt requested (%s): %s", kind, reason))
	}

	c.redirectMu.Lock()
	if r.Hard {
		a.o.Compactor.CancelBeforeCommit(true)
	}
	if r.Generation == nil {
		publish()
	} else {
		a.activity.mu.Lock()
		if a.activity.abortClaim == nil || *a.activity.abortClaim != *r.Generation {
			a.activity.mu.Unlock()
			c.redirectMu.Unlock()
			return false
		}
		a.activity.abortClaim = nil
		publish()
		a.activity.mu.Unlock()
	}
	if r.Hard {
		a.o.Compactor.CancelBeforeCommit(false)
	}
	c.redirect = ""
	c.redirectMu.Unlock()

	if s := c.codex(); s != nil {
		s.Interrupt()
	}
	c.abortRequest(abortInterrupt)
	c.signalTools(reason)
	for _, child := range a.children.Active() {
		if r.Hard {
			child.Interrupt(InterruptRequest{Message: r.Message, Hard: true, ToolReason: reason})
		} else {
			child.Interrupt(InterruptRequest{Message: r.Message})
		}
	}
	return true
}

func (a *Agent) clearInterrupt(preserveRedirect, hard bool) bool {
	c := &a.control
	c.redirectMu.Lock()
	if preserveRedirect && c.redirect == "" {
		c.redirectMu.Unlock()
		return false
	}
	c.requested = false
	c.message, c.toolReason = "", ""
	c.hard = false
	c.requestedFlag.Store(false)
	c.hardFlag.Store(false)
	if !preserveRedirect {
		c.redirect = ""
	}
	c.redirectMu.Unlock()

	c.bindMu.Lock()
	c.signalPending = false
	if c.signalled {
		c.turnInterrupt, c.signalled = nil, false
	}
	c.bindMu.Unlock()

	if hard {
		c.steerMu.Lock()
		for _, part := range c.steers {
			if part.ticket != "" {
				c.fate(part.ticket, false)
			}
		}
		c.steers = nil
		c.steerMu.Unlock()
	}
	return true
}

func (c *control) fate(ticket string, consumed bool) {
	if c.fates == nil {
		c.fates = map[string]bool{}
	}
	c.fates[ticket] = consumed
}

func joinSteers(parts []steerPart) string {
	texts := make([]string, len(parts))
	for i, p := range parts {
		texts[i] = p.text
	}
	return strings.Join(texts, "\n")
}

func (a *Agent) Steer(text string, ticket string) bool {
	cleaned := trimText(text)
	if cleaned == "" {
		return false
	}
	c := &a.control
	c.steerMu.Lock()
	c.steers = append(c.steers, steerPart{ticket: ticket, text: cleaned})
	c.steerMu.Unlock()
	a.userIntervened.Store(true)
	return true
}

func (a *Agent) Redirect(text string) bool {
	cleaned := trimText(text)
	if cleaned == "" {
		return false
	}
	c := &a.control
	if s := c.codex(); s != nil {
		c.redirectMu.Lock()
		requested := c.requested
		c.redirectMu.Unlock()
		if requested {
			return false
		}
		accepted := s.Steer(cleaned)
		if accepted {
			a.userIntervened.Store(true)
		}
		return accepted
	}

	c.bindMu.Lock()
	executing, in := c.executingTools, c.turnInterrupt
	c.bindMu.Unlock()
	if executing {
		accepted := a.Steer(cleaned, "")
		if accepted && in != nil {
			in.Yield().Request()
		}
		return accepted
	}

	c.redirectMu.Lock()
	if !c.modelActive {
		c.redirectMu.Unlock()
		return false
	}
	existing := c.redirect
	if c.requested && existing == "" {
		c.redirectMu.Unlock()
		return false
	}
	if existing != "" {
		c.redirect = existing + redirectJoin + cleaned
	} else {
		c.redirect = cleaned
	}
	c.requested = true
	c.requestedFlag.Store(true)
	c.message = ""
	a.userIntervened.Store(true)
	abort := c.abort
	c.redirectMu.Unlock()
	if abort != nil {
		abort(&AbortError{Reason: abortRedirect})
	}
	return true
}

func (a *Agent) WithdrawSteer(ticket string) bool {
	if ticket == "" {
		return false
	}
	c := &a.control
	c.steerMu.Lock()
	defer c.steerMu.Unlock()
	kept := make([]steerPart, 0, len(c.steers))
	for _, part := range c.steers {
		if part.ticket != ticket {
			kept = append(kept, part)
		}
	}
	consumed, known := c.fates[ticket]
	delete(c.fates, ticket)
	if !known {
		consumed = true
	}
	if len(kept) == len(c.steers) {
		return !consumed
	}
	c.steers = kept
	return true
}

func (a *Agent) Interrupted() bool {
	return a.control.requestedFlag.Load()
}

type interruptState struct {
	requested  bool
	hard       bool
	message    string
	toolReason string
}

func (a *Agent) interruptState() interruptState {
	c := &a.control
	c.redirectMu.Lock()
	defer c.redirectMu.Unlock()
	return interruptState{requested: c.requested, hard: c.hard, message: c.message, toolReason: c.toolReason}
}

func (s interruptState) issuer() (string, bool) {
	if s.toolReason == "" || UserInterruptReason(s.toolReason) {
		return "", false
	}
	return strings.ReplaceAll(trimText(s.toolReason), " ", "_"), true
}

func (s interruptState) skipWording() string {
	if s.toolReason == "" {
		return "Turn interrupted"
	}
	if UserInterruptReason(s.toolReason) {
		return strings.ToUpper(s.toolReason[:1]) + strings.ToLower(s.toolReason[1:])
	}
	return "Turn aborted — " + strings.ReplaceAll(s.toolReason, "_", " ")
}

func (s interruptState) duringAPICall() ExitReason {
	if issuer, ok := s.issuer(); ok {
		return ExitReason("interrupted_during_api_call(" + issuer + ")")
	}
	return ExitInterruptedDuringAPICall
}

func (s interruptState) exit() ExitReason {
	if issuer, ok := s.issuer(); ok {
		return ExitInterruptedBySystem(issuer)
	}
	return ExitInterruptedByUser
}

func (a *Agent) InterruptState() (requested, hard bool) {
	return a.control.requestedFlag.Load(), a.control.hardFlag.Load()
}

func (a *Agent) Touch(ctx context.Context, desc string, p state.ActivityProvenance) {
	a.touch(ctx, desc, p, false)
}

type CancelCause struct {
	Reason string
	User   bool
}

func (e *CancelCause) Error() string {
	return e.Reason
}

func (a *Agent) watchCancel(ctx context.Context) (stop func()) {
	release := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		select {
		case <-release:
		case <-ctx.Done():
			cause := context.Cause(ctx)
			r := InterruptRequest{Message: cause.Error(), Hard: true, ToolReason: ReasonHostCancel}
			var host *CancelCause
			if errors.As(cause, &host) && host.User {
				r.ToolReason = ""
			}
			a.Interrupt(r)
		}
	}()
	return func() {
		close(release)
		<-finished
	}
}

func (a *Agent) hasPendingRedirect() bool {
	c := &a.control
	c.redirectMu.Lock()
	defer c.redirectMu.Unlock()
	return c.redirect != ""
}

func (a *Agent) drainPendingRedirect() (string, bool) {
	c := &a.control
	c.redirectMu.Lock()
	defer c.redirectMu.Unlock()
	text := c.redirect
	c.redirect = ""
	return text, text != ""
}

func (a *Agent) drainPendingSteer() (string, bool) {
	c := &a.control
	c.steerMu.Lock()
	defer c.steerMu.Unlock()
	parts := c.steers
	c.steers = nil
	for _, part := range parts {
		if part.ticket != "" {
			c.fate(part.ticket, true)
		}
	}
	text := joinSteers(parts)
	c.lastDrain = steerDrain{text: text, parts: parts, set: true}
	return text, text != ""
}

func (a *Agent) drainLeftoverSteer() (string, bool) {
	c := &a.control
	c.steerMu.Lock()
	defer c.steerMu.Unlock()
	var kept, loose []steerPart
	for _, part := range c.steers {
		if part.ticket != "" {
			kept = append(kept, part)
		} else {
			loose = append(loose, part)
		}
	}
	c.steers = kept
	text := joinSteers(loose)
	return text, text != ""
}

func (a *Agent) requeueDrainedSteer(text string) {
	c := &a.control
	c.steerMu.Lock()
	defer c.steerMu.Unlock()
	last := c.lastDrain
	c.lastDrain = steerDrain{}
	parts := last.parts
	if !last.set || last.text != text || len(parts) == 0 {
		parts = []steerPart{{text: text}}
	}
	for _, part := range parts {
		delete(c.fates, part.ticket)
	}
	c.steers = append(c.steers, parts...)
}

func (a *Agent) pendingSteerText() string {
	c := &a.control
	c.steerMu.Lock()
	defer c.steerMu.Unlock()
	return joinSteers(c.steers)
}

func (a *Agent) touch(ctx context.Context, description string, provenance state.ActivityProvenance, forcePersist bool) {
	resolved := state.NormalizeActivityProvenance(string(provenance))
	act := &a.activity
	act.mu.Lock()
	act.generation++
	act.lastAt = a.now()
	act.description = state.BoundActivityDescription(description)
	act.provenance = resolved
	act.abortClaim = nil
	if forcePersist || terminalCompressionProvenance(resolved) {
		act.lastPersist = time.Time{}
	}
	act.mu.Unlock()
	if a.o.Bridge != nil {
		a.o.Bridge.Touched(ctx, a)
	}
	a.persistActivityIfDue(ctx)
}

func terminalCompressionProvenance(p state.ActivityProvenance) bool {
	switch p {
	case state.ProvenanceAgentCompressionTimeout, state.ProvenanceAgentCompressionCooldown, state.ProvenanceAgentCompressionTurnhold:
		return true
	}
	return false
}

func (a *Agent) persistActivityIfDue(ctx context.Context) {
	sessionID := a.SessionID()
	if sessionID == "" || a.o.Store == nil {
		return
	}
	act := &a.activity
	now := time.Now()
	act.mu.Lock()
	if !act.lastPersist.IsZero() && now.Sub(act.lastPersist) < activityPersistInterval {
		act.mu.Unlock()
		return
	}
	act.lastPersist = now
	at, description, provenance := act.lastAt, act.description, act.provenance
	act.mu.Unlock()
	if err := a.o.Store.TouchSessionActivity(ctx, sessionID, at, description, provenance); err != nil {
		slog.Debug("session activity heartbeat write failed (ignored)", "error", err)
	}
}

func (a *Agent) resetActivityLabels(ctx context.Context) {
	act := &a.activity
	act.mu.Lock()
	act.description = ""
	act.provenance = state.ProvenanceUnknown
	act.mu.Unlock()
	sessionID := a.SessionID()
	if sessionID == "" || a.o.Store == nil {
		return
	}
	if err := a.o.Store.ClearSessionActivityLabels(ctx, sessionID); err != nil {
		slog.Debug("session activity label clear failed (ignored)", "error", err)
	}
}

func (a *Agent) setCurrentTool(name string) {
	a.activity.mu.Lock()
	a.activity.currentTool = name
	a.activity.mu.Unlock()
}

func (a *Agent) setAPICalls(n int) {
	a.activity.mu.Lock()
	a.activity.apiCalls = n
	a.activity.mu.Unlock()
}

func (a *Agent) setRunning(on bool) {
	a.activity.mu.Lock()
	a.activity.running = on
	a.activity.mu.Unlock()
}

func (a *Agent) setRunStarted(at time.Time) {
	a.activity.mu.Lock()
	a.activity.runStarted = at
	a.activity.mu.Unlock()
}

func (a *Agent) setGraceCall(on bool) {
	a.activity.mu.Lock()
	a.activity.graceCall = on
	a.activity.mu.Unlock()
}

func (a *Agent) Activity() Activity {
	act := &a.activity
	act.mu.Lock()
	out := Activity{
		LastAt:        act.lastAt,
		Description:   state.BoundActivityDescription(act.description),
		Provenance:    state.NormalizeActivityProvenance(string(act.provenance)),
		CurrentTool:   act.currentTool,
		APICalls:      act.apiCalls,
		MaxIterations: a.maxIterations,
		Generation:    act.generation,
		Running:       act.running,
		Budget: BudgetState{
			RunBudget: a.limits.runBudget,
			GraceCall: act.graceCall,
		},
	}
	if !act.runStarted.IsZero() {
		out.Budget.RunElapsed = a.now().Sub(act.runStarted)
	}
	act.mu.Unlock()
	out.Budget.Used, out.Budget.Max = a.budget.Used(), a.budget.Max()
	return out
}
