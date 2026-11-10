package plugins

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/pm"
)

// Temp home holding the cadence markers with the package logger captured
func cadenceHome(t *testing.T) (context.Context, string, *bytes.Buffer) {
	t.Helper()
	h := t.TempDir()
	t.Setenv(home.EnvHome, h)
	config.ResetCache()
	var buf bytes.Buffer
	saved := Logger
	Logger = slog.New(slog.NewTextHandler(&syncWriter{w: &buf}, &slog.HandlerOptions{Level: slog.LevelDebug}))
	t.Cleanup(func() { Logger = saved })
	return home.WithOverride(context.Background(), h), h, &buf
}

func cadenceRow(name, class string, available *bool, needsFixing string) *CheckResult {
	return &CheckResult{Name: name, Class: class, UpdateAvailable: available, NeedsFixing: needsFixing}
}

func TestCheckDueClockGate(t *testing.T) {
	ctx, h, _ := cadenceHome(t)
	now, day, zero := 1000.0, 24.0, 0.0
	if !CheckDue(ctx, &now, &day, nil) {
		t.Fatal("never run is not due")
	}
	marker := filepath.Join(h, "plugin-update-checks", "last-run")
	c6Write(t, marker, "x")
	recent := time.Now().Add(-100 * time.Second)
	if err := os.Chtimes(marker, recent, recent); err != nil {
		t.Fatal(err)
	}
	current := float64(time.Now().UnixNano()) / 1e9
	if CheckDue(ctx, &current, &day, nil) {
		t.Fatal("due within the interval")
	}
	if CheckDue(ctx, &current, &zero, nil) {
		t.Fatal("zero interval is due")
	}
}

func TestCheckIntervalHoursReadsConfig(t *testing.T) {
	ctx, _, _ := cadenceHome(t)
	for _, tc := range []struct {
		value any
		want  float64
	}{{nil, 24}, {6, 6}, {0, 0}, {"garbage", 24}, {"6.5", 6.5}, {-3, 0}, {true, 1}} {
		value := tc.value
		if got := CheckIntervalHours(ctx, func(string, string) (any, error) { return value, nil }); got != tc.want {
			t.Errorf("%v: %v", tc.value, got)
		}
	}
	if got := CheckIntervalHours(ctx, func(string, string) (any, error) { return nil, errors.New("boom") }); got != 24 {
		t.Errorf("raising reader %v", got)
	}
}

func TestScheduledCheckWritesReceiptAndMarker(t *testing.T) {
	for _, damaged := range []bool{false, true} {
		ctx, h, _ := cadenceHome(t)
		plugins := filepath.Join(h, "plugins")
		if damaged {
			c6Write(t, filepath.Join(plugins, ".install-metadata.json"), "\xff\xfe{not utf-8")
		}
		var calls []string
		yes := true
		opts := ScheduledCheckOptions{PluginsDir: plugins, RunChecks: func(_ context.Context, dir string) ([]*CheckResult, error) {
			calls = append(calls, dir)
			return []*CheckResult{cadenceRow("plug", "git", &yes, "")}, nil
		}}
		results, ran := RunScheduledCheck(ctx, opts)
		if !ran || results[0].Name != "plug" {
			t.Fatalf("%v %v", results, ran)
		}
		if got := pm.LatestReceipt(ctx)["outcome"]; got != "updates-available" {
			t.Fatalf("outcome %v", got)
		}
		if _, ran := RunScheduledCheck(ctx, opts); ran || len(calls) != 1 {
			t.Fatalf("second tick ran %v calls %v", ran, calls)
		}
	}
}

func TestAutoApplySelectsOnlyUpdateableGitRows(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		ctx, h, buf := cadenceHome(t)
		yes := true
		rows := []*CheckResult{cadenceRow("gitplug", "git", &yes, ""), cadenceRow("pipplug", "pip", &yes, ""), cadenceRow("broken", "git", nil, "mismatch")}
		var applied []string
		RunScheduledCheck(ctx, ScheduledCheckOptions{PluginsDir: filepath.Join(h, "plugins"),
			RunChecks:   func(context.Context, string) ([]*CheckResult, error) { return rows, nil },
			ApplyUpdate: func(_ context.Context, name string) error { applied = append(applied, name); return nil },
			ConfigGet: func(section, key string) (any, error) {
				if key == "auto_apply" {
					return enabled, nil
				}
				return 24, nil
			}})
		want := []string(nil)
		if enabled {
			want = []string{"gitplug"}
		}
		if !reflect.DeepEqual(applied, want) || pm.LatestReceipt(ctx)["outcome"] != "updates-available" {
			t.Fatalf("%v %v", applied, pm.LatestReceipt(ctx))
		}
		if !strings.Contains(buf.String(), "broken") || !strings.Contains(buf.String(), "trust-update-url") {
			t.Fatal(buf.String())
		}
	}
}

func TestGatewayCheckRunsRealCadenceAndBacksOff(t *testing.T) {
	for _, failed := range []bool{false, true} {
		ctx, h, buf := cadenceHome(t)
		c6Write(t, filepath.Join(h, "config.yaml"), "plugins:\n  auto_apply: true\n")
		config.ResetCache()
		var calls, applied []string
		savedChecks, savedApply := gatewayRunChecks, gatewayApplyUpdate
		gatewayRunChecks = func(_ context.Context, dir string) ([]*CheckResult, error) {
			calls = append(calls, dir)
			if failed {
				return nil, errors.New("offline")
			}
			yes := true
			return []*CheckResult{cadenceRow("plug", "git", &yes, "")}, nil
		}
		gatewayApplyUpdate = func(_ context.Context, name string) error { applied = append(applied, name); return nil }
		for range 2 {
			if _, _, err := MaybeRunGatewayCheck(ctx, ScheduledCheckOptions{}); err != nil {
				t.Fatal(err)
			}
		}
		gatewayRunChecks, gatewayApplyUpdate = savedChecks, savedApply
		if !reflect.DeepEqual(calls, []string{filepath.Join(h, "plugins")}) {
			t.Fatalf("calls %v", calls)
		}
		if (failed && applied != nil) || (!failed && !reflect.DeepEqual(applied, []string{"plug"})) {
			t.Fatalf("applied %v", applied)
		}
		saved := pm.LatestReceipt(ctx)
		outcome, code := "updates-available", 0
		if failed {
			outcome, code = "failed", 1
		}
		if saved["outcome"] != outcome || !pyrepr.Equal(saved["exit_code"], code) {
			t.Fatalf("%v", saved)
		}
		if _, err := os.Stat(filepath.Join(h, "plugin-update-checks", "last-run")); err != nil {
			t.Fatal(err)
		}
		if failed {
			warnings, _ := saved["warnings"].([]any)
			first, _ := warnings[0].(map[string]any)
			if msg := pyrepr.Str(first["message"]); msg == "" || !strings.Contains(buf.String(), msg) {
				t.Fatalf("%v %s", saved["warnings"], buf.String())
			}
		}
	}
}

func TestCachedGitUpdatesFollowTheCheckedInstall(t *testing.T) {
	ctx, h, _ := cadenceHome(t)
	dir := filepath.Join(h, "plugins")
	checked := map[string]any{"source": "https://example.invalid/tern.git", "revision": strings.Repeat("a", 40), "pinned": false}
	with := func(key string, value any) map[string]any {
		out := map[string]any{}
		for k, v := range checked {
			out[k] = v
		}
		out[key] = value
		return out
	}
	writeMeta := func(rows map[string]any) {
		if err := config.AtomicJSONWriteMode(ctx, filepath.Join(dir, ".install-metadata.json"), rows, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeMeta(map[string]any{"tern": checked, "feed": checked})
	before := snapshotInstalls(dir)
	writeMeta(map[string]any{"tern": with("revision", strings.Repeat("b", 40)), "feed": checked})
	yes := true
	recordCheckResults(ctx, []*CheckResult{
		{Name: "tern", Class: "git", Current: strings.Repeat("a", 40), Latest: strings.Repeat("b", 40), UpdateAvailable: &yes},
		{Name: "feed", Class: "git", Current: "1.0.0", Latest: "1.1.0", UpdateAvailable: &yes},
		{Name: "spotify", Class: "catalog", Current: strings.Repeat("c", 40), Latest: strings.Repeat("d", 40), UpdateAvailable: &yes},
	}, before)
	if got := CachedGitUpdates(ctx, map[string]any{"tern": checked, "feed": checked}); !reflect.DeepEqual(got, map[string]string{"tern": strings.Repeat("b", 40), "feed": "1.1.0"}) {
		t.Fatalf("%v", got)
	}
	for _, record := range []map[string]any{with("revision", strings.Repeat("b", 40)), with("source", "https://example.invalid/fork.git"), with("pinned", true)} {
		if got := CachedGitUpdates(ctx, map[string]any{"tern": record}); len(got) != 0 {
			t.Fatalf("%v %v", record, got)
		}
	}
}
