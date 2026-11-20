package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
	"github.com/nickheyer/icebath/pkg/tools"
)

type emptyProfileSecrets struct{}

func (emptyProfileSecrets) ManagedDotenvKeys() []string   { return nil }
func (emptyProfileSecrets) SourceSuppliedNames() []string { return nil }
func (emptyProfileSecrets) BuildProfileSecretScope(string) map[string]string {
	return map[string]string{}
}
func (emptyProfileSecrets) CurrentSecretScope(context.Context) map[string]string {
	return map[string]string{}
}

// Child env secret sources answering empty scopes where cmd/icebath installs the real provider and env table
func installChildEnvSourcesDouble() {
	tools.ProfileSecrets = emptyProfileSecrets{}
	if tools.ConfigEnvTableKeys == nil {
		tools.ConfigEnvTableKeys = func() []string { return nil }
	}
}

// Container supervision answers for tests, never an s6 host
func installS6SupervisorDouble() {
	S6Running = func() bool { return false }
	S6Scandir = func() string { return "/run/service" }
	S6IsRunning = func(string) (bool, error) { return false, nil }
	S6Start = func(string) error { return nil }
}

// Replaces a package seam for the test's lifetime
func swapSeam[T any](t *testing.T, seam *T, value T) {
	t.Helper()
	old := *seam
	*seam = value
	t.Cleanup(func() { *seam = old })
}

// Ensure seams of an absent owner with no service, recording every unmanaged spawn target
func absentOwnerSpawns(t *testing.T) *[]string {
	t.Helper()
	spawned := &[]string{}
	swapSeam(t, &ensureDiscover, func(context.Context, string, time.Duration) (ObservedEndpoint, error) {
		return ObservedEndpoint{State: "absent"}, nil
	})
	swapSeam(t, &discoverExistingGatewayService, func(context.Context, string, float64) (*ExistingService, error) { return nil, nil })
	swapSeam(t, &spawnUnmanagedGateway, func(_ context.Context, target string, _ float64, _, _ bool) (SpawnedGateway, error) {
		*spawned = append(*spawned, target)
		return nil, nil
	})
	return spawned
}

func evalSymlinkedPath(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestUnmanagedRuntimeDropsSessionConfigBypassFlags(t *testing.T) {
	flags := []string{"ICEBATH_SAFE_MODE", "ICEBATH_IGNORE_USER_CONFIG"}
	for _, key := range flags {
		t.Setenv(key, "1")
	}
	t.Setenv("RUNTIME_TEST_SENTINEL", "retained")
	var captured []string
	swapSeam(t, &gatewayPopen, func(cmd *exec.Cmd) (SpawnedGateway, error) {
		captured = cmd.Env
		return &unmanagedChild{cmd: &exec.Cmd{Process: &os.Process{Pid: 123}}, done: false}, nil
	})
	child, err := SpawnUnmanagedGateway(context.Background(), t.TempDir(), pythread.Monotonic()+5, false, false)
	if err != nil || child.Pid() != 123 {
		t.Fatal(child, err)
	}
	env := map[string]string{}
	for _, kv := range captured {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}
	for _, key := range flags {
		if _, ok := env[key]; ok {
			t.Fatalf("%s reached the daemon", key)
		}
		if os.Getenv(key) != "1" {
			t.Fatalf("%s changed in the launcher", key)
		}
	}
	if env["RUNTIME_TEST_SENTINEL"] != "retained" {
		t.Fatal(env)
	}
}

func TestUpdateFenceFollowsUpdateLockJudgement(t *testing.T) {
	tmp := t.TempDir()
	marker := filepath.Join(tmp, updaterecord.MarkerName)
	dead := exec.Command(os.Args[0], "-test.run=^$")
	if err := dead.Start(); err != nil {
		t.Fatal(err)
	}
	deadCT := updaterecord.ProcessCreateTime(dead.Process.Pid)
	dead.Wait()
	ct := 1.0
	if deadCT != nil && *deadCT != 0 {
		ct = *deadCT
	}
	held := false
	swapSeam(t, &ensureCheckoutLockHeld, func(context.Context) (bool, error) { return held, nil })
	ctx := context.Background()
	for _, body := range []string{fmt.Sprintf("%d\n%d\nct:%.3f\n", dead.Process.Pid, time.Now().Unix(), ct), "not a marker"} {
		os.WriteFile(marker, []byte(body), 0o644)
		if fenced, err := updateFenced(ctx, []string{tmp}); err != nil || fenced {
			t.Fatalf("%q fenced over a free lock", body)
		}
		held = true
		if fenced, err := updateFenced(ctx, []string{tmp}); err != nil || !fenced {
			t.Fatalf("%q not fenced while the lock is held", body)
		}
		held = false
	}
	own := updaterecord.ProcessCreateTime(os.Getpid())
	os.WriteFile(marker, []byte(fmt.Sprintf("%d\n%d\nct:%.3f\n", os.Getpid(), time.Now().Unix(), *own)), 0o644)
	if fenced, err := updateFenced(ctx, []string{tmp}); err != nil || !fenced {
		t.Fatal("a live claim did not fence")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("the client cleared the updater's fence")
	}
}

func TestNamedProfileWithoutMultiplexEvidenceStartsTheHostGateway(t *testing.T) {
	for _, standalone := range []bool{false, true} {
		t.Run(fmt.Sprint(standalone), func(t *testing.T) {
			root := filepath.Join(t.TempDir(), ".icebath")
			h := filepath.Join(root, "profiles", "alpha")
			os.MkdirAll(h, 0o700)
			if standalone {
				os.WriteFile(filepath.Join(h, "config.yaml"), []byte("gateway:\n  standalone: true\n"), 0o644)
			}
			spawned := absentOwnerSpawns(t)
			EnsureGatewayRuntime(context.Background(), h, 0.3, false)
			want := root
			if standalone {
				want = h
			}
			if len(*spawned) != 1 || (*spawned)[0] != evalSymlinkedPath(t, want) {
				t.Fatalf("%v want %s", *spawned, want)
			}
		})
	}
}

func TestColdStartTargetFollowsBootMultiplexPolicy(t *testing.T) {
	for _, tc := range []struct {
		flag       string
		standalone bool
		owner      string
	}{{"false", false, "root"}, {"true", true, "home"}} {
		t.Run(tc.flag, func(t *testing.T) {
			os.Unsetenv("GATEWAY_MULTIPLEX_PROFILES")
			root := filepath.Join(t.TempDir(), ".icebath")
			h := filepath.Join(root, "profiles", "alpha")
			os.MkdirAll(h, 0o700)
			os.WriteFile(filepath.Join(root, "config.yaml"), []byte("gateway:\n  multiplex_profiles: "+tc.flag+"\n"), 0o644)
			if tc.standalone {
				os.WriteFile(filepath.Join(h, "config.yaml"), []byte("gateway:\n  standalone: true\n"), 0o644)
			}
			spawned := absentOwnerSpawns(t)
			EnsureGatewayRuntime(context.Background(), h, 0.3, false)
			want := map[string]string{"root": root, "home": h}[tc.owner]
			if len(*spawned) != 1 || (*spawned)[0] != evalSymlinkedPath(t, want) {
				t.Fatalf("%v want %s", *spawned, want)
			}
		})
	}
}

const standaloneHostReason = "duplicate Telegram token; run icebath gateway migrate --multiplex"

// Named profile whose root holds its lock with record written to the root's gateway_state.json
func standaloneHost(t *testing.T, record map[string]any) (string, *[]string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), ".icebath")
	h := filepath.Join(root, "profiles", "alpha")
	os.MkdirAll(h, 0o700)
	if record != nil {
		raw, _ := json.Marshal(record)
		os.WriteFile(filepath.Join(root, "gateway_state.json"), raw, 0o644)
	}
	resolvedRoot := evalSymlinkedPath(t, root)
	swapSeam(t, &ensureUpdateFenced, func(context.Context, []string) (bool, error) { return false, nil })
	swapSeam(t, &ensureMissingOwnerState, func(path string) string {
		if r, err := filepath.EvalSymlinks(path); err == nil && r == resolvedRoot {
			return "starting"
		}
		return "absent"
	})
	return h, absentOwnerSpawns(t)
}

func TestEnsureReportsAStandaloneHostReasonForAnUnservedNamedProfile(t *testing.T) {
	h, spawned := standaloneHost(t, map[string]any{"gateway_state": "running", "pid": os.Getpid(), "served_profiles": []any{},
		"multiplex_standalone_reason": standaloneHostReason})
	result, err := EnsureGatewayRuntime(context.Background(), h, 1, false)
	if err != nil || result.State != "inaccessible" || result.ReasonCode != "profile_parked" || result.Detail != standaloneHostReason {
		t.Fatalf("%+v %v", result, err)
	}
	if len(*spawned) != 0 {
		t.Fatal(*spawned)
	}
}

func TestAReservedRootWithoutALiveRefusalIsStillAwaited(t *testing.T) {
	for _, tc := range []struct {
		id     string
		record map[string]any
	}{
		{"no-state", nil},
		{"no-reason", map[string]any{"gateway_state": "running", "pid": os.Getpid()}},
		{"stopped", map[string]any{"gateway_state": "stopped", "pid": os.Getpid(), "multiplex_standalone_reason": standaloneHostReason}},
		{"dead-pid", map[string]any{"gateway_state": "running", "pid": 999_999_999, "multiplex_standalone_reason": standaloneHostReason}},
	} {
		t.Run(tc.id, func(t *testing.T) {
			h, spawned := standaloneHost(t, tc.record)
			result, err := EnsureGatewayRuntime(context.Background(), h, 0.5, false)
			if err != nil || result.State != "starting" || result.ReasonCode != "deadline" {
				t.Fatalf("%+v %v", result, err)
			}
			if len(*spawned) != 0 {
				t.Fatal(*spawned)
			}
		})
	}
}
