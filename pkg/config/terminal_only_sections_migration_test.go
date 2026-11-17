package config

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestMigration54DropsTheRetiredInterfaceSections(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	path := filepath.Join(h, "config.yaml")
	text, err := SafeDump(map[string]any{
		"_config_version": 53,
		"dashboard":       map[string]any{"theme": "midnight"},
		"webapp":          map[string]any{"enabled": true},
		"desktop":         map[string]any{"font_family": "Mono"},
		"display":         map[string]any{"skin": "slate"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, text, 0o644); err != nil {
		t.Fatal(err)
	}
	ResetCache()
	t.Cleanup(ResetCache)
	res, err := MigrateConfig(context.Background(), h, false, true)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := SafeLoad(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	doc, _ := parsed.(map[string]any)
	for _, section := range []string{"dashboard", "webapp", "desktop"} {
		if _, kept := doc[section]; kept {
			t.Fatalf("%s survived the migration: %v", section, doc)
		}
	}
	if display, _ := doc["display"].(map[string]any); display["skin"] != "slate" {
		t.Fatalf("display touched: %v", doc["display"])
	}
	found := false
	for _, added := range res.ConfigAdded {
		found = found || added == "removed dashboard, webapp, desktop"
	}
	if !found {
		t.Fatalf("config added %v", res.ConfigAdded)
	}
}

// Runs step 54 over a document, returning what it persisted, the results and the printed message
func m54Run(t *testing.T, doc map[string]any) (map[string]any, *MigrationResults, string) {
	t.Helper()
	var out bytes.Buffer
	var persisted map[string]any
	res := &MigrationResults{}
	r := &migrationRun{h: t.TempDir(), res: res, src: MigrationSources{
		Out:     &out,
		ReadRaw: func(string) (map[string]any, error) { return doc, nil },
		Persist: func(_ string, cfg map[string]any) error { persisted = cfg; return nil },
	}}
	if err := migrateTo54(r); err != nil {
		t.Fatal(err)
	}
	return persisted, res, out.String()
}

func TestMigration54CarriesDashboardSessionKeysToSessions(t *testing.T) {
	persisted, res, out := m54Run(t, map[string]any{
		"dashboard": map[string]any{
			"theme":                      "midnight",
			"ws_orphan_reap_grace_s":     45.0,
			"ws_orphan_activity_stale_s": 900.0,
			"startup_orphan_sweep":       false,
			"auto_continue":              map[string]any{"enabled": false, "freshness_minutes": 30, "max_attempts": 5},
		},
		"sessions": map[string]any{"retention_days": 30},
	})
	want := map[string]any{"sessions": map[string]any{
		"retention_days":             30,
		"ws_orphan_reap_grace_s":     45.0,
		"ws_orphan_activity_stale_s": 900.0,
		"startup_orphan_sweep":       false,
		"auto_continue":              map[string]any{"enabled": false, "freshness_minutes": 30, "max_attempts": 5},
	}}
	if !reflect.DeepEqual(persisted, want) {
		t.Fatalf("persisted %v", persisted)
	}
	moved := "dashboard.ws_orphan_reap_grace_s, dashboard.ws_orphan_activity_stale_s, dashboard.startup_orphan_sweep, dashboard.auto_continue"
	if !reflect.DeepEqual(res.ConfigAdded, []string{"moved " + moved + " to sessions, removed dashboard"}) {
		t.Fatalf("config added %v", res.ConfigAdded)
	}
	wantOut := "  ✓ Moved " + moved + " to sessions — the terminal UI's session socket reads them there.\n" +
		"  ✓ Removed dashboard — the terminal UI is the one interface.\n"
	if out != wantOut {
		t.Fatalf("out %q", out)
	}
}

func TestMigration54KeepsSessionsValuesOverDashboardOnes(t *testing.T) {
	persisted, res, out := m54Run(t, map[string]any{
		"dashboard": map[string]any{
			"ws_orphan_reap_grace_s":     45.0,
			"ws_orphan_activity_stale_s": 900.0,
			"startup_orphan_sweep":       false,
			"auto_continue":              map[string]any{"enabled": false, "freshness_minutes": 30, "max_attempts": 5},
		},
		"sessions": map[string]any{
			"ws_orphan_reap_grace_s": 0.0,
			"startup_orphan_sweep":   true,
			"auto_continue":          map[string]any{"freshness_minutes": 60},
		},
		"webapp": map[string]any{"enabled": true},
	})
	want := map[string]any{"sessions": map[string]any{
		"ws_orphan_reap_grace_s":     0.0,
		"ws_orphan_activity_stale_s": 900.0,
		"startup_orphan_sweep":       true,
		"auto_continue":              map[string]any{"enabled": false, "freshness_minutes": 60, "max_attempts": 5},
	}}
	if !reflect.DeepEqual(persisted, want) {
		t.Fatalf("persisted %v", persisted)
	}
	moved := "dashboard.ws_orphan_activity_stale_s, dashboard.auto_continue.enabled, dashboard.auto_continue.max_attempts"
	if !reflect.DeepEqual(res.ConfigAdded, []string{"moved " + moved + " to sessions, removed dashboard, webapp"}) {
		t.Fatalf("config added %v", res.ConfigAdded)
	}
	wantOut := "  ✓ Moved " + moved + " to sessions — the terminal UI's session socket reads them there.\n" +
		"  ✓ Kept sessions.ws_orphan_reap_grace_s, sessions.startup_orphan_sweep, sessions.auto_continue.freshness_minutes" +
		" — already set there, so the dashboard values were dropped.\n" +
		"  ✓ Removed dashboard, webapp — the terminal UI is the one interface.\n"
	if out != wantOut {
		t.Fatalf("out %q", out)
	}
}

func TestMigration54WithoutDashboardSessionKeysOnlyDropsSections(t *testing.T) {
	persisted, res, out := m54Run(t, map[string]any{
		"dashboard": map[string]any{"theme": "midnight", "ws_ping_interval": 20.0},
		"desktop":   map[string]any{"font_family": "Mono"},
		"display":   map[string]any{"skin": "slate"},
	})
	if !reflect.DeepEqual(persisted, map[string]any{"display": map[string]any{"skin": "slate"}}) {
		t.Fatalf("persisted %v", persisted)
	}
	if !reflect.DeepEqual(res.ConfigAdded, []string{"removed dashboard, desktop"}) {
		t.Fatalf("config added %v", res.ConfigAdded)
	}
	if out != "  ✓ Removed dashboard, desktop — the terminal UI is the one interface.\n" {
		t.Fatalf("out %q", out)
	}
	if persisted, res, out := m54Run(t, map[string]any{"display": map[string]any{"skin": "slate"}}); persisted != nil || len(res.ConfigAdded) != 0 || out != "" {
		t.Fatalf("untouched config persisted %v added %v out %q", persisted, res.ConfigAdded, out)
	}
}

func TestMigration54FillsAnEmptySessionsSectionAndReportsAScalarOne(t *testing.T) {
	persisted, _, _ := m54Run(t, map[string]any{
		"dashboard": map[string]any{"startup_orphan_sweep": false},
		"sessions":  nil,
	})
	if !reflect.DeepEqual(persisted, map[string]any{"sessions": map[string]any{"startup_orphan_sweep": false}}) {
		t.Fatalf("persisted %v", persisted)
	}
	persisted, _, out := m54Run(t, map[string]any{
		"dashboard": map[string]any{"startup_orphan_sweep": false, "ws_orphan_reap_grace_s": 5.0},
		"sessions":  "off",
	})
	if !reflect.DeepEqual(persisted, map[string]any{"sessions": "off"}) {
		t.Fatalf("persisted %v", persisted)
	}
	if !strings.Contains(out, "  ⚠ Dropped dashboard.ws_orphan_reap_grace_s, dashboard.startup_orphan_sweep — sessions is not a mapping") {
		t.Fatalf("out %q", out)
	}
}

func TestMigration54MovedSessionKeysReachTheLoadedConfig(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	path := filepath.Join(h, "config.yaml")
	text, err := SafeDump(map[string]any{
		"_config_version": 53,
		"dashboard": map[string]any{"ws_orphan_reap_grace_s": 45.0, "startup_orphan_sweep": false,
			"auto_continue": map[string]any{"freshness_minutes": 30}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, text, 0o644); err != nil {
		t.Fatal(err)
	}
	ResetCache()
	t.Cleanup(ResetCache)
	if _, err := MigrateConfig(context.Background(), h, false, true); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := SafeLoad(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	doc, _ := parsed.(map[string]any)
	sessions, _ := doc["sessions"].(map[string]any)
	autoContinue, _ := sessions["auto_continue"].(map[string]any)
	if _, kept := doc["dashboard"]; kept || sessions["ws_orphan_reap_grace_s"] != 45.0 || sessions["startup_orphan_sweep"] != false ||
		autoContinue["freshness_minutes"] != 30 {
		t.Fatalf("doc %v", doc)
	}
}
