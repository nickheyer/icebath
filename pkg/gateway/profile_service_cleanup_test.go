package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/profiles/manage"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Records every supervisor command the profile cleanup would run, executing none
func recordSupervisorCalls(t *testing.T) *[][]string {
	t.Helper()
	calls := &[][]string{}
	t.Cleanup(manage.SwapRunQuiet(func(_ time.Duration, args ...string) (int, error) {
		*calls = append(*calls, append([]string(nil), args...))
		return 0, nil
	}))
	return calls
}

// Unit file pinning a gateway home, the shape the service installer writes
func writePinnedUnit(t *testing.T, path, pinned string) {
	t.Helper()
	writeUnit(t, path, "[Service]\nEnvironment=\"ICEBATH_HOME="+pinned+"\"\nExecStart=/usr/bin/icebath gateway run\n")
}

func TestProfileDeleteFromScratchHomeLeavesTheAccountsProfileUnit(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux platform test")
	}
	tmp := t.TempDir()
	account, scratch := filepath.Join(tmp, "account"), filepath.Join(tmp, "scratch")
	profileDir := filepath.Join(scratch, ".icebath", "profiles", "coder")
	if err := os.MkdirAll(profileDir, 0o755); err != nil {
		t.Fatal(err)
	}
	realUnit := filepath.Join(account, ".config", "systemd", "user", "icebath-gateway-coder.service")
	writePinnedUnit(t, realUnit, filepath.Join(account, ".icebath", "profiles", "coder"))
	for _, k := range []string{home.EnvHome, "XDG_CONFIG_HOME"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	t.Setenv("HOME", scratch)
	t.Setenv("ICEBATH_REAL_HOME", account)
	t.Cleanup(home.PatchUserHome(scratch))
	home.ResetKeyCache()
	pinServiceSeams(t, "linux", 1000, filepath.Join(tmp, "no-system-units"))
	calls := recordSupervisorCalls(t)
	var out bytes.Buffer
	t.Cleanup(manage.SwapOutput(&out, &out))
	if manage.CleanupGatewayService(context.Background(), "coder", profileDir) {
		t.Fatal("cleanup reported a removed unit")
	}
	if len(*calls) != 0 {
		t.Fatalf("supervisor calls %v", *calls)
	}
	if _, err := os.Stat(realUnit); err != nil {
		t.Fatalf("account unit removed: %v", err)
	}
	if !strings.Contains(out.String(), "another install's service, left alone") {
		t.Fatalf("output %q", out.String())
	}
}

func TestDeleteFromAPinnedMultiplexerTargetsTheVictimsUnitOnly(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux platform test")
	}
	tmp := t.TempDir()
	t.Cleanup(home.PatchUserHome(tmp))
	launch := filepath.Join(tmp, ".icebath")
	victim := filepath.Join(launch, "profiles", "victim")
	if err := os.MkdirAll(victim, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(home.EnvHome, launch)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "xdg"))
	home.ResetKeyCache()
	unitDir := filepath.Join(tmp, "xdg", "systemd", "user")
	hostUnit := filepath.Join(unitDir, "icebath-gateway.service")
	victimUnit := filepath.Join(unitDir, "icebath-gateway-victim.service")
	writeUnit(t, hostUnit, "[Service]\n")
	writeUnit(t, victimUnit, "[Service]\n")
	calls := recordSupervisorCalls(t)
	var out bytes.Buffer
	t.Cleanup(manage.SwapOutput(&out, &out))
	provider.SetMultiplexActive(true)
	t.Cleanup(func() { provider.SetMultiplexActive(false) })
	manage.CleanupGatewayService(context.Background(), "victim", victim)
	if _, err := os.Stat(victimUnit); !os.IsNotExist(err) {
		t.Fatalf("victim unit %v", err)
	}
	if _, err := os.Stat(hostUnit); err != nil {
		t.Fatal("the host multiplexer's own unit must survive a profile delete")
	}
	found := false
	for _, c := range *calls {
		found = found || reflect.DeepEqual(c, []string{"systemctl", "--user", "disable", "icebath-gateway-victim"})
		if len(c) >= 4 && reflect.DeepEqual(c[:3], []string{"systemctl", "--user", "disable"}) && c[3] == "icebath-gateway" {
			t.Fatalf("host unit disabled %v", *calls)
		}
	}
	if !found {
		t.Fatalf("calls %v", *calls)
	}
	if got := os.Getenv(home.EnvHome); got != launch {
		t.Fatalf("process home %q", got)
	}
}

func TestEdgeCasesGatewayRunningCheckFallsBackToRuntimeState(t *testing.T) {
	tmp := t.TempDir()
	t.Cleanup(home.PatchUserHome(tmp))
	def := filepath.Join(tmp, ".icebath")
	if err := os.MkdirAll(def, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(home.EnvHome, def)
	home.ResetKeyCache()
	live := os.Getpid()
	doc, err := json.Marshal(map[string]any{
		"pid": live, "kind": "icebath-gateway", "argv": []string{"icebath", "gateway", "run"},
		"start_time": statusProcessStartTime(live), "gateway_state": "running", "active_agents": 0,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(def, "gateway_state.json"), doc, 0o644); err != nil {
		t.Fatal(err)
	}
	swapSeam(t, &statusGetRunningPid, func(string, bool) (int, bool, error) { return 0, false, nil })
	swapSeam(t, &statusReadProcessCmdline, func(int) (string, bool) { return "icebath gateway run --replace", true })
	if !manage.CheckGatewayRunning(def) {
		t.Fatal("runtime state fallback did not see the live gateway")
	}
}
