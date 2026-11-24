//go:build !windows

package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	timeutil "github.com/nickheyer/icebath/pkg/utils/time"
	"golang.org/x/sys/unix"
)

const (
	unmanagedChildEnv    = "ICEBATH_TEST_UNMANAGED_CHILD"
	unmanagedLauncherEnv = "ICEBATH_TEST_UNMANAGED_LAUNCHER"
	unmanagedOwnerFDEnv  = "ICEBATH_TEST_UNMANAGED_OWNER_FD"
)

// Read end of the owner pipe the env names, its only write end held by the owning test process
func unmanagedOwnerPipe() (*os.File, error) {
	fd, err := strconv.Atoi(os.Getenv(unmanagedOwnerFDEnv))
	if err != nil || fd < 3 {
		return nil, fmt.Errorf("%s names no inherited owner pipe descriptor", unmanagedOwnerFDEnv)
	}
	return os.NewFile(uintptr(fd), "unmanaged-owner"), nil
}

// Stand in gateway child recording its identity, then running until its owner pipe reaches end of file
func runUnmanagedStandIn() int {
	owner, err := unmanagedOwnerPipe()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	h := os.Getenv("ICEBATH_HOME")
	sid, _ := unix.Getsid(0)
	raw, _ := json.Marshal(map[string]any{"pid": os.Getpid(), "sid": sid, "home": h})
	os.WriteFile(filepath.Join(h, "child.json"), raw, 0o600)
	_, _ = io.Copy(io.Discard, owner)
	return 0
}

// Runs the re-executed launcher or gateway child of the unmanaged spawn tests, false in a normal run
func dispatchUnmanagedSpawnTestModes() (int, bool) {
	if os.Getenv(unmanagedChildEnv) == "1" && len(os.Args) >= 3 && strings.Join(os.Args[len(os.Args)-3:], " ") == "gateway run --quiet" {
		return runUnmanagedStandIn(), true
	}
	h := os.Getenv(unmanagedLauncherEnv)
	if h == "" {
		return 0, false
	}
	os.Unsetenv(unmanagedLauncherEnv)
	owner, err := unmanagedOwnerPipe()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1, true
	}
	gatewayLaunchPrefix = func() ([]string, error) { return []string{os.Args[0]}, nil }
	real := gatewayPopen
	gatewayPopen = func(cmd *exec.Cmd) (SpawnedGateway, error) {
		argv := cmd.Args
		if strings.Join(argv[len(argv)-3:], " ") != "gateway run --quiet" {
			return nil, fmt.Errorf("argv %v", argv)
		}
		for _, arg := range argv {
			if arg == "--replace" || arg == "--force" {
				return nil, fmt.Errorf("argv %v", argv)
			}
		}
		pinned := ""
		for _, kv := range cmd.Env {
			if v, ok := strings.CutPrefix(kv, "ICEBATH_HOME="); ok {
				pinned = v
			}
		}
		if pinned != h {
			return nil, fmt.Errorf("home %q", pinned)
		}
		cmd.Env = append(cmd.Env, unmanagedOwnerFDEnv+"="+strconv.Itoa(3+len(cmd.ExtraFiles)))
		cmd.ExtraFiles = append(cmd.ExtraFiles, owner)
		return real(cmd)
	}
	child, err := SpawnUnmanagedGateway(context.Background(), h, timeutil.Monotonic()+5, false, false)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1, true
	}
	fmt.Println(child.Pid())
	return 0, true
}

func TestReservedRootNeverSuppressesAStandaloneProfileStart(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	root := filepath.Join(t.TempDir(), ".icebath")
	h := filepath.Join(root, "profiles", "alpha")
	os.MkdirAll(h, 0o700)
	os.WriteFile(filepath.Join(root, "config.yaml"), []byte("model: {default: m}\n"), 0o644)
	os.WriteFile(filepath.Join(h, "config.yaml"), []byte("gateway:\n  standalone: true\n"), 0o644)
	var spawned []string
	swapSeam(t, &discoverExistingGatewayService, func(context.Context, string, float64) (*ExistingService, error) { return nil, nil })
	swapSeam(t, &spawnUnmanagedGateway, func(_ context.Context, target string, _ float64, _, _ bool) (SpawnedGateway, error) {
		spawned = append(spawned, target)
		return nil, nil
	})
	owner := NewProfileOwnership()
	if err := owner.Reserve(context.Background(), []string{root}, ""); err != nil {
		t.Fatal(err)
	}
	EnsureGatewayRuntime(context.Background(), h, 0.5, false)
	owner.Close()
	if len(spawned) != 1 || spawned[0] != evalSymlinkedPath(t, h) {
		t.Fatal(spawned)
	}
}

func TestDeadUnmanagedStartIsAPromptTerminalVerdict(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	h := filepath.Join(t.TempDir(), "profile")
	os.MkdirAll(filepath.Join(h, "logs"), 0o700)
	os.WriteFile(filepath.Join(h, "logs", "gateway-stdio.log"), []byte("OLD_RUN_TRACEBACK\n"), 0o600)
	secret := "sk-proj-" + "A1b2C3d4E5f6G7h8I9j0K1l2M3n4"
	var children []SpawnedGateway
	swapSeam(t, &discoverExistingGatewayService, func(context.Context, string, float64) (*ExistingService, error) { return nil, nil })
	start := gatewayPopen
	swapSeam(t, &spawnUnmanagedGateway, func(_ context.Context, target string, _ float64, _, _ bool) (SpawnedGateway, error) {
		log, err := os.OpenFile(filepath.Join(target, "logs", "gateway-stdio.log"), os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return nil, err
		}
		defer log.Close()
		cmd := exec.Command("/bin/sh", "-c", "echo 'Traceback (most recent call last):'; echo 'RuntimeError: boot failed with key "+secret+"'; exit 3")
		cmd.Stdout, cmd.Stderr = log, log
		child, err := start(cmd)
		children = append(children, child)
		return child, err
	})
	before := time.Now()
	result, err := EnsureGatewayRuntime(context.Background(), h, 20, false)
	if time.Since(before) >= 10*time.Second || err != nil {
		t.Fatal(time.Since(before), err)
	}
	if result.State != "inaccessible" || result.ReasonCode != "runtime_exited" || len(children) != 1 {
		t.Fatalf("%+v %d", result, len(children))
	}
	for _, want := range []string{"status 3", "RuntimeError: boot failed with key", "gateway-stdio.log"} {
		if !strings.Contains(result.Detail, want) {
			t.Fatalf("%q lacks %q", result.Detail, want)
		}
	}
	if strings.Contains(result.Detail, secret) || strings.Contains(result.Detail, "OLD_RUN_TRACEBACK") {
		t.Fatal(result.Detail)
	}
}
