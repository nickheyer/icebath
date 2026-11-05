package plugins

import (
	"context"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Manager with discovery marked done and contexts for the named plugins
func busManager(t *testing.T, names ...string) (*Manager, map[string]*Context) {
	t.Helper()
	m := NewManager(t.TempDir())
	m.discovered = true
	ctxs := map[string]*Context{}
	for _, n := range names {
		ctxs[n] = newContext(&Manifest{Name: n, Key: n}, m)
	}
	return m, ctxs
}

// Fails the test when queued deliveries do not drain
func drain(t *testing.T, m *Manager) {
	t.Helper()
	if !m.waitForEventDispatch(eventDispatchWaitDefault) {
		t.Fatal("event dispatch did not go idle")
	}
}

func TestTwoPluginsCommunicate(t *testing.T) {
	m, c := busManager(t, "a", "b")
	var mu sync.Mutex
	var got []map[string]any
	if err := c["b"].Subscribe("a:ping", func(_ context.Context, p map[string]any) (any, error) {
		mu.Lock()
		got = append(got, p)
		mu.Unlock()
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	n, err := c["a"].Emit(context.Background(), "ping", map[string]any{"x": 1})
	if err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	drain(t, m)
	if !reflect.DeepEqual(got, []map[string]any{{"x": 1}}) {
		t.Fatalf("got %v", got)
	}
}

func TestEmitNilPayloadDeliversEmptyFields(t *testing.T) {
	m, c := busManager(t, "a")
	var got map[string]any
	c["a"].Subscribe("a:ping", func(_ context.Context, p map[string]any) (any, error) { got = p; return nil, nil })
	c["a"].Emit(context.Background(), "ping", nil)
	drain(t, m)
	if got == nil || len(got) != 0 {
		t.Fatalf("got %v", got)
	}
}

func TestNamespaceForcedToEmitterKeyAndFallsBackToName(t *testing.T) {
	m := NewManager(t.TempDir())
	keyed := newContext(&Manifest{Name: "shown", Key: "cat/real"}, m)
	unkeyed := newContext(&Manifest{Name: "plain"}, m)
	var mu sync.Mutex
	seen := map[string]bool{}
	record := func(name string) EventFunc {
		return func(context.Context, map[string]any) (any, error) {
			mu.Lock()
			seen[name] = true
			mu.Unlock()
			return nil, nil
		}
	}
	keyed.Subscribe("cat/real:x", record("keyed"))
	keyed.Subscribe("shown:x", record("wrong"))
	keyed.Subscribe("plain:x", record("unkeyed"))
	keyed.Emit(context.Background(), "x", nil)
	unkeyed.Emit(context.Background(), "x", nil)
	drain(t, m)
	if !seen["keyed"] || !seen["unkeyed"] || seen["wrong"] {
		t.Fatalf("seen %v", seen)
	}
}

func TestEmitRejectsNamespacedAndEmptyNames(t *testing.T) {
	_, c := busManager(t, "a")
	for _, bad := range []string{"hermes:x", "b:x", "a:x", ":x", ""} {
		if _, err := c["a"].Emit(context.Background(), bad, nil); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
	if err := c["a"].Subscribe("", func(context.Context, map[string]any) (any, error) { return nil, nil }); err == nil {
		t.Fatal("empty subscribe accepted")
	}
	if err := c["a"].Subscribe("a:x", nil); err == nil || !strings.Contains(err.Error(), "must be callable") {
		t.Fatalf("nil subscriber %v", err)
	}
}

func TestPerCallbackIsolationAndFailureWarnsOnce(t *testing.T) {
	m, c := busManager(t, "a")
	logs := captureLogs(t)
	var ok atomic.Int32
	c["a"].Subscribe("a:ping", func(context.Context, map[string]any) (any, error) { return nil, &runtimeError{"subscriber broke"} })
	c["a"].Subscribe("a:ping", func(context.Context, map[string]any) (any, error) { panic("subscriber panicked") })
	c["a"].Subscribe("a:ping", func(context.Context, map[string]any) (any, error) { ok.Add(1); return nil, nil })
	for range 3 {
		c["a"].Emit(context.Background(), "ping", nil)
		drain(t, m)
	}
	if ok.Load() != 3 || strings.Count(logs.String(), "subscriber broke") != 3 || strings.Count(logs.String(), "level=WARN msg=\"Event 'a:ping' callback") != 2 {
		t.Fatalf("ok %d logs %s", ok.Load(), logs.String())
	}
}

func TestEmitReturnsBeforeBlockingSubscriberFinishes(t *testing.T) {
	m, c := busManager(t, "a")
	release := make(chan struct{})
	c["a"].Subscribe("a:slow", func(context.Context, map[string]any) (any, error) { <-release; return nil, nil })
	t0 := time.Now()
	if n, _ := c["a"].Emit(context.Background(), "slow", nil); n != 1 || time.Since(t0) > time.Second {
		t.Fatalf("emit blocked")
	}
	close(release)
	drain(t, m)
}

func TestPendingBudgetDropsNewEventWithoutBlocking(t *testing.T) {
	m, c := busManager(t, "a")
	m.eventPendingCap = 1
	logs := captureLogs(t)
	release, started := make(chan struct{}), make(chan struct{}, 1)
	c["a"].Subscribe("a:slow", func(context.Context, map[string]any) (any, error) {
		started <- struct{}{}
		<-release
		return nil, nil
	})
	if n, _ := c["a"].Emit(context.Background(), "slow", nil); n != 1 {
		t.Fatal("first emit not scheduled")
	}
	<-started
	if n, _ := c["a"].Emit(context.Background(), "slow", nil); n != 0 || !strings.Contains(logs.String(), "pending budget (1) exhausted") {
		t.Fatalf("second emit %d", n)
	}
	close(release)
	drain(t, m)
}

func TestEachSubscriberReceivesDeepCopiedPayload(t *testing.T) {
	m, c := busManager(t, "a")
	var second map[string]any
	c["a"].Subscribe("a:p", func(_ context.Context, p map[string]any) (any, error) {
		p["nested"].(map[string]any)["k"] = "mutated"
		p["list"].([]string)[0] = "mutated"
		return nil, nil
	})
	c["a"].Subscribe("a:p", func(_ context.Context, p map[string]any) (any, error) { second = p; return nil, nil })
	c["a"].Emit(context.Background(), "p", map[string]any{"nested": map[string]any{"k": "v"}, "list": []string{"orig"}})
	drain(t, m)
	if second["nested"].(map[string]any)["k"] != "v" || second["list"].([]string)[0] != "orig" {
		t.Fatalf("second saw %v", second)
	}
}

func TestAwaitableSubscriberIsAwaited(t *testing.T) {
	m, c := busManager(t, "a")
	var done atomic.Bool
	c["a"].Subscribe("a:p", func(context.Context, map[string]any) (any, error) {
		return AwaitFunc(func(context.Context) (any, error) { done.Store(true); return nil, nil }), nil
	})
	c["a"].Emit(context.Background(), "p", nil)
	drain(t, m)
	if !done.Load() {
		t.Fatal("awaitable not awaited")
	}
}

func TestRemovePluginSubscriptionsCancelsOwnerEntries(t *testing.T) {
	m, c := busManager(t, "a", "b")
	noop := func(context.Context, map[string]any) (any, error) { return nil, nil }
	c["a"].Subscribe("b:ping", noop)
	c["a"].Subscribe("x:y", noop)
	c["b"].Subscribe("x:y", noop)
	if n := m.removePluginSubscriptions("a"); n != 2 {
		t.Fatalf("removed %d", n)
	}
	if _, ok := m.subscriptions["b:ping"]; ok || len(m.subscriptions["x:y"]) != 1 {
		t.Fatalf("subs %v", m.subscriptions)
	}
}

func TestOwnerRemovalCancelsCallbackAlreadySnapshottedInQueue(t *testing.T) {
	m, c := busManager(t, "a", "b")
	release, started := make(chan struct{}), make(chan struct{}, 1)
	var late atomic.Int32
	c["b"].Subscribe("b:block", func(context.Context, map[string]any) (any, error) {
		started <- struct{}{}
		<-release
		return nil, nil
	})
	c["a"].Subscribe("b:ping", func(context.Context, map[string]any) (any, error) { late.Add(1); return nil, nil })
	c["b"].Emit(context.Background(), "block", nil)
	<-started
	c["b"].Emit(context.Background(), "ping", nil)
	m.removePluginSubscriptions("a")
	close(release)
	drain(t, m)
	if late.Load() != 0 {
		t.Fatal("removed subscriber still ran")
	}
}

func TestRecursionCapTerminates(t *testing.T) {
	m, c := busManager(t, "a", "b")
	logs := captureLogs(t)
	var calls atomic.Int32
	c["a"].Subscribe("b:pong", func(ctx context.Context, _ map[string]any) (any, error) {
		calls.Add(1)
		c["a"].Emit(ctx, "ping", nil)
		return nil, nil
	})
	c["b"].Subscribe("a:ping", func(ctx context.Context, _ map[string]any) (any, error) {
		calls.Add(1)
		c["b"].Emit(ctx, "pong", nil)
		return nil, nil
	})
	c["a"].Emit(context.Background(), "ping", nil)
	drain(t, m)
	if calls.Load() > eventEmitDepthCap+1 || !strings.Contains(logs.String(), "recursion cap (8) exceeded") {
		t.Fatalf("calls %d", calls.Load())
	}
}

func TestSystemPromptSectionRegistrationValidates(t *testing.T) {
	m := NewManager(t.TempDir())
	c := newContext(&Manifest{Name: "example-plugin"}, m)
	for _, bad := range []string{"", "UPPER.case", "has space", "line\nbreak", strings.Repeat("x", 129)} {
		if _, err := c.RegisterSystemPromptSection(bad, "content"); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
	if _, err := c.RegisterSystemPromptSection("example.rules", "content", WithSectionPosition("priority-17")); err == nil {
		t.Fatal("position accepted")
	}
	if _, err := c.RegisterSystemPromptSection("example.rules", "content", WithSectionMaxChars(0)); err == nil {
		t.Fatal("max_chars 0 accepted")
	}
	if _, err := c.RegisterSystemPromptSection("example.rules", 42); err == nil {
		t.Fatal("non text content accepted")
	}
	if _, err := c.RegisterSystemPromptSection("example.rules", "content"); err != nil {
		t.Fatal(err)
	}
	other := newContext(&Manifest{Name: "other-plugin"}, m)
	if _, err := other.RegisterSystemPromptSection("example.rules", "other content"); err == nil || !strings.Contains(err.Error(), "already registered") {
		t.Fatalf("duplicate %v", err)
	}
}

func TestRenderIsDeterministicBoundedAndSessionInfoIsPrivate(t *testing.T) {
	m := NewManager(t.TempDir())
	c := newContext(&Manifest{Name: "example-plugin"}, m)
	logs := captureLogs(t)
	info := map[string]any{"session_id": "session-1"}
	var observed map[string]any
	c.RegisterSystemPromptSection("example.z", func(i map[string]any) (any, error) {
		observed = i
		i["session_id"] = "changed"
		return "B", nil
	}, WithSectionMaxChars(4))
	c.RegisterSystemPromptSection("example.a", "A", WithSectionMaxChars(4))
	c.RegisterSystemPromptSection("example.too-large", "12345", WithSectionMaxChars(4))
	rendered := m.RenderSystemPromptSections(info)
	var got [][2]string
	for _, r := range rendered {
		got = append(got, [2]string{r.ID, r.Content})
	}
	if !reflect.DeepEqual(got, [][2]string{{"example.a", "A"}, {"example.z", "B"}}) || info["session_id"] != "session-1" || observed == nil {
		t.Fatalf("got %v info %v", got, info)
	}
	if !strings.Contains(logs.String(), "exceeded max_chars") {
		t.Fatal("no max_chars warning")
	}
}

func TestRenderFailsOpenForCallbackFailureWrongTypeAndAggregateBudget(t *testing.T) {
	m := NewManager(t.TempDir())
	c := newContext(&Manifest{Name: "example-plugin"}, m)
	logs := captureLogs(t)
	c.RegisterSystemPromptSection("example.boom", func(map[string]any) (any, error) { return nil, &runtimeError{"plugin exploded"} })
	c.RegisterSystemPromptSection("example.wrong", func(map[string]any) (any, error) { return map[string]any{"not": "text"}, nil })
	chunk := MaxSystemPromptSectionsTotalChars/2 - 200
	c.RegisterSystemPromptSection("example.first", strings.Repeat("a", chunk), WithSectionMaxChars(chunk))
	c.RegisterSystemPromptSection("example.second", strings.Repeat("b", chunk), WithSectionMaxChars(chunk))
	c.RegisterSystemPromptSection("example.zzlast", strings.Repeat("c", 500), WithSectionMaxChars(500))
	var ids []string
	for _, r := range m.RenderSystemPromptSections(map[string]any{}) {
		ids = append(ids, r.ID)
	}
	out := logs.String()
	if !reflect.DeepEqual(ids, []string{"example.first", "example.second"}) || !strings.Contains(out, "plugin exploded") ||
		!strings.Contains(out, "returned dict, not str") || !strings.Contains(out, "aggregate") {
		t.Fatalf("ids %v logs %s", ids, out)
	}
}

func TestSystemPromptSectionUnloadRestoresSlot(t *testing.T) {
	m := NewManager(t.TempDir())
	c := newContext(&Manifest{Name: "example-plugin"}, m)
	h, err := c.RegisterSystemPromptSection("example.rules", "content")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Dispose(); err != nil {
		t.Fatal(err)
	}
	if len(m.systemPromptSections) != 0 {
		t.Fatalf("sections %v", m.systemPromptSections)
	}
}

func TestAbandonedLoadIgnoresLateRegistrations(t *testing.T) {
	m := NewManager(t.TempDir())
	c := newContext(&Manifest{Name: "late"}, m)
	logs := captureLogs(t)
	c.abandonLoad()
	if h, err := c.RegisterSystemPromptSection("late.x", "text"); h != nil || err != nil {
		t.Fatal("registration after abandon")
	}
	if err := c.Subscribe("a:b", func(context.Context, map[string]any) (any, error) { return nil, nil }); err != nil || len(m.subscriptions) != 0 {
		t.Fatal("subscribe after abandon")
	}
	if !strings.Contains(logs.String(), "after its load timed out; ignored") {
		t.Fatal("no warning")
	}
}
