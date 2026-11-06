//go:build !windows

package updaterecord

import (
	"bufio"
	"context"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

func pcLoad(text string) map[string]any {
	v, err := pyjson.Loads(text)
	if err != nil {
		panic(err)
	}
	return pyjson.Plain(v).(map[string]any)
}

// Child stand-ins for the updaters, launches and gateways the pause record tests kill and race
func pcChild(mode string, args []string) int {
	ctx := context.Background()
	pbArgv()
	must := func(err error) {
		if err != nil {
			panic(err)
		}
	}
	resumes := func(hang bool) {
		SetPauseGatewayResumer(func(ctx context.Context, token map[string]any, stdout io.Writer) error {
			names := []any{}
			for _, name := range pbProfiles(token) {
				names = append(names, name)
			}
			pcPrint("resume " + pyrepr.Repr(names))
			if hang {
				time.Sleep(120 * time.Second)
			}
			token["resume_needed"] = false
			return nil
		})
		must(RecoverPauses(ctx, []string{"status"}))
		pcPrint("done")
	}
	switch mode {
	case "owner":
		token, err := StampTree(ctx, map[string]any{"resume_needed": true, "profiles": pcLoad(args[0])}, "")
		must(err)
		must(WritePause(ctx, token, PauseIdentity(0), ""))
		pcPrint("written")
		time.Sleep(120 * time.Second)
	case "orphans":
		found, err := PauseOrphans(ctx, "")
		must(err)
		if len(found) == 0 {
			pcPrint("null")
			return 0
		}
		text, err := pyjson.Dumps(mapOf(found[0].Body["token"])["profiles"])
		must(err)
		pcPrint(text)
	case "recover":
		resumes(args[0] == "hang")
	case "after_claim":
		var updates []*exec.Cmd
		pauseClaim = func(ctx context.Context, src string) (string, map[string]any, bool, error) {
			won, body, ok, err := ClaimPause(ctx, src)
			update := pcCommand("hold_checkout")
			stdout, _ := update.StdoutPipe()
			must(update.Start())
			updates = append(updates, update)
			if line, _ := bufio.NewReader(stdout).ReadString('\n'); strings.TrimSpace(line) != "True" {
				panic("the update stand-in did not take the checkout: " + line)
			}
			return won, body, ok, err
		}
		resumes(false)
		for _, update := range updates {
			_ = update.Process.Kill()
			_ = update.Wait()
		}
	case "hold_checkout":
		held, err := NewUpdateLock(UpdateMarkerPath(), PauseInstallRoot(), false).AcquireCheckout(PauseInstallRoot())
		must(err)
		pcPrint(map[bool]string{true: "True", false: "False"}[held])
		time.Sleep(120 * time.Second)
	case "claim_pause":
		pauseTransferHook = func(point string) {
			if point == "claim" {
				pcPrint("paused")
				pcWaitFile(args[1])
			}
		}
		won, _, ok, err := ClaimPause(ctx, args[0])
		must(err)
		pauseTransferHook = func(string) {}
		if !ok {
			pcPrint("null")
		} else {
			text, _ := pyjson.Dumps(won)
			pcPrint(text)
		}
	case "claim_kill":
		pauseTransferHook = func(point string) {
			if point == "claim" {
				os.Exit(71)
			}
		}
		must(RecoverPauses(ctx, []string{"status"}))
	case "record_kill":
		adopted, claims, err := AdoptPauseOrphans(ctx)
		must(err)
		pauseTransferHook = func(point string) {
			if point == "record" {
				os.Exit(71)
			}
		}
		_, err = RecordPause(ctx, map[string]any{"resume_needed": true, "profiles": map[string]any{"beta": 99}}, adopted, claims)
		must(err)
	case "draining":
		stops := make(chan os.Signal, 1)
		signal.Notify(stops, syscall.SIGTERM)
		pcPrint("up")
		<-stops
		pcPrint("stopping")
		pcWaitFile(args[0])
		return 0
	case "updater":
		pid, _ := strconv.Atoi(args[0])
		token, err := RecordPause(ctx, map[string]any{"resume_needed": true, "profiles": map[string]any{"default": pid},
			"identities": map[string]any{strconv.Itoa(pid): PauseIdentity(pid)["ct"]}}, nil, nil)
		must(err)
		switch args[1] {
		case "asked":
			must(MarkStopRequested(ctx, token, []int{pid}, nil))
			must(MarkStopSent(ctx, token, pid))
			must(syscall.Kill(pid, syscall.SIGTERM))
		case "intended":
			must(MarkStopRequested(ctx, token, []int{pid}, nil))
		}
		pcPrint(args[1])
		time.Sleep(120 * time.Second)
	case "other_writer":
		pauseInstall = func() string { return args[0] }
		token, err := StampTree(ctx, map[string]any{"resume_needed": true, "profiles": map[string]any{"beta": 1}}, "")
		must(err)
		must(WritePause(ctx, token, PauseUnowned(), ""))
		found, err := PauseOrphans(ctx, "")
		must(err)
		body, err := ReadPauseRecord(PauseRecordPath())
		must(err)
		text, err := pyjson.Dumps([]any{len(found), body})
		must(err)
		pcPrint(text)
	default:
		if code, ok := lockChild(mode, args); ok {
			return code
		}
		panic("unknown pause test child " + mode)
	}
	return 0
}

func pcOrphan(t *testing.T, profiles string) {
	t.Helper()
	owner := pcStart(t, "owner", profiles)
	if got := owner.line(t); got != "written" {
		t.Fatalf("owner said %q", got)
	}
	owner.kill(t)
}

func pcOrphaned(t *testing.T) string {
	t.Helper()
	out, _ := pcStart(t, "orphans").rest(t)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	return lines[len(lines)-1]
}

func pcLaunches(t *testing.T, n int) [][]string {
	t.Helper()
	seen := [][]string{}
	for range n {
		out, _ := pcStart(t, "recover", "ok").rest(t)
		resumed := []string{}
		for _, line := range strings.Split(out, "\n") {
			if strings.HasPrefix(line, "resume") {
				resumed = append(resumed, line)
			}
		}
		seen = append(seen, resumed)
	}
	return seen
}

func pcRecordFiles(t *testing.T, home string) []string {
	t.Helper()
	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, e := range entries {
		ext := filepath.Ext(e.Name())
		if strings.HasPrefix(e.Name(), PauseRecordStem) && (ext == ".json" || ext == ".claim") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names
}

func TestPauseRecordWrittenByAKilledUpdaterIsOrphanedOnlyAfterItsDeath(t *testing.T) {
	psHome(t)
	owner := pcStart(t, "owner", `{"default": 4242}`)
	if got := owner.line(t); got != "written" {
		t.Fatalf("owner said %q", got)
	}
	body, err := ReadPauseRecord(PauseRecordPath())
	if err != nil || body == nil {
		t.Fatalf("record unreadable: %v", err)
	}
	ownerID := mapOf(body["owner"])
	if !pyrepr.Equal(ownerID["pid"], owner.cmd.Process.Pid) || !strings.HasPrefix(pyrepr.Str(ownerID["ct"]), "ct:") {
		t.Fatalf("owner identity %v", ownerID)
	}
	if profiles := mapOf(mapOf(body["token"])["profiles"]); len(profiles) != 1 || !pyrepr.Equal(profiles["default"], 4242) {
		t.Fatalf("profiles %v", profiles)
	}
	if got := pcOrphaned(t); got != "null" {
		t.Fatalf("a live owner's set was orphaned: %s", got)
	}
	owner.kill(t)
	if got := pcOrphaned(t); got != `{"default": 4242}` {
		t.Fatalf("a dead owner's set was not orphaned: %s", got)
	}
}

func TestPauseRecordALaunchKilledMidRecoveryLeavesTheSetToTheNextLaunch(t *testing.T) {
	psHome(t)
	pcOrphan(t, `{"default": 4242}`)
	first := pcStart(t, "recover", "hang")
	if got := first.line(t); got != "resume ['default']" {
		t.Fatalf("first launch said %q", got)
	}
	first.kill(t)
	out, _ := pcStart(t, "recover", "ok").rest(t)
	if !strings.HasPrefix(out, "resume ['default']\n") {
		t.Fatalf("second launch: %q", out)
	}
	if out, _ = pcStart(t, "recover", "ok").rest(t); strings.TrimSpace(out) != "done" {
		t.Fatalf("a resumed set was resumed again: %q", out)
	}
}

func TestPauseRecordRecoveryNeverStartsGatewaysUnderAnUpdateThatTookTheCheckout(t *testing.T) {
	psHome(t)
	pcOrphan(t, `{"default": 4242}`)
	if out, _ := pcStart(t, "after_claim").rest(t); strings.TrimSpace(out) != "done" {
		t.Fatalf("gateways restarted while an update owned the checkout: %q", out)
	}
	if out, _ := pcStart(t, "recover", "ok").rest(t); !strings.HasPrefix(out, "resume ['default']\n") {
		t.Fatalf("the handed-back set was stranded: %q", out)
	}
}

func TestPauseRecordAClaimInTransferCannotBeTakenByASecondLaunch(t *testing.T) {
	home := psHome(t)
	pcOrphan(t, `{"default": 4242}`)
	src := PauseRecordPath()
	flag := filepath.Join(home, "go")
	first := pcStart(t, "claim_pause", src, flag)
	func() {
		defer os.WriteFile(flag, nil, 0o644)
		if got := first.line(t); got != "paused" {
			t.Fatalf("first launch said %q", got)
		}
		for _, p := range append([]string{src}, pauseClaims(src)...) {
			if _, _, ok, err := ClaimPause(context.Background(), p); ok || err != nil {
				t.Fatalf("a second launch took a claim whose transfer was in flight: %v %v", ok, err)
			}
		}
	}()
	won, err := pyjson.Loads(first.line(t))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = first.rest(t)
	if got := pcRecordFiles(t, home); !reflect.DeepEqual(got, []string{filepath.Base(won.(string))}) {
		t.Fatalf("the paused set is now carried by %v", got)
	}
}

func TestPauseRecordALaunchKilledBetweenClaimAndRetireResumesTheSetOnce(t *testing.T) {
	home := psHome(t)
	pcOrphan(t, `{"default": 4242}`)
	if _, code := pcStart(t, "claim_kill").rest(t); code != 71 || len(pcRecordFiles(t, home)) != 2 {
		t.Fatalf("premise: killed after publishing the claim (%d, %v)", code, pcRecordFiles(t, home))
	}
	if got := pcLaunches(t, 2); !reflect.DeepEqual(got, [][]string{{"resume ['default']"}, {}}) {
		t.Fatalf("launches %v", got)
	}
	if got := pcRecordFiles(t, home); len(got) != 0 {
		t.Fatalf("records left %v", got)
	}
}

func TestPauseRecordAnUpdateKilledAfterPublishingItsRecordNeverRestartsASetTwice(t *testing.T) {
	home := psHome(t)
	pcOrphan(t, `{"default": 4242}`)
	if _, code := pcStart(t, "record_kill").rest(t); code != 71 || len(pcRecordFiles(t, home)) != 2 {
		t.Fatalf("premise: killed before retiring the claim (%d, %v)", code, pcRecordFiles(t, home))
	}
	if got := pcLaunches(t, 2); !reflect.DeepEqual(got, [][]string{{"resume ['beta', 'default']"}, {}}) {
		t.Fatalf("launches %v", got)
	}
	if got := pcRecordFiles(t, home); len(got) != 0 {
		t.Fatalf("records left %v", got)
	}
}

func TestPauseRecordAGatewayDrainingAfterTheStopRequestKeepsItsRestartDebt(t *testing.T) {
	home := psHome(t)
	flag := filepath.Join(home, "drained")
	gateway := pcStart(t, "draining", flag)
	if got := gateway.line(t); got != "up" {
		t.Fatalf("gateway said %q", got)
	}
	updater := pcStart(t, "updater", strconv.Itoa(gateway.cmd.Process.Pid), "asked")
	if got := updater.line(t); got != "asked" {
		t.Fatalf("updater said %q", got)
	}
	if got := gateway.line(t); got != "stopping" {
		t.Fatalf("gateway said %q", got)
	}
	updater.kill(t)
	func() {
		defer os.WriteFile(flag, nil, 0o644)
		if got := pcLaunches(t, 1); !reflect.DeepEqual(got, [][]string{{}}) {
			t.Fatalf("a draining gateway was restarted before it exited: %v", got)
		}
		if got := pcRecordFiles(t, home); len(got) != 1 {
			t.Fatalf("a draining gateway's restart debt was dropped: %v", got)
		}
	}()
	_, _ = gateway.rest(t)
	if got := pcLaunches(t, 2); !reflect.DeepEqual(got, [][]string{{"resume ['default']"}, {}}) {
		t.Fatalf("launches %v", got)
	}
}

func TestPauseRecordAGatewayNeverAskedToStopIsNotRestarted(t *testing.T) {
	home := psHome(t)
	gateway := pcStart(t, "draining", filepath.Join(home, "unused"))
	if got := gateway.line(t); got != "up" {
		t.Fatalf("gateway said %q", got)
	}
	updater := pcStart(t, "updater", strconv.Itoa(gateway.cmd.Process.Pid), "recorded")
	if got := updater.line(t); got != "recorded" {
		t.Fatalf("updater said %q", got)
	}
	updater.kill(t)
	if got := pcLaunches(t, 1); !reflect.DeepEqual(got, [][]string{{}}) {
		t.Fatalf("launches %v", got)
	}
	if got := pcRecordFiles(t, home); len(got) != 0 {
		t.Fatalf("a still-serving gateway's entry stayed owed: %v", got)
	}
	gateway.kill(t)
}

func TestPauseRecordAStopIntendedButNeverIssuedLeavesAServingGatewayAloneAndALaterUserStopFinal(t *testing.T) {
	home := psHome(t)
	gateway := pcStart(t, "draining", filepath.Join(home, "unused"))
	if got := gateway.line(t); got != "up" {
		t.Fatalf("gateway said %q", got)
	}
	updater := pcStart(t, "updater", strconv.Itoa(gateway.cmd.Process.Pid), "intended")
	if got := updater.line(t); got != "intended" {
		t.Fatalf("updater said %q", got)
	}
	updater.kill(t)
	if got := pcLaunches(t, 1); !reflect.DeepEqual(got, [][]string{{}}) {
		t.Fatalf("launches %v", got)
	}
	if got := pcRecordFiles(t, home); len(got) != 0 {
		t.Fatalf("a gateway never asked to stop is held as draining: %v", got)
	}
	gateway.kill(t)
	if got := pcLaunches(t, 1); !reflect.DeepEqual(got, [][]string{{}}) {
		t.Fatalf("a gateway the user stopped was restarted for an update that never stopped it: %v", got)
	}
}

func TestPauseRecordAnotherCheckoutsWriterNeverImportsOrRelabelsThisCheckoutsDebt(t *testing.T) {
	home := psHome(t)
	pcOrphan(t, `{"alpha": 4242}`)
	other := filepath.Join(home, "other")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	ptGit(t, other, "init", "-q")
	ptWrite(t, filepath.Join(other, "marker"), "b\n")
	ptGit(t, other, "add", ".")
	ptGit(t, other, "commit", "-qm", "b")
	out, _ := pcStart(t, "other_writer", other).rest(t)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	pair := pcLoadList(t, lines[len(lines)-1])
	theirs := mapOf(pair[1])
	if !pyrepr.Equal(pair[0], 1) || !reflect.DeepEqual(pbProfiles(mapOf(theirs["token"])), []string{"beta"}) {
		t.Fatalf("imported this checkout's set: %v", pair)
	}
	ours, err := ReadPauseRecord(PauseRecordPath())
	if err != nil {
		t.Fatal(err)
	}
	if !pyrepr.Equal(ours["install_root"], PauseInstallRoot()) || !reflect.DeepEqual(pbProfiles(mapOf(ours["token"])), []string{"alpha"}) {
		t.Fatalf("ours %v", ours)
	}
}

func pcLoadList(t *testing.T, text string) []any {
	t.Helper()
	v, err := pyjson.Loads(text)
	if err != nil {
		t.Fatal(err)
	}
	return pyjson.Plain(v).([]any)
}
