package provider

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/pm"
)

// Catalog of exactly entries with each one's first variant staged
func lmStaged(t *testing.T, entries ...CatalogEntry) {
	t.Helper()
	setCatalog(entries)
	var ids []string
	for _, e := range entries {
		ids = append(ids, e.Variants[0].ModelID())
	}
	lmDeps.stagedModelIDs = func() []string { return ids }
}

// Touches a file, creating its directory
func lmTouch(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
}

func TestInstalledModelsMissingTheirDraftGetItFromTheEngineUpdate(t *testing.T) {
	lmTestHome(t, ".icebath")
	entry := lr6HybridEntry()
	unstaged := lr6HybridEntry()
	unstaged.ID = "other"
	other := lr6Build
	other.Files = []AssetFile{{Path: "Other.gguf", SizeBytes: lr6GiB, Revision: "main"}}
	unstaged.Variants = []QuantVariant{other}
	newer := lr6HybridEntry()
	newer.ID = "newer"
	newerBuild := lr6Build
	newerBuild.Files = []AssetFile{{Path: "Newer.gguf", SizeBytes: lr6GiB, Revision: "main"}}
	newer.Variants = []QuantVariant{newerBuild}
	newer.Draft, newer.DraftMinEngine = &AssetFile{Path: "Newer-DFlash.gguf", SizeBytes: lr6GiB, Revision: "main"}, "b99999"
	lmStaged(t, entry, newer)
	setCatalog([]CatalogEntry{entry, newer, unstaged})
	dest := filepath.Join(localAssetsDir(), entry.Draft.LocalName())
	plan, err := lmMissingDrafts("b11671")
	if err != nil {
		t.Fatal(err)
	}
	want := []lmPlanRow{{"https://huggingface.co/lab/Hybrid-DFlash-GGUF/resolve/0123abc/Hybrid-DFlash.gguf", dest, lr6GiB}}
	if !reflect.DeepEqual(plan, want) {
		t.Fatalf("plan %v", plan)
	}
	lmTouch(t, dest)
	if plan, err := lmMissingDrafts("b11671"); err != nil || len(plan) != 0 {
		t.Fatalf("plan %v %v", plan, err)
	}
}

// Supervisor stand in that only has to exist
type lmPresentSupervisor struct{}

func (lmPresentSupervisor) Models(time.Duration) (RouterModels, error) { return RouterModels{}, nil }

func (lmPresentSupervisor) UnloadModel(string) error { return nil }

// Restart log of an engine update over a running server, the restarts recorded in order
type lmRestarts struct{ calls *lmCalls }

func (r lmRestarts) list() []any { return r.calls.list() }

// The engine update over a running server on serving, with b11671 already installed
func lmUpdateJob(t *testing.T, serving string, onDownload func(job map[string]any, plan []lmPlanRow) error) lmRestarts {
	t.Helper()
	lmStaged(t, lr6HybridEntry())
	engine := Engine{Backend: "cpu", Tag: "b11671"}
	lmDeps.runtimeTarget = func(context.Context, string) (string, string, error) { return "b11671", "cpu", nil }
	lmDeps.installedEngine = func(context.Context, string) (*Engine, error) { e := engine; return &e, nil }
	lmDeps.installEngineJob = func(context.Context, map[string]any, string) (Engine, error) { return engine, nil }
	if onDownload == nil {
		onDownload = func(map[string]any, []lmPlanRow) error { return nil }
	}
	lmDeps.downloadJob = func(ctx context.Context, job map[string]any, plan []lmPlanRow) error { return onDownload(job, plan) }
	lmDeps.getSupervisor = func() localModelsSupervisor { return lmPresentSupervisor{} }
	lmDeps.servingEngine = func() *Engine { return &Engine{Backend: "cpu", Tag: serving} }
	calls := &lmCalls{}
	lmDeps.refreshLocalRuntime = func(context.Context) (bool, error) { calls.add("restart"); return false, nil }
	return lmRestarts{calls}
}

// Starts the runtime install job for the cpu backend
func lmStartRuntimeInstall(t *testing.T) string {
	t.Helper()
	resp, err := LocalModelsRuntimeInstall(context.Background(), "cpu", "")
	if err != nil {
		t.Fatal(err)
	}
	return resp["job_id"].(string)
}

func TestTheUpdateJobDownloadsMissingDraftsThenRestartsTheServer(t *testing.T) {
	lmDownloadJobsClient(t)
	var mu sync.Mutex
	var fetched []lmPlanRow
	events := lmUpdateJob(t, "", func(job map[string]any, plan []lmPlanRow) error {
		mu.Lock()
		fetched = append(fetched, plan...)
		mu.Unlock()
		return nil
	})
	lmDeps.servingEngine = servingEngine
	jobID := lmStartRuntimeInstall(t)
	if job := lmPoll(t, jobID, 15*time.Second, "done", "error"); job["status"] != "done" {
		t.Fatalf("job %v", job)
	}
	var names []string
	mu.Lock()
	for _, row := range fetched {
		names = append(names, filepath.Base(row.Dest))
	}
	mu.Unlock()
	if !reflect.DeepEqual(names, []string{lr6HybridEntry().Draft.LocalName()}) {
		t.Fatalf("fetched %v", names)
	}
	if !reflect.DeepEqual(events.list(), []any{"restart"}) {
		t.Fatalf("events %v", events.list())
	}
}

func TestADraftThatFailsToDownloadLeavesTheModelDownloaded(t *testing.T) {
	lmDownloadJobsClient(t)
	entry := lr6HybridEntry()
	lmStaged(t, entry)
	dir := t.TempDir()
	models, assets := filepath.Join(dir, "models"), filepath.Join(dir, "assets")
	lmDeps.modelsDir = func() string { return models }
	lmDeps.assetsDir = func() string { return assets }
	lmDeps.refreshRuntime = func(context.Context, func(context.Context) (bool, error), string) {}
	lmDeps.downloadJob = func(ctx context.Context, job map[string]any, plan []lmPlanRow) error {
		for _, row := range plan {
			if filepath.Base(row.Dest) == entry.Draft.LocalName() {
				return &pm.DownloadError{Kind: pm.DownloadTransportFailed,
					Message: "download failed from https://huggingface.co/lab/Hybrid-DFlash-GGUF: HTTP Error 404"}
			}
		}
		for _, row := range plan {
			lmTouch(t, row.Dest)
		}
		return nil
	}
	resp, err := LocalModelsDownload(context.Background(), lr6Build.ModelID())
	if err != nil {
		t.Fatal(err)
	}
	if job := lmPoll(t, resp["job_id"].(string), 15*time.Second, "done", "error"); job["status"] != "done" {
		t.Fatalf("job %v", job)
	}
	if !lrIsFile(filepath.Join(models, lr6Build.Files[0].LocalName())) {
		t.Fatal("model not downloaded")
	}
	if lrPathExists(filepath.Join(assets, entry.Draft.LocalName())) {
		t.Fatal("failed draft left a file")
	}
}

func TestAPauseBetweenTheDraftsAndTheRestartStillRestartsOnResume(t *testing.T) {
	lmDownloadJobsClient(t)
	events := lmUpdateJob(t, "b11671", func(job map[string]any, plan []lmPlanRow) error {
		for _, row := range plan {
			lmTouch(t, row.Dest)
		}
		lmJobsMu.Lock()
		lmRunning[lmJobID(job)].pause.Set()
		lmJobsMu.Unlock()
		return nil
	})
	jobID := lmStartRuntimeInstall(t)
	if job := lmPoll(t, jobID, 15*time.Second, "paused", "done", "error"); job["status"] != "paused" {
		t.Fatalf("job %v", job)
	}
	if got := events.list(); len(got) != 0 {
		t.Fatalf("events %v", got)
	}
	resumed, err := LocalModelsDownloadResume(context.Background(), jobID)
	if err != nil || resumed["resumed"] != true {
		t.Fatalf("resume %v %v", resumed, err)
	}
	if job := lmPoll(t, jobID, 15*time.Second, "done", "error"); job["status"] != "done" {
		t.Fatalf("job %v", job)
	}
	if !reflect.DeepEqual(events.list(), []any{"restart"}) {
		t.Fatalf("events %v", events.list())
	}
}

// Draft download double failing the way an HTTP 404 transfer does
func lmFailingDownload(map[string]any, []lmPlanRow) error {
	return &pm.DownloadError{Kind: pm.DownloadTransportFailed,
		Message: "download failed from https://huggingface.co/lab/Hybrid-DFlash-GGUF: HTTP Error 404"}
}

func TestAFailedDraftDownloadStillSwitchesTheServerToTheNewBuild(t *testing.T) {
	lmDownloadJobsClient(t)
	events := lmUpdateJob(t, "b11370", lmFailingDownload)
	jobID := lmStartRuntimeInstall(t)
	if job := lmPoll(t, jobID, 15*time.Second, "done", "error"); job["status"] != "done" {
		t.Fatalf("job %v", job)
	}
	if !reflect.DeepEqual(events.list(), []any{"restart"}) {
		t.Fatalf("events %v", events.list())
	}
}

func TestAFailedDraftDownloadAloneLeavesTheServerRunning(t *testing.T) {
	lmDownloadJobsClient(t)
	events := lmUpdateJob(t, "b11671", lmFailingDownload)
	jobID := lmStartRuntimeInstall(t)
	if job := lmPoll(t, jobID, 15*time.Second, "done", "error"); job["status"] != "done" {
		t.Fatalf("job %v", job)
	}
	if got := events.list(); len(got) != 0 {
		t.Fatalf("events %v", got)
	}
}

func TestAPauseDuringTheDraftDownloadPausesTheUpdate(t *testing.T) {
	lmDownloadJobsClient(t)
	events := lmUpdateJob(t, "b11370", func(map[string]any, []lmPlanRow) error { return lmDownloadPaused("download paused") })
	jobID := lmStartRuntimeInstall(t)
	if job := lmPoll(t, jobID, 15*time.Second, "paused", "done", "error"); job["status"] != "paused" {
		t.Fatalf("job %v", job)
	}
	if got := events.list(); len(got) != 0 {
		t.Fatalf("events %v", got)
	}
}

func TestTheUpdateRestartsAServerStillOnAnOlderBuild(t *testing.T) {
	lmDownloadJobsClient(t)
	lmTouch(t, filepath.Join(localAssetsDir(), lr6HybridEntry().Draft.LocalName()))
	events := lmUpdateJob(t, "b11370", nil)
	jobID := lmStartRuntimeInstall(t)
	if job := lmPoll(t, jobID, 15*time.Second, "done", "error"); job["status"] != "done" {
		t.Fatalf("job %v", job)
	}
	if !reflect.DeepEqual(events.list(), []any{"restart"}) {
		t.Fatalf("events %v", events.list())
	}
}

func TestAnInstalledModelWithoutItsDraftStillReadsAsDownloaded(t *testing.T) {
	lmTestHome(t, ".icebath")
	entry := lr6HybridEntry()
	lmStaged(t, entry)
	lmTouch(t, filepath.Join(localModelsDir(), lr6Build.Files[0].LocalName()))
	row, err := lmCatalogRow(context.Background(), entry, lr6Roomy(), nil, nil, map[string]bool{lr6Build.ModelID(): true})
	if err != nil {
		t.Fatal(err)
	}
	if row["downloaded"] != true || row["size_bytes"] != entry.DownloadBytes(lr6Build) {
		t.Fatalf("row %v", row)
	}
}
