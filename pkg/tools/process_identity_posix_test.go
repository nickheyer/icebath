//go:build !windows

package tools

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// A child that sleeps long enough that no test passes because it exited on its own
func startVictim(t *testing.T, script string) *exec.Cmd {
	t.Helper()
	if script == "" {
		script = "exec sleep 300"
	}
	cmd := exec.Command("sh", "-c", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		syscall.Kill(cmd.Process.Pid, syscall.SIGKILL)
		cmd.Wait()
	})
	return cmd
}

func pidAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func procWaitUntil(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Waits for a direct child to exit, false at the timeout
func waitExited(cmd *exec.Cmd, timeout time.Duration) bool {
	done := make(chan struct{})
	go func() { cmd.Wait(); close(done) }()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

func scriptFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "process_supervisor.sh")
	if err := os.WriteFile(path, []byte(supervisorScript), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// Feeds stream to the real script with kill recorded instead of sent, returns the groups it TERMed
func terminatedGroups(t *testing.T, stream string, parentPgid int) map[int]bool {
	t.Helper()
	recorder := `kill() { case $1 in -0) command kill "$@" ;;` +
		` -TERM) echo "${2#-}" >> "$ICEBATH_TEST_LOG"; return 1 ;; *) return 1 ;; esac; };` +
		` . "$ICEBATH_TEST_SCRIPT"`
	log := filepath.Join(t.TempDir(), "supervisor.log")
	cmd := exec.Command("/bin/sh", "-c", recorder, "supervisor", strconv.Itoa(parentPgid))
	cmd.Stdin = strings.NewReader(stream)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Env = append(os.Environ(), "ICEBATH_TEST_LOG="+log, "ICEBATH_TEST_SCRIPT="+scriptFile(t))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("supervisor run failed: %v %s", err, out)
	}
	groups := map[int]bool{}
	data, err := os.ReadFile(log)
	if err != nil {
		return groups
	}
	for _, f := range strings.Fields(string(data)) {
		n, _ := strconv.Atoi(f)
		groups[n] = true
	}
	return groups
}

func sameGroups(got map[int]bool, want ...int) bool {
	if len(got) != len(want) {
		return false
	}
	for _, w := range want {
		if !got[w] {
			return false
		}
	}
	return true
}

func TestRegistrationsSurviveToEOFAndUnregistrationsAreDropped(t *testing.T) {
	if got := terminatedGroups(t, "register 111\nregister 222\nunregister 111\n", 777); !sameGroups(got, 222) {
		t.Fatalf("groups %v", got)
	}
}

func TestGarbageLinesDoNotCostTheOtherRegistrations(t *testing.T) {
	stream := "register 111\n\nregister\nregister notanumber\nregister 222 333\nexplode 444\nregister 99999999999999\nregister 555\n"
	if got := terminatedGroups(t, stream, 777); !sameGroups(got, 111, 555) {
		t.Fatalf("groups %v", got)
	}
}

func TestAnUnterminatedLineAtEOFIsNeverActedOn(t *testing.T) {
	if got := terminatedGroups(t, "register 111\nregister 9", 777); !sameGroups(got, 111) {
		t.Fatalf("groups %v", got)
	}
}

func TestUnsafeTargetsAreRejectedAtRegistrationTime(t *testing.T) {
	if got := terminatedGroups(t, "register 0\nregister 1\nregister 777\nregister 999\n", 777); !sameGroups(got, 999) {
		t.Fatalf("groups %v", got)
	}
}

func TestRefusesToRunInsideTheParentsOwnProcessGroup(t *testing.T) {
	cmd := exec.Command(posixSh, "-c", supervisorScript, supervisorArgv0, strconv.Itoa(syscall.Getpgrp()))
	var stderr strings.Builder
	cmd.Stderr = &stderr
	err := cmd.Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 2 || !strings.Contains(stderr.String(), "process-group leader") {
		t.Fatalf("err=%v stderr=%s", err, stderr.String())
	}
}

func TestTheSupervisorRefusesAMalformedParentPgid(t *testing.T) {
	for _, parent := range []string{"", "12x"} {
		cmd := exec.Command(posixSh, "-c", supervisorScript, supervisorArgv0, parent)
		cmd.Stdin = strings.NewReader("")
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		var stderr strings.Builder
		cmd.Stderr = &stderr
		err := cmd.Run()
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 2 || !strings.Contains(stderr.String(), "bad parent pgid") {
			t.Fatalf("parent %q: err=%v stderr=%s", parent, err, stderr.String())
		}
	}
}

// A supervisor shell started by a test with its own control pipe
type testSupervisor struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
}

func startTestSupervisor(t *testing.T, env []string) *testSupervisor {
	t.Helper()
	cmd := exec.Command(posixSh, "-c", supervisorScript, supervisorArgv0, strconv.Itoa(syscall.Getpgrp()))
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if env != nil {
		cmd.Env = env
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		syscall.Kill(cmd.Process.Pid, syscall.SIGKILL)
		cmd.Wait()
	})
	return &testSupervisor{cmd: cmd, stdin: stdin}
}

func TestReapsARegisteredGroupWhenTheControlPipeReachesEOF(t *testing.T) {
	victim := startVictim(t, "")
	sup := startTestSupervisor(t, nil)
	pgid, _ := syscall.Getpgid(victim.Process.Pid)
	fmt.Fprintf(sup.stdin, "register %d\n", pgid)
	time.Sleep(100 * time.Millisecond)
	if !pidAlive(victim.Process.Pid) {
		t.Fatal("victim should outlive registration")
	}
	sup.stdin.Close()
	if !waitExited(victim, 15*time.Second) {
		t.Fatal("registered group survived parent death")
	}
}

func armedVictim(t *testing.T) (*exec.Cmd, int) {
	t.Helper()
	cmd := exec.Command("sh", "-c", "trap '' TERM; echo armed; exec sleep 300")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		cmd.Wait()
	})
	line, _ := bufio.NewReader(out).ReadString('\n')
	if strings.TrimSpace(line) != "armed" {
		t.Fatalf("victim never armed: %q", line)
	}
	return cmd, cmd.Process.Pid
}

func TestEscalatesToSigkillForAGroupThatIgnoresSigterm(t *testing.T) {
	victim, pid := armedVictim(t)
	sup := startTestSupervisor(t, nil)
	fmt.Fprintf(sup.stdin, "register %d\n", pid)
	sup.stdin.Close()
	if !waitExited(victim, 15*time.Second) {
		t.Fatal("a SIGTERM-ignoring group survived the reap")
	}
	if sig := signalOf(victim.ProcessState); sig != int(syscall.SIGKILL) {
		t.Fatalf("victim ended by signal %d", sig)
	}
}

func TestTheTermGraceHoldsEvenWithABrokenPath(t *testing.T) {
	victim, pid := armedVictim(t)
	sup := startTestSupervisor(t, []string{"PATH=/nonexistent"})
	fmt.Fprintf(sup.stdin, "register %d\n", pid)
	sup.stdin.Close()
	closed := time.Now()
	if !waitExited(victim, 15*time.Second) {
		t.Fatal("a SIGTERM-ignoring group survived the reap")
	}
	if time.Since(closed) < 1500*time.Millisecond {
		t.Fatal("the KILL skipped the TERM grace")
	}
}

func TestLeavesAnUnregisteredGroupAloneAtEOF(t *testing.T) {
	survivor := startVictim(t, "")
	sup := startTestSupervisor(t, nil)
	pgid, _ := syscall.Getpgid(survivor.Process.Pid)
	fmt.Fprintf(sup.stdin, "register %d\nunregister %d\n", pgid, pgid)
	sup.stdin.Close()
	if !waitExited(sup.cmd, 15*time.Second) {
		t.Fatal("supervisor did not exit at EOF")
	}
	if !pidAlive(survivor.Process.Pid) {
		t.Fatal("a cleanly unregistered child was killed")
	}
}

// Re executes the test binary as a helper when the marker env names one
func helperCommand(t *testing.T, helper string, env ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^"+helper+"$")
	cmd.Env = append(os.Environ(), append([]string{"ICEBATH_TEST_HELPER=" + helper}, env...)...)
	return cmd
}

// Helper process standing in for icebath, registers a child then blocks holding the only write end
func TestFakeParentHelper(t *testing.T) {
	if os.Getenv("ICEBATH_TEST_HELPER") != "TestFakeParentHelper" {
		t.Skip("helper process entry point")
	}
	victim := exec.Command("sh", "-c", "exec sleep 300")
	victim.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := victim.Start(); err != nil {
		os.Exit(3)
	}
	h, err := startSupervisorProcess()
	if err != nil {
		os.Exit(4)
	}
	pgid, _ := syscall.Getpgid(victim.Process.Pid)
	h.write(fmt.Sprintf("register %d\n", pgid))
	fmt.Printf("%d %d\n", victim.Process.Pid, h.pid())
	os.Stdout.Sync()
	time.Sleep(300 * time.Second)
}

func TestReapsTheChildWhenTheRegisteringParentIsSigkilled(t *testing.T) {
	parent := helperCommand(t, "TestFakeParentHelper")
	out, err := parent.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := parent.Start(); err != nil {
		t.Fatal(err)
	}
	line, _ := bufio.NewReader(out).ReadString('\n')
	fields := strings.Fields(line)
	if len(fields) != 2 {
		parent.Process.Kill()
		parent.Wait()
		t.Fatalf("helper output %q", line)
	}
	victimPid, _ := strconv.Atoi(fields[0])
	supPid, _ := strconv.Atoi(fields[1])
	t.Cleanup(func() {
		syscall.Kill(victimPid, syscall.SIGKILL)
		syscall.Kill(supPid, syscall.SIGKILL)
		parent.Process.Kill()
		parent.Wait()
	})
	if !pidAlive(victimPid) {
		t.Fatal("victim not alive after registration")
	}
	parent.Process.Kill()
	parent.Wait()
	procWaitUntil(t, 15*time.Second, "the owned child to die with its killed parent", func() bool { return !pidAlive(victimPid) })
}

func TestReapsAGrandchildLeftInTheRegisteredGroup(t *testing.T) {
	server := exec.Command("sh", "-c", "sleep 300 & echo $!")
	server.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	out, err := server.Output()
	if err != nil {
		t.Fatal(err)
	}
	grandchild, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	t.Cleanup(func() { syscall.Kill(grandchild, syscall.SIGKILL) })
	if !pidAlive(grandchild) {
		t.Fatal("grandchild should outlive its parent")
	}
	sup := startTestSupervisor(t, nil)
	fmt.Fprintf(sup.stdin, "register %d\n", server.Process.Pid)
	sup.stdin.Close()
	procWaitUntil(t, 15*time.Second, "the orphaned grandchild to be reaped", func() bool { return !pidAlive(grandchild) })
}

// Stand in for the supervisor process recording the control stream
type fakeSupervisor struct {
	mu      sync.Mutex
	sent    strings.Builder
	exited  bool
	closed  bool
	waited  bool
	broken  bool
	writeFn func(string) error
}

func (f *fakeSupervisor) write(payload string) error {
	if f.writeFn != nil {
		return f.writeFn(payload)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.broken {
		return syscall.EPIPE
	}
	f.sent.WriteString(payload)
	return nil
}
func (f *fakeSupervisor) closeStdin() error {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
	return nil
}
func (f *fakeSupervisor) poll() (bool, int) {
	if f.exited {
		return true, 1
	}
	return false, 0
}
func (f *fakeSupervisor) wait() int { f.waited = true; return 0 }
func (f *fakeSupervisor) pid() int  { return 4242 }
func (f *fakeSupervisor) lines() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Split(strings.TrimSuffix(f.sent.String(), "\n"), "\n")
}
func (f *fakeSupervisor) isClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

func hasLine(lines []string, want string) bool {
	for _, l := range lines {
		if l == want {
			return true
		}
	}
	return false
}

// Resets the supervisor client state and the idle grace for a protocol test
func resetSupervisorState(t *testing.T) {
	t.Helper()
	supervisorMu.Lock()
	cancelIdleReleaseLocked()
	ownerSupervisor = nil
	supervisedPgids = map[int]bool{}
	supervisorMu.Unlock()
	prevIdle, prevSpawn, prevProbe, prevMin := supervisorIdleExit, spawnSupervisorFn, processGroupGoneFn, supervisorRestartMinUptime
	supervisorIdleExit = 0
	t.Cleanup(func() {
		supervisorMu.Lock()
		cancelIdleReleaseLocked()
		if ownerSupervisor != nil {
			ownerSupervisor.closeStdin()
		}
		ownerSupervisor = nil
		supervisedPgids = map[int]bool{}
		supervisorMu.Unlock()
		supervisorIdleExit, spawnSupervisorFn, processGroupGoneFn, supervisorRestartMinUptime = prevIdle, prevSpawn, prevProbe, prevMin
	})
}

func allGroupsAlive() { processGroupGoneFn = func(int) bool { return false } }

func supervisedSet() []int {
	supervisorMu.Lock()
	defer supervisorMu.Unlock()
	out := []int{}
	for p := range supervisedPgids {
		out = append(out, p)
	}
	sort.Ints(out)
	return out
}

func currentSupervisor() supervisorHandle {
	supervisorMu.Lock()
	defer supervisorMu.Unlock()
	return ownerSupervisor
}

func TestSpawnOwnedStartsAGroupLeaderRegistersItAndRecordsTheLedger(t *testing.T) {
	resetSupervisorState(t)
	fake := &fakeSupervisor{}
	spawnSupervisorFn = func() supervisorHandle { return fake }
	ledger := [][2]any{}
	prevRegister := registerChildFn
	registerChildFn = func(pid int, purpose, root string) bool { ledger = append(ledger, [2]any{pid, purpose}); return true }
	t.Cleanup(func() { registerChildFn = prevRegister })
	cmd := exec.Command("sh", "-c", "exec sleep 300")
	if err := SpawnOwned(cmd, "unit-test"); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() { syscall.Kill(pid, syscall.SIGKILL); cmd.Wait() })
	if pgid, _ := syscall.Getpgid(pid); pgid != pid || pgid == syscall.Getpgrp() {
		t.Fatalf("owned child does not lead its own group (pgid %d)", pgid)
	}
	if !hasLine(fake.lines(), fmt.Sprintf("register %d", pid)) {
		t.Fatalf("group never reached the supervisor: %v", fake.lines())
	}
	if len(ledger) != 1 || ledger[0][0] != pid || ledger[0][1] != "unit-test" {
		t.Fatalf("ledger %v", ledger)
	}
	releaseProcessGroups([]int{pid})
	if set := supervisedSet(); len(set) != 0 {
		t.Fatalf("still supervised %v", set)
	}
}

func TestRegisterStartsTheSupervisorOnceAndReusesIt(t *testing.T) {
	resetSupervisorState(t)
	allGroupsAlive()
	spawned := []*fakeSupervisor{}
	spawnSupervisorFn = func() supervisorHandle { f := &fakeSupervisor{}; spawned = append(spawned, f); return f }
	ownProcessGroups([]int{111})
	ownProcessGroups([]int{222})
	if len(spawned) != 1 {
		t.Fatalf("each register spawned its own supervisor (%d)", len(spawned))
	}
	if lines := spawned[0].lines(); strings.Join(lines, "|") != "register 111|register 222" {
		t.Fatalf("lines %v", lines)
	}
}

func TestSupervisorIsReleasedOnceNothingIsLeftToReap(t *testing.T) {
	resetSupervisorState(t)
	allGroupsAlive()
	spawned := []*fakeSupervisor{}
	spawnSupervisorFn = func() supervisorHandle { f := &fakeSupervisor{}; spawned = append(spawned, f); return f }
	ownProcessGroups([]int{111, 222})
	releaseProcessGroups([]int{111})
	if spawned[0].isClosed() {
		t.Fatal("released the supervisor while a group was still registered")
	}
	releaseProcessGroups([]int{222})
	if !spawned[0].isClosed() {
		t.Fatal("supervisor kept resident with nothing left to reap")
	}
	if spawned[0].waited {
		t.Fatal("the release blocked on the supervisor's exit")
	}
	lines := spawned[0].lines()
	if lines[len(lines)-1] != "unregister 222" {
		t.Fatalf("release happened before the last unregister: %v", lines)
	}
	if currentSupervisor() != nil {
		t.Fatal("supervisor still current after the release")
	}
	ownProcessGroups([]int{333})
	if len(spawned) != 2 || strings.Join(spawned[1].lines(), "|") != "register 333" {
		t.Fatalf("spawned %d, lines %v", len(spawned), spawned[len(spawned)-1].lines())
	}
}

func TestSupervisorSurvivesTheRealEOFRelease(t *testing.T) {
	resetSupervisorState(t)
	child := startVictim(t, "")
	pgid, _ := syscall.Getpgid(child.Process.Pid)
	ownProcessGroups([]int{pgid})
	h := currentSupervisor()
	if h == nil {
		t.Fatal("no supervisor started")
	}
	if exited, _ := h.poll(); exited {
		t.Fatal("supervisor exited at once")
	}
	releaseProcessGroups([]int{pgid})
	if currentSupervisor() != nil {
		t.Fatal("supervisor not released")
	}
	if code := h.wait(); code != 0 {
		t.Fatalf("supervisor exit %d", code)
	}
	if !pidAlive(child.Process.Pid) {
		t.Fatal("release reaped a group that had been unregistered")
	}
}

func TestUnregisterAloneDoesNotStartASupervisor(t *testing.T) {
	resetSupervisorState(t)
	spawned := 0
	spawnSupervisorFn = func() supervisorHandle { spawned++; return &fakeSupervisor{} }
	releaseProcessGroups([]int{111})
	if spawned != 0 {
		t.Fatal("unregister spawned a supervisor")
	}
}

func TestADeadSupervisorIsReplacedAndLiveCoverageReplayed(t *testing.T) {
	resetSupervisorState(t)
	allGroupsAlive()
	dead := &fakeSupervisor{exited: true}
	replacement := &fakeSupervisor{}
	queue := []supervisorHandle{dead, replacement}
	spawnSupervisorFn = func() supervisorHandle { h := queue[0]; queue = queue[1:]; return h }
	ownProcessGroups([]int{111})
	ownProcessGroups([]int{222})
	lines := replacement.lines()
	if !hasLine(lines, "register 111") || !hasLine(lines, "register 222") || len(lines) != 2 {
		t.Fatalf("replacement lines %v", lines)
	}
}

func TestReplayDoesNotResurrectAnUnregisteredGroup(t *testing.T) {
	resetSupervisorState(t)
	allGroupsAlive()
	dead := &fakeSupervisor{exited: true}
	replacement := &fakeSupervisor{}
	queue := []supervisorHandle{dead, replacement}
	spawnSupervisorFn = func() supervisorHandle { h := queue[0]; queue = queue[1:]; return h }
	ownProcessGroups([]int{111})
	ownProcessGroups([]int{222})
	releaseProcessGroups([]int{111})
	if set := supervisedSet(); len(set) != 1 || set[0] != 222 {
		t.Fatalf("set %v", set)
	}
	lines := replacement.lines()
	reg, unreg := -1, -1
	for i, l := range lines {
		if l == "register 111" && reg < 0 {
			reg = i
		}
		if l == "unregister 111" {
			unreg = i
		}
	}
	if reg < 0 || unreg < reg {
		t.Fatalf("lines %v", lines)
	}
	for _, l := range lines[unreg:] {
		if l == "register 111" {
			t.Fatalf("111 resurrected after unregister: %v", lines)
		}
	}
	before := len(lines)
	ownProcessGroups([]int{333})
	for _, l := range replacement.lines()[before:] {
		if l == "register 111" {
			t.Fatal("a later replay resurrected 111")
		}
	}
}

func TestABrokenPipeNeverPropagatesIntoALiveChild(t *testing.T) {
	resetSupervisorState(t)
	allGroupsAlive()
	spawnSupervisorFn = func() supervisorHandle { return &fakeSupervisor{broken: true} }
	ownProcessGroups([]int{111})
	if currentSupervisor() != nil {
		t.Fatal("a supervisor with a dead pipe was kept")
	}
}

func TestUnregisterAfterABrokenPipeRebuildsCoverageForSurvivors(t *testing.T) {
	resetSupervisorState(t)
	allGroupsAlive()
	spawned := []*fakeSupervisor{}
	spawnSupervisorFn = func() supervisorHandle { f := &fakeSupervisor{}; spawned = append(spawned, f); return f }
	ownProcessGroups([]int{111, 222})
	spawned[0].mu.Lock()
	spawned[0].broken = true
	spawned[0].mu.Unlock()
	ownProcessGroups([]int{333})
	if currentSupervisor() != nil {
		t.Fatal("dead supervisor kept after the failed write")
	}
	if set := supervisedSet(); fmt.Sprint(set) != "[111 222 333]" {
		t.Fatalf("set %v", set)
	}
	releaseProcessGroups([]int{222})
	if len(spawned) != 2 {
		t.Fatalf("unregister after a lost supervisor did not respawn one (%d)", len(spawned))
	}
	lines := spawned[1].lines()
	sort.Strings(lines)
	if strings.Join(lines, "|") != "register 111|register 333" {
		t.Fatalf("replacement lines %v", lines)
	}
	if currentSupervisor() != supervisorHandle(spawned[1]) {
		t.Fatal("replacement is not the current supervisor")
	}
}

func TestASupervisorThatCannotStartIsNotFatal(t *testing.T) {
	resetSupervisorState(t)
	allGroupsAlive()
	spawnSupervisorFn = func() supervisorHandle { return nil }
	ownProcessGroups([]int{111})
	if currentSupervisor() != nil {
		t.Fatal("a nil supervisor was recorded")
	}
}

func TestAGroupWithNothingLeftAliveIsForgottenAndUnregistered(t *testing.T) {
	resetSupervisorState(t)
	fake := &fakeSupervisor{}
	spawnSupervisorFn = func() supervisorHandle { return fake }
	doomed := startVictim(t, "")
	doomedPgid, _ := syscall.Getpgid(doomed.Process.Pid)
	survivor := startVictim(t, "")
	survivorPgid, _ := syscall.Getpgid(survivor.Process.Pid)
	ownProcessGroups([]int{doomedPgid, survivorPgid})
	if set := supervisedSet(); len(set) != 2 {
		t.Fatalf("set %v", set)
	}
	syscall.Kill(doomed.Process.Pid, syscall.SIGKILL)
	doomed.Wait()
	ownProcessGroups([]int{survivorPgid})
	set := supervisedSet()
	if len(set) != 1 || set[0] != survivorPgid {
		t.Fatalf("set %v after the doomed group died", set)
	}
	if !hasLine(fake.lines(), fmt.Sprintf("unregister %d", doomedPgid)) {
		t.Fatalf("the supervisor was never told to forget the dead group: %v", fake.lines())
	}
}

func TestPruningKeepsGroupsItCannotProveAreGone(t *testing.T) {
	resetSupervisorState(t)
	supervisorMu.Lock()
	supervisedPgids = map[int]bool{111: true, 222: true}
	supervisorMu.Unlock()
	processGroupGoneFn = func(pgid int) bool { return pgid != 111 }
	supervisorMu.Lock()
	stale := pruneDeadSupervisedPgids()
	supervisorMu.Unlock()
	if len(stale) != 1 || !stale[222] {
		t.Fatalf("stale %v", stale)
	}
	if set := supervisedSet(); len(set) != 1 || set[0] != 111 {
		t.Fatalf("set %v", set)
	}
}

func TestNoPgidsIsANoOp(t *testing.T) {
	resetSupervisorState(t)
	spawned := 0
	spawnSupervisorFn = func() supervisorHandle { spawned++; return &fakeSupervisor{} }
	ownProcessGroups(nil)
	if spawned != 0 {
		t.Fatal("an empty registration spawned a supervisor")
	}
}

func TestAReleasedSupervisorLeavesNoZombieChild(t *testing.T) {
	resetSupervisorState(t)
	child := startVictim(t, "")
	ownProcessGroups([]int{child.Process.Pid})
	sup := currentSupervisor()
	releaseProcessGroups([]int{child.Process.Pid})
	procWaitUntil(t, 10*time.Second, "the released supervisor to be reaped", func() bool { return !pidAlive(sup.pid()) })
}

func TestASupervisorKilledOnItsOwnIsRestartedWithEveryGroupReplayed(t *testing.T) {
	resetSupervisorState(t)
	child := startVictim(t, "")
	ownProcessGroups([]int{child.Process.Pid})
	first := currentSupervisor()
	syscall.Kill(first.pid(), syscall.SIGKILL)
	procWaitUntil(t, 10*time.Second, "a replacement supervisor", func() bool { h := currentSupervisor(); return h != nil && h != first })
	second := currentSupervisor()
	procWaitUntil(t, 10*time.Second, "the killed supervisor reaped", func() bool { return !pidAlive(first.pid()) })
	if !pidAlive(child.Process.Pid) {
		t.Fatal("restarting the supervisor touched a live group")
	}
	second.closeStdin()
	procWaitUntil(t, 15*time.Second, "the replayed group killed by the replacement", func() bool { return waitExited(child, 10*time.Millisecond) })
}

func TestASupervisorDroppedOnABrokenPipeIsStillRestartedByItsWaiter(t *testing.T) {
	resetSupervisorState(t)
	supervisorRestartMinUptime = 0
	child := startVictim(t, "")
	ownProcessGroups([]int{child.Process.Pid})
	first := currentSupervisor()
	supervisorMu.Lock()
	syscall.Kill(first.pid(), syscall.SIGKILL)
	ownerSupervisor = nil
	supervisorMu.Unlock()
	procWaitUntil(t, 10*time.Second, "a replacement for the dropped supervisor", func() bool { h := currentSupervisor(); return h != nil && h != first })
	currentSupervisor().closeStdin()
	procWaitUntil(t, 15*time.Second, "the replayed group killed", func() bool { return waitExited(child, 10*time.Millisecond) })
}

func TestAnIdleSupervisorStaysForTheNextCommandAndExitsAfterTheGrace(t *testing.T) {
	resetSupervisorState(t)
	supervisorIdleExit = time.Second
	var first supervisorHandle
	for i := 0; i < 2; i++ {
		cmd := exec.Command("sh", "-c", "true")
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		ownProcessGroups([]int{cmd.Process.Pid})
		cmd.Wait()
		ReleaseProcessGroupOnceEmpty(cmd.Process.Pid, nil, 5*time.Second)
		if i == 0 {
			first = currentSupervisor()
			if first == nil {
				t.Fatal("no supervisor after the first command")
			}
			if exited, _ := first.poll(); exited {
				t.Fatal("the supervisor exited with the first command")
			}
		}
	}
	if currentSupervisor() != first {
		t.Fatal("the second command spawned another supervisor")
	}
	procWaitUntil(t, 10*time.Second, "the idle supervisor released", func() bool { return currentSupervisor() == nil })
	procWaitUntil(t, 10*time.Second, "the idle supervisor to exit", func() bool { exited, _ := first.poll(); return exited })
}

func TestASupervisorKilledEveryTimeItStartsIsNotRestartedInALoop(t *testing.T) {
	resetSupervisorState(t)
	prevScript := supervisorScript
	supervisorScript = "kill -9 $$\n"
	t.Cleanup(func() { supervisorScript = prevScript })
	var mu sync.Mutex
	spawned := []supervisorHandle{}
	spawnSupervisorFn = func() supervisorHandle {
		h := spawnSupervisor()
		mu.Lock()
		spawned = append(spawned, h)
		mu.Unlock()
		return h
	}
	child := startVictim(t, "")
	ownProcessGroups([]int{child.Process.Pid})
	settled := func() bool {
		mu.Lock()
		defer mu.Unlock()
		if len(spawned) < 2 || currentSupervisor() != nil {
			return false
		}
		for _, h := range spawned {
			if h == nil {
				continue
			}
			if exited, _ := h.poll(); !exited {
				return false
			}
		}
		return true
	}
	procWaitUntil(t, 15*time.Second, "the killed supervisor and its one young restart to settle", settled)
	time.Sleep(time.Second)
	mu.Lock()
	n := len(spawned)
	mu.Unlock()
	if n != 2 {
		t.Fatalf("%d supervisors spawned for one registration", n)
	}
}

// Release once empty

func releasedGroups(t *testing.T) *[][]int {
	t.Helper()
	calls := [][]int{}
	prev := releaseGroupsFn
	releaseGroupsFn = func(pgids []int) { calls = append(calls, append([]int{}, pgids...)) }
	t.Cleanup(func() { releaseGroupsFn = prev })
	return &calls
}

func TestAnEmptyGroupIsReleasedEvenWithAnUnreapedLeader(t *testing.T) {
	calls := releasedGroups(t)
	leader, err := popenBash([]string{"sh", "-c", "exit 0"}, nil, func(c *exec.Cmd) { startNewSession(c) })
	if err != nil {
		t.Fatal(err)
	}
	if !ReleaseProcessGroupOnceEmpty(leader.Pid(), func() { leader.Poll() }, 5*time.Second) {
		t.Fatal("empty group not released")
	}
	if len(*calls) != 1 || (*calls)[0][0] != leader.Pid() {
		t.Fatalf("calls %v", *calls)
	}
}

func TestAGroupWithALiveMemberStaysCovered(t *testing.T) {
	calls := releasedGroups(t)
	leader := startVictim(t, "trap '' TERM; exec sleep 60")
	if ReleaseProcessGroupOnceEmpty(leader.Process.Pid, nil, 200*time.Millisecond) {
		t.Fatal("a live group was released")
	}
	if len(*calls) != 0 {
		t.Fatalf("calls %v", *calls)
	}
}

func TestItWaitsForAMemberThatExitsLate(t *testing.T) {
	calls := releasedGroups(t)
	leader := startVictim(t, "exec sleep 60")
	time.AfterFunc(300*time.Millisecond, func() { syscall.Kill(-leader.Process.Pid, syscall.SIGKILL) })
	if !ReleaseProcessGroupOnceEmpty(leader.Process.Pid, func() { waitExited(leader, time.Millisecond) }, 10*time.Second) {
		t.Fatal("group not released after the late exit")
	}
	if len(*calls) != 1 || (*calls)[0][0] != leader.Process.Pid {
		t.Fatalf("calls %v", *calls)
	}
}

// Strict liveness probes still read the zombie state on POSIX

type countingProber struct {
	processProber
	mu    sync.Mutex
	calls []int
}

func (c *countingProber) Zombie(pid int) bool {
	c.mu.Lock()
	c.calls = append(c.calls, pid)
	c.mu.Unlock()
	return c.processProber.Zombie(pid)
}

func TestPosixStrictBranchStillProbesStatus(t *testing.T) {
	counting := &countingProber{processProber: osProcessProber{}}
	prev := identityProber
	identityProber = counting
	t.Cleanup(func() { identityProber = prev })
	pid := os.Getpid()
	birth, err := ProcessBirth(pid)
	if err != nil {
		t.Fatal(err)
	}
	alive, known := pidAliveMatches(pid, &birth, true, nil)
	if !alive || !known {
		t.Fatalf("alive=%v known=%v", alive, known)
	}
	if len(counting.calls) != 1 || counting.calls[0] != pid {
		t.Fatalf("zombie probes %v", counting.calls)
	}
}

func TestLedgerEntriesExcludesAKilledButUnreapedProcess(t *testing.T) {
	ledger := filepath.Join(t.TempDir(), "spawn-ledger.json")
	prev := ledgerPathFn
	ledgerPathFn = func() string { return ledger }
	t.Cleanup(func() { ledgerPathFn = prev })
	child := exec.Command("sleep", "60")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { child.Process.Kill(); child.Wait() })
	created, err := ProcessBirth(child.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	child.Process.Signal(syscall.SIGTERM)
	procWaitUntil(t, 10*time.Second, "the child to become a zombie", func() bool { return identityProber.Zombie(child.Process.Pid) })
	writeLedgerFile(t, ledger, []map[string]any{ledgerEntry(child.Process.Pid, created, "dashboard", "", nil, nil)})
	if live := LedgerEntries(LedgerOptions{ProjectRoot: "/x/install"}); len(live) != 0 {
		t.Fatalf("zombie listed as live: %v", live)
	}
}

func TestRegisterSelfWritesLedgerWith0600(t *testing.T) {
	_, ledger := identityFixture(t, map[int]float64{999: 50.0}, 999)
	old := syscall.Umask(0o022)
	defer syscall.Umask(old)
	if !RegisterSelf("serve", "/x/install", nil) {
		t.Fatal("register_self failed")
	}
	st, err := os.Stat(ledger)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v err %v", st.Mode(), err)
	}
}

// Helper process registering n children into the ledger at a shared instant
func TestLedgerWriterHelper(t *testing.T) {
	if os.Getenv("ICEBATH_TEST_HELPER") != "TestLedgerWriterHelper" {
		t.Skip("helper process entry point")
	}
	startAt, _ := strconv.ParseFloat(os.Getenv("ICEBATH_TEST_START_AT"), 64)
	n, _ := strconv.Atoi(os.Getenv("ICEBATH_TEST_N"))
	kids := []*exec.Cmd{}
	for i := 0; i < n; i++ {
		kid := exec.Command("sleep", "120")
		if err := kid.Start(); err != nil {
			os.Exit(3)
		}
		kids = append(kids, kid)
	}
	for nowSeconds() < startAt {
		time.Sleep(time.Millisecond)
	}
	for _, kid := range kids {
		RegisterChild(kid.Process.Pid, "ledger-race", "")
	}
	pids := []string{}
	for _, kid := range kids {
		pids = append(pids, strconv.Itoa(kid.Process.Pid))
	}
	fmt.Println(strings.Join(pids, " "))
	os.Stdout.Sync()
	io.ReadAll(os.Stdin)
	for _, kid := range kids {
		kid.Process.Kill()
		kid.Wait()
	}
}

func TestConcurrentProcessesNeverDropEachOthersLedgerRows(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	startAt := nowSeconds() + 2.0
	type writer struct {
		cmd   *exec.Cmd
		stdin io.WriteCloser
		out   *bufio.Reader
	}
	writers := []writer{}
	for i := 0; i < 3; i++ {
		cmd := helperCommand(t, "TestLedgerWriterHelper", "ICEBATH_HOME="+home,
			"ICEBATH_TEST_START_AT="+strconv.FormatFloat(startAt, 'f', 3, 64), "ICEBATH_TEST_N=15")
		stdin, _ := cmd.StdinPipe()
		stdout, _ := cmd.StdoutPipe()
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		writers = append(writers, writer{cmd, stdin, bufio.NewReader(stdout)})
	}
	defer func() {
		for _, w := range writers {
			w.stdin.Close()
			w.cmd.Wait()
		}
	}()
	pids := map[int]bool{}
	for _, w := range writers {
		line, _ := w.out.ReadString('\n')
		for _, f := range strings.Fields(line) {
			n, _ := strconv.Atoi(f)
			pids[n] = true
		}
	}
	if len(pids) != 45 {
		t.Fatalf("%d pids reported", len(pids))
	}
	rows := readLedgerFile(t, filepath.Join(home, "spawn-ledger.json"))
	kept := map[int]bool{}
	for _, r := range rows {
		if r["purpose"] == "ledger-race" {
			kept[int(r["pid"].(float64))] = true
		}
	}
	lost := 0
	for pid := range pids {
		if !kept[pid] {
			lost++
		}
	}
	if lost != 0 || len(kept) != len(pids) {
		t.Fatalf("%d of 45 rows lost to a concurrent writer", lost)
	}
}

func TestAStalledLedgerLockHolderNeverBlocksRegisterChild(t *testing.T) {
	ledger := filepath.Join(t.TempDir(), "home", "spawn-ledger.json")
	os.MkdirAll(filepath.Dir(ledger), 0o700)
	prevPath, prevTimeout, prevLogger := ledgerPathFn, ledgerLockTimeout, identityLogger
	ledgerPathFn = func() string { return ledger }
	ledgerLockTimeout = 300 * time.Millisecond
	ledgerLockWarned = false
	rec := &recordingHandler{}
	identityLogger = func() *slog.Logger { return slog.New(rec) }
	t.Cleanup(func() {
		ledgerPathFn, ledgerLockTimeout, identityLogger = prevPath, prevTimeout, prevLogger
		ledgerLockWarned = false
	})
	holder, err := os.OpenFile(ledger+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if err := syscall.Flock(int(holder.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	done := make(chan bool, 1)
	go func() { done <- RegisterChild(os.Getpid(), "stalled-lock", "") }()
	select {
	case ok := <-done:
		if !ok {
			t.Fatal("register_child failed past the stalled lock")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("register_child waited on a stalled lock holder")
	}
	if !RegisterChild(os.Getpid(), "stalled-again", "") {
		t.Fatal("a second write past the lock failed")
	}
	if n := rec.warnings("spawn ledger lock"); n != 1 {
		t.Fatalf("%d warnings for two writes past a stalled lock", n)
	}
	found := false
	for _, r := range readLedgerFile(t, ledger) {
		if r["purpose"] == "stalled-again" {
			found = true
		}
	}
	if !found {
		t.Fatal("the row was not written past the timeout")
	}
}

// Stdio MCP helper children in the ledger with real processes

func ledgerAt(t *testing.T) string {
	t.Helper()
	ledger := filepath.Join(t.TempDir(), LedgerFilename)
	prev := ledgerPathFn
	ledgerPathFn = func() string { return ledger }
	t.Cleanup(func() { ledgerPathFn = prev })
	return ledger
}

func sleepChild(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("sleep", "300")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	return cmd
}

// Pid and birth of a real process that is provably dead
func deadProcessIdentity(t *testing.T) (int, float64) {
	t.Helper()
	cmd := exec.Command("sleep", "300")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	create, err := ProcessBirth(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	cmd.Process.Kill()
	cmd.Wait()
	procWaitUntil(t, 5*time.Second, "the killed child to vanish", func() bool { return !identityProber.Running(cmd.Process.Pid) })
	return cmd.Process.Pid, create
}

func TestRegisterChildEntryShape(t *testing.T) {
	ledger := ledgerAt(t)
	child := sleepChild(t)
	if !RegisterChild(child.Process.Pid, "mcp-helper", "") {
		t.Fatal("register_child failed")
	}
	entries := readLedgerFile(t, ledger)
	if len(entries) != 1 {
		t.Fatalf("entries %v", entries)
	}
	e := entries[0]
	selfBirth, _ := ProcessBirth(os.Getpid())
	childBirth, _ := ProcessBirth(child.Process.Pid)
	if int(e["pid"].(float64)) != child.Process.Pid || e["purpose"] != "mcp-helper" || e["install"] != InstallID("") ||
		int(e["spawner_pid"].(float64)) != os.Getpid() || !approx(e["spawner_create"].(float64), selfBirth, 2.0) ||
		!approx(e["create_time"].(float64), childBirth, 2.0) || !approx(e["registered_at"].(float64), nowSeconds(), 30.0) {
		t.Fatalf("entry %v", e)
	}
	live := LedgerEntries(LedgerOptions{})
	if len(live) != 1 {
		t.Fatalf("live %v", live)
	}
	if pid, _ := entryInt(live[0], "pid"); pid != child.Process.Pid {
		t.Fatalf("live pid %d", pid)
	}
}

func TestRegisterChildRejectsDeadOrInvalidPids(t *testing.T) {
	ledger := ledgerAt(t)
	deadPid, _ := deadProcessIdentity(t)
	if RegisterChild(deadPid, "mcp-helper", "") || RegisterChild(0, "mcp-helper", "") || RegisterChild(-5, "mcp-helper", "") {
		t.Fatal("a dead or invalid pid was registered")
	}
	if _, err := os.Stat(ledger); err == nil && len(readLedgerFile(t, ledger)) != 0 {
		t.Fatal("ledger has rows")
	}
}

func TestLiveSpawnerProtection(t *testing.T) {
	ledgerAt(t)
	child := sleepChild(t)
	RegisterChild(child.Process.Pid, "mcp-helper", "")
	entries := LedgerEntries(LedgerOptions{})
	if len(entries) != 1 {
		t.Fatalf("entries %v", entries)
	}
	if dead, known := SpawnerIsDead(entries[0]); dead || !known {
		t.Fatalf("dead=%v known=%v", dead, known)
	}
	killed := []int{}
	reaped := ReapOrphanedMCPHelpers("", func(pid int) error { killed = append(killed, pid); return nil })
	if len(reaped) != 0 || len(killed) != 0 || !identityProber.Running(child.Process.Pid) {
		t.Fatalf("reaped %v killed %v", reaped, killed)
	}
}

// Rewrites the child's ledger entry so its spawner is a real dead process
func orphanEntryFor(t *testing.T, childPid int) {
	t.Helper()
	path := ledgerPathFn()
	deadPid, deadCreate := deadProcessIdentity(t)
	entries := readLedgerFile(t, path)
	for _, e := range entries {
		if int(e["pid"].(float64)) == childPid {
			e["spawner_pid"] = deadPid
			e["spawner_create"] = deadCreate
		}
	}
	writeLedgerFile(t, path, entries)
}

func TestDeadSpawnerReapEligibility(t *testing.T) {
	ledgerAt(t)
	child := sleepChild(t)
	RegisterChild(child.Process.Pid, "mcp-helper", "")
	orphanEntryFor(t, child.Process.Pid)
	entries := LedgerEntries(LedgerOptions{})
	if dead, known := SpawnerIsDead(entries[0]); !dead || !known {
		t.Fatalf("dead=%v known=%v", dead, known)
	}
	killed := []int{}
	reaped := ReapOrphanedMCPHelpers("", func(pid int) error { killed = append(killed, pid); return nil })
	if len(reaped) != 1 || reaped[0] != child.Process.Pid || len(killed) != 1 || killed[0] != child.Process.Pid {
		t.Fatalf("reaped %v killed %v", reaped, killed)
	}
}

func TestReapSelectionMixesLiveAndDeadSpawners(t *testing.T) {
	ledgerAt(t)
	live := sleepChild(t)
	orphan := sleepChild(t)
	RegisterChild(live.Process.Pid, "mcp-helper", "")
	RegisterChild(orphan.Process.Pid, "mcp-helper", "")
	orphanEntryFor(t, orphan.Process.Pid)
	killed := []int{}
	reaped := ReapOrphanedMCPHelpers("", func(pid int) error { killed = append(killed, pid); return nil })
	if len(reaped) != 1 || reaped[0] != orphan.Process.Pid {
		t.Fatalf("reaped %v", reaped)
	}
	for _, pid := range killed {
		if pid == live.Process.Pid {
			t.Fatal("the live spawner's helper was killed")
		}
	}
}

func TestReapActuallyKillsOrphan(t *testing.T) {
	ledgerAt(t)
	child := sleepChild(t)
	RegisterChild(child.Process.Pid, "mcp-helper", "")
	orphanEntryFor(t, child.Process.Pid)
	if reaped := ReapOrphanedMCPHelpers("", nil); len(reaped) != 1 || reaped[0] != child.Process.Pid {
		t.Fatalf("reaped %v", reaped)
	}
	if !waitExited(child, 5*time.Second) {
		t.Fatal("the orphan survived the reap")
	}
}

func TestReapIgnoresNonMCPPurposes(t *testing.T) {
	ledgerAt(t)
	child := sleepChild(t)
	RegisterChild(child.Process.Pid, "serve-child-not-helper", "")
	orphanEntryFor(t, child.Process.Pid)
	if reaped := ReapOrphanedMCPHelpers("", nil); len(reaped) != 0 {
		t.Fatalf("reaped %v", reaped)
	}
	if !identityProber.Running(child.Process.Pid) {
		t.Fatal("a non helper purpose was reaped")
	}
}

func TestLedgerPrunesExitedChildrenOnNextWrite(t *testing.T) {
	ledger := ledgerAt(t)
	doomed := exec.Command("sleep", "300")
	if err := doomed.Start(); err != nil {
		t.Fatal(err)
	}
	RegisterChild(doomed.Process.Pid, "mcp-helper", "")
	doomed.Process.Kill()
	doomed.Wait()
	procWaitUntil(t, 5*time.Second, "the doomed child to vanish", func() bool { return !identityProber.Running(doomed.Process.Pid) })
	survivor := sleepChild(t)
	RegisterChild(survivor.Process.Pid, "mcp-helper", "")
	pids := ledgerPids(readLedgerFile(t, ledger))
	if !pids[survivor.Process.Pid] || pids[doomed.Process.Pid] {
		t.Fatalf("pids %v", pids)
	}
}

func TestSupervisorArgvIsRecognised(t *testing.T) {
	argv := []string{posixSh, "-c", supervisorScript, supervisorArgv0, "123"}
	if !IsOwnerDeathSupervisor(argv) || IsOwnerDeathSupervisor([]string{"sh", "-c", "echo hi"}) {
		t.Fatal("supervisor argv recognition")
	}
}
