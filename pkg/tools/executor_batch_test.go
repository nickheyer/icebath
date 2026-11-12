package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/state"
)

func timeoutEnv(t *testing.T, tools map[string]any, envValue string) *Env {
	env := newTestToolEnv(t)
	if tools != nil {
		env.Config = config.Tree{"timeouts": map[string]any{"tools": tools}}
	}
	t.Setenv("ICEBATH_CONCURRENT_TOOL_TIMEOUT_S", envValue)
	t.Setenv("HERMES_CONCURRENT_TOOL_TIMEOUT_S", "")
	return env
}

func TestConcurrentToolTimeoutMigration(t *testing.T) {
	if got := resolveConcurrentToolTimeout(timeoutEnv(t, nil, "60")); got != 60*time.Second {
		t.Fatalf("env var %v", got)
	}
	if got := resolveConcurrentToolTimeout(timeoutEnv(t, nil, "0")); got != 0 {
		t.Fatalf("env zero %v", got)
	}
	if got := resolveConcurrentToolTimeout(timeoutEnv(t, map[string]any{"concurrent_batch": 300}, "60")); got != 300*time.Second {
		t.Fatalf("config key %v", got)
	}
}

func TestSequentialToolTimeoutResolver(t *testing.T) {
	env := timeoutEnv(t, nil, "")
	if got := resolveSequentialToolTimeout(env); got != resolveConcurrentToolTimeout(env) || got != 420*time.Second {
		t.Fatalf("inherits default %v", got)
	}
	cases := []struct {
		tools map[string]any
		env   string
		want  time.Duration
	}{
		{nil, "60", 60 * time.Second},
		{map[string]any{"concurrent_batch": 300, "sequential_call": 90}, "", 90 * time.Second},
		{map[string]any{"concurrent_batch": 300, "sequential_call": 0}, "", 0},
		{map[string]any{"concurrent_batch": 0}, "", 0},
	}
	for _, c := range cases {
		if got := resolveSequentialToolTimeout(timeoutEnv(t, c.tools, c.env)); got != c.want {
			t.Fatalf("%v env %q got %v want %v", c.tools, c.env, got, c.want)
		}
	}
}

func TestFlushAfterToolProgressPersistsTheInjectionMadeBeforeTheFlush(t *testing.T) {
	notice := "[iteration budget checkpoint: 3 of 4]"
	for _, content := range []any{"result", []any{map[string]any{"type": "text", "text": "result"}}} {
		host := &fakeExecutorHost{env: newTestToolEnv(t)}
		host.beforeFlush = func(m *[]*state.Message) {
			last := (*m)[len(*m)-1]
			if s, ok := last.Content.(string); ok {
				last.Content = s + "\n\n" + notice
			} else {
				last.Content = append(last.Content.([]any), map[string]any{"type": "text", "text": notice})
			}
		}
		var flushed string
		host.flush = func(msgs []*state.Message) (bool, error) {
			flushed = fmt.Sprint(msgs[len(msgs)-1].Content)
			return true, nil
		}
		e := NewExecutor(host, NewDispatcher(NewRegistry()))
		messages := []*state.Message{{Role: "user", Content: "work"}, {Role: "tool", ToolCallID: "t0", Content: content}}
		if !e.flushAfterToolProgress(&messages, "checkpoint") || !strings.Contains(flushed, notice) {
			t.Fatalf("flushed %q", flushed)
		}
	}
	host := &fakeExecutorHost{env: newTestToolEnv(t)}
	e := NewExecutor(host, NewDispatcher(NewRegistry()))
	messages := []*state.Message{{Role: "tool", Content: "r"}}
	host.flush = func([]*state.Message) (bool, error) { return false, nil }
	if e.flushAfterToolProgress(&messages, "checkpoint") || !host.persistFail.Load() || fmt.Sprint(host.causes) != "[]" {
		t.Fatalf("declined flush causes %q", host.causes)
	}
	full := errors.New("database or disk is full")
	host.flush = func([]*state.Message) (bool, error) { return false, full }
	host.causes = nil
	if e.flushAfterToolProgress(&messages, "checkpoint") || len(host.causes) != 1 || host.causes[0] != ClassifyPersistenceError(full) || host.causes[0] == "" {
		t.Fatalf("raised flush causes %q", host.causes)
	}
}

// Process wide provider stand in that files each write under the caller's active home
type scopeRecordingMemory struct {
	mu     sync.Mutex
	stores map[string][]string
	gsks   map[string]bool
}

func (m *scopeRecordingMemory) HasTool(name string) bool { return name == "pw_remember" }

func (m *scopeRecordingMemory) HandleToolCall(ctx context.Context, _ string, args map[string]any, callerGSK string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	dir := home.Dir(ctx)
	m.stores[dir] = append(m.stores[dir], args["content"].(string))
	m.gsks[callerGSK] = true
	return `{"ok": true}`
}

func TestMemoryToolCallsCarryTheCallerScopeAndGatewayKeyOnBothPaths(t *testing.T) {
	root := t.TempDir()
	mem := &scopeRecordingMemory{stores: map[string][]string{}, gsks: map[string]bool{}}
	type profile struct{ home, gsk, label string }
	profiles := []profile{{root + "/profiles/alpha", "agent:alpha:api_server:dm:1", "alpha"}, {root + "/profiles/beta", "agent:beta:api_server:dm:2", "beta"}}
	var wg sync.WaitGroup
	errs := make(chan string, 40)
	start := make(chan struct{})
	for _, p := range profiles {
		host := &fakeExecutorHost{env: newTestToolEnv(t), memory: mem, gsk: p.gsk}
		e := NewExecutor(host, NewDispatcher(NewRegistry()))
		wg.Add(1)
		go func(p profile) {
			defer wg.Done()
			ctx := home.WithOverride(context.Background(), p.home)
			<-start
			for i := 0; i < 20; i++ {
				args := map[string]any{"content": fmt.Sprintf("%s-%d", p.label, i)}
				var msgs []*state.Message
				var result string
				var err error
				if i%2 == 1 {
					ref := &ToolCallRef{Name: "pw_remember", Args: args, TaskID: "t", CallID: "c", Trace: []map[string]any{}}
					result, err = e.resolveSequentialDispatch(ctx, ref, &msgs).execute(ctx, args)
				} else {
					result, err = e.invokeTool(ctx, "pw_remember", args, "", "t", "c", &msgs, nil)
				}
				if err != nil || result != `{"ok": true}` {
					errs <- fmt.Sprintf("%s %v", result, err)
				}
			}
		}(p)
	}
	close(start)
	wg.Wait()
	close(errs)
	for msg := range errs {
		t.Fatal(msg)
	}
	for _, p := range profiles {
		if got := mem.stores[home.Dir(home.WithOverride(context.Background(), p.home))]; len(got) != 20 {
			t.Fatalf("%s store %v", p.label, got)
		}
	}
	if len(mem.gsks) != 2 || !mem.gsks[profiles[0].gsk] || !mem.gsks[profiles[1].gsk] {
		t.Fatalf("gateway keys %v", mem.gsks)
	}
}

func TestToolExecutorUsesCanonicalResponsesPairingID(t *testing.T) {
	e, _, _ := registryExecutor(t, handlerDef("web_search", func(context.Context, *Call) string { return "ok" }))
	ctx := context.Background()
	responses := state.ToolCall{ID: "fc_123", Type: "function", Extra: map[string]any{"call_id": "call_ABC"}, Function: state.ToolCallFunction{Name: "web_search", Arguments: "{}"}}
	bridged := state.ToolCall{ID: "call_ABC|fc_123", Type: "function", Function: state.ToolCallFunction{Name: "web_search", Arguments: "{}"}}
	for _, tc := range []state.ToolCall{responses, bridged} {
		if id := e.ParseToolCall(ctx, tc, true).Ref("t").CallID; id != "call_ABC" {
			t.Fatalf("pairing id %q", id)
		}
		var messages []*state.Message
		if err := e.ExecuteToolCallsSequential(ctx, []state.ToolCall{tc}, &messages, "t", true); err != nil || messages[0].ToolCallID != "call_ABC" {
			t.Fatalf("result row id %q err %v", messages[0].ToolCallID, err)
		}
	}
}

func TestFileCheckpointRefusesNTNamespacePathWithoutResolving(t *testing.T) {
	f := newCPFixture(t)
	m := f.enabled()
	e := NewExecutor(&fakeExecutorHost{env: f.env, ckptMgr: m}, NewDispatcher(NewRegistry()))
	e.beginToolExecution(&ToolCallRef{Name: "write_file", Args: map[string]any{"path": `\??\UNC\attacker.example\share\x`}, TaskID: "default", CallID: "c"}, 0)
	if f.exists(f.base) {
		t.Fatal("checkpoint store touched for an NT namespace path")
	}
}

type batchEvent struct{ kind, name, id string }

// Executor over web_search and terminal handlers recording start and end of every dispatch
func segmentExecutor(t *testing.T, during func(name, id string) string) (*Executor, *fakeExecutorHost, func() []batchEvent) {
	var mu sync.Mutex
	var events []batchEvent
	handler := func(_ context.Context, call *Call) string {
		mu.Lock()
		events = append(events, batchEvent{"start", call.Name, call.ToolCallID})
		mu.Unlock()
		result := during(call.Name, call.ToolCallID)
		mu.Lock()
		events = append(events, batchEvent{"end", call.Name, call.ToolCallID})
		mu.Unlock()
		return result
	}
	e, host, _ := registryExecutor(t, handlerDef("web_search", handler), handlerDef("terminal", handler))
	return e, host, func() []batchEvent {
		mu.Lock()
		defer mu.Unlock()
		return append([]batchEvent(nil), events...)
	}
}

func segCall(name, args, id string) state.ToolCall {
	return state.ToolCall{ID: id, Type: "function", Function: state.ToolCallFunction{Name: name, Arguments: args}}
}

func rowIDs(messages []*state.Message) string {
	ids := make([]string, len(messages))
	for i, m := range messages {
		ids[i] = m.ToolCallID
	}
	return strings.Join(ids, ",")
}

func TestMixedBatchRunsSafePrefixConcurrentlyAndBarrierAfter(t *testing.T) {
	var arrived sync.WaitGroup
	arrived.Add(2)
	both := make(chan struct{})
	go func() { arrived.Wait(); close(both) }()
	e, _, events := segmentExecutor(t, func(name, _ string) string {
		if name == "web_search" {
			arrived.Done()
			select {
			case <-both:
			case <-time.After(10 * time.Second):
				return "searches never overlapped"
			}
		}
		return fmt.Sprintf(`{"ok": "%s"}`, name)
	})
	calls := []state.ToolCall{segCall("web_search", `{"query":"a"}`, "s1"), segCall("web_search", `{"query":"b"}`, "s2"), segCall("terminal", `{"command":"echo done"}`, "t1")}
	var messages []*state.Message
	if err := e.ExecuteToolCallsSegmented(context.Background(), calls, &messages, "task-1", nil); err != nil {
		t.Fatal(err)
	}
	if rowIDs(messages) != "s1,s2,t1" || strings.Contains(messageText(messages[0])+messageText(messages[1]), "never overlapped") {
		t.Fatalf("rows %s %q", rowIDs(messages), messageText(messages[0]))
	}
	ev := events()
	terminalStart, searchEnds := -1, []int{}
	for i, x := range ev {
		if x == (batchEvent{"start", "terminal", "t1"}) {
			terminalStart = i
		}
		if x.kind == "end" && x.name == "web_search" {
			searchEnds = append(searchEnds, i)
		}
	}
	if len(searchEnds) != 2 || searchEnds[0] > terminalStart || searchEnds[1] > terminalStart {
		t.Fatalf("barrier started before the searches ended %v", ev)
	}
}

func TestMixedBatchPreservesOrderWithBarrierInMiddle(t *testing.T) {
	e, _, events := segmentExecutor(t, func(string, string) string { return `{"ok": true}` })
	calls := []state.ToolCall{segCall("web_search", `{"query":"a"}`, "s1"), segCall("web_search", `{"query":"b"}`, "s2"),
		segCall("terminal", `{"command":"touch x"}`, "t1"), segCall("web_search", `{"query":"c"}`, "s3"), segCall("web_search", `{"query":"d"}`, "s4")}
	var messages []*state.Message
	if err := e.ExecuteToolCallsSegmented(context.Background(), calls, &messages, "task-1", nil); err != nil {
		t.Fatal(err)
	}
	if rowIDs(messages) != "s1,s2,t1,s3,s4" {
		t.Fatalf("rows %s", rowIDs(messages))
	}
	var executed []string
	for _, x := range events() {
		if x.kind == "start" {
			executed = append(executed, x.id)
		}
	}
	pos := strings.Index(strings.Join(executed, ","), "t1")
	before, after := strings.Join(executed, ",")[:pos], strings.Join(executed, ",")[pos+2:]
	if !strings.Contains(before, "s1") || !strings.Contains(before, "s2") || !strings.Contains(after, "s3") || !strings.Contains(after, "s4") {
		t.Fatalf("executed %v", executed)
	}
}

func TestInterruptDuringBarrierDrainsLaterSegments(t *testing.T) {
	var host *fakeExecutorHost
	e, h, events := segmentExecutor(t, func(_, id string) string {
		if id == "t1" {
			host.Interrupt("", ReasonUserInterrupt)
		}
		return `{"ok": true}`
	})
	host = h
	calls := []state.ToolCall{segCall("web_search", `{"query":"a"}`, "s1"), segCall("web_search", `{"query":"b"}`, "s2"),
		segCall("terminal", `{"command":"long"}`, "t1"), segCall("web_search", `{"query":"c"}`, "s3"), segCall("web_search", `{"query":"d"}`, "s4")}
	var messages []*state.Message
	if err := e.ExecuteToolCallsSegmented(context.Background(), calls, &messages, "task-1", nil); err != nil {
		t.Fatal(err)
	}
	if rowIDs(messages) != "s1,s2,t1,s3,s4" {
		t.Fatalf("rows %s", rowIDs(messages))
	}
	for _, x := range events() {
		if x.id == "s3" || x.id == "s4" {
			t.Fatalf("drained call executed %v", x)
		}
	}
	for _, m := range messages[3:] {
		if c := messageText(m); !strings.Contains(c, "cancelled") && !strings.Contains(c, "skipped") {
			t.Fatalf("drained row %q", c)
		}
	}
}

func TestSteerIsAppliedOnceAfterTurnBudgetInEveryDispatchPath(t *testing.T) {
	old := budgetForAgent
	budgetForAgent = func(ExecutorHost) BudgetConfig {
		return BudgetConfig{DefaultResultSize: 10_000, TurnBudget: 48, PreviewSize: 16}
	}
	t.Cleanup(func() { budgetForAgent = old })
	large := strings.Repeat("L", 1000)
	paths := []struct {
		name  string
		calls []state.ToolCall
		run   func(e *Executor, calls []state.ToolCall, messages *[]*state.Message) error
	}{
		{"parallel", []state.ToolCall{segCall("web_search", `{"query":"large"}`, "parallel-large"), segCall("web_search", `{"query":"small"}`, "parallel-small")},
			func(e *Executor, calls []state.ToolCall, m *[]*state.Message) error {
				return e.ExecuteToolCallsConcurrent(context.Background(), calls, m, "task-1", true)
			}},
		{"sequential", []state.ToolCall{segCall("terminal", `{"command":"large"}`, "sequential-large")},
			func(e *Executor, calls []state.ToolCall, m *[]*state.Message) error {
				return e.ExecuteToolCallsSequential(context.Background(), calls, m, "task-1", true)
			}},
		{"mixed-parallel-large", []state.ToolCall{segCall("web_search", `{"query":"large"}`, "mixed-large"), segCall("web_search", `{"query":"b"}`, "s2"), segCall("terminal", `{"command":"echo"}`, "t1")},
			func(e *Executor, calls []state.ToolCall, m *[]*state.Message) error {
				return e.ExecuteToolCallsSegmented(context.Background(), calls, m, "task-1", nil)
			}},
		{"mixed-sequential-large", []state.ToolCall{segCall("web_search", `{"query":"a"}`, "s1"), segCall("web_search", `{"query":"b"}`, "s2"), segCall("terminal", `{"command":"large"}`, "terminal-large")},
			func(e *Executor, calls []state.ToolCall, m *[]*state.Message) error {
				return e.ExecuteToolCallsSegmented(context.Background(), calls, m, "task-1", nil)
			}},
		{"malformed", []state.ToolCall{segCall("terminal", "{not json", "malformed")},
			func(e *Executor, calls []state.ToolCall, m *[]*state.Message) error {
				return e.ExecuteToolCallsSequential(context.Background(), calls, m, "task-1", true)
			}},
	}
	for _, p := range paths {
		e, host, _ := segmentExecutor(t, func(_, id string) string {
			if strings.HasSuffix(id, "large") {
				return large
			}
			return "small"
		})
		var atSteer []string
		host.steerHook = func(m *[]*state.Message, n int) {
			for _, msg := range (*m)[len(*m)-n:] {
				atSteer = append(atSteer, fmt.Sprint(msg.Content))
			}
		}
		var messages []*state.Message
		if err := p.run(e, p.calls, &messages); err != nil {
			t.Fatalf("%s %v", p.name, err)
		}
		if len(host.steers) != 1 || host.steers[0] != len(p.calls) || len(atSteer) != len(p.calls) {
			t.Fatalf("%s steer calls %v rows %d", p.name, host.steers, len(atSteer))
		}
		for i, c := range p.calls {
			if (strings.HasSuffix(c.ID, "large") || c.ID == "malformed") && (strings.Contains(atSteer[i], large) || len(atSteer[i]) >= 1000 && c.ID != "malformed") {
				t.Fatalf("%s steer ran before the turn budget replaced %s", p.name, c.ID)
			}
		}
	}
}

func TestSoftInterruptWithToolReasonIsAttributedToTheSystem(t *testing.T) {
	swapDuration(t, &sequentialInterruptPollInterval, 20*time.Millisecond)
	host := &fakeExecutorHost{env: newTestToolEnv(t)}
	e := NewExecutor(host, NewDispatcher(NewRegistry()))
	var msgs []*state.Message
	prepared := func(id string) *terminalSlot {
		pc := &ParsedCall{ToolCall: segCall("terminal", "{}", id), Name: "terminal", Args: map[string]any{}, Trace: []map[string]any{}}
		return e.newTerminalBatch(&msgs, "t", []*ParsedCall{pc}).slots[0]
	}
	in := func(id string) managedInput {
		return managedInput{name: "terminal", args: map[string]any{}, taskID: "t", toolCallID: id,
			execute: func(context.Context, map[string]any) (string, error) { return "", nil }}
	}
	t.Setenv("ICEBATH_CONCURRENT_TOOL_TIMEOUT_S", "0.05")
	slot := prepared("c1")
	managed, err := e.runSequentialToolExecutionMiddleware(context.Background(), in("c1"), slot)
	if err != nil || managed == nil || !managed.TimedOut || !slot.batch.isCancelled() {
		t.Fatalf("batch timeout guard %+v %v", managed, err)
	}
	if !host.Interrupted() || fmt.Sprint(host.interrupts) != "[|terminal batch timeout]" {
		t.Fatalf("system stop booked as %q", host.interrupts)
	}
	host.interrupted.Store(false)
	host.interrupts = nil
	host.Interrupt("fix the login bug", ReasonUserInterrupt)
	t.Setenv("ICEBATH_CONCURRENT_TOOL_TIMEOUT_S", "60")
	slot = prepared("c2")
	managed, err = e.runSequentialToolExecutionMiddleware(context.Background(), in("c2"), slot)
	if err != nil || managed == nil || !managed.Cancelled || !slot.batch.isCancelled() {
		t.Fatalf("user stop of a wedged batch %+v %v", managed, err)
	}
	if fmt.Sprint(host.interrupts) != "[fix the login bug|"+ReasonUserInterrupt+"]" || host.ToolInterruptReason() != ReasonUserInterrupt {
		t.Fatalf("user stop rebooked %q", host.interrupts)
	}
	if got := InterruptSkipWording("guard {x}"); got != "Turn aborted — guard {x}" {
		t.Fatalf("system wording %q", got)
	}
}

// Human stop reasons read capitalized, an empty reason reads interrupted, any other reason reads aborted
func TestInterruptSkipWordingReadsTheRecordedReason(t *testing.T) {
	for reason, want := range map[string]string{
		"": "Turn interrupted", "explicit stop requested": "Explicit stop requested", "user sent a new message": "User sent a new message",
		ReasonUserInterrupt: "User interrupt", "terminal_batch_timeout": "Turn aborted — terminal batch timeout",
	} {
		if got := InterruptSkipWording(reason); got != want {
			t.Errorf("%q gives %q, want %q", reason, got, want)
		}
	}
}
