package gateway

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/provider"
)

var restartFallbackArgv = []string{"python", "-m", "icebath_cli.main", "gateway", "run"}

// Pins the captured argv of every pid like patching the argv capture
func pinRestartArgv(t *testing.T, argv []string) {
	t.Helper()
	swapSeam(t, &updateRestartCaptureArgv, func(int) []string { return argv })
}

// Replaces the profile relaunch with a fixed answer
func fixProfileRelaunch(t *testing.T, launched bool) {
	t.Helper()
	swapSeam(t, &updateRestartByProfile, func(context.Context, string, int) (bool, error) { return launched, nil })
}

// Replaces the argv replay with one that fails the test when reached
func forbidCmdlineReplay(t *testing.T, why string) {
	t.Helper()
	swapSeam(t, &updateRestartByCmdline, func(context.Context, int, []string, *string) (bool, error) {
		t.Fatal(why)
		return false, nil
	})
}

// Replaces the profile relaunch with one that fails the test when reached
func forbidProfileRelaunch(t *testing.T) {
	t.Helper()
	swapSeam(t, &updateRestartByProfile, func(context.Context, string, int) (bool, error) {
		t.Fatal("detached watcher must not be launched")
		return false, nil
	})
}

// Mode and armed verdict of the update restart preparation
func prepareRestart(t *testing.T, profile string, pid int) (string, bool) {
	t.Helper()
	mode, armed, err := PrepareProfileGatewayUpdateRestart(context.Background(), profile, pid)
	if err != nil {
		t.Fatal(err)
	}
	return mode, armed
}

func TestProfileRelaunchWinsAndSkipsTheCmdlineReplay(t *testing.T) {
	pinRestartArgv(t, slices.Clone(restartFallbackArgv))
	fixProfileRelaunch(t, true)
	forbidCmdlineReplay(t, "cmdline replay must not run when the profile path works")
	if mode, armed := prepareRestart(t, "fitness", 4242); !armed || mode != "detached" {
		t.Fatalf("%q %v", mode, armed)
	}
}

func TestFallsBackToCmdlineReplayWhenProfileRelaunchFails(t *testing.T) {
	pinRestartArgv(t, slices.Clone(restartFallbackArgv))
	fixProfileRelaunch(t, false)
	type replay struct {
		pid  int
		argv []string
	}
	var seen []replay
	swapSeam(t, &updateRestartByCmdline, func(_ context.Context, pid int, argv []string, _ *string) (bool, error) {
		seen = append(seen, replay{pid, argv})
		return true, nil
	})
	if mode, armed := prepareRestart(t, "fitness", 4242); !armed || mode != "detached-cmdline" {
		t.Fatalf("%q %v", mode, armed)
	}
	if len(seen) != 1 || seen[0].pid != 4242 || !slices.Equal(seen[0].argv, restartFallbackArgv) {
		t.Fatalf("%v", seen)
	}
}

func TestReturnsNoneWhenThereIsNoArgvToReplay(t *testing.T) {
	pinRestartArgv(t, []string{})
	fixProfileRelaunch(t, false)
	forbidCmdlineReplay(t, "must not replay an empty argv")
	if mode, armed := prepareRestart(t, "fitness", 4242); armed {
		t.Fatalf("%q", mode)
	}
}

func TestReturnsNoneWhenBothRelaunchPathsFail(t *testing.T) {
	pinRestartArgv(t, slices.Clone(restartFallbackArgv))
	fixProfileRelaunch(t, false)
	swapSeam(t, &updateRestartByCmdline, func(context.Context, int, []string, *string) (bool, error) { return false, nil })
	if mode, armed := prepareRestart(t, "fitness", 4242); armed {
		t.Fatalf("%q", mode)
	}
}

func TestExternalSupervisorStillShortCircuitsBeforeAnyReplay(t *testing.T) {
	pinRestartArgv(t, append(slices.Clone(restartFallbackArgv), "--external-supervisor"))
	forbidProfileRelaunch(t)
	forbidCmdlineReplay(t, "cmdline replay must not be launched")
	if mode, armed := prepareRestart(t, "fitness", 4242); !armed || mode != "external-supervisor" {
		t.Fatalf("%q %v", mode, armed)
	}
}

func TestUpdateHandsExternalSupervisorGatewayBackWithoutWatcher(t *testing.T) {
	pinRestartArgv(t, []string{"python", "-m", "icebath_cli.main", "gateway", "run", "--external-supervisor"})
	forbidProfileRelaunch(t)
	if mode, armed := prepareRestart(t, "work", 1234); !armed || mode != "external-supervisor" {
		t.Fatalf("%q %v", mode, armed)
	}
}

func TestUpdateHandsGeneratedLaunchdInnerArgvBackWithoutWatcher(t *testing.T) {
	pinRestartArgv(t, []string{"/usr/bin/python3", "-m", "icebath_cli.main", "--profile", "work", "gateway", "run", "--replace", "--external-supervisor"})
	forbidProfileRelaunch(t)
	if mode, armed := prepareRestart(t, "work", 1234); !armed || mode != "external-supervisor" {
		t.Fatalf("%q %v", mode, armed)
	}
}

func TestUpdateFollowsWrapperUpgradeOfStalePlistArgv(t *testing.T) {
	stale := []string{"/usr/bin/python3", "-m", "icebath_cli.main", "gateway", "run", "--replace"}
	upgraded := prepareChildCommand(stale, map[string]string{"XPC_SERVICE_NAME": "ai.icebath.gateway-work"})
	if upgraded[len(upgraded)-1] != "--external-supervisor" {
		t.Fatalf("%v", upgraded)
	}
	pinRestartArgv(t, upgraded)
	forbidProfileRelaunch(t)
	if mode, armed := prepareRestart(t, "work", 1234); !armed || mode != "external-supervisor" {
		t.Fatalf("%q %v", mode, armed)
	}
}

func TestUpdateStillUsesDetachedWatcherWithoutSupervisorFlag(t *testing.T) {
	pinRestartArgv(t, []string{"python", "-m", "icebath_cli.main", "gateway", "run", "--replace"})
	type launch struct {
		profile string
		pid     int
	}
	var launched []launch
	swapSeam(t, &updateRestartByProfile, func(_ context.Context, profile string, pid int) (bool, error) {
		launched = append(launched, launch{profile, pid})
		return true, nil
	})
	if mode, armed := prepareRestart(t, "work", 1234); !armed || mode != "detached" {
		t.Fatalf("%q %v", mode, armed)
	}
	if len(launched) != 1 || launched[0] != (launch{"work", 1234}) {
		t.Fatalf("%v", launched)
	}
}

func TestSpawnedRestartWatcherIsNotIdentifiedAsAGateway(t *testing.T) {
	var captured [][]string
	swapSeam(t, &restartWatcherBinary, func() string { return "/opt/icebath/icebath" })
	swapSeam(t, &restartWatcherStart, func(cmd *exec.Cmd) error {
		captured = append(captured, slices.Clone(cmd.Args))
		return nil
	})
	ok, err := LaunchDetachedGatewayRestartByCmdline(context.Background(), 14980, []string{"python", "-m", "icebath_cli.main", "gateway", "run"}, nil)
	if err != nil || !ok || len(captured) != 1 {
		t.Fatalf("ok %v err %v captured %v", ok, err, captured)
	}
	cmdline := strings.Join(captured[0], " ")
	if !strings.Contains(cmdline, "icebath_cli.main gateway run") {
		t.Fatalf("%q", cmdline)
	}
	if sub, found := gatewayCommandSubcommand(cmdline); found {
		t.Fatalf("subcommand %q read off %q", sub, cmdline)
	}
	if LooksLikeGatewayCommandLine(cmdline) || LooksLikeGatewayRuntimeCommandLine(cmdline) {
		t.Fatalf("%q", cmdline)
	}
	if sub, found := GatewaySpawnIntentSubcommand(cmdline); !found || sub != "run" {
		t.Fatalf("spawn intent %q %v", sub, found)
	}
}

func TestRespawnSourceCompiles(t *testing.T) {
	var captured [][]string
	swapSeam(t, &restartWatcherBinary, func() string { return "/opt/icebath/icebath" })
	swapSeam(t, &restartWatcherStart, func(cmd *exec.Cmd) error {
		captured = append(captured, slices.Clone(cmd.Args))
		return nil
	})
	host := false
	ok, err := SpawnGatewayRestartWatcher(context.Background(), 999999, []string{"python", "-m", "icebath_cli.main", "gateway", "run"}, &host, "")
	if err != nil || !ok || len(captured) != 1 {
		t.Fatalf("ok %v err %v captured %v", ok, err, captured)
	}
	argv := captured[0]
	if argv[1] != GatewayRestartWatcherEntry {
		t.Fatalf("%v", argv)
	}
	cwd, overlay, pid, command, err := parseRestartWatcherArgs(argv[2:])
	if err != nil || pid != 999999 || !slices.Equal(command, []string{"python", "-m", "icebath_cli.main", "gateway", "run"}) || overlay == nil {
		t.Fatalf("cwd %q overlay %v pid %d command %v err %v", cwd, overlay, pid, command, err)
	}
}

// Default root plus profiles worker, each a real home, the worker dotenv holding the only token
func restartTwoHomes(t *testing.T) (string, string) {
	t.Helper()
	defaultHome := filepath.Join(t.TempDir(), "default")
	workerHome := filepath.Join(defaultHome, "profiles", "worker")
	if err := os.MkdirAll(workerHome, 0o755); err != nil {
		t.Fatal(err)
	}
	for path, text := range map[string]string{
		filepath.Join(defaultHome, "config.yaml"): "gateway:\n  multiplex_profiles: true\n",
		filepath.Join(defaultHome, ".env"):        "",
		filepath.Join(workerHome, "config.yaml"):  "gateway:\n  multiplex_profiles: true\n",
		filepath.Join(workerHome, ".env"):         "TELEGRAM_BOT_TOKEN=worker-profile-token-456\n",
	} {
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	provider.SetMultiplexActive(false)
	t.Cleanup(func() { provider.SetMultiplexActive(false) })
	return defaultHome, workerHome
}

// Process env of one spawned from the worker profile, its home and its dotenv
func restartInheritWorkerEnv(t *testing.T, workerHome string) {
	t.Helper()
	t.Setenv("ICEBATH_HOME", workerHome)
	t.Setenv("TELEGRAM_BOT_TOKEN", "worker-profile-token-456")
}

func TestSelectorlessReplayOfALiveHostIsHostEvenFromNamedHome(t *testing.T) {
	_, workerHome := restartTwoHomes(t)
	t.Setenv("ICEBATH_GATEWAY_LOCK_DIR", filepath.Join(t.TempDir(), "locks"))
	if err := os.MkdirAll(os.Getenv("ICEBATH_GATEWAY_LOCK_DIR"), 0o755); err != nil {
		t.Fatal(err)
	}
	if o, err := ClaimHostLock(context.Background(), RoleGateway); err != nil || o != HostLockAcquired {
		t.Fatalf("%v %v", o, err)
	}
	t.Cleanup(func() { ReleaseHostLock(RoleGateway) })
	if PublishHostRecord(context.Background(), RoleGateway, HostPublishOptions{Profiles: []string{"default", "worker"}, Home: workerHome}) == nil {
		t.Fatal("fixture must publish a host record")
	}
	if ReadHostRecord(RoleGateway, false) == nil {
		t.Fatal("published record unreadable")
	}
	restartInheritWorkerEnv(t, workerHome)
	if !RestartArgvIsHostGateway(context.Background(), []string{"python", "-m", "icebath_cli.main", "gateway", "run"}) {
		t.Fatal("a live host multiplexer's selector-less argv must replay as the host")
	}
}

func TestReplayStaysProfileScopedWithoutALiveHostRecord(t *testing.T) {
	_, workerHome := restartTwoHomes(t)
	if err := os.WriteFile(filepath.Join(workerHome, "config.yaml"), []byte("gateway: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	restartInheritWorkerEnv(t, workerHome)
	t.Setenv("ICEBATH_GATEWAY_LOCK_DIR", filepath.Join(t.TempDir(), "empty-locks"))
	if RestartArgvIsHostGateway(context.Background(), []string{"python", "-m", "icebath_cli.main", "gateway", "run"}) {
		t.Fatal("without settled proof a named-home process must not mint host authority")
	}
}
