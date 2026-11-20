package agent

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	trajectoryWriterEnv = "ICEBATH_TEST_TRAJECTORY_WRITER"
	trajectoryHolderEnv = "ICEBATH_TEST_TRAJECTORY_HOLDER"
	trajectoryTargetEnv = "ICEBATH_TEST_TRAJECTORY_TARGET"
)

// Runs a trajectory writer or lock holder child and exits before the suite when the env selects one
func dispatchTrajectoryChild() {
	target := os.Getenv(trajectoryTargetEnv)
	if n := os.Getenv(trajectoryWriterEnv); n != "" {
		big := strings.Repeat("x", 300_000)
		for i := range 5 {
			SaveTrajectory([]any{map[string]any{"from": "human", "value": fmt.Sprintf("P%s-%d ", n, i) + big}}, "m", true, target)
		}
		os.Exit(0)
	}
	if os.Getenv(trajectoryHolderEnv) != "" {
		f, err := os.OpenFile(target, os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil || lockAppendHandle(f, true) != nil {
			os.Exit(3)
		}
		fmt.Println("locked")
		time.Sleep(1500 * time.Millisecond)
		os.Exit(0)
	}
}

func trajectoryChild(t *testing.T, env ...string) *exec.Cmd {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^$")
	cmd.Env = append(os.Environ(), env...)
	return cmd
}

func TestTrajectoryConcurrentProcessAppendsStayParseable(t *testing.T) {
	target := filepath.Join(t.TempDir(), "trajectory_samples.jsonl")
	var procs []*exec.Cmd
	for n := range 6 {
		cmd := trajectoryChild(t, fmt.Sprintf("%s=%d", trajectoryWriterEnv, n), trajectoryTargetEnv+"="+target)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		procs = append(procs, cmd)
	}
	for _, p := range procs {
		if err := p.Wait(); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != 30 {
		t.Fatalf("%d lines", len(lines))
	}
	tags := map[string]bool{}
	for _, ln := range lines {
		var entry struct {
			Conversations []struct{ Value string } `json:"conversations"`
		}
		if err := json.Unmarshal([]byte(ln), &entry); err != nil {
			t.Fatalf("line does not parse: %v", err)
		}
		tags[strings.SplitN(entry.Conversations[0].Value, " ", 2)[0]] = true
	}
	for n := range 6 {
		for i := range 5 {
			if !tags[fmt.Sprintf("P%d-%d", n, i)] {
				t.Fatalf("missing P%d-%d", n, i)
			}
		}
	}
}

func TestTrajectoryAppendHonoursAForeignExclusiveLock(t *testing.T) {
	target := filepath.Join(t.TempDir(), "failed_trajectories.jsonl")
	if err := os.WriteFile(target, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	holder := trajectoryChild(t, trajectoryHolderEnv+"=1", trajectoryTargetEnv+"="+target)
	out, err := holder.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = holder.Process.Kill()
		_ = holder.Wait()
	}()
	line, _ := bufio.NewReader(out).ReadString('\n')
	if strings.TrimSpace(line) != "locked" {
		t.Fatalf("holder said %q", line)
	}
	started := time.Now()
	SaveTrajectory([]any{map[string]any{"from": "human", "value": "hi"}}, "m", false, target)
	if waited := time.Since(started); waited < time.Second {
		t.Fatalf("append went through a held lock after %.2fs", waited.Seconds())
	}
	data, _ := os.ReadFile(target)
	rows := strings.Split(strings.TrimSpace(string(data)), "\n")
	var entry map[string]any
	if err := json.Unmarshal([]byte(rows[len(rows)-1]), &entry); err != nil || entry["completed"] != false {
		t.Fatalf("last entry %v %v", entry, err)
	}
}

func TestTrajectoryEntryShapeAndScratchpadHelpers(t *testing.T) {
	trajectoryNow = func() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 6000, time.Local) }
	t.Cleanup(func() { trajectoryNow = time.Now })
	dir := t.TempDir()
	t.Chdir(dir)
	SaveTrajectory([]any{map[string]any{"from": "gpt", "value": "é"}}, "m", true, "")
	data, err := os.ReadFile(filepath.Join(dir, "trajectory_samples.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"conversations": [{"from": "gpt", "value": "é"}], "timestamp": "2026-01-02T03:04:05.000006", "model": "m", "completed": true}` + "\n"
	if string(data) != want {
		t.Fatalf("entry %q", data)
	}
	if ConvertScratchpadToThink("a<REASONING_SCRATCHPAD>b</REASONING_SCRATCHPAD>") != "a<think>b</think>" || ConvertScratchpadToThink("") != "" {
		t.Fatal("scratchpad conversion")
	}
	if !HasIncompleteScratchpad("<REASONING_SCRATCHPAD>x") || HasIncompleteScratchpad("<REASONING_SCRATCHPAD>x</REASONING_SCRATCHPAD>") || HasIncompleteScratchpad("") {
		t.Fatal("incomplete scratchpad")
	}
}
