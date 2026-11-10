package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const lifecycleHostPID = 4242

// Root home with a worker profile and multiplexing config like the homes fixture, locks inside it
func multiplexHomes(t *testing.T) (string, string) {
	t.Helper()
	tmp := t.TempDir()
	root := filepath.Join(tmp, ".icebath")
	secondary := filepath.Join(root, "profiles", "worker")
	if err := os.MkdirAll(secondary, 0o755); err != nil {
		t.Fatal(err)
	}
	writeLifecycleFile(t, filepath.Join(secondary, "config.yaml"), "model: {default: worker-model}\n")
	writeLifecycleFile(t, filepath.Join(root, "config.yaml"), "gateway: {multiplex_profiles: true}\n")
	t.Setenv("HOME", tmp)
	t.Setenv("ICEBATH_HOME", root)
	t.Setenv("ICEBATH_GATEWAY_LOCK_DIR", filepath.Join(tmp, ".local", "state", "icebath", "gateway-locks"))
	invalidateHostGatewayCache()
	t.Cleanup(invalidateHostGatewayCache)
	return root, secondary
}

// Homes fixture of the host stop refusal cases, service and process table discovery answering nothing
func stopRefusalHomes(t *testing.T) (string, string) {
	t.Helper()
	root, secondary := multiplexHomes(t)
	noDiscoveredPids(t)
	return root, secondary
}

func noDiscoveredPids(t *testing.T) {
	t.Helper()
	swapSetup(t, &servicePIDsHook, func(context.Context, bool) ([]int, error) { return nil, nil })
	swapSetup(t, &scanGatewayPIDsHook, func(context.Context, map[int]bool, bool, bool) ([]int, error) { return nil, nil })
}

func writeLifecycleFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Live host record launched from h whose control socket answers only when asked, returning the probe waits
func liveHost(t *testing.T, h string, socketAnswers bool) *[]time.Duration {
	t.Helper()
	record := &HostRecord{PID: lifecycleHostPID, Home: h, Role: roleGateway}
	swapSetup(t, &hostReadRecord, func(string, bool) *HostRecord { return record })
	swapSetup(t, &hostRecordLive, func(*HostRecord) bool { return true })
	dials := &[]time.Duration{}
	swapSetup(t, &hostProbe, func(wait time.Duration) *HostGateway {
		*dials = append(*dials, wait)
		return &HostGateway{PID: lifecycleHostPID, Home: h, Profiles: []string{"default", "worker"}, ServedKnown: socketAnswers}
	})
	runningPid(t, lifecycleHostPID, nil)
	return dials
}

func probeAnswers(t *testing.T, owner *HostGateway) {
	t.Helper()
	swapSetup(t, &hostProbe, func(time.Duration) *HostGateway { return owner })
}

func processHomes(t *testing.T, homeOf func(pid int) (string, bool, error)) {
	t.Helper()
	swapSetup(t, &homeForPIDHook, func(_ context.Context, pid int, _ bool) (string, bool, error) { return homeOf(pid) })
}

func runningPidSequence(t *testing.T, pids ...int) {
	t.Helper()
	next := 0
	swapSetup(t, &statusGetRunningPid, func(string, bool) (int, bool, error) {
		pid := pids[next]
		next++
		return pid, true, nil
	})
}

func forbidStopEffects(t *testing.T, why string) {
	t.Helper()
	swapSetup(t, &stopKill, func(int, bool) error { t.Fatal(why + " was signalled"); return nil })
	swapSetup(t, &stopReapOrphans, func(context.Context, map[int]bool, float64) (bool, error) {
		t.Fatal(why + " reached the reaper")
		return false, nil
	})
}

func ownGatewayPid(t *testing.T) (int, bool) {
	t.Helper()
	return ownedGatewayPID(context.Background(), "", processHermesHome())
}

func gatewayPids(t *testing.T) []int {
	t.Helper()
	pids, err := FindGatewayPIDsFor(context.Background(), nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if pids == nil {
		pids = []int{}
	}
	return pids
}

func stopsGateway(t *testing.T) bool {
	t.Helper()
	stopped, err := StopProfileGateway(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return stopped
}

func TestStopRefusesTheServingHostWhenTheSocketDoesNotAnswer(t *testing.T) {
	root, secondary := stopRefusalHomes(t)
	liveHost(t, root, false)
	t.Setenv("ICEBATH_HOME", secondary)
	swapSetup(t, &stopMarkPlannedStop, func(context.Context, int, bool) { t.Fatal("planned-stop marker written for the host pid") })
	swapSetup(t, &stopKill, func(int, bool) error { t.Fatal("stop_profile_gateway SIGTERMed the host pid"); return nil })
	swapSetup(t, &stopReapOrphans, func(context.Context, map[int]bool, float64) (bool, error) {
		t.Fatal("host pid reached the reaper")
		return false, nil
	})
	if got := gatewayPids(t); len(got) != 0 {
		t.Fatalf("find_gateway_pids = %v", got)
	}
	if stopsGateway(t) {
		t.Fatal("stop_profile_gateway reported a stop")
	}
	swapSetup(t, &statusTerminatePid, func(int, bool, any) error { t.Fatal("uninstall kill reached the host pid"); return nil })
	killed, err := KillGatewayProcesses(context.Background(), io.Discard, false, nil, false)
	if err != nil || killed != 0 {
		t.Fatalf("kill_gateway_processes = %d, %v", killed, err)
	}
	var out bytes.Buffer
	if code := ReportProfileStopFailure(context.Background(), &out); code != 1 {
		t.Fatalf("exit code %d", code)
	}
	for _, want := range []string{"host gateway serving this profile", "gateway stop --all"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q in %q", want, out.String())
		}
	}
}

func TestStopFromTheHostsOwnHomeStillStopsIt(t *testing.T) {
	root, _ := stopRefusalHomes(t)
	liveHost(t, root, true)
	var killed []int
	swapSetup(t, &stopMarkPlannedStop, func(context.Context, int, bool) {})
	swapSetup(t, &stopKill, func(pid int, _ bool) error { killed = append(killed, pid); return nil })
	swapSetup(t, &stopRemovePidFile, func() {})
	swapSetup(t, &statusPidExists, func(int) bool { return false })
	swapSetup(t, &stopReapOrphans, func(context.Context, map[int]bool, float64) (bool, error) { return false, nil })
	if !stopsGateway(t) {
		t.Fatal("the launching home did not stop its own gateway")
	}
	if !reflect.DeepEqual(killed, []int{lifecycleHostPID}) {
		t.Fatalf("killed = %v", killed)
	}
}

func TestStickyProfileForceGatewayStaysStoppable(t *testing.T) {
	root, secondary := stopRefusalHomes(t)
	child := exec.Command("sleep", "60")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.Process.Kill(); _ = child.Wait() })
	pid := child.Process.Pid
	writeLifecycleFile(t, filepath.Join(root, "active_profile"), "default\n")
	record, err := json.Marshal(map[string]any{
		"pid": pid, "kind": gatewayKind, "argv": []string{"icebath", "gateway", "run", "--force"},
		"start_time": statusProcessStartTime(pid), "hermes_home": secondary,
	})
	if err != nil {
		t.Fatal(err)
	}
	writeLifecycleFile(t, filepath.Join(secondary, "gateway.pid"), string(record))
	t.Setenv("ICEBATH_HOME", secondary)
	swapSetup(t, &hostReadRecord, func(string, bool) *HostRecord { return nil })
	swapSetup(t, &pidEnvironHook, func(int) (map[string]string, error) {
		return map[string]string{"ICEBATH_HOME": root, "HOME": filepath.Dir(root)}, nil
	})
	runningPid(t, pid, nil)
	if h, ok, err := HermesHomeForPID(context.Background(), pid, true); err != nil || ok {
		t.Fatalf("launch identity home = %q %v %v", h, ok, err)
	}
	if got, ok := ownGatewayPid(t); !ok || got != pid {
		t.Fatalf("owned gateway pid = %d %v", got, ok)
	}
	if got := gatewayPids(t); !reflect.DeepEqual(got, []int{pid}) {
		t.Fatalf("find_gateway_pids = %v", got)
	}
	swapSetup(t, &statusProcessStartTime, func(int) any { return int64(-1) })
	if got, ok := ownGatewayPid(t); ok {
		t.Fatalf("recycled pid owned as %d", got)
	}
}

func TestOnlyTheHostsPidDialsItsControlSocket(t *testing.T) {
	root, secondary := stopRefusalHomes(t)
	dials := liveHost(t, root, true)
	t.Setenv("ICEBATH_HOME", secondary)
	runningPid(t, 5151, nil)
	processHomes(t, func(int) (string, bool, error) { return secondary, true, nil })
	if got := gatewayPids(t); !reflect.DeepEqual(got, []int{5151}) {
		t.Fatalf("find_gateway_pids = %v", got)
	}
	runningPid(t, lifecycleHostPID, nil)
	if got := gatewayPids(t); len(got) != 0 {
		t.Fatalf("find_gateway_pids = %v", got)
	}
	if len(*dials) != 0 {
		t.Fatalf("dials = %v", *dials)
	}
}

func TestProcessDiscoveryExcludesHostEvenAfterUnserve(t *testing.T) {
	for _, served := range []bool{true, false} {
		t.Run(fmt.Sprint(served), func(t *testing.T) {
			root, secondary := multiplexHomes(t)
			profiles := []string{"default"}
			if served {
				profiles = append(profiles, "worker")
			}
			owner := &HostGateway{PID: lifecycleHostPID, Home: root, ServedKnown: true, Profiles: profiles}
			t.Setenv("ICEBATH_HOME", secondary)
			probeAnswers(t, owner)
			processHomes(t, func(pid int) (string, bool, error) {
				if pid == lifecycleHostPID {
					return root, true, nil
				}
				return secondary, true, nil
			})
			noDiscoveredPids(t)
			runningPid(t, owner.PID, nil)
			if got := gatewayPids(t); len(got) != 0 {
				t.Fatalf("find_gateway_pids = %v", got)
			}
			if stopsGateway(t) {
				t.Fatal("stop_profile_gateway stopped the host")
			}
			runningPid(t, 4343, nil)
			if got := gatewayPids(t); !reflect.DeepEqual(got, []int{4343}) {
				t.Fatalf("find_gateway_pids = %v", got)
			}
			t.Setenv("ICEBATH_HOME", root)
			runningPid(t, owner.PID, nil)
			if got := gatewayPids(t); !reflect.DeepEqual(got, []int{owner.PID}) {
				t.Fatalf("find_gateway_pids = %v", got)
			}
		})
	}
}

// Memoized probe result as host_attach._cached_probe holds it
func seedHostProbeCache(result *HostGateway) {
	hostProbeCache.mu.Lock()
	hostProbeCache.set, hostProbeCache.at, hostProbeCache.result = true, time.Now(), result
	hostProbeCache.mu.Unlock()
}

func TestStopNeverOwnsSharedPidFromAnAbsentOrOldProbe(t *testing.T) {
	for _, probe := range []string{"cached_absent", "failure", "switched"} {
		t.Run(probe, func(t *testing.T) {
			root, secondary := multiplexHomes(t)
			t.Setenv("ICEBATH_HOME", secondary)
			runningPid(t, lifecycleHostPID, nil)
			noDiscoveredPids(t)
			processHomes(t, func(int) (string, bool, error) { return root, true, nil })
			old := &HostGateway{PID: 4141, Home: root, Profiles: []string{"default", "worker"}, ServedKnown: true}
			current := &HostGateway{PID: lifecycleHostPID, Home: root, Profiles: []string{"default", "worker"}, ServedKnown: true}
			if probe == "switched" {
				seedHostProbeCache(old)
			} else {
				seedHostProbeCache(nil)
			}
			swapSetup(t, &hostProbe, func(time.Duration) *HostGateway {
				if probe == "failure" {
					return nil
				}
				return current
			})
			swapSetup(t, &stopKill, func(int, bool) error { t.Fatal("shared host was signalled"); return nil })
			if got := gatewayPids(t); len(got) != 0 {
				t.Fatalf("find_gateway_pids = %v", got)
			}
			if stopsGateway(t) {
				t.Fatal("stop_profile_gateway stopped the shared host")
			}
		})
	}
}

func TestAbsentHostRequiresPositiveProcessOwnership(t *testing.T) {
	for _, processHome := range []string{"none", "foreign", "error"} {
		t.Run(processHome, func(t *testing.T) {
			root, secondary := multiplexHomes(t)
			t.Setenv("ICEBATH_HOME", secondary)
			runningPid(t, lifecycleHostPID, nil)
			probeAnswers(t, nil)
			processHomes(t, func(int) (string, bool, error) {
				switch processHome {
				case "error":
					return "", false, fmt.Errorf("process inaccessible: %w", os.ErrPermission)
				case "foreign":
					return root, true, nil
				}
				return "", false, nil
			})
			forbidStopEffects(t, "unproven PID")
			if stopsGateway(t) {
				t.Fatal("unproven PID was stopped")
			}
		})
	}
}

func TestOwnerSwitchDuringProbeIsRejected(t *testing.T) {
	_, secondary := multiplexHomes(t)
	t.Setenv("ICEBATH_HOME", secondary)
	runningPidSequence(t, 4343, lifecycleHostPID)
	probeAnswers(t, nil)
	processHomes(t, func(int) (string, bool, error) { return secondary, true, nil })
	if got, ok := ownGatewayPid(t); ok {
		t.Fatalf("owner switch accepted as %d", got)
	}
}

func TestStickyProfileCannotReassignRunningHost(t *testing.T) {
	for _, rendezvous := range []string{"absent", "unreadable"} {
		t.Run(rendezvous, func(t *testing.T) {
			root, secondary := multiplexHomes(t)
			t.Setenv("ICEBATH_HOME", secondary)
			swapSetup(t, &pidEnvironHook, func(int) (map[string]string, error) {
				return map[string]string{"ICEBATH_HOME": root}, nil
			})
			swapSetup(t, &pidCmdlineHook, func(context.Context, int) ([]string, bool) {
				return []string{"icebath", "gateway", "run"}, true
			})
			writeLifecycleFile(t, filepath.Join(root, "active_profile"), "worker\n")
			if h, ok, err := HermesHomeForPID(context.Background(), lifecycleHostPID, false); err != nil || !ok || h != secondary {
				t.Fatalf("sticky home = %q %v %v", h, ok, err)
			}
			probeAnswers(t, nil)
			runningPid(t, lifecycleHostPID, nil)
			forbidStopEffects(t, "shared host")
			if got, ok := ownGatewayPid(t); ok {
				t.Fatalf("sticky selection owned %d", got)
			}
			if stopsGateway(t) {
				t.Fatal("stop_profile_gateway stopped the shared host")
			}
		})
	}
}

func TestLaunchIdentityDoesNotFollowStickySelection(t *testing.T) {
	for _, identity := range []string{"explicit", "profile_home", "supervised", "ambiguous"} {
		t.Run(identity, func(t *testing.T) {
			root, secondary := multiplexHomes(t)
			writeLifecycleFile(t, filepath.Join(root, "active_profile"), "other\n")
			env := map[string]string{"ICEBATH_HOME": root}
			argv := []string{"icebath", "gateway", "run"}
			target := secondary
			switch identity {
			case "explicit":
				argv = []string{"icebath", "-p", "worker", "gateway", "run"}
			case "profile_home":
				env["ICEBATH_HOME"] = secondary
			case "supervised":
				env["ICEBATH_GATEWAY_EXTERNAL_SUPERVISOR"] = "1"
				target = root
			}
			t.Setenv("ICEBATH_HOME", target)
			swapSetup(t, &pidEnvironHook, func(int) (map[string]string, error) { return env, nil })
			swapSetup(t, &pidCmdlineHook, func(context.Context, int) ([]string, bool) { return argv, true })
			probeAnswers(t, nil)
			runningPid(t, lifecycleHostPID, nil)
			got, ok := ownGatewayPid(t)
			if identity == "ambiguous" {
				if ok {
					t.Fatalf("ambiguous launch owned %d", got)
				}
				return
			}
			if !ok || got != lifecycleHostPID {
				t.Fatalf("owned gateway pid = %d %v", got, ok)
			}
		})
	}
}

func TestSameHomeRecordRequiresIdentityAndStablePid(t *testing.T) {
	for _, tc := range []struct{ verified, changed bool }{{true, false}, {true, true}, {false, false}} {
		t.Run(fmt.Sprintf("%v-%v", tc.verified, tc.changed), func(t *testing.T) {
			root, _ := multiplexHomes(t)
			t.Setenv("ICEBATH_HOME", root)
			probeAnswers(t, &HostGateway{PID: lifecycleHostPID, Home: root, Profiles: []string{"default"}, ServedKnown: tc.verified})
			swapSetup(t, &hostReadRecord, func(string, bool) *HostRecord { return &HostRecord{PID: lifecycleHostPID, Home: root} })
			swapSetup(t, &hostRecordLive, func(*HostRecord) bool { return true })
			processHomes(t, func(int) (string, bool, error) { return "", false, nil })
			second := lifecycleHostPID
			if tc.changed {
				second = 4343
			}
			runningPidSequence(t, lifecycleHostPID, second)
			got, ok := ownGatewayPid(t)
			if tc.verified && !tc.changed {
				if !ok || got != lifecycleHostPID {
					t.Fatalf("owned gateway pid = %d %v", got, ok)
				}
				return
			}
			if ok {
				t.Fatalf("owned gateway pid = %d", got)
			}
		})
	}
}

func TestReportProfileStopFailurePrintsAbsenceOnlyWhenProven(t *testing.T) {
	stopRefusalHomes(t)
	swapSetup(t, &statusGetRunningPid, func(string, bool) (int, bool, error) { return 0, false, nil })
	var out bytes.Buffer
	if code := ReportProfileStopFailure(context.Background(), &out); code != 0 || out.String() != "✗ No gateway running for this profile\n" {
		t.Fatalf("absent report = %d %q", code, out.String())
	}
	swapSetup(t, &statusGetRunningPid, func(string, bool) (int, bool, error) { return 0, false, errors.New("cannot inspect gateway") })
	out.Reset()
	if code := ReportProfileStopFailure(context.Background(), &out); code != 1 || !strings.Contains(out.String(), "not confirmed") || strings.Contains(out.String(), "No gateway running") {
		t.Fatalf("unknown report = %d %q", code, out.String())
	}
}
