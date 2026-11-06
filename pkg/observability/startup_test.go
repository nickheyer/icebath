package observability

import (
	"context"
	"os"
	"reflect"
	"strconv"
	"testing"
	"time"
)

// Captures startup marks with collection on, a fresh latch set and the process start pinned ago seconds back
func startupMarks(t *testing.T, ago float64) *markCapture {
	t.Helper()
	m := captureMarks(t)
	savedStarted, savedPid, savedGo := processStartedAt, startupPid, startupGo
	startupMu.Lock()
	savedRecorded := startupRecorded
	startupRecorded = map[startupKey]bool{}
	startupMu.Unlock()
	t.Cleanup(func() {
		processStartedAt, startupPid, startupGo = savedStarted, savedPid, savedGo
		startupMu.Lock()
		startupRecorded = savedRecorded
		startupMu.Unlock()
	})
	processStartedAt = func() (float64, bool) { return float64(time.Now().UnixNano())/1e9 - ago, true }
	return m
}

// Waits up to two seconds for the first captured mark
func waitForMarks(m *markCapture) {
	for range 200 {
		if len(m.snapshot()) > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func startupRow(bucket, surface string) Mark {
	return Mark{Name: StartupLatencyMark, Data: map[string]string{"latency_bucket": bucket, "surface": surface}}
}

func TestStartupLatencyBucketsAreValidCounterDimensions(t *testing.T) {
	for _, c := range []struct {
		ms     any
		bucket string
	}{{0, "lt_500ms"}, {499.9, "lt_500ms"}, {500, "500ms_to_1s"}, {1_999, "1s_to_2s"}, {2_000, "2s_to_5s"},
		{9_999, "5s_to_10s"}, {10_000, "gte_10s"}, {3_600_000, "gte_10s"}} {
		fields := StartupLatencyFields("gateway_boot", c.ms)
		if !reflect.DeepEqual(fields, map[string]string{"latency_bucket": c.bucket, "surface": "gateway_boot"}) {
			t.Fatalf("%v: %v", c.ms, fields)
		}
		if !CounterDimensionsAreValid(StartupLatencyMetric, relayData(fields)) {
			t.Fatalf("%v invalid", fields)
		}
	}
}

func TestStartupLatencyRejectsUnknownSurfacesAndBadDurations(t *testing.T) {
	for _, raw := range [][2]any{{"web", 10}, {"cli", -1}, {"cli", "fast"}, {"cli", true}} {
		if got := StartupLatencyFields(raw[0], raw[1]); got != nil {
			t.Fatalf("%v: %v", raw, got)
		}
	}
}

func TestProcessReadyRecordsProcessStartToNowOncePerSurface(t *testing.T) {
	m := startupMarks(t, 1.5)
	ctx := context.Background()
	RecordProcessReady(ctx, "cli", false)
	RecordProcessReady(ctx, "cli", false)
	RecordProcessReady(ctx, "gateway_boot", false)
	want := []Mark{startupRow("1s_to_2s", "cli"), startupRow("1s_to_2s", "gateway_boot")}
	if got := m.snapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("%v", got)
	}
}

func TestProcessReadyRecordsNothingWhenSharedMetricsAreOff(t *testing.T) {
	m := startupMarks(t, 1)
	m.setPolicy(false)
	RecordProcessReady(context.Background(), "serve_boot", false)
	if got := m.snapshot(); len(got) != 0 {
		t.Fatalf("%v", got)
	}
}

func TestBackgroundReadyRecordDoesNotBlockAndStillLands(t *testing.T) {
	m := startupMarks(t, 0.1)
	RecordProcessReady(context.Background(), "serve_boot", true)
	waitForMarks(m)
	if got := m.snapshot(); !reflect.DeepEqual(got, []Mark{startupRow("lt_500ms", "serve_boot")}) {
		t.Fatalf("%v", got)
	}
}

func TestKanbanWorkerOneShotIsNotACliStartup(t *testing.T) {
	m := startupMarks(t, 0)
	t.Setenv("ICEBATH_KANBAN_TASK", "t_1")
	RecordCliOneShotReady(context.Background())
	if got := m.snapshot(); len(got) != 0 {
		t.Fatalf("%v", got)
	}
}

func TestRpcTakesTheDeclaredSurfaceElseEnvDetection(t *testing.T) {
	m := startupMarks(t, 0)
	ctx := context.Background()
	RecordRpcStartupLatency(ctx, "desktop_attach", 2500, "a")
	RecordRpcStartupLatency(ctx, "tui", 700, "b")
	RecordRpcStartupLatency(ctx, "desktop", 12_000, "c")
	RecordRpcStartupLatency(ctx, "tui", -5, "d")
	RecordRpcStartupLatency(ctx, "web", 700, "e")
	want := []Mark{startupRow("2s_to_5s", "desktop_attach"), startupRow("500ms_to_1s", "tui"), startupRow("gte_10s", "desktop_attach")}
	if got := m.snapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("%v", got)
	}
}

func TestRpcCountsEachClientLaunchOncePerBackendProcess(t *testing.T) {
	m := startupMarks(t, 0)
	ctx := context.Background()
	for range 3 {
		RecordRpcStartupLatency(ctx, "desktop_attach", 1200, "launch-1")
	}
	RecordRpcStartupLatency(ctx, "desktop_attach", 300, "launch-2")
	for range 2 {
		RecordRpcStartupLatency(ctx, "tui", 1200, nil)
	}
	want := []Mark{startupRow("1s_to_2s", "desktop_attach"), startupRow("lt_500ms", "desktop_attach"), startupRow("1s_to_2s", "tui")}
	if got := m.snapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("%v", got)
	}
}

func TestInPlaceRelaunchIsNotAStartupButItsChildrenAre(t *testing.T) {
	m := startupMarks(t, 30)
	t.Setenv(RelaunchedPidEnv, "")
	MarkInPlaceRelaunch()
	RecordProcessReady(context.Background(), "cli", false)
	if got := m.snapshot(); len(got) != 0 {
		t.Fatalf("relaunched program counted %v", got)
	}
	if os.Getenv(RelaunchedPidEnv) != strconv.Itoa(os.Getpid()) {
		t.Fatal("relaunch pid not stamped")
	}
	child := os.Getpid() + 1
	startupPid = func() int { return child }
	RecordProcessReady(context.Background(), "gateway_boot", false)
	if got := m.snapshot(); len(got) != 1 || got[0].Data["surface"] != "gateway_boot" {
		t.Fatalf("%v", got)
	}
}

func TestProcessReadyWhenOffStartsNoThreadAndReadsNoProcessTime(t *testing.T) {
	m := startupMarks(t, 0)
	m.setPolicy(false)
	var touched []string
	processStartedAt = func() (float64, bool) { touched = append(touched, "psutil"); return 0, false }
	startupGo = func(func()) { touched = append(touched, "thread") }
	RecordProcessReady(context.Background(), "serve_boot", true)
	RecordProcessReady(context.Background(), "cli", false)
	if len(touched) != 0 {
		t.Fatalf("%v", touched)
	}
}

func TestCliFirstRenderedPromptRecordsOnce(t *testing.T) {
	m := startupMarks(t, 0.2)
	onRender := CliPromptReadyHandler(context.Background())
	for range 3 {
		onRender(struct{}{})
	}
	waitForMarks(m)
	time.Sleep(50 * time.Millisecond)
	if got := m.snapshot(); !reflect.DeepEqual(got, []Mark{startupRow("lt_500ms", "cli")}) {
		t.Fatalf("%v", got)
	}
}
