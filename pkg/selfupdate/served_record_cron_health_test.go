package selfupdate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/cron"
	"github.com/nickheyer/icebath/pkg/gatewaycli"
)

// Hidden first argument of a re-executed test binary whose command line reads as a running gateway
const servedGatewayStandInEntry = "__selfupdate_gateway_standin__"

// Default root whose live gateway record serves the coder profile, with the coder profile active
func servedRoot(t *testing.T) (string, int) {
	t.Helper()
	tmp := t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	standIn := exec.Command(exe, servedGatewayStandInEntry, "icebath_cli.main", "gateway", "run")
	if err := standIn.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		standIn.Process.Kill()
		standIn.Wait()
	})
	pid := standIn.Process.Pid
	root := filepath.Join(tmp, "icebath")
	for _, name := range []string{"coder", "other"} {
		mustWriteFile(t, filepath.Join(root, "profiles", name, "config.yaml"), "{}\n")
	}
	mustWriteFile(t, filepath.Join(root, "config.yaml"), "model: {default: x}\n")
	record, _ := json.Marshal(map[string]any{"pid": pid, "hermes_home": root})
	mustWriteFile(t, filepath.Join(root, "gateway.pid"), string(record))
	state, _ := json.Marshal(map[string]any{"pid": pid, "hermes_home": root, "gateway_state": "running", "served_profiles": []string{"default", "coder"}})
	mustWriteFile(t, filepath.Join(root, "gateway_state.json"), string(state))
	t.Setenv("ICEBATH_HOME", filepath.Join(root, "profiles", "coder"))
	t.Setenv("GATEWAY_MULTIPLEX_PROFILES", "")
	os.Unsetenv("GATEWAY_MULTIPLEX_PROFILES")
	os.MkdirAll(filepath.Join(tmp, "locks"), 0o755)
	t.Setenv("ICEBATH_GATEWAY_LOCK_DIR", filepath.Join(tmp, "locks"))
	return root, pid
}

func mustWriteFile(t *testing.T, p, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSatelliteGatewayIdentityDoesNotImplyCronHealth(t *testing.T) {
	root, pid := servedRoot(t)
	saved := statusGatewaySnapshot
	statusGatewaySnapshot = func(context.Context, bool) (gatewaycli.GatewayRuntimeSnapshot, error) {
		return gatewaycli.GatewayRuntimeSnapshot{Manager: "systemd (user)"}, nil
	}
	t.Cleanup(func() { statusGatewaySnapshot = saved })
	var out bytes.Buffer
	ctx := context.Background()
	s := &statusRun{ctx: ctx, h: filepath.Join(root, "profiles", "coder"), w: &out, c: &cliui.Console{Out: &out}, config: map[string]any{}}
	if err := s.renderGateway(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "running") || !strings.Contains(out.String(), "multiplexer") {
		t.Fatalf("status gateway section %q", out.String())
	}
	var cronOut bytes.Buffer
	if _, err := cron.CronCommand(ctx, filepath.Join(root, "profiles", "coder"), &cron.CronArgs{Command: "status"}, &cronOut); err != nil {
		t.Fatal(err)
	}
	text := cronOut.String()
	if want := fmt.Sprintf("Scheduler host: the host gateway (PID %d) serving profiles default, coder", pid); !strings.Contains(text, want) {
		t.Fatalf("missing %q in\n%s", want, text)
	}
	if !strings.Contains(text, "restart: icebath --profile default gateway restart") || !strings.Contains(text, "has not reported a heartbeat") ||
		strings.Contains(text, "will fire automatically") {
		t.Fatalf("cron status\n%s", text)
	}
}
