package dashboard

import (
	"context"
	"slices"
	"strings"
	"testing"
)

type fakeActionProc struct {
	pid  int
	code *int
}

func (p *fakeActionProc) Pid() int         { return p.pid }
func (p *fakeActionProc) Poll() *int       { return p.code }
func (p *fakeActionProc) Terminate() error { return nil }

// Child handle that already exited zero
func exitedProc(pid int) *fakeActionProc {
	code := 0
	return &fakeActionProc{pid: pid, code: &code}
}

// Restores the restart cooldown and action tables after a case
func resetRestartState(t *testing.T, procs map[string]actionProc, commands map[string][]string) {
	t.Helper()
	actionMu.Lock()
	savedProcs, savedCommands := actionProcs, actionCommands
	actionProcs, actionCommands = procs, commands
	actionMu.Unlock()
	savedSub, savedSpawn, savedClock := restartGatewaySubcommand, restartSpawnAction, restartMonotonic
	lastGatewayRestart = nil
	t.Cleanup(func() {
		actionMu.Lock()
		actionProcs, actionCommands = savedProcs, savedCommands
		actionMu.Unlock()
		restartGatewaySubcommand, restartSpawnAction, restartMonotonic = savedSub, savedSpawn, savedClock
		lastGatewayRestart = nil
	})
}

// Fake spawner counting calls and returning the handles in order
func fakeSpawner(procs ...actionProc) (*int, func(context.Context, []string, string, map[string]string) (actionProc, error)) {
	calls := 0
	return &calls, func(context.Context, []string, string, map[string]string) (actionProc, error) {
		p := procs[min(calls, len(procs)-1)]
		calls++
		return p, nil
	}
}

// Clock returning the values in order, failing the case when exhausted
func fakeClock(t *testing.T, values ...float64) func() float64 {
	i := 0
	return func() float64 {
		if i >= len(values) {
			t.Fatalf("monotonic called more than %d times", len(values))
		}
		i++
		return values[i-1]
	}
}

func fixedSubcommand(argv ...string) func(context.Context, string, string) ([]string, error) {
	return func(context.Context, string, string) ([]string, error) { return slices.Clone(argv), nil }
}

func TestSecondRequestAfterTheChildExitsIsCoalesced(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	resetRestartState(t, map[string]actionProc{}, map[string][]string{})
	proc := exitedProc(4242)
	calls, spawn := fakeSpawner(proc)
	restartGatewaySubcommand, restartSpawnAction = fixedSubcommand("gateway", "restart"), spawn
	restartMonotonic = fakeClock(t, 100.0, 103.5)
	first, firstReused, err := spawnGatewayRestart(context.Background(), "", false)
	if err != nil {
		t.Fatal(err)
	}
	second, secondReused, err := spawnGatewayRestart(context.Background(), "", false)
	if err != nil {
		t.Fatal(err)
	}
	if *calls != 1 {
		t.Fatalf("a repeat request 3.5s later must not start a second restart, spawned %d", *calls)
	}
	if first != proc || firstReused || second != proc || !secondReused {
		t.Fatalf("first=%v/%v second=%v/%v", first, firstReused, second, secondReused)
	}
}

func TestAStormOfRequestsProducesExactlyOneRestart(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	resetRestartState(t, map[string]actionProc{}, map[string][]string{})
	calls, spawn := fakeSpawner(exitedProc(4242))
	restartGatewaySubcommand, restartSpawnAction = fixedSubcommand("gateway", "restart"), spawn
	restartMonotonic = fakeClock(t, 100.0, 103.5, 107.0)
	for range 3 {
		if _, _, err := spawnGatewayRestart(context.Background(), "", false); err != nil {
			t.Fatal(err)
		}
	}
	if *calls != 1 {
		t.Fatalf("spawned %d", *calls)
	}
}

func TestCooldownSurvivesTheActionTableBeingCleared(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	resetRestartState(t, map[string]actionProc{}, map[string][]string{})
	calls, spawn := fakeSpawner(exitedProc(4242))
	restartGatewaySubcommand, restartSpawnAction = fixedSubcommand("gateway", "restart"), spawn
	restartMonotonic = fakeClock(t, 100.0, 102.0)
	if _, _, err := spawnGatewayRestart(context.Background(), "", false); err != nil {
		t.Fatal(err)
	}
	actionMu.Lock()
	clear(actionProcs)
	clear(actionCommands)
	actionMu.Unlock()
	_, reused, err := spawnGatewayRestart(context.Background(), "", false)
	if err != nil {
		t.Fatal(err)
	}
	if *calls != 1 || !reused {
		t.Fatalf("spawned %d reused %v", *calls, reused)
	}
}

func TestRequestAfterTheWindowStartsARealRestart(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	resetRestartState(t, map[string]actionProc{}, map[string][]string{})
	calls, spawn := fakeSpawner(exitedProc(1), exitedProc(2))
	restartGatewaySubcommand, restartSpawnAction = fixedSubcommand("gateway", "restart"), spawn
	restartMonotonic = fakeClock(t, 100.0, 111.0, 111.0)
	if _, _, err := spawnGatewayRestart(context.Background(), "", false); err != nil {
		t.Fatal(err)
	}
	second, reused, err := spawnGatewayRestart(context.Background(), "", false)
	if err != nil {
		t.Fatal(err)
	}
	if *calls != 2 || reused || second.Pid() != 2 {
		t.Fatalf("spawned %d reused %v pid %d", *calls, reused, second.Pid())
	}
}

func TestADifferentProfileIsNeverCoalesced(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	resetRestartState(t, map[string]actionProc{}, map[string][]string{})
	calls, spawn := fakeSpawner(exitedProc(1), exitedProc(2))
	argvs := [][]string{{"gateway", "restart"}, {"-p", "coder", "gateway", "restart"}}
	n := 0
	restartGatewaySubcommand = func(context.Context, string, string) ([]string, error) {
		n++
		return argvs[n-1], nil
	}
	restartSpawnAction = spawn
	restartMonotonic = fakeClock(t, 100.0, 101.0)
	if _, _, err := spawnGatewayRestart(context.Background(), "", false); err != nil {
		t.Fatal(err)
	}
	second, reused, err := spawnGatewayRestart(context.Background(), "coder", false)
	if err != nil {
		t.Fatal(err)
	}
	if *calls != 2 || reused || second.Pid() != 2 {
		t.Fatalf("spawned %d reused %v pid %d", *calls, reused, second.Pid())
	}
}

func TestLiveChildIsStillReusedWithoutConsultingTheClock(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	live := &fakeActionProc{pid: 7}
	resetRestartState(t, map[string]actionProc{"gateway-restart": live},
		map[string][]string{"gateway-restart": {"gateway", "restart"}})
	calls, spawn := fakeSpawner(exitedProc(1))
	restartGatewaySubcommand, restartSpawnAction = fixedSubcommand("gateway", "restart"), spawn
	restartMonotonic = fakeClock(t)
	proc, reused, err := spawnGatewayRestart(context.Background(), "", false)
	if err != nil {
		t.Fatal(err)
	}
	if proc != live || !reused || *calls != 0 {
		t.Fatalf("proc %v reused %v spawned %d", proc, reused, *calls)
	}
}

func TestLiveChildForAnotherProfileStillRaises(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	live := &fakeActionProc{pid: 7}
	resetRestartState(t, map[string]actionProc{"gateway-restart": live},
		map[string][]string{"gateway-restart": {"-p", "coder", "gateway", "restart"}})
	calls, spawn := fakeSpawner(exitedProc(1))
	restartGatewaySubcommand, restartSpawnAction = fixedSubcommand("gateway", "restart"), spawn
	_, _, err := spawnGatewayRestart(context.Background(), "", false)
	if err == nil || !strings.Contains(err.Error(), "another profile") {
		t.Fatalf("err = %v", err)
	}
	if *calls != 0 {
		t.Fatalf("spawned %d", *calls)
	}
}

func TestBackendSpawnsRestartWithoutStoppingAnything(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	resetRestartState(t, map[string]actionProc{}, map[string][]string{})
	mockProc := &fakeActionProc{pid: 9}
	calls, spawn := fakeSpawner(mockProc)
	restartGatewaySubcommand, restartSpawnAction = fixedSubcommand("gateway", "restart"), spawn
	proc, reused, err := spawnGatewayRestart(context.Background(), "", false)
	if err != nil {
		t.Fatal(err)
	}
	if *calls != 1 || proc != mockProc || reused {
		t.Fatalf("spawned %d proc %v reused %v", *calls, proc, reused)
	}
}
