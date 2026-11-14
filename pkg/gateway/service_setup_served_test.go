package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Default root whose live gateway, this test process, records the coder profile as served
func servedRoot(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	root := filepath.Join(tmp, "icebath")
	for _, name := range []string{"coder", "other"} {
		dir := filepath.Join(root, "profiles", name)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write := func(name string, v any) {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, name), raw, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte("model: {default: x}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	write("gateway.pid", map[string]any{"pid": os.Getpid(), "hermes_home": root})
	write("gateway_state.json", map[string]any{"pid": os.Getpid(), "hermes_home": root, "gateway_state": "running",
		"served_profiles": []any{"default", "coder"}})
	t.Setenv("ICEBATH_HOME", filepath.Join(root, "profiles", "coder"))
	t.Setenv("GATEWAY_MULTIPLEX_PROFILES", "")
	os.Unsetenv("GATEWAY_MULTIPLEX_PROFILES")
	if err := os.MkdirAll(filepath.Join(tmp, "locks"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_GATEWAY_LOCK_DIR", filepath.Join(tmp, "locks"))
	real := statusReadProcessCmdline
	swapSetup(t, &statusReadProcessCmdline, func(pid int) (string, bool) {
		if pid == os.Getpid() {
			return "python -m icebath_cli.main gateway run", true
		}
		return real(pid)
	})
	return root
}

func TestSetupGatewayServiceStepSkipsInstallForServedProfile(t *testing.T) {
	root := servedRoot(t)
	var calls []string
	swapSetup(t, &setupSupportsSystemd, func(context.Context) bool { return true })
	swapSetup(t, &setupServiceRunning, func(context.Context) (bool, error) { return false, nil })
	swapSetup(t, &setupServiceInstalled, func(context.Context) (bool, error) {
		for _, c := range calls {
			if c == "install" {
				return true, nil
			}
		}
		return false, nil
	})
	swapSetup(t, &setupConflictingUnits, func(context.Context) (bool, error) { return false, nil })
	swapSetup(t, &setupSystemdInstall, func(context.Context, io.Writer, bool, bool, string, bool, bool, bool) error {
		calls = append(calls, "install")
		return nil
	})
	swapSetup(t, &setupSystemdStart, func(context.Context, bool) error { calls = append(calls, "start"); return nil })
	var out bytes.Buffer
	ok, err := EnsureGatewayService(context.Background(), &out, ServiceSetupOptions{Context: "setup", Install: true})
	if err != nil || !ok || len(calls) != 0 || !strings.Contains(out.String(), "already served by the default multiplexer") {
		t.Fatalf("ok %v %v calls %v\n%s", ok, err, calls, out.String())
	}
	t.Setenv("ICEBATH_HOME", filepath.Join(root, "profiles", "other"))
	out.Reset()
	ok, err = EnsureGatewayService(context.Background(), &out, ServiceSetupOptions{Context: "setup", Install: true})
	if err != nil || !ok || len(calls) != 0 || !strings.Contains(out.String(), "Profile 'other' does not get a gateway of its own") {
		t.Fatalf("ok %v %v calls %v\n%s", ok, err, calls, out.String())
	}
}
