package observability

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyctx"
	"github.com/nickheyer/icebath/pkg/tools"
)

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// Routes the default logger into a goroutine safe buffer for the test
func captureSlog(t *testing.T) *lockedBuffer {
	t.Helper()
	buf := &lockedBuffer{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return buf
}

func metricsRoot(ctx context.Context) string {
	return filepath.Join(home.Dir(ctx), "telemetry", "shared_metrics")
}

func dimsOf(v any) map[string]any {
	switch d := v.(type) {
	case map[string]any:
		return d
	case map[string]string:
		out := map[string]any{}
		for k, s := range d {
			out[k] = s
		}
		return out
	}
	return nil
}

type storedValue struct {
	dims  map[string]any
	value int64
}

// Counter rows of the profile store as metric name, dimensions and value
func storedRows(t *testing.T, ctx context.Context) []CounterRow {
	t.Helper()
	rows, err := openStore(t, metricsRoot(ctx)).CounterSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func storedValues(t *testing.T, ctx context.Context, metric string) []storedValue {
	t.Helper()
	var out []storedValue
	for _, row := range storedRows(t, ctx) {
		if row.MetricName == metric {
			out = append(out, storedValue{dimsOf(row.Dimensions), row.Value})
		}
	}
	return out
}

func storedCounters(t *testing.T, ctx context.Context) map[string][]map[string]any {
	t.Helper()
	out := map[string][]map[string]any{}
	for _, row := range storedRows(t, ctx) {
		out[row.MetricName] = append(out[row.MetricName], dimsOf(row.Dimensions))
	}
	return out
}

func outboxPackages(t *testing.T, root string) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(root, "outbox", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	return paths
}

func packageMetricsByName(t *testing.T, path string) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	for _, m := range readPackage(t, path)["metrics"].([]any) {
		metric := m.(map[string]any)
		out[metric["name"].(string)] = metric
	}
	return out
}

func finishDesktopTask(ctx context.Context, sessionID, taskID string) {
	hook := map[string]any{"session_id": sessionID, "task_id": taskID, "platform": "desktop"}
	invokeHook(ctx, "pre_llm_call", hook)
	invokeHook(ctx, "on_session_end", hook, successEnd)
}

func cliTurn(ctx context.Context, sessionID, parentSessionID string) {
	hook := map[string]any{"session_id": sessionID, "task_id": sessionID + "-t", "platform": "cli"}
	invokeHook(ctx, "pre_llm_call", hook, map[string]any{"parent_session_id": parentSessionID})
	FinishTaskRun(ctx, sessionID, sessionID+"-t", "cli", map[string]any{"completed": true}, nil)
}

func writeProfileConfig(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

const enabledConfig = "telemetry:\n  shared_metrics:\n    enabled: true\n"

func TestCoreRuntimeIsFailOpenWithoutAPublishedBinding(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	resetForTests()
	tools.ResetRelayRuntimeForTests()
	tools.RegisterNemoRelayBinding(nil)
	t.Cleanup(tools.ResetRelayRuntimeForTests)
	logs := captureSlog(t)
	ctx := context.Background()
	if rt := tools.GetRelayRuntime(ctx, true, ""); rt != nil {
		t.Fatalf("runtime %v", rt)
	}
	host, ok := tools.RelayHosts.ForProfile(ctx, "", true).(tools.NoopRelayRuntime)
	if !ok || host.ProfileKey != tools.CurrentRelayProfileKey(ctx) || !strings.Contains(host.Reason, "nemo_relay") {
		t.Fatalf("host %#v", host)
	}
	args, err := host.ApplyToolRequestIntercepts(ctx, "s1", "terminal", map[string]any{"command": "true"})
	if err != nil || !reflect.DeepEqual(args, map[string]any{"command": "true"}) {
		t.Fatalf("args %v %v", args, err)
	}
	if !strings.Contains(logs.String(), "Icebath Relay runtime initialization failed") {
		t.Fatalf("log %q", logs.String())
	}
}

func TestManagedConfigCannotOverrideSharedMetricsConsent(t *testing.T) {
	no, yes := false, true
	for _, tc := range []struct {
		profile *bool
		managed bool
	}{{nil, true}, {&no, true}, {&yes, false}} {
		t.Run(fmt.Sprintf("%v-%v", tc.profile != nil && *tc.profile, tc.managed), func(t *testing.T) {
			dir := t.TempDir()
			profile, managed := filepath.Join(dir, "profile"), filepath.Join(dir, "managed")
			body := "{}\n"
			if tc.profile != nil {
				body = fmt.Sprintf("telemetry:\n  shared_metrics:\n    enabled: %v\n", *tc.profile)
			}
			writeProfileConfig(t, profile, body)
			writeProfileConfig(t, managed, fmt.Sprintf("telemetry:\n  shared_metrics:\n    enabled: %v\n", tc.managed))
			t.Setenv("ICEBATH_HOME", filepath.Join(dir, "launch"))
			t.Setenv(config.EnvManagedDir, managed)
			config.ResetCache()
			config.InvalidateManagedCache()
			t.Cleanup(func() {
				resetForTests()
				tools.ResetRelayRuntimeForTests()
				config.ResetCache()
				config.InvalidateManagedCache()
			})
			ctx := home.WithOverride(context.Background(), profile)
			tree, err := config.LoadReadonly(ctx, profile, os.LookupEnv)
			if err != nil {
				t.Fatal(err)
			}
			if got := tree.Get("telemetry.shared_metrics.enabled"); got != tc.managed {
				t.Fatalf("managed value %v", got)
			}
			if got := Enabled(ctx); got != (tc.profile != nil && *tc.profile) {
				t.Fatalf("enabled %v", got)
			}
		})
	}
}

func TestDisablingSharedMetricsStopsCollectionAndShutdownExport(t *testing.T) {
	_, ctx := directRuntime(t)
	StartTaskRun(ctx, nil, "session", "task", "cli", "")
	rt := getRuntime(ctx, false, nil)
	if rt == nil {
		t.Fatal("no runtime")
	}
	writeProfileConfig(t, home.Dir(ctx), "telemetry:\n  shared_metrics:\n    enabled: false\n")
	if Enabled(ctx) {
		t.Fatal("still enabled")
	}
	before, err := rt.Subscriber.Store.CounterSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rt.Subscriber.Call(ctx, &RelayEvent{Kind: "scope", Category: "function", Name: TaskScope, ScopeCategory: "start",
		Metadata: map[string]any{SchemaKey: "icebath.metrics.event.v1", tools.RelayRuntimeInstanceKey: rt.host.RuntimeID},
		Data:     map[string]any{"entrypoint": "interactive", "execution_surface": "cli"}})
	if after, _ := rt.Subscriber.Store.CounterSnapshot(ctx); !reflect.DeepEqual(after, before) {
		t.Fatalf("stale event counted %v", after)
	}
	if _, task, _ := rt.startTask(ctx, nil, map[string]any{"session_id": "session", "task_id": "stale-runtime-task", "platform": "cli"}); task != nil {
		t.Fatalf("stale runtime started %v", task)
	}
	FinishTaskRun(ctx, "session", "task", "cli", map[string]any{"completed": true}, nil)
	resetForTests()
	var names []string
	for _, row := range storedRows(t, ctx) {
		names = append(names, row.MetricName)
	}
	if want := []string{"icebath.client.active", "icebath.install.milestone", "icebath.install.snapshot", "icebath.task_run.started"}; !slices.Equal(names, want) {
		t.Fatalf("metrics %v", names)
	}
	if paths := outboxPackages(t, metricsRoot(ctx)); len(paths) != 0 {
		t.Fatalf("exported %v", paths)
	}
}

func TestSyncSessionRunnerReleasesLockBeforeCallback(t *testing.T) {
	fake, ctx := directRuntime(t)
	host := tools.GetRelayRuntime(ctx, true, "")
	session, err := host.EnsureSession(ctx, map[string]any{"session_id": "sync-session"}, tools.RelaySessionOptions{})
	if host == nil || session == nil || err != nil {
		t.Fatalf("session %v %v", session, err)
	}
	acquired, contender := make(chan struct{}), make(chan struct{})
	result, err := host.RunInSession(ctx, session, func(c context.Context) (any, error) {
		go func() {
			defer close(contender)
			session.Mu.Lock()
			close(acquired)
			session.Mu.Unlock()
		}()
		select {
		case <-acquired:
		case <-time.After(time.Second):
			return nil, errors.New("session lock held across the callback")
		}
		return fake.getScopeStack(c)
	}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-contender:
	case <-time.After(time.Second):
		t.Fatal("contender still alive")
	}
	if result != session.CurrentHandle() {
		t.Fatalf("callback scope %v", result)
	}
}

func handleEvents(f *obsRelayFake, op string, handle any) []fakeEvent {
	var out []fakeEvent
	for _, e := range f.ops(op) {
		if any(e.handle) == handle {
			out = append(out, e)
		}
	}
	return out
}

func TestCloseSessionDrainsOrphanedScopesBeforeSessionPop(t *testing.T) {
	fake, ctx := directRuntime(t)
	host := tools.GetRelayRuntime(ctx, true, "")
	session, err := host.EnsureSession(ctx, map[string]any{"session_id": "orphan-drain"}, tools.RelaySessionOptions{})
	if session == nil || err != nil {
		t.Fatalf("session %v %v", session, err)
	}
	sessionHandle := session.CurrentHandle()
	orphan, err := host.RunInSession(ctx, session, func(c context.Context) (any, error) {
		return fake.push(c, "orphaned-physical-llm", "function", map[string]any{"handle": sessionHandle})
	}, false, nil)
	if orphan == nil || err != nil {
		t.Fatalf("orphan %v %v", orphan, err)
	}
	host.CloseSession(ctx, map[string]any{"session_id": "orphan-drain"})
	if host.GetSession("orphan-drain") != nil {
		t.Fatal("session still registered")
	}
	if len(handleEvents(fake, "scope.pop", orphan)) == 0 {
		t.Fatal("orphaned physical scope was not drained")
	}
	if len(handleEvents(fake, "scope.pop", sessionHandle)) == 0 {
		t.Fatalf("session scope never closed (rejected=%v)", handleEvents(fake, "scope.pop.rejected", sessionHandle))
	}
}

// Two turns on one leased conversation where the second is the concurrent one
func concurrentTurns(t *testing.T, ctx context.Context) (*tools.ConversationLease, *tools.RelayTurnContext, *tools.RelayTurnContext) {
	t.Helper()
	c := tools.RelayCoordinator
	lease := c.AcquireConversation(ctx, tools.RelayConversation{ProfileKey: tools.CurrentRelayProfileKey(ctx), SessionID: "shared-session", Platform: "cli"})
	first, err := c.BeginTurn(ctx, lease, "first", "first-task", nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.BeginTurn(ctx, lease, "second", "second-task", nil)
	if err != nil {
		t.Fatal(err)
	}
	return lease, first, second
}

func assertNoExecutionContext(t *testing.T, ctx context.Context) {
	t.Helper()
	if rt, session, parent, err := tools.ResolveRelayExecutionContext(ctx, "shared-session"); rt != nil || session != nil || parent != nil || err != nil {
		t.Fatalf("execution context %v %v %v %v", rt, session, parent, err)
	}
}

func scopePushes(f *obsRelayFake, names ...string) []fakeEvent {
	var out []fakeEvent
	for _, e := range f.ops("scope.push") {
		if slices.Contains(names, e.name) {
			out = append(out, e)
		}
	}
	return out
}

func TestConcurrentTurnSkipsRelayBeforeScopeStackCanInterleave(t *testing.T) {
	fake, ctx := directRuntime(t)
	c := tools.RelayCoordinator
	lease, first, second := concurrentTurns(t, ctx)
	if !first.RelayEnabled() || first.Handle == nil || second.RelayEnabled() || second.Handle != nil {
		t.Fatalf("turns %v %v", first, second)
	}
	assertNoExecutionContext(t, ctx)
	c.EndTurn(ctx, first, "success")
	c.EndTurn(ctx, second, "success")
	c.ReleaseConversation(lease)
	c.FinalizeConversation(ctx, tools.CurrentRelayProfileKey(ctx), "shared-session")
	if closes := handleEvents(fake, "scope.pop", first.Handle); len(closes) != 1 {
		t.Fatalf("turn closes %v", closes)
	}
	if rejected := fake.ops("scope.pop.rejected"); len(rejected) != 0 {
		t.Fatalf("rejected %v", rejected)
	}
}

func TestConcurrentTurnSkipsSharedMetricsScopeCreation(t *testing.T) {
	fake, ctx := directRuntime(t)
	c := tools.RelayCoordinator
	lease, first, second := concurrentTurns(t, ctx)
	relayObserveLifecycle(ctx, nil, "pre_llm_call", map[string]any{"session_id": "shared-session", "task_id": "second-task", "platform": "cli"})
	relayObserveLifecycle(ctx, nil, "pre_api_request", map[string]any{"session_id": "shared-session", "task_id": "second-task", "api_request_id": "second-request", "platform": "cli"})
	if second.RelayEnabled() {
		t.Fatal("second turn instrumented")
	}
	if pushes := scopePushes(fake, TaskScope); len(pushes) != 0 {
		t.Fatalf("task scopes %v", pushes)
	}
	c.EndTurn(ctx, first, "success")
	c.EndTurn(ctx, second, "success")
	c.ReleaseConversation(lease)
}

func TestSkippedTurnStaysGatedAfterInstrumentedTurnEnds(t *testing.T) {
	fake, ctx := directRuntime(t)
	c := tools.RelayCoordinator
	lease, first, second := concurrentTurns(t, ctx)
	inherited := pyctx.With(ctx, pyctx.CopyContext(ctx))
	c.EndTurn(ctx, first, "success")
	if tools.CurrentRelayTurn(ctx) != second || tools.CurrentRelayTurn(inherited) != second {
		t.Fatal("current turn is not the skipped turn")
	}
	if tools.RelayInstrumentationEnabled(ctx) || tools.RelayInstrumentationEnabled(inherited) {
		t.Fatal("skipped turn instrumented")
	}
	assertNoExecutionContext(t, ctx)
	relayObserveLifecycle(ctx, nil, "pre_llm_call", map[string]any{"session_id": "shared-session", "task_id": "second-task", "platform": "cli"})
	relayObserveLifecycle(inherited, nil, "pre_api_request", map[string]any{"session_id": "shared-session", "task_id": "second-task", "api_request_id": "second-request", "platform": "cli"})
	if pushes := scopePushes(fake, TaskScope, ModelCallScope); len(pushes) != 0 {
		t.Fatalf("scopes %v", pushes)
	}
	c.EndTurn(ctx, second, "success")
	if tools.CurrentRelayTurn(ctx) != nil || tools.CurrentRelayTurn(inherited) != second || tools.RelayInstrumentationEnabled(inherited) {
		t.Fatal("turn context after the skipped turn ended")
	}
	c.ReleaseConversation(lease)
}

func TestToolCategoryComesFromRuntimeRegistryMetadata(t *testing.T) {
	fake, ctx := directRuntime(t)
	saved := toolsetForTool
	toolsetForTool = func(_ context.Context, name string) string {
		if name == "runtime_only_tool" {
			return "terminal"
		}
		return ""
	}
	t.Cleanup(func() { toolsetForTool = saved })
	base := map[string]any{"session_id": "s1", "task_id": "t1", "turn_id": "turn-1", "platform": "cli"}
	invokeHook(ctx, "pre_llm_call", base)
	invokeHook(ctx, "post_tool_call", base, map[string]any{"tool_call_id": "tool-1", "tool_name": "runtime_only_tool", "result": map[string]any{"output": "private"}, "status": "ok"})
	invokeHook(ctx, "on_session_end", base, successEnd)
	finalizeSession(ctx, "s1")
	ends := fake.ops("tool.call_end")
	if len(ends) != 1 || ends[0].payload.(map[string]any)["tool_category"] != "terminal" {
		t.Fatalf("tool ends %v", ends)
	}
}

func TestFailedFlushKeepsDailyExportOpenForLaterTask(t *testing.T) {
	fake, ctx := directRuntime(t)
	now := time.Date(2026, 7, 28, 9, 0, 0, 0, time.UTC)
	setClock(t, &now)
	logs := captureSlog(t)
	original := fake.relay.Subscribers.Flush
	var attempts atomic.Int32
	fake.relay.Subscribers.Flush = func() error {
		if attempts.Add(1) == 1 {
			return errors.New("simulated flush failure")
		}
		return original()
	}
	finish := func(taskID string) {
		finishDesktopTask(ctx, "s1", taskID)
		joinExportWorkers(t, ctx)
	}
	finish("t1")
	root := metricsRoot(ctx)
	if paths := outboxPackages(t, root); len(paths) != 0 {
		t.Fatalf("exported %v", paths)
	}
	db, err := sql.Open("sqlite", filepath.Join(root, "metrics.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	var packages int
	err = db.QueryRow("SELECT COUNT(*) FROM package_outbox").Scan(&packages)
	db.Close()
	if err != nil || packages != 0 {
		t.Fatalf("packages %d %v", packages, err)
	}
	finish("t2")
	paths := outboxPackages(t, root)
	if len(paths) != 1 {
		t.Fatalf("packages %v", paths)
	}
	metrics := packageMetricsByName(t, paths[0])
	if metrics["icebath.task_run.started"]["value"] != 2.0 || metrics["icebath.task_run.finished"]["value"] != 2.0 {
		t.Fatalf("metrics %v", metrics)
	}
	if attempts.Load() != 2 || !strings.Contains(logs.String(), "Icebath shared-metrics task flush failed") {
		t.Fatalf("attempts %d log %q", attempts.Load(), logs.String())
	}
}

func TestTaskEndsDoNotWaitOnTheProcessWideFlushBarrier(t *testing.T) {
	fake, ctx := directRuntime(t)
	now := time.Date(2026, 7, 28, 9, 0, 0, 0, time.UTC)
	setClock(t, &now)
	var attempts atomic.Int32
	entered, release := make(chan struct{}), make(chan struct{})
	var enterOnce, releaseOnce sync.Once
	fake.relay.Subscribers.Flush = func() error {
		attempts.Add(1)
		enterOnce.Do(func() { close(entered) })
		select {
		case <-release:
		case <-time.After(3 * time.Second):
		}
		return nil
	}
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
		joinExportWorkers(t, ctx)
	})
	finishDesktopTask(ctx, "s1", "t1")
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("flush never entered")
	}
	root := metricsRoot(ctx)
	if paths := outboxPackages(t, root); len(paths) != 0 {
		t.Fatalf("exported %v", paths)
	}
	for _, taskID := range []string{"t2", "t3", "t4"} {
		finishDesktopTask(ctx, "s1", taskID)
	}
	releaseOnce.Do(func() { close(release) })
	joinExportWorkers(t, ctx)
	if attempts.Load() != 2 || len(outboxPackages(t, root)) != 1 {
		t.Fatalf("attempts %d packages %v", attempts.Load(), outboxPackages(t, root))
	}
}

func TestBackgroundFlushRunsUnderTheFinishingTurnsProfile(t *testing.T) {
	_, ctx := directRuntime(t)
	now := time.Date(2026, 7, 28, 9, 0, 0, 0, time.UTC)
	setClock(t, &now)
	other := filepath.Join(t.TempDir(), "other-profile")
	writeProfileConfig(t, other, enabledConfig)
	octx := home.WithOverride(ctx, other)
	finishDesktopTask(octx, "s1", "t1")
	joinExportWorkers(t, octx)
	if paths := outboxPackages(t, metricsRoot(octx)); len(paths) != 1 {
		t.Fatalf("profile packages %v", paths)
	}
	if paths := outboxPackages(t, metricsRoot(ctx)); len(paths) != 0 {
		t.Fatalf("launch profile packages %v", paths)
	}
}

func TestSkillLifecycleFlowsThroughRelayToAPrivacySafePackage(t *testing.T) {
	fake, ctx := directRuntime(t)
	common := map[string]any{"skill_name": "private-skill-name", "provenance": "agent_created"}
	invokeHook(ctx, "on_skill_lifecycle", common, map[string]any{"action": "created"})
	invokeHook(ctx, "on_skill_lifecycle", common, map[string]any{"action": "loaded", "use_count": 1, "reused": false, "reuse_after_patch": false})
	invokeHook(ctx, "on_skill_lifecycle", common, map[string]any{"action": "patched"})
	invokeHook(ctx, "on_skill_lifecycle", common, map[string]any{"action": "loaded", "use_count": 2, "reused": true, "reuse_after_patch": true})
	rt := getRuntime(ctx, false, nil)
	if rt == nil {
		t.Fatal("no runtime")
	}
	rt.Shutdown()
	marks := fake.ops("scope.event")
	var names []string
	for _, m := range marks {
		names = append(names, m.name)
	}
	if want := []string{SkillLifecycleMark, SkillLoadMark, SkillLifecycleMark, SkillLoadMark}; !slices.Equal(names, want) {
		t.Fatalf("marks %v", names)
	}
	if strings.Contains(fmt.Sprintf("%v", marks), "private-skill-name") {
		t.Fatal("skill name leaked into marks")
	}
	paths := outboxPackages(t, metricsRoot(ctx))
	if len(paths) != 1 {
		t.Fatalf("packages %v", paths)
	}
	skills := map[string]bool{}
	for name := range packageMetricsByName(t, paths[0]) {
		if strings.HasPrefix(name, "icebath.skill.") {
			skills[name] = true
		}
	}
	if !reflect.DeepEqual(skills, map[string]bool{"icebath.skill.lifecycle.count": true, "icebath.skill.load.count": true}) {
		t.Fatalf("skill metrics %v", skills)
	}
	if body, _ := os.ReadFile(paths[0]); strings.Contains(string(body), "private-skill-name") {
		t.Fatal("skill name leaked into the package")
	}
}

func TestV3FailureDimensionsNeverExportThirdPartyIdentifiers(t *testing.T) {
	_, ctx := directRuntime(t)
	withProviderNames(t, "anthropic")
	base := map[string]any{"session_id": "s1", "task_id": "t1", "api_request_id": "r1", "platform": "telegram", "provider": "anthropic", "model": "claude-sonnet"}
	invokeHook(ctx, "pre_llm_call", base)
	invokeHook(ctx, "pre_api_request", base)
	invokeHook(ctx, "api_request_error", base, map[string]any{"retryable": true, "reason": "rate_limit"})
	for _, c := range [][3]string{{"c1", "acme_private_tool", "AcmePrivateError"}, {"c2", "terminal", "tool_timeout"}} {
		invokeHook(ctx, "pre_tool_call", base, map[string]any{"tool_call_id": c[0], "tool_name": c[1]})
		invokeHook(ctx, "post_tool_call", base, map[string]any{"tool_call_id": c[0], "tool_name": c[1], "status": "error", "error_type": c[2], "duration_ms": 10})
	}
	FinishTaskRun(ctx, "s1", "t1", "telegram", map[string]any{"failed": true, "failure_reason": "rate_limit", "turn_exit_reason": "acme private exit reason"}, nil)
	finalizeSession(ctx, "s1")
	counters := storedCounters(t, ctx)
	if body, _ := json.Marshal(counters); strings.Contains(strings.ToLower(string(body)), "acme") {
		t.Fatalf("third party identifier exported %s", body)
	}
	usage := map[[2]any]bool{}
	for _, d := range counters["icebath.tool.usage.count"] {
		usage[[2]any{d["tool_name"], d["error_class"]}] = true
	}
	if !reflect.DeepEqual(usage, map[[2]any]bool{{"plugin", "exception"}: true, {"terminal", "timeout"}: true}) {
		t.Fatalf("tool usage %v", usage)
	}
	routes := counters["icebath.model_route.count"]
	if len(routes) != 1 || routes[0]["outcome"] != "failed" || routes[0]["error_class"] != "rate_limit" {
		t.Fatalf("routes %v", routes)
	}
	finished := counters["icebath.task_run.finished"]
	if len(finished) != 1 || finished[0]["platform"] != "telegram" || finished[0]["failure_class"] != "rate_limit" {
		t.Fatalf("finished %v", finished)
	}
}

func TestInstallSnapshotIsDailyAndCarriesOnlyBucketedCounts(t *testing.T) {
	_, ctx := directRuntime(t)
	writeProfileConfig(t, home.Dir(ctx), enabledConfig+"memory:\n  provider: acme-private-memory\n"+
		"mcp_servers:\n  acme-private-server:\n    command: x\n  off:\n    enabled: false\n"+
		"plugins:\n  enabled: [acme-private-plugin, b, c]\n")
	for _, taskID := range []string{"t1", "t2"} {
		event := map[string]any{"session_id": taskID, "task_id": taskID, "platform": "cli"}
		invokeHook(ctx, "pre_llm_call", event)
		FinishTaskRun(ctx, taskID, taskID, "cli", map[string]any{"completed": true}, nil)
		finalizeSession(ctx, taskID)
	}
	resetForTests()
	invokeHook(ctx, "pre_llm_call", map[string]any{"session_id": "t3", "task_id": "t3", "platform": "cli"})
	finalizeSession(ctx, "t3")
	snapshots := storedCounters(t, ctx)["icebath.install.snapshot"]
	if len(snapshots) != 1 {
		t.Fatalf("snapshots %v", snapshots)
	}
	snapshot := snapshots[0]
	want := map[string]any{
		"cron_job_count_bucket": "0", "display_language": "en", "main_provider": "none", "mcp_server_count_bucket": "1",
		"memory_provider": "plugin", "messaging_platform_count_bucket": "0", "plugin_count_bucket": "3_to_5",
		"skill_count_bucket": "0", "terminal_backend": "local", "local_model_provider_used": "no", "compression_prepare_ahead": "off",
	}
	for _, k := range []string{"install_age_bucket", "profile_count_bucket", "behind_bucket", "gpu_class", "ram_bucket", "release_channel", "version_age_bucket"} {
		want[k] = snapshot[k]
	}
	if !reflect.DeepEqual(snapshot, want) {
		t.Fatalf("snapshot %v", snapshot)
	}
}

func TestSessionsSummarizeOnCloseAndMilestonesLatchOncePerInstall(t *testing.T) {
	_, ctx := directRuntime(t)
	for _, s := range []struct {
		id    string
		turns int
	}{{"s1", 3}, {"s2", 1}} {
		for turn := range s.turns {
			task := fmt.Sprintf("%s-t%d", s.id, turn)
			invokeHook(ctx, "pre_llm_call", map[string]any{"session_id": s.id, "task_id": task, "platform": "cli"})
			FinishTaskRun(ctx, s.id, task, "cli", map[string]any{"completed": true}, nil)
		}
		finalizeSession(ctx, s.id)
	}
	var sessions []string
	for _, v := range storedValues(t, ctx, "icebath.session.count") {
		sessions = append(sessions, fmt.Sprintf("%v/%v/%d", v.dims["turn_count_bucket"], v.dims["last_outcome"], v.value))
	}
	slices.Sort(sessions)
	if want := []string{"1/success/1", "3_to_5/success/1"}; !slices.Equal(sessions, want) {
		t.Fatalf("sessions %v", sessions)
	}
	milestones := map[any]int64{}
	for _, v := range storedValues(t, ctx, "icebath.install.milestone") {
		milestones[v.dims["milestone"]] = v.value
	}
	if milestones["first_task_started"] != 1 || milestones["first_task_success"] != 1 {
		t.Fatalf("milestones %v", milestones)
	}
}

func TestUserNamedProvidersAndModelsNeverReachCounters(t *testing.T) {
	_, ctx := directRuntime(t)
	setCatalog(t, "provider_names", "lmstudio", "openrouter")
	setCatalog(t, "user_named_model_providers", "custom", "lmstudio")
	RecordSetupCompleted(ctx, "cli", "custom:acme-secret-llm")
	RecordModelSwitch(ctx, "custom:acme-secret-llm", "lmstudio", "cli", nil, nil)
	RecordFallback(ctx, "acme-unknown", "openrouter", "rate_limit")
	rt := getRuntime(ctx, true, nil)
	if rt == nil {
		t.Fatal("no runtime")
	}
	for _, pm := range [][2]string{{"custom:acme-secret-llm", "acme-internal"}, {"lmstudio", "bob-finetune"}, {"openrouter", "c:/users/bob/model.gguf"}, {"openrouter", "anthropic/claude-sonnet"}} {
		if err := rt.recordAuxiliaryTokens(ctx, map[string]any{"usage": map[string]any{"input_tokens": 5}, "aux_task": "compression", "provider": pm[0], "model": pm[1]}); err != nil {
			t.Fatal(err)
		}
	}
	if err := rt.relay.Subscribers.Flush(); err != nil {
		t.Fatal(err)
	}
	stored, _ := json.Marshal(storedRows(t, ctx))
	for _, leak := range []string{"acme", "bob", "users"} {
		if strings.Contains(string(stored), leak) {
			t.Fatalf("%s leaked %s", leak, stored)
		}
	}
	tokens := map[[2]any]bool{}
	for _, v := range storedValues(t, ctx, "icebath.model_tokens.sum") {
		tokens[[2]any{v.dims["provider"], v.dims["model"]}] = true
	}
	want := map[[2]any]bool{{"custom", "custom"}: true, {"lmstudio", "custom"}: true, {"openrouter", "custom"}: true, {"openrouter", "anthropic/claude-sonnet"}: true}
	if !reflect.DeepEqual(tokens, want) {
		t.Fatalf("tokens %v", tokens)
	}
}

func TestMilestoneInstallAgeIsTheSubscriberProfileNotTheRelayThread(t *testing.T) {
	dir := t.TempDir()
	launch, other := filepath.Join(dir, "A"), filepath.Join(dir, "B")
	mustMkdir(t, launch)
	mustMkdir(t, other)
	db, err := sql.Open("sqlite", filepath.Join(other, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec("CREATE TABLE sessions (id TEXT, started_at REAL)")
	if err == nil {
		_, err = db.Exec("INSERT INTO sessions VALUES ('s1', ?)", float64(time.Now().Unix()-120*86400))
	}
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", launch)
	octx := home.WithOverride(context.Background(), other)
	store, err := NewSharedMetricsStore(octx, "", "")
	if err != nil {
		t.Fatal(err)
	}
	subscriber, err := NewSharedMetricsSubscriber(octx, store, "1.0", nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := subscriber.recordMilestones(ctx, "icebath.setup.completed", map[string]string{"provider": "nous", "surface": "cli"}); err != nil {
		t.Fatal(err)
	}
	rows, err := subscriber.Store.CounterSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var ages []any
	for _, row := range rows {
		if row.MetricName == "icebath.install.milestone" {
			ages = append(ages, dimsOf(row.Dimensions)["install_age_bucket"])
		}
	}
	if !reflect.DeepEqual(ages, []any{"gte_90d"}) {
		t.Fatalf("ages %v", ages)
	}
}

func TestTokenUsageIsSummedPerModelAndAuxiliaryTask(t *testing.T) {
	_, ctx := directRuntime(t)
	withProviderNames(t, "anthropic", "openrouter")
	setCatalog(t, "aux_task_names", "compression")
	base := map[string]any{"session_id": "s1", "task_id": "t1", "provider": "anthropic", "model": "claude-sonnet"}
	for _, requestID := range []string{"r1", "r2"} {
		invokeHook(ctx, "pre_api_request", base, map[string]any{"api_request_id": requestID})
		invokeHook(ctx, "post_api_request", base, map[string]any{"api_request_id": requestID, "usage": map[string]any{"input_tokens": 100, "output_tokens": 10, "cache_read_tokens": 40}})
	}
	for _, auxTask := range []string{"compression", "acme-private-task"} {
		invokeHook(ctx, "post_auxiliary_call", map[string]any{"aux_task": auxTask, "provider": "openrouter", "model": "small/model", "usage": map[string]any{"input_tokens": 7, "output_tokens": 3}})
	}
	FinishTaskRun(ctx, "s1", "t1", "cli", map[string]any{"completed": true}, nil)
	finalizeSession(ctx, "s1")
	sums := map[[3]any]int64{}
	for _, v := range storedValues(t, ctx, "icebath.model_tokens.sum") {
		sums[[3]any{v.dims["call_role"], v.dims["aux_task"], v.dims["token_type"]}] = v.value
	}
	want := map[[3]any]int64{
		{"primary", "none", "input"}: 200, {"primary", "none", "output"}: 20, {"primary", "none", "cache_read"}: 80,
		{"auxiliary", "compression", "input"}: 7, {"auxiliary", "compression", "output"}: 3,
		{"auxiliary", "other", "input"}: 7, {"auxiliary", "other", "output"}: 3,
	}
	if !reflect.DeepEqual(sums, want) {
		t.Fatalf("sums %v", sums)
	}
}

func TestFiniteCliRunsReportTheirOwnEntrypoint(t *testing.T) {
	_, ctx := directRuntime(t)
	for _, name := range []string{"ICEBATH_SINGLE_QUERY_SESSION", "ICEBATH_SESSION_SOURCE"} {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
	cliTurn(ctx, "repl", "")
	t.Setenv("ICEBATH_SINGLE_QUERY_SESSION", "1")
	cliTurn(ctx, "oneshot", "")
	cliTurn(ctx, "child", "oneshot")
	finishDesktopTask(ctx, "desktop", "desktop-t")
	t.Setenv("ICEBATH_SESSION_SOURCE", "kanban")
	cliTurn(ctx, "kanban", "")
	for _, id := range []string{"repl", "oneshot", "child", "desktop", "kanban"} {
		finalizeSession(ctx, id)
	}
	joinExportWorkers(t, ctx)
	var started []string
	for _, v := range storedValues(t, ctx, "icebath.task_run.started") {
		started = append(started, fmt.Sprintf("%v/%v/%d", v.dims["entrypoint"], v.dims["execution_surface"], v.value))
	}
	slices.Sort(started)
	if want := []string{"background/cli/1", "delegated/cli/1", "interactive/cli/1", "interactive/desktop/1", "one_shot/cli/1"}; !slices.Equal(started, want) {
		t.Fatalf("started %v", started)
	}
	finished := map[any]bool{}
	for _, v := range storedValues(t, ctx, "icebath.task_run.finished") {
		finished[v.dims["entrypoint"]] = true
	}
	if !reflect.DeepEqual(finished, map[any]bool{"background": true, "delegated": true, "interactive": true, "one_shot": true}) {
		t.Fatalf("finished %v", finished)
	}
	var sessions []string
	for _, v := range storedValues(t, ctx, "icebath.session.count") {
		sessions = append(sessions, fmt.Sprintf("%v/%v/%v", v.dims["entrypoint"], v.dims["execution_surface"], v.dims["turn_count_bucket"]))
	}
	slices.Sort(sessions)
	if want := []string{"background/cli/1", "interactive/cli/1", "interactive/desktop/1", "one_shot/cli/1"}; !slices.Equal(sessions, want) {
		t.Fatalf("sessions %v", sessions)
	}
}

func TestDirectRuntimeRecordsWithoutEnablingAPlugin(t *testing.T) {
	fake, ctx := directRuntime(t)
	withProviderNames(t, "anthropic")
	setCatalog(t, "user_named_model_providers", "custom")
	base := map[string]any{"session_id": "sensitive-session", "task_id": "task-1", "turn_id": "turn-1", "api_request_id": "request-1",
		"platform": "cli", "provider": "custom", "model": "gpt-sensitive-model-id", "base_url": "http://127.0.0.1:11434/v1"}
	if !HandlesHook(ctx, "pre_api_request") {
		t.Fatal("pre_api_request not handled")
	}
	prompt := map[string]any{"request": map[string]any{"body": map[string]any{"messages": []any{"sensitive-prompt"}}}}
	invokeHook(ctx, "on_session_start", base)
	invokeHook(ctx, "pre_llm_call", base)
	invokeHook(ctx, "pre_api_request", base, prompt)
	invokeHook(ctx, "pre_tool_call", base, map[string]any{"tool_call_id": "sensitive-tool-call", "tool_name": "terminal", "toolset": "terminal", "args": map[string]any{"command": "sensitive-command"}})
	invokeHook(ctx, "post_approval_response", map[string]any{"turn_id": "turn-1", "tool_call_id": "sensitive-tool-call", "choice": "once", "command": "sensitive-command", "description": "sensitive-approval-description"})
	invokeHook(ctx, "post_tool_call", base, map[string]any{"tool_call_id": "sensitive-tool-call", "tool_name": "terminal", "toolset": "terminal", "args": map[string]any{"command": "sensitive-command"},
		"result": map[string]any{"output": "sensitive-tool-result"}, "status": "ok", "duration_ms": 275, "retry_count": 0})
	invokeHook(ctx, "api_request_error", base, map[string]any{"retryable": true, "error": map[string]any{"message": "sensitive-error"}})
	anthropic := mergeEvent(base, map[string]any{"provider": "anthropic", "model": "claude-sonnet", "base_url": "https://api.anthropic.com"})
	invokeHook(ctx, "pre_api_request", anthropic, prompt)
	invokeHook(ctx, "post_api_request", anthropic, map[string]any{"response": map[string]any{"content": "sensitive-response"}})
	invokeHook(ctx, "on_session_end", base, successEnd)
	joinExportWorkers(t, ctx)
	finalizeSession(ctx, "sensitive-session")
	starts, ends, toolStarts, toolEnds, scopeStarts := fake.ops("llm.call"), fake.ops("llm.call_end"), fake.ops("tool.call"), fake.ops("tool.call_end"), fake.ops("scope.push")
	if len(scopeStarts) != 2 || scopeStarts[0].scopeType != fake.relay.ScopeType.Agent || scopeStarts[1].name != TaskScope || scopeStarts[1].scopeType != fake.relay.ScopeType.Function {
		t.Fatalf("scope starts %v", scopeStarts)
	}
	if h, _ := scopeStarts[1].kwargs["handle"].(*fakeHandle); h == nil || h.name != tools.RelaySessionScope {
		t.Fatalf("task parent %v", scopeStarts[1].kwargs["handle"])
	}
	if !reflect.DeepEqual(scopeStarts[1].kwargs["input"], map[string]any{"entrypoint": "interactive", "execution_surface": "cli", "platform": "none"}) {
		t.Fatalf("task input %v", scopeStarts[1].kwargs["input"])
	}
	if len(starts) != 1 || len(ends) != 1 || len(toolStarts) != 1 || len(toolEnds) != 1 {
		t.Fatalf("calls %d %d %d %d", len(starts), len(ends), len(toolStarts), len(toolEnds))
	}
	if toolStarts[0].name != "icebath.tool_call" || !reflect.DeepEqual(toolStarts[0].payload, map[string]any{}) {
		t.Fatalf("tool start %v", toolStarts[0])
	}
	wantToolEnd := map[string]any{"approval_outcome": "approved", "error_class": "none", "latency_bucket": "250ms_to_500ms", "outcome": "success",
		"retry_count_bucket": "0", "tool_category": "terminal", "tool_name": "terminal"}
	if !reflect.DeepEqual(toolEnds[0].payload, wantToolEnd) {
		t.Fatalf("tool end %v", toolEnds[0].payload)
	}
	if !reflect.DeepEqual(starts[0].payload, map[string]any{}) || starts[0].kwargs["model_name"] != "unknown" {
		t.Fatalf("llm start %v", starts[0])
	}
	var active []fakeEvent
	for _, e := range fake.ops("scope.event") {
		if e.name == "icebath.client.active" {
			active = append(active, e)
		}
	}
	if len(active) != 2 || !reflect.DeepEqual(active[0].kwargs["data"], map[string]any{}) || !reflect.DeepEqual(active[1].kwargs["data"], map[string]any{}) {
		t.Fatalf("active marks %v", active)
	}
	route := map[string]any{"call_role": "primary", "error_class": "unknown", "model": "claude-sonnet", "outcome": "success", "provider": "anthropic", "ttft_bucket": "unknown"}
	if !reflect.DeepEqual(ends[0].payload, route) {
		t.Fatalf("llm end %v", ends[0].payload)
	}
	fake.mu.Lock()
	serialized := fmt.Sprintf("%v", fake.events)
	fake.mu.Unlock()
	for _, secret := range []string{"sensitive-prompt", "sensitive-response", "sensitive-error", "sensitive-command", "sensitive-tool-result",
		"sensitive-tool-call", "sensitive-approval-description", "gpt-sensitive-model-id"} {
		if strings.Contains(serialized, secret) {
			t.Fatalf("%s reached the relay events", secret)
		}
	}
	paths := outboxPackages(t, metricsRoot(ctx))
	if len(paths) != 1 {
		t.Fatalf("packages %v", paths)
	}
	metrics := packageMetricsByName(t, paths[0])
	names := slices.Sorted(func(yield func(string) bool) {
		for name := range metrics {
			if !yield(name) {
				return
			}
		}
	})
	wantNames := []string{"icebath.client.active", "icebath.install.milestone", "icebath.install.snapshot", "icebath.model_route.count",
		"icebath.task_cost.count", "icebath.task_run.duration", "icebath.task_run.finished", "icebath.task_run.started",
		"icebath.tool.usage.count", "icebath.tool_approval.count", "icebath.tool_call.count", "icebath.tool_call.latency"}
	if !slices.Equal(names, wantNames) {
		t.Fatalf("package metrics %v", names)
	}
	counter := func(name string, dims map[string]any) map[string]any {
		return map[string]any{"name": name, "type": "counter", "dimensions": dims, "value": 1.0}
	}
	checks := map[string]any{
		"icebath.tool.usage.count":    map[string]any{"error_class": "none", "outcome": "success", "tool_name": "terminal"},
		"icebath.model_route.count":   route,
		"icebath.tool_call.latency":   map[string]any{"latency_bucket": "250ms_to_500ms", "retry_count_bucket": "0", "tool_category": "terminal"},
		"icebath.task_run.finished":   map[string]any{"end_reason": "completed", "entrypoint": "interactive", "execution_surface": "cli", "failure_class": "none", "outcome": "success", "platform": "none", "termination": "none"},
		"icebath.client.active":       counter("icebath.client.active", map[string]any{}),
		"icebath.tool_call.count":     counter("icebath.tool_call.count", map[string]any{"approval_outcome": "approved", "outcome": "success", "tool_category": "terminal"}),
		"icebath.tool_approval.count": counter("icebath.tool_approval.count", map[string]any{"attribution": "tool_call", "outcome": "approved"}),
		"icebath.task_run.started":    counter("icebath.task_run.started", map[string]any{"entrypoint": "interactive", "execution_surface": "cli", "platform": "none"}),
	}
	for name, want := range checks {
		got := any(metrics[name])
		if _, whole := want.(map[string]any)["type"]; !whole {
			got = metrics[name]["dimensions"]
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s %v", name, got)
		}
	}
	if metrics["icebath.install.snapshot"]["value"] != 1.0 || metrics["icebath.model_route.count"]["value"] != 1.0 {
		t.Fatalf("values %v %v", metrics["icebath.install.snapshot"], metrics["icebath.model_route.count"])
	}
	duration := maps.Clone(metrics["icebath.task_run.duration"]["dimensions"].(map[string]any))
	if !slices.Contains([]any{"lt_1s", "1s_to_5s", "5s_to_30s", "30s_to_2m", "2m_to_10m", "gte_10m"}, duration["duration_bucket"]) {
		t.Fatalf("duration %v", duration)
	}
	delete(duration, "duration_bucket")
	if !reflect.DeepEqual(duration, map[string]any{"execution_surface": "cli", "outcome": "success", "retry_count_bucket": "1"}) {
		t.Fatalf("duration %v", duration)
	}
	cost := metrics["icebath.task_cost.count"]["dimensions"].(map[string]any)
	if cost["api_calls_bucket"] != "1" || cost["tool_calls_bucket"] != "1" {
		t.Fatalf("cost %v", cost)
	}
}
