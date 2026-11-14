//go:build !windows

package tuiserver

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Pid of a live descendant of this process named name whose arguments carry marker, zero when none runs yet
func descendantRunning(t *testing.T, name, marker string) int {
	t.Helper()
	out, err := exec.Command("ps", "-A", "-o", "pid=", "-o", "ppid=", "-o", "stat=", "-o", "comm=", "-o", "args=").Output()
	if err != nil {
		t.Fatal(err)
	}
	parents := map[int]int{}
	var matches []int
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		pid, _ := strconv.Atoi(fields[0])
		ppid, _ := strconv.Atoi(fields[1])
		parents[pid] = ppid
		if !strings.HasPrefix(fields[2], "Z") && fields[3] == name && strings.Contains(strings.Join(fields[4:], " "), marker) {
			matches = append(matches, pid)
		}
	}
	self := os.Getpid()
	for _, pid := range matches {
		for p, hops := parents[pid], 0; p > 1 && hops < 64; p, hops = parents[p], hops+1 {
			if p == self {
				return pid
			}
		}
	}
	return 0
}

// Waits for the process to end, a zombie counting as ended, false when it outlives the timeout
func waitProcessGone(pid int, timeout time.Duration) bool {
	for deadline := time.Now().Add(timeout); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if syscall.Kill(pid, 0) != nil {
			return true
		}
		out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
		if err != nil || strings.HasPrefix(strings.TrimSpace(string(out)), "Z") {
			return true
		}
	}
	return false
}

// Waits up to twenty seconds for the foreground sleep carrying the marker to start
func awaitForegroundSleep(t *testing.T, marker string) int {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if pid := descendantRunning(t, "sleep", marker); pid != 0 {
			return pid
		}
	}
	t.Fatal("test setup: foreground sleep never started")
	return 0
}

// Agent whose interrupt raises the interrupt bit of the tool worker of its turn
type midToolAgent struct{ worker tools.WorkerID }

func (a *midToolAgent) Interrupt(message string) { tools.SetInterrupt(a.worker, true, "") }

func TestShutdownMidToolKillsTheCommandAndKeepsItsResult(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	t.Cleanup(tools.ResetForegroundExitFenceForTests)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	env := tools.NewLocalEnvironment(ctx, &tools.Env{Home: h}, cwd, 600, nil, false)
	defer env.Cleanup(false)
	var mu sync.Mutex
	messages := []map[string]any{{"role": "assistant", "tool_calls": []any{map[string]any{"id": "call-1"}}}}
	worker := tools.NewWorkerID()
	runThread := memory.SpawnContextThread(ctx, "turn", func(ctx context.Context) {
		out, _ := env.Execute(tools.WithWorker(ctx, worker), "sleep 3518", tools.ExecOptions{Timeout: 600})
		time.Sleep(200 * time.Millisecond)
		mu.Lock()
		messages = append(messages, map[string]any{"role": "tool", "tool_call_id": "call-1", "content": out.Output})
		mu.Unlock()
	})
	runThread.Start()
	defer tools.SetInterrupt(worker, false, "")
	sleeper := awaitForegroundSleep(t, "3518")
	defer syscall.Kill(sleeper, syscall.SIGKILL)
	var atTeardown [][]map[string]any
	swapShutdownSteps(t, func(*float64) int { return 0 }, func() bool { return false },
		func(context.Context, string, string, func(*tuigateway.Session) bool) bool {
			mu.Lock()
			defer mu.Unlock()
			atTeardown = append(atTeardown, append([]map[string]any(nil), messages...))
			return true
		})
	savedSettle := exitTurnSettleS
	exitTurnSettleS = 10.0
	defer func() { exitTurnSettleS = savedSettle }()
	session := tuigateway.NewSession(map[string]any{"agent": &midToolAgent{worker: worker}, "session_key": "sess-mid-tool",
		"running": true, "_run_thread": runThread, "history_lock": &pythread.RLock{}})
	swapSessions(t, map[string]*tuigateway.Session{"sess-mid-tool": session})
	shutdownSessions()
	if !waitProcessGone(sleeper, 15*time.Second) {
		t.Fatal("foreground command survived gateway shutdown")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(atTeardown) == 0 || atTeardown[0][len(atTeardown[0])-1]["tool_call_id"] != "call-1" {
		t.Fatalf("teardown persisted a tool_call with no result: %v", atTeardown)
	}
}

func TestSigtermGraceHardExitKillsASigtermIgnoringCommand(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	t.Cleanup(tools.ResetForegroundExitFenceForTests)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	env := tools.NewLocalEnvironment(ctx, &tools.Env{Home: h}, cwd, 600, nil, false)
	defer env.Cleanup(false)
	ran := make(chan struct{})
	go func() {
		defer close(ran)
		_, _ = env.Execute(ctx, "trap '' TERM; sleep 3522", tools.ExecOptions{Timeout: 600})
	}()
	sleeper := awaitForegroundSleep(t, "3522")
	defer syscall.Kill(sleeper, syscall.SIGKILL)
	var exits []int
	savedExit := entryOSExit
	entryOSExit = func(code int) { exits = append(exits, code) }
	defer func() { entryOSExit = savedExit }()
	started := time.Now()
	hardExit()
	elapsed := time.Since(started)
	if !waitProcessGone(sleeper, 5*time.Second) {
		t.Fatal("SIGTERM-ignoring foreground command survived the hard exit")
	}
	if len(exits) != 1 || exits[0] != 0 || elapsed >= 900*time.Millisecond {
		t.Fatalf("hard exit waited %.2fs (a TERM grace) first, exits %v", elapsed.Seconds(), exits)
	}
	select {
	case <-ran:
	case <-time.After(5 * time.Second):
	}
}
