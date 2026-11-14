package cron

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/provider"
)

// Store whose execution ledger lives in a throwaway file, the execution_ledger fixture
func executionLedgerStore(t *testing.T, h string) *Store {
	t.Helper()
	s := openCronStore(h, nil)
	s.executionsFile = filepath.Join(t.TempDir(), "executions.db")
	return s
}

// Restores the scheduler doors one case replaces
func saveSchedulerDoors(t *testing.T) {
	t.Helper()
	launch, claim, mark, body := launchExternalCronWorkerSeam, claimDispatchSeam, markJobRunSeam, waitForExternalCronWorkerBodySeam
	t.Cleanup(func() {
		launchExternalCronWorkerSeam, claimDispatchSeam, markJobRunSeam, waitForExternalCronWorkerBodySeam = launch, claim, mark, body
	})
}

func handoffBoom(*Store, context.Context, *Job) (bool, error) {
	return false, errors.New("worker exited before ownership acknowledgement")
}

func TestDispatchFailureOpensIncidentAndDeliversFailureNotice(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	s := executionLedgerStore(t, filepath.Join(t.TempDir(), "home"))
	ctx := context.Background()
	saveSchedulerDoors(t)
	launchExternalCronWorkerSeam = handoffBoom
	markJobRunSeam = func(*Store, context.Context, string, bool, MarkOptions) (bool, error) { return true, nil }
	var delivered []string
	prevDeliver := deliverResultSeam
	t.Cleanup(func() { deliverResultSeam = prevDeliver })
	deliverResultSeam = func(_ *Store, _ context.Context, _ *Job, content string, _, _ any, _ bool) (*string, error) {
		delivered = append(delivered, content)
		return nil, nil
	}
	ledger := openExecutionsLedger(s)
	record, err := ledger.Create(ctx, "job-dispatch", "builtin", nil)
	if err != nil {
		t.Fatal(err)
	}
	id := record["id"].(string)
	if ok, err := runOneJob(ctx, s, &Job{ID: "job-dispatch", ExecutionID: &id, Deliver: strPtr("telegram:123")}, runOneJobOptions{}); err != nil || !ok {
		t.Fatalf("run = %v, %v", ok, err)
	}
	rows, err := openIncidents(s).ListIncidents(nil)
	if err != nil || len(rows) != 1 || rows[0].JobID != "job-dispatch" || rows[0].State != "alerted" ||
		!strings.Contains(rows[0].Error, "Restart-safe cron worker dispatch failed") {
		t.Fatalf("incidents = %+v, %v", rows, err)
	}
	if len(delivered) != 1 || strings.TrimSpace(delivered[0]) == "" {
		t.Fatalf("delivered = %q", delivered)
	}
	finished, err := ledger.Get(ctx, id)
	if err != nil || finished["status"] != "failed" || !strings.Contains(pyStrOrEmpty(finished["error"]), "Restart-safe cron worker dispatch failed") ||
		finished["delivery_outcome"] != "delivered" {
		t.Fatalf("finished = %v, %v", finished, err)
	}
	repeat, err := ledger.Create(ctx, "job-dispatch", "builtin", nil)
	if err != nil {
		t.Fatal(err)
	}
	repeatID := repeat["id"].(string)
	if ok, err := runOneJob(ctx, s, &Job{ID: "job-dispatch", ExecutionID: &repeatID, Deliver: strPtr("telegram:123")}, runOneJobOptions{}); err != nil || !ok {
		t.Fatalf("repeat run = %v, %v", ok, err)
	}
	if rows, _ := openIncidents(s).ListIncidents(nil); len(rows) != 1 {
		t.Fatalf("incidents = %+v", rows)
	}
	if len(delivered) != 1 {
		t.Fatalf("delivered = %q", delivered)
	}
	if got, err := ledger.Get(ctx, repeatID); err != nil || got["delivery_outcome"] != "suppressed_acked" {
		t.Fatalf("repeat = %v, %v", got, err)
	}
}

func TestDispatchFailureNoticeResolvesTheOwningProfilesHomeChannel(t *testing.T) {
	for _, stage := range []string{"handoff", "claim_dispatch"} {
		t.Run(stage, func(t *testing.T) {
			base := t.TempDir()
			t.Setenv("ICEBATH_HOME", base)
			h := filepath.Join(base, "profiles", "worker")
			if err := os.MkdirAll(h, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(h, ".env"), []byte("TELEGRAM_HOME_CHANNEL=\"111111111\"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("TELEGRAM_HOME_CHANNEL", "")
			os.Unsetenv("TELEGRAM_HOME_CHANNEL")
			registerHomePlatforms(t, "telegram")
			s := executionLedgerStore(t, h)
			ctx := context.Background()
			saveSchedulerDoors(t)
			if stage == "handoff" {
				launchExternalCronWorkerSeam = handoffBoom
			} else {
				launchExternalCronWorkerSeam = func(*Store, context.Context, *Job) (bool, error) { return false, nil }
				claimDispatchSeam = func(*Store, context.Context, string) (bool, error) {
					return false, errors.New("worker exited before ownership acknowledgement")
				}
			}
			markJobRunSeam = func(*Store, context.Context, string, bool, MarkOptions) (bool, error) { return true, nil }
			var resolved []string
			prevDeliver := deliverResultSeam
			t.Cleanup(func() { deliverResultSeam = prevDeliver })
			deliverResultSeam = func(_ *Store, dctx context.Context, _ *Job, _ string, _, _ any, _ bool) (*string, error) {
				resolved = append(resolved, envHomeTargetChatID(dctx, "telegram"))
				return nil, nil
			}
			record, err := openExecutionsLedger(s).Create(ctx, "job-scoped", "builtin", nil)
			if err != nil {
				t.Fatal(err)
			}
			id := record["id"].(string)
			provider.SetMultiplexActive(true)
			t.Cleanup(func() { provider.SetMultiplexActive(false) })
			if provider.CurrentSecretScope(ctx) != nil {
				t.Fatal("a secret scope is installed before the fire")
			}
			ok, err := runOneJob(ctx, s, &Job{ID: "job-scoped", ExecutionID: &id, Deliver: strPtr("telegram")}, runOneJobOptions{})
			if err != nil || ok != (stage == "handoff") {
				t.Fatalf("run = %v, %v", ok, err)
			}
			if provider.CurrentSecretScope(ctx) != nil {
				t.Fatal("the fire's secret scope leaked")
			}
			if len(resolved) != 1 || resolved[0] != "111111111" {
				t.Fatalf("resolved = %q", resolved)
			}
			if rows, _ := openIncidents(s).ListIncidents(nil); len(rows) != 1 {
				t.Fatalf("incidents = %+v", rows)
			}
			if finished, err := openExecutionsLedger(s).Get(ctx, id); err != nil || finished["delivery_outcome"] != "delivered" {
				t.Fatalf("finished = %v, %v", finished, err)
			}
		})
	}
}

func TestPostHandoffWaiterFailureRecordsBookkeepingWithoutAlert(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	s := executionLedgerStore(t, filepath.Join(t.TempDir(), "home"))
	ctx := context.Background()
	saveSchedulerDoors(t)
	waitForExternalCronWorkerBodySeam = func(*Store, context.Context, workerProcess, string, *string) (bool, error) {
		return false, errors.New("cron external worker exited before durable recovery")
	}
	launchExternalCronWorkerSeam = func(ls *Store, lctx context.Context, job *Job) (bool, error) {
		return ls.waitForExternalCronWorker(lctx, fakeWorkerProcess{}, *job.ExecutionID, nil, nil, nil)
	}
	type mark struct {
		success bool
		errText string
	}
	var marks []mark
	markJobRunSeam = func(_ *Store, _ context.Context, _ string, success bool, o MarkOptions) (bool, error) {
		marks = append(marks, mark{success, deref(o.Error)})
		return true, nil
	}
	var delivered []string
	prevDeliver := deliverResultSeam
	t.Cleanup(func() { deliverResultSeam = prevDeliver })
	deliverResultSeam = func(_ *Store, _ context.Context, _ *Job, content string, _, _ any, _ bool) (*string, error) {
		delivered = append(delivered, content)
		return nil, nil
	}
	record, err := openExecutionsLedger(s).Create(ctx, "job-post", "builtin", nil)
	if err != nil {
		t.Fatal(err)
	}
	id := record["id"].(string)
	if ok, err := runOneJob(ctx, s, &Job{ID: "job-post", ExecutionID: &id, Deliver: strPtr("telegram:123")}, runOneJobOptions{}); err != nil || !ok {
		t.Fatalf("run = %v, %v", ok, err)
	}
	if rows, _ := openIncidents(s).ListIncidents(nil); len(rows) != 0 {
		t.Fatalf("incidents = %+v", rows)
	}
	if len(delivered) != 0 {
		t.Fatalf("delivered = %q", delivered)
	}
	if len(marks) != 1 || marks[0].success || !strings.HasPrefix(marks[0].errText, "Restart-safe cron worker failed after handoff: ") {
		t.Fatalf("marks = %+v", marks)
	}
	if got, err := openExecutionsLedger(s).Get(ctx, id); err != nil || got["status"] != "failed" {
		t.Fatalf("execution = %v, %v", got, err)
	}
}
