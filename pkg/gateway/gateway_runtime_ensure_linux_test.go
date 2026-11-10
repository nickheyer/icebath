package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
)

func TestEnsureWaitsForRealControlOwnerWithoutClaimingPendingIsReady(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	h := shortControlHome(t)
	var mu sync.Mutex
	state, served, stalled := "starting", true, false
	set := func(apply func()) { mu.Lock(); apply(); mu.Unlock() }
	unstall := make(chan struct{})
	serveIdentify(t, h, func() *pyjson.Object {
		mu.Lock()
		st, sv, sl := state, served, stalled
		mu.Unlock()
		if sl {
			select {
			case <-unstall:
			case <-time.After(30 * time.Second):
			}
		}
		profiles := "[]"
		if sv {
			profiles = fmt.Sprintf(`[{"profile_id": %q, "home": %q}]`, h, h)
		}
		v, _ := pyjson.Loads(fmt.Sprintf(`{"runtime_protocol": 1, "state": %q, "instance_id": "peer", "served_profiles": %s}`, st, profiles))
		return v.(*pyjson.Object)
	})
	t.Cleanup(func() { close(unstall) })
	ensure := func() ObservedEndpoint {
		result, err := EnsureGatewayRuntime(context.Background(), h, 3, false)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	before := time.Now()
	if result := ensure(); result.State != "starting" || result.ReasonCode != "deadline" || time.Since(before) >= 6*time.Second {
		t.Fatalf("%+v", result)
	}
	set(func() { state = "draining" })
	if result := ensure(); result.State != "draining" {
		t.Fatalf("%+v", result)
	}
	set(func() { served = false })
	if result := ensure(); result.State != "inaccessible" || result.ReasonCode != "profile_mismatch" {
		t.Fatalf("%+v", result)
	}
	set(func() { stalled = true })
	if result := ensure(); result.State != "starting" || result.ReasonCode != "deadline" {
		t.Fatalf("%+v", result)
	}
	if _, err := os.Stat(filepath.Join(h, "logs")); err == nil {
		t.Fatal("ensure wrote logs")
	}
}

func TestInstalledServiceStartIsNonmutatingAndFailedManagerNeverSpawns(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	h := filepath.Join(tmp, "profile")
	os.Mkdir(h, 0o700)
	t.Setenv("ICEBATH_HOME", h)
	suffix, err := ServiceSuffix(h)
	if err != nil {
		t.Fatal(err)
	}
	unit := filepath.Join(tmp, ".config/systemd/user", "icebath-gateway-"+suffix+".service")
	os.MkdirAll(filepath.Dir(unit), 0o755)
	os.WriteFile(unit, []byte("[Service]\nExecStart=/owned/inert/gateway\n"), 0o644)
	original, _ := os.ReadFile(unit)
	identity := map[string]string{}
	for _, key := range SystemdIdentityProperties {
		identity[key] = ""
	}
	python := "/opt/inert/bin/python3"
	identity["User"], identity["DynamicUser"] = strconv.Itoa(os.Getuid()), "no"
	identity["Environment"] = `"ICEBATH_HOME=` + h + `"`
	identity["ExecStart"] = "{ path=" + python + " ; argv[]=" + python + " -m icebath_cli.main gateway run ; ignore_errors=no ; }"
	var effective []string
	for _, key := range SystemdIdentityProperties {
		effective = append(effective, key+"="+identity[key])
	}
	calls := filepath.Join(tmp, "calls.txt")
	helper := filepath.Join(tmp, "inert-supervisor")
	script := "#!/bin/sh\nd='" + tmp + "'\nprintf '%s\\n' \"$*\" >> \"$d/calls.txt\"\n" +
		"[ -e \"$d/stall\" ] && exec sleep 5\n" +
		"[ -e \"$d/fail\" ] && { echo private-supervisor-token; exit 3; }\n" +
		"show=0; system=0; env=0; start=0\nfor a in \"$@\"; do case \"$a\" in show) show=1;; system) system=1;; show-environment) env=1;; start) start=1;; esac; done\n" +
		"if [ $show = 1 ] && [ $system = 1 ] && [ -e \"$d/single\" ]; then echo LoadState=not-found\n" +
		"elif [ $env = 1 ]; then echo\n" +
		"elif [ $show = 1 ]; then printf 'LoadState=loaded\\nActiveState=inactive\\nSubState=dead\\nUnitFileState=enabled\\n'; cat <<'EOF'\n" + strings.Join(effective, "\n") + "\nEOF\n" +
		"elif [ $start = 1 ]; then exit 0\nelse exit 91\nfi\n"
	os.WriteFile(helper, []byte(script), 0o700)
	swapSeam(t, &runtimeSystemctlCmd, func(system bool) []string {
		if system {
			return []string{helper, "system"}
		}
		return []string{helper, "user"}
	})
	lines := func() []string {
		raw, _ := os.ReadFile(calls)
		return strings.Split(strings.TrimSpace(string(raw)), "\n")
	}
	starts := func() int {
		n := 0
		for _, line := range lines() {
			for _, arg := range strings.Fields(line) {
				if arg == "start" {
					n++
				}
			}
		}
		return n
	}
	unchanged := func() {
		if now, _ := os.ReadFile(unit); string(now) != string(original) {
			t.Fatal("unit rewritten")
		}
		if _, err := os.Stat(filepath.Join(h, "logs")); err == nil {
			t.Fatal("ensure wrote logs")
		}
	}
	ensure := func() ObservedEndpoint {
		result, err := EnsureGatewayRuntime(context.Background(), h, 3, false)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	if result := ensure(); result.State != "conflict" {
		t.Fatalf("%+v", result)
	}
	unchanged()
	for _, line := range lines() {
		if !strings.Contains(" "+line+" ", " show ") && !strings.Contains(" "+line+" ", " show-environment ") {
			t.Fatalf("mutating call %q", line)
		}
	}
	os.WriteFile(filepath.Join(tmp, "single"), nil, 0o600)
	if result := ensure(); result.State != "starting" || result.ReasonCode != "deadline" || starts() != 1 {
		t.Fatalf("%+v %d", result, starts())
	}
	unchanged()
	os.WriteFile(filepath.Join(tmp, "fail"), nil, 0o600)
	result := ensure()
	if result.State != "inaccessible" || result.ReasonCode != "service_manager_unavailable" || strings.Contains(fmt.Sprintf("%#v", result), "private-supervisor-token") || starts() != 1 {
		t.Fatalf("%+v", result)
	}
	unchanged()
	os.WriteFile(filepath.Join(tmp, "stall"), nil, 0o600)
	before := time.Now()
	if result := ensure(); result.ReasonCode != "deadline" || time.Since(before) >= 6*time.Second || starts() != 1 {
		t.Fatalf("%+v %s", result, time.Since(before))
	}
}

func TestUnmanagedChildUsesExplicitHomeAndSurvivesLauncherExit(t *testing.T) {
	tmp := t.TempDir()
	h := filepath.Join(tmp, "profile")
	os.Mkdir(h, 0o700)
	t.Cleanup(func() { os.WriteFile(filepath.Join(h, "exit"), nil, 0o600) })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	launcher := exec.CommandContext(ctx, os.Args[0], "-test.run=^$")
	launcher.Env = append(os.Environ(), unmanagedLauncherEnv+"="+h, unmanagedChildEnv+"=1", "ICEBATH_HOME="+filepath.Join(tmp, "wrong"))
	out, err := launcher.Output()
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	witness := filepath.Join(h, "child.json")
	for time.Now().Before(deadline) {
		if _, err := os.Stat(witness); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	raw, err := os.ReadFile(witness)
	if err != nil {
		t.Fatal(err)
	}
	var value struct {
		PID, SID int
		Home     string
	}
	json.Unmarshal(raw, &value)
	pid, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	if value.PID != pid || value.SID != pid || value.Home != h {
		t.Fatalf("%+v stdout %q", value, out)
	}
}

func TestUnmanagedRuntimeDoesNotInheritClientLaunchFlags(t *testing.T) {
	tmp := t.TempDir()
	h := filepath.Join(tmp, "policy-home")
	os.Mkdir(h, 0o700)
	witness := filepath.Join(h, "policy.json")
	executable := filepath.Join(tmp, "owned-interpreter")
	os.WriteFile(executable, []byte("#!/bin/sh\nprintf '{\"yolo\": \"%s\", \"ignore_rules\": \"%s\", \"sentinel\": \"%s\"}' \"${ICEBATH_YOLO_MODE-unset}\" \"${ICEBATH_IGNORE_RULES-unset}\" \"${RUNTIME_TEST_SENTINEL-unset}\" > '"+witness+"'\n"), 0o700)
	swapSeam(t, &gatewayLaunchPrefix, func() ([]string, error) { return []string{executable}, nil })
	t.Setenv("ICEBATH_YOLO_MODE", "1")
	t.Setenv("ICEBATH_IGNORE_RULES", "1")
	t.Setenv("RUNTIME_TEST_SENTINEL", "retained")
	child, err := SpawnUnmanagedGateway(context.Background(), h, pythread.Monotonic()+5, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if status, done := child.(*unmanagedChild).Wait(10 * time.Second); !done || status != 0 {
		t.Fatal(status, done)
	}
	raw, _ := os.ReadFile(witness)
	var got map[string]string
	if err := json.Unmarshal(raw, &got); err != nil || got["yolo"] != "unset" || got["ignore_rules"] != "unset" || got["sentinel"] != "retained" {
		t.Fatalf("%s %v", raw, err)
	}
	if os.Getenv("ICEBATH_YOLO_MODE") != "1" || os.Getenv("ICEBATH_IGNORE_RULES") != "1" {
		t.Fatal("launcher env changed")
	}
}

func TestReservedProfileWithoutPidOrControlIsNeverAbsent(t *testing.T) {
	h := filepath.Join(t.TempDir(), "reserved")
	os.Mkdir(h, 0o700)
	owner := NewProfileOwnership()
	if err := owner.Reserve(context.Background(), []string{h}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(h, "gateway.pid")); err == nil {
		t.Fatal("reserve wrote a pid file")
	}
	if got := discover(h, 2*time.Second).State; got != "starting" {
		owner.Close()
		t.Fatal(got)
	}
	os.Chmod(filepath.Join(h, "gateway.lock"), 0)
	got := discover(h, 2*time.Second).State
	os.Chmod(filepath.Join(h, "gateway.lock"), 0o600)
	owner.Close()
	if got != "inaccessible" {
		t.Fatal(got)
	}
	if got := discover(h, 2*time.Second).State; got != "absent" {
		t.Fatal(got)
	}
}
