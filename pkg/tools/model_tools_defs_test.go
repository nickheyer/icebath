package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
)

// Provider metadata doubles answering with the configured window and no runtime override
func installContextLengthDoubles() {
	RuntimeProviderCredentials = func(context.Context, *Env, string, string) (string, string, error) { return "", "", nil }
	CachedContextLength = func(context.Context, string, string) (int, bool) { return 0, false }
	ModelContextLength = func(_ context.Context, q ContextLengthQuery) int { return q.ConfigContextLength }
}

const (
	memoMail   = "mcp__probe_mail__list_messages"
	memoAnchor = "mcp__probe_anchor__ping"
)

var memoBoth = []string{"mcp-probe-mail", "mcp-probe-anchor"}

type memoClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *memoClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *memoClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func (c *memoClock) set(at time.Time) {
	c.mu.Lock()
	c.now = at
	c.mu.Unlock()
}

// One fake clock read by the verdict cache and the definitions memo
func installMemoClock(t *testing.T) *memoClock {
	c := &memoClock{now: time.Unix(1000, 0)}
	prev := checkNow
	checkNow = c.Now
	t.Cleanup(func() { checkNow = prev })
	return c
}

type memoMailState struct {
	mu          sync.Mutex
	up          bool
	probes      int
	duringProbe func()
	computes    int
}

func (s *memoMailState) setUp(up bool) {
	s.mu.Lock()
	s.up = up
	s.mu.Unlock()
}

func (s *memoMailState) probeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.probes
}

func memoSchema(name, description string) map[string]any {
	return map[string]any{"name": name, "description": description, "parameters": map[string]any{"type": "object", "properties": map[string]any{}}}
}

// A recovering mail tool next to an always healthy anchor whose dynamic schema counts definition builds
func memoRegistry(t *testing.T) (*Registry, *memoMailState, *Env) {
	t.Helper()
	InvalidateCheckCache()
	ClearToolDefinitionsCache()
	t.Cleanup(func() { InvalidateCheckCache(); ClearToolDefinitionsCache() })
	st := &memoMailState{}
	r := NewRegistry()
	r.Register(Def{Name: memoMail, Toolset: "mcp-probe-mail", Schema: memoSchema(memoMail, "List messages in the probe mailbox"), Handler: constHandler("MAIL_OK"),
		Check: func(*Env) bool {
			st.mu.Lock()
			st.probes++
			during := st.duringProbe
			st.mu.Unlock()
			if during != nil {
				during()
			}
			st.mu.Lock()
			defer st.mu.Unlock()
			return st.up
		}})
	r.Register(Def{Name: memoAnchor, Toolset: "mcp-probe-anchor", Schema: memoSchema(memoAnchor, "Ping the anchor server"), Handler: constHandler("ANCHOR_OK"),
		Check: func(*Env) bool { return true },
		DynamicSchema: func(*Env) map[string]any {
			st.mu.Lock()
			st.computes++
			st.mu.Unlock()
			return nil
		}})
	return r, st, &Env{Home: t.TempDir()}
}

func bridgeDefNames(r *Registry, env *Env, enabled []string) map[string]bool {
	out := map[string]bool{}
	for _, td := range r.ToolDefinitions(context.Background(), env, DefinitionOptions{Enabled: enabled, Quiet: true, SkipToolSearchAssembly: true}) {
		out[toolDefName(td)] = true
	}
	return out
}

func wantNames(t *testing.T, got map[string]bool, want ...string) {
	t.Helper()
	w := map[string]bool{}
	for _, n := range want {
		w[n] = true
	}
	if !reflect.DeepEqual(got, w) {
		t.Fatalf("names %v, want %v", got, w)
	}
}

// Memo entry the quiet definitions path cached, exactly one entry expected
func cachedQuietDefs(t *testing.T) []map[string]any {
	t.Helper()
	toolDefsCacheMu.Lock()
	defer toolDefsCacheMu.Unlock()
	if len(toolDefsCache) != 1 {
		t.Fatalf("cache entries %d", len(toolDefsCache))
	}
	for _, e := range toolDefsCache {
		return e.defs
	}
	return nil
}

func TestFirstUncachedCallReturnsFreshList(t *testing.T) {
	r, st, env := memoRegistry(t)
	st.setUp(true)
	first := r.ToolDefinitions(context.Background(), env, DefinitionOptions{Enabled: memoBoth, Quiet: true, SkipToolSearchAssembly: true})
	cached := cachedQuietDefs(t)
	if len(first) != 2 || &first[0] == &cached[0] {
		t.Fatalf("first uncached call aliases the memo: %d defs", len(first))
	}
}

func TestCacheHitReturnsFreshList(t *testing.T) {
	r, st, env := memoRegistry(t)
	st.setUp(true)
	first := r.ToolDefinitions(context.Background(), env, DefinitionOptions{Enabled: memoBoth, Quiet: true, SkipToolSearchAssembly: true})
	cached := cachedQuietDefs(t)
	first = append(first[:0], map[string]any{"type": "function", "function": map[string]any{"name": "memory"}})
	second := r.ToolDefinitions(context.Background(), env, DefinitionOptions{Enabled: memoBoth, Quiet: true, SkipToolSearchAssembly: true})
	if len(second) != 2 || &second[0] == &cached[0] || &second[0] == &first[0] {
		t.Fatalf("cache hit returned an aliased or mutated list %v", second)
	}
	wantNames(t, bridgeDefNames(r, env, memoBoth), memoMail, memoAnchor)
}

func TestCacheBoundedByEviction(t *testing.T) {
	r, _, env := memoRegistry(t)
	for i := range toolDefsCacheMax {
		r.ToolDefinitions(context.Background(), env, DefinitionOptions{Enabled: []string{fmt.Sprintf("fake_toolset_%d", i)}, Quiet: true})
	}
	toolDefsCacheMu.Lock()
	full, oldest := len(toolDefsCache), toolDefsCacheOrder[0]
	toolDefsCacheMu.Unlock()
	r.ToolDefinitions(context.Background(), env, DefinitionOptions{Enabled: []string{"fake_toolset_overflow"}, Quiet: true})
	toolDefsCacheMu.Lock()
	defer toolDefsCacheMu.Unlock()
	if _, kept := toolDefsCache[oldest]; full != toolDefsCacheMax || len(toolDefsCache) != toolDefsCacheMax || kept {
		t.Fatalf("entries %d then %d, oldest kept %v", full, len(toolDefsCache), kept)
	}
}

func TestConcurrentCapacityMissesEvictAtomically(t *testing.T) {
	_, _, env := memoRegistry(t)
	r := NewRegistry()
	var arrived sync.WaitGroup
	arrived.Add(2)
	barrier := func(*Env) bool {
		arrived.Done()
		done := make(chan struct{})
		go func() { arrived.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("barrier timed out")
		}
		return false
	}
	other := func(env *Env) bool { return barrier(env) }
	r.Register(Def{Name: "c0", Toolset: "concurrent_0", Schema: memoSchema("c0", "c0"), Handler: constHandler("ok"), Check: barrier})
	r.Register(Def{Name: "c1", Toolset: "concurrent_1", Schema: memoSchema("c1", "c1"), Handler: constHandler("ok"), Check: other})
	toolDefsCacheMu.Lock()
	for i := range toolDefsCacheMax {
		key := fmt.Sprintf("old\x1f%d", i)
		toolDefsCache[key] = toolDefsCacheEntry{defs: nil, expiry: farFuture}
		toolDefsCacheOrder = append(toolDefsCacheOrder, key)
	}
	toolDefsCacheMu.Unlock()
	results := make([][]map[string]any, 2)
	var wg sync.WaitGroup
	for i := range 2 {
		wg.Go(func() {
			results[i] = r.ToolDefinitions(context.Background(), env, DefinitionOptions{Enabled: []string{fmt.Sprintf("concurrent_%d", i)}, Quiet: true, SkipToolSearchAssembly: true})
		})
	}
	wg.Wait()
	toolDefsCacheMu.Lock()
	defer toolDefsCacheMu.Unlock()
	if len(results[0]) != 0 || len(results[1]) != 0 || len(toolDefsCache) != toolDefsCacheMax || len(toolDefsCacheOrder) != toolDefsCacheMax {
		t.Fatalf("results %v entries %d order %d", results, len(toolDefsCache), len(toolDefsCacheOrder))
	}
}

func TestReporterReproducerExplicitInvalidationReachesQuietMemo(t *testing.T) {
	r, st, env := memoRegistry(t)
	wantNames(t, bridgeDefNames(r, env, []string{"mcp-probe-mail"}))
	st.setUp(true)
	InvalidateCheckCache()
	wantNames(t, bridgeDefNames(r, env, []string{"mcp-probe-mail"}), memoMail)
}

func TestFalseToTrueRecoversOnNaturalTTLExpiry(t *testing.T) {
	r, st, env := memoRegistry(t)
	clock := installMemoClock(t)
	wantNames(t, bridgeDefNames(r, env, memoBoth), memoAnchor)
	st.setUp(true)
	clock.advance(checkTTL - time.Second)
	wantNames(t, bridgeDefNames(r, env, memoBoth), memoAnchor)
	clock.advance(2 * time.Second)
	wantNames(t, bridgeDefNames(r, env, memoBoth), memoAnchor, memoMail)
}

func TestMemoHitsWithinVerdictTTL(t *testing.T) {
	r, st, env := memoRegistry(t)
	clock := installMemoClock(t)
	for range 5 {
		bridgeDefNames(r, env, memoBoth)
		clock.advance(5 * time.Second)
	}
	if st.computes != 1 || st.probeCount() != 1 {
		t.Fatalf("computes %d probes %d", st.computes, st.probeCount())
	}
	clock.advance(checkTTL)
	bridgeDefNames(r, env, memoBoth)
	bridgeDefNames(r, env, memoBoth)
	if st.computes != 2 || st.probeCount() != 2 {
		t.Fatalf("computes %d probes %d after the TTL", st.computes, st.probeCount())
	}
}

func TestTrueToFalseRespectsTTLThenFailureGrace(t *testing.T) {
	r, st, env := memoRegistry(t)
	clock := installMemoClock(t)
	st.setUp(true)
	start := clock.Now()
	if !bridgeDefNames(r, env, memoBoth)[memoMail] {
		t.Fatal("mail missing while up")
	}
	st.setUp(false)
	clock.set(start.Add(checkTTL - time.Second))
	if !bridgeDefNames(r, env, memoBoth)[memoMail] || st.probeCount() != 1 {
		t.Fatalf("cached True verdict not served, probes %d", st.probeCount())
	}
	clock.set(start.Add(checkTTL + time.Second))
	if !bridgeDefNames(r, env, memoBoth)[memoMail] {
		t.Fatal("failure inside the grace window dropped the tool")
	}
	clock.advance(time.Second)
	if !bridgeDefNames(r, env, memoBoth)[memoMail] || st.probeCount() != 2 {
		t.Fatalf("grace list not reused, probes %d", st.probeCount())
	}
	clock.set(start.Add(checkGrace + time.Second))
	wantNames(t, bridgeDefNames(r, env, memoBoth), memoAnchor)
}

func TestRestrictedToolsetNeverGainsTheRecoveredTool(t *testing.T) {
	r, st, env := memoRegistry(t)
	clock := installMemoClock(t)
	restricted := []string{"mcp-probe-anchor"}
	wantNames(t, bridgeDefNames(r, env, restricted), memoAnchor)
	st.setUp(true)
	clock.advance(checkTTL + time.Second)
	InvalidateCheckCache()
	wantNames(t, bridgeDefNames(r, env, restricted), memoAnchor)
	call := &Call{Name: ToolCallName, Args: map[string]any{"calls": []any{map[string]any{"name": memoMail, "arguments": map[string]any{}}}}, Env: env}
	result, underlying, _, bridged := NewDispatcher(r).dispatchBridgeTool(context.Background(), call, restricted, nil)
	if !bridged || underlying != "" || decodeModelToolsResult(t, result)["error"] == nil {
		t.Fatalf("bridged %v underlying %q result %s", bridged, underlying, result)
	}
}

func TestProfileScopedVerdictsStaySeparate(t *testing.T) {
	r, st, envA := memoRegistry(t)
	envB := &Env{Home: t.TempDir()}
	st.setUp(true)
	r.Register(Def{Name: memoMail, Toolset: "mcp-probe-mail", Schema: memoSchema(memoMail, "List messages"), Handler: constHandler("MAIL_OK"),
		Check: func(env *Env) bool { return env.Home == envA.Home }})
	if !bridgeDefNames(r, envA, memoBoth)[memoMail] {
		t.Fatal("profile a lost its True verdict")
	}
	wantNames(t, bridgeDefNames(r, envB, memoBoth), memoAnchor)
	if !bridgeDefNames(r, envA, memoBoth)[memoMail] {
		t.Fatal("profile b's False verdict leaked into profile a")
	}
	unresolved := &Env{Values: &sync.Map{}}
	unresolved.Values.Store("session.multiplex", true)
	if _, bypass := CheckFnCacheScope(unresolved); !bypass {
		t.Fatal("a routed env without a home must bypass the check cache")
	}
	ClearToolDefinitionsCache()
	bridgeDefNames(r, unresolved, memoBoth)
	toolDefsCacheMu.Lock()
	defer toolDefsCacheMu.Unlock()
	if len(toolDefsCache) != 0 {
		t.Fatalf("unresolved profile identity memoized %d entries", len(toolDefsCache))
	}
}

func TestGraceVerdictReuseBoundsFailingProbesAndEndsWithTheGrace(t *testing.T) {
	r, st, env := memoRegistry(t)
	clock := installMemoClock(t)
	st.setUp(true)
	start := clock.Now()
	bridgeDefNames(r, env, memoBoth)
	st.setUp(false)
	clock.set(start.Add(checkTTL + time.Second))
	before := st.probeCount()
	for range 15 {
		if !bridgeDefNames(r, env, memoBoth)[memoMail] {
			t.Fatal("grace verdict dropped the tool")
		}
		clock.advance(500 * time.Millisecond)
	}
	if got := st.probeCount() - before; got != 2 {
		t.Fatalf("probes inside the grace window %d, want 2", got)
	}
	clock.set(start.Add(checkGrace - time.Second))
	if !bridgeDefNames(r, env, memoBoth)[memoMail] {
		t.Fatal("tool dropped before the grace ended")
	}
	clock.set(start.Add(checkGrace + 500*time.Millisecond))
	wantNames(t, bridgeDefNames(r, env, memoBoth), memoAnchor)
}

func TestGraceReuseWindowStartsWhenASlowFailingProbeReturns(t *testing.T) {
	r, st, env := memoRegistry(t)
	clock := installMemoClock(t)
	st.setUp(true)
	start := clock.Now()
	bridgeDefNames(r, env, memoBoth)
	st.mu.Lock()
	st.up = false
	st.duringProbe = func() { clock.advance(5100 * time.Millisecond) }
	st.mu.Unlock()
	clock.set(start.Add(checkTTL + time.Second))
	before := st.probeCount()
	for range 3 {
		if !bridgeDefNames(r, env, memoBoth)[memoMail] {
			t.Fatal("grace verdict dropped the tool")
		}
		clock.advance(500 * time.Millisecond)
	}
	if got := st.probeCount() - before; got != 1 {
		t.Fatalf("slow probe reran %d times", got)
	}
}

type contextLengthCapture struct {
	runtime  [][2]string
	query    *ContextLengthQuery
	cachedAt []string
}

// Fake provider metadata recording each lookup, the runtime hook answering with rt or failing with rtErr
func captureContextLength(t *testing.T, rtBase, rtKey string, rtErr error, answer func(q ContextLengthQuery) int) *contextLengthCapture {
	c := &contextLengthCapture{}
	prevRT, prevCached, prevModel := RuntimeProviderCredentials, CachedContextLength, ModelContextLength
	t.Cleanup(func() {
		RuntimeProviderCredentials, CachedContextLength, ModelContextLength = prevRT, prevCached, prevModel
	})
	RuntimeProviderCredentials = func(_ context.Context, _ *Env, requested, target string) (string, string, error) {
		c.runtime = append(c.runtime, [2]string{requested, target})
		return rtBase, rtKey, rtErr
	}
	CachedContextLength = func(_ context.Context, model, baseURL string) (int, bool) {
		c.cachedAt = append(c.cachedAt, model+" "+baseURL)
		return 0, false
	}
	ModelContextLength = func(_ context.Context, q ContextLengthQuery) int {
		c.query = &q
		return answer(q)
	}
	return c
}

func codexModelEnv(overrides map[string]any) *Env {
	model := map[string]any{"model": "gpt-5.6-sol", "provider": "openai-codex", "base_url": ""}
	for k, v := range overrides {
		model[k] = v
	}
	return &Env{Config: config.Tree{"model": model}}
}

func TestPassesProviderBaseUrlAndKeyFromRuntime(t *testing.T) {
	c := captureContextLength(t, "https://chatgpt.com/backend-api/codex", "tok-live", nil, func(ContextLengthQuery) int { return 272_000 })
	if got := resolveActiveContextLength(context.Background(), codexModelEnv(nil)); got != 272_000 {
		t.Fatalf("context length %d", got)
	}
	want := ContextLengthQuery{Model: "gpt-5.6-sol", BaseURL: "https://chatgpt.com/backend-api/codex", APIKey: "tok-live", Provider: "openai-codex"}
	if *c.query != want || !reflect.DeepEqual(c.runtime, [][2]string{{"openai-codex", "gpt-5.6-sol"}}) {
		t.Fatalf("query %+v runtime %v", *c.query, c.runtime)
	}
}

func TestOfflineCredentialFailureDegradesToConfigValues(t *testing.T) {
	c := captureContextLength(t, "", "", errors.New("no credentials"), func(ContextLengthQuery) int { return 272_000 })
	env := codexModelEnv(map[string]any{"base_url": "https://chatgpt.com/backend-api/codex"})
	if got := resolveActiveContextLength(context.Background(), env); got != 272_000 {
		t.Fatalf("context length %d", got)
	}
	if c.query.Provider != "openai-codex" || c.query.BaseURL != "https://chatgpt.com/backend-api/codex" || c.query.APIKey != "" {
		t.Fatalf("query %+v", *c.query)
	}
}

func TestNoProviderConfiguredSkipsRuntimeResolution(t *testing.T) {
	c := captureContextLength(t, "x", "y", nil, func(ContextLengthQuery) int { return 200_000 })
	env := &Env{Config: config.Tree{"model": map[string]any{"model": "some-model"}}}
	if got := resolveActiveContextLength(context.Background(), env); got != 200_000 {
		t.Fatalf("context length %d", got)
	}
	if c.query.Provider != "" || c.query.BaseURL != "" || len(c.runtime) != 0 || len(c.cachedAt) != 0 {
		t.Fatalf("query %+v runtime %v cached %v", *c.query, c.runtime, c.cachedAt)
	}
}

func TestConfigContextLengthStillShortCircuits(t *testing.T) {
	c := captureContextLength(t, "https://chatgpt.com/backend-api/codex", "tok", nil, func(q ContextLengthQuery) int { return q.ConfigContextLength })
	if got := resolveActiveContextLength(context.Background(), codexModelEnv(map[string]any{"context_length": 150_000})); got != 150_000 {
		t.Fatalf("context length %d", got)
	}
	if c.query.ConfigContextLength != 150_000 || len(c.cachedAt) != 0 {
		t.Fatalf("query %+v cached %v", *c.query, c.cachedAt)
	}
}

func TestToolsetForceAddSuppressed(t *testing.T) {
	t.Setenv("ICEBATH_KANBAN_TASK", "t_worker")
	os.Unsetenv("ICEBATH_NON_DISPATCHER_OWNED_CONTEXT")
	env := &Env{Values: &sync.Map{}}
	if !isDispatcherOwnedWorkerContext(env) {
		t.Fatal("worker env must own its dispatcher")
	}
	env.Values.Store("session.non_dispatcher_owned", true)
	if isDispatcherOwnedWorkerContext(env) {
		t.Fatal("non dispatcher owned execution still reads as the worker")
	}
	env.Values.Delete("session.non_dispatcher_owned")
	if !isDispatcherOwnedWorkerContext(env) || !definitionSelectionContext(env).DispatcherOwnedWorker {
		t.Fatal("ownership not restored after the marker cleared")
	}
}

func TestRewriterChangeReachesAFreshBuild(t *testing.T) {
	ClearToolDefinitionsCache()
	schemaRewritersMu.Lock()
	prev, had := schemaRewriters["clarify"]
	schemaRewritersMu.Unlock()
	t.Cleanup(func() {
		schemaRewritersMu.Lock()
		if had {
			schemaRewriters["clarify"] = prev
		} else {
			delete(schemaRewriters, "clarify")
		}
		schemaRewritersMu.Unlock()
		ClearToolDefinitionsCache()
	})
	r := NewRegistry()
	r.Register(Def{Name: "clarify", Toolset: "clarify", Schema: memoSchema("clarify", "Ask the user."), Handler: constHandler("{}")})
	env := &Env{Home: t.TempDir()}
	opts := DefinitionOptions{Enabled: []string{"clarify"}, Quiet: true, SkipToolSearchAssembly: true}
	description := func() string {
		defs := r.ToolDefinitions(context.Background(), env, opts)
		fn, _ := defs[0]["function"].(map[string]any)
		s, _ := fn["description"].(string)
		return s
	}
	if got := description(); got != "Ask the user." {
		t.Fatalf("static description %q", got)
	}
	changed := "Changed after the session pinned its tools."
	RegisterSchemaRewriter("clarify", func(_ *Env, td map[string]any, _ map[string]bool) map[string]any {
		fn := map[string]any{}
		for k, v := range td["function"].(map[string]any) {
			fn[k] = v
		}
		fn["description"] = changed
		return map[string]any{"type": td["type"], "function": fn}
	})
	if got := description(); got != changed {
		t.Fatalf("rewritten description %q", got)
	}
}
