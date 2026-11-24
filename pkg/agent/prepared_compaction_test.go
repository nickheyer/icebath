package agent

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	timeutil "github.com/nickheyer/icebath/pkg/utils/time"
)

type fakePreparedCompressor struct {
	stubEngine
	mu               sync.Mutex
	state            *PreparedCompaction
	sessionID        string
	awaiting         bool
	fallbackStreak   int
	micro            bool
	cooldownUntil    float64
	headEnd          int
	tailKeep         int
	attrs            map[string]any
	flags            map[string]bool
	telemetry        *jsonutil.Object
	previous         string
	lastError        string
	success          []string
	summary          func(ctx context.Context, f *fakePreparedCompressor, turns []map[string]any) (*string, error)
	sessionDBCleared bool
	isWorker         bool
	calls            int
}

func newFakePreparedCompressor(sessionID string) *fakePreparedCompressor {
	f := &fakePreparedCompressor{stubEngine: *newStubEngine(20000, 1.0), state: NewPreparedCompaction(), sessionID: sessionID,
		headEnd: 1, tailKeep: 2, attrs: map[string]any{}, flags: map[string]bool{}}
	f.LastPromptTokens = 19500
	return f
}

func (f *fakePreparedCompressor) PreparedCompactionState() *PreparedCompaction { return f.state }
func (f *fakePreparedCompressor) CompressorSessionID() string                  { return f.sessionID }
func (f *fakePreparedCompressor) AwaitingRealUsageAfterCompression() bool      { return f.awaiting }
func (f *fakePreparedCompressor) FallbackCompressionStreak() int               { return f.fallbackStreak }
func (f *fakePreparedCompressor) MicroCompactEnabled() bool                    { return f.micro }
func (f *fakePreparedCompressor) SummaryFailureCooldownUntil() float64         { return f.cooldownUntil }

func (f *fakePreparedCompressor) PlanCompactionWindow(messages []map[string]any) ([]map[string]any, int, int, int) {
	return slices.Clone(messages), 0, f.headEnd, max(f.headEnd, len(messages)-f.tailKeep)
}

func (f *fakePreparedCompressor) ScanWindowHandoffs(messages []map[string]any, start, end int, turns []map[string]any) HandoffScan {
	return HandoffScan{TurnsToSummarize: turns, TailStart: end}
}

func (f *fakePreparedCompressor) DeriveAutoFocusTopic(messages []map[string]any) *string { return nil }

func (f *fakePreparedCompressor) ShallowCopy() PreparedCompactionCompressor {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := &fakePreparedCompressor{stubEngine: f.stubEngine, state: f.state, sessionID: f.sessionID, headEnd: f.headEnd,
		tailKeep: f.tailKeep, attrs: maps.Clone(f.attrs), flags: f.flags, telemetry: f.telemetry, previous: f.previous,
		summary: f.summary, cooldownUntil: f.cooldownUntil, isWorker: true}
	return c
}

func (f *fakePreparedCompressor) SetSessionDB(db SessionStore)             { f.sessionDBCleared = db == nil }
func (f *fakePreparedCompressor) SetCompressionCancelledCheck(func() bool) {}

func (f *fakePreparedCompressor) BeginCompressionTelemetry(currentTokens *int) {
	f.telemetry = jsonutil.NewObject()
	f.telemetry.Set("attempt_id", "worker-attempt")
}

func (f *fakePreparedCompressor) GenerateSummary(ctx context.Context, turns []map[string]any, focusTopic *string) (*string, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	return f.summary(ctx, f, turns)
}

func (f *fakePreparedCompressor) ActiveCompressionTelemetry() *jsonutil.Object { return f.telemetry }
func (f *fakePreparedCompressor) PreviousSummary() string                      { return f.previous }
func (f *fakePreparedCompressor) LastSummaryError() string                     { return f.lastError }
func (f *fakePreparedCompressor) SummaryCallAttr(name string) any              { return f.attrs[name] }
func (f *fakePreparedCompressor) SetSummaryCallAttr(name string, value any)    { f.attrs[name] = value }
func (f *fakePreparedCompressor) SummaryFailureFlag(flag string) bool          { return f.flags[flag] }
func (f *fakePreparedCompressor) RecordSummarySuccess(summary string) {
	f.previous = summary
	f.success = append(f.success, summary)
}

// Alternating user and assistant messages each carrying a durable uid
func preparedMessages(n int, tag string) []map[string]any {
	out := make([]map[string]any, 0, n)
	for i := 0; i < n; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		out = append(out, map[string]any{"role": role, "content": fmt.Sprintf("%s %d %s", tag, i, strings.Repeat("x", 40)),
			state.MessageUID: fmt.Sprintf("%s-uid-%d", tag, i)})
	}
	return out
}

func preparedAgent(cc *fakePreparedCompressor) *AIAgent {
	return &AIAgent{compressionEnabled: true, contextCompressor: cc, platform: "cli"}
}

func TestPreparedGatesStartNoPass(t *testing.T) {
	cases := map[string]func(a *AIAgent, cc *fakePreparedCompressor){
		"compression_off":     func(a *AIAgent, cc *fakePreparedCompressor) { a.compressionEnabled = false },
		"background_fork":     func(a *AIAgent, cc *fakePreparedCompressor) { a.persistDisabled = true },
		"codex_app_server":    func(a *AIAgent, cc *fakePreparedCompressor) { a.apiMode = "codex_app_server" },
		"native_compaction":   func(a *AIAgent, cc *fakePreparedCompressor) { a.codexResponsesNativeCompaction = true },
		"micro_compact":       func(a *AIAgent, cc *fakePreparedCompressor) { cc.micro = true },
		"awaiting_usage":      func(a *AIAgent, cc *fakePreparedCompressor) { cc.awaiting = true },
		"benched_summariser":  func(a *AIAgent, cc *fakePreparedCompressor) { cc.fallbackStreak = 2 },
		"below_band":          func(a *AIAgent, cc *fakePreparedCompressor) { cc.LastPromptTokens = 17000 },
		"no_usage":            func(a *AIAgent, cc *fakePreparedCompressor) { cc.LastPromptTokens = 0 },
		"cooldown":            func(a *AIAgent, cc *fakePreparedCompressor) { cc.cooldownUntil = timeutil.Monotonic() + 60 },
		"provider_context":    func(a *AIAgent, cc *fakePreparedCompressor) { cc.state.StopForProviderContext(context.Background()) },
		"retry_after_failure": func(a *AIAgent, cc *fakePreparedCompressor) { cc.state.retryAfter = timeutil.Monotonic() + 60 },
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			cc := newFakePreparedCompressor("S1")
			a := preparedAgent(cc)
			setup(a, cc)
			if th := MaybePrepareCompaction(context.Background(), a, preparedMessages(10, "m"), "tool-batch"); th != nil {
				t.Fatalf("%s started a pass", name)
			}
			if cc.state.pending != nil {
				t.Fatalf("%s left a pending pass", name)
			}
		})
	}
	floor := newFakePreparedCompressor("S1")
	floor.LastPromptTokens = int(20000 * (1 - PrepareBandRatio))
	if floor.state.blocked(preparedAgent(floor), floor, "tool-batch") {
		t.Fatal("a prompt exactly at the band floor must start a pass")
	}
}

func TestPreparedTurnEndPassSkippedWhereTheSessionEndsWithTheTurn(t *testing.T) {
	for name, setup := range map[string]func(a *AIAgent){
		"cron":     func(a *AIAgent) { a.platform = "cron" },
		"subagent": func(a *AIAgent) { a.platform = "subagent" },
		"delegate": func(a *AIAgent) { a.delegateDepth = 1 },
		"oneshot": func(a *AIAgent) {
			prev := tools.ActivePolicySource
			tools.ActivePolicySource = func(context.Context) (string, bool) { return "oneshot", true }
			t.Cleanup(func() { tools.ActivePolicySource = prev })
		},
	} {
		cc := newFakePreparedCompressor("S1")
		a := preparedAgent(cc)
		setup(a)
		if !cc.state.blocked(a, cc, "turn-end") {
			t.Errorf("%s: turn-end pass must be blocked", name)
		}
		if name != "oneshot" && !blockedForPlatform(a, "turn-end") {
			t.Errorf("%s: platform gate", name)
		}
		if blockedForPlatform(a, "tool-batch") {
			t.Errorf("%s: a tool batch pass is never blocked by the platform", name)
		}
	}
}

// Window over messages as a finished pass would have published it
func publishPrepared(cc *fakePreparedCompressor, messages []map[string]any, start, end int) *PreparedSummary {
	entry := &PreparedSummary{preparedWindow: preparedWindow{cc.sessionID, start, end, PrefixIdentities(messages, end)},
		Summary: "[CONTEXT COMPACTION] body", Body: "body",
		CallState: map[string]any{"summary_model": "main-model", "_last_aux_model_failure_error": "boom",
			"_last_aux_model_failure_model": "aux", "_last_aux_resolved_model": "aux-resolved"},
		Telemetry: map[string]any{"aux_model": "aux", "fallback_used": true, "failure_class": nil}}
	cc.state.entry = entry
	return entry
}

func TestPreparedAppendOnlyGrowthSplicesAndAdoptCarriesFallbackAndTelemetry(t *testing.T) {
	cc := newFakePreparedCompressor("S1")
	live := preparedMessages(10, "m")
	publishPrepared(cc, live, 1, 8)
	grown := append(slices.Clone(live), preparedMessages(4, "new")...)
	taken := cc.state.Take(context.Background(), grown, "S1", 1, true)
	if taken == nil || taken.CompressEnd != 8 {
		t.Fatalf("append only growth must splice at the candidate boundary, got %+v", taken)
	}
	cc.telemetry = jsonutil.NewObject()
	cc.telemetry.Set("attempt_id", "live-attempt")
	AdoptPreparedSummary(cc, taken)
	if cc.previous != "body" || cc.attrs["summary_model"] != "main-model" || cc.attrs["_last_aux_resolved_model"] != "aux-resolved" {
		t.Fatalf("adopt must leave the inline call state, got %v %v", cc.previous, cc.attrs)
	}
	if v, _ := cc.telemetry.Get("prepared_summary"); v != true {
		t.Fatal("telemetry must flag the prepared summary")
	}
	if v, _ := cc.telemetry.Get("aux_model"); v != "aux" {
		t.Fatal("pass telemetry copied onto the attempt")
	}
	if _, ok := cc.telemetry.Get("failure_class"); ok {
		t.Fatal("None telemetry values are not copied")
	}
	if cc.state.SplicedAttemptID() != "live-attempt" {
		t.Fatal("adopt records the splicing attempt")
	}
}

func TestPreparedChangedPrefixNeverSplices(t *testing.T) {
	cases := map[string]struct {
		mutate  func(msgs []map[string]any) []map[string]any
		session string
		start   int
	}{
		"other_session":      {func(m []map[string]any) []map[string]any { return m }, "S2", 1},
		"head_moved":         {func(m []map[string]any) []map[string]any { return m }, "S1", 2},
		"transcript_shorter": {func(m []map[string]any) []map[string]any { return m[:6] }, "S1", 1},
		"shape_changed": {func(m []map[string]any) []map[string]any {
			m[3]["role"] = "user"
			return m
		}, "S1", 1},
		"rewound": {func(m []map[string]any) []map[string]any {
			m[4][state.MessageUID] = "branch-uid"
			return m
		}, "S1", 1},
		"text_changed_without_uid": {func(m []map[string]any) []map[string]any {
			delete(m[5], state.MessageUID)
			m[5]["content"] = "rewritten"
			return m
		}, "S1", 1},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			cc := newFakePreparedCompressor("S1")
			live := preparedMessages(10, "m")
			publishPrepared(cc, live, 1, 8)
			msgs := tc.mutate(pycopyMessages(live))
			if got := cc.state.Take(context.Background(), msgs, tc.session, tc.start, true); got != nil {
				t.Fatal("changed prefix spliced")
			}
			if cc.state.entry != nil {
				t.Fatal("a miss clears the candidate")
			}
		})
	}
}

func pycopyMessages(msgs []map[string]any) []map[string]any {
	out := make([]map[string]any, len(msgs))
	for i, m := range msgs {
		out[i] = map[string]any{}
		for k, v := range m {
			out[i][k] = v
		}
	}
	return out
}

func TestPreparedInPlaceTextRewriteOfSummarisedMessagesKeepsTheCandidate(t *testing.T) {
	cc := newFakePreparedCompressor("S1")
	live := preparedMessages(10, "m")
	publishPrepared(cc, live, 1, 8)
	msgs := pycopyMessages(live)
	msgs[2]["content"] = "a plugin rewrote this text in place"
	if cc.state.Take(context.Background(), msgs, "S1", 1, true) == nil {
		t.Fatal("a uid vouches for a rewritten message")
	}
}

func TestPreparedIdentityIgnoresToolBodiesAndKeepsCallIDs(t *testing.T) {
	base := map[string]any{"role": "assistant", "content": "  hi  ", "tool_calls": []any{
		map[string]any{"id": "c1", "function": map[string]any{"name": "read", "arguments": "{}"}},
		map[string]any{"id": nil, "function": nil},
	}}
	got := messageIdentity(base)
	if got.Structure != "assistant\x1fc1:read\x1fNone:" || got.UID != "" {
		t.Fatalf("structure = %q", got.Structure)
	}
	pruned := pycopyMessages([]map[string]any{base})[0]
	pruned["tool_calls"] = []any{
		map[string]any{"id": "c1", "function": map[string]any{"name": "read", "arguments": "[pruned]"}},
		map[string]any{"id": nil, "function": nil},
	}
	pruned["content"] = "hi"
	if messageIdentity(pruned) != got {
		t.Fatal("argument prune and surrounding whitespace keep the identity")
	}
	tool := messageIdentity(map[string]any{"role": "tool", "tool_call_id": "c1", "content": "body"})
	if tool != (PreparedIdentity{Structure: "tool\x1fc1"}) {
		t.Fatalf("tool identity = %+v", tool)
	}
	if messageIdentity("not a dict") != (PreparedIdentity{Structure: "?"}) {
		t.Fatal("a non dict message")
	}
	obj := messageIdentity(map[string]any{"role": "assistant", "tool_calls": []any{&state.ToolCall{ID: "c2", Function: state.ToolCallFunction{Name: "x"}}}})
	if !strings.HasSuffix(obj.Structure, "\x1fc2:x") || len(obj.Digest) != 16 {
		t.Fatalf("object tool call identity = %+v", obj)
	}
}

func TestPreparedFreshSummaryPathsIgnoreAndDropTheCandidate(t *testing.T) {
	cc := newFakePreparedCompressor("S1")
	live := preparedMessages(10, "m")
	publishPrepared(cc, live, 1, 8)
	if cc.state.Take(context.Background(), live, "S1", 1, false) != nil {
		t.Fatal("ineligible compaction spliced")
	}
	publishPrepared(cc, live, 1, 8)
	cc.state.StopForProviderContext(context.Background())
	cc.state.StopForProviderContext(context.Background())
	if cc.state.Take(context.Background(), live, "S1", 1, false) != nil {
		t.Fatal("provider context compaction spliced")
	}
	if cc.state.Take(context.Background(), live, "S1", 1, true) != nil {
		t.Fatal("no candidate and no pass is a silent miss")
	}
}

func TestPreparedSessionBoundaryDiscardsCandidateAndReleaseCancelsPass(t *testing.T) {
	cc := newFakePreparedCompressor("S1")
	live := preparedMessages(10, "m")
	publishPrepared(cc, live, 1, 8)
	cc.state.Discard(context.Background(), "session reset", "")
	if cc.state.entry != nil || cc.state.boundaries != 1 {
		t.Fatal("boundary drops the candidate")
	}
	pending := newPendingPass(preparedWindow{"S1", 1, 8, nil})
	cc.state.pending = pending
	ReleaseForAgent(context.Background(), preparedAgent(cc))
	if !pending.cancelled.IsSet() || pending.reason() != "agent_released" || cc.state.pending != nil {
		t.Fatal("release cancels the running pass with its reason")
	}
	pending.cancel("superseded")
	if pending.reason() != "agent_released" {
		t.Fatal("the first cancel reason wins")
	}
	cc.state.Discard(context.Background(), "nothing left", "")
	if cc.state.boundaries != 2 {
		t.Fatalf("boundaries %d", cc.state.boundaries)
	}
}

func TestPreparedHeldCandidateOrBusySlotsStartNoPass(t *testing.T) {
	cc := newFakePreparedCompressor("S1")
	a := preparedAgent(cc)
	live := preparedMessages(10, "m")
	publishPrepared(cc, live, 1, 8)
	cc.LastPromptTokens = 40000
	if MaybePrepareCompaction(context.Background(), a, preparedMessages(30, "m"), "tool-batch") != nil {
		t.Fatal("a held candidate starts no further pass")
	}
	cc.state.entry = nil
	for preparedSlots.TryAcquire() {
		t.Cleanup(func() { _ = preparedSlots.Release() })
	}
	for i := 0; i < 3; i++ {
		if MaybePrepareCompaction(context.Background(), a, live, "tool-batch") != nil {
			t.Fatal("no slot, no pass")
		}
	}
}

func TestPreparedWorkerCopyKeepsWritesOffTheLiveCompressor(t *testing.T) {
	cc := newFakePreparedCompressor("S1")
	cc.attrs["_last_aux_model_failure_error"] = "old"
	worker := workerCopy(cc).(*fakePreparedCompressor)
	if !worker.isWorker || !worker.sessionDBCleared || worker.telemetry == nil {
		t.Fatal("worker copy detaches the durable row and starts private telemetry")
	}
	if worker.attrs["_last_aux_model_failure_error"] != nil || cc.attrs["_last_aux_model_failure_error"] != "old" {
		t.Fatal("the worker clears its own fallback fields only")
	}
}
