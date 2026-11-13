package agent

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pycopy"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/observability"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Fraction below the compaction trigger where the real prompt count starts a prepared pass
const PrepareBandRatio = 0.12

const failedPassBackoffSeconds = 60.0

const waitPollSeconds = 0.5

var preparedSlots = pythread.NewBoundedSemaphore(2)

var summaryCallState = []string{
	"summary_model", "_last_aux_model_failure_error", "_last_aux_model_failure_model", "_last_aux_resolved_model",
}

var summaryCallTelemetry = []string{
	"aux_prompt_tokens", "aux_output_reservation", "aux_provider", "aux_model", "effective_aux_context", "fit_margin",
	"aux_call_duration_ms", "queue_wait_ms", "prompt_build_ms", "time_to_first_progress_ms", "summary_generation_ms",
	"summary_input_chars", "summary_input_sampled_chars", "summary_input_omitted_chars", "summary_input_record_count",
	"summary_input_sampled_record_count", "summary_input_elided_record_count", "fallback_used", "failure_class",
}

var waitCancelReasons = map[string]bool{"wait_timeout": true, "host_cancelled": true}

// PreparedIdentity is one message's structure, durable uid (empty for none) and text digest
type PreparedIdentity struct {
	Structure string
	UID       string
	Digest    string
}

type preparedMiss struct {
	Code string
	Text string
}

func (m *preparedMiss) String() string { return m.Text }

// HandoffScan is the result of the compressor window handoff scan
type HandoffScan struct {
	TurnsToSummarize      []map[string]any
	SummaryIndices        map[int]bool
	TailStart             int
	PreviousSummaryBefore *string
	HasUserTurnBefore     *bool
}

// PreparedCompactionCompressor is the compressor surface a prepared pass plans on, copies and splices into
type PreparedCompactionCompressor interface {
	ContextEngine
	PreparedCompactionState() *PreparedCompaction
	CompressorSessionID() string
	AwaitingRealUsageAfterCompression() bool
	FallbackCompressionStreak() int
	MicroCompactEnabled() bool
	SummaryFailureCooldownUntil() float64
	PlanCompactionWindow(messages []map[string]any) (working []map[string]any, pruned, start, end int)
	ScanWindowHandoffs(messages []map[string]any, start, end int, turns []map[string]any) HandoffScan
	DeriveAutoFocusTopic(messages []map[string]any) *string
	ShallowCopy() PreparedCompactionCompressor
	SetSessionDB(db SessionStore)
	SetCompressionCancelledCheck(check func() bool)
	BeginCompressionTelemetry(currentTokens *int)
	GenerateSummary(ctx context.Context, turns []map[string]any, focusTopic *string) (*string, error)
	ActiveCompressionTelemetry() *pyjson.Object
	PreviousSummary() string
	LastSummaryError() string
	SummaryCallAttr(name string) any
	SetSummaryCallAttr(name string, value any)
	SummaryFailureFlag(flag string) bool
	RecordSummarySuccess(summary string)
}

// Counts one prepared compaction event in opt in shared metrics, swapped by tests to observe events
var recordPrepared = observability.RecordPreparedCompaction

type preparedWindow struct {
	SessionID     string
	CompressStart int
	CompressEnd   int
	Identities    []PreparedIdentity
}

// PreparedSummary is a completed pass summary with the window it covers and the call state it left
type PreparedSummary struct {
	preparedWindow
	Summary   string
	Body      string
	CallState map[string]any
	Telemetry map[string]any
	Waited    *float64
}

type pendingPass struct {
	window       preparedWindow
	done         pythread.Event
	cancelled    pythread.Event
	mu           sync.Mutex
	cancelReason string
	onProgress   func() error
	onReading    func(float64)
}

func newPendingPass(window preparedWindow) *pendingPass {
	return &pendingPass{window: window, cancelReason: "superseded"}
}

// Records the first cancel reason and closes the pass stream
func (p *pendingPass) cancel(reason string) {
	p.mu.Lock()
	if !p.cancelled.IsSet() {
		p.cancelReason = reason
	}
	p.mu.Unlock()
	p.cancelled.Set()
}

func (p *pendingPass) reason() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cancelReason
}

func (p *pendingPass) setHooks(progress func() error, reading func(float64)) {
	p.mu.Lock()
	p.onProgress, p.onReading = progress, reading
	p.mu.Unlock()
}

// Forwards pass progress to the waiting compaction's progress hook
func (p *pendingPass) touch() error {
	p.mu.Lock()
	hook := p.onProgress
	p.mu.Unlock()
	if hook != nil {
		return hook()
	}
	return nil
}

// Forwards pass prompt reading to the waiting compaction's reading hook
func (p *pendingPass) read(seconds float64) {
	p.mu.Lock()
	hook := p.onReading
	p.mu.Unlock()
	if hook != nil {
		hook(seconds)
	}
}

func pyGetDefault(m map[string]any, key string, def any) any {
	if v, ok := m[key]; ok {
		return v
	}
	return def
}

// Role with tool call ids and names, the durable uid, and a digest of the text no uid vouches for
func messageIdentity(msg any) PreparedIdentity {
	m, ok := msg.(map[string]any)
	if !ok {
		return PreparedIdentity{Structure: "?"}
	}
	role := pyStrOr(m["role"], "")
	uid, _ := state.MessageUIDOrNone(m)
	if role == "tool" {
		return PreparedIdentity{Structure: "tool\x1f" + pyStrOr(m["tool_call_id"], ""), UID: uid}
	}
	text := pystr.Strip(memory.SanitizeContext(state.FlattenMessageText(m["content"], "\n")))
	digest := hex.EncodeToString(provider.Blake2bSum(pystr.EncodeUTF8Replace(text), 8))
	parts := []string{role}
	calls, _ := m["tool_calls"].([]any)
	if mc, ok := m["tool_calls"].([]map[string]any); ok {
		for _, c := range mc {
			calls = append(calls, c)
		}
	}
	for _, tc := range calls {
		if d, ok := pyjson.Dict(tc); ok {
			name := ""
			fn := d["function"]
			if !pyrepr.Truthy(fn) {
				fn = map[string]any{}
			}
			if fd, ok := pyjson.Dict(fn); ok {
				name = pyrepr.Str(pyGetDefault(fd, "name", ""))
			}
			parts = append(parts, pyrepr.Str(pyGetDefault(d, "id", ""))+":"+name)
		} else {
			parts = append(parts, toolCallAttrIdentity(tc))
		}
	}
	return PreparedIdentity{Structure: strings.Join(parts, "\x1f"), UID: uid, Digest: digest}
}

// Id and function name of an object tool call, empty for attributes it lacks
func toolCallAttrIdentity(tc any) string {
	switch c := tc.(type) {
	case *state.ToolCall:
		return c.ID + ":" + c.Function.Name
	case state.ToolCall:
		return c.ID + ":" + c.Function.Name
	}
	return ":"
}

// Identity of each message in the prefix ending at end
func PrefixIdentities(messages []map[string]any, end int) []PreparedIdentity {
	end = min(max(end, 0), len(messages))
	out := make([]PreparedIdentity, 0, end)
	for _, msg := range messages[:end] {
		out = append(out, messageIdentity(msg))
	}
	return out
}

// Why the window cannot splice with the head ending at start, nil when it can
func preparedMismatch(window preparedWindow, messages []map[string]any, sessionID string, start int) *preparedMiss {
	if window.SessionID != sessionID {
		return &preparedMiss{"other_session", "another session"}
	}
	if window.CompressStart != start {
		return &preparedMiss{"head_moved", fmt.Sprintf("head moved (%d -> %d)", window.CompressStart, start)}
	}
	if window.CompressEnd > len(messages) {
		return &preparedMiss{"transcript_shorter", "transcript shorter than the prepared window"}
	}
	for idx := 0; idx < min(len(window.Identities), len(messages)); idx++ {
		was, now := window.Identities[idx], messageIdentity(messages[idx])
		if now.Structure != was.Structure {
			return &preparedMiss{"shape_changed", fmt.Sprintf("message %d changed shape", idx)}
		}
		if was.UID != "" && now.UID != "" {
			if was.UID != now.UID {
				return &preparedMiss{"rewound", fmt.Sprintf("message %d is a different message (rewound or branched)", idx)}
			}
		} else if was.Digest != now.Digest {
			return &preparedMiss{"text_changed", fmt.Sprintf("message %d changed and has no durable id to vouch for it", idx)}
		}
	}
	return nil
}

// Compressor copy with no durable row, no host cancel consult, cleared fallback fields and private telemetry
func workerCopy(compressor PreparedCompactionCompressor) PreparedCompactionCompressor {
	worker := compressor.ShallowCopy()
	worker.SetSessionDB(nil)
	worker.SetCompressionCancelledCheck(nil)
	worker.SetSummaryCallAttr("_last_aux_model_failure_error", nil)
	worker.SetSummaryCallAttr("_last_aux_model_failure_model", nil)
	worker.BeginCompressionTelemetry(nil)
	return worker
}

// True for a turn end pass in a session that ends with its turn
func blockedForPlatform(a *AIAgent, origin string) bool {
	if origin != "turn-end" {
		return false
	}
	if a.platform == "cron" || a.platform == "subagent" || a.delegateDepth > 0 {
		return true
	}
	return a.agentIsOneshot(context.Background())
}

type busyWindow struct {
	sessionID  string
	start, end int
}

// PreparedCompaction is the prepared summary state one context compressor owns
type PreparedCompaction struct {
	mu                   sync.Mutex
	entry                *PreparedSummary
	pending              *pendingPass
	retryAfter           float64
	needsProviderContext bool
	busyWindow           *busyWindow
	taken                *PreparedSummary
	splicedAttemptID     any
	boundaries           int
}

// Fresh state with no candidate and no running pass
func NewPreparedCompaction() *PreparedCompaction { return &PreparedCompaction{} }

// Attempt id of the compaction that spliced the last summary, nil when none did
func (p *PreparedCompaction) SplicedAttemptID() any {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.splicedAttemptID
}

// Counts once a taken summary that no splice adopted
func (p *PreparedCompaction) countUnspliced(ctx context.Context) {
	p.mu.Lock()
	taken := p.taken
	p.taken = nil
	p.mu.Unlock()
	if taken != nil {
		recordPrepared(ctx, "discarded", "", "compaction_aborted", "", taken.Waited)
	}
}

// Drops the candidate and cancels a running pass, code empty meaning session_boundary
func (p *PreparedCompaction) Discard(ctx context.Context, reason, code string) {
	if code == "" {
		code = "session_boundary"
	}
	p.countUnspliced(ctx)
	p.mu.Lock()
	entry, pending := p.entry, p.pending
	p.entry, p.pending = nil, nil
	if pending != nil {
		pending.cancel(code)
	}
	if entry != nil || pending != nil {
		p.boundaries++
	}
	p.mu.Unlock()
	if entry != nil || pending != nil {
		recordPrepared(ctx, "discarded", "", code, "", nil)
	}
	var dropped *preparedWindow
	if entry != nil {
		dropped = &entry.preparedWindow
	} else if pending != nil {
		dropped = &pending.window
	}
	if dropped != nil {
		running := " (pass running)"
		if entry != nil {
			running = ""
		}
		slog.Info(fmt.Sprintf("prepared compaction: dropped session=%s window=%d-%d reason=%s%s", dropped.SessionID,
			dropped.CompressStart, dropped.CompressEnd, reason, running))
	}
}

// Stops preparing summaries once a memory provider supplies compaction context
func (p *PreparedCompaction) StopForProviderContext(ctx context.Context) {
	p.mu.Lock()
	first := !p.needsProviderContext
	p.needsProviderContext = true
	p.mu.Unlock()
	if first {
		slog.Info("prepared compaction: off for this agent (memory provider supplies compaction context)")
		recordPrepared(ctx, "auto_disabled", "", "provider_context", "", nil)
	}
}

func (p *PreparedCompaction) providerContextNeeded() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.needsProviderContext
}

// The candidate still covering the same messages after the head, else what a fitting running pass publishes
func (p *PreparedCompaction) Take(ctx context.Context, messages []map[string]any, sessionID string, start int, eligible bool) *PreparedSummary {
	p.countUnspliced(ctx)
	p.mu.Lock()
	entry, pending := p.entry, p.pending
	p.entry = nil
	boundaries := p.boundaries
	p.mu.Unlock()
	var reason *preparedMiss
	if !eligible {
		code := "fresh_required"
		if p.providerContextNeeded() {
			code = "provider_context"
		}
		reason = &preparedMiss{code, "fresh summary required"}
	}
	if reason == nil && entry != nil {
		reason = preparedMismatch(entry.preparedWindow, messages, sessionID, start)
	}
	var waited *float64
	func() {
		defer func() {
			p.mu.Lock()
			defer p.mu.Unlock()
			if p.pending == pending {
				p.pending = nil
			}
			p.entry = nil
			if pending != nil {
				code := "superseded"
				if reason != nil && waitCancelReasons[reason.Code] {
					code = reason.Code
				}
				pending.cancel(code)
			}
		}()
		if eligible && (entry == nil || reason != nil) {
			if pending != nil && preparedMismatch(pending.window, messages, sessionID, start) == nil {
				waitStarted := pythread.Monotonic()
				entry, reason = p.await(ctx, pending)
				w := pythread.Monotonic() - waitStarted
				waited = &w
				p.mu.Lock()
				crossed := p.boundaries != boundaries
				p.mu.Unlock()
				if entry == nil && crossed {
					reason = &preparedMiss{"discarded_while_waiting", "running pass discarded"}
				}
			} else if entry == nil {
				if pending != nil {
					reason = &preparedMiss{"running_not_fit", "running pass does not fit"}
				} else {
					reason = &preparedMiss{"none", "no candidate"}
				}
			}
		}
	}()
	if reason != nil || entry == nil {
		if entry != nil || pending != nil {
			slog.Info(fmt.Sprintf("prepared compaction: splice miss session=%s reason=%s", sessionID, reason.Text))
			if reason.Code != "discarded_while_waiting" {
				recordPrepared(ctx, "discarded", "", reason.Code, "", waited)
			}
		}
		return nil
	}
	slog.Info(fmt.Sprintf("prepared compaction: splice hit session=%s window=%d-%d", sessionID, entry.CompressStart, entry.CompressEnd))
	taken := *entry
	taken.Waited = waited
	p.mu.Lock()
	p.taken = &taken
	p.mu.Unlock()
	return &taken
}

// Waits for the pending pass while its progress and prompt reading feed this compaction's hooks
func (p *PreparedCompaction) await(ctx context.Context, pending *pendingPass) (*PreparedSummary, *preparedMiss) {
	pending.setHooks(provider.CurrentAuxProgressHook(ctx), provider.CurrentPromptReadingHook(ctx))
	cancelCheck := provider.CaptureAuxCancelCheck(ctx)
	fallback := time.Now().Add(time.Duration(provider.EffectiveAuxTimeout(ctx, "compression", nil) * float64(time.Second)))
	slog.Info(fmt.Sprintf("prepared compaction: waiting for the running pass session=%s window=%d-%d",
		pending.window.SessionID, pending.window.CompressStart, pending.window.CompressEnd))
	deadline := func() time.Time {
		if d, ok := provider.CurrentAuxStreamDeadline(ctx); ok {
			return d
		}
		return fallback
	}
	miss := func() *preparedMiss {
		defer pending.setHooks(nil, nil)
		for !pending.done.Wait(max(0.0, min(waitPollSeconds, time.Until(deadline()).Seconds()))) {
			if pending.cancelled.IsSet() {
				return &preparedMiss{"discarded_while_waiting", "running pass discarded"}
			}
			if cancelCheck != nil && provider.CapturedAuxCancelRequested(cancelCheck) {
				return &preparedMiss{"host_cancelled", "host cancelled the wait"}
			}
			if !time.Now().Before(deadline()) {
				return &preparedMiss{"wait_timeout", "running pass did not finish in time"}
			}
		}
		return nil
	}()
	if miss != nil {
		return nil, miss
	}
	p.mu.Lock()
	entry := p.entry
	p.entry = nil
	discarded := pending.cancelled.IsSet()
	p.mu.Unlock()
	if discarded {
		return nil, &preparedMiss{"discarded_while_waiting", "running pass discarded"}
	}
	if entry == nil || !slices.Equal(entry.Identities, pending.window.Identities) {
		return nil, &preparedMiss{"pass_failed_while_waiting", "running pass failed"}
	}
	return entry, nil
}

// True when no pass may start for this agent and compressor now
func (p *PreparedCompaction) blocked(a *AIAgent, compressor PreparedCompactionCompressor, origin string) bool {
	if !a.compressionEnabled || a.persistDisabled {
		return true
	}
	if a.apiMode == "codex_app_server" || a.codexResponsesNativeCompaction || compressor.MicroCompactEnabled() {
		return true
	}
	if p.providerContextNeeded() || blockedForPlatform(a, origin) {
		return true
	}
	if compressor.AwaitingRealUsageAfterCompression() || compressor.FallbackCompressionStreak() >= 2 {
		return true
	}
	base := compressor.Base()
	tokens := base.LastPromptTokens
	bandFloor := int(float64(base.ThresholdTokens) * (1 - PrepareBandRatio))
	if tokens <= 0 || base.ThresholdTokens <= 0 || tokens < bandFloor {
		return true
	}
	now := pythread.Monotonic()
	p.mu.Lock()
	retryAfter := p.retryAfter
	p.mu.Unlock()
	return now < compressor.SummaryFailureCooldownUntil() || now < retryAfter
}

// A Python KeyboardInterrupt keeps unwinding past the except Exception handlers
func isUserInterruptPanic(r any) bool {
	switch v := r.(type) {
	case plugins.UserInterrupt, *plugins.UserInterrupt:
		return true
	case error:
		var ui *plugins.UserInterrupt
		return errors.As(v, &ui)
	}
	return false
}

// Starts the background pass when the next compaction is near and none ran yet, never panics
func (p *PreparedCompaction) MaybePrepare(ctx context.Context, a *AIAgent, compressor PreparedCompactionCompressor, messages []map[string]any, origin string) (thread *memory.ContextThread) {
	var token *pendingPass
	defer func() {
		r := recover()
		if r == nil {
			return
		}
		if isUserInterruptPanic(r) {
			panic(r)
		}
		if token != nil {
			p.mu.Lock()
			if p.pending == token {
				p.pending = nil
			}
			p.mu.Unlock()
			token.done.Set()
			_ = preparedSlots.Release()
		}
		slog.Debug("prepared compaction: maybe_prepare failed", "error", r)
		recordPrepared(ctx, "internal_error", origin, "", "exception", nil)
		thread = nil
	}()
	p.countUnspliced(ctx)
	if p.blocked(a, compressor, origin) {
		return nil
	}
	p.mu.Lock()
	busy := p.pending != nil || p.entry != nil
	p.mu.Unlock()
	if busy {
		return nil
	}
	working, _, start, end := compressor.PlanCompactionWindow(messages)
	if start >= end {
		return nil
	}
	worker := workerCopy(compressor)
	scan := worker.ScanWindowHandoffs(working, start, end, slices.Clone(working[start:end]))
	if scan.TailStart != end {
		return nil
	}
	turns := scan.TurnsToSummarize
	if len(turns) == 0 {
		return nil
	}
	focus := worker.DeriveAutoFocusTopic(working)
	window := preparedWindow{compressor.CompressorSessionID(), start, end, PrefixIdentities(working, end)}
	if !preparedSlots.Acquire(false) {
		key := busyWindow{window.SessionID, start, end}
		p.mu.Lock()
		fresh := p.busyWindow == nil || *p.busyWindow != key
		if fresh {
			p.busyWindow = &key
		}
		p.mu.Unlock()
		if fresh {
			recordPrepared(ctx, "skipped_busy", origin, "", "", nil)
		}
		return nil
	}
	p.mu.Lock()
	if p.pending != nil || p.entry != nil {
		p.mu.Unlock()
		_ = preparedSlots.Release()
		return nil
	}
	token = newPendingPass(window)
	p.pending = token
	p.mu.Unlock()
	turnsCopy := pycopy.DeepCopy(turns).([]map[string]any)
	pass := token
	thread = memory.SpawnContextThread(ctx, "compaction-prepare", func(ctx context.Context) {
		p.runPass(ctx, worker, turnsCopy, focus, pass, origin)
	})
	thread.Start()
	recordPrepared(ctx, "pass_started", origin, "", "", nil)
	return thread
}

// Runs the summariser on the worker copy and publishes the summary when the pass still owns the slot
func (p *PreparedCompaction) runPass(ctx context.Context, worker PreparedCompactionCompressor, turns []map[string]any, focus *string, token *pendingPass, origin string) {
	window := token.window
	var entry *PreparedSummary
	event, failureClass := "pass_cancelled", ""
	started := pythread.Monotonic()
	logCancelled := func() {
		slog.Info(fmt.Sprintf("prepared compaction: pass cancelled session=%s after %.1fs (%s)", window.SessionID,
			pythread.Monotonic()-started, token.reason()))
	}
	defer func() {
		if r := recover(); r != nil && !isUserInterruptPanic(r) {
			slog.Warn(fmt.Sprintf("prepared compaction: pass crashed session=%s", window.SessionID), "error", r)
			event, failureClass = "internal_error", "exception"
		}
		p.mu.Lock()
		if entry != nil && p.pending == token {
			event = "pass_completed"
		}
		if p.pending == token {
			p.pending = nil
			if entry != nil {
				p.entry = entry
			} else {
				p.retryAfter = max(worker.SummaryFailureCooldownUntil(), pythread.Monotonic()+failedPassBackoffSeconds)
			}
		}
		p.mu.Unlock()
		token.done.Set()
		_ = preparedSlots.Release()
		reason := ""
		if event == "pass_cancelled" {
			reason = token.reason()
		}
		recordPrepared(ctx, event, origin, reason, failureClass, nil)
	}()
	slog.Info(fmt.Sprintf("prepared compaction: pass started session=%s origin=%s window=%d-%d turns=%d",
		window.SessionID, origin, window.CompressStart, window.CompressEnd, len(turns)))
	passCtx := tools.SilenceOutput(ctx)
	passCtx = provider.AuxInterruptProtection(passCtx, true, nil, &token.cancelled)
	passCtx = provider.AuxProgressHook(passCtx, token.touch)
	passCtx = provider.PromptReadingHook(passCtx, token.read)
	summary, err := worker.GenerateSummary(passCtx, turns, focus)
	if err != nil {
		var explicit *provider.AuxiliaryExplicitCancellation
		if errors.As(err, &explicit) {
			logCancelled()
			return
		}
		slog.Warn(fmt.Sprintf("prepared compaction: pass crashed session=%s", window.SessionID), "error", err)
		event, failureClass = "internal_error", "exception"
		return
	}
	switch {
	case token.cancelled.IsSet():
		logCancelled()
	case summary != nil && *summary != "":
		values := map[string]any{}
		if telemetry := worker.ActiveCompressionTelemetry(); telemetry != nil {
			for _, key := range summaryCallTelemetry {
				values[key], _ = telemetry.Get(key)
			}
		} else {
			for _, key := range summaryCallTelemetry {
				values[key] = nil
			}
		}
		callState := map[string]any{}
		for _, name := range summaryCallState {
			callState[name] = worker.SummaryCallAttr(name)
		}
		entry = &PreparedSummary{preparedWindow: window, Summary: *summary, Body: worker.PreviousSummary(),
			CallState: callState, Telemetry: values}
		slog.Info(fmt.Sprintf("prepared compaction: pass completed session=%s window=%d-%d in %.1fs",
			window.SessionID, window.CompressStart, window.CompressEnd, pythread.Monotonic()-started))
	default:
		why := worker.LastSummaryError()
		if why == "" {
			why = "summariser returned nothing"
		}
		slog.Warn(fmt.Sprintf("prepared compaction: pass failed session=%s after %.1fs: %s", window.SessionID,
			pythread.Monotonic()-started, why))
		event, failureClass = "pass_failed", passFailureClass(worker)
	}
}

// Failure class an inline attempt logs for this failed summary call, a terminal failure first
func passFailureClass(worker PreparedCompactionCompressor) string {
	for _, f := range terminalSummaryFailures {
		if worker.SummaryFailureFlag(f.Flag) {
			return f.Class
		}
	}
	if telemetry := worker.ActiveCompressionTelemetry(); telemetry != nil {
		if v, _ := telemetry.Get("failure_class"); pyrepr.Truthy(v) {
			return pyrepr.Str(v)
		}
	}
	return "summary_generation_failed"
}

func preparedStateOf(engine ContextEngine) (PreparedCompactionCompressor, *PreparedCompaction) {
	compressor, ok := engine.(PreparedCompactionCompressor)
	if !ok {
		return nil, nil
	}
	return compressor, compressor.PreparedCompactionState()
}

// Cancels a running pass of an agent being released since no compaction of it can splice the summary
func ReleaseForAgent(ctx context.Context, a *AIAgent) {
	if _, st := preparedStateOf(a.contextCompressor); st != nil {
		st.Discard(ctx, "agent released", "agent_released")
	}
}

// Leaves the live compressor as the inline summary call would, the pass summary as the iterative base
func AdoptPreparedSummary(ctx context.Context, compressor PreparedCompactionCompressor, prepared *PreparedSummary) {
	st := compressor.PreparedCompactionState()
	telemetry := compressor.ActiveCompressionTelemetry()
	if st != nil {
		st.mu.Lock()
		if st.taken == prepared {
			st.taken = nil
		}
		st.splicedAttemptID = nil
		if telemetry != nil {
			st.splicedAttemptID, _ = telemetry.Get("attempt_id")
		}
		st.mu.Unlock()
	}
	compressor.RecordSummarySuccess(prepared.Body)
	for _, name := range summaryCallState {
		if value, ok := prepared.CallState[name]; ok {
			compressor.SetSummaryCallAttr(name, value)
		}
	}
	if telemetry != nil {
		for _, key := range summaryCallTelemetry {
			if value := prepared.Telemetry[key]; value != nil {
				telemetry.Set(key, value)
			}
		}
		telemetry.Set("prepared_summary", true)
	}
	reason := ""
	if pyrepr.Truthy(prepared.Telemetry["fallback_used"]) {
		reason = "aux_fallback_main"
	}
	recordPrepared(ctx, "used", "", reason, "", prepared.Waited)
}

// Turn loop hook starting a prepared pass, nothing unless prepare ahead gave the compressor state
func MaybePrepareCompaction(ctx context.Context, a *AIAgent, messages []map[string]any, origin string) *memory.ContextThread {
	compressor, st := preparedStateOf(a.contextCompressor)
	if st == nil {
		return nil
	}
	return st.MaybePrepare(ctx, a, compressor, messages, origin)
}
