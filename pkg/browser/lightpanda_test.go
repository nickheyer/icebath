package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/tools"
)

type fakeLightpanda struct {
	mu         sync.Mutex
	pidValue   int
	rc         *int
	terminated bool
	killed     bool
}

func newFakeLightpanda(pid int, exit *int) *fakeLightpanda {
	return &fakeLightpanda{pidValue: pid, rc: exit}
}

func (f *fakeLightpanda) pid() int { return f.pidValue }
func (f *fakeLightpanda) poll() (int, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.rc == nil {
		return 0, false
	}
	return *f.rc, true
}
func (f *fakeLightpanda) terminate() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.terminated, f.rc = true, intp(-15)
	return nil
}
func (f *fakeLightpanda) kill() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.killed, f.rc = true, intp(-9)
	return nil
}
func (f *fakeLightpanda) wait(time.Duration) bool { return true }

func intp(v int) *int { return &v }

// Isolated state dir, no installer candidates, start time 111 and an http cache capable binary
func isolateLightpanda(t *testing.T) (*tools.Env, string) {
	t.Helper()
	env := testEnv(t)
	state := filepath.Join(t.TempDir(), "state")
	os.Mkdir(state, 0o755)
	pin(t, &lightpandaStateDir, func(*tools.Env) (string, error) { return state, nil })
	pin(t, &lightpandaHomeCandidates, func(*tools.Env) []string { return nil })
	pin(t, &lightpandaStartTime, func(int) *int64 { v := int64(111); return &v })
	clearLightpandaProbe()
	pin(t, &lightpandaSupportsHTTPCache, func(string) bool { return true })
	clearLightpandaServers()
	t.Cleanup(clearLightpandaServers)
	return env, state
}

func clearLightpandaServers() {
	lightpandaMu.Lock()
	lightpandaNames, lightpandaByKey = nil, map[string]*LightpandaServer{}
	lightpandaMu.Unlock()
}

func lpExe(t *testing.T, path string) string {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLightpandaFindBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		env, _ := isolateLightpanda(t)
		if got := findLightpandaBinary(env); got != "" {
			t.Fatalf("windows found %q", got)
		}
		return
	}
	t.Run("prefers path", func(t *testing.T) {
		env, _ := isolateLightpanda(t)
		dir := t.TempDir()
		exe := lpExe(t, filepath.Join(dir, "bin", "lightpanda"))
		t.Setenv("PATH", filepath.Join(dir, "bin"))
		pin(t, &lightpandaMergeSanePath, func(p string) string { return p })
		if got := findLightpandaBinary(env); got != exe {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("falls back to home candidates", func(t *testing.T) {
		env, _ := isolateLightpanda(t)
		dir := t.TempDir()
		t.Setenv("PATH", filepath.Join(dir, "empty"))
		pin(t, &lightpandaMergeSanePath, func(p string) string { return p })
		exe := lpExe(t, filepath.Join(dir, ".lightpanda", "lightpanda"))
		pin(t, &lightpandaHomeCandidates, func(*tools.Env) []string { return []string{exe} })
		if got := findLightpandaBinary(env); got != exe {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("none when absent", func(t *testing.T) {
		env, _ := isolateLightpanda(t)
		t.Setenv("PATH", filepath.Join(t.TempDir(), "empty"))
		pin(t, &lightpandaMergeSanePath, func(p string) string { return p })
		if got := findLightpandaBinary(env); got != "" {
			t.Fatalf("got %q", got)
		}
	})
}

func TestLightpandaMergeSanePathKeepsOrderAndAppendsExistingDirs(t *testing.T) {
	got := strings.Split(mergeSanePath("/custom:/usr/bin::/custom2"), ":")
	if !slices.Equal(got[:3], []string{"/custom", "/usr/bin", "/custom2"}) {
		t.Fatalf("got %v", got)
	}
	for _, d := range got[3:] {
		if d == "/usr/bin" || !slices.Contains(lightpandaSanePathDirs, d) {
			t.Fatalf("unexpected appended dir %q in %v", d, got)
		}
		if st, err := os.Stat(d); err != nil || !st.IsDir() {
			t.Fatalf("appended a missing dir %q", d)
		}
	}
}

type lpLaunch struct {
	argv    []string
	procEnv map[string]string
	server  *LightpandaServer
	err     string
}

func launchFakeLightpanda(t *testing.T, env *tools.Env, child *fakeLightpanda, ready bool, stderr string, block bool) lpLaunch {
	t.Helper()
	var out lpLaunch
	pin(t, &lightpandaFindBinary, func(*tools.Env) string { return "/opt/lightpanda" })
	pin(t, &lightpandaFreePort, func() (int, error) { return 43111, nil })
	pin(t, &lightpandaCDPReady, func(string) bool { return ready })
	pin(t, &lightpandaBrowserEnv, func(*tools.Env) map[string]string { return map[string]string{"PATH": "/usr/bin"} })
	pin(t, &lightpandaPopen, func(argv []string, logFile *os.File, procEnv map[string]string) (lightpandaChild, error) {
		out.argv, out.procEnv = argv, procEnv
		if stderr != "" {
			logFile.WriteString(stderr)
		}
		if child == nil {
			return newFakeLightpanda(4242, nil), nil
		}
		return child, nil
	})
	out.server, out.err = launchLightpanda(context.Background(), env, "lp_test", block)
	return out
}

func TestLightpandaLaunchMissingBinaryReturnsInstallHint(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix install hint")
	}
	env, _ := isolateLightpanda(t)
	pin(t, &lightpandaFindBinary, func(*tools.Env) string { return "" })
	server, err := launchLightpanda(context.Background(), env, "lp_test", false)
	if server != nil || !strings.Contains(err, "browser.engine") || !strings.Contains(err, lightpandaInstallURL) {
		t.Fatalf("server %v err %q", server, err)
	}
}

func TestLightpandaLaunchSpawnsServeOnLoopback(t *testing.T) {
	env, state := isolateLightpanda(t)
	got := launchFakeLightpanda(t, env, nil, true, "", false)
	if got.err != "" {
		t.Fatal(got.err)
	}
	want := []string{"/opt/lightpanda", "serve", "--host", "127.0.0.1", "--port", "43111", "--http-cache-dir", filepath.Join(state, "http-cache")}
	if !slices.Equal(got.argv, want) {
		t.Fatalf("argv %v", got.argv)
	}
	if len(got.procEnv) != 1 || got.procEnv["PATH"] != "/usr/bin" {
		t.Fatalf("env %v", got.procEnv)
	}
	if got.server.CDPURL() != "http://127.0.0.1:43111" || *got.server.StartTime != 111 {
		t.Fatalf("server %+v", got.server)
	}
	if getLightpandaServer("lp_test") != got.server {
		t.Fatal("server not tracked")
	}
	var record map[string]any
	data, _ := os.ReadFile(filepath.Join(state, "lp_test.json"))
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	if record["pid"] != 4242.0 || record["port"] != 43111.0 || record["owner_pid"] != float64(os.Getpid()) || record["start_time"] != 111.0 {
		t.Fatalf("record %v", record)
	}
	if !strings.HasPrefix(string(data), `{"pid": 4242, "port": 43111, "owner_pid": `) {
		t.Fatalf("record text %s", data)
	}
}

func TestLightpandaHTTPCacheDirIsSharedAcrossSessions(t *testing.T) {
	env, state := isolateLightpanda(t)
	first := launchFakeLightpanda(t, env, nil, true, "", false)
	clearLightpandaServers()
	second := launchFakeLightpanda(t, env, nil, true, "", false)
	cache := filepath.Join(state, "http-cache")
	for _, l := range []lpLaunch{first, second} {
		if l.argv[slices.Index(l.argv, "--http-cache-dir")+1] != cache {
			t.Fatalf("argv %v", l.argv)
		}
	}
	if st, err := os.Stat(cache); err != nil || !st.IsDir() {
		t.Fatal("cache dir missing")
	}
	if matches, _ := filepath.Glob(filepath.Join(cache, "*.json")); len(matches) != 0 {
		t.Fatalf("cache holds records %v", matches)
	}
}

func TestLightpandaNoHTTPCacheFlagOnOldBinary(t *testing.T) {
	env, _ := isolateLightpanda(t)
	pin(t, &lightpandaSupportsHTTPCache, func(string) bool { return false })
	got := launchFakeLightpanda(t, env, nil, true, "", false)
	if got.err != "" || slices.Contains(got.argv, "--http-cache-dir") || got.argv[len(got.argv)-1] != "43111" {
		t.Fatalf("argv %v err %q", got.argv, got.err)
	}
}

func TestLightpandaHTTPCacheProbeCachesAndDetectsFlag(t *testing.T) {
	isolateLightpanda(t)
	exe := lpExe(t, filepath.Join(t.TempDir(), "lightpanda"))
	runs := 0
	pin(t, &lightpandaHelpRun, func(string) (string, error) {
		runs++
		if runs == 1 {
			return "--http-cache-dir <PATH>", nil
		}
		return "", nil
	})
	if !binarySupportsHTTPCache(exe) || !binarySupportsHTTPCache(exe) || runs != 1 {
		t.Fatalf("probe ran %d times", runs)
	}
	clearLightpandaProbe()
	pin(t, &lightpandaHelpRun, func(string) (string, error) { return "no such flag", nil })
	if binarySupportsHTTPCache(exe) {
		t.Fatal("old binary detected as capable")
	}
	clearLightpandaProbe()
	pin(t, &lightpandaHelpRun, func(string) (string, error) { return "", errors.New("timed out after 3 seconds") })
	if binarySupportsHTTPCache(exe) {
		t.Fatal("hung probe detected as capable")
	}
}

func TestLightpandaHelpRunReadsStdoutAndStderr(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix shell")
	}
	exe := filepath.Join(t.TempDir(), "lightpanda")
	os.WriteFile(exe, []byte("#!/bin/sh\necho usage\necho '  --http-cache-dir <PATH>' >&2\nexit 1\n"), 0o755)
	text, err := runLightpandaHelp(exe)
	if err != nil || !strings.Contains(text, "--http-cache-dir") {
		t.Fatalf("text %q err %v", text, err)
	}
	slow := filepath.Join(t.TempDir(), "lightpanda")
	os.WriteFile(slow, []byte("#!/bin/sh\nexec sleep 30\n"), 0o755)
	start := time.Now()
	if _, err := runLightpandaHelp(slow); err == nil || time.Since(start) > 10*time.Second {
		t.Fatalf("hung help returned err %v after %s", err, time.Since(start))
	}
}

func TestLightpandaBlockPrivateNetworksFlag(t *testing.T) {
	env, _ := isolateLightpanda(t)
	got := launchFakeLightpanda(t, env, nil, true, "", true)
	if got.err != "" || got.argv[len(got.argv)-1] != "--block-private-networks" {
		t.Fatalf("argv %v err %q", got.argv, got.err)
	}
}

func TestLightpandaEarlyExitReportsStderrTail(t *testing.T) {
	env, _ := isolateLightpanda(t)
	got := launchFakeLightpanda(t, env, newFakeLightpanda(4242, intp(1)), false, "info: starting\nFATAL app : unknown argument --bogus\n", false)
	if got.server != nil || !strings.Contains(got.err, "exited with code 1") || !strings.Contains(got.err, "unknown argument --bogus") {
		t.Fatalf("err %q", got.err)
	}
	if getLightpandaServer("lp_test") != nil {
		t.Fatal("dead server tracked")
	}
}

func TestLightpandaReadyTimeoutTerminatesChild(t *testing.T) {
	env, state := isolateLightpanda(t)
	pin(t, &lightpandaReadyTimeout, 50*time.Millisecond)
	pin(t, &lightpandaPollInterval, 10*time.Millisecond)
	child := newFakeLightpanda(4242, nil)
	got := launchFakeLightpanda(t, env, child, false, "", false)
	if got.server != nil || !strings.Contains(got.err, "did not expose") || !child.terminated {
		t.Fatalf("err %q terminated %v", got.err, child.terminated)
	}
	if _, err := os.Stat(filepath.Join(state, "lp_test.json")); err == nil || getLightpandaServer("lp_test") != nil {
		t.Fatal("timed out server recorded")
	}
}

func TestLightpandaSpawnFailureIsReported(t *testing.T) {
	env, _ := isolateLightpanda(t)
	pin(t, &lightpandaFindBinary, func(*tools.Env) string { return "/opt/lightpanda" })
	pin(t, &lightpandaFreePort, func() (int, error) { return 1, nil })
	pin(t, &lightpandaBrowserEnv, func(*tools.Env) map[string]string { return map[string]string{} })
	pin(t, &lightpandaPopen, func([]string, *os.File, map[string]string) (lightpandaChild, error) {
		return nil, errors.New("exec format error")
	})
	server, err := launchLightpanda(context.Background(), env, "lp_test", false)
	if server != nil || !strings.Contains(err, "exec format error") {
		t.Fatalf("err %q", err)
	}
}

func seedLightpanda(t *testing.T, env *tools.Env, name string, alive bool) *LightpandaServer {
	t.Helper()
	var exit *int
	if !alive {
		exit = intp(0)
	}
	start := int64(111)
	server := &LightpandaServer{SessionName: name, Port: 43111, proc: newFakeLightpanda(777, exit), LogPath: name + ".log", StartTime: &start}
	writeLightpandaRecord(context.Background(), env, server)
	lightpandaMu.Lock()
	lightpandaNames = append(lightpandaNames, name)
	lightpandaByKey[name] = server
	lightpandaMu.Unlock()
	return server
}

type treeKills struct {
	mu    sync.Mutex
	calls [][2]any
}

func recordTreeKills(t *testing.T, fail error) *treeKills {
	k := &treeKills{}
	pin(t, &lightpandaTreeKill, func(pid int, start *int64) error {
		k.mu.Lock()
		defer k.mu.Unlock()
		var s any
		if start != nil {
			s = *start
		}
		k.calls = append(k.calls, [2]any{pid, s})
		return fail
	})
	return k
}

func TestLightpandaStopUsesTreeKillWithExpectedStart(t *testing.T) {
	env, state := isolateLightpanda(t)
	server := seedLightpanda(t, env, "lp_test", true)
	kills := recordTreeKills(t, nil)
	stopLightpanda(env, "lp_test")
	if len(kills.calls) != 1 || kills.calls[0] != [2]any{777, int64(111)} {
		t.Fatalf("kills %v", kills.calls)
	}
	if getLightpandaServer("lp_test") != nil || server.proc.(*fakeLightpanda).terminated {
		t.Fatal("server tracked or terminated after a tree kill")
	}
	if _, err := os.Stat(filepath.Join(state, "lp_test.json")); err == nil {
		t.Fatal("record kept")
	}
}

func TestLightpandaStopFallsBackToTerminateWhenTreeKillFails(t *testing.T) {
	env, _ := isolateLightpanda(t)
	server := seedLightpanda(t, env, "lp_test", true)
	recordTreeKills(t, errors.New("psutil missing"))
	stopLightpanda(env, "lp_test")
	if !server.proc.(*fakeLightpanda).terminated {
		t.Fatal("not terminated")
	}
}

func TestLightpandaTreeKillPanicIsTheError(t *testing.T) {
	isolateLightpanda(t)
	pin(t, &lightpandaTreeKill, func(int, *int64) error { panic("psutil missing") })
	err := func() (err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("%v", r)
			}
		}()
		return lightpandaTreeKill(1, nil)
	}()
	if err == nil || err.Error() != "psutil missing" {
		t.Fatalf("err %v", err)
	}
	if err := treeKillLightpanda(1<<30, nil); err != nil {
		t.Fatalf("absent pid tree kill err %v", err)
	}
}

func TestLightpandaStopDeadServerJustDropsRecord(t *testing.T) {
	env, state := isolateLightpanda(t)
	seedLightpanda(t, env, "lp_test", false)
	kills := recordTreeKills(t, nil)
	stopLightpanda(env, "lp_test")
	if len(kills.calls) != 0 {
		t.Fatal("dead server killed")
	}
	if _, err := os.Stat(filepath.Join(state, "lp_test.json")); err == nil {
		t.Fatal("record kept")
	}
}

func TestLightpandaStopUnknownSessionIsNoop(t *testing.T) {
	env, _ := isolateLightpanda(t)
	stopLightpanda(env, "lp_nope")
}

func TestLightpandaStopAll(t *testing.T) {
	env, _ := isolateLightpanda(t)
	seedLightpanda(t, env, "lp_a", true)
	seedLightpanda(t, env, "lp_b", true)
	kills := recordTreeKills(t, nil)
	stopAllLightpanda(env)
	if len(kills.calls) != 2 || getLightpandaServer("lp_a") != nil || getLightpandaServer("lp_b") != nil {
		t.Fatalf("kills %v", kills.calls)
	}
}

func writeLightpandaFixture(t *testing.T, state, name string, pid, owner any) string {
	t.Helper()
	path := filepath.Join(state, name+".json")
	data, _ := json.Marshal(map[string]any{"pid": pid, "port": 43111, "owner_pid": owner, "start_time": 111, "started_at": 0})
	os.WriteFile(path, data, 0o644)
	return path
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestLightpandaReapOrphans(t *testing.T) {
	verified := func(ok bool) func(int, any, any) bool { return func(int, any, any) bool { return ok } }
	t.Run("live other owner is skipped", func(t *testing.T) {
		env, state := isolateLightpanda(t)
		rec := writeLightpandaFixture(t, state, "lp_x", 999, 12345)
		pin(t, &lightpandaPidExists, func(int) bool { return true })
		kills := recordTreeKills(t, nil)
		if n, err := reapOrphanedLightpanda(env); n != 0 || err != nil || len(kills.calls) != 0 || !exists(rec) {
			t.Fatalf("n %d err %v kills %v", n, err, kills.calls)
		}
	})
	t.Run("own tracked session is skipped", func(t *testing.T) {
		env, state := isolateLightpanda(t)
		rec := writeLightpandaFixture(t, state, "lp_x", 999, os.Getpid())
		lightpandaByKey["lp_x"] = &LightpandaServer{SessionName: "lp_x", Port: 43111, proc: newFakeLightpanda(4242, nil)}
		kills := recordTreeKills(t, nil)
		if n, err := reapOrphanedLightpanda(env); n != 0 || err != nil || len(kills.calls) != 0 || !exists(rec) {
			t.Fatalf("n %d err %v kills %v", n, err, kills.calls)
		}
	})
	t.Run("dead owner verified process is killed", func(t *testing.T) {
		env, state := isolateLightpanda(t)
		rec := writeLightpandaFixture(t, state, "lp_x", 999, 12345)
		pin(t, &lightpandaPidExists, func(int) bool { return false })
		pin(t, &lightpandaIsServe, verified(true))
		kills := recordTreeKills(t, nil)
		if n, err := reapOrphanedLightpanda(env); n != 1 || err != nil || exists(rec) {
			t.Fatalf("n %d err %v", n, err)
		}
		if len(kills.calls) != 1 || kills.calls[0] != [2]any{999, int64(111)} {
			t.Fatalf("kills %v", kills.calls)
		}
	})
	t.Run("own untracked session is reaped", func(t *testing.T) {
		env, state := isolateLightpanda(t)
		writeLightpandaFixture(t, state, "lp_x", 999, os.Getpid())
		pin(t, &lightpandaIsServe, verified(true))
		kills := recordTreeKills(t, nil)
		if n, err := reapOrphanedLightpanda(env); n != 1 || err != nil || len(kills.calls) != 1 {
			t.Fatalf("n %d err %v kills %v", n, err, kills.calls)
		}
	})
	t.Run("unverified pid is never signalled", func(t *testing.T) {
		env, state := isolateLightpanda(t)
		rec := writeLightpandaFixture(t, state, "lp_x", 999, 12345)
		pin(t, &lightpandaPidExists, func(int) bool { return false })
		pin(t, &lightpandaIsServe, verified(false))
		kills := recordTreeKills(t, nil)
		if n, err := reapOrphanedLightpanda(env); n != 0 || err != nil || len(kills.calls) != 0 || exists(rec) {
			t.Fatalf("n %d err %v kills %v", n, err, kills.calls)
		}
	})
	t.Run("corrupt record is removed", func(t *testing.T) {
		env, state := isolateLightpanda(t)
		rec := filepath.Join(state, "lp_bad.json")
		os.WriteFile(rec, []byte("{not json"), 0o644)
		if n, err := reapOrphanedLightpanda(env); n != 0 || err != nil || exists(rec) {
			t.Fatalf("n %d err %v", n, err)
		}
	})
	t.Run("unparseable owner pid raises out of the sweep", func(t *testing.T) {
		env, state := isolateLightpanda(t)
		writeLightpandaFixture(t, state, "lp_x", 999, "not a pid")
		if _, err := reapOrphanedLightpanda(env); err == nil {
			t.Fatal("int of a non numeric owner pid did not raise")
		}
	})
}

// A parseable non object state file is swept like an unreadable one under the real home state dir
func TestLightpandaOrphanReaperSurvivesNonDictRecord(t *testing.T) {
	env := testEnv(t)
	clearLightpandaServers()
	state, err := lightpandaStateDir(env)
	if err != nil || state != filepath.Join(env.Home, "cache", "browser-use", "lightpanda") {
		t.Fatalf("state %q err %v", state, err)
	}
	bad := filepath.Join(state, "broken.json")
	os.WriteFile(bad, []byte(`"not a record"`), 0o644)
	dead := filepath.Join(state, "dead.json")
	os.WriteFile(dead, []byte(fmt.Sprintf(`{"owner_pid": %d}`, 1<<22+7)), 0o644)
	if n, err := reapOrphanedLightpanda(env); n != 0 || err != nil || exists(bad) || exists(dead) {
		t.Fatalf("n %d err %v", n, err)
	}
}

// A sweep running while the owner rewrites its record never sees a torn file
func TestLightpandaOrphanSweepMidRecordWriteKeepsTheLiveRecord(t *testing.T) {
	env := testEnv(t)
	clearLightpandaServers()
	t.Cleanup(clearLightpandaServers)
	server := &LightpandaServer{SessionName: "live", Port: 9222, proc: newFakeLightpanda(os.Getpid(), nil), LogPath: "x.log"}
	lightpandaMu.Lock()
	lightpandaNames, lightpandaByKey["live"] = []string{"live"}, server
	lightpandaMu.Unlock()
	var stop atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		for !stop.Load() {
			reapOrphanedLightpanda(env)
		}
	}()
	for range 200 {
		writeLightpandaRecord(context.Background(), env, server)
	}
	stop.Store(true)
	<-done
	state, _ := lightpandaStateDir(env)
	data, err := os.ReadFile(filepath.Join(state, "live.json"))
	var record map[string]any
	if err != nil || json.Unmarshal(data, &record) != nil || record["port"] != 9222.0 {
		t.Fatalf("record %s err %v", data, err)
	}
}

type fakeProcFacts struct {
	name    string
	cmdline []string
	start   any
}

func pinProcFacts(t *testing.T, f fakeProcFacts) {
	pin(t, &lightpandaProcName, func(int) (string, error) { return f.name, nil })
	pin(t, &lightpandaProcCmdline, func(int) ([]string, error) { return f.cmdline, nil })
	pin(t, &lightpandaProcStartTime, func(int) any { return f.start })
}

func TestLightpandaProcessIdentity(t *testing.T) {
	serve := []string{"/opt/lightpanda", "serve", "--host", "127.0.0.1", "--port", "43111"}
	pinProcFacts(t, fakeProcFacts{"lightpanda", serve, int64(111)})
	if !isLightpandaProcess(999, int64(43111), int64(111)) {
		t.Fatal("matching serve rejected")
	}
	pinProcFacts(t, fakeProcFacts{"lightpanda", serve, int64(222)})
	if isLightpandaProcess(999, int64(43111), int64(111)) {
		t.Fatal("recycled start time accepted")
	}
	pinProcFacts(t, fakeProcFacts{"chrome", []string{"chrome", "--remote-debugging-port=43111"}, nil})
	if isLightpandaProcess(999, int64(43111), nil) {
		t.Fatal("other process on a recycled pid accepted")
	}
	pinProcFacts(t, fakeProcFacts{"lightpanda", []string{"lightpanda", "serve", "--port", "1"}, nil})
	if isLightpandaProcess(999, int64(43111), nil) {
		t.Fatal("lightpanda on another port accepted")
	}
	pin(t, &lightpandaProcName, func(int) (string, error) { return "", errors.New("no such process") })
	if isLightpandaProcess(999, int64(43111), nil) {
		t.Fatal("vanished process accepted")
	}
}

func TestLightpandaRecordStartTime(t *testing.T) {
	for _, c := range []struct {
		in   any
		want string
	}{{nil, "nil"}, {int64(111), "111"}, {111.0, "111"}, {false, "0"}, {"", "-1"}, {111.5, "-1"}} {
		got := "nil"
		if p := recordStartTime(c.in); p != nil {
			got = strconv.FormatInt(*p, 10)
		}
		if got != c.want {
			t.Fatalf("%#v -> %s want %s", c.in, got, c.want)
		}
	}
}

func lightpandaStatusEnv(t *testing.T, browser map[string]any) *tools.Env {
	t.Helper()
	env := testEnv(t)
	t.Setenv("BROWSER_CDP_URL", "")
	t.Setenv("BROWSER_USE_API_KEY", "")
	section := map[string]any{"engine": "lightpanda"}
	for k, v := range browser {
		section[k] = v
	}
	env.Config = config.Tree{"browser": section}
	pin(t, &lightpandaCloudProvider, func(*tools.Env) (BrowserProvider, error) { return nil, nil })
	return env
}

func TestLightpandaEngineStatus(t *testing.T) {
	t.Run("not lightpanda", func(t *testing.T) {
		env := lightpandaStatusEnv(t, map[string]any{"engine": "auto"})
		if used, reason := lightpandaEngineStatus(env); used || reason != "" {
			t.Fatal(used, reason)
		}
	})
	t.Run("used when nothing shadows it", func(t *testing.T) {
		env := lightpandaStatusEnv(t, nil)
		if used, reason := lightpandaEngineStatus(env); !used || !strings.Contains(reason, "lightpanda serve") {
			t.Fatal(used, reason)
		}
	})
	t.Run("shadowed by cdp override", func(t *testing.T) {
		env := lightpandaStatusEnv(t, map[string]any{"cdp_url": "ws://x"})
		if used, reason := lightpandaEngineStatus(env); used || !strings.Contains(reason, "CDP override") {
			t.Fatal(used, reason)
		}
	})
	t.Run("shadowed by cloud provider", func(t *testing.T) {
		env := lightpandaStatusEnv(t, nil)
		pin(t, &lightpandaCloudProvider, func(*tools.Env) (BrowserProvider, error) { return newFakeCloud("Browserbase", true), nil })
		if used, reason := lightpandaEngineStatus(env); used || !strings.Contains(reason, "Browserbase") {
			t.Fatal(used, reason)
		}
	})
	t.Run("unknown explicit provider reads as none", func(t *testing.T) {
		env := lightpandaStatusEnv(t, nil)
		pin(t, &lightpandaCloudProvider, func(*tools.Env) (BrowserProvider, error) { return nil, errors.New("no registered browser plugin") })
		if used, _ := lightpandaEngineStatus(env); !used {
			t.Fatal("unknown provider shadowed lightpanda")
		}
	})
	t.Run("shadowed by legacy browser use cloud", func(t *testing.T) {
		env := lightpandaStatusEnv(t, nil)
		t.Setenv("BROWSER_USE_API_KEY", "bu-key")
		if used, reason := lightpandaEngineStatus(env); used || !strings.Contains(reason, "Browser Use cloud") {
			t.Fatal(used, reason)
		}
	})
	t.Run("shadowed by real profile", func(t *testing.T) {
		env := lightpandaStatusEnv(t, map[string]any{"use_real_profile": true})
		if used, reason := lightpandaEngineStatus(env); used || !strings.Contains(reason, "use_real_profile") {
			t.Fatal(used, reason)
		}
	})
	t.Run("real profile wins over cloud provider", func(t *testing.T) {
		env := lightpandaStatusEnv(t, map[string]any{"use_real_profile": true})
		pin(t, &lightpandaCloudProvider, func(*tools.Env) (BrowserProvider, error) { return newFakeCloud("Browserbase", true), nil })
		if used, reason := lightpandaEngineStatus(env); used || !strings.Contains(reason, "use_real_profile") {
			t.Fatal(used, reason)
		}
	})
}

func TestLegacyBrowserUseCloudConfig(t *testing.T) {
	for _, c := range []struct {
		browser map[string]any
		key     string
		want    bool
	}{
		{map[string]any{}, "k", true},
		{map[string]any{"cloud_provider": "browser-use"}, "k", true},
		{map[string]any{"cloud_provider": "browserbase"}, "k", false},
		{map[string]any{"cloud_provider": "nous"}, "k", false},
		{map[string]any{"use_gateway": true}, "k", false},
		{map[string]any{}, "", false},
	} {
		env := testEnv(t)
		env.Config = config.Tree{"browser": c.browser}
		t.Setenv("BROWSER_USE_API_KEY", c.key)
		if got := isLegacyBrowserUseCloudConfig(env); got != c.want {
			t.Fatalf("%v key %q got %v", c.browser, c.key, got)
		}
	}
}
