package cron

import (
	"context"
	"errors"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/state"
)

// Frozen policy the local adapter holds for a cron session
func localPolicyOf(t *testing.T, ctx context.Context, authority *gateway.SessionAuthority, sid string) *gateway.LocalSessionPolicy {
	t.Helper()
	adapters, err := gateway.LocalAdapterMap(ctx, authority)
	if err != nil {
		t.Fatal(err)
	}
	local, _ := adapters.Get(gateway.PlatformLocal)
	policy, ok := local.(*gateway.LocalSessionAdapter).Policies[sid]
	if !ok {
		t.Fatalf("no policy for %s", sid)
	}
	return policy
}

func TestCronAdmissionUsesJobModelAndExactRetry(t *testing.T) {
	s, _ := newTestStore(t)
	seedJobs(t, s, map[string]any{"id": "job", "prompt": "original", "model": "per-job-model"})
	authority, _, ctx := recoveryOwner(t, s)
	actor := &gateway.Principal{Subject: "owner", ProfileID: s.Home(),
		Capabilities: map[string]bool{"session:create": true, "session:submit": true, "session:control": true}, TransportID: "viewer"}
	conn := ownerTestConnection{authority: authority, actor: actor}
	params := map[string]any{"job_id": "job", "request_id": "fire", "extra_prompt": "extra"}
	receipt, err := gateway.CronRPC(ctx, conn, "submit", params)
	if err != nil {
		t.Fatal(err)
	}
	if again, err := gateway.CronRPC(ctx, conn, "submit", params); err != nil || !maps.Equal(again, receipt) {
		t.Fatalf("retry = %v, %v want %v", again, err, receipt)
	}
	if owned, err := gateway.CronOwnerOperation(ctx, authority, "submit", params); err != nil || !maps.Equal(owned, receipt) {
		t.Fatalf("owner retry = %v, %v want %v", owned, err, receipt)
	}
	changed := maps.Clone(params)
	changed["extra_prompt"] = "changed"
	var refusal *state.RuntimeStoreError
	if _, err := gateway.CronRPC(ctx, conn, "submit", changed); !errors.As(err, &refusal) {
		t.Fatalf("changed extra = %v", err)
	}
	policy := localPolicyOf(t, ctx, authority, receipt["session_id"].(string))
	if policy.Model == nil || *policy.Model != "per-job-model" || policy.Source != "cron" {
		t.Fatalf("policy model %v source %s", policy.Model, policy.Source)
	}
}

func TestDrainingOwnerStillAnswersStatusAndCancelOfAdmittedCronWork(t *testing.T) {
	s, _ := newTestStore(t)
	seedJobs(t, s, map[string]any{"id": "job", "prompt": "p", "model": "m"})
	authority, db, ctx, draining := drainableOwner(t, s)
	admitted := make(chan struct{}, 1)
	prevSchedule := gateway.SwapSessionSchedule(func(context.Context, *gateway.SessionAuthority, gateway.SessionRef) error {
		select {
		case admitted <- struct{}{}:
		default:
		}
		return nil
	})
	t.Cleanup(func() { gateway.SwapSessionSchedule(prevSchedule) })
	polledWhileDraining := make(chan struct{})
	var polledOnce bool
	realOperation := cronOwnerOperation
	t.Cleanup(func() { cronOwnerOperation = realOperation })
	cronOwnerOperation = func(opCtx context.Context, owner any, name string, params map[string]any) (map[string]any, error) {
		wasDraining := draining.Load()
		result, err := realOperation(opCtx, owner, name, params)
		if name == "status" && wasDraining && !polledOnce {
			polledOnce = true
			close(polledWhileDraining)
		}
		return result, err
	}
	release := bindRecoveryOwner(ctx, authority)
	defer release()
	type firerResult struct {
		out jobRunOutcome
		err error
	}
	firer := make(chan firerResult, 1)
	go func() {
		out, err := s.runCanonicalJob(ctx, &Job{ID: "job"}, nil, nil, strPtr("fire"))
		firer <- firerResult{out, err}
	}()
	select {
	case <-admitted:
	case result := <-firer:
		t.Fatalf("firer ended before admission: %+v, %v", result.out, result.err)
	case <-time.After(10 * time.Second):
		t.Fatal("admission never scheduled")
	}
	draining.Store(true)
	sid := ""
	for id := range authority.Sessions {
		sid = id
	}
	row := ownerClaim(t, ctx, db, authority, sid)
	admissionID, _ := row.Get("admission_id")
	cancelled, err := gateway.CronOwnerOperation(ctx, authority, "cancel", map[string]any{"session_id": sid, "admission_id": admissionID})
	if err != nil || len(cancelled) != 1 || cancelled["ok"] != true {
		t.Fatalf("cancel = %v, %v", cancelled, err)
	}
	select {
	case <-polledWhileDraining:
	case <-time.After(10 * time.Second):
		t.Fatal("firer never polled status during the drain")
	}
	generation, _ := row.Get("generation")
	result := pyjsonCronResult(true, "doc", "answer")
	if _, err := db.SettleSessionInput(ctx, state.SettleInput{Epoch: authority.Epoch, AdmissionID: admissionID.(string),
		Generation: generation.(int64), Outcome: "completed", Result: result}); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-firer:
		if got.err != nil || !got.out.Success || got.out.Output != "doc" || got.out.FinalResponse != "answer" || got.out.Error != nil {
			t.Fatalf("firer = %+v, %v", got.out, got.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("firer never settled")
	}
	_, err = gateway.CronOwnerOperation(ctx, authority, "submit", map[string]any{"job_id": "job", "request_id": "next", "extra_prompt": nil})
	var refusal *state.RuntimeStoreError
	if !errors.As(err, &refusal) || !strings.Contains(err.Error(), "runtime_draining") {
		t.Fatalf("submit while draining = %v", err)
	}
}
