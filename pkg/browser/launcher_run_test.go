package browser

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/tools"
)

func orderLaunch(t *testing.T) (string, func(key string) *LaunchedBrowser) {
	env := launcherTestEnv(t)
	exe := fakeExe(t, "order-chromium")
	pin(t, &launcherResolveExecutable, func(*tools.Env) string { return exe })
	return env.Home, func(key string) *LaunchedBrowser {
		t.Helper()
		b, err := launchEphemeral(env, key, LaunchOptions{})
		if err != "" || b == nil {
			t.Fatalf("launch %s: %s", key, err)
		}
		return b
	}
}

func argvOf(t *testing.T, b *LaunchedBrowser) []string {
	raw, err := os.ReadFile(filepath.Join(b.UserDataDir, "argv.json"))
	if err != nil {
		t.Fatal(err)
	}
	var argv []string
	json.Unmarshal(raw, &argv)
	return argv
}

func TestLaunchReadsDevtoolsPortAndRunsHeadlessOnAnEphemeralHomeProfile(t *testing.T) {
	homeDir, launch := orderLaunch(t)
	b := launch("task-a")
	if b.CDPURL != "ws://127.0.0.1:45678/devtools/browser/fake-id" || b.HTTPURL != "http://127.0.0.1:45678" {
		t.Fatalf("urls %s %s", b.CDPURL, b.HTTPURL)
	}
	if !b.Owned || !b.Ephemeral || filepath.Dir(b.UserDataDir) != filepath.Join(homeDir, "cache", "browser", "chromium") {
		t.Fatalf("browser %+v", b)
	}
	argv := argvOf(t, b)
	for _, want := range []string{"--remote-debugging-port=0", "--user-data-dir=" + b.UserDataDir, "--headless=new"} {
		if !slices.Contains(argv, want) {
			t.Fatalf("argv %q lacks %s", argv, want)
		}
	}
	if argv[len(argv)-1] != "about:blank" {
		t.Fatalf("argv %q", argv)
	}
}

func TestLaunchRecordLivesInTheProfilesPrivateStateDir(t *testing.T) {
	homeDir, launch := orderLaunch(t)
	b := launch("recorded")
	state := filepath.Join(homeDir, "cache", "browser")
	for _, dir := range []string{"chromium-records", "chromium"} {
		if info, err := os.Stat(filepath.Join(state, dir)); err != nil || info.Mode().Perm() != 0o700 {
			t.Fatalf("%s mode %v %v", dir, info, err)
		}
	}
	records := globNames(filepath.Join(state, "chromium-records"), "*.json")
	if len(records) != 1 {
		t.Fatalf("records %v", records)
	}
	data := loadStateFile(filepath.Join(state, "chromium-records", records[0]))
	if v, _ := jsonInt(objGet(data, "pid")); v != *b.PID {
		t.Fatalf("record pid %v", objGet(data, "pid"))
	}
	stopBrowser(context.Background(), "recorded")
	if left := globNames(filepath.Join(state, "chromium-records"), "*.json"); len(left) != 0 {
		t.Fatalf("records left %v", left)
	}
}

func TestOneBrowserPerKeyAndSeparateBrowsersForSeparateKeys(t *testing.T) {
	_, launch := orderLaunch(t)
	a1, a2, b := launch("named-a"), launch("named-a"), launch("named-b")
	if *a1.PID != *a2.PID || *b.PID == *a1.PID || b.UserDataDir == a1.UserDataDir {
		t.Fatalf("a1 %d a2 %d b %d", *a1.PID, *a2.PID, *b.PID)
	}
}

func TestTwoProcessesOnOneProfileNeverShareAnEphemeralProfile(t *testing.T) {
	homeDir, launch := orderLaunch(t)
	c := startChild(t, "default-udd", "CHILD_KEY=browser-exec-default", "CHILD_HOME_KEY="+cfgOf(childEnvFor(homeDir)).homeKey())
	other := c.line()
	os.MkdirAll(filepath.Join(other, "Default"), 0o755)
	os.WriteFile(filepath.Join(other, "Default", "Cookies"), []byte("the other process's session"), 0o644)
	mine := launch("browser-exec-default")
	if mine.UserDataDir == other {
		t.Fatal("both processes got one throwaway profile")
	}
	if raw, _ := os.ReadFile(filepath.Join(other, "Default", "Cookies")); string(raw) != "the other process's session" {
		t.Fatal("the launch wiped the other process's profile")
	}
}

func TestStopSignalsTheBrowserBeforeItsHelpersAndRemovesTheEphemeralProfile(t *testing.T) {
	_, launch := orderLaunch(t)
	b := launch("stop-me")
	orderLog := filepath.Join(b.UserDataDir, "order.log")
	snapshot := filepath.Join(filepath.Dir(filepath.Dir(orderLog)), "order-snapshot.log")
	pin(t, &launcherRemoveAll, func(path string) {
		if path == b.UserDataDir && lexists(orderLog) {
			raw, _ := os.ReadFile(orderLog)
			os.WriteFile(snapshot, raw, 0o644)
		}
		rmtree(path)
	})
	stopBrowser(context.Background(), "stop-me")
	raw, _ := os.ReadFile(snapshot)
	if got := strings.Fields(string(raw)); !slices.Equal(got, []string{"parent", "child"}) {
		t.Fatalf("order %q", got)
	}
	if lexists(b.UserDataDir) || getBrowser("stop-me") != nil {
		t.Fatal("the stopped browser left its profile or registration")
	}
}

func TestADeadBrowserIsDroppedSoTheNextLaunchStartsANewOne(t *testing.T) {
	_, launch := orderLaunch(t)
	first := launch("crashy")
	syscall.Kill(int(*first.PID), syscall.SIGKILL)
	launcherMu.Lock()
	p := procs["crashy"]
	launcherMu.Unlock()
	p.wait(5 * time.Second)
	if getBrowser("crashy") != nil {
		t.Fatal("a dead browser is still registered")
	}
	if second := launch("crashy"); *second.PID == *first.PID {
		t.Fatal("the relaunch reused the dead browser")
	}
}

func TestAStopRacingARelaunchNeverForgetsTheReplacement(t *testing.T) {
	_, launch := orderLaunch(t)
	old := launch("racy")
	var relaunched *LaunchedBrowser
	worker := make(chan struct{})
	real := launcherTerminate
	launcherTerminate = func(pid, startTime *int64, p *launchProc) {
		real(pid, startTime, p)
		launcherTerminate = real
		go func() { defer close(worker); relaunched = launch("racy") }()
		select {
		case <-worker:
		case <-time.After(2 * time.Second):
		}
	}
	t.Cleanup(func() { launcherTerminate = real })
	stopBrowser(context.Background(), "racy")
	select {
	case <-worker:
	case <-time.After(60 * time.Second):
		t.Fatal("the relaunch never finished")
	}
	if relaunched == nil || *relaunched.PID == *old.PID {
		t.Fatal("no replacement browser")
	}
	if getBrowser("racy") != relaunched {
		real(relaunched.PID, relaunched.StartTime, nil)
		t.Fatal("the stop forgot the replacement browser")
	}
	if !lexists(filepath.Join(relaunched.UserDataDir, devtoolsFile)) {
		t.Fatal("the stop deleted the replacement's profile")
	}
}

func TestLaunchFailureReportsTheExitAndLeavesNothingRegistered(t *testing.T) {
	env := launcherTestEnv(t)
	broken := fakeExe(t, "exit-code", "FAKE_EXIT=7", "FAKE_STDERR='No usable sandbox!'")
	b, err := launchEphemeral(env, "broken", LaunchOptions{Executable: broken, Timeout: durp(10 * time.Second)})
	if b != nil || !strings.Contains(err, "exited with code 7") || !strings.Contains(err, "--no-sandbox") {
		t.Fatalf("launch %v %q", b, err)
	}
	if getBrowser("broken") != nil {
		t.Fatal("a failed launch is registered")
	}
}

func TestABrowserThatExitedDuringStartupIsNotSignalled(t *testing.T) {
	env := launcherTestEnv(t)
	logs := captureLogs(t)
	broken := fakeExe(t, "exit-code", "FAKE_EXIT=21")
	b, err := launchEphemeral(env, "dies", LaunchOptions{Executable: broken, Timeout: durp(10 * time.Second)})
	if b != nil || !strings.Contains(err, "exited with code 21") {
		t.Fatalf("launch %v %q", b, err)
	}
	if warned := logs.atLeastWarn(); len(warned) != 0 {
		t.Fatalf("warnings %q", warned)
	}
}

func writeOrphanRecord(t *testing.T, records string, pid int, udd string) string {
	t.Helper()
	return writeStateJSON(t, filepath.Join(records, fmt.Sprintf("orphan-%d.json", pid)), map[string]any{
		"owner_pid": deadPid(t), "owner_start_time": nil, "pid": pid, "start_time": optInt(startTimeOf(pid)),
		"user_data_dir": udd, "home_key": "x", "ephemeral": false, "ns": nsValue()})
}

func TestOrphanReapRefusesAPidWhoseCmdlineLacksTheRecordedUserDataDir(t *testing.T) {
	env := launcherTestEnv(t)
	stranger := startProc(t, "sleep", "3001")
	record := writeOrphanRecord(t, homeRecords(t, env), stranger.cmd.Process.Pid, filepath.Join(t.TempDir(), "some-profile"))
	if n := reapOrphanedBrowsers(context.Background(), nil); n != 0 {
		t.Fatalf("reaped %d", n)
	}
	if !stranger.running() || lexists(record) {
		t.Fatal("the stranger was signalled or its stale record kept")
	}
}

func TestOrphanReapStopsACrashedOwnersBrowser(t *testing.T) {
	env := launcherTestEnv(t)
	udd := filepath.Join(t.TempDir(), "orphan-profile")
	fake := startFake(t, fakeExe(t, "order-chromium"), udd)
	writeOrphanRecord(t, homeRecords(t, env), fake.cmd.Process.Pid, udd)
	if n := reapOrphanedBrowsers(context.Background(), nil); n != 1 {
		t.Fatalf("reaped %d", n)
	}
	select {
	case <-fake.done:
	case <-time.After(10 * time.Second):
		t.Fatal("the orphan survived")
	}
}

func TestRecordsOfALiveOwnerAreLeftAlone(t *testing.T) {
	homeDir, launch := orderLaunch(t)
	launch("mine")
	if n := reapOrphanedBrowsers(context.Background(), nil); n != 0 || getBrowser("mine") == nil {
		t.Fatalf("reaped %d", n)
	}
	if len(globNames(recordsDir(homeDir), "*.json")) == 0 {
		t.Fatal("the live owner's record was removed")
	}
}

func TestASharedProfileBrowserOutlivesItsLauncherWhileAnotherProcessHoldsIt(t *testing.T) {
	env := launcherTestEnv(t)
	exe := fakeExe(t, "order-chromium")
	udd := filepath.Join(t.TempDir(), "shared-profile")
	mine, err := launchShared(env, exe, udd)
	if err != "" || !mine.Owned {
		t.Fatalf("launch %q", err)
	}
	defer killFake(*mine.PID)
	holder, _, owned := profileUser(t, exe, udd, "CHILD_RUNNING="+strconv.FormatInt(*mine.PID, 10))
	if owned {
		t.Fatal("the holder claims to own the browser")
	}
	stopBrowser(context.Background(), "profile:"+udd)
	if !launcherPidAlive(int(*mine.PID)) {
		t.Fatal("the launcher let go of a browser another process still drives")
	}
	holder.finish(t)
	gone(t, *mine.PID)
	if left := statefulNames(sharedStateDir(udd)); len(left) != 0 {
		t.Fatalf("launcher state left %v", left)
	}
}

func statefulNames(dir string) []string {
	var out []string
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), ".") {
			out = append(out, e.Name())
		}
	}
	return out
}

func TestACrashedLaunchersSharedBrowserIsReapedOnlyAfterItsLastHolderIsGone(t *testing.T) {
	launcherTestEnv(t)
	exe := fakeExe(t, "order-chromium")
	udd := filepath.Join(t.TempDir(), "shared-profile")
	owner, pid, _ := profileUser(t, exe, udd)
	defer killFake(pid)
	holder, _, _ := profileUser(t, exe, udd, "CHILD_RUNNING="+strconv.FormatInt(pid, 10))
	owner.kill()
	if n := reapOrphanedBrowsers(context.Background(), nil); n != 0 || !launcherPidAlive(int(pid)) {
		t.Fatalf("reaped %d while a live process still holds it", n)
	}
	holder.kill()
	if n := reapOrphanedBrowsers(context.Background(), nil); n != 1 {
		t.Fatalf("reaped %d", n)
	}
	gone(t, pid)
}

func TestAThrowawayProfileLeftByAProcessThatDiedMidLaunchIsReclaimed(t *testing.T) {
	launcherTestEnv(t)
	died := startChild(t, "prepare-dir")
	diedDir := died.line()
	died.finish(t)
	alive := startChild(t, "prepare-dir", "CHILD_HOLD=1")
	held := alive.line()
	if !isDir(diedDir) || !isDir(held) || filepath.Dir(diedDir) != filepath.Dir(held) {
		t.Fatalf("dirs %s %s", diedDir, held)
	}
	reapOrphanedBrowsers(context.Background(), nil)
	if lexists(diedDir) || !isDir(held) {
		t.Fatal("the dead owner's dir survived or the live one's was removed")
	}
	alive.finish(t)
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func TestALauncherKilledDuringTheReadinessWaitLeavesItsChromiumToTheReaper(t *testing.T) {
	env := launcherTestEnv(t)
	slow := fakeExe(t, "never-ready")
	icebath := startChild(t, "waiting-launcher", "CHILD_EXE="+slow)
	udd := icebath.line()
	var pid int64
	waitUntil(t, 30*time.Second, "the launched Chromium to start", func() bool {
		raw, err := os.ReadFile(filepath.Join(udd, "pid"))
		pid, _ = strconv.ParseInt(string(raw), 10, 64)
		return err == nil && pid > 0
	})
	defer killFake(pid)
	records := homeRecords(t, env)
	waitUntil(t, 30*time.Second, "the starting Chromium's launch record", func() bool {
		for _, name := range globNames(records, "*.json") {
			if r := loadStateFile(filepath.Join(records, name)); r != nil && pyEqualInt(objGet(r, "pid"), pid) {
				return true
			}
		}
		return false
	})
	icebath.kill()
	if !launcherPidAlive(int(pid)) {
		t.Fatal("the starting Chromium died with its launcher")
	}
	if n := reapOrphanedBrowsers(context.Background(), nil); n != 1 {
		t.Fatalf("a Chromium whose launcher died mid-launch has no record, reaped %d", n)
	}
	gone(t, pid)
}

func pyEqualInt(v any, n int64) bool {
	got, ok := jsonInt(v)
	return ok && got == n
}

func TestAReapDuringThisProcesssOwnReadinessWaitLeavesTheStartingChromium(t *testing.T) {
	env := launcherTestEnv(t)
	slow := fakeExe(t, "never-ready")
	type result struct {
		b   *LaunchedBrowser
		err string
	}
	out := make(chan result, 1)
	go func() {
		b, err := launchEphemeral(env, "slow", LaunchOptions{Executable: slow, Headed: boolp(false), Timeout: durp(4 * time.Second)})
		out <- result{b, err}
	}()
	records := homeRecords(t, env)
	var pid int
	waitUntil(t, 30*time.Second, "the starting Chromium's launch record", func() bool {
		launcherMu.Lock()
		defer launcherMu.Unlock()
		if len(launching.keys) == 0 || len(globNames(records, "*.json")) == 0 {
			return false
		}
		pid, _ = strconv.Atoi(launching.keys[0])
		return true
	})
	if n := reapOrphanedBrowsers(context.Background(), nil); n != 0 || !launcherPidAlive(pid) {
		t.Fatalf("the janitor killed this process's own starting browser, reaped %d", n)
	}
	r := <-out
	if r.b != nil || !strings.Contains(r.err, "in time") {
		t.Fatalf("launch %v %q", r.b, r.err)
	}
	launcherMu.Lock()
	still := len(launching.keys)
	launcherMu.Unlock()
	if still != 0 || len(globNames(records, "*.json")) != 0 {
		t.Fatal("the abandoned launch left state behind")
	}
}

const (
	otherHost = "container-3f9c2a"
	oldNS     = "4026531836:00000000-0000-0000-0000-000000000000"
	liveNS    = "4026532999:11111111-1111-1111-1111-111111111111"
	foreignNS = oldNS
)

func asContainerBrowser(udd string, pid int) {
	lock := filepath.Join(udd, "SingletonLock")
	os.Remove(lock)
	os.Symlink(fmt.Sprintf("%s-%d", otherHost, pid), lock)
}

func identityRecord(t *testing.T, udd string, pid int, tag, ns string, startTime int, devtools string) string {
	data := map[string]any{"owner_pid": pid + 1, "owner_start_time": 1, "pid": pid, "start_time": startTime,
		"user_data_dir": udd, "home_key": "elsewhere", "ephemeral": false, "ns": ns}
	if devtools != "" {
		data["devtools"] = devtools
	}
	return writeStateJSON(t, filepath.Join(sharedStateDir(udd), fmt.Sprintf("%s-%d%s.json", profileDigest(udd)[:16], pid, tag)), data)
}

func ownerSees(t *testing.T, record, ns string, startTime int) int {
	c := startChild(t, "owner-view", "CHILD_RECORD="+record, "CHILD_NS="+ns, "CHILD_START="+strconv.Itoa(startTime))
	n, err := strconv.Atoi(c.line())
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestAnOlderRecordOnTheSamePidDoesNotTakeTheLiveBrowsersHolders(t *testing.T) {
	for _, oldDevtools := range []string{"/devtools/browser/1", ""} {
		t.Run(map[bool]string{true: "old-launch-was-ready", false: "old-launch-never-ready"}[oldDevtools != ""], func(t *testing.T) {
			env := launcherTestEnv(t)
			script := fakeExe(t, "serving-chromium")
			udd := filepath.Join(t.TempDir(), "pinned-profile")
			key := "profile:" + udd
			theirs := startFake(t, script, udd)
			defer stopBrowser(context.Background(), key)
			pid := deadPid(t)
			asContainerBrowser(udd, pid)
			raw, _ := os.ReadFile(filepath.Join(udd, devtoolsFile))
			livePath := strings.Fields(string(raw))[1]
			old := identityRecord(t, udd, pid, "-n000000000000", oldNS, 111, oldDevtools)
			live := identityRecord(t, udd, pid, "-nffffffffffff", liveNS, 222, livePath)
			oldBytes, _ := os.ReadFile(old)
			b, err := launchShared(env, script, udd)
			if err != "" || b == nil || !b.Foreign {
				t.Fatalf("launch %v %q", b, err)
			}
			if ownerSees(t, live, liveNS, 222) != 1 {
				t.Fatal("the live browser's owner does not see the attacher")
			}
			if len(globNames(filepath.Dir(old), stem(old)+"@*.lease")) != 0 {
				t.Fatal("the attacher leased an earlier launch")
			}
			if now, _ := os.ReadFile(old); string(now) != string(oldBytes) {
				t.Fatal("the earlier launch's record was rewritten")
			}
			stopBrowser(context.Background(), key)
			if len(globNames(filepath.Dir(live), "*.lease")) != 0 || !theirs.running() {
				t.Fatal("a lease was left or the other environment's browser was stopped")
			}
		})
	}
}

func TestStoppingOneOfTwoSamePidBrowsersDropsOnlyItsOwnLease(t *testing.T) {
	env := launcherTestEnv(t)
	script := fakeExe(t, "serving-chromium")
	pid := deadPid(t)
	root := t.TempDir()
	profiles := []string{filepath.Join(root, "profile-a"), filepath.Join(root, "profile-b")}
	var fakes []*started
	for _, udd := range profiles {
		fakes = append(fakes, startFake(t, script, udd))
	}
	for _, udd := range profiles {
		asContainerBrowser(udd, pid)
	}
	for _, udd := range profiles {
		b, err := launchShared(env, script, udd)
		if err != "" || b == nil || !b.Foreign || *b.PID != int64(pid) {
			t.Fatalf("launch %v %q", b, err)
		}
	}
	states := []string{sharedStateDir(profiles[0]), sharedStateDir(profiles[1])}
	for _, s := range states {
		if len(globNames(s, "*.lease")) == 0 {
			t.Fatalf("no lease in %s", s)
		}
	}
	stopBrowser(context.Background(), "profile:"+profiles[0])
	if len(globNames(states[0], "*.lease")) != 0 {
		t.Fatal("the stopped session left its lease: its browser is held forever")
	}
	if len(globNames(states[1], "*.lease")) == 0 || getBrowser("profile:"+profiles[1]) == nil {
		t.Fatal("stopping one browser dropped the other one's lease")
	}
	for _, f := range fakes {
		if !f.running() {
			t.Fatal("a fake browser was stopped")
		}
	}
}

func TestALeaseOnAnEarlierLaunchDoesNotKeepTheCurrentBrowserUp(t *testing.T) {
	env := launcherTestEnv(t)
	script := fakeExe(t, "serving-chromium")
	udd := filepath.Join(t.TempDir(), "pinned-profile")
	mine, err := launchShared(env, script, udd)
	if err != "" || !mine.Owned {
		t.Fatalf("launch %q", err)
	}
	defer killFake(*mine.PID)
	record, goneHere := recordPath(mine), deadPid(t)
	stale := writeStateJSON(t, filepath.Join(filepath.Dir(record), fmt.Sprintf("%s@%d-nffffffffffff.lease", stem(record), goneHere)),
		map[string]any{"holder_pid": goneHere, "holder_start_time": nil, "pid": *mine.PID, "start_time": *mine.StartTime - 1,
			"user_data_dir": udd, "ns": liveNS})
	stopBrowser(context.Background(), "profile:"+udd)
	gone(t, *mine.PID)
	if lexists(record) || lexists(stale) {
		t.Fatal("the record or the stale lease survived")
	}
}

func TestStateFromAnotherNamespaceIsNeverJudgedDead(t *testing.T) {
	for _, ns := range []any{foreignNS, nil} {
		t.Run(fmt.Sprint(ns), func(t *testing.T) {
			env := launcherTestEnv(t)
			records, throwaway := homeRecords(t, env), ephemeralRoot(env.Home)
			mkdirLeaf(throwaway, 0o700)
			owner, browser, holder := deadPid(t), deadPid(t), deadPid(t)
			suffix := ""
			if ns != nil {
				suffix = "-nffffffffffff"
			}
			withNS := func(data map[string]any) map[string]any {
				if ns != nil {
					data["ns"] = ns
				}
				return data
			}
			profile := filepath.Join(throwaway, fmt.Sprintf("%s-%d-1%s", strings.Repeat("a", 16), browser, suffix))
			os.MkdirAll(filepath.Join(profile, "Default"), 0o755)
			orphanDir := filepath.Join(throwaway, fmt.Sprintf("%s-%d-1%s", strings.Repeat("b", 16), owner, suffix))
			os.Mkdir(orphanDir, 0o755)
			record := writeStateJSON(t, filepath.Join(records, fmt.Sprintf("%s-%d%s.json", strings.Repeat("a", 16), browser, suffix)),
				withNS(map[string]any{"owner_pid": owner, "owner_start_time": nil, "pid": browser, "start_time": 1,
					"user_data_dir": profile, "home_key": "x", "ephemeral": true}))
			lease := writeStateJSON(t, filepath.Join(records, fmt.Sprintf("%s-%d%s@%d%s.lease", strings.Repeat("c", 16), browser, suffix, holder, suffix)),
				withNS(map[string]any{"holder_pid": holder, "holder_start_time": nil, "pid": browser, "start_time": 1}))
			if n := reapOrphanedBrowsers(context.Background(), nil); n != 0 {
				t.Fatalf("reaped %d", n)
			}
			if !lexists(record) || !isDir(filepath.Join(profile, "Default")) || !isDir(orphanDir) || !lexists(lease) {
				t.Fatal("state from another namespace was judged dead")
			}
		})
	}
}

func TestAHolderInAnotherNamespaceKeepsTheSharedBrowserUp(t *testing.T) {
	env := launcherTestEnv(t)
	script := fakeExe(t, "serving-chromium")
	udd := filepath.Join(t.TempDir(), "pinned-profile")
	mine, err := launchShared(env, script, udd)
	if err != "" || !mine.Owned {
		t.Fatalf("launch %q", err)
	}
	defer killFake(*mine.PID)
	record, goneHere := recordPath(mine), deadPid(t)
	foreign := writeStateJSON(t, filepath.Join(filepath.Dir(record), fmt.Sprintf("%s@%d-nffffffffffff.lease", stem(record), goneHere)),
		map[string]any{"holder_pid": goneHere, "holder_start_time": nil, "pid": *mine.PID, "start_time": *mine.StartTime,
			"user_data_dir": udd, "ns": foreignNS})
	stopBrowser(context.Background(), "profile:"+udd)
	if !launcherPidAlive(int(*mine.PID)) {
		t.Fatal("stopped a browser a holder in another namespace may still drive")
	}
	if n := reapOrphanedBrowsers(context.Background(), nil); n != 0 || !launcherPidAlive(int(*mine.PID)) || !lexists(foreign) || !lexists(record) {
		t.Fatalf("reaped %d", n)
	}
}

func TestAnAttacherFromAnotherNamespaceIsSeenByTheOwner(t *testing.T) {
	env := launcherTestEnv(t)
	script := fakeExe(t, "serving-chromium")
	udd := filepath.Join(t.TempDir(), "pinned-profile")
	mine, err := launchShared(env, script, udd)
	if err != "" || !mine.Owned {
		t.Fatalf("launch %q", err)
	}
	defer killFake(*mine.PID)
	holder, pid, owned := profileUser(t, script, udd, "CHILD_NS="+foreignNS)
	if pid != *mine.PID || owned {
		t.Fatalf("holder saw %d %v", pid, owned)
	}
	record := recordPath(mine)
	sum := sha256.Sum256([]byte(foreignNS))
	want := fmt.Sprintf("%s@%d-n%s.lease", stem(record), holder.cmd.Process.Pid, hex.EncodeToString(sum[:])[:12])
	if got := globNames(filepath.Dir(record), "*.lease"); !slices.Equal(got, []string{want}) {
		t.Fatalf("leases %v want %s", got, want)
	}
	stopBrowser(context.Background(), "profile:"+udd)
	if !launcherPidAlive(int(*mine.PID)) {
		t.Fatal("the owner stopped a browser another namespace's holder still drives")
	}
	if n := reapOrphanedBrowsers(context.Background(), nil); n != 0 || !launcherPidAlive(int(*mine.PID)) {
		t.Fatalf("reaped %d", n)
	}
	holder.finish(t)
	if len(globNames(filepath.Dir(record), "*.lease")) != 0 {
		t.Fatal("the attacher left its lease behind")
	}
	if n := reapOrphanedBrowsers(context.Background(), nil); n != 1 {
		t.Fatalf("reaped %d", n)
	}
	gone(t, *mine.PID)
}

func TestADeadHolderInThisNamespaceIsStillDropped(t *testing.T) {
	env := launcherTestEnv(t)
	script := fakeExe(t, "serving-chromium")
	udd := filepath.Join(t.TempDir(), "pinned-profile")
	mine, err := launchShared(env, script, udd)
	if err != "" || !mine.Owned {
		t.Fatalf("launch %q", err)
	}
	defer killFake(*mine.PID)
	holder, pid, owned := profileUser(t, script, udd)
	if pid != *mine.PID || owned {
		t.Fatalf("holder saw %d %v", pid, owned)
	}
	record := recordPath(mine)
	if len(globNames(filepath.Dir(record), "*.lease")) == 0 {
		t.Fatal("the holder wrote no lease")
	}
	holder.kill()
	stopBrowser(context.Background(), "profile:"+udd)
	gone(t, *mine.PID)
	if len(globNames(filepath.Dir(record), "*.lease")) != 0 || lexists(record) {
		t.Fatal("the dead holder's lease or the record survived")
	}
}

func launcherState(homes ...string) []string {
	roots := []string{sharedProfilesRoot()}
	for _, h := range homes {
		roots = append(roots, recordsDir(h))
	}
	var out []string
	for _, root := range roots {
		filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err == nil && (strings.HasSuffix(path, ".json") || strings.HasSuffix(path, ".lease")) {
				out = append(out, d.Name())
			}
			return nil
		})
	}
	slices.Sort(out)
	return out
}

func TestTwoHomesPinningOneProfileShareItsBrowserUntilTheLastOneLeaves(t *testing.T) {
	launcherTestEnv(t)
	script := fakeExe(t, "serving-chromium")
	root := t.TempDir()
	udd, h1, h2 := filepath.Join(root, "pinned-profile"), filepath.Join(root, "home-1"), filepath.Join(root, "home-2")
	os.MkdirAll(h1, 0o755)
	os.MkdirAll(h2, 0o755)
	a, pid, aOwned := profileUser(t, script, udd, "ICEBATH_HOME="+h1)
	defer killFake(pid)
	b, bPid, bOwned := profileUser(t, script, udd, "ICEBATH_HOME="+h2)
	if !aOwned || bOwned || bPid != pid {
		t.Fatalf("owned %v %v pid %d %d", aOwned, bOwned, pid, bPid)
	}
	a.finish(t)
	if !launcherPidAlive(int(pid)) {
		t.Fatal("the other home's launcher stopped a browser this home still holds")
	}
	b.finish(t)
	gone(t, pid)
	if left := launcherState(h1, h2); len(left) != 0 {
		t.Fatalf("launcher state left %v", left)
	}
	if got := notedPids(udd, "launches"); !slices.Equal(got, []int{int(pid)}) {
		t.Fatalf("launches %v", got)
	}
}

func TestSharedProfileStateLivesInAPrivatePerUserDirOutsideEveryHome(t *testing.T) {
	env := launcherTestEnv(t)
	script := fakeExe(t, "serving-chromium")
	udd := filepath.Join(t.TempDir(), "pinned-profile")
	b, err := launchShared(env, script, udd)
	if err != "" {
		t.Fatal(err)
	}
	defer killFake(*b.PID)
	defer stopBrowser(context.Background(), "profile:"+udd)
	record := recordPath(b)
	root := sharedProfilesRoot()
	if !lexists(record) || filepath.Dir(filepath.Dir(record)) != root || strings.HasPrefix(record, env.Home+string(filepath.Separator)) {
		t.Fatalf("record %s root %s", record, root)
	}
	for _, state := range []string{root, filepath.Dir(record)} {
		info, err := os.Lstat(state)
		if err != nil || info.Mode().Perm() != 0o700 || info.Mode()&os.ModeSymlink != 0 {
			t.Fatalf("%s %v %v", state, info, err)
		}
	}
}

func TestACrashedLaunchersSharedBrowserIsReapedFromAnotherHome(t *testing.T) {
	launcherTestEnv(t)
	script := fakeExe(t, "serving-chromium")
	root := t.TempDir()
	udd, h1 := filepath.Join(root, "pinned-profile"), filepath.Join(root, "home-1")
	os.MkdirAll(h1, 0o755)
	owner, pid, _ := profileUser(t, script, udd, "ICEBATH_HOME="+h1)
	defer killFake(pid)
	owner.kill()
	if !launcherPidAlive(int(pid)) {
		t.Fatal("the browser died with its launcher")
	}
	if n := reapOrphanedBrowsers(context.Background(), nil); n != 1 {
		t.Fatalf("reaped %d", n)
	}
	gone(t, pid)
}

func TestOneProcessHoldingAProfileUnderTwoKeysKeepsItUntilBothLetGo(t *testing.T) {
	env := launcherTestEnv(t)
	script := fakeExe(t, "serving-chromium")
	root := t.TempDir()
	udd, alias := filepath.Join(root, "pinned-profile"), filepath.Join(root, "alias")
	os.Symlink(udd, alias)
	first, err := launchShared(env, script, udd)
	if err != "" || !first.Owned {
		t.Fatalf("launch %q", err)
	}
	defer killFake(*first.PID)
	second, err := launchShared(env, script, alias)
	if err != "" || *second.PID != *first.PID || second.Owned {
		t.Fatalf("second %v %q", second, err)
	}
	stopBrowser(context.Background(), "profile:"+udd)
	if !launcherPidAlive(int(*first.PID)) {
		t.Fatal("dropping one key stopped the browser the other key still drives")
	}
	if getBrowser("profile:"+alias) != second {
		t.Fatal("the alias key lost its browser")
	}
	stopBrowser(context.Background(), "profile:"+alias)
	gone(t, *first.PID)
	if left := launcherState(); len(left) != 0 {
		t.Fatalf("launcher state left %v", left)
	}
}

func racer(t *testing.T, script, udd string, env ...string) *child {
	t.Helper()
	c := startChild(t, "profile-user", append([]string{"CHILD_EXE=" + script, "CHILD_UDD=" + udd, "CHILD_RACER=1"}, env...)...)
	if l := c.line(); l != "ready" {
		t.Fatalf("racer said %q", l)
	}
	return c
}

func TestTwoProcessesRacingAFirstLaunchGetOneBrowser(t *testing.T) {
	launcherTestEnv(t)
	script := fakeExe(t, "serving-chromium")
	udd := filepath.Join(t.TempDir(), "pinned-profile")
	racers := []*child{racer(t, script, udd, "FAKE_CHROMIUM_READY_DELAY=2"), racer(t, script, udd, "FAKE_CHROMIUM_READY_DELAY=2")}
	defer func() {
		for _, pid := range notedPids(udd, "spawns") {
			killFake(int64(pid))
		}
	}()
	for _, r := range racers {
		r.stdin.Write([]byte("go\n"))
	}
	var results [][]string
	for _, r := range racers {
		results = append(results, strings.SplitN(r.line(), " ", 3))
	}
	for _, r := range results {
		if r[0] == "ERR" || len(r) != 3 {
			t.Fatalf("results %q", results)
		}
	}
	if results[0][0] != results[1][0] || results[0][2] != results[1][2] {
		t.Fatalf("results %q", results)
	}
	owned := []string{results[0][1], results[1][1]}
	slices.Sort(owned)
	if !slices.Equal(owned, []string{"False", "True"}) {
		t.Fatalf("results %q", results)
	}
	pid, _ := strconv.Atoi(results[0][0])
	if got := notedPids(udd, "spawns"); !slices.Equal(got, []int{pid}) {
		t.Fatalf("a second Chromium was started on the same profile %v", got)
	}
	for _, r := range racers {
		r.finish(t)
	}
	gone(t, int64(pid))
}

func TestAnOwnersStopCannotFallBetweenTheAttachProbeAndTheLease(t *testing.T) {
	env := launcherTestEnv(t)
	script := fakeExe(t, "serving-chromium")
	udd := filepath.Join(t.TempDir(), "pinned-profile")
	owner := racer(t, script, udd)
	owner.stdin.Write([]byte("go\n"))
	pid, _ := strconv.ParseInt(strings.Fields(owner.line())[0], 10, 64)
	defer killFake(pid)
	exited := make(chan struct{})
	go func() { owner.cmd.Wait(); close(exited) }()
	fired := false
	pin(t, &launcherEndpointLive, func(port int, path string) bool {
		live := devtoolsEndpointLive(port, path)
		if live && !fired {
			fired = true
			owner.stdin.Write([]byte("\n"))
			select {
			case <-exited:
			case <-time.After(3 * time.Second):
			}
		}
		return live
	})
	attached, err := launchShared(env, script, udd)
	if !fired || err != "" || *attached.PID != pid || attached.Owned {
		t.Fatalf("attach %v %q fired %v", attached, err, fired)
	}
	select {
	case <-exited:
	case <-time.After(30 * time.Second):
		t.Fatal("the owner never exited")
	}
	close(owner.done)
	if !launcherPidAlive(int(pid)) {
		t.Fatal("the owner stopped the browser this process had just attached to")
	}
	stopBrowser(context.Background(), "profile:"+udd)
	gone(t, pid)
}

func TestTheReaperSkipsAProfileWhoseFirstLaunchIsStillStarting(t *testing.T) {
	launcherTestEnv(t)
	script := fakeExe(t, "serving-chromium")
	udd := filepath.Join(t.TempDir(), "pinned-profile")
	starter := racer(t, script, udd, "FAKE_CHROMIUM_READY_DELAY=600")
	defer func() {
		starter.kill()
		for _, pid := range notedPids(udd, "spawns") {
			killFake(int64(pid))
		}
	}()
	starter.stdin.Write([]byte("go\n"))
	waitUntil(t, 30*time.Second, "the first launch's Chromium to start", func() bool { return len(notedPids(udd, "launches")) == 1 })
	pid := notedPids(udd, "launches")[0]
	reaped := make(chan struct{})
	go func() { reapOrphanedBrowsers(context.Background(), nil); close(reaped) }()
	select {
	case <-reaped:
	case <-time.After(20 * time.Second):
		t.Fatal("the reaper is blocked behind a launch still in its readiness wait")
	}
	if !launcherPidAlive(pid) {
		t.Fatal("the reaper stopped the starting browser")
	}
}

func deadOwnersDir(t *testing.T) string {
	t.Helper()
	c := startChild(t, "prepare-dir")
	udd := c.line()
	c.finish(t)
	return udd
}

func startBrowserOn(t *testing.T, script, udd string) *started {
	t.Helper()
	s := startProc(t, script, "--user-data-dir="+udd)
	waitUntil(t, 15*time.Second, "the fake browser to write DevToolsActivePort", func() bool {
		return lexists(filepath.Join(udd, devtoolsFile)) || !s.running()
	})
	if !s.running() {
		t.Fatal("the fake browser exited during startup")
	}
	return s
}

func TestAnUnrecordedLiveBrowsersThrowawayProfileSurvivesTheReclaim(t *testing.T) {
	launcherTestEnv(t)
	script := fakeExe(t, "serving-chromium")
	udd := deadOwnersDir(t)
	p := startBrowserOn(t, script, udd)
	reapOrphanedBrowsers(context.Background(), nil)
	if !isDir(udd) || !p.running() {
		t.Fatal("the reaper deleted the profile of a Chromium still running on it")
	}
	p.cmd.Process.Signal(syscall.SIGTERM)
	<-p.done
	reapOrphanedBrowsers(context.Background(), nil)
	if lexists(udd) {
		t.Fatal("the dead owner's profile survived once its browser stopped")
	}
}

func TestALiveBrowsersProfileSurvivesWhenItsRecordCannotProveThePid(t *testing.T) {
	env := launcherTestEnv(t)
	script := fakeExe(t, "serving-chromium")
	udd := deadOwnersDir(t)
	p := startBrowserOn(t, script, udd)
	one := int64(1)
	writeRecord(context.Background(), &LaunchedBrowser{Key: "lost", PID: int64p(p.cmd.Process.Pid), StartTime: &one, UserDataDir: udd, Owned: true,
		Ephemeral: true, HomeKey: cfgOf(env).homeKey()})
	if n := reapOrphanedBrowsers(context.Background(), nil); n != 0 || !p.running() {
		t.Fatalf("signalled on an unproven pid, reaped %d", n)
	}
	if !isDir(udd) {
		t.Fatal("the reaper deleted the profile of a Chromium still running on it")
	}
}

func crashingHolder(t *testing.T, script, udd string) (int64, bool) {
	t.Helper()
	c, pid, owned := profileUser(t, script, udd)
	c.kill()
	return pid, owned
}

func TestABrowserAdoptedThroughASymlinkIsStillReapedAfterItsAdopterCrashes(t *testing.T) {
	launcherTestEnv(t)
	script := fakeExe(t, "serving-chromium")
	root := t.TempDir()
	udd, alias := filepath.Join(root, "profile"), filepath.Join(root, "alias")
	os.Symlink(udd, alias)
	defer func() {
		for _, pid := range notedPids(udd, "spawns") {
			killFake(int64(pid))
		}
	}()
	pid, owned := crashingHolder(t, script, udd)
	if !owned {
		t.Fatal("the first holder did not launch the browser")
	}
	if adopted, owned := crashingHolder(t, script, alias); adopted != pid || owned {
		t.Fatalf("adopter saw %d %v", adopted, owned)
	}
	if n := reapOrphanedBrowsers(context.Background(), nil); n != 1 {
		t.Fatalf("reaped %d", n)
	}
	gone(t, pid)
	if left := globNames(sharedStateDir(udd), "*.json"); len(left) != 0 {
		t.Fatalf("a launch record outlived its reaped browser %v", left)
	}
}

func TestAPausedSharedBrowserIsRefusedThenAttachedOnceItAnswersAgain(t *testing.T) {
	env := launcherTestEnv(t)
	script := fakeExe(t, "serving-chromium")
	udd := filepath.Join(t.TempDir(), "pinned-profile")
	b := startFake(t, script, udd)
	pid := b.cmd.Process.Pid
	portFile, _ := os.ReadFile(filepath.Join(udd, devtoolsFile))
	attach := func() (*LaunchedBrowser, string) {
		return launchChromium(context.Background(), env, "profile:"+udd, LaunchOptions{Executable: script, UserDataDir: udd, Headed: boolp(false),
			ProcEnv: environ(), Timeout: durp(3 * time.Second), SandboxBypass: true})
	}
	syscall.Kill(pid, syscall.SIGSTOP)
	attached, err := attach()
	syscall.Kill(pid, syscall.SIGCONT)
	if attached != nil || !strings.Contains(err, strconv.Itoa(pid)) {
		t.Fatalf("attach %v %q", attached, err)
	}
	if now, _ := os.ReadFile(filepath.Join(udd, devtoolsFile)); string(now) != string(portFile) {
		t.Fatal("the live browser's endpoint file was wiped")
	}
	if got := notedPids(udd, "spawns"); !slices.Equal(got, []int{pid}) {
		t.Fatalf("a second Chromium was started on the live browser's profile %v", got)
	}
	attached, err = attach()
	if err != "" || *attached.PID != int64(pid) || attached.Owned {
		t.Fatalf("attach %v %q", attached, err)
	}
	stopBrowser(context.Background(), "profile:"+udd)
}

func relock(udd, host string, pid int) {
	lock := filepath.Join(udd, "SingletonLock")
	os.Remove(lock)
	os.Symlink(fmt.Sprintf("%s-%d", host, pid), lock)
}

func TestAReachableBrowserFromAnotherEnvironmentIsAttachedAndNeverStopped(t *testing.T) {
	env := launcherTestEnv(t)
	script := fakeExe(t, "serving-chromium")
	udd := filepath.Join(t.TempDir(), "pinned-profile")
	key := "profile:" + udd
	theirs := startFake(t, script, udd)
	relock(udd, otherHost, deadPid(t))
	portFile, _ := os.ReadFile(filepath.Join(udd, devtoolsFile))
	b, err := launchShared(env, script, udd)
	if err != "" || b == nil || b.Owned {
		t.Fatalf("launch %v %q", b, err)
	}
	fields := strings.Fields(string(portFile))
	if b.CDPURL != "ws://127.0.0.1:"+fields[0]+fields[1] {
		t.Fatalf("cdp %s", b.CDPURL)
	}
	if got := notedPids(udd, "spawns"); !slices.Equal(got, []int{theirs.cmd.Process.Pid}) {
		t.Fatalf("a second Chromium was started on the held profile %v", got)
	}
	state := sharedStateDir(udd)
	if len(globNames(state, "*.lease")) == 0 || getBrowser(key) != b {
		t.Fatal("attached without a lease or registration")
	}
	stopBrowser(context.Background(), key)
	if n := reapOrphanedBrowsers(context.Background(), nil); n != 0 || !theirs.running() {
		t.Fatalf("stopped a browser another environment holds, reaped %d", n)
	}
	if now, _ := os.ReadFile(filepath.Join(udd, devtoolsFile)); len(globNames(state, "*.lease")) != 0 || string(now) != string(portFile) {
		t.Fatal("a lease survived or the endpoint file changed")
	}
}

func TestAnAttachedBrowserFromAnotherEnvironmentIsDroppedOnceItsEndpointIsGone(t *testing.T) {
	env := launcherTestEnv(t)
	script := fakeExe(t, "serving-chromium")
	udd := filepath.Join(t.TempDir(), "pinned-profile")
	theirs := startFake(t, script, udd)
	relock(udd, otherHost, deadPid(t))
	if b, err := launchShared(env, script, udd); err != "" || b == nil {
		t.Fatalf("launch %v %q", b, err)
	}
	killFake(int64(theirs.cmd.Process.Pid))
	<-theirs.done
	if getBrowser("profile:"+udd) != nil {
		t.Fatal("a browser whose endpoint is gone is still registered")
	}
}

func TestAnUnreachableBrowserFromAnotherEnvironmentIsAnErrorNotASecondLaunch(t *testing.T) {
	for _, evidence := range []string{"other-hostname", "foreign-record"} {
		t.Run(evidence, func(t *testing.T) {
			env := launcherTestEnv(t)
			script := fakeExe(t, "serving-chromium")
			udd := filepath.Join(t.TempDir(), "pinned-profile")
			os.Mkdir(udd, 0o755)
			pid := deadPid(t)
			ln, _ := net.Listen("tcp", "127.0.0.1:0")
			portFile := fmt.Sprintf("%d\n/devtools/browser/elsewhere\n", ln.Addr().(*net.TCPAddr).Port)
			ln.Close()
			os.WriteFile(filepath.Join(udd, devtoolsFile), []byte(portFile), 0o644)
			if evidence == "other-hostname" {
				relock(udd, otherHost, pid)
			} else {
				relock(udd, launcherHostname(), pid)
				writeStateJSON(t, filepath.Join(sharedStateDir(udd), fmt.Sprintf("%s-%d-nffffffffffff.json", profileDigest(udd)[:16], pid)),
					map[string]any{"owner_pid": pid + 1, "owner_start_time": 1, "pid": pid, "start_time": 7,
						"user_data_dir": udd, "home_key": "elsewhere", "ephemeral": false, "ns": foreignNS})
			}
			b, err := launchShared(env, script, udd)
			if b != nil || !strings.Contains(err, udd) || !strings.Contains(err, "another environment") {
				t.Fatalf("launch %v %q", b, err)
			}
			if got := notedPids(udd, "spawns"); len(got) != 0 {
				t.Fatalf("a second Chromium was started on a profile another environment holds %v", got)
			}
			target, _ := os.Readlink(filepath.Join(udd, "SingletonLock"))
			if now, _ := os.ReadFile(filepath.Join(udd, devtoolsFile)); string(now) != portFile || !strings.HasSuffix(target, fmt.Sprintf("-%d", pid)) {
				t.Fatal("the other environment's endpoint file or lock was touched")
			}
		})
	}
}

type reaperFixture struct {
	env        *tools.Env
	dir        string
	terminated *[]int64
}

func (f reaperFixture) ephemeral(name string) string {
	return filepath.Join(ephemeralRoot(f.env.Home), name)
}

func reaperRecords(t *testing.T) reaperFixture {
	env := launcherTestEnv(t)
	terminated := &[]int64{}
	pin(t, &launcherTerminate, func(pid, _ *int64, _ *launchProc) { *terminated = append(*terminated, *pid) })
	return reaperFixture{env: env, dir: homeRecords(t, env), terminated: terminated}
}

func reaperRecord(t *testing.T, dir string, pid int, udd string, ownerPid int, startTime any, ephemeral bool) string {
	return writeStateJSON(t, filepath.Join(dir, fmt.Sprintf("rec-%d.json", pid)), map[string]any{"owner_pid": ownerPid,
		"owner_start_time": nil, "pid": pid, "start_time": startTime, "user_data_dir": udd, "home_key": "x",
		"ephemeral": ephemeral, "ns": nsValue()})
}

const (
	deadOwner      = 99999
	liveOtherOwner = 88888
)

func aliveOnly(t *testing.T, pids ...int) {
	pin(t, &launcherPidAlive, func(pid int) bool { return slices.Contains(pids, pid) })
}

func startTimeIs(t *testing.T, v int64, ok bool) {
	pin(t, &launcherStartTime, func(int) (int64, bool) { return v, ok })
}

// Every pid's argv names the dir it is asked about, or none does
func cmdlineNamesEveryDir(t *testing.T, names bool) {
	pin(t, &launcherNamesDir, func(int, string) bool { return names })
}

func TestUnfingerprintableBrowserIsRefused(t *testing.T) {
	f := reaperRecords(t)
	reaperRecord(t, f.dir, 12345, f.ephemeral("p"), deadOwner, nil, true)
	aliveOnly(t, 12345)
	startTimeIs(t, 0, false)
	cmdlineNamesEveryDir(t, true)
	reapOrphanedBrowsers(context.Background(), nil)
	if len(*f.terminated) != 0 {
		t.Fatalf("terminated %v", *f.terminated)
	}
}

func TestCorruptRecordIsCleaned(t *testing.T) {
	f := reaperRecords(t)
	bad, listed := filepath.Join(f.dir, "corrupt.json"), filepath.Join(f.dir, "list.json")
	os.WriteFile(bad, []byte("not-json"), 0o600)
	os.WriteFile(listed, []byte("[1, 2]"), 0o600)
	if n := reapOrphanedBrowsers(context.Background(), nil); n != 0 || lexists(bad) || lexists(listed) || len(*f.terminated) != 0 {
		t.Fatalf("reaped %d", n)
	}
}

func TestAliveOwnerIsNotReapedEvenWhenUntracked(t *testing.T) {
	f := reaperRecords(t)
	rec := reaperRecord(t, f.dir, 12345, f.ephemeral("p"), liveOtherOwner, 777, true)
	aliveOnly(t, 12345, liveOtherOwner)
	startTimeIs(t, 777, true)
	cmdlineNamesEveryDir(t, true)
	if n := reapOrphanedBrowsers(context.Background(), nil); n != 0 || len(*f.terminated) != 0 || !lexists(rec) {
		t.Fatalf("reaped %d", n)
	}
}

func TestReaperIdentityGuard(t *testing.T) {
	udd := "/home/u/.icebath/cache/browser/chromium/abc"
	run := func(argv []string, err error) bool {
		pin(t, &launcherCmdline, func(int) ([]string, error) { return argv, err })
		return cmdlineNamesDir(12345, udd)
	}
	if !run([]string{"chrome", "--remote-debugging-port=0", "--user-data-dir=" + udd}, nil) {
		t.Fatal("a real browser bound via its argv is not reapable")
	}
	if run([]string{"chrome", "--user-data-dir=/home/u/.icebath/cache/browser/chromium/OTHER"}, nil) {
		t.Fatal("a recycled pid bound to another dir is reapable")
	}
	if run([]string{"grep", udd, "/var/log/syslog"}, nil) || run([]string{"grep", "abc", "/var/log/syslog"}, nil) {
		t.Fatal("a bare token names the dir")
	}
	if !run([]string{"chrome", "--user-data-dir=" + udd + "/"}, nil) {
		t.Fatal("a trailing slash spelling does not name the dir")
	}
	if run(nil, os.ErrNotExist) || run(nil, os.ErrPermission) {
		t.Fatal("an unreadable identity did not fail closed")
	}
}

func TestUntrackedOwnBrowserIsReaped(t *testing.T) {
	f := reaperRecords(t)
	rec := reaperRecord(t, f.dir, 12345, f.ephemeral("p"), os.Getpid(), 777, true)
	aliveOnly(t, 12345, os.Getpid())
	startTimeIs(t, 777, true)
	cmdlineNamesEveryDir(t, true)
	if n := reapOrphanedBrowsers(context.Background(), nil); n != 1 || !slices.Equal(*f.terminated, []int64{12345}) || lexists(rec) {
		t.Fatalf("reaped %d terminated %v", n, *f.terminated)
	}
}

func TestTrackedOwnBrowserIsSpared(t *testing.T) {
	f := reaperRecords(t)
	udd := f.ephemeral("p")
	rec := reaperRecord(t, f.dir, 12345, udd, os.Getpid(), 777, true)
	st := int64(777)
	launcherMu.Lock()
	browsers.set("mine", &LaunchedBrowser{Key: "mine", PID: int64p(12345), StartTime: &st, CDPURL: "ws://127.0.0.1:1/devtools/browser/x",
		HTTPURL: "http://127.0.0.1:1", UserDataDir: udd, Owned: true, Ephemeral: true, HomeKey: cfgOf(f.env).homeKey()})
	launcherMu.Unlock()
	t.Cleanup(func() { launcherMu.Lock(); browsers.del("mine"); launcherMu.Unlock() })
	aliveOnly(t, 12345, os.Getpid())
	startTimeIs(t, 777, true)
	cmdlineNamesEveryDir(t, true)
	if n := reapOrphanedBrowsers(context.Background(), nil); n != 0 || len(*f.terminated) != 0 || !lexists(rec) {
		t.Fatalf("reaped %d", n)
	}
}

func TestIdentityGuardStillGatesTheLeakPath(t *testing.T) {
	f := reaperRecords(t)
	reaperRecord(t, f.dir, 12345, f.ephemeral("p"), os.Getpid(), 777, true)
	aliveOnly(t, 12345, os.Getpid())
	startTimeIs(t, 777, true)
	cmdlineNamesEveryDir(t, false)
	if n := reapOrphanedBrowsers(context.Background(), nil); n != 0 || len(*f.terminated) != 0 {
		t.Fatalf("reaped %d", n)
	}
}

func TestThrowawayProfileOfACrashedOwnerIsRemovedEvenWhenItsBrowserDiedToo(t *testing.T) {
	f := reaperRecords(t)
	udd := f.ephemeral("crashed-abc-99999")
	os.MkdirAll(filepath.Join(udd, "Default"), 0o755)
	rec := reaperRecord(t, f.dir, 4242, udd, deadOwner, 777, true)
	aliveOnly(t)
	if n := reapOrphanedBrowsers(context.Background(), nil); n != 0 || len(*f.terminated) != 0 || lexists(udd) || lexists(rec) {
		t.Fatalf("reaped %d", n)
	}
}

func TestBrowserAttachedAfterItsOwnerCrashedIsNeverReapedWhileInUse(t *testing.T) {
	f := reaperRecords(t)
	udd := filepath.Join(t.TempDir(), "persistent-profile")
	aliveOnly(t, 4242)
	startTimeIs(t, 777, true)
	cmdlineNamesEveryDir(t, true)
	pin(t, &runningInstance, func(d string) (int, string, int, bool) {
		if d == udd {
			return 41234, "/devtools/browser/abc", 4242, true
		}
		return 0, "", 0, false
	})
	st := int64(777)
	crashed := &LaunchedBrowser{Key: "profile:" + udd, PID: int64p(4242), StartTime: &st, UserDataDir: udd, Owned: true,
		HomeKey: cfgOf(f.env).homeKey()}
	record := recordPath(crashed)
	writeStateJSON(t, record, map[string]any{"owner_pid": deadOwner, "owner_start_time": nil, "pid": 4242, "start_time": 777,
		"user_data_dir": udd, "home_key": crashed.HomeKey, "ephemeral": false, "ns": nsValue()})
	attached, err := launchChromium(context.Background(), f.env, "profile:"+udd, LaunchOptions{Executable: "/opt/chrome", UserDataDir: udd, SandboxBypass: true})
	if err != "" || attached.Owned {
		t.Fatalf("attach %v %q", attached, err)
	}
	if n := reapOrphanedBrowsers(context.Background(), nil); n != 0 || len(*f.terminated) != 0 {
		t.Fatalf("reaped %d", n)
	}
	if data := loadStateFile(record); !pyEqualInt(objGet(data, "owner_pid"), int64(os.Getpid())) {
		t.Fatalf("record owner %v", objGet(data, "owner_pid"))
	}
}

func TestRecordsDirWritableByOthersIsNotRead(t *testing.T) {
	f := reaperRecords(t)
	rec := reaperRecord(t, f.dir, 12345, f.ephemeral("p"), deadOwner, 777, true)
	aliveOnly(t, 12345)
	startTimeIs(t, 777, true)
	cmdlineNamesEveryDir(t, true)
	os.Chmod(f.dir, 0o777)
	if n := reapOrphanedBrowsers(context.Background(), nil); n != 0 || len(*f.terminated) != 0 || !lexists(rec) {
		t.Fatalf("reaped %d", n)
	}
}

func TestSymlinkedRecordsDirIsNotRead(t *testing.T) {
	f := reaperRecords(t)
	real := filepath.Join(t.TempDir(), "elsewhere")
	os.Rename(f.dir, real)
	os.Symlink(real, f.dir)
	rec := reaperRecord(t, f.dir, 12345, f.ephemeral("p"), deadOwner, 777, true)
	aliveOnly(t, 12345)
	startTimeIs(t, 777, true)
	cmdlineNamesEveryDir(t, true)
	if n := reapOrphanedBrowsers(context.Background(), nil); n != 0 || len(*f.terminated) != 0 || !lexists(rec) {
		t.Fatalf("reaped %d", n)
	}
}

func TestEphemeralRecordOutsideTheEphemeralRootIsNeitherSignalledNorDeleted(t *testing.T) {
	f := reaperRecords(t)
	outside := filepath.Join(t.TempDir(), "not-a-launcher-profile")
	os.MkdirAll(filepath.Join(outside, "Default"), 0o755)
	stand := startProc(t, "sh", "-c", "sleep 60", "--user-data-dir="+outside)
	var st int64
	waitUntil(t, 10*time.Second, "the stand-in's start time", func() bool {
		v, ok := launcherStartTime(stand.cmd.Process.Pid)
		st = v
		return ok
	})
	reaperRecord(t, f.dir, stand.cmd.Process.Pid, outside, deadOwner, st, true)
	if n := reapOrphanedBrowsers(context.Background(), nil); n != 0 || len(*f.terminated) != 0 || !isDir(filepath.Join(outside, "Default")) || !stand.running() {
		t.Fatalf("reaped %d", n)
	}
}

func TestHeadedWindowInLocalMode(t *testing.T) {
	env := launcherTestEnv(t)
	env.Config = config.Tree{"browser": map[string]any{"headed": true}}
	var captured [][]string
	pin(t, &launcherSpawn, func(argv []string, _ map[string]string, _ string) (*launchProc, error) {
		captured = append(captured, argv)
		p := &launchProc{cmd: &exec.Cmd{Process: &os.Process{Pid: 4321}}, done: make(chan struct{}), code: 1}
		close(p.done)
		return p, nil
	})
	startTimeIs(t, 0, false)
	pin(t, &launcherTerminate, func(*int64, *int64, *launchProc) {})
	launchEphemeral(env, "headed", LaunchOptions{Executable: "chromium", UserDataDir: filepath.Join(t.TempDir(), "udd"),
		ProcEnv: map[string]string{"DISPLAY": ":1"}, Timeout: durp(time.Second)})
	if len(captured) != 1 {
		t.Fatalf("spawns %d", len(captured))
	}
	for _, arg := range captured[0] {
		if strings.HasPrefix(arg, "--headless") {
			t.Fatalf("argv %q", captured[0])
		}
	}
}

func TestRunningInstanceAnswersTheLiveChromiumOnTheDirOnly(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"webSocketDebuggerUrl": "ws://127.0.0.1/devtools/browser/source"}`))
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	port := ln.Addr().(*net.TCPAddr).Port
	live := t.TempDir()
	if err := os.WriteFile(filepath.Join(live, "DevToolsActivePort"), []byte(fmt.Sprintf("%d\n/devtools/browser/source\n", port)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(fmt.Sprintf("host-%d", os.Getpid()), filepath.Join(live, "SingletonLock")); err != nil {
		t.Fatal(err)
	}
	if p, path, pid, ok := RunningInstance(live); !ok || p != port || path != "/devtools/browser/source" || pid != os.Getpid() {
		t.Fatalf("live %d %q %d %v", p, path, pid, ok)
	}
	if _, _, _, ok := RunningInstance(t.TempDir()); ok {
		t.Fatal("empty dir answered a running instance")
	}
}
