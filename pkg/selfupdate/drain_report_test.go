package selfupdate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

func TestDrainProgressReporterPrintsHolderAndConfigKnob(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("ICEBATH_HOME", tmp)
	if err := os.MkdirAll(filepath.Join(tmp, "cron"), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(tmp, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("cron/jobs.json", `{"jobs": [{"id": "job-a", "name": "nightly-scout"}]}`)
	write("gateway_state.json", `{"pid": 1, "gateway_state": "draining", "active_work": [{"kind": "cron", "job_id": "job-a", "pid": 4242, "external": true, "elapsed_s": 95}]}`)
	var out []string
	tick := drainProgressReporter(context.Background(), tmp, 600, 0.0, func(s string) { out = append(out, s) })
	tick()
	if len(out) != 1 || !strings.Contains(out[0], "nightly-scout") || !strings.Contains(out[0], "job-a") || !strings.Contains(out[0], "4242") {
		t.Fatalf("report %q", out)
	}
	if !strings.Contains(out[0], "cron job job-a (nightly-scout) in external worker pid 4242, running 1m35s") || !strings.Contains(out[0], "agent.restart_after_turn_timeout") {
		t.Fatalf("report lines %q", out[0])
	}
	write("gateway_state.json", `{"pid": 1, "gateway_state": "draining"}`)
	out = nil
	tick()
	if len(out) != 1 || strings.TrimSpace(out[0]) == "" || !strings.Contains(out[0], "did not report what it is waiting on") {
		t.Fatalf("pre fix gateway report %q", out)
	}
}

func TestDrainHintNamesTheDrainingProfile(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, ".icebath")
	if err := os.MkdirAll(filepath.Join(root, "profiles", "work"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", tmp)
	t.Setenv("ICEBATH_HOME", root)
	work := []any{map[string]any{"kind": "chat", "session": "s1"}}
	for h, want := range map[string]string{filepath.Join(root, "profiles", "work"): "`icebath -p work --tui`", root: "`icebath --tui`"} {
		got, err := formatDrainReport(context.Background(), work, 60, h)
		if err != nil || !strings.Contains(got, want) {
			t.Fatalf("%s report %q %v", h, got, err)
		}
	}
}

func TestDrainReportDescribesEveryUnitShape(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		unit any
		want string
	}{
		{map[string]any{"kind": "chat", "session": "telegram:dm:1", "model": "test/model", "current_tool": "terminal", "elapsed_s": 10.7}, "chat turn telegram:dm:1, running 10s [model test/model, tool terminal]"},
		{map[string]any{"kind": "delegation", "delegation_id": "d1", "pid": 7, "goal": "  scout  ", "elapsed_s": "3600"}, "background delegation d1 pid 7, running 60m00s [scout]"},
		{map[string]any{"kind": "cron", "job_id": "x", "elapsed_s": "soon"}, "cron job x in-process, running ?"},
		{map[string]any{"pid": 0}, "work run"},
		{map[string]any{"kind": "batch", "elapsed_s": -5}, "batch run, running -5s"},
	} {
		got, err := describeActiveWorkUnit(ctx, c.unit, t.TempDir())
		if err != nil || got != c.want {
			t.Fatalf("unit %v got %q want %q (%v)", c.unit, got, c.want, err)
		}
	}
	if _, err := describeActiveWorkUnit(ctx, "chat", ""); err == nil {
		t.Fatal("a non mapping unit must raise as attribute access does")
	}
	empty, err := formatDrainReport(ctx, []any{}, -3, "")
	if err != nil || empty != "  ⏳ still draining — 0s left before the forced restart\n     (no active work reported; the gateway should exit momentarily)" {
		t.Fatalf("empty report %q %v", empty, err)
	}
}

func TestDrainReporterWaitsOneIntervalAndIsInstalledForTheGateway(t *testing.T) {
	now := 100.0
	saved := drainMonotonic
	drainMonotonic = func() float64 { return now }
	t.Cleanup(func() { drainMonotonic = saved })
	h := t.TempDir()
	var out []string
	tick := drainProgressReporter(context.Background(), h, 90, 30, func(s string) { out = append(out, s) })
	now = 129
	tick()
	if len(out) != 0 {
		t.Fatalf("reported before one interval %q", out)
	}
	now = 131
	tick()
	if len(out) != 1 || !strings.HasPrefix(out[0], "  ⏳ still draining — 59s left") {
		t.Fatalf("report %q", out)
	}
	if gateway.DrainProgressReporterHook == nil || gateway.DrainProgressReporterHook(context.Background(), h, 30) == nil {
		t.Fatal("the gateway drain reporter hook is not installed")
	}
}

// Id and name pairs of the jobs file under h, the cron store double TestMain installs
func testCronJobs(h string) ([][2]string, error) {
	raw, err := os.ReadFile(filepath.Join(h, "cron", "jobs.json"))
	if err != nil {
		return nil, err
	}
	doc, err := pyjson.Loads(string(raw))
	if err != nil {
		return nil, err
	}
	list, _ := doc.([]any)
	if obj, ok := doc.(*pyjson.Object); ok {
		jobs, _ := obj.Get("jobs")
		list, _ = jobs.([]any)
	}
	var pairs [][2]string
	for _, j := range list {
		if obj, ok := j.(*pyjson.Object); ok {
			id, _ := obj.Get("id")
			name, _ := obj.Get("name")
			pairs = append(pairs, [2]string{pyrepr.Str(id), drainOr(name, "")})
		}
	}
	return pairs, nil
}
