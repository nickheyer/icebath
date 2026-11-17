package provider

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"

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

func TestPauseRequestPreventsTransitionIntoActivation(t *testing.T) {
	lmDownloadJobsClient(t)
	job := lmNewJob("quickstart", "model", nil)
	lmJobsMu.Lock()
	job["phase"] = "downloading"
	lmRunning[lmJobID(job)] = &lmRunningEntry{pause: pm.NewPauseEvent()}
	lmJobsMu.Unlock()
	if !LocalModelsPauseJob(lmJobID(job)) {
		t.Fatal("pause refused")
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
