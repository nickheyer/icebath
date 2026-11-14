package selfupdate

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
)

const launchdUID = 501

// Where the locate probe finds a label, or the failure it raises
type launchdLocation struct {
	domain  string
	pid     int
	havePID bool
	err     error
}

func locatedAt(domain string, pid int) launchdLocation {
	return launchdLocation{domain: domain, pid: pid, havePID: true}
}

type launchdFleetConfig struct {
	legacy            []string
	labels            []string
	located           map[string]launchdLocation
	noPlist           bool
	drainResults      map[int]bool
	kickErrors        map[string]error
	waitResults       map[string]bool
	currentUnverified bool
}

type launchdFleetRecorder struct {
	kickstarts, currentRestarts, waits, locates, currentVerifies []string
	drains                                                       []int
}

// Wires a fake launchd fleet through the gateway seams of the restart phase
func launchdFleet(t *testing.T, current string, cfg launchdFleetConfig) *launchdFleetRecorder {
	t.Helper()
	rec := &launchdFleetRecorder{}
	tmp := t.TempDir()
	t.Setenv("ICEBATH_HOME", filepath.Join(tmp, "home"))
	plist := filepath.Join(tmp, current+".plist")
	if !cfg.noPlist {
		if err := os.WriteFile(plist, []byte("<plist/>"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	vsSwap(t, &fleetLaunchdLabel, func(context.Context) (string, error) { return current, nil })
	vsSwap(t, &fleetLaunchdPlistPath, func(context.Context) (string, error) { return plist, nil })
	vsSwap(t, &fleetLaunchdLabelsForInstall, func(context.Context) ([]string, error) { return append([]string{}, cfg.labels...), nil })
	vsSwap(t, &fleetLegacyLaunchdLabels, func(context.Context, map[string]bool) ([]string, error) {
		return append([]string{}, cfg.legacy...), nil
	})
	vsSwap(t, &fleetLocateLaunchdService, func(_ context.Context, label string) (string, int, bool, error) {
		rec.locates = append(rec.locates, label)
		at := cfg.located[label]
		return at.domain, at.pid, at.havePID, at.err
	})
	vsSwap(t, &fleetGracefulRestart, func(pid int, _ float64, _ func()) bool {
		rec.drains = append(rec.drains, pid)
		return cfg.drainResults[pid]
	})
	vsSwap(t, &fleetLaunchdKickstart, func(_ context.Context, label, domain string) error {
		if err := cfg.kickErrors[label]; err != nil {
			return err
		}
		rec.kickstarts = append(rec.kickstarts, domain+"/"+label)
		return nil
	})
	vsSwap(t, &fleetWaitLaunchdPID, func(_ context.Context, label string, _ int, _ bool, _ float64, domain string) (bool, error) {
		rec.waits = append(rec.waits, domain+"/"+label)
		if result, scripted := cfg.waitResults[label]; scripted {
			return result, nil
		}
		return true, nil
	})
	vsSwap(t, &fleetLaunchdRestart, func(context.Context) error {
		rec.currentRestarts = append(rec.currentRestarts, current)
		return nil
	})
	vsSwap(t, &fleetWaitLaunchdSupervised, func(_ context.Context, label string, _ int, _ bool) (bool, error) {
		rec.currentVerifies = append(rec.currentVerifies, label)
		return !cfg.currentUnverified, nil
	})
	return rec
}

func runLaunchdRestart(t *testing.T, w io.Writer) ([]string, []string) {
	t.Helper()
	restarted, failed := []string{}, []string{}
	if err := restartMacosLaunchdGateways(context.Background(), w, &restarted, &failed, 0.0, false, nil); err != nil {
		t.Fatal(err)
	}
	return restarted, failed
}

func gui(label string) string { return fmt.Sprintf("gui/%d/%s", launchdUID, label) }

var (
	guiDomain  = fmt.Sprintf("gui/%d", launchdUID)
	userDomain = fmt.Sprintf("user/%d", launchdUID)
)

func wantLaunchd(t *testing.T, what string, got, want []string) {
	t.Helper()
	if len(got) == 0 && len(want) == 0 {
		return
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s = %v, want %v", what, got, want)
	}
}

func TestRestartMacosLaunchdGatewaysCurrentDelegatesAndSiblingsKickstartInOwnDomains(t *testing.T) {
	current := "ai.icebath.gateway-merit-ops"
	rec := launchdFleet(t, current, launchdFleetConfig{
		labels: []string{"ai.icebath.gateway", current, "ai.icebath.gateway-user-scoped"},
		located: map[string]launchdLocation{"ai.icebath.gateway": locatedAt(guiDomain, 100), current: locatedAt(guiDomain, 200),
			"ai.icebath.gateway-user-scoped": locatedAt(userDomain, 300)},
	})
	restarted, failed := runLaunchdRestart(t, io.Discard)
	wantLaunchd(t, "current restarts", rec.currentRestarts, []string{current})
	siblings := []string{gui("ai.icebath.gateway"), userDomain + "/ai.icebath.gateway-user-scoped"}
	wantLaunchd(t, "kickstarts", rec.kickstarts, siblings)
	wantLaunchd(t, "waits", rec.waits, siblings)
	wantLaunchd(t, "restarted", restarted, []string{current, "ai.icebath.gateway", "ai.icebath.gateway-user-scoped"})
	wantLaunchd(t, "failed", failed, nil)
	drains := append([]int{}, rec.drains...)
	sort.Ints(drains)
	if !reflect.DeepEqual(drains, []int{100, 300}) {
		t.Fatalf("drains = %v", rec.drains)
	}
}

func TestRestartMacosLaunchdGatewaysCurrentProfileWithoutPlistMakesNoLaunchctlCalls(t *testing.T) {
	current := "ai.icebath.gateway"
	rec := launchdFleet(t, current, launchdFleetConfig{
		labels: []string{current, "ai.icebath.gateway-a"}, noPlist: true,
		located: map[string]launchdLocation{"ai.icebath.gateway-a": locatedAt(guiDomain, 200)},
	})
	restarted, failed := runLaunchdRestart(t, io.Discard)
	wantLaunchd(t, "current restarts", rec.currentRestarts, nil)
	if slices.Contains(rec.locates, current) {
		t.Fatalf("the current label was located: %v", rec.locates)
	}
	wantLaunchd(t, "restarted", restarted, []string{"ai.icebath.gateway-a"})
	wantLaunchd(t, "failed", failed, nil)
}

func TestRestartMacosLaunchdGatewaysCurrentProfileRegisteredButUnlocatableStillRestarts(t *testing.T) {
	current := "ai.icebath.gateway"
	rec := launchdFleet(t, current, launchdFleetConfig{labels: []string{current}, located: map[string]launchdLocation{current: {}}})
	restarted, failed := runLaunchdRestart(t, io.Discard)
	wantLaunchd(t, "current restarts", rec.currentRestarts, []string{current})
	if slices.Contains(rec.locates, current) {
		t.Fatalf("the current label was located: %v", rec.locates)
	}
	wantLaunchd(t, "restarted", restarted, []string{current})
	wantLaunchd(t, "failed", failed, nil)
}

func TestRestartMacosLaunchdGatewaysUnbootstrappedSiblingIsSkippedNotFailed(t *testing.T) {
	rec := launchdFleet(t, "ai.icebath.gateway", launchdFleetConfig{
		labels:  []string{"ai.icebath.gateway", "ai.icebath.gateway-idle"},
		located: map[string]launchdLocation{"ai.icebath.gateway": locatedAt(guiDomain, 100), "ai.icebath.gateway-idle": {}},
	})
	restarted, failed := runLaunchdRestart(t, io.Discard)
	wantLaunchd(t, "kickstarts", rec.kickstarts, nil)
	wantLaunchd(t, "restarted", restarted, []string{"ai.icebath.gateway"})
	wantLaunchd(t, "failed", failed, nil)
}

func TestRestartMacosLaunchdGatewaysLoadedButNotRunningSiblingIsKickstarted(t *testing.T) {
	rec := launchdFleet(t, "ai.icebath.gateway", launchdFleetConfig{
		labels: []string{"ai.icebath.gateway", "ai.icebath.gateway-dormant"},
		located: map[string]launchdLocation{"ai.icebath.gateway": locatedAt(guiDomain, 100),
			"ai.icebath.gateway-dormant": {domain: guiDomain}},
	})
	restarted, failed := runLaunchdRestart(t, io.Discard)
	if len(rec.drains) != 0 {
		t.Fatalf("drains = %v", rec.drains)
	}
	wantLaunchd(t, "kickstarts", rec.kickstarts, []string{gui("ai.icebath.gateway-dormant")})
	wantLaunchd(t, "restarted", restarted, []string{"ai.icebath.gateway", "ai.icebath.gateway-dormant"})
	wantLaunchd(t, "failed", failed, nil)
}

func TestRestartMacosLaunchdGatewaysGracefulDrainWithKeepaliveRespawnSkipsKickstart(t *testing.T) {
	rec := launchdFleet(t, "ai.icebath.gateway", launchdFleetConfig{
		labels:       []string{"ai.icebath.gateway", "ai.icebath.gateway-a"},
		located:      map[string]launchdLocation{"ai.icebath.gateway": locatedAt(guiDomain, 100), "ai.icebath.gateway-a": locatedAt(guiDomain, 200)},
		drainResults: map[int]bool{200: true},
	})
	restarted, failed := runLaunchdRestart(t, io.Discard)
	if !reflect.DeepEqual(rec.drains, []int{200}) {
		t.Fatalf("drains = %v", rec.drains)
	}
	wantLaunchd(t, "kickstarts", rec.kickstarts, nil)
	wantLaunchd(t, "waits", rec.waits, []string{gui("ai.icebath.gateway-a")})
	wantLaunchd(t, "restarted", restarted, []string{"ai.icebath.gateway", "ai.icebath.gateway-a"})
	wantLaunchd(t, "failed", failed, nil)
}

func threeLabelFleet(middle string) ([]string, map[string]launchdLocation) {
	last := "ai.icebath.gateway-after"
	if middle == "ai.icebath.gateway-bad" {
		last = "ai.icebath.gateway-good"
	}
	return []string{"ai.icebath.gateway", middle, last}, map[string]launchdLocation{
		"ai.icebath.gateway": locatedAt(guiDomain, 100), middle: locatedAt(guiDomain, 200), last: locatedAt(guiDomain, 300)}
}

func TestRestartMacosLaunchdGatewaysKickstartFailureIsRecordedAndRestContinue(t *testing.T) {
	labels, located := threeLabelFleet("ai.icebath.gateway-bad")
	rec := launchdFleet(t, "ai.icebath.gateway", launchdFleetConfig{labels: labels, located: located,
		kickErrors: map[string]error{"ai.icebath.gateway-bad": &gateway.CalledProcessError{Cmd: []string{"launchctl", "kickstart"}, ReturnCode: 5}}})
	restarted, failed := runLaunchdRestart(t, io.Discard)
	wantLaunchd(t, "failed", failed, []string{"ai.icebath.gateway-bad"})
	wantLaunchd(t, "kickstarts", rec.kickstarts, []string{gui("ai.icebath.gateway-good")})
	wantLaunchd(t, "restarted", restarted, []string{"ai.icebath.gateway", "ai.icebath.gateway-good"})
}

func TestRestartMacosLaunchdGatewaysTimeoutDuringDiscoveryIsFailedAndRestContinue(t *testing.T) {
	labels, located := threeLabelFleet("ai.icebath.gateway-wedged")
	located["ai.icebath.gateway-wedged"] = launchdLocation{err: &pysubprocess.TimeoutExpired{Cmd: []string{"launchctl", "print"}, Timeout: 5}}
	rec := launchdFleet(t, "ai.icebath.gateway", launchdFleetConfig{labels: labels, located: located})
	restarted, failed := runLaunchdRestart(t, io.Discard)
	wantLaunchd(t, "failed", failed, []string{"ai.icebath.gateway-wedged"})
	wantLaunchd(t, "kickstarts", rec.kickstarts, []string{gui("ai.icebath.gateway-after")})
	wantLaunchd(t, "restarted", restarted, []string{"ai.icebath.gateway", "ai.icebath.gateway-after"})
}

func TestRestartMacosLaunchdGatewaysTimeoutDuringKickstartIsFailedAndRestContinue(t *testing.T) {
	labels, located := threeLabelFleet("ai.icebath.gateway-wedged")
	rec := launchdFleet(t, "ai.icebath.gateway", launchdFleetConfig{labels: labels, located: located,
		kickErrors: map[string]error{"ai.icebath.gateway-wedged": fmt.Errorf("%w: launchctl kickstart", gateway.ErrCommandTimeout)}})
	restarted, failed := runLaunchdRestart(t, io.Discard)
	wantLaunchd(t, "failed", failed, []string{"ai.icebath.gateway-wedged"})
	wantLaunchd(t, "kickstarts", rec.kickstarts, []string{gui("ai.icebath.gateway-after")})
	wantLaunchd(t, "restarted", restarted, []string{"ai.icebath.gateway", "ai.icebath.gateway-after"})
}

func TestRestartMacosLaunchdGatewaysSiblingThatNeverComesBackIsFailed(t *testing.T) {
	launchdFleet(t, "ai.icebath.gateway", launchdFleetConfig{
		labels:      []string{"ai.icebath.gateway", "ai.icebath.gateway-zombie"},
		located:     map[string]launchdLocation{"ai.icebath.gateway": locatedAt(guiDomain, 100), "ai.icebath.gateway-zombie": locatedAt(guiDomain, 200)},
		waitResults: map[string]bool{"ai.icebath.gateway-zombie": false},
	})
	restarted, failed := runLaunchdRestart(t, io.Discard)
	wantLaunchd(t, "restarted", restarted, []string{"ai.icebath.gateway"})
	wantLaunchd(t, "failed", failed, []string{"ai.icebath.gateway-zombie"})
}

func TestRestartMacosLaunchdGatewaysLegacyUnitsLegacyLabelledSiblingIsRestartedLikeAnyOther(t *testing.T) {
	current, legacy := "ai.icebath.gateway", "ai.icebath.gateway-398559f7"
	rec := launchdFleet(t, current, launchdFleetConfig{
		labels: []string{current, "ai.icebath.gateway-gopod"}, legacy: []string{legacy},
		located: map[string]launchdLocation{current: locatedAt(guiDomain, 100), "ai.icebath.gateway-gopod": locatedAt(guiDomain, 200),
			legacy: locatedAt(userDomain, 300)},
	})
	var out bytes.Buffer
	restarted, failed := runLaunchdRestart(t, &out)
	wantLaunchd(t, "kickstarts", rec.kickstarts, []string{gui("ai.icebath.gateway-gopod"), userDomain + "/" + legacy})
	wantLaunchd(t, "restarted", restarted, []string{current, "ai.icebath.gateway-gopod", legacy})
	wantLaunchd(t, "failed", failed, nil)
	drains := append([]int{}, rec.drains...)
	sort.Ints(drains)
	if !reflect.DeepEqual(drains, []int{200, 300}) || !strings.Contains(out.String(), legacy) {
		t.Fatalf("drains = %v out = %q", rec.drains, out.String())
	}
}

func TestIncompleteWarningOnMacosLaunchdLabelsGetBootstrapHint(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("launchd arm of the warning")
	}
	var out bytes.Buffer
	warnIncompleteGatewayFleetRestart(&out, []string{"ai.icebath.gateway-merit-ops"})
	if !strings.Contains(out.String(), "launchctl bootstrap") || strings.Contains(out.String(), "systemctl") {
		t.Fatalf("out = %q", out.String())
	}
}

func TestPendingLaunchdRequiresCompleteSupervision(t *testing.T) {
	for _, failure := range []string{"listing", "restart", "inactive", "unloaded", ""} {
		t.Run("failure="+failure, func(t *testing.T) {
			tmp := t.TempDir()
			t.Setenv("ICEBATH_HOME", filepath.Join(tmp, "home"))
			current, sibling := "ai.icebath.gateway", "ai.icebath.gateway-two"
			if err := os.WriteFile(filepath.Join(tmp, sibling+".plist"), nil, 0o644); err != nil {
				t.Fatal(err)
			}
			vsSwap(t, &fleetLaunchdLabel, func(context.Context) (string, error) { return current, nil })
			vsSwap(t, &fleetLaunchdPlistPath, func(context.Context) (string, error) { return filepath.Join(tmp, current+".plist"), nil })
			vsSwap(t, &fleetLaunchdLabelsForInstall, func(context.Context) ([]string, error) { return []string{current, sibling}, nil })
			vsSwap(t, &fleetRestartOwnLaunchd, func(context.Context, io.Writer, bool, map[int]bool) ([]string, []string, error) {
				return []string{}, []string{}, nil
			})
			vsSwap(t, &fleetLocateLaunchdService, func(context.Context, string) (string, int, bool, error) {
				if failure == "unloaded" {
					return "", 0, false, nil
				}
				return "gui/501", 0, false, nil
			})
			vsSwap(t, &fleetWaitLaunchdPID, func(context.Context, string, int, bool, float64, string) (bool, error) {
				return failure != "inactive", nil
			})
			vsSwap(t, &fleetLaunchdKickstart, func(context.Context, string, string) error {
				if failure == "restart" {
					return &gateway.CalledProcessError{Cmd: []string{"launchctl"}, ReturnCode: 1}
				}
				return nil
			})
			vsSwap(t, &fleetRunCommand, func(_ context.Context, cmd []string, _ float64) (*pysubprocess.Completed, error) {
				code := 0
				if failure == "listing" {
					code = 1
				}
				return &pysubprocess.Completed{Args: cmd, Returncode: code}, nil
			})
			restarted, failed := []string{}, []string{}
			if err := restartMacosLaunchdGateways(context.Background(), io.Discard, &restarted, &failed, 0, true, nil); err != nil {
				t.Fatal(err)
			}
			if (len(failed) > 0) != (failure != "") || slices.Contains(restarted, sibling) != (failure == "") {
				t.Fatalf("restarted=%v failed=%v", restarted, failed)
			}
		})
	}
}
