package provider

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/pm"
)

// The download jobs client fixture, temp home, fresh registry and a refresh that bounces nothing
func lmDownloadJobsClient(t *testing.T) string {
	t.Helper()
	h := lmTestHome(t, "home")
	lmDeps.refreshLocalRuntime = func(context.Context) (bool, error) { return false, nil }
	return h
}

func TestRateAndEtaReportOnlyAnHonestNumber(t *testing.T) {
	mib := int64(1 << 20)
	check := func(samples []lmRateSample, done, total int64, wantRate *float64, wantETA *int64) {
		t.Helper()
		rate, eta := lmRateAndETA(samples, done, total)
		if (rate == nil) != (wantRate == nil) || (rate != nil && *rate != *wantRate) {
			t.Fatalf("rate %v want %v", rate, wantRate)
		}
		if (eta == nil) != (wantETA == nil) || (eta != nil && *eta != *wantETA) {
			t.Fatalf("eta %v want %v", eta, wantETA)
		}
	}
	rate, two := float64(mib), int64(2)
	check([]lmRateSample{{0, 0}, {1, mib}}, mib, 3*mib, &rate, &two)
	check([]lmRateSample{{0, 0}}, 0, 100, nil, nil)
	check([]lmRateSample{{0, 0}, {0.1, 50}}, 50, 100, nil, nil)
	check([]lmRateSample{{0, 40}, {1, 40}}, 40, 100, nil, nil)
	check([]lmRateSample{{0, 0}, {1, mib}}, mib, 0, &rate, nil)
}

// Download double that blocks its first mid transfer tick until released
type lmObservedDownload struct {
	inner    lmDownloader
	flowing  chan struct{}
	release  chan struct{}
	flowOnce *sync.Once
	t        *testing.T
}

func (d *lmObservedDownload) Run(progress pm.ProgressFunc) ([]string, error) {
	return d.inner.Run(func(done, total int64, ranges []pm.RangeSet) error {
		if progress != nil {
			if err := progress(done, total, ranges); err != nil {
				return err
			}
		}
		if 0 < done && done < total {
			first := false
			d.flowOnce.Do(func() { first = true })
			if first {
				close(d.flowing)
				select {
				case <-d.release:
				case <-time.After(15 * time.Second):
					d.t.Errorf("release never came")
				}
			}
		}
		return nil
	})
}

func TestBrowsedDownloadCanPauseAndResumePmTransfer(t *testing.T) {
	lmDownloadJobsClient(t)
	srv := newLMRangeServer(t)
	payload := bytes.Repeat(lmBytes256(), 32*1024)
	srv.set("/test.gguf", payload)
	lmDeps.hfURL = func(repo, path, revision string) string { return srv.url("/test.gguf") }
	flowing, release, once := make(chan struct{}), make(chan struct{}), &sync.Once{}
	real := lmDeps.newDownload
	lmDeps.newDownload = func(sources []pm.Source, pause *pm.PauseEvent) lmDownloader {
		return &lmObservedDownload{inner: real(sources, pause), flowing: flowing, release: release, flowOnce: once, t: t}
	}
	ctx := context.Background()
	resp, err := LocalModelsDownloadBrowsed(ctx, "a/b", []string{"test.gguf"})
	if err != nil {
		t.Fatal(err)
	}
	jobID := resp["job_id"].(string)
	func() {
		defer close(release)
		select {
		case <-flowing:
		case <-time.After(5 * time.Second):
			t.Fatal("browsed download bypassed PM's downloader")
		}
		paused, err := LocalModelsDownloadPause(ctx, jobID)
		if err != nil || paused["paused"] != true {
			t.Fatalf("pause %v %v", paused, err)
		}
	}()
	job := lmPoll(t, jobID, 15*time.Second, "done", "error", "paused")
	if job["status"] != "paused" {
		t.Fatalf("job %v", job)
	}
	if job["can_resume"] != true || job["can_pause"] != false {
		t.Fatalf("resume and pause facts %v", job)
	}
	if job["error"] != nil {
		t.Fatalf("error %v", job["error"])
	}
	if _, ok := job["bytes_per_sec"]; ok {
		t.Fatalf("parked transfer reports a rate %v", job)
	}
	if _, ok := job["eta_seconds"]; ok {
		t.Fatalf("parked transfer reports an eta %v", job)
	}
	if before := job["done_bytes"].(int64); before <= 0 {
		t.Fatalf("done bytes %d", before)
	}
	resumed, err := LocalModelsDownloadResume(ctx, jobID)
	if err != nil || resumed["resumed"] != true {
		t.Fatalf("resume %v %v", resumed, err)
	}
	job = lmPoll(t, jobID, 15*time.Second, "done", "error")
	if job["status"] != "done" {
		t.Fatalf("job %v", job)
	}
	if job["done_bytes"] != int64(len(payload)) || job["total_bytes"] != int64(len(payload)) {
		t.Fatalf("bytes %v of %v", job["done_bytes"], job["total_bytes"])
	}
	got, err := os.ReadFile(filepath.Join(localModelsDir(), "test.gguf"))
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("staged file %d bytes %v", len(got), err)
	}
	if job["can_pause"] != false || job["can_resume"] != false {
		t.Fatalf("settled job facts %v", job)
	}
}

// Every byte value in order, the Python bytes of range 256
func lmBytes256() []byte {
	out := make([]byte, 256)
	for i := range out {
		out[i] = byte(i)
	}
	return out
}

func TestQuickstartPlanProgressUsesActualWholePlanBytes(t *testing.T) {
	lmDownloadJobsClient(t)
	srv := newLMRangeServer(t)
	dir := t.TempDir()
	bodies := []struct{ name, body string }{{"first", "first"}, {"second", "second"}}
	var plan []lmPlanRow
	for _, b := range bodies {
		srv.set("/"+b.name, []byte(b.body))
		plan = append(plan, lmPlanRow{srv.url("/" + b.name), filepath.Join(dir, b.name+".gguf"), 1000})
	}
	job := lmNewJob("quickstart", "test", nil)
	if err := lmRunDownloadPlan(context.Background(), job, plan, "test", nil); err != nil {
		t.Fatal(err)
	}
	expected := int64(len("first") + len("second"))
	if job["done_bytes"] != expected || job["total_bytes"] != expected {
		t.Fatalf("bytes %v of %v", job["done_bytes"], job["total_bytes"])
	}
	if got := lmRangesTotal(t, job["ranges"]); got != expected {
		t.Fatalf("ranges cover %d", got)
	}
	lmJobsMu.Lock()
	job["status"] = "done"
	lmJobsMu.Unlock()
}

func TestExistingWeightsDoNotSkipMissingCompanionDownload(t *testing.T) {
	lmDownloadJobsClient(t)
	srv := newLMRangeServer(t)
	entry := Catalog()[0]
	entry.MMProj, entry.Draft = &AssetFile{Path: "projector.gguf", SizeBytes: 1000, Revision: "main"}, nil
	variant := entry.Variants[0]
	lmDeps.downloadTarget = func(context.Context, string) (CatalogEntry, QuantVariant, error) { return entry, variant, nil }
	lmDeps.stagedModelIDs = func() []string { return []string{variant.ModelID()} }
	dir := t.TempDir()
	weights := filepath.Join(dir, "weights.gguf")
	if err := os.WriteFile(weights, []byte("already downloaded"), 0o644); err != nil {
		t.Fatal(err)
	}
	companion := filepath.Join(dir, "assets", "projector.gguf")
	srv.set("/projector", []byte("projector bytes"))
	plan := []lmPlanRow{{srv.url("/weights"), weights, 1000}, {srv.url("/projector"), companion, 1000}}
	lmDeps.downloadPlan = func(CatalogEntry, QuantVariant) []lmPlanRow { return plan }
	resp, err := LocalModelsDownload(context.Background(), entry.ID)
	if err != nil {
		t.Fatal(err)
	}
	jobID, _ := resp["job_id"].(string)
	if jobID == "" {
		t.Fatalf("weights-only staging incorrectly skipped the projector: %v", resp)
	}
	job := lmPoll(t, jobID, 15*time.Second, "done", "error", "paused")
	if job["status"] != "done" {
		t.Fatalf("job %v", job)
	}
	if got, _ := os.ReadFile(companion); string(got) != "projector bytes" {
		t.Fatalf("companion %q", got)
	}
	ws, _ := os.Stat(weights)
	cs, _ := os.Stat(companion)
	if job["done_bytes"] != ws.Size()+cs.Size() {
		t.Fatalf("done bytes %v", job["done_bytes"])
	}
}

func TestJobsKeepEveryActiveDownloadAheadOfBoundedHistory(t *testing.T) {
	lmDownloadJobsClient(t)
	paused := lmNewJob("runtime-install", "engine", nil)
	lmJobsMu.Lock()
	paused["status"] = "paused"
	lmJobsMu.Unlock()
	for i := range 25 {
		job := lmNewJob("model-download", fmt.Sprintf("model-%d", i), nil)
		lmJobsMu.Lock()
		job["status"] = "done"
		lmJobsMu.Unlock()
	}
	var active []map[string]any
	for i := range 25 {
		active = append(active, lmNewJob("model-download", fmt.Sprintf("active-%d", i), nil))
	}
	resp, err := LocalModelsJobs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, view := range resp["jobs"].([]any) {
		ids[view.(map[string]any)["job_id"].(string)] = true
	}
	if !ids[lmJobID(paused)] {
		t.Fatal("paused job dropped")
	}
	for _, job := range active {
		if !ids[lmJobID(job)] {
			t.Fatalf("active job %v dropped", job["target"])
		}
	}
	lmJobsMu.Lock()
	for _, job := range active {
		job["status"] = "done"
	}
	lmJobsMu.Unlock()
}

func TestPauseRequestPreventsTransitionIntoActivation(t *testing.T) {
	lmDownloadJobsClient(t)
	job := lmNewJob("quickstart", "model", nil)
	lmJobsMu.Lock()
	job["phase"] = "downloading"
	lmRunning[lmJobID(job)] = &lmRunningEntry{pause: pm.NewPauseEvent()}
	lmJobsMu.Unlock()
	resp, err := LocalModelsDownloadPause(context.Background(), lmJobID(job))
	if err != nil || resp["paused"] != true {
		t.Fatalf("pause %v %v", resp, err)
	}
	if err := lmStep(job, "starting-server", "Starting"); !lmIsDownloadPaused(err) {
		t.Fatalf("step %v", err)
	}
	lmJobsMu.Lock()
	defer lmJobsMu.Unlock()
	if job["phase"] != "downloading" {
		t.Fatalf("phase %v", job["phase"])
	}
	job["status"] = "done"
}

func TestRuntimeStagesPreservePmWholePlanProgress(t *testing.T) {
	lmTestHome(t, "home")
	job := lmNewJob("runtime-install", "engine", nil)
	stage := lmRuntimeProgressHook(job)
	transfer := lmDownloadProgressHook(job)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(stage("download", 40, 100, "1/2"))
	must(transfer(40, 100, []pm.RangeSet{{Key: "engine.zip", Ranges: []pm.Range{{Start: 0, End: 40}}}, {Key: "dlls.zip"}}))
	if job["phase"] != "downloading-runtime" || !bytes.Contains([]byte(job["detail"].(string)), []byte("1/2")) {
		t.Fatalf("phase %v detail %v", job["phase"], job["detail"])
	}
	must(stage("download", 50, 100, "2/2"))
	must(transfer(50, 100, []pm.RangeSet{{Key: "engine.zip", Ranges: []pm.Range{{Start: 0, End: 40}}}, {Key: "dlls.zip", Ranges: []pm.Range{{Start: 0, End: 10}}}}))
	if job["done_bytes"] != int64(50) || job["total_bytes"] != int64(100) {
		t.Fatalf("bytes %v of %v", job["done_bytes"], job["total_bytes"])
	}
	must(transfer(100, 100, []pm.RangeSet{{Key: "engine.zip", Ranges: []pm.Range{{Start: 0, End: 40}}}, {Key: "dlls.zip", Ranges: []pm.Range{{Start: 0, End: 60}}}}))
	must(stage("unpack", 0, 0, "2/2"))
	if job["phase"] != "unpacking-runtime" {
		t.Fatalf("phase %v", job["phase"])
	}
	if job["done_bytes"] != int64(100) || job["total_bytes"] != int64(100) {
		t.Fatalf("bytes %v of %v", job["done_bytes"], job["total_bytes"])
	}
	must(stage("verify", 0, 0, ""))
	if job["phase"] != "verifying-runtime" {
		t.Fatalf("phase %v", job["phase"])
	}
	if got := lmRangesTotal(t, job["ranges"]); got != 100 {
		t.Fatalf("ranges cover %d", got)
	}
	lmJobsMu.Lock()
	job["status"] = "done"
	lmJobsMu.Unlock()
}
