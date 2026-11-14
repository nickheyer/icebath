package gateway

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// Installs launchctl and lifecycle doubles recording every launchctl argv
func launchdSeam(t *testing.T, plistBody string) (*[][]string, *bytes.Buffer, string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("ICEBATH_HOME", filepath.Join(dir, "home"))
	plistPath := filepath.Join(dir, "ai.icebath.gateway.plist")
	if err := os.WriteFile(plistPath, []byte(plistBody), 0o644); err != nil {
		t.Fatal(err)
	}
	var calls [][]string
	out := &bytes.Buffer{}
	saved := []func(){}
	save := func(restore func()) { saved = append(saved, restore) }
	oldRun, oldLabel, oldDomain, oldPath := launchctlRun, launchdLabelOf, launchdDomainOf, launchdPlistPathOf
	oldPid, oldNoCleanup, oldSelf, oldGraceful := lifecycleRunningPid, lifecycleRunningPidNoCleanup, launchdRequestSelfRestart, lifecycleGracefulRestart
	oldWaitPID, oldOut, oldPrepare := launchdWaitForServicePID, lifecycleOut, launchdPrepareServiceLauncher
	save(func() {
		launchctlRun, launchdLabelOf, launchdDomainOf, launchdPlistPathOf = oldRun, oldLabel, oldDomain, oldPath
		lifecycleRunningPid, lifecycleRunningPidNoCleanup, launchdRequestSelfRestart, lifecycleGracefulRestart = oldPid, oldNoCleanup, oldSelf, oldGraceful
		launchdWaitForServicePID, lifecycleOut, launchdPrepareServiceLauncher = oldWaitPID, oldOut, oldPrepare
	})
	t.Cleanup(func() {
		for _, restore := range saved {
			restore()
		}
	})
	launchdLabelOf = func(context.Context) (string, error) { return "ai.icebath.gateway", nil }
	launchdDomainOf = func(context.Context) (string, error) { return "gui/501", nil }
	launchdPlistPathOf = func(context.Context) (string, error) { return plistPath, nil }
	lifecycleRunningPid = func() (int, bool) { return 0, false }
	lifecycleRunningPidNoCleanup = func() (int, bool) { return 0, false }
	launchdRequestSelfRestart = func(int) bool { return false }
	lifecycleGracefulRestart = func(int, float64, func()) bool { return false }
	launchdWaitForServicePID = func(context.Context, string, int, bool, float64, string) (bool, error) { return false, nil }
	launchdPrepareServiceLauncher = func(context.Context, bool, string) error { return nil }
	lifecycleOut = out
	launchctlRun = func(_ context.Context, timeout time.Duration, _ bool, argv ...string) (CommandResult, error) {
		calls = append(calls, argv)
		if argv[0] == "launchctl" && argv[1] == "kickstart" && slices.Contains(argv, "-k") {
			return CommandResult{}, fmt.Errorf("%w: %q after %v", ErrCommandTimeout, argv, 90*time.Second)
		}
		return CommandResult{}, nil
	}
	return &calls, out, plistPath
}

func TestStalePlistIsRefreshedBeforeAnyKickstart(t *testing.T) {
	calls, _, _ := launchdSeam(t, "<plist>whatever</plist>")
	refreshed := 0
	old := launchdRefreshPlist
	t.Cleanup(func() { launchdRefreshPlist = old })
	launchdRefreshPlist = func(context.Context) (bool, error) { refreshed++; return true, nil }
	err := LaunchdRestart(context.Background())
	if err == nil || !strings.Contains(err.Error(), ErrCommandTimeout.Error()) {
		t.Fatalf("want the kickstart timeout to escape, got %v", err)
	}
	if refreshed != 1 {
		t.Fatalf("refresh must run before the restart decision, ran %d times", refreshed)
	}
	var kickstarts [][]string
	for _, c := range *calls {
		if len(c) > 1 && c[0] == "launchctl" && c[1] == "kickstart" {
			kickstarts = append(kickstarts, c)
		}
	}
	if len(kickstarts) == 0 {
		t.Fatal("a current definition still needs the kickstart relaunch")
	}
}

func TestFailedRefreshRoutesToBoundedBootstrapNotThe90sKickstart(t *testing.T) {
	calls, out, _ := launchdSeam(t, "<plist>whatever</plist>")
	old := launchdRefreshPlist
	t.Cleanup(func() { launchdRefreshPlist = old })
	launchdRefreshPlist = func(context.Context) (bool, error) { return false, nil }
	if err := LaunchdRestart(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	bootstraps := 0
	for _, c := range *calls {
		if slices.Contains(c, "kickstart") && slices.Contains(c, "-k") {
			t.Fatalf("kickstart -k after a failed refresh is the exact 90s hang this guards against: %q", c)
		}
		if len(c) > 1 && c[0] == "launchctl" && c[1] == "bootstrap" {
			bootstraps++
		}
	}
	if bootstraps == 0 {
		t.Fatalf("the bounded bootstrap revival must run instead: %q", *calls)
	}
}

func TestRefreshReportsRegistrationOutcome(t *testing.T) {
	for _, registered := range []bool{false, true} {
		_, out, _ := launchdSeam(t, "<old/>")
		oldCurrent, oldGen, oldRefuse, oldRetry := launchdPlistIsCurrentFn, launchdGeneratePlist, launchdRefuseTempHome, launchdRetryBootstrap
		launchdPlistIsCurrentFn = func(context.Context) (bool, error) { return false, nil }
		launchdGeneratePlist = func(context.Context) (string, error) { return "<new/>", nil }
		launchdRefuseTempHome = func(string, string) bool { return false }
		launchdRetryBootstrap = func(context.Context, string, string, string, time.Time) (bool, error) { return registered, nil }
		got, err := RefreshLaunchdPlistIfNeeded(context.Background())
		launchdPlistIsCurrentFn, launchdGeneratePlist, launchdRefuseTempHome, launchdRetryBootstrap = oldCurrent, oldGen, oldRefuse, oldRetry
		if err != nil || got != registered {
			t.Fatalf("registered=%v got %v %v", registered, got, err)
		}
		if strings.Contains(out.String(), "Updated") != registered {
			t.Fatalf("registered=%v output %q", registered, out.String())
		}
	}
}
