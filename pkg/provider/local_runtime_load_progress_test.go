package provider

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"
)

// Clears the snapshot and keeps the background watcher from attaching to any router during a test
func lr3LoadReset(t *testing.T) {
	clearLoadSnapshot()
	prevEnsure, prevEndpoint, prevNow := loadEnsureWatcher, loadEndpoint, loadMonotonicNow
	loadEnsureWatcher = func() {}
	t.Cleanup(func() {
		loadEnsureWatcher, loadEndpoint, loadMonotonicNow = prevEnsure, prevEndpoint, prevNow
		clearLoadSnapshot()
	})
}

func lr3Percent(t *testing.T, stages []string, current string, value float64) int {
	t.Helper()
	p, err := loadCompositePercent(stages, current, value)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func lr3LoadingEvent(value float64, current string) map[string]any {
	return map[string]any{"status": "loading", "progress": map[string]any{
		"stages": []any{"text_model", "mmproj_model"}, "current": current, "value": value}}
}

func lr3Apply(t *testing.T, model, event string, data map[string]any) {
	t.Helper()
	if err := applyLoadEvent(model, event, data); err != nil {
		t.Fatal(err)
	}
}

func TestCompositePercentTextStageDominates(t *testing.T) {
	stages := []string{"text_model", "spec_model", "mmproj_model"}
	if got := lr3Percent(t, stages, "text_model", 0.5); got != 42 {
		t.Fatalf("text halfway = %d", got)
	}
	if got := lr3Percent(t, stages, "spec_model", 0.0); got != 85 {
		t.Fatalf("spec start = %d", got)
	}
	if got := lr3Percent(t, stages, "mmproj_model", 1.0); got != 100 {
		t.Fatalf("mmproj end = %d", got)
	}
}

func TestCompositePercentMonotoneAcrossStageWalk(t *testing.T) {
	stages := []string{"text_model", "spec_model", "mmproj_model"}
	var seen []int
	for _, stage := range stages {
		for v := 0; v <= 10; v++ {
			seen = append(seen, lr3Percent(t, stages, stage, float64(v)/10))
		}
	}
	if !slices.IsSorted(seen) || seen[0] != 0 || seen[len(seen)-1] != 100 {
		t.Fatalf("walk = %v", seen)
	}
}

func TestCompositePercentSingleStageIsPlain(t *testing.T) {
	if got := lr3Percent(t, []string{"text_model"}, "text_model", 0.4); got != 40 {
		t.Fatalf("single stage = %d", got)
	}
}

func TestLoadingEventsBuildSnapshotAndTerminalClears(t *testing.T) {
	lr3LoadReset(t)
	lr3Apply(t, "m1", "status_change", lr3LoadingEvent(0.5, "text_model"))
	snap := GetLoadingProgress()
	if e, ok := snap["m1"]; !ok || e.Percent != 42 || e.Stage != "text_model" {
		t.Fatalf("snapshot = %+v", snap)
	}
	lr3Apply(t, "m1", "status_change", map[string]any{"status": "loaded", "info": map[string]any{}})
	if snap := GetLoadingProgress(); len(snap) != 0 {
		t.Fatalf("terminal status kept %+v", snap)
	}
}

func TestUnloadAndFailureClearToo(t *testing.T) {
	lr3LoadReset(t)
	lr3Apply(t, "m1", "status_change", lr3LoadingEvent(0.2, "text_model"))
	lr3Apply(t, "m1", "status_change", map[string]any{"status": "unloaded", "exit_code": 1})
	if snap := GetLoadingProgress(); len(snap) != 0 {
		t.Fatalf("unload kept %+v", snap)
	}
	lr3Apply(t, "m2", "status_change", lr3LoadingEvent(0.9, "text_model"))
	lr3Apply(t, "m2", "model_remove", map[string]any{})
	if snap := GetLoadingProgress(); len(snap) != 0 {
		t.Fatalf("model_remove kept %+v", snap)
	}
}

func TestProgresslessLoadingEventKeepsEntryAlive(t *testing.T) {
	lr3LoadReset(t)
	lr3Apply(t, "m1", "model_status", map[string]any{"status": "loading"})
	if e, ok := GetLoadingProgress()["m1"]; !ok || e.Percent != 0 {
		t.Fatalf("progressless load = %+v %v", e, ok)
	}
}

func TestStaleEntriesExpire(t *testing.T) {
	lr3LoadReset(t)
	lr3Apply(t, "m1", "status_change", lr3LoadingEvent(0.5, "text_model"))
	loadMu.Lock()
	loadSnapshot["m1"].ts = loadSnapshot["m1"].ts.Add(-(loadStaleEntryTTL + time.Second))
	loadMu.Unlock()
	if snap := GetLoadingProgress(); len(snap) != 0 {
		t.Fatalf("stale entry kept %+v", snap)
	}
}

func TestLoadProgressStreamAppliesSSEEventsInDocumentStageOrder(t *testing.T) {
	lr3LoadReset(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models/sse" || r.Header.Get("Authorization") != "Bearer k" || r.Header.Get("Accept") != "text/event-stream" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		fmt.Fprint(w, ": comment\n")
		fmt.Fprint(w, `data: {"model":"m1","event":"status_change","data":{"status":"loading","progress":{"stages":{"text_model":1,"mmproj_model":1},"current":"text_model","value":0.5}}}`+"\n")
		fmt.Fprint(w, `data: {"model":"m2","event":"model_status","data":{"status":"loading","progress":{"stages":{"mmproj_model":1,"text_model":1},"current":"text_model","value":0.5}}}`+"\n")
		fmt.Fprint(w, "data: not json\n")
		fmt.Fprint(w, `data: {"model":"m3","event":"status_change","data":{"status":"loading","progress":{"stages":7,"current":"text_model","value":0.5}}}`+"\n")
	}))
	defer srv.Close()
	if err := streamLoadEvents(srv.URL, "k"); err != nil {
		t.Fatal(err)
	}
	snap := GetLoadingProgress()
	if snap["m1"].Percent != 42 || snap["m2"].Percent != 57 {
		t.Fatalf("snapshot = %+v", snap)
	}
	if _, ok := snap["m3"]; ok {
		t.Fatal("a non iterable stages value registered a load")
	}
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `data: ["not","a","dict"]`+"\n")
	}))
	defer broken.Close()
	if err := streamLoadEvents(broken.URL, "k"); err == nil {
		t.Fatal("a non object message did not end the stream")
	}
}

func TestPrefillProgressReadsBusiestProcessingSlot(t *testing.T) {
	lr3LoadReset(t)
	body := `[{"id":0,"is_processing":false,"n_prompt_tokens_processed":500},` +
		`{"id":1,"is_processing":true,"n_prompt_tokens_processed":42},` +
		`{"id":2,"is_processing":true,"n_prompt_tokens_processed":32768}]`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, body)
	}))
	defer srv.Close()
	loadEndpoint = func() (string, string, bool) { return srv.URL, "k", true }
	if n, ok, err := GetPrefillProgress("m"); err != nil || !ok || n != 32768 {
		t.Fatalf("busiest slot = %d %v %v", n, ok, err)
	}
	body = `[{"id":0,"is_processing":false,"n_prompt_tokens_processed":500}]`
	if n, ok, err := GetPrefillProgress("m"); err != nil || ok {
		t.Fatalf("idle slots = %d %v %v", n, ok, err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := "http://" + ln.Addr().String()
	ln.Close()
	loadEndpoint = func() (string, string, bool) { return dead, "k", true }
	if n, ok, err := GetPrefillProgress("m"); err != nil || ok {
		t.Fatalf("unreachable server = %d %v %v", n, ok, err)
	}
}
