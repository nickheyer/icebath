package observability

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

type availabilityAgent struct {
	depth      int
	platform   string
	background bool
	scoped     map[string]bool
}

func (a availabilityAgent) DelegateDepth() int                     { return a.depth }
func (a availabilityAgent) Platform() string                       { return a.platform }
func (a availabilityAgent) BackgroundReview() bool                 { return a.background }
func (a availabilityAgent) ToolSearchScopedNames() map[string]bool { return a.scoped }

// Executor host exposing only the toolset selection the tool search scope resolver reads
type scopedToolsetHost struct {
	tools.ExecutorHost
	env      *tools.Env
	disabled []string
}

func (h scopedToolsetHost) Env() *tools.Env            { return h.env }
func (h scopedToolsetHost) EnabledToolsets() []string  { return nil }
func (h scopedToolsetHost) DisabledToolsets() []string { return h.disabled }

type setupCancelled struct{}

func (setupCancelled) Error() string   { return "setup cancelled" }
func (setupCancelled) SetupCancelled() {}

type setupGoBack struct{ steps int }

func (e setupGoBack) Error() string { return fmt.Sprintf("setup go back %d", e.steps) }
func (setupGoBack) SetupGoBack()    {}

var signalsResource = map[string]string{"icebath_version": "1.0.0", "os_family": "linux", "architecture": "x86_64", "install_method": "git"}

// Surface, provider, event and failure class of every provider setup row, each a valid counter row
func setupEnds(t *testing.T, rows []Mark) [][4]string {
	t.Helper()
	var out [][4]string
	for _, r := range rows {
		if !CounterDimensionsAreValid(ProviderSetupMetric, anyDims(r.Data)) {
			t.Fatalf("invalid setup row %v", r.Data)
		}
		if r.Name == ProviderSetupMark {
			out = append(out, [4]string{r.Data["surface"], r.Data["provider"], r.Data["event"], r.Data["failure_class"]})
		}
	}
	return out
}

func withoutStarted(rows [][4]string) [][4]string {
	return slices.DeleteFunc(rows, func(r [4]string) bool { return r[2] == "started" })
}

func TestOnlyADisabledShippedBuiltinIsReported(t *testing.T) {
	route := map[string]string{"model": "m", "provider": "openrouter"}
	agent := availabilityAgent{}
	fields := ToolUnavailableFields(agent, "browser_exec", "unknown_tool", route)
	if dimsKey(anyDims(fields)) != "model=m,provider=openrouter,tool_name=browser_exec" || !CounterDimensionsAreValid(ToolUnavailableMetric, anyDims(fields)) {
		t.Fatalf("fields %v", fields)
	}
	if ToolUnavailableFields(agent, "mcp_github_search", "unknown_tool", route) != nil ||
		ToolUnavailableFields(agent, "browser_exec", "invalid_json", route) != nil ||
		ToolUnavailableFields(availabilityAgent{depth: 1}, "memory", "unknown_tool", route) != nil ||
		ToolUnavailableFields(availabilityAgent{background: true}, "memory", "unknown_tool", route) != nil {
		t.Fatal("narrowed or non builtin call reported")
	}
}

func TestDeferredBuiltinsAndCronNarrowingAreNotUnavailable(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	env := &tools.Env{Home: os.Getenv("ICEBATH_HOME"), Config: config.Tree{}}
	route := map[string]string{"model": "m", "provider": "openrouter"}
	open := availabilityAgent{scoped: tools.ToolSearchScopedNames(ctx, scopedToolsetHost{env: env})}
	if ToolUnavailableFields(open, tools.TodoToolName, "unknown_tool", route) != nil {
		t.Fatal("deferred builtin reported unavailable")
	}
	narrowed := availabilityAgent{scoped: tools.ToolSearchScopedNames(ctx, scopedToolsetHost{env: env, disabled: []string{"todo"}})}
	if ToolUnavailableFields(narrowed, tools.TodoToolName, "unknown_tool", route) == nil {
		t.Fatal("disabled toolset not reported")
	}
	if ToolUnavailableFields(availabilityAgent{platform: "cron"}, "clarify", "unknown_tool", route) != nil {
		t.Fatal("cron narrowing reported")
	}
}

func TestTurnValidationReportsDisabledBuiltinOnceAndUnknownNamesNever(t *testing.T) {
	m := captureMarks(t)
	agent := &metricsAgentFake{provider: "openrouter", model: "anthropic/claude-sonnet-4", tools: []map[string]any{
		{"type": "function", "function": map[string]any{"name": "memory", "parameters": map[string]any{"type": "object"}}}}}
	RecordToolCallQuality(context.Background(), agent, []EmittedToolCall{{Name: "browser_exec", Arguments: "{}"}, {Name: "memory", Arguments: "{}"}, {Name: "my_plugin_tool", Arguments: "{}"}})
	var unavailable []map[string]string
	for _, r := range m.snapshot() {
		if r.Name == ToolUnavailableMark {
			unavailable = append(unavailable, r.Data)
		}
	}
	if len(unavailable) != 1 || unavailable[0]["tool_name"] != "browser_exec" || unavailable[0]["provider"] != "openrouter" || len(unavailable[0]) != 3 ||
		!CounterDimensionsAreValid(ToolUnavailableMetric, anyDims(unavailable[0])) {
		t.Fatalf("unavailable %v", unavailable)
	}
}

func TestCounterRowsMapToFeatures(t *testing.T) {
	cases := []struct {
		metric string
		dims   map[string]string
		want   []string
	}{
		{MemoryOpMetric, map[string]string{"origin": "foreground", "outcome": "success"}, []string{"memory"}},
		{MemoryOpMetric, map[string]string{"origin": "background_review", "outcome": "success"}, nil},
		{ToolUsageMetric, map[string]string{"tool_name": "mcp", "outcome": "success"}, []string{"mcp"}},
		{ToolUsageMetric, map[string]string{"tool_name": "browser_click", "outcome": "failed"}, nil},
		{ToolUsageMetric, map[string]string{"tool_name": "kanban_create", "outcome": "success"}, []string{"kanban"}},
		{TaskStartedMetric, map[string]string{"platform": "telegram", "execution_surface": "desktop"}, []string{"desktop", "gateway_platform"}},
		{FeatureUsedMark, map[string]string{"feature": "bot_mode"}, []string{"bot_mode"}},
	}
	for _, c := range cases {
		got := FeaturesFor(c.metric, c.dims)
		slices.Sort(got)
		if len(got) == 0 {
			got = nil
		}
		if !slices.Equal(got, c.want) {
			t.Fatalf("%s %v got %v", c.metric, c.dims, got)
		}
	}
}

func TestIcebathInternalWorkNeverLatchesAdoption(t *testing.T) {
	curator := map[string]string{"archived_bucket": "0", "created_bucket": "0", "merged_bucket": "0", "patched_bucket": "0", "outcome": "success"}
	with := func(k, v string) map[string]string {
		out := map[string]string{k: v}
		for key, value := range curator {
			out[key] = value
		}
		return out
	}
	if got := FeaturesFor(CuratorRunMetric, with("trigger", "scheduled")); len(got) != 0 {
		t.Fatalf("scheduled curator %v", got)
	}
	if got := FeaturesFor(CuratorRunMetric, with("trigger", "manual")); !slices.Equal(got, []string{"curator"}) {
		t.Fatalf("manual curator %v", got)
	}
	byReview := map[string]string{"action": "created", "provenance": "agent_created"}
	if len(FeaturesFor(SkillLifecycleMetric, byReview)) != 0 || len(MilestonesFor(SkillLifecycleMetric, byReview)) != 0 {
		t.Fatal("background review latched adoption")
	}
	byUser := map[string]string{"action": "created", "provenance": "local"}
	if !slices.Equal(FeaturesFor(SkillLifecycleMetric, byUser), []string{"skills_created"}) ||
		!slices.Equal(MilestonesFor(SkillLifecycleMetric, byUser), []string{"first_skill_created"}) {
		t.Fatal("user skill not latched")
	}
}

// Profile home whose state.db holds one session started at the given value
func homeWithFirstSession(t *testing.T, startedAt any) string {
	t.Helper()
	dir := t.TempDir()
	conn, err := state.ConnectSQLite("file:"+filepath.Join(dir, "state.db"), state.SQLiteConfig{Timeout: 1, URI: true})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(context.Background(), "CREATE TABLE sessions (id TEXT, started_at)"); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(context.Background(), "INSERT INTO sessions VALUES ('s1', ?)", startedAt); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestUnreadableFirstSessionReadsUnknown(t *testing.T) {
	if got := DaysSinceInstallBucket(context.Background(), homeWithFirstSession(t, "yesterday")); got != "unknown" {
		t.Fatalf("bucket %s", got)
	}
}

func TestDaysSinceInstallBucket(t *testing.T) {
	for _, c := range []struct {
		age    float64
		bucket string
	}{{60, "same_day"}, {2 * 86_400, "1d_to_7d"}, {10 * 86_400, "7d_to_30d"}, {40 * 86_400, "30d_to_90d"}, {200 * 86_400, "gte_90d"}} {
		if got := DaysSinceInstallBucket(context.Background(), homeWithFirstSession(t, pyTime()-c.age)); got != c.bucket {
			t.Fatalf("age %v bucket %s want %s", c.age, got, c.bucket)
		}
	}
}

func TestFeatureAdoptionLatchesOncePerInstall(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store := openStore(t, dir)
	if first, err := store.RecordFeatureAdoption(ctx, "memory", "same_day", signalsResource); err != nil || !first {
		t.Fatalf("first %v %v", first, err)
	}
	again := openStore(t, dir)
	if second, err := again.RecordFeatureAdoption(ctx, "memory", "1d_to_7d", signalsResource); err != nil || second {
		t.Fatalf("second %v %v", second, err)
	}
	if got, err := again.RecordedFeatures(ctx); err != nil || len(got) != 1 || !got.Has("memory") {
		t.Fatalf("recorded %v %v", got, err)
	}
	if _, err := store.RecordFeatureAdoption(ctx, "not_a_feature", "same_day", signalsResource); err == nil {
		t.Fatal("unknown feature accepted")
	}
}

func TestFeatureUsedMarkProjectionAndDisabledGate(t *testing.T) {
	m := captureMarks(t)
	ctx := context.Background()
	RecordFeatureUsed(ctx, "projects", "")
	RecordFeatureUsed(ctx, "nope", "")
	want := []Mark{{Name: FeatureUsedMark, Data: map[string]string{"feature": "projects"}}}
	if got := m.snapshot(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("marks %v", got)
	}
	m.setPolicy(false)
	RecordFeatureUsed(ctx, "projects", "")
	if got := m.snapshot(); len(got) != 1 {
		t.Fatalf("disabled gate %v", got)
	}
	projected, ok := FeatureUsedCounter(&RelayEvent{Kind: "mark", Name: FeatureUsedMark, Data: map[string]any{"feature": "projects"}})
	if ok && (projected.Metric != FeatureUsedMark || dimsKey(anyDims(projected.Dimensions)) != "feature=projects") {
		t.Fatalf("projection %v", projected)
	}
}

func TestStartedThenCompletedOnceAndMarkerCleared(t *testing.T) {
	m := captureMarks(t)
	ctx := context.Background()
	flow := BeginProviderSetup(ctx, "cli_model", "openrouter", "")
	if flow == nil {
		t.Fatal("no flow")
	}
	if _, err := os.Stat(flow.Marker); err != nil {
		t.Fatal(err)
	}
	FinishProviderSetup(ctx, flow, "completed", "none")
	FinishProviderSetup(ctx, flow, "failed", "auth")
	want := [][4]string{{"cli_model", "openrouter", "started", "none"}, {"cli_model", "openrouter", "completed", "none"}}
	if got := setupEnds(t, m.snapshot()); !slices.Equal(got, want) {
		t.Fatalf("rows %v", got)
	}
	if entries, _ := os.ReadDir(setupMarkersDir(m.home)); len(entries) != 0 {
		t.Fatalf("markers left %v", entries)
	}
}

func TestCustomEndpointReadsCustomAndNoRawValues(t *testing.T) {
	m := captureMarks(t)
	ctx := context.Background()
	FinishProviderSetup(ctx, BeginProviderSetup(ctx, "desktop", "custom:my-box", ""), "completed", "none")
	for _, r := range setupEnds(t, m.snapshot()) {
		if r[1] != "custom" {
			t.Fatalf("provider %v", r)
		}
	}
}

func TestDisabledCollectionWritesNothing(t *testing.T) {
	m := captureMarks(t)
	m.setPolicy(false)
	if BeginProviderSetup(context.Background(), "cli_setup", "openrouter", "") != nil {
		t.Fatal("flow while disabled")
	}
	if _, err := os.Stat(setupMarkersDir(m.home)); len(m.snapshot()) != 0 || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("wrote %v %v", m.snapshot(), err)
	}
}

func TestDeadOrStaleMarkerIsAbandonedAtTheNextStartAndLiveWaits(t *testing.T) {
	m := captureMarks(t)
	ctx := context.Background()
	dir := setupMarkersDir(m.home)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	now := pyTime()
	write := func(name string, record map[string]any) string {
		raw, _ := json.Marshal(record)
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	parent := os.Getppid()
	write("cli_setup-1-1.json", map[string]any{"pid": deadPID(t), "start_time": nil, "started_at": now, "surface": "cli_setup", "provider": "anthropic"})
	live := write(fmt.Sprintf("tui-%d-2.json", parent), map[string]any{"pid": parent, "start_time": nil, "started_at": now, "surface": "tui", "provider": "nous"})
	write(fmt.Sprintf("dashboard-%d-3.json", parent), map[string]any{"pid": parent, "start_time": nil, "started_at": now - 2*SetupStaleAfter, "surface": "dashboard", "provider": "xai"})
	flow := BeginProviderSetup(ctx, "cli_setup", "openrouter", "")
	ReportAbandonedSetups(ctx, m.home)
	var abandoned [][4]string
	for _, r := range setupEnds(t, m.snapshot()) {
		if r[2] == "abandoned" {
			abandoned = append(abandoned, r)
		}
	}
	slices.SortFunc(abandoned, func(a, b [4]string) int { return slices.Compare(a[:], b[:]) })
	want := [][4]string{{"cli_setup", "anthropic", "abandoned", "none"}, {"dashboard", "xai", "abandoned", "none"}}
	if !slices.Equal(abandoned, want) {
		t.Fatalf("abandoned %v", abandoned)
	}
	for _, path := range []string{live, flow.Marker} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("live marker gone %v", err)
		}
	}
}

// Writes the profile config model route the CLI flow compares before and after
func writeModelRoute(t *testing.T, homeDir, provider, model string) {
	t.Helper()
	writeProfileConfig(t, homeDir, fmt.Sprintf("model:\n  provider: %s\n  default: %s\n", provider, model))
}

func TestCliFlowClassifiesLandedBackedOutFailedAndRaised(t *testing.T) {
	m := captureMarks(t)
	ctx := context.Background()
	writeModelRoute(t, m.home, "openrouter", "a")
	noop := func(context.Context) error { return nil }
	if err := CLIProviderSetup(ctx, "openrouter", noop); err != nil || len(m.snapshot()) != 0 {
		t.Fatalf("untracked flow recorded %v %v", m.snapshot(), err)
	}
	down := &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
	err := ProviderSetupSurface(ctx, "cli_setup", func(ctx context.Context) error {
		_ = CLIProviderSetup(ctx, "anthropic", func(context.Context) error { NoteProviderSetupSaved(); return nil })
		_ = CLIProviderSetup(ctx, "anthropic", noop)
		_ = CLIProviderSetup(ctx, "nous", func(context.Context) error { NoteProviderSetupFailure("no_models"); return nil })
		_ = CLIProviderSetup(ctx, "gemini", func(context.Context) error { writeModelRoute(t, m.home, "gemini", "g"); return nil })
		if err := CLIProviderSetup(ctx, "xai", func(context.Context) error { return cliui.ErrInterrupt }); !errors.Is(err, cliui.ErrInterrupt) {
			t.Fatalf("interrupt swallowed %v", err)
		}
		if err := CLIProviderSetup(ctx, "xai", func(context.Context) error { return down }); err != down {
			t.Fatalf("connection error swallowed %v", err)
		}
		return CLIProviderSetup(ctx, "remove-custom", noop)
	})
	if err != nil {
		t.Fatal(err)
	}
	want := [][4]string{{"cli_setup", "anthropic", "completed", "none"}, {"cli_setup", "anthropic", "abandoned", "none"},
		{"cli_setup", "nous", "failed", "no_models"}, {"cli_setup", "gemini", "completed", "none"},
		{"cli_setup", "xai", "abandoned", "none"}, {"cli_setup", "xai", "failed", "network"}}
	if got := withoutStarted(setupEnds(t, m.snapshot())); !slices.Equal(got, want) {
		t.Fatalf("ends %v", got)
	}
	NoteProviderSetupSaved()
}

func TestCliSetupNavigationEscCancelsAndBackResumesOneFlow(t *testing.T) {
	m := captureMarks(t)
	ctx := context.Background()
	writeModelRoute(t, m.home, "openrouter", "a")
	attempt := func(ctx context.Context, provider string, raise error) error {
		return CLIProviderSetup(ctx, provider, func(context.Context) error {
			if raise != nil {
				return raise
			}
			NoteProviderSetupSaved()
			return nil
		})
	}
	err := ProviderSetupSurface(ctx, "cli_model", func(ctx context.Context) error {
		if err := attempt(ctx, "xai", setupCancelled{}); !errors.As(err, new(setupCancelled)) {
			t.Fatalf("esc %v", err)
		}
		if err := attempt(ctx, "anthropic", setupGoBack{1}); !errors.As(err, new(setupGoBack)) {
			t.Fatalf("back %v", err)
		}
		_ = attempt(ctx, "anthropic", nil)
		_ = attempt(ctx, "nous", setupGoBack{1})
		return attempt(ctx, "gemini", nil)
	})
	if err != nil {
		t.Fatal(err)
	}
	err = ProviderSetupSurface(ctx, "cli_setup", func(ctx context.Context) error { return attempt(ctx, "xai", setupGoBack{0}) })
	if !errors.As(err, new(setupGoBack)) {
		t.Fatalf("replay %v", err)
	}
	_ = ProviderSetupSurface(ctx, "cli_setup", func(context.Context) error { return nil })
	want := [][4]string{{"cli_model", "xai", "started", "none"}, {"cli_model", "xai", "abandoned", "none"},
		{"cli_model", "anthropic", "started", "none"}, {"cli_model", "anthropic", "completed", "none"},
		{"cli_model", "nous", "started", "none"}, {"cli_model", "nous", "abandoned", "none"},
		{"cli_model", "gemini", "started", "none"}, {"cli_model", "gemini", "completed", "none"},
		{"cli_setup", "xai", "started", "none"}, {"cli_setup", "xai", "abandoned", "none"}}
	if got := setupEnds(t, m.snapshot()); !slices.Equal(got, want) {
		t.Fatalf("rows %v", got)
	}
}

// The setup wizard raises Esc and Back as panics, which must end and resume flows exactly like returned errors
func TestCliSetupNavigationRaisedAsPanicsResumesOneFlow(t *testing.T) {
	m := captureMarks(t)
	ctx := context.Background()
	writeModelRoute(t, m.home, "openrouter", "a")
	raising := func(ctx context.Context, provider string, raise error) error {
		return CLIProviderSetup(ctx, provider, func(context.Context) error {
			if raise != nil {
				panic(raise)
			}
			NoteProviderSetupSaved()
			return nil
		})
	}
	attempt := func(ctx context.Context, provider string, raise error) (recovered any) {
		defer func() { recovered = recover() }()
		_ = raising(ctx, provider, raise)
		return nil
	}
	err := ProviderSetupSurface(ctx, "cli_model", func(ctx context.Context) error {
		if r := attempt(ctx, "xai", setupCancelled{}); r != (setupCancelled{}) {
			t.Fatalf("esc %v", r)
		}
		if r := attempt(ctx, "anthropic", setupGoBack{1}); r != (setupGoBack{1}) {
			t.Fatalf("back %v", r)
		}
		attempt(ctx, "anthropic", nil)
		attempt(ctx, "nous", setupGoBack{1})
		return raising(ctx, "gemini", nil)
	})
	if err != nil {
		t.Fatal(err)
	}
	func() {
		defer func() {
			if r := recover(); r != (setupGoBack{0}) {
				t.Fatalf("replay %v", r)
			}
		}()
		_ = ProviderSetupSurface(ctx, "cli_setup", func(ctx context.Context) error { return raising(ctx, "xai", setupGoBack{0}) })
	}()
	_ = ProviderSetupSurface(ctx, "cli_setup", func(context.Context) error { return nil })
	want := [][4]string{{"cli_model", "xai", "started", "none"}, {"cli_model", "xai", "abandoned", "none"},
		{"cli_model", "anthropic", "started", "none"}, {"cli_model", "anthropic", "completed", "none"},
		{"cli_model", "nous", "started", "none"}, {"cli_model", "nous", "abandoned", "none"},
		{"cli_model", "gemini", "started", "none"}, {"cli_model", "gemini", "completed", "none"},
		{"cli_setup", "xai", "started", "none"}, {"cli_setup", "xai", "abandoned", "none"}}
	if got := setupEnds(t, m.snapshot()); !slices.Equal(got, want) {
		t.Fatalf("rows %v", got)
	}
}

func TestStoreCountsFeatureDisabledOncePerDay(t *testing.T) {
	ctx := context.Background()
	store := openStore(t, t.TempDir())
	dims := map[string]string{"event": "disabled", "kind": "toolset", "name": "memory", "surface": "cli_tools"}
	with := func(k, v string) map[string]string {
		out := map[string]string{k: v}
		for key, value := range dims {
			if key != k {
				out[key] = value
			}
		}
		return out
	}
	for i, c := range []struct {
		dims map[string]string
		want bool
	}{{dims, true}, {with("surface", "desktop"), false}, {with("event", "re_enabled"), true}} {
		if got, err := store.RecordCounterOncePerDay(ctx, FeatureDisabledMetric, c.dims, signalsResource); err != nil || got != c.want {
			t.Fatalf("case %d got %v %v", i, got, err)
		}
	}
}
