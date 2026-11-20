package state

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

const (
	cell2DBEnv      = "ICEBATH_CELL2_DB"
	cell2BarrierEnv = "ICEBATH_CELL2_BARRIER"
	cell2ReadyEnv   = "ICEBATH_CELL2_READY"
	cell2Claimants  = 8
)

// Child claimant that waits on the file barrier then exits 0 when its handoff claim wins and 10 when it loses
func TestCell2ClaimantHelperProcess(t *testing.T) {
	dbPath := os.Getenv(cell2DBEnv)
	if dbPath == "" {
		return
	}
	if err := os.WriteFile(os.Getenv(cell2ReadyEnv), nil, 0o644); err != nil {
		os.Exit(2)
	}
	deadline := time.Now().Add(55 * time.Second)
	for {
		if _, err := os.Stat(os.Getenv(cell2BarrierEnv)); err == nil {
			break
		}
		if time.Now().After(deadline) {
			os.Exit(3)
		}
		time.Sleep(5 * time.Millisecond)
	}
	db, err := NewSessionDB(context.Background(), dbPath, false)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(4)
	}
	won, err := db.ClaimHandoff(context.Background(), "cell2")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(5)
	}
	if won {
		os.Exit(0)
	}
	os.Exit(10)
}

func TestExactlyOneClaimantWins(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath, barrier := filepath.Join(dir, "state.db"), filepath.Join(dir, "go")
	db := usageOpen(t, dbPath)
	if err := db.CreateSession(NewSession{ID: "cell2", Source: "conformance"}); err != nil {
		t.Fatal(err)
	}
	if ok, err := db.RequestHandoff(ctx, "cell2", "telegram"); err != nil || !ok {
		t.Fatal(ok, err)
	}
	children := make([]*exec.Cmd, cell2Claimants)
	ready := make([]string, cell2Claimants)
	for i := range children {
		ready[i] = filepath.Join(dir, fmt.Sprintf("ready-%d", i))
		cmd := exec.Command(os.Args[0], "-test.run=^TestCell2ClaimantHelperProcess$")
		cmd.Env = append(os.Environ(), cell2DBEnv+"="+dbPath, cell2BarrierEnv+"="+barrier, cell2ReadyEnv+"="+ready[i])
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		children[i] = cmd
	}
	deadline := time.Now().Add(60 * time.Second)
	for waiting := true; waiting; {
		waiting = false
		for _, r := range ready {
			if _, err := os.Stat(r); err != nil {
				waiting = true
			}
		}
		if waiting && time.Now().After(deadline) {
			t.Fatal("claimants never reached the barrier")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := os.WriteFile(barrier, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	codes := make([]int, cell2Claimants)
	winners := 0
	for i, cmd := range children {
		err := cmd.Wait()
		var exit *exec.ExitError
		switch {
		case err == nil:
			codes[i] = 0
		case errors.As(err, &exit):
			codes[i] = exit.ExitCode()
		default:
			t.Fatal(err)
		}
		if codes[i] == 0 {
			winners++
		}
	}
	mode := repairScalar(t, db, "PRAGMA journal_mode")
	for _, rc := range codes {
		if rc != 0 && rc != 10 {
			t.Fatalf("[journal_mode=%v] claimant crashed or timed out: %v", mode, codes)
		}
	}
	if winners != 1 {
		t.Fatalf("[journal_mode=%v] consume-once violated: %d of %d concurrent claimants won", mode, winners, cell2Claimants)
	}
}
