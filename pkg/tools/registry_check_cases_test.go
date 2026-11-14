package tools

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
)

// Fresh registry with the verdict and definition caches cleared before and after the test
func checkCaseRegistry(t *testing.T) *Registry {
	t.Helper()
	InvalidateCheckCache()
	ClearToolDefinitionsCache()
	t.Cleanup(func() {
		InvalidateCheckCache()
		ClearToolDefinitionsCache()
	})
	return NewRegistry()
}

// Marks the process as a multiplexer for the test
func multiplexFor(t *testing.T) {
	t.Helper()
	SetMultiplexActive(true)
	t.Cleanup(func() { SetMultiplexActive(false) })
}

// Probe reading a profile secret the fail closed way, panicking on an unscoped read like get_secret
func secretProbe(name string) func(*Env) bool {
	return func(env *Env) bool {
		v, err := ResolvePassthroughValue(env, name, nil)
		if err != nil {
			panic(err)
		}
		return v != nil && *v != ""
	}
}

func checkSchema(name string) map[string]any {
	return map[string]any{"name": name, "description": "test-only probe", "parameters": map[string]any{"type": "object", "properties": map[string]any{}}}
}

// Snapshot of the recorded log records
func (h *recordingHandler) snapshot() []slog.Record {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]slog.Record(nil), h.records...)
}

// True when the record carries a panic attribute, the traceback of the hermes exc_info
func recordHasPanic(r slog.Record) bool {
	found := false
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "panic" {
			found = true
		}
		return !found
	})
	return found
}

func TestTransientFailureAfterSuccessIsSuppressed(t *testing.T) {
	r := checkCaseRegistry(t)
	clock := installMemoClock(t)
	env := &Env{}
	calls := 0
	flaky := func(*Env) bool {
		calls++
		return calls == 1
	}
	if !r.checkCached(context.Background(), env, flaky) {
		t.Fatal("first probe not recorded as good")
	}
	clock.advance(checkTTL + time.Second)
	if !r.checkCached(context.Background(), env, flaky) || calls != 2 {
		t.Fatalf("flake within grace not suppressed, calls %d", calls)
	}
}

func TestPersistentFailureAfterGraceIsHonored(t *testing.T) {
	r := checkCaseRegistry(t)
	clock := installMemoClock(t)
	env := &Env{}
	good := func(*Env) bool { return true }
	bad := func(*Env) bool { return false }
	if !r.checkCached(context.Background(), env, good) {
		t.Fatal("good probe unavailable")
	}
	clock.advance(checkGrace + time.Second)
	if r.checkCached(context.Background(), env, bad) {
		t.Fatal("failure with no recent success was suppressed")
	}
}

func TestCoreToolDropAfterSuccessWarnsOnceNeverConfiguredStaysInfo(t *testing.T) {
	r := checkCaseRegistry(t)
	clock := installMemoClock(t)
	env, logs := recordingEnv()
	prevCore := CoreTools
	CoreTools = append(append([]string(nil), CoreTools...), "core_probe_tool", "never_probe_tool")
	t.Cleanup(func() { CoreTools = prevCore })
	core := true
	coreGate := func(*Env) bool { return core }
	neverGate := func(*Env) bool { return false }
	r.Register(Def{Name: "core_probe_tool", Toolset: "core_probe_tool", Schema: map[string]any{"name": "core_probe_tool"}, Handler: constHandler("{}"), Check: coreGate})
	r.Register(Def{Name: "never_probe_tool", Toolset: "never_probe_tool", Schema: map[string]any{"name": "never_probe_tool"}, Handler: constHandler("{}"), Check: neverGate})
	ctx := context.Background()
	if !r.checkCached(ctx, env, coreGate) || r.checkCached(ctx, env, neverGate) {
		t.Fatal("initial verdicts wrong")
	}
	core = false
	clock.advance(checkGrace + checkTTL + time.Second)
	if r.checkCached(ctx, env, coreGate) || r.checkCached(ctx, env, neverGate) {
		t.Fatal("dropped core gate still available")
	}
	clock.advance(checkTTL + time.Second)
	if r.checkCached(ctx, env, coreGate) {
		t.Fatal("core gate available after the drop")
	}
	var coreLevels, neverLevels []slog.Level
	coreMessage := ""
	for _, rec := range logs.snapshot() {
		switch {
		case strings.Contains(rec.Message, fnLabel(coreGate)):
			coreLevels = append(coreLevels, rec.Level)
			if coreMessage == "" {
				coreMessage = rec.Message
			}
		case strings.Contains(rec.Message, fnLabel(neverGate)):
			neverLevels = append(neverLevels, rec.Level)
		}
	}
	if len(coreLevels) != 2 || coreLevels[0] != slog.LevelWarn || coreLevels[1] != slog.LevelInfo || !strings.Contains(coreMessage, "core_probe_tool") {
		t.Fatalf("core gate levels %v message %q", coreLevels, coreMessage)
	}
	if len(neverLevels) == 0 {
		t.Fatal("never configured gate logged nothing")
	}
	for _, level := range neverLevels {
		if level != slog.LevelInfo {
			t.Fatalf("never configured gate logged at %v", level)
		}
	}
}

func TestGraceExpiryLetsRealOutageThrough(t *testing.T) {
	r := checkCaseRegistry(t)
	clock := installMemoClock(t)
	env := &Env{}
	ok := true
	probe := func(*Env) bool { return ok }
	ctx := context.Background()
	if !r.checkCached(ctx, env, probe) {
		t.Fatal("initial probe unavailable")
	}
	ok = false
	clock.advance(checkTTL + time.Second)
	if !r.checkCached(ctx, env, probe) {
		t.Fatal("flake within grace not suppressed")
	}
	clock.advance(checkGrace + time.Second)
	if r.checkCached(ctx, env, probe) {
		t.Fatal("outage past the grace window still suppressed")
	}
}

func TestProfileScopedAvailabilityDoesNotCrossMultiplexProfiles(t *testing.T) {
	r := checkCaseRegistry(t)
	multiplexFor(t)
	root := t.TempDir()
	profileA, profileB := filepath.Join(root, "profiles", "a"), filepath.Join(root, "profiles", "b")
	for _, dir := range []string{profileA, profileB} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(profileA, ".env"), []byte("PROFILE_CACHE_TEST_TOKEN=token-a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(profileB, ".env"), []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	const tool = "profile_scoped_availability_probe"
	r.Register(Def{Name: tool, Toolset: "profile-cache-test", Schema: checkSchema(tool), Handler: constHandler("ok"), Check: secretProbe("PROFILE_CACHE_TEST_TOKEN")})
	names := func(h string) map[string]bool {
		env := &Env{Home: h, Secrets: config.NewSecrets(h)}
		out := map[string]bool{}
		for _, td := range r.ToolDefinitions(context.Background(), env, DefinitionOptions{Enabled: []string{"profile-cache-test"}, Quiet: true, SkipToolSearchAssembly: true}) {
			out[toolDefName(td)] = true
		}
		return out
	}
	if !names(profileA)[tool] || names(profileB)[tool] {
		t.Fatal("availability verdict crossed profiles")
	}
}

func TestUnscopedMultiplexRequestBypassesCache(t *testing.T) {
	r := checkCaseRegistry(t)
	multiplexFor(t)
	ctx := context.Background()
	env := &Env{}
	values, i := []bool{true, false}, 0
	probe := func(*Env) bool {
		v := values[i]
		i++
		return v
	}
	if !r.checkCached(ctx, env, probe) || r.checkCached(ctx, env, probe) {
		t.Fatal("unscoped verdicts were served from the cache")
	}
	checkMu.Lock()
	cached := len(checkCache)
	checkMu.Unlock()
	if cached != 0 {
		t.Fatalf("unscoped request cached %d verdicts", cached)
	}
	builds := 0
	r.Register(Def{Name: "unscoped_probe_tool", Toolset: "unscoped-probe", Schema: checkSchema("unscoped_probe_tool"), Handler: constHandler("{}"),
		Check: func(*Env) bool { builds++; return true }})
	for range 2 {
		r.ToolDefinitions(ctx, env, DefinitionOptions{Enabled: []string{"unscoped-probe"}, Quiet: true, SkipToolSearchAssembly: true})
	}
	toolDefsCacheMu.Lock()
	memo := len(toolDefsCache)
	toolDefsCacheMu.Unlock()
	if builds != 2 || memo != 0 {
		t.Fatalf("unscoped definitions shared a memo: builds %d memo %d", builds, memo)
	}
}

func TestProfileScopedCheckCacheIsBounded(t *testing.T) {
	r := checkCaseRegistry(t)
	multiplexFor(t)
	available := func(*Env) bool { return true }
	for i := range 1000 {
		if !r.checkCached(context.Background(), &Env{Home: fmt.Sprintf("/profiles/%d", i)}, available) {
			t.Fatal("probe unavailable")
		}
	}
	checkMu.Lock()
	defer checkMu.Unlock()
	if len(checkCache) > checkCacheMax || len(checkLastGood) > checkCacheMax {
		t.Fatalf("caches grew past the cap: %d verdicts %d last good", len(checkCache), len(checkLastGood))
	}
}

func TestSubagentKeepsFileToolsThroughDockerFlake(t *testing.T) {
	InvalidateCheckCache()
	ClearToolDefinitionsCache()
	t.Cleanup(func() {
		InvalidateCheckCache()
		ClearToolDefinitionsCache()
	})
	clock := installMemoClock(t)
	flakeAt := clock.Now().Add(time.Second)
	prev := envConfigFn
	envConfigFn = func(*Env) (map[string]any, error) {
		if clock.Now().Before(flakeAt) {
			return map[string]any{"env_type": "local"}, nil
		}
		return nil, errors.New("docker daemon busy")
	}
	t.Cleanup(func() { envConfigFn = prev })
	ctx := context.Background()
	env := &Env{}
	names := func() map[string]bool {
		out := map[string]bool{}
		for _, td := range Default.ToolDefinitions(ctx, env, DefinitionOptions{Enabled: []string{"terminal", "file"}, Quiet: true}) {
			out[toolDefName(td)] = true
		}
		return out
	}
	if !names()["read_file"] {
		t.Fatal("parent build lacks read_file")
	}
	clock.advance(checkTTL + time.Second)
	ClearToolDefinitionsCache()
	child := names()
	for _, name := range []string{"read_file", "write_file", "patch", "search_files", "terminal"} {
		if !child[name] {
			t.Fatalf("subagent build lost %s through the flake: %v", name, child)
		}
	}
}

func TestExpectedFailClosedProbeIsQuietButLostScopeStaysLoud(t *testing.T) {
	multiplexFor(t)
	probe := secretProbe("REGISTRY_LOG_PROBE_TOKEN")
	env, logs := recordingEnv()
	if runCheckLogged(env, probe) {
		t.Fatal("unscoped probe read as available")
	}
	boot := logs.snapshot()
	if len(boot) == 0 {
		t.Fatal("boot time miss logged nothing")
	}
	for _, rec := range boot {
		if rec.Level != slog.LevelDebug || recordHasPanic(rec) {
			t.Fatalf("boot time miss logged %v with panic %v", rec.Level, recordHasPanic(rec))
		}
	}
	scopedLogs := &recordingHandler{}
	scoped := &Env{Log: slog.New(scopedLogs), Secrets: config.NewSecrets(t.TempDir())}
	bareGoroutineProbe := func(*Env) bool { return probe(nil) }
	if runCheckLogged(scoped, bareGoroutineProbe) {
		t.Fatal("lost scope probe read as available")
	}
	loud := false
	for _, rec := range scopedLogs.snapshot() {
		loud = loud || (rec.Level >= slog.LevelWarn && recordHasPanic(rec))
	}
	if !loud {
		t.Fatal("a lost scope inside the probe stayed quiet")
	}
}

func TestUncachedProbeAtBootIsNotReportedAsALostScope(t *testing.T) {
	r := checkCaseRegistry(t)
	multiplexFor(t)
	probe := NoCacheCheck(secretProbe("REGISTRY_LOG_PROBE_TOKEN"))
	t.Cleanup(func() {
		checkMu.Lock()
		delete(noCacheChecks, fnPointer(probe))
		checkMu.Unlock()
	})
	env, logs := recordingEnv()
	if r.checkCached(context.Background(), env, probe) {
		t.Fatal("unscoped uncached probe read as available")
	}
	records := logs.snapshot()
	if len(records) == 0 {
		t.Fatal("uncached boot time miss logged nothing")
	}
	for _, rec := range records {
		if rec.Level != slog.LevelDebug || recordHasPanic(rec) {
			t.Fatalf("uncached boot time miss logged %v with panic %v", rec.Level, recordHasPanic(rec))
		}
	}
}
