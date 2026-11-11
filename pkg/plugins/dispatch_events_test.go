package plugins

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
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

func TestRegistrationValidatesStableIdPositionBudgetAndDuplicates(t *testing.T) {
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

func TestRenderIsDeterministicBoundedAndSessionInfoIsReadOnly(t *testing.T) {
	m := NewManager(t.TempDir())
	c := newContext(&Manifest{Name: "example-plugin"}, m)
	logs := captureLogs(t)
	info := map[string]any{"session_id": "session-1"}
	var observed map[string]any
	var observedSession any
	c.RegisterSystemPromptSection("example.z", func(i map[string]any) (any, error) {
		observed, observedSession = i, i["session_id"]
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
	if !reflect.DeepEqual(got, [][2]string{{"example.a", "A"}, {"example.z", "B"}}) || info["session_id"] != "session-1" || observed == nil || observedSession != "session-1" {
		t.Fatalf("got %v info %v observed session %v", got, info, observedSession)
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

// Manager with discovery marked done and contexts plugin_a keyed a and plugin_b keyed b
func busPair(t *testing.T) (*Manager, *Context, *Context) {
	t.Helper()
	m := NewManager(t.TempDir())
	m.discovered = true
	return m, newContext(&Manifest{Name: "plugin_a", Key: "a"}, m), newContext(&Manifest{Name: "plugin_b", Key: "b"}, m)
}

// Subscriber appending each payload it receives to a shared list
type busRecorder struct {
	mu       sync.Mutex
	payloads []map[string]any
	names    []string
}

func (r *busRecorder) payload() EventFunc {
	return func(_ context.Context, p map[string]any) (any, error) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.payloads = append(r.payloads, p)
		return nil, nil
	}
}

func (r *busRecorder) named(name string) EventFunc {
	return func(context.Context, map[string]any) (any, error) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.names = append(r.names, name)
		return nil, nil
	}
}

func TestEmitNonePayloadDeliversEmptyKwargs(t *testing.T) {
	m, a, b := busPair(t)
	seen := &busRecorder{}
	if err := a.Subscribe("b:ping", seen.payload()); err != nil {
		t.Fatal(err)
	}
	count, err := b.Emit(context.Background(), "ping", nil)
	drain(t, m)
	if err != nil || count != 1 || !reflect.DeepEqual(seen.payloads, []map[string]any{{}}) {
		t.Fatalf("count %d err %v seen %v", count, err, seen.payloads)
	}
}

func TestNamespaceForcedToEmitterKey(t *testing.T) {
	m, a, b := busPair(t)
	delivered := &busRecorder{}
	for _, name := range []string{"b:ping", "a:ping"} {
		if err := a.Subscribe(name, delivered.named(name)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := b.Emit(context.Background(), "ping", nil); err != nil {
		t.Fatal(err)
	}
	drain(t, m)
	if !reflect.DeepEqual(delivered.names, []string{"b:ping"}) {
		t.Fatalf("delivered %v", delivered.names)
	}
}

func TestNamespaceFallsBackToNameWhenKeyEmpty(t *testing.T) {
	m := NewManager(t.TempDir())
	m.discovered = true
	c := newContext(&Manifest{Name: "plugin_named", Key: ""}, m)
	got := &busRecorder{}
	if err := c.Subscribe("plugin_named:evt", got.payload()); err != nil {
		t.Fatal(err)
	}
	count, err := c.Emit(context.Background(), "evt", map[string]any{"v": 1})
	drain(t, m)
	if err != nil || count != 1 || !reflect.DeepEqual(got.payloads, []map[string]any{{"v": 1}}) {
		t.Fatalf("count %d err %v got %v", count, err, got.payloads)
	}
}

func TestEmitRejectsNamespacedNames(t *testing.T) {
	for _, bad := range []string{"hermes:x", "a:x", "b:x", "other:evt", ":x", "x:"} {
		t.Run(bad, func(t *testing.T) {
			m, _, b := busPair(t)
			fired := &busRecorder{}
			for _, name := range []string{bad, "b:" + bad, "hermes:x", "a:x", "b:x"} {
				if err := b.Subscribe(name, fired.named(name)); err != nil {
					t.Fatalf("subscribe %q: %v", name, err)
				}
			}
			if _, err := b.Emit(context.Background(), bad, nil); err == nil {
				t.Fatalf("emit accepted %q", bad)
			}
			drain(t, m)
			if len(fired.names) != 0 {
				t.Fatalf("rejected emit delivered to %v", fired.names)
			}
		})
	}
}

func TestEmitRejectsEmptyEvent(t *testing.T) {
	_, _, b := busPair(t)
	if _, err := b.Emit(context.Background(), "", nil); err == nil {
		t.Fatal("emit accepted an empty event name")
	}
}

func TestPerCallbackIsolation(t *testing.T) {
	m, a, b := busPair(t)
	received := &busRecorder{}
	logs := captureLogs(t)
	if err := a.Subscribe("b:ping", func(context.Context, map[string]any) (any, error) {
		return nil, &runtimeError{"subscriber exploded"}
	}); err != nil {
		t.Fatal(err)
	}
	if err := a.Subscribe("b:ping", received.payload()); err != nil {
		t.Fatal(err)
	}
	count, err := b.Emit(context.Background(), "ping", map[string]any{"ok": true})
	drain(t, m)
	if err != nil || count != 2 || !reflect.DeepEqual(received.payloads, []map[string]any{{"ok": true}}) {
		t.Fatalf("count %d err %v received %v", count, err, received.payloads)
	}
	if !warnedWith(logs.String(), "subscriber exploded") && !warnedWith(logs.String(), "raised") {
		t.Fatalf("no warning names the failing subscriber: %s", logs.String())
	}
}

func TestAsyncSubscriberIsAwaited(t *testing.T) {
	m, a, b := busPair(t)
	var mu sync.Mutex
	var observed []any
	if err := a.Subscribe("b:ping", func(_ context.Context, payload map[string]any) (any, error) {
		return AwaitFunc(func(context.Context) (any, error) {
			mu.Lock()
			defer mu.Unlock()
			observed = append(observed, payload["value"])
			return nil, nil
		}), nil
	}); err != nil {
		t.Fatal(err)
	}
	if count, err := b.Emit(context.Background(), "ping", map[string]any{"value": 7}); err != nil || count != 1 {
		t.Fatalf("count %d err %v", count, err)
	}
	drain(t, m)
	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(observed, []any{7}) {
		t.Fatalf("observed %v", observed)
	}
}

func TestManifestParseReadsEmitsListens(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "myplug")
	writePluginDir(t, dir, "emits:\n- ping\nlistens:\n- other:evt\nname: myplug\n")
	manifest := ParseManifestFile(filepath.Join(dir, "plugin.yaml"), dir, "user", "")
	if manifest == nil {
		t.Fatal("the manifest did not parse")
	}
	if !reflect.DeepEqual(manifest.Emits, []string{"ping"}) || !reflect.DeepEqual(manifest.Listens, []string{"other:evt"}) {
		t.Fatalf("emits %v listens %v", manifest.Emits, manifest.Listens)
	}
}

func TestPluginsShowIncludesEmitsListens(t *testing.T) {
	ctx, h := listWorld(t)
	writePluginDir(t, filepath.Join(h, "plugins", "showplug"),
		"description: a demo plugin\nemits:\n- ping\n- pong\nlistens:\n- other:ready\nname: showplug\nversion: 1.2.3\n")
	var out bytes.Buffer
	if err := CmdShow(ctx, "showplug", &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"showplug", "ping", "pong", "other:ready"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("show output lacks %q: %s", want, out.String())
		}
	}
}

func TestPluginsShowNotFoundExits(t *testing.T) {
	ctx, _ := listWorld(t)
	var out bytes.Buffer
	err := CmdShow(ctx, "nope", &out)
	var exit *CommandExit
	if !errors.As(err, &exit) || exit.Code == 0 {
		t.Fatalf("show of an unknown plugin returned %v", err)
	}
}
