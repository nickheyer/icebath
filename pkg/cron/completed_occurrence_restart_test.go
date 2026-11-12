package cron

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Production seams in place of the recorders the test package installs, for a child process run
func restoreRealSeams() {
	runOneJobSeam, reconcilePending, drainLegacyPending = realRunOneJob, realReconcilePending, realDrainLegacyPending
	worktreePruneStaleWorktrees, loadCronScheduler = realWorktreePrune, realLoadCronScheduler
	deliverResultSeam, resolveDeliveryTargets = realDeliverResult, realResolveDeliveryTargets
	schedulerResolver = resolveCronScheduler
}

// Child modes of the restart test, a fresh process firing the home's jobs or acting as the external worker
func runOccurrenceChild(ctx context.Context, s *Store, mode, arg string) error {
	restoreRealSeams()
	switch mode {
	case "external-worker":
		SetupWorkerLogging = func(string, string) error { return nil }
		RegisterWorkerConfigHooks = func(context.Context, string, slog.Level) {}
		WorkerProcessBoot = func() {}
		DispatchExternalWorkerEntry(os.Exit)
		return errors.New("the external worker entry did not dispatch")
	case "fire":
		return fireOccurrenceChild(withCronStore(ctx, s), s, arg)
	case "crash-before-dispatch":
		tickCreateExecution = func(*Ledger, context.Context, string, string, *string) (map[string]any, error) {
			os.Exit(137)
			return nil, nil
		}
		_, err := schedulerTick(withCronStore(ctx, s), s, tickOptions{Verbose: false, Sync: true})
		return err
	}
	return fmt.Errorf("unknown ledger child mode %s", mode)
}

// One fire of every stored job through the tick, the provider or a handed off external worker
func fireOccurrenceChild(ctx context.Context, s *Store, mode string) error {
	if mode == "builtin" {
		_, err := schedulerTick(ctx, s, tickOptions{Verbose: false, Sync: true})
		return err
	}
	provider := &InProcessCronScheduler{}
	jobs, err := s.LoadJobs(ctx)
	if err != nil {
		return err
	}
	for _, job := range jobs {
		if mode != "worker" {
			if _, err := schedulerFireDue(ctx, provider, job.ID, FireOptions{Force: mode == "manual"}); err != nil {
				return err
			}
			continue
		}
		claim, err := schedulerClaimFire(ctx, provider, job.ID, false, false)
		if err != nil {
			return err
		}
		if claim == nil {
			continue
		}
		if _, err := openExecutionsLedger(s).MarkHandoffPending(ctx, *claim.ExecutionID); err != nil {
			return err
		}
		payload := filepath.Join(s.Home(), *claim.ExecutionID+".json")
		ack := filepath.Join(s.Home(), *claim.ExecutionID+".ready")
		if err := writeWorkerPayload(payload, claim, s.Home(), false); err != nil {
			return err
		}
		wctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		worker := exec.CommandContext(wctx, os.Args[0], ExternalWorkerEntry, "--external-worker-file", payload, "--ack-file", ack)
		worker.Env = append(childEnviron(), ledgerChildEnv+"=external-worker")
		out, err := worker.CombinedOutput()
		cancel()
		if err != nil {
			return fmt.Errorf("external worker: %v %s", err, out)
		}
		raw, err := os.ReadFile(ack)
		if err != nil {
			return err
		}
		var acked map[string]any
		if err := json.Unmarshal(raw, &acked); err != nil {
			return err
		}
		if pid, _ := acked["pid"].(float64); int(pid) == os.Getpid() {
			return errors.New("the worker acknowledged from the firing process")
		}
	}
	return nil
}

// Environment of this process without the ledger child selectors
func childEnviron() []string {
	var out []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, ledgerChildEnv) {
			out = append(out, kv)
		}
	}
	return out
}

// Fires the home's jobs from a fresh process the way the Python _fire helper runs a new interpreter
func fireOccurrence(t *testing.T, h, mode string) {
	t.Helper()
	var env []string
	for _, kv := range os.Environ() {
		key, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(key, "ICEBATH_") || strings.HasPrefix(key, "_ICEBATH_") || strings.HasSuffix(key, "_API_KEY") || strings.HasSuffix(key, "_TOKEN") {
			continue
		}
		env = append(env, kv)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0])
	cmd.Env = append(env, "ICEBATH_HOME="+h, ledgerChildEnv+"=fire", ledgerChildEnv+"_ARG="+mode)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s fire: %v %s", mode, err, out)
	}
}

func TestCompletedOccurrenceSurvivesRestartAndPrestampRollback(t *testing.T) {
	for _, mode := range []string{"builtin", "provider", "worker"} {
		t.Run(mode, func(t *testing.T) {
			h := filepath.Join(t.TempDir(), mode)
			for _, d := range []string{"cron", "scripts"} {
				if err := os.MkdirAll(filepath.Join(h, d), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			effect := filepath.Join(h, "effects.txt")
			script := filepath.Join(h, "scripts", "effect.py")
			body := fmt.Sprintf("from pathlib import Path\np = Path(%s)\nwith p.open('a') as f: f.write('effect\\n')\nprint('done')\n", pyrepr.StrRepr(effect))
			if err := os.WriteFile(script, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			job := map[string]any{
				"id": "occurrence", "name": "occurrence", "prompt": "",
				"schedule":    map[string]any{"kind": "interval", "minutes": 240},
				"next_run_at": FormatISO(time.Now().Add(-30 * time.Minute)), "enabled": true, "state": "scheduled",
				"script": script, "no_agent": true, "deliver": "local",
				"repeat": map[string]any{"times": nil, "completed": 0},
			}
			store := filepath.Join(h, "cron", "jobs.json")
			write := func() string {
				t.Helper()
				raw, err := json.Marshal(map[string]any{"jobs": []any{job}})
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(store, raw, 0o600); err != nil {
					t.Fatal(err)
				}
				return string(raw)
			}
			effects := func() []string {
				t.Helper()
				raw, err := os.ReadFile(effect)
				if err != nil {
					t.Fatal(err)
				}
				return strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
			}
			snapshot := write()
			fireOccurrence(t, h, mode)
			if got := effects(); !reflect.DeepEqual(got, []string{"effect"}) {
				t.Fatalf("first fire %v", got)
			}
			if err := os.WriteFile(store, []byte(snapshot), 0o600); err != nil {
				t.Fatal(err)
			}
			fireOccurrence(t, h, mode)
			if got := effects(); !reflect.DeepEqual(got, []string{"effect"}) {
				t.Fatalf("completed occurrence executed twice %v", got)
			}
			job["next_run_at"] = FormatISO(time.Now().Add(-10 * time.Minute))
			write()
			fireOccurrence(t, h, mode)
			if got := effects(); len(got) != 2 {
				t.Fatalf("another missed slot %v", got)
			}
			job["next_run_at"] = FormatISO(time.Now().Add(-5 * time.Minute))
			pending := write()
			fireOccurrence(t, h, "manual")
			if err := os.WriteFile(store, []byte(pending), 0o600); err != nil {
				t.Fatal(err)
			}
			fireOccurrence(t, h, mode)
			if got := effects(); len(got) != 4 {
				t.Fatalf("manual force completed the pending slot %v", got)
			}
		})
	}
}
