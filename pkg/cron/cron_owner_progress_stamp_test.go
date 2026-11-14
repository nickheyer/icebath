package cron

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

// Runner double holding a real session store and an empty launch adapter map, never draining
type progressStampRunner struct {
	store    *gateway.SessionStore
	adapters *pydict.Dict[gateway.Platform, gateway.PlatformAdapter]
}

func (progressStampRunner) SessionRuntimeDescriptor() map[string]any { return nil }
func (progressStampRunner) Draining() bool                           { return false }
func (progressStampRunner) Running() bool                            { return true }
func (r progressStampRunner) SessionStore() *gateway.SessionStore    { return r.store }
func (progressStampRunner) AdapterForSource(context.Context, *gateway.SessionSource) (gateway.PlatformAdapter, error) {
	return nil, nil
}
func (progressStampRunner) MultiplexProfiles() bool { return false }
func (r progressStampRunner) Adapters() *pydict.Dict[gateway.Platform, gateway.PlatformAdapter] {
	return r.adapters
}
func (progressStampRunner) ProfileAdapterMap(string) *pydict.Dict[gateway.Platform, gateway.PlatformAdapter] {
	return nil
}

func TestOwnerProgressReachesTheFirersLedgerRow(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := home.WithOverride(context.Background(), s.Home())
	seedJobs(t, s, map[string]any{"id": "job", "prompt": "p", "model": "m"})
	prevTouch := touchExecutionProgress
	touchExecutionProgress = func(context.Context, *Store, string) (bool, error) { return false, nil }
	t.Cleanup(func() { touchExecutionProgress = prevTouch })
	execution, err := openExecutionsLedger(s).Create(ctx, "job", "cron", nil)
	if err != nil {
		t.Fatal(err)
	}
	executionID := execution["id"].(string)
	if _, err := openExecutionsLedger(s).MarkRunning(ctx, executionID); err != nil {
		t.Fatal(err)
	}
	db, err := state.NewSessionDB(ctx, filepath.Join(s.Home(), "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	epoch, err := db.BeginRuntimeEpoch(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	cfg := gateway.DefaultGatewayConfig(ctx)
	runner := progressStampRunner{store: gateway.NewSessionStore(ctx, filepath.Join(s.Home(), "sessions"), cfg, nil),
		adapters: pydict.New[gateway.Platform, gateway.PlatformAdapter]()}
	authority := gateway.NewSessionAuthority(runner, s.Home(), "test", db, epoch)
	admitted := make(chan struct{}, 1)
	prevSchedule := gateway.SwapSessionSchedule(func(context.Context, *gateway.SessionAuthority, gateway.SessionRef) error {
		select {
		case admitted <- struct{}{}:
		default:
		}
		return nil
	})
	t.Cleanup(func() { gateway.SwapSessionSchedule(prevSchedule) })
	firerStamped := func() bool {
		var progressAt sql.NullString
		if err := openExecutionsLedger(s).transaction(ctx, func(conn state.SQLiteConn) error {
			return conn.QueryRowContext(ctx, "SELECT progress_at FROM executions WHERE id=?", executionID).Scan(&progressAt)
		}); err != nil {
			t.Error(err)
			return false
		}
		return progressAt.Valid
	}
	prevRun := ownerRunJob
	ownerRunJob = func(rs *Store, runCtx context.Context, job *Job, _ runJobOptions) (jobRunOutcome, error) {
		id := ""
		if job.ExecutionID != nil {
			id = *job.ExecutionID
		}
		stamper := newExecutionProgressStamper(rs, id, "job", func() float64 { return 0 }, 0.001)
		stamper.last -= 1
		stamper.tick(runCtx)
		for deadline := time.Now().Add(20 * time.Second); !firerStamped(); time.Sleep(5 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Error("timed out waiting for the firer's ledger row stamped with progress")
				break
			}
		}
		return jobRunOutcome{Success: true, Output: "doc", FinalResponse: "answer"}, nil
	}
	t.Cleanup(func() { ownerRunJob = prevRun })

	loop, stopLoop := context.WithCancel(ctx)
	defer stopLoop()
	gateway.BindCronOwner(loop, authority)
	defer gateway.UnbindCronOwner(authority)
	type firerResult struct {
		out jobRunOutcome
		err error
	}
	firer := make(chan firerResult, 1)
	go func() {
		out, err := s.runCanonicalJob(ctx, &Job{ID: "job"}, nil, nil, &executionID)
		firer <- firerResult{out, err}
	}()
	select {
	case <-admitted:
	case result := <-firer:
		t.Fatalf("firer ended before admission: %+v, %v", result.out, result.err)
	case <-time.After(10 * time.Second):
		t.Fatal("admission never scheduled")
	}
	sid := ""
	for id := range authority.Sessions {
		sid = id
	}
	row, _, err := db.ClaimSessionInput(ctx, state.ClaimInput{Epoch: epoch, SessionID: sid})
	if err != nil || row == nil {
		t.Fatalf("claim = %v, %v", row, err)
	}
	adapters, err := gateway.LocalAdapterMap(ctx, authority)
	if err != nil {
		t.Fatal(err)
	}
	local, _ := adapters.Get(gateway.PlatformLocal)
	policy := local.(*gateway.LocalSessionAdapter).Policies[sid]
	if _, err := gateway.CronExecute(ctx, authority, gateway.SessionRef{ProfileID: authority.ProfileID, SessionID: sid}, row, policy); err != nil {
		t.Fatal(err)
	}
	result := pyjson.ObjectOf("result", pyjson.ObjectOf("cron_result", []any{true, "doc", "answer", nil}), "usage", pyjson.NewObject())
	if _, err := db.SettleSessionInput(ctx, state.SettleInput{Epoch: epoch, AdmissionID: rowField(row, "admission_id"),
		Generation: *gateway.RowInt(row, "generation"), Outcome: "completed", Result: result}); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-firer:
		if got.err != nil || !got.out.Success || got.out.Output != "doc" || got.out.FinalResponse != "answer" || got.out.Error != nil {
			t.Fatalf("firer = %+v, %v", got.out, got.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("firer never observed the terminal result")
	}
	if !firerStamped() {
		t.Fatal("the firer's row never saw the owner's progress")
	}
}
