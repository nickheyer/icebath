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
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
)

// Pins unit ownership so fake units on an invented main pid still prove budgets and health
func unitsBelongToThisUpdate(t *testing.T) {
	t.Helper()
	vsSwap(t, &fleetUnitOwned, func(context.Context, io.Writer, []string, string) (bool, error) { return true, nil })
}

type systemctlCall struct {
	cmd     []string
	timeout float64
}

func countVerbs(calls []systemctlCall, verbs ...string) int {
	count := 0
	for _, call := range calls {
		for _, verb := range verbs {
			if slices.Contains(call.cmd, verb) {
				count++
				break
			}
		}
	}
	return count
}

func hasPrefix(cmd, prefix []string) bool {
	return len(cmd) >= len(prefix) && reflect.DeepEqual(cmd[:len(prefix)], prefix)
}

func TestUnitTransactionBudgetPreservesScopeAndHealth(t *testing.T) {
	for _, tc := range []struct{ graceful, retry bool }{{false, false}, {false, true}, {true, false}} {
		t.Run(fmt.Sprintf("graceful=%v,retry=%v", tc.graceful, tc.retry), func(t *testing.T) {
			unitsBelongToThisUpdate(t)
			scope := []string{"systemctl", "--no-ask-password"}
			manage := append([]string{"sudo", "-n"}, scope...)
			calls := []systemctlCall{}
			vsSwap(t, &fleetSystemctl, func(_ context.Context, cmd []string, timeout float64) (*pysubprocess.Completed, error) {
				calls = append(calls, systemctlCall{cmd, timeout})
				if slices.Contains(cmd, "show") {
					if !hasPrefix(cmd, scope) {
						t.Errorf("property read left its scope: %v", cmd)
					}
					if slices.Contains(cmd, "--property=MainPID") {
						return completedText(cmd, "42"), nil
					}
					return completedText(cmd, "TimeoutStopUSec=70s\nTimeoutStartUSec=90s"), nil
				}
				if slices.Contains(cmd, "restart") || slices.Contains(cmd, "start") {
					floor := 160.0
					if tc.graceful {
						floor = 90.0
					}
					if !hasPrefix(cmd, manage) || timeout <= floor {
						t.Errorf("manage verb %v ran with timeout %v", cmd, timeout)
					}
				}
				return completedText(cmd, "active"), nil
			})
			vsSwap(t, &fleetDrainOrSignal, func(context.Context, io.Writer, int, float64, string, ...map[int]bool) (bool, error) {
				return true, nil
			})
			health := []bool{true}
			if tc.retry {
				health = []bool{false, true}
			}
			vsSwap(t, &fleetWaitServiceActive, func(context.Context, []string, string, float64) (bool, error) {
				next := health[0]
				health = health[1:]
				return next, nil
			})
			name := "icebath-serve-test"
			if tc.graceful {
				name = "icebath-gateway-test"
			}
			restarted, failed := []string{}, []string{}
			err := restartOneSystemdGatewayUnit(context.Background(), io.Discard, name, "system", scope, 45,
				map[string][]string{"system": manage}, &restarted, &failed, nil)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(restarted, []string{name}) || len(failed) != 0 {
				t.Fatalf("restarted=%v failed=%v", restarted, failed)
			}
			if want := map[bool]int{false: 1, true: 2}[tc.retry]; countVerbs(calls, "restart", "start") != want {
				t.Fatalf("restart verbs = %d, want %d", countVerbs(calls, "restart", "start"), want)
			}
		})
	}
}

func TestFleetRestartBudgetPreservesUserScopeAndVerifiesHealth(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("systemd restart phase")
	}
	for _, healthy := range []bool{true, false} {
		t.Run(fmt.Sprintf("healthy=%v", healthy), func(t *testing.T) {
			unitsBelongToThisUpdate(t)
			t.Setenv("ICEBATH_HOME", t.TempDir())
			scope := []string{"systemctl", "--user"}
			manage := append(append([]string{}, scope...), "--no-ask-password")
			calls := []systemctlCall{}
			type healthCheck struct {
				command []string
				unit    string
			}
			checks := []healthCheck{}
			name := "icebath-serve-test"
			vsSwap(t, &fleetSystemctl, func(_ context.Context, cmd []string, timeout float64) (*pysubprocess.Completed, error) {
				calls = append(calls, systemctlCall{cmd, timeout})
				if !hasPrefix(cmd, scope) {
					t.Errorf("call left the user scope: %v", cmd)
				}
				switch {
				case slices.Contains(cmd, "list-units"):
					return completedText(cmd, name+".service loaded active running"), nil
				case slices.Contains(cmd, "show"):
					return completedText(cmd, "TimeoutStopUSec=70s\nTimeoutStartUSec=90s"), nil
				case slices.Contains(cmd, "restart"):
					if !hasPrefix(cmd, manage) || timeout <= 160 {
						t.Errorf("restart %v ran with timeout %v", cmd, timeout)
					}
				}
				return completedText(cmd, "active"), nil
			})
			vsSwap(t, &systemdScopes, []systemdScope{{"user", scope}})
			vsSwap(t, &fleetWaitServiceActive, func(_ context.Context, command []string, unit string, _ float64) (bool, error) {
				checks = append(checks, healthCheck{command, unit})
				return healthy, nil
			})
			vsSwap(t, &fleetSupportsSystemd, func(context.Context) bool { return true })
			vsSwap(t, &fleetEnsureUserSystemdEnv, func() {})
			restarted, failed, scoped := []string{}, []string{}, map[string]bool{}
			if err := restartSystemdGatewayUnits(context.Background(), io.Discard, &restarted, &failed, scoped, 45, nil); err != nil {
				t.Fatal(err)
			}

			attempts := map[bool]int{true: 1, false: 2}[healthy]
			wantChecks := []healthCheck{}
			for range attempts {
				wantChecks = append(wantChecks, healthCheck{scope, name})
			}
			if !reflect.DeepEqual(checks, wantChecks) || countVerbs(calls, "restart") != attempts {
				t.Fatalf("checks=%v restarts=%d", checks, countVerbs(calls, "restart"))
			}
			wantRestarted, wantScoped, wantFailed := []string{name}, map[string]bool{"user/" + name: true}, []string{}
			if !healthy {
				wantRestarted, wantScoped, wantFailed = []string{}, map[string]bool{}, []string{name}
			}
			if !reflect.DeepEqual(restarted, wantRestarted) || !reflect.DeepEqual(scoped, wantScoped) || !reflect.DeepEqual(failed, wantFailed) {
				t.Fatalf("restarted=%v scoped=%v failed=%v", restarted, scoped, failed)
			}
		})
	}
}

func TestBudgetFallbackKeepsRealErrors(t *testing.T) {
	limitCases := []string{"", "TimeoutStopUSec=infinity\nTimeoutStartUSec=invalid", "TimeoutStopUSec=70000000\nTimeoutStartUSec=90s"}
	for i, limits := range limitCases {
		for _, outcome := range []any{0, 7, "timeout"} {
			t.Run(fmt.Sprintf("limits%d-%v", i, outcome), func(t *testing.T) {
				vsSwap(t, &fleetSystemctl, func(_ context.Context, cmd []string, timeout float64) (*pysubprocess.Completed, error) {
					switch {
					case slices.Contains(cmd, "show"):
						return completedText(cmd, limits), nil
					case slices.Contains(cmd, "restart"):
						if timeout <= 160 || timeout >= float64(1<<31)/1000 {
							t.Errorf("restart timeout = %v", timeout)
						}
						if outcome == "timeout" {
							return nil, &pysubprocess.TimeoutExpired{Cmd: cmd, Timeout: int(timeout)}
						}
						return &pysubprocess.Completed{Args: cmd, Returncode: outcome.(int), Stdout: []byte{}, Stderr: []byte("manager diagnostic")}, nil
					}
					return completedText(cmd, ""), nil
				})
				if outcome == "timeout" {
					clock := installFleetClock(t)
					result, err := systemctlResetAndRestart(context.Background(), io.Discard, []string{"systemctl"}, "icebath-serve-test", nil)
					if err != nil || result.Returncode != unitJobStillRunning {
						t.Fatalf("result = %v, %v", result, err)
					}
					if clock.now.Seconds() > unitJobHardCapS {
						t.Fatalf("waited %v", clock.now)
					}
					return
				}
				result, err := systemctlResetAndRestart(context.Background(), io.Discard, []string{"systemctl"}, "icebath-serve-test", nil)
				if err != nil || result.Returncode != outcome.(int) || string(result.Stderr) != "manager diagnostic" {
					t.Fatalf("result = %v, %v", result, err)
				}
			})
		}
	}
}

func TestFleetRestartRepairsASystemUnitThatCannotParkOnExit78(t *testing.T) {
	for _, root := range []bool{true, false} {
		t.Run(fmt.Sprintf("root=%v", root), func(t *testing.T) {
			unitsBelongToThisUpdate(t)
			unitDir := filepath.Join(t.TempDir(), "system")
			if err := os.Mkdir(unitDir, 0o755); err != nil {
				t.Fatal(err)
			}
			write := func(name, text string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(unitDir, name), []byte(text), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			write("icebath-gateway.service", "[Service]\nRestart=on-failure\nRestartSec=10\n")
			write("icebath-gateway-ops.service", fmt.Sprintf("[Service]\nRestart=always\nRestartPreventExitStatus=%d\n", gateway.GatewayFatalConfigExitCode))
			vsSwap(t, &fleetSystemUnitDir, func() string { return unitDir })
			vsSwap(t, &fleetServiceName, func(context.Context) (string, error) { return "icebath-gateway", nil })
			vsSwap(t, &fleetEUID, func() int { return map[bool]int{true: 0, false: 1000}[root] })
			refreshed := []bool{}
			vsSwap(t, &fleetRefreshUnit, func(_ context.Context, system bool) (bool, error) {
				refreshed = append(refreshed, system)
				return true, nil
			})
			vsSwap(t, &fleetSystemctl, func(_ context.Context, cmd []string, _ float64) (*pysubprocess.Completed, error) {
				return completedText(cmd, "active"), nil
			})
			vsSwap(t, &fleetDrainOrSignal, func(context.Context, io.Writer, int, float64, string, ...map[int]bool) (bool, error) {
				return true, nil
			})
			vsSwap(t, &fleetWaitServiceActive, func(context.Context, []string, string, float64) (bool, error) { return true, nil })

			var out bytes.Buffer
			for _, name := range []string{"icebath-gateway", "icebath-gateway-ops"} {
				restarted, failed := []string{}, []string{}
				err := restartOneSystemdGatewayUnit(context.Background(), &out, name, "system", []string{"systemctl"}, 5,
					map[string][]string{"system": {"systemctl"}}, &restarted, &failed, nil)
				if err != nil {
					t.Fatal(err)
				}
			}

			if root {
				if !reflect.DeepEqual(refreshed, []bool{true}) || strings.Contains(out.String(), "RestartPreventExitStatus") {
					t.Fatalf("refreshed=%v out=%q", refreshed, out.String())
				}
				return
			}
			if len(refreshed) != 0 || strings.Count(out.String(), "RestartPreventExitStatus=78") != 1 ||
				!strings.Contains(out.String(), "icebath-gateway lacks") || !strings.Contains(out.String(), "sudo icebath gateway install --system") {
				t.Fatalf("refreshed=%v out=%q", refreshed, out.String())
			}
		})
	}
}

func TestRestartCountsAUnitWhoseStartJobOutlivedTheClient(t *testing.T) {
	const unit = "icebath-gateway"
	scope := []string{"systemctl", "--user"}
	manage := append(append([]string{}, scope...), "--no-ask-password")
	clock := installFleetClock(t)
	const jobSeconds = 190.0
	doneAt, issued := 0.0, false
	verbs := []string{}
	state := func() (string, int, string) {
		if issued && clock.now.Seconds() >= doneAt {
			return "active", 5555, ""
		}
		return "deactivating", 0, "7"
	}
	scripted := []string{"active", "42"}
	vsSwap(t, &fleetSystemctl, func(_ context.Context, cmd []string, timeout float64) (*pysubprocess.Completed, error) {
		if len(verbs) == 0 && (slices.Contains(cmd, "is-active") || slices.Contains(cmd, "--property=MainPID")) {
			next := scripted[0]
			scripted = scripted[1:]
			return completedText(cmd, next), nil
		}
		for _, verb := range []string{"stop", "start", "restart"} {
			if slices.Contains(cmd, verb) {
				verbs = append(verbs, verb)
				doneAt, issued = clock.now.Seconds()+jobSeconds, true
				fleetSleep(secondsDuration(timeout))
				return nil, &pysubprocess.TimeoutExpired{Cmd: cmd, Timeout: int(timeout)}
			}
		}
		active, mainPID, job := state()
		if slices.Contains(cmd, "is-active") {
			return completedText(cmd, active), nil
		}
		prop := ""
		for _, arg := range cmd {
			if strings.HasPrefix(arg, "--property=") {
				prop = arg
			}
		}
		switch {
		case strings.Contains(prop, "TimeoutStopUSec"):
			return completedText(cmd, "TimeoutStopUSec=90s\nTimeoutStartUSec=90s"), nil
		case prop == "--property=MainPID":
			return completedText(cmd, fmt.Sprint(mainPID)), nil
		}
		return completedText(cmd, fmt.Sprintf("ActiveState=%s\nSubState=x\nMainPID=%d\nJob=%s", active, mainPID, job)), nil
	})
	unitsBelongToThisUpdate(t)
	vsSwap(t, &fleetRepairUnit, func(context.Context, io.Writer, string, string) error { return nil })
	vsSwap(t, &fleetDrainOrSignal, func(context.Context, io.Writer, int, float64, string, ...map[int]bool) (bool, error) {
		return true, nil
	})
	restarted, failed := []string{}, []string{}
	err := restartOneSystemdGatewayUnit(context.Background(), io.Discard, unit, "user", scope, 45,
		map[string][]string{"user": manage}, &restarted, &failed, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(restarted, []string{unit}) || len(failed) != 0 || !reflect.DeepEqual(verbs, []string{"start"}) {
		t.Fatalf("restarted=%v failed=%v verbs=%v", restarted, failed, verbs)
	}
}

// Sandboxes the user home so a unit that declares no home resolves to this update's own home
func bestEffortRestartHost(t *testing.T, mainPID func(svc string) int) *[]string {
	t.Helper()
	userHome := t.TempDir()
	t.Setenv("HOME", userHome)
	t.Setenv("ICEBATH_HOME", filepath.Join(userHome, ".icebath"))
	if err := os.MkdirAll(filepath.Join(userHome, ".icebath"), 0o755); err != nil {
		t.Fatal(err)
	}
	vsSwap(t, &fleetUnitMainPid, func(_ context.Context, _ []string, svc string) (int, error) { return mainPID(svc), nil })
	vsSwap(t, &fleetHomeForPID, func(context.Context, int) (string, bool, error) { return "", false, nil })
	// The host's systemd is never asked what an invented unit declares, it answers as for an unknown unit
	vsSwap(t, &fleetSystemctl, func(_ context.Context, cmd []string, _ float64) (*pysubprocess.Completed, error) {
		return completedText(cmd, ""), nil
	})
	restarted := []string{}
	vsSwap(t, &fleetResetAndRestart, func(_ context.Context, _ io.Writer, _ []string, svc string, _ []string) (*pysubprocess.Completed, error) {
		restarted = append(restarted, svc)
		return &pysubprocess.Completed{}, nil
	})
	vsSwap(t, &fleetWaitServiceActive, func(context.Context, []string, string, float64) (bool, error) { return true, nil })
	vsSwap(t, &systemdScopes, []systemdScope{{"user", []string{"systemctl", "--user"}}})
	return &restarted
}

func userListing(units ...string) []systemdUnitListing {
	lines := make([]string, len(units))
	for i, unit := range units {
		lines[i] = unit + " loaded active running x"
	}
	return []systemdUnitListing{{Scope: "user", ScopeCmd: []string{"systemctl", "--user"}, Listing: completedText(nil, strings.Join(lines, "\n"))}}
}

func TestLeftoverPerProfileUnitsRestartTheirOneHostProcessOnce(t *testing.T) {
	restarted := bestEffortRestartHost(t, func(string) int { return 4242 })
	failed := []string{}
	err := restartSystemdGatewayUnitsBestEffort(context.Background(), io.Discard, &failed,
		userListing("icebath-gateway.service", "icebath-gateway-coder.service", "icebath-gateway-writer.service"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(*restarted, []string{"icebath-gateway"}) || len(failed) != 0 {
		t.Fatalf("restarted=%v failed=%v", *restarted, failed)
	}
}

func TestUnitsWithDistinctLivePidsAreEachRestarted(t *testing.T) {
	pids := map[string]int{"icebath-gateway": 1, "icebath-gateway-coder": 2}
	restarted := bestEffortRestartHost(t, func(svc string) int { return pids[svc] })
	failed := []string{}
	if err := restartSystemdGatewayUnitsBestEffort(context.Background(), io.Discard, &failed, userListing("icebath-gateway.service", "icebath-gateway-coder.service")); err != nil {
		t.Fatal(err)
	}
	got := append([]string{}, *restarted...)
	slices.Sort(got)
	if !reflect.DeepEqual(got, []string{"icebath-gateway", "icebath-gateway-coder"}) {
		t.Fatalf("restarted = %v", *restarted)
	}
}
