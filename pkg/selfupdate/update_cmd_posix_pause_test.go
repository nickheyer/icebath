//go:build !windows

package selfupdate

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pycopy"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Hidden first argument that routes a re-executed test binary to a pause child stand-in
const ppChildEntry = "__posix_pause_test_child__"

const ppUnitName = "icebath-gateway-p2probe.service"

func init() {
	if len(os.Args) > 2 && os.Args[1] == ppChildEntry {
		os.Exit(ppChild(os.Args[2], os.Args[3:]))
	}
}

func ppUnit() map[string]any {
	return map[string]any{"kind": "systemd", "scope": "user", "unit": ppUnitName, "pid": 4242}
}

func ppEntryPids(token map[string]any) []int {
	pids := []int{}
	for _, key := range []string{"unmapped", "posix_units"} {
		for _, entry := range tokenEntries(token[key]) {
			pids = append(pids, entryPid(entry))
		}
	}
	sort.Ints(pids)
	return pids
}

// Child stand-ins for the updaters, gateways and probes the pause tests kill and question
func ppChild(mode string, args []string) int {
	ctx := context.Background()
	must := func(err error) {
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(3)
		}
	}
	emit := func(v any) {
		text, err := json.Marshal(v)
		must(err)
		fmt.Println(string(text))
	}
	switch mode {
	case "sleep":
		time.Sleep(120 * time.Second)
	case "owner":
		token, err := updaterecord.StampTree(ctx, map[string]any{"platform": "posix", "resume_needed": true, "posix_units": []any{ppUnit()}}, "")
		must(err)
		must(updaterecord.WritePause(ctx, token, updaterecord.PauseIdentity(0), ""))
		fmt.Println("written")
		time.Sleep(120 * time.Second)
	case "next":
		adopted, claims, err := updaterecord.AdoptPauseOrphans(ctx)
		must(err)
		token, err := updaterecord.RecordPause(ctx, map[string]any{"platform": "posix", "resume_needed": true, "unmapped": []any{}}, adopted, claims)
		must(err)
		emit(map[string]any{"units": token["posix_units"], "platform": token["platform"]})
	case "killed":
		pid, _ := strconv.Atoi(args[0])
		kind, asked, served := args[1], args[2] == "1", args[3]
		entry := map[string]any{"pid": pid, "home": served}
		if kind == "systemd" {
			entry["kind"], entry["scope"], entry["unit"], entry["cgroup"] = "systemd", "user", ppUnitName, nil
		} else {
			entry["argv"] = []any{"icebath", "gateway", "run"}
		}
		pauseDiscoverSystemd = func(context.Context) ([]map[string]any, error) {
			if kind == "systemd" {
				return []map[string]any{entry}, nil
			}
			return nil, nil
		}
		pauseDiscoverLaunchd = func(context.Context) ([]map[string]any, error) { return nil, nil }
		pauseLeftRunning = func(_ context.Context, units, jobs []map[string]any) ([]map[string]any, []map[string]any, []string, error) {
			return units, jobs, nil, nil
		}
		pauseDiscoverBare = func(context.Context, map[int]bool) ([]map[string]any, []string, error) {
			if kind == "systemd" {
				return nil, nil, nil
			}
			return []map[string]any{entry}, nil, nil
		}
		pauseStopGateways = func(ctx context.Context, token map[string]any, _ []map[string]any) error {
			if asked {
				must(updaterecord.MarkStopSent(ctx, token, pid))
			}
			must(syscall.Kill(os.Getpid(), syscall.SIGKILL))
			time.Sleep(120 * time.Second)
			return nil
		}
		must(pause(ctx, emptyToken()))
	case "probe":
		found, err := updaterecord.PauseOrphans(ctx, "")
		must(err)
		token, draining := map[string]any{}, map[string]any{}
		if len(found) > 0 {
			recorded, _ := found[0].Body["token"].(map[string]any)
			for key, value := range recorded {
				token[key] = value
			}
			token, err = updaterecord.DropNeverStopped(token)
			must(err)
			draining = updaterecord.SplitDraining(token)
		}
		emit(map[string]any{"recorded": len(found) > 0, "relaunched": ppEntryPids(token), "held": ppEntryPids(draining)})
	case "replay-env":
		tools.ProfileSecrets = provider.ProfileSecretSource()
		root := args[0]
		got := map[string]any{}
		for who, served := range map[string]string{"alpha": root + "/profiles/alpha", "beta": root + "/profiles/beta", "root": root} {
			env, err := replayEnv(ctx, served)
			must(err)
			got[who] = nil
			if value, ok := env["TELEGRAM_BOT_TOKEN"]; ok {
				got[who] = value
			}
		}
		emit(got)
	}
	return 0
}

func ppCommand(env []string, mode string, args ...string) *exec.Cmd {
	cmd := exec.Command(os.Args[0], append([]string{ppChildEntry, mode}, args...)...)
	cmd.Env = append(os.Environ(), env...)
	cmd.Stderr = os.Stderr
	return cmd
}

// Last stdout line of a finished child decoded as a JSON object
func ppLastJSON(t *testing.T, cmd *exec.Cmd) map[string]any {
	t.Helper()
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("child failed %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	var got map[string]any
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &got); err != nil {
		t.Fatalf("child printed %q %v", out, err)
	}
	return got
}

func ppSleeper(t *testing.T) *exec.Cmd {
	t.Helper()
	gatewayProc := ppCommand(nil, "sleep")
	if err := gatewayProc.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = gatewayProc.Process.Kill()
		_ = gatewayProc.Wait()
	})
	return gatewayProc
}

func TestAKilledUpdatersSupervisedUnitsAreAdoptedByTheNextUpdate(t *testing.T) {
	env := []string{"ICEBATH_HOME=" + t.TempDir()}
	owner := ppCommand(env, "owner")
	stdout, err := owner.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := owner.Start(); err != nil {
		t.Fatal(err)
	}
	line, _ := bufio.NewReader(stdout).ReadString('\n')
	if strings.TrimSpace(line) != "written" {
		_ = owner.Process.Kill()
		_ = owner.Wait()
		t.Fatalf("owner said %q", line)
	}
	_ = owner.Process.Signal(syscall.SIGKILL)
	_ = owner.Wait()

	got := ppLastJSON(t, ppCommand(env, "next"))
	want := map[string]any{"units": []any{map[string]any{"kind": "systemd", "scope": "user", "unit": ppUnitName, "pid": float64(4242)}}, "platform": "posix"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("the adopted unit was lost: %v", got)
	}
}

func TestAPromptWhileTheGatewaysArePausedTakesItsDefaultAtOnce(t *testing.T) {
	served := t.TempDir()
	t.Setenv("ICEBATH_HOME", served)
	saved := posixPauseRun
	posixPauseRun = map[string]any{"platform": "posix", "posix_stopped": true, "resume_needed": true}
	t.Cleanup(func() { posixPauseRun = saved })
	answered := make(chan string, 1)
	go func() {
		var out bytes.Buffer
		answer, _ := gatewayPrompt(context.Background(), &out, "Restore local changes now?", "n", 300)
		answered <- answer
	}()
	select {
	case answer := <-answered:
		if answer != "n" {
			t.Fatalf("answer %q", answer)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a prompt waited for a gateway this update had stopped")
	}
	if _, err := os.Stat(filepath.Join(served, ".update_prompt.json")); err == nil {
		t.Fatal("the prompt marker was written")
	}
}

func TestAKilledUpdatesRecordOwesOnlyWhatItStopped(t *testing.T) {
	for _, kind := range []string{"bare", "systemd"} {
		for _, asked := range []bool{false, true} {
			id := "never-asked"
			if asked {
				id = "asked"
			}
			t.Run(kind+"-"+id, func(t *testing.T) {
				served := t.TempDir()
				env := []string{"ICEBATH_HOME=" + served}
				gatewayProc := ppSleeper(t)
				flag := "0"
				if asked {
					flag = "1"
				}
				owner := ppCommand(env, "killed", strconv.Itoa(gatewayProc.Process.Pid), kind, flag, served)
				_ = owner.Run()
				status, _ := owner.ProcessState.Sys().(syscall.WaitStatus)
				if !status.Signaled() || status.Signal() != syscall.SIGKILL {
					t.Fatalf("premise: the update was not killed mid-stop (%v)", owner.ProcessState)
				}
				got := ppLastJSON(t, ppCommand(env, "probe"))
				if got["recorded"] != true {
					t.Fatal("premise: the killed update left no record")
				}
				if relaunched, _ := got["relaunched"].([]any); len(relaunched) != 0 {
					t.Fatalf("a still-running gateway would be relaunched beside itself: %v", got)
				}
				held, _ := got["held"].([]any)
				want := []any{}
				if asked {
					want = []any{float64(gatewayProc.Process.Pid)}
				}
				if !reflect.DeepEqual(held, want) {
					t.Fatalf("wrong restart debt: %v", got)
				}
			})
		}
	}
}

func TestARecordThatCannotBeWrittenLeavesTheUpdateUnpausedAndUngated(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes through a read-only dir")
	}
	priors := map[string]map[string]any{"fresh": {}, "adopted": {"pause_id": "adopted-1", "resume_needed": true}}
	for _, id := range []string{"fresh", "adopted"} {
		t.Run(id, func(t *testing.T) {
			gatewayProc := ppSleeper(t)
			served := filepath.Join(t.TempDir(), "home")
			if err := os.Mkdir(served, 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("ICEBATH_HOME", served)
			entry := map[string]any{"pid": gatewayProc.Process.Pid, "argv": []any{"icebath", "gateway", "run"}, "home": served}
			savedSystemd, savedLaunchd, savedBare, savedStop := pauseDiscoverSystemd, pauseDiscoverLaunchd, pauseDiscoverBare, pauseStopGateways
			t.Cleanup(func() {
				pauseDiscoverSystemd, pauseDiscoverLaunchd, pauseDiscoverBare, pauseStopGateways = savedSystemd, savedLaunchd, savedBare, savedStop
			})
			pauseDiscoverSystemd = func(context.Context) ([]map[string]any, error) { return nil, nil }
			pauseDiscoverLaunchd = func(context.Context) ([]map[string]any, error) { return nil, nil }
			pauseDiscoverBare = func(context.Context, map[int]bool) ([]map[string]any, []string, error) {
				return []map[string]any{pycopy.DeepCopy(entry).(map[string]any)}, nil, nil
			}
			stopped := false
			pauseStopGateways = func(context.Context, map[string]any, []map[string]any) error {
				stopped = true
				return nil
			}
			token := emptyToken()
			for key, value := range priors[id] {
				token[key] = value
			}
			before := pycopy.DeepCopy(token).(map[string]any)
			if err := os.Chmod(served, 0o500); err != nil {
				t.Fatal(err)
			}
			err := pause(context.Background(), token)
			_ = os.Chmod(served, 0o700)
			if err != nil {
				t.Fatalf("the pause refused the update: %v", err)
			}
			if stopped {
				t.Fatal("a gateway was stopped without a record")
			}
			if !reflect.DeepEqual(token, before) {
				t.Fatalf("the failed attempt left state behind: %v", token)
			}
		})
	}
}

func TestAReplayedGatewayKeepsItsOwnExportedTokenAndNoOneElses(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, ".icebath")
	for _, name := range []string{"alpha", "beta"} {
		if err := os.MkdirAll(filepath.Join(root, "profiles", name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	got := ppLastJSON(t, ppCommand([]string{"HOME=" + tmp, "ICEBATH_HOME=" + filepath.Join(root, "profiles", "alpha"),
		"TELEGRAM_BOT_TOKEN=exported-alpha-token"}, "replay-env", root))
	want := map[string]any{"alpha": "exported-alpha-token", "beta": nil, "root": nil}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
}

func TestUnknownCgroupMembershipNeverAuthorizesStoppingTheUnit(t *testing.T) {
	if escapeCgroup(context.Background(), map[string]any{"unit": ppUnitName, "pid": 4242, "cgroup": nil}) {
		t.Fatal("unreadable membership authorized the stop")
	}
}
