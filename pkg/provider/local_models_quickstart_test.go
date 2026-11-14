package provider

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

const lmGiB = int64(1) << 30

// Budget of the given usable, device and RAM sizes in GiB
func lmBudget(usable, total, ram float64, uma bool) HardwareBudget {
	b := NewHardwareBudget(int64(usable*float64(lmGiB)), int64(total*float64(lmGiB)), int64(ram*float64(lmGiB)))
	b.UMA = uma
	return b
}

// Polls a quickstart job until it leaves the running state
func lmWaitJob(t *testing.T, jobID string) map[string]any {
	t.Helper()
	return lmPoll(t, jobID, 10*time.Second, "paused", "done", "error")
}

// Concurrency safe ordered call log
type lmCalls struct {
	mu    sync.Mutex
	items []any
}

func (c *lmCalls) add(item any) {
	c.mu.Lock()
	c.items = append(c.items, item)
	c.mu.Unlock()
}

func (c *lmCalls) list() []any {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]any(nil), c.items...)
}

// Writes local_runtime settings into the temp home's config
func lmSetRuntimeConfig(t *testing.T, settings map[string]any) {
	t.Helper()
	ctx := context.Background()
	cfg, err := configLoadFn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	section, _ := cfg["local_runtime"].(map[string]any)
	if section == nil {
		section = map[string]any{}
		cfg["local_runtime"] = section
	}
	for k, v := range settings {
		section[k] = v
	}
	if err := configSaveFn(ctx, cfg); err != nil {
		t.Fatal(err)
	}
}

func TestQuickstartUnknownModel404s(t *testing.T) {
	lmTestHome(t, ".icebath")
	if _, err := LocalModelsQuickstart(context.Background(), "no-such", ""); lmStatusOf(err) != 404 {
		t.Fatalf("quickstart %v", err)
	}
}

func TestQuickstartWithoutRecommendationRequiresExplicitChoice(t *testing.T) {
	lmTestHome(t, ".icebath")
	ctx := context.Background()
	budget := NewHardwareBudget(14*lmGiB, 16*lmGiB, 64*lmGiB)
	lmDeps.probeBudget = func(bool) (HardwareBudget, error) { return budget, nil }
	lmDeps.refreshCatalogSoon = func() {}
	lmDeps.installedEngine = func(context.Context, string) (*Engine, error) {
		return &Engine{Backend: "cpu", Tag: "b1", Binary: "unused"}, nil
	}
	lmDeps.runtimeTarget = func(context.Context, string) (string, string, error) { return "b1", "cpu", nil }
	lmDeps.stagedModelIDs = func() []string { return nil }
	lmSetRuntimeConfig(t, map[string]any{"backend": "cpu", "enabled": false})
	calls := &lmCalls{}
	var chosenModelID string
	lmDeps.runDownloadPlan = func(ctx context.Context, job map[string]any, plan []lmPlanRow, label string, entry *CatalogEntry) error {
		calls.add([]any{"download", label})
		return nil
	}
	lmDeps.ensureLocalRuntime = func(ctx context.Context, cfg map[string]any, force bool) (localModelsSupervisor, error) {
		calls.add([]any{"server", cfg["local_runtime"].(map[string]any)["enabled"], force})
		return &lmFakeSupervisor{models: lmRouterModels(map[string]any{chosenModelID: "loaded"})}, nil
	}
	lmDeps.applyAssignment = func(ctx context.Context, scope, provider, model, task, baseURL, apiKey string) (map[string]any, error) {
		calls.add([]any{"assign", scope, provider, model, task, baseURL, apiKey})
		return nil, nil
	}
	catalog, err := LocalModelsCatalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var chosen map[string]any
	for _, raw := range catalog["models"].([]any) {
		row := raw.(map[string]any)
		if row["recommended"] == true {
			t.Fatalf("row %v recommended", row["id"])
		}
		if row["id"] == "qwen3.8-27b" {
			chosen = row
		}
	}
	if chosen == nil || chosen["fits"] != true || chosen["spilled"] != true {
		t.Fatalf("chosen %v", chosen)
	}
	chosenModelID = chosen["model_id"].(string)
	_, err = LocalModelsQuickstart(ctx, "", "")
	if lmStatusOf(err) != 409 || !strings.Contains(strings.ToLower(err.(*HTTPException).Detail), "no automatic recommendation") {
		t.Fatalf("automatic %v", err)
	}
	if got := calls.list(); len(got) != 0 {
		t.Fatalf("calls %v", got)
	}
	if got := lmRuntimeEnabled(t); got != false {
		t.Fatalf("enabled %v", got)
	}
	result, err := LocalModelsQuickstart(ctx, chosen["id"].(string), "")
	if err != nil {
		t.Fatal(err)
	}
	if result["model_id"] != chosen["id"] || result["needs_download"] != true || result["needs_runtime"] != false {
		t.Fatalf("result %v", result)
	}
	job := lmWaitJob(t, result["job_id"].(string))
	if job["status"] != "done" {
		t.Fatalf("job %v", job["error"])
	}
	want := []any{
		[]any{"download", chosen["display_name"]},
		[]any{"server", true, true},
		[]any{"assign", "main", "llamacpp", chosenModelID, "", "", ""},
	}
	if got := calls.list(); !reflect.DeepEqual(got, want) {
		t.Fatalf("calls %v", got)
	}
	if got := lmRuntimeEnabled(t); got != true {
		t.Fatalf("enabled %v", got)
	}
}

func TestQuickstartRefusesWhenNothingFits(t *testing.T) {
	lmTestHome(t, ".icebath")
	catalogSelectVariantFn = func(CatalogEntry, HardwareBudget) *VariantChoice { return nil }
	_, err := LocalModelsQuickstart(context.Background(), "", "")
	if lmStatusOf(err) != 409 || !strings.Contains(err.(*HTTPException).Detail, "Local Models") {
		t.Fatalf("quickstart %v", err)
	}
}

// Success path orchestration needs a model to fit, independent of host load
func lmCapableHardware(t *testing.T) {
	budget := NewHardwareBudget(64*lmGiB, 64*lmGiB, 128*lmGiB)
	lmDeps.probeBudget = func(bool) (HardwareBudget, error) { return budget, nil }
}

func TestQuickstartRunsAllThreeLegs(t *testing.T) {
	lmTestHome(t, ".icebath")
	lmCapableHardware(t)
	calls := &lmCalls{}
	lmSetRuntimeConfig(t, map[string]any{"backend": "cpu"})
	lmDeps.installedEngine = func(context.Context, string) (*Engine, error) { return nil, nil }
	lmDeps.ensureEngine = func(context.Context, string, EnsureEngineOptions) (Engine, error) {
		calls.add("install")
		return Engine{}, nil
	}
	lmDeps.downloadJob = func(ctx context.Context, job map[string]any, plan []lmPlanRow) error {
		for _, row := range plan {
			if err := os.MkdirAll(filepath.Dir(row.Dest), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(row.Dest, []byte("GGUF\x00"), 0o644); err != nil {
				return err
			}
		}
		calls.add("download")
		return nil
	}
	lmDeps.ensureLocalRuntime = func(context.Context, map[string]any, bool) (localModelsSupervisor, error) {
		calls.add("server")
		return nil, nil
	}
	lmDeps.stateEndpoint = func() (map[string]any, error) {
		return map[string]any{"base_url": "http://127.0.0.1:1/v1", "api_key": "k"}, nil
	}
	lmDeps.applyAssignment = func(context.Context, string, string, string, string, string, string) (map[string]any, error) {
		calls.add("assign")
		return nil, nil
	}
	body, err := LocalModelsQuickstart(context.Background(), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if body["needs_runtime"] != true || body["needs_download"] != true || body["download_bytes"].(int64) <= 0 {
		t.Fatalf("body %v", body)
	}
	job := lmWaitJob(t, body["job_id"].(string))
	if job["status"] != "done" {
		t.Fatalf("job %v", job["error"])
	}
	if job["kind"] != "quickstart" {
		t.Fatalf("kind %v", job["kind"])
	}
	got := calls.list()
	if len(got) == 0 || got[0] != "install" || !slices.Contains(got, any("download")) {
		t.Fatalf("calls %v", got)
	}
	if !(slices.Index(got, any("install")) < slices.Index(got, any("download")) && slices.Index(got, any("download")) < slices.Index(got, any("assign"))) {
		t.Fatalf("leg order %v", got)
	}
	if enabled := lmRuntimeEnabled(t); enabled != true {
		t.Fatalf("enabled %v", enabled)
	}
}

func TestQuickstartSkipsSatisfiedLegs(t *testing.T) {
	lmTestHome(t, ".icebath")
	lmCapableHardware(t)
	calls := &lmCalls{}
	lmDeps.installedEngine = func(context.Context, string) (*Engine, error) {
		return &Engine{Backend: "cpu", Tag: "b10362", Binary: "unused"}, nil
	}
	lmDeps.ensureEngine = func(context.Context, string, EnsureEngineOptions) (Engine, error) {
		calls.add("install")
		return Engine{}, nil
	}
	for _, entry := range Catalog() {
		for _, variant := range entry.Variants {
			for _, row := range lmDownloadPlan(entry, variant) {
				if err := os.MkdirAll(filepath.Dir(row.Dest), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(row.Dest, []byte("downloaded fixture"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	lmDeps.downloadJob = func(context.Context, map[string]any, []lmPlanRow) error {
		calls.add("download")
		return nil
	}
	lmDeps.ensureLocalRuntime = func(context.Context, map[string]any, bool) (localModelsSupervisor, error) { return nil, nil }
	lmDeps.stateEndpoint = func() (map[string]any, error) {
		return map[string]any{"base_url": "http://127.0.0.1:1/v1", "api_key": "k"}, nil
	}
	lmDeps.applyAssignment = func(context.Context, string, string, string, string, string, string) (map[string]any, error) {
		calls.add("assign")
		return nil, nil
	}
	body, err := LocalModelsQuickstart(context.Background(), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if body["needs_runtime"] != false || body["needs_download"] != false || body["download_bytes"] != int64(0) {
		t.Fatalf("body %v", body)
	}
	job := lmWaitJob(t, body["job_id"].(string))
	if job["status"] != "done" {
		t.Fatalf("job %v", job["error"])
	}
	got := calls.list()
	if slices.Contains(got, any("install")) || slices.Contains(got, any("download")) {
		t.Fatalf("satisfied legs ran %v", got)
	}
	if !reflect.DeepEqual(got, []any{"assign"}) && (len(got) == 0 || got[len(got)-1] != "assign") {
		t.Fatalf("calls %v", got)
	}
}

// Preflight passes without hardware or network, every entry's first variant servable
func lmQuickstartReady(t *testing.T) {
	lmDeps.installedEngine = func(context.Context, string) (*Engine, error) {
		return &Engine{Backend: "cpu", Tag: "b10362", Binary: "unused"}, nil
	}
	catalogSelectVariantFn = func(entry CatalogEntry, budget HardwareBudget) *VariantChoice {
		return &VariantChoice{Variant: entry.Variants[0], ZeroSpill: true, ReasonKey: "best-fits"}
	}
	lmDeps.engineTooOld = func(context.Context, string) (bool, error) { return false, nil }
}

func TestQuickstartIsSingleFlight(t *testing.T) {
	lmTestHome(t, ".icebath")
	lmQuickstartReady(t)
	lmQuickstartMu.Lock()
	defer lmQuickstartMu.Unlock()
	_, err := LocalModelsQuickstart(context.Background(), "", "")
	if lmStatusOf(err) != 409 || !strings.Contains(strings.ToLower(err.(*HTTPException).Detail), "already running") {
		t.Fatalf("quickstart %v", err)
	}
}

func TestAssignDefaultReachesModelAssignment(t *testing.T) {
	lmTestHome(t, ".icebath")
	var seen [][]string
	lmDeps.applyAssignment = func(ctx context.Context, scope, provider, model, task, baseURL, apiKey string) (map[string]any, error) {
		seen = append(seen, []string{scope, provider, model, task, baseURL, apiKey})
		return nil, nil
	}
	if err := lmAssignDefault(context.Background(), map[string]any{}, "some-model"); err != nil {
		t.Fatal(err)
	}
	if want := [][]string{{"main", "llamacpp", "some-model", "", "", ""}}; !reflect.DeepEqual(seen, want) {
		t.Fatalf("seen %v", seen)
	}
}

func TestQuickstartPricesAfterInstallingTheEngine(t *testing.T) {
	lmTestHome(t, ".icebath")
	asRAM := lmBudget(50, 50, 128, true)
	asCard := lmBudget(22, 24, 128, false)
	guess, fit := Catalog()[0], Catalog()[1]
	var mu sync.Mutex
	var installed, downloaded, assigned []string
	lmDeps.probeBudget = func(bool) (HardwareBudget, error) {
		mu.Lock()
		defer mu.Unlock()
		if len(installed) > 0 {
			return asCard, nil
		}
		return asRAM, nil
	}
	lmDeps.recommendedEntry = func(budget HardwareBudget, entries []CatalogEntry, backend string) (*CatalogEntry, string) {
		if reflect.DeepEqual(budget, asRAM) {
			return &guess, "best-fits"
		}
		return &fit, "best-fits"
	}
	catalogSelectVariantFn = func(entry CatalogEntry, budget HardwareBudget) *VariantChoice {
		return &VariantChoice{Variant: entry.Variants[0], ZeroSpill: true, ReasonKey: "best-fits"}
	}
	lmDeps.engineTooOld = func(context.Context, string) (bool, error) { return false, nil }
	lmDeps.runtimeTarget = func(context.Context, string) (string, string, error) { return "b1", "vulkan", nil }
	lmDeps.installedEngine = func(context.Context, string) (*Engine, error) { return nil, nil }
	lmDeps.ensureEngine = func(ctx context.Context, backend string, opts EnsureEngineOptions) (Engine, error) {
		mu.Lock()
		installed = append(installed, backend)
		mu.Unlock()
		return Engine{}, nil
	}
	lmDeps.runDownloadPlan = func(ctx context.Context, job map[string]any, plan []lmPlanRow, label string, entry *CatalogEntry) error {
		downloaded = append(downloaded, label)
		return nil
	}
	lmDeps.ensureServer = func(context.Context, map[string]any, map[string]any, string, string, string) error { return nil }
	lmDeps.assignDefault = func(ctx context.Context, job map[string]any, modelID string) error {
		assigned = append(assigned, modelID)
		return nil
	}
	resp, err := LocalModelsQuickstart(context.Background(), "", "")
	if err != nil {
		t.Fatal(err)
	}
	job := lmWaitJob(t, resp["job_id"].(string))
	if job["status"] != "done" {
		t.Fatalf("job %v", job["error"])
	}
	if !reflect.DeepEqual(installed, []string{"vulkan"}) {
		t.Fatalf("installed %v", installed)
	}
	if job["model_id"] != fit.ID || !reflect.DeepEqual(downloaded, []string{fit.DisplayName}) ||
		!reflect.DeepEqual(assigned, []string{fit.Variants[0].ModelID()}) {
		t.Fatalf("model %v downloaded %v assigned %v", job["model_id"], downloaded, assigned)
	}
}
