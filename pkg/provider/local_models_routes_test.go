package provider

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/pm"
)

// The routes client fixture, a temp home serving the packaged catalog with no GitHub fetch
func lmRoutesClient(t *testing.T) {
	t.Helper()
	lmTestHome(t, ".icebath")
	lmDeps.refreshCatalogSoon = func() {}
}

func TestStatusShapeAndDefaults(t *testing.T) {
	lmRoutesClient(t)
	data, err := LocalModelsStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := data["enabled"].(bool); !ok {
		t.Fatalf("enabled %T", data["enabled"])
	}
	if tag, ok := data["tag"].(string); !ok || !strings.HasPrefix(tag, "b") {
		t.Fatalf("tag %v", data["tag"])
	}
	for _, key := range []string{"runtime_installed", "server_running"} {
		if _, ok := data[key].(bool); !ok {
			t.Fatalf("%s %T", key, data[key])
		}
	}
	if _, ok := data["models"].([]any); !ok {
		t.Fatalf("models %T", data["models"])
	}
}

func TestStatusRendersDegradedWhenConfigCannotBeRead(t *testing.T) {
	lmRoutesClient(t)
	configLoadReadonlyFn = func(context.Context) (map[string]any, error) {
		return nil, &os.PathError{Op: "open", Path: "profile home", Err: errors.New("profile home is gone")}
	}
	data, err := LocalModelsStatus(context.Background())
	if err != nil || data["enabled"] != false {
		t.Fatalf("status %v %v", data, err)
	}
}

// Status rows keyed by model id
func lmStatusRows(t *testing.T) map[string]map[string]any {
	t.Helper()
	data, err := LocalModelsStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	rows := map[string]map[string]any{}
	for _, raw := range data["models"].([]any) {
		row := raw.(map[string]any)
		rows[row["id"].(string)] = row
	}
	return rows
}

func TestStatusListsStagedModelsWithLabels(t *testing.T) {
	lmRoutesClient(t)
	lmWriteFakeGGUF(t, filepath.Join(localModelsDir(), "Some-Model.gguf"), 2048)
	row, ok := lmStatusRows(t)["Some-Model"]
	if !ok {
		t.Fatal("staged model missing")
	}
	if row["size_bytes"].(int64) <= 0 || !strings.HasSuffix(row["size_label"].(string), "GB") {
		t.Fatalf("row %v", row)
	}
}

func TestStatusSizesASplitModelByAllItsParts(t *testing.T) {
	lmRoutesClient(t)
	lmWriteFakeGGUF(t, filepath.Join(localModelsDir(), "Side-Split-00001-of-00002.gguf"), 1024)
	lmWriteFakeGGUF(t, filepath.Join(localModelsDir(), "Side-Split-00002-of-00002.gguf"), 8192)
	if got := lmStatusRows(t)["Side-Split"]["size_bytes"]; got != int64((4+1024)+(4+8192)) {
		t.Fatalf("size %v", got)
	}
}

func TestStatusTracksPresetSpillAndRestoredWindow(t *testing.T) {
	lmRoutesClient(t)
	layers := make([]ProfileLayer, 32)
	for i := range layers {
		layers[i] = ProfileLayer{Kind: LayerFull, KVBytesPerTok: 4096}
	}
	profile := NewModelProfile("status-mtp", 16<<30, 0, 262144, layers)
	profile.NVocab = 151936
	modelID := profile.Name
	lmWriteFakeGGUF(t, filepath.Join(localModelsDir(), modelID+".gguf"), 1024)
	lr6FakeHeaders(t, func(string) ModelProfile { return profile })
	lmDeps.stateEndpoint = func() (map[string]any, error) { return map[string]any{"base_url": "http://127.0.0.1:1/v1"}, nil }
	var mu sync.Mutex
	serverWindow := ContextFloor
	lmDeps.routerRequest = func(endpoint map[string]any, route string, timeout time.Duration, payload []byte) (any, error) {
		if route == "/models" {
			return map[string]any{"data": []any{map[string]any{"id": modelID, "status": map[string]any{"value": "loaded"}}}}, nil
		}
		if route != fmt.Sprintf("/props?model=%s&autoload=false", modelID) {
			t.Errorf("route %q", route)
		}
		mu.Lock()
		defer mu.Unlock()
		return map[string]any{"default_generation_settings": map[string]any{"n_ctx": serverWindow}}, nil
	}
	scaled := profile
	scaled.KVScale = 1.2
	floorNeed := profile.WeightsBytes + CtxBytes(scaled, ContextFloor, true)
	lean := RuntimeOverheadBytes + UbLogitsBytes(profile.NVocab, true, false)
	stacked := RuntimeOverheadBytes + UbLogitsBytes(profile.NVocab, true, true)
	ini := filepath.Join(RuntimesRoot(), "presets.ini")
	grown := 73728
	for _, c := range []struct {
		device   int64
		override int
		spilled  bool
	}{{floorNeed + lean - 1, ContextFloor, true}, {floorNeed + stacked, grown, false}} {
		if err := SaveWindowOverride(modelID, c.override); err != nil {
			t.Fatal(err)
		}
		entries, err := GeneratePresets(localModelsDir(), NewHardwareBudget(c.device, c.device, 8<<30), ini, map[string]bool{modelID: true}, nil, 1, "")
		if err != nil {
			t.Fatal(err)
		}
		preset := entries[0]
		if preset.Window != c.override || preset.Spilled != c.spilled {
			t.Fatalf("preset %+v", preset)
		}
		if preset.Keys["spec-type"] != "draft-mtp" {
			t.Fatalf("spec type %v", preset.Keys["spec-type"])
		}
		_, ubatch := preset.Keys["ubatch-size"]
		_, override := preset.Keys["override-tensor"]
		if ubatch || override {
			t.Fatalf("keys %v", preset.Keys)
		}
		mu.Lock()
		serverWindow = preset.Window - 1024
		granted := serverWindow
		mu.Unlock()
		data, err := LocalModelsStatus(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if data["loaded_models"].(map[string]any)[modelID] != "loaded" {
			t.Fatalf("loaded %v", data["loaded_models"])
		}
		placement := data["placement"].(map[string]any)[modelID].(map[string]any)
		if placement["spilled"] != c.spilled || placement["window"] != preset.Window ||
			placement["granted_window"] != int64(granted) || placement["granted_window_label"] != lmKLabel(int64(granted)) {
			t.Fatalf("placement %v", placement)
		}
	}
}

func TestHardwarePlainFacts(t *testing.T) {
	lmRoutesClient(t)
	data, err := LocalModelsHardware(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := data["uma"].(bool); !ok {
		t.Fatalf("uma %T", data["uma"])
	}
	if data["ram_total_bytes"].(int64) <= 0 || data["vram_total_bytes"].(int64) < 0 {
		t.Fatalf("data %v", data)
	}
	for _, key := range []string{"gpu_name", "gpu_util_percent", "vram_used_bytes"} {
		if _, ok := data[key]; !ok {
			t.Fatalf("%s missing", key)
		}
	}
}

// Catalog rows of the catalog route
func lmCatalogRows(t *testing.T) []map[string]any {
	t.Helper()
	data, err := LocalModelsCatalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	for _, raw := range data["models"].([]any) {
		rows = append(rows, raw.(map[string]any))
	}
	return rows
}

func TestCatalogPricesEveryEntryForThisMachine(t *testing.T) {
	lmRoutesClient(t)
	rows := lmCatalogRows(t)
	if len(rows) < 3 {
		t.Fatalf("%d rows", len(rows))
	}
	for _, row := range rows {
		if !strings.HasSuffix(row["size_label"].(string), "GB") || !pyTruthy(row["fit_summary"]) {
			t.Fatalf("row %v", row)
		}
		fits, ok := row["fits"].(bool)
		if !ok {
			t.Fatalf("fits %T", row["fits"])
		}
		if fits {
			if row["start_window"].(int) < 1 || !strings.HasSuffix(row["start_window_label"].(string), "K") {
				t.Fatalf("row %v", row)
			}
		} else if !strings.Contains(strings.ToLower(row["fit_summary"].(string)), "memory") {
			t.Fatalf("row %v", row)
		}
		if _, ok := row["downloaded"].(bool); !ok {
			t.Fatalf("downloaded %T", row["downloaded"])
		}
	}
}

func TestCalibratedCatalogAndQuickstartRespectBackend(t *testing.T) {
	for _, c := range []struct{ backend, expected string }{
		{"auto", "qwen3.8-27b"}, {"cuda", "qwen3.8-27b"}, {"vulkan", "qwen3.6-35b-a3b"}, {"cpu", "qwen3.6-35b-a3b"},
	} {
		t.Run(c.backend+"-"+c.expected, func(t *testing.T) {
			lmRoutesClient(t)
			gib48 := float64(48 << 30)
			budget := NewHardwareBudget(int64(gib48*.8), 48<<30, 0)
			budget.UMA, budget.GPUName, budget.Platform = true, "NVIDIA RTX Spark N1X (5120-core Blackwell RTX GPU)", "win32"
			lmDeps.probeBudget = func(bool) (HardwareBudget, error) { return budget, nil }
			lmDeps.stagedModelIDs = func() []string { return nil }
			lmDeps.installedEngine = func(context.Context, string) (*Engine, error) {
				return &Engine{Backend: "cuda", Tag: "b10964", Binary: "unused"}, nil
			}
			lmDeps.loadConfig = func(context.Context) map[string]any {
				return map[string]any{"local_runtime": map[string]any{"backend": c.backend}}
			}
			savedQuery, savedClass := bootNvidiaQuery, bootGPUClass
			t.Cleanup(func() { bootNvidiaQuery, bootGPUClass = savedQuery, savedClass })
			bootNvidiaQuery = func() (*NvidiaGPUQuery, error) {
				t.Error("recommendation must not launch a model or run another hardware probe")
				return nil, nil
			}
			bootGPUClass = func() string {
				t.Error("recommendation must not launch a model or run another hardware probe")
				return ""
			}
			lmDeps.ensureLocalRuntime = func(context.Context, map[string]any, bool) (localModelsSupervisor, error) {
				t.Error("recommendation must not launch a model or run another hardware probe")
				return nil, nil
			}
			var chosen []string
			for _, row := range lmCatalogRows(t) {
				if row["recommended"] == true {
					chosen = append(chosen, row["id"].(string))
				}
			}
			if !reflect.DeepEqual(chosen, []string{c.expected}) {
				t.Fatalf("chosen %v", chosen)
			}
			entry, _, err := lmQuickstartTarget(context.Background(), "", budget)
			if err != nil || entry.ID != c.expected {
				t.Fatalf("automatic %v %v", entry.ID, err)
			}
			entry, _, err = lmQuickstartTarget(context.Background(), "qwen3.8-27b", budget)
			if err != nil || entry.ID != "qwen3.8-27b" {
				t.Fatalf("explicit %v %v", entry.ID, err)
			}
		})
	}
}

func TestQuickstartFallsBackPastEntriesThatAreNeverRecommended(t *testing.T) {
	lmTestHome(t, ".icebath")
	budget := NewHardwareBudget(44<<30, 48<<30, 64<<30)
	bolt := CatalogByID()["nemotron-3.5-bolt-74b-a9b"]
	the27b := CatalogByID()["qwen3.8-27b"]
	realSelect := catalogSelectVariantFn
	lmDeps.engineTooOld = func(context.Context, string) (bool, error) { return false, nil }
	lmDeps.recommendedEntry = func(HardwareBudget, []CatalogEntry, string) (*CatalogEntry, string) {
		return &the27b, "best-quality-resident"
	}
	catalogSelectVariantFn = func(entry CatalogEntry, b HardwareBudget) *VariantChoice {
		if entry.ID == bolt.ID {
			return realSelect(entry, b)
		}
		return nil
	}
	if realSelect(bolt, budget) == nil {
		t.Fatal("bolt must fit")
	}
	if _, _, err := lmQuickstartTarget(context.Background(), "", budget); lmStatusOf(err) != 409 {
		t.Fatalf("automatic %v", err)
	}
	entry, _, err := lmQuickstartTarget(context.Background(), bolt.ID, budget)
	if err != nil || entry.ID != bolt.ID {
		t.Fatalf("explicit %v %v", entry.ID, err)
	}
}

func TestCatalogNeverHidesUnaffordableModels(t *testing.T) {
	lmRoutesClient(t)
	tiny := NewHardwareBudget(1<<30, 1<<30, 1<<30)
	lmDeps.probeBudget = func(bool) (HardwareBudget, error) { return tiny, nil }
	rows := lmCatalogRows(t)
	if len(rows) != len(Catalog()) {
		t.Fatalf("%d rows of %d", len(rows), len(Catalog()))
	}
	var refused []map[string]any
	for _, row := range rows {
		if row["fits"] != true {
			refused = append(refused, row)
		}
	}
	if len(refused) == 0 {
		t.Fatal("a 1 GiB machine must refuse the 20 GB models")
	}
	for _, row := range refused {
		if !pyTruthy(row["fit_detail"]) && !pyTruthy(row["fit_summary"]) {
			t.Fatalf("row %v", row)
		}
	}
}

// Generous GPU budget so the download path, not variant selection, is what a test exercises
func lmPinBudget(t *testing.T) HardwareBudget {
	budget := NewHardwareBudget(64<<30, 64<<30, 64<<30)
	lmDeps.probeBudget = func(bool) (HardwareBudget, error) { return budget, nil }
	return budget
}

// Whether the quickstart single flight lock is held
func lmQuickstartLocked() bool {
	if lmQuickstartMu.TryLock() {
		lmQuickstartMu.Unlock()
		return false
	}
	return true
}

// Running entry of a job, nil once its resources were released
func lmRunningOf(jobID string) *lmRunningEntry {
	lmJobsMu.Lock()
	defer lmJobsMu.Unlock()
	return lmRunning[jobID]
}

// Files of a directory matching a glob
func lmGlob(t *testing.T, dir, pattern string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, pattern))
	if err != nil {
		t.Fatal(err)
	}
	return matches
}

func TestRuntimeInstallRejectsImpossibleCombo(t *testing.T) {
	lmRoutesClient(t)
	lockPath := filepath.Join(filepath.Dir(pm.PartialsRoot()), "unavailable-lock.json")
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := pm.NewLockfile(lockPath).Save(); err != nil {
		t.Fatal(err)
	}
	prev := pm.SwapLockfilePath(func() string { return lockPath })
	t.Cleanup(func() { pm.SwapLockfilePath(prev) })
	_, err := LocalModelsRuntimeInstall(context.Background(), "cpu")
	if lmStatusOf(err) != 400 || !strings.Contains(err.(*HTTPException).Detail, "not pinned") {
		t.Fatalf("install %v", err)
	}
}

// Points the download plan at the slow loopback range server, one served body per plan file
func lmServePlan(t *testing.T, bodies []struct {
	name string
	body []byte
}) (*lmRangeServer, []lmPlanRow) {
	srv := newLMRangeServer(t)
	srv.slow(200*time.Millisecond, 128*1024)
	var plan []lmPlanRow
	for _, b := range bodies {
		srv.set("/"+b.name, b.body)
		plan = append(plan, lmPlanRow{srv.url("/" + b.name), filepath.Join(localModelsDir(), b.name+".gguf"), int64(len(b.body))})
	}
	lmDeps.downloadPlan = func(CatalogEntry, QuantVariant) []lmPlanRow { return plan }
	lmDeps.refreshLocalRuntime = func(context.Context) (bool, error) { return false, nil }
	return srv, plan
}

// Pauses once bytes are moving so the pause lands mid flight
func lmPauseWhenFlowing(t *testing.T, jobID string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		view, err := LocalModelsJobView(jobID)
		if err != nil {
			t.Fatal(err)
		}
		if done, _ := view["done_bytes"].(int64); done > 0 {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	deadline = time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if LocalModelsPauseJob(jobID) {
			return
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatal("pause never reached the live download handle")
}

// Eight MiB of every byte value repeated
func lmBigBody() []byte { return bytes.Repeat(lmBytes256(), 8*1024*1024/256) }

func TestQuickstartPauseStopsTheSequence(t *testing.T) {
	lmRoutesClient(t)
	lmPinBudget(t)
	big := lmBigBody()
	srv, _ := lmServePlan(t, []struct {
		name string
		body []byte
	}{{"QsPartA", big}, {"QsPartB", big}})
	lmDeps.installedEngine = func(context.Context, string) (*Engine, error) {
		return &Engine{Backend: "cpu", Tag: "b99999", Binary: "unused"}, nil
	}
	lmDeps.runtimeTarget = func(context.Context, string) (string, string, error) { return "b1", "cpu", nil }
	var mu sync.Mutex
	calls := map[string]int{"server": 0, "assign": 0}
	count := func(key string) {
		mu.Lock()
		calls[key]++
		mu.Unlock()
	}
	snapshot := func() map[string]int {
		mu.Lock()
		defer mu.Unlock()
		return map[string]int{"server": calls["server"], "assign": calls["assign"]}
	}
	lmDeps.ensureLocalRuntime = func(context.Context, map[string]any, bool) (localModelsSupervisor, error) {
		count("server")
		return nil, errors.New("server must not start after a pause")
	}
	lmDeps.applyAssignment = func(context.Context, string, string, string, string) (map[string]any, error) {
		count("assign")
		return nil, nil
	}
	entryID := Catalog()[0].ID
	resp, err := LocalModelsQuickstart(context.Background(), entryID)
	if err != nil {
		t.Fatal(err)
	}
	jobID := resp["job_id"].(string)
	lmPauseWhenFlowing(t, jobID)
	if status := lmPoll(t, jobID, 15*time.Second, "paused", "done", "error"); status["status"] != "paused" {
		t.Fatalf("status %v", status)
	}
	if got := snapshot(); !reflect.DeepEqual(got, map[string]int{"server": 0, "assign": 0}) {
		t.Fatalf("calls %v", got)
	}
	if lrPathExists(filepath.Join(localModelsDir(), "QsPartB.gguf")) {
		t.Fatal("second plan file touched after the pause")
	}
	if lmRunningOf(jobID) != nil {
		t.Fatal("paused quickstart kept its handles")
	}
	if lmQuickstartLocked() {
		t.Fatal("paused quickstart kept the setup lock")
	}
	lmDeps.ensureServer = func(context.Context, map[string]any, map[string]any, string, string, string) error {
		count("server")
		return nil
	}
	lmDeps.assignDefault = func(context.Context, map[string]any, string) error {
		count("assign")
		return nil
	}
	srv.slow(0, 128*1024)
	again, err := LocalModelsQuickstart(context.Background(), entryID)
	if err != nil {
		t.Fatalf("second quickstart %v", err)
	}
	againID := again["job_id"].(string)
	if status := lmPoll(t, againID, 15*time.Second, "done", "error"); status["status"] != "done" {
		t.Fatalf("status %v", status)
	}
	if got := snapshot(); !reflect.DeepEqual(got, map[string]int{"server": 1, "assign": 1}) {
		t.Fatalf("calls %v", got)
	}
	for _, name := range []string{"QsPartA.gguf", "QsPartB.gguf"} {
		if got, _ := os.ReadFile(filepath.Join(localModelsDir(), name)); !bytes.Equal(got, big) {
			t.Fatalf("%s %d bytes", name, len(got))
		}
	}
	if lmRunningOf(againID) != nil {
		t.Fatal("finished quickstart kept its handles")
	}
	if lmQuickstartLocked() {
		t.Fatal("finished quickstart kept the setup lock")
	}
}
