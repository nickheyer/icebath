//go:build linux

package gatewaycli

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Fake proc tree without a supervisor process so reconcile never reads a host container argv
func hermeticContainerArgv(t *testing.T, argv ...string) {
	t.Helper()
	root := t.TempDir()
	if len(argv) > 0 {
		os.MkdirAll(filepath.Join(root, "1"), 0o755)
		os.WriteFile(filepath.Join(root, "1", "cmdline"), []byte(strings.Join(argv, "\x00")+"\x00"), 0o644)
	}
	saved, uid, gid := procRoot, icebathUID, icebathGID
	procRoot, icebathUID, icebathGID = root, os.Getuid(), os.Getgid()
	t.Cleanup(func() { procRoot, icebathUID, icebathGID = saved, uid, gid })
}

func makeBootProfile(t *testing.T, home, name string, state, desired string) string {
	t.Helper()
	p := filepath.Join(home, "profiles", name)
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(p, "SOUL.md"), []byte("# fake profile\n"), 0o644)
	if state != "" || desired != "" {
		payload := map[string]any{"timestamp": 1234567890}
		if state != "" {
			payload["gateway_state"] = state
		}
		if desired != "" {
			payload["desired_state"] = desired
		}
		raw, _ := json.Marshal(payload)
		os.WriteFile(filepath.Join(p, "gateway_state.json"), raw, 0o644)
	}
	return p
}

func seedDefaultRoot(t *testing.T, home, state string) {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"gateway_state": state, "timestamp": 1234567890})
	os.WriteFile(filepath.Join(home, "gateway_state.json"), raw, 0o644)
}

func namedActions(actions []ReconcileAction) []ReconcileAction {
	var out []ReconcileAction
	for _, a := range actions {
		if a.Profile != "default" {
			out = append(out, a)
		}
	}
	return out
}

func byProfile(actions []ReconcileAction) map[string]ReconcileAction {
	out := map[string]ReconcileAction{}
	for _, a := range actions {
		out[a.Profile] = a
	}
	return out
}

func isRegisteredCoder(t *testing.T, actions []ReconcileAction) {
	t.Helper()
	named := namedActions(actions)
	if len(named) != 1 || named[0].Profile != "coder" || named[0].PriorState == nil || *named[0].PriorState != "running" ||
		named[0].Action != "registered" || named[0].PriorExit != "unknown" || !named[0].FoldedIntoRoot {
		t.Fatalf("named actions = %+v", named)
	}
}

func TestANamedProfileSlotIsRegisteredButNeverAutostarted(t *testing.T) {
	hermeticContainerArgv(t)
	home := t.TempDir()
	scandir := filepath.Join(home, "run-service")
	os.Mkdir(scandir, 0o755)
	makeBootProfile(t, home, "coder", "running", "")
	os.WriteFile(filepath.Join(home, "gateway_state.json"), []byte(`{"gateway_state": "running"}`), 0o644)
	actions, err := ReconcileProfileGateways(context.Background(), home, scandir, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	isRegisteredCoder(t, actions)
	svc := filepath.Join(scandir, "gateway-coder")
	info, err := os.Stat(filepath.Join(svc, "run"))
	if err != nil || info.Mode()&0o111 == 0 {
		t.Fatalf("run missing or not executable: %v", err)
	}
	if raw, _ := os.ReadFile(filepath.Join(svc, "type")); strings.TrimSpace(string(raw)) != "longrun" {
		t.Fatalf("type = %q", raw)
	}
	if _, err := os.Stat(filepath.Join(svc, "down")); err != nil {
		t.Fatal("a named slot must not boot itself")
	}
	if byProfile(actions)["default"].Action != "started" {
		t.Fatalf("default action = %+v", byProfile(actions)["default"])
	}
}

func TestTheRetiredOptOutCannotBootASecondGatewayInTheContainer(t *testing.T) {
	for _, tc := range []struct{ id, config, env string }{{"config-false", "false", ""}, {"env-false-overrides-config", "true", "false"}} {
		t.Run(tc.id, func(t *testing.T) {
			hermeticContainerArgv(t)
			home := t.TempDir()
			scandir := filepath.Join(home, "run-service")
			os.Mkdir(scandir, 0o755)
			makeBootProfile(t, home, "coder", "running", "")
			os.WriteFile(filepath.Join(home, "config.yaml"), []byte("multiplex_profiles: "+tc.config+"\n"), 0o644)
			t.Setenv("ICEBATH_HOME", home)
			if tc.env == "" {
				os.Unsetenv("GATEWAY_MULTIPLEX_PROFILES")
			} else {
				t.Setenv("GATEWAY_MULTIPLEX_PROFILES", tc.env)
			}
			actions, err := ReconcileProfileGateways(context.Background(), home, scandir, false, nil)
			if err != nil {
				t.Fatal(err)
			}
			isRegisteredCoder(t, actions)
			if _, err := os.Stat(filepath.Join(scandir, "gateway-coder", "down")); err != nil {
				t.Fatal("slot not down")
			}
		})
	}
}

func TestRegisteredProfileHasFinishScript(t *testing.T) {
	hermeticContainerArgv(t)
	home := t.TempDir()
	scandir := filepath.Join(home, "run-service")
	os.Mkdir(scandir, 0o755)
	makeBootProfile(t, home, "coder", "running", "")
	if _, err := ReconcileProfileGateways(context.Background(), home, scandir, false, nil); err != nil {
		t.Fatal(err)
	}
	finish := filepath.Join(scandir, "gateway-coder", "finish")
	info, err := os.Stat(finish)
	if err != nil || info.Mode()&0o111 == 0 {
		t.Fatalf("finish missing or not executable: %v", err)
	}
	text, _ := os.ReadFile(finish)
	if !bytes.Contains(text, []byte("78")) || !bytes.Contains(text, []byte("125")) {
		t.Fatalf("finish = %s", text)
	}
}

func TestRegisterServiceOverwritesExistingSlot(t *testing.T) {
	hermeticContainerArgv(t)
	home := t.TempDir()
	scandir := filepath.Join(home, "run-service")
	os.Mkdir(scandir, 0o755)
	profile := makeBootProfile(t, home, "coder", "running", "")
	if _, err := ReconcileProfileGateways(context.Background(), home, scandir, false, nil); err != nil {
		t.Fatal(err)
	}
	first, _ := os.ReadFile(filepath.Join(scandir, "gateway-coder", "run"))
	os.WriteFile(filepath.Join(profile, "gateway_state.json"), []byte(`{"gateway_state": "stopped"}`), 0o644)
	if _, err := ReconcileProfileGateways(context.Background(), home, scandir, false, nil); err != nil {
		t.Fatal(err)
	}
	again, _ := os.ReadFile(filepath.Join(scandir, "gateway-coder", "run"))
	if !bytes.Equal(first, again) {
		t.Fatal("run script changed")
	}
	for _, pattern := range []string{"*.tmp", ".*.tmp"} {
		if m, _ := filepath.Glob(filepath.Join(scandir, pattern)); len(m) != 0 {
			t.Fatalf("leftover temp dirs %v", m)
		}
	}
	if _, err := os.Stat(filepath.Join(scandir, "gateway-coder", "down")); err != nil {
		t.Fatal("slot not down")
	}
}

func TestProfilesDefaultSubdirIsSkippedWithWarning(t *testing.T) {
	hermeticContainerArgv(t)
	var buf bytes.Buffer
	saved := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(saved) })
	home := t.TempDir()
	scandir := filepath.Join(home, "run-service")
	os.Mkdir(scandir, 0o755)
	makeBootProfile(t, home, "default", "running", "")
	actions, err := ReconcileProfileGateways(context.Background(), home, scandir, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, a := range actions {
		if a.Profile == "default" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("default actions = %d", count)
	}
	if !strings.Contains(buf.String(), "profiles/default/") {
		t.Fatalf("no warning: %s", buf.String())
	}
}

func TestMainSkipsReconcileInDashboardContainerS6v3(t *testing.T) {
	hermeticContainerArgv(t, "/bin/sh", "-e", "/run/s6/basedir/scripts/rc.init", "top", "/opt/icebath/docker/main-wrapper.sh",
		"dashboard", "--host", "0.0.0.0", "--port", "9119", "--no-open", "--insecure")
	home := t.TempDir()
	scandir := filepath.Join(home, "run-service")
	os.Mkdir(scandir, 0o755)
	makeBootProfile(t, home, "worker", "running", "")
	t.Setenv("ICEBATH_HOME", home)
	t.Setenv("S6_PROFILE_GATEWAY_SCANDIR", scandir)
	var out bytes.Buffer
	if rc := ContainerBootMain(context.Background(), &out); rc != 0 {
		t.Fatalf("rc = %d", rc)
	}
	for _, name := range []string{"gateway-worker", "gateway-default"} {
		if _, err := os.Stat(filepath.Join(scandir, name)); err == nil {
			t.Fatalf("%s registered in a dashboard container", name)
		}
	}
}

func TestANamedSlotsAutostartIntentBootsTheRootSlot(t *testing.T) {
	hermeticContainerArgv(t)
	root := t.TempDir()
	home := filepath.Join(root, "data")
	os.Mkdir(home, 0o755)
	makeBootProfile(t, home, "coder", "", "running")
	actions, err := ReconcileProfileGateways(context.Background(), home, filepath.Join(root, "svc"), true, []string{})
	if err != nil {
		t.Fatal(err)
	}
	by := byProfile(actions)
	if by["default"].Action != "started" || !by["default"].FoldedIntoRoot {
		t.Fatalf("default = %+v", by["default"])
	}
	if by["coder"].Action != "registered" || !by["coder"].FoldedIntoRoot {
		t.Fatalf("coder = %+v", by["coder"])
	}
}

func TestAStandaloneProfileBootsItsOwnSlotInsteadOfFoldingIntoRoot(t *testing.T) {
	hermeticContainerArgv(t)
	root := t.TempDir()
	home := filepath.Join(root, "data")
	os.Mkdir(home, 0o755)
	seedDefaultRoot(t, home, "stopped")
	solo := makeBootProfile(t, home, "solo", "", "running")
	os.WriteFile(filepath.Join(solo, "config.yaml"), []byte("gateway:\n  standalone: true\n"), 0o644)
	makeBootProfile(t, home, "coder", "", "running")
	actions, err := ReconcileProfileGateways(context.Background(), home, filepath.Join(root, "svc"), true, []string{})
	if err != nil {
		t.Fatal(err)
	}
	by := byProfile(actions)
	if by["solo"].Action != "started" || by["solo"].FoldedIntoRoot {
		t.Fatalf("solo = %+v", by["solo"])
	}
	if by["coder"].Action != "registered" || !by["coder"].FoldedIntoRoot {
		t.Fatalf("coder = %+v", by["coder"])
	}
	if by["default"].Action != "started" {
		t.Fatalf("default = %+v", by["default"])
	}
}

func TestAStoppedFleetStillBootsNothing(t *testing.T) {
	hermeticContainerArgv(t)
	root := t.TempDir()
	home := filepath.Join(root, "data")
	os.Mkdir(home, 0o755)
	seedDefaultRoot(t, home, "stopped")
	makeBootProfile(t, home, "coder", "", "stopped")
	makeBootProfile(t, home, "ops", "", "startup_failed")
	actions, err := ReconcileProfileGateways(context.Background(), home, filepath.Join(root, "svc"), true, []string{})
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 3 {
		t.Fatalf("actions = %+v", actions)
	}
	for _, a := range actions {
		if a.Action != "registered" || a.FoldedIntoRoot {
			t.Fatalf("action = %+v", a)
		}
	}
}
