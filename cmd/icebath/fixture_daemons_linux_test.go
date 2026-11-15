//go:build linux

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/state"
)

// Ordinary daemon that appends every goroutine stack and its identify answer to logs/stacks.txt, the stack_dump_daemon fixture
const stackDumpDaemonMode = "stack-dump-daemon"

// Ordinary daemon whose scheduling skips a session holding a SAFE_QUEUE admission, the api_recovery_daemon fixture
const apiRecoveryDaemonMode = "api-recovery-daemon"

func init() {
	childModes[stackDumpDaemonMode] = armStackDump
	childModes[apiRecoveryDaemonMode] = holdSafeQueueScheduling
}

// Starts the delayed repeating diagnostics dump and returns to the ordinary entry
func armStackDump() {
	h := os.Getenv(home.EnvHome)
	path := filepath.Join(h, "logs", "stacks.txt")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	sink, err := os.Create(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	delay := 60.0
	if raw, set := os.LookupEnv("UGW_STACK_DUMP_AFTER"); set {
		if delay, err = strconv.ParseFloat(raw, 64); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
	}
	go func() {
		for range time.Tick(time.Duration(delay * float64(time.Second))) {
			buf := make([]byte, 1<<22)
			buf = buf[:runtime.Stack(buf, true)]
			fmt.Fprintf(sink, "\n=== goroutines @ %d ===\n%s\n=== runtime descriptors ===\n", time.Now().Unix(), buf)
			identified, _ := json.Marshal(gateway.IdentifyGateway(h, 2*time.Second))
			fmt.Fprintf(sink, "local identify=%s\n", identified)
		}
	}()
}

// Wraps scheduling so a session with a pending SAFE_QUEUE admission is held while everything else drains
func holdSafeQueueScheduling() {
	var schedule gateway.SessionSchedule
	schedule = gateway.SwapSessionSchedule(func(ctx context.Context, a *gateway.SessionAuthority, ref gateway.SessionRef) error {
		pending, err := state.ListSessionAdmissions(ctx, a.DB, ref.SessionID, true)
		if err != nil {
			return err
		}
		for _, row := range pending {
			raw, err := json.Marshal(row)
			if err != nil {
				return err
			}
			var decoded struct {
				Payload map[string]any `json:"payload"`
			}
			if err := json.Unmarshal(raw, &decoded); err != nil {
				return err
			}
			if decoded.Payload["text"] == "SAFE_QUEUE" {
				return nil
			}
		}
		return schedule(ctx, a, ref)
	})
}
