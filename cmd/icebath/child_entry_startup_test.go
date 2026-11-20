package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/cron"
)

// The external cron worker entry dispatched by the startup order logs its payload failure to the home log
func TestExternalCronWorkerEntryRunsThroughStartupOrder(t *testing.T) {
	h := t.TempDir()
	payload := filepath.Join(h, "cron", "external-workers", "exec-1.json")
	ack := filepath.Join(h, "cron", "external-workers", "exec-1.ready")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], cron.ExternalWorkerEntry, "--external-worker-file", payload, "--ack-file", ack)
	cmd.Env = append(os.Environ(), "ICEBATH_HOME="+h)
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 {
		t.Fatalf("worker exit = %v, want status 1\n%s", err, out)
	}
	if strings.Contains(string(out), "panic") {
		t.Fatalf("worker panicked\n%s", out)
	}
	logged, err := os.ReadFile(filepath.Join(h, "logs", "errors.log"))
	if err != nil {
		t.Fatalf("worker wrote no persistent error log: %v\n%s", err, out)
	}
	if !strings.Contains(string(logged), "Cron external worker could not load payload "+payload) {
		t.Fatalf("errors.log lacks the payload failure\n%s\n%s", logged, out)
	}
	if _, err := os.Stat(ack); !os.IsNotExist(err) {
		t.Fatalf("worker published an acknowledgement for an unloadable payload: %v", err)
	}
}
