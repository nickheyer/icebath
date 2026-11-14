package gateway

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Settable clock of the driver cases
type hrFakeClock struct {
	mu    sync.Mutex
	value float64
}

func (c *hrFakeClock) now() any {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.value
}

func (c *hrFakeClock) advance(seconds float64) {
	c.mu.Lock()
	c.value += seconds
	c.mu.Unlock()
}

func hrClock() *hrFakeClock { return &hrFakeClock{value: 100} }

func hrIdentity(taskID, turnID string) TaskIdentity {
	return hrOK(NewTaskIdentity("room-1", taskID, "thread-1", turnID))
}

func hrPayload(prompt string, seq int, member string) map[string]any {
	if prompt == "" {
		prompt = "Inspect the release candidate."
	}
	payload := map[string]any{"target_profile": "ops", "prompt": prompt, "source_event_seq": seq}
	if member != "" {
		payload["target_member_id"] = member
	}
	return payload
}

// Room store with one live room the driver cases act on
func hrDriverDB(t *testing.T) string {
	t.Helper()
	db := hrDB(t)
	hrOK(CreateRoom(context.Background(), db, CreateRoomInput{RoomID: "room-1", Name: "Release room",
		Members: []any{map[string]any{"profile": "ops", "handle": "ops"}}, AuthorityGatewayID: "gateway-a", Now: hrFloatPtr(90)}))
	return db
}

func hrLease(db string, clock *hrFakeClock, gateway string, epoch int64, process string, ttl float64) (DriverLease, error) {
	if gateway == "" {
		gateway = "gateway-a"
	}
	if process == "" {
		process = "process-a"
	}
	return DriverAcquireLease(context.Background(), db, "room-1", gateway, epoch, process, ttl, clock.now)
}

func hrAdmit(db string, identity TaskIdentity, clock *hrFakeClock, payload map[string]any) (map[string]any, error) {
	if payload == nil {
		payload = hrPayload("", 1, "")
	}
	return DriverAdmitTask(context.Background(), db, identity, payload, clock.now)
}

func hrStart(db string, identity TaskIdentity, lease DriverLease, clock *hrFakeClock) (TaskAttempt, error) {
	return DriverStartTask(context.Background(), db, identity, lease, 0, clock.now)
}

func hrSettle(db string, attempt TaskAttempt, settlement, status, text string, clock *hrFakeClock) (map[string]any, error) {
	return DriverSettleTask(context.Background(), db, attempt, settlement, status, map[string]any{"text": text}, clock.now)
}

func TestTwoContendersHaveOneWinner(t *testing.T) {
	db := hrDriverDB(t)
	clock := hrClock()
	results := make([]*DriverLease, 2)
	var wg sync.WaitGroup
	for i, process := range []string{"process-a", "process-b"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lease, err := hrLease(db, clock, "", 1, process, 30)
			var held *LeaseHeldError
			if err != nil && !errors.As(err, &held) {
				t.Error(err)
			}
			if err == nil {
				results[i] = &lease
			}
		}()
	}
	wg.Wait()
	var winners []*DriverLease
	for _, result := range results {
		if result != nil {
			winners = append(winners, result)
		}
	}
	if len(winners) != 1 || winners[0].LeaseGeneration != 1 {
		t.Fatalf("winners %v", winners)
	}
}

func TestExpiryAllowsReclaimAndFencesStaleRenewAndRelease(t *testing.T) {
	db := hrDriverDB(t)
	clock := hrClock()
	first := hrOK(hrLease(db, clock, "", 1, "", 5))
	clock.advance(5)
	second := hrOK(hrLease(db, clock, "", 1, "process-b", 30))
	if !second.Reclaimed || second.LeaseGeneration != first.LeaseGeneration+1 {
		t.Fatalf("second %+v", second)
	}
	_, err := DriverRenewLease(context.Background(), db, first, 30, clock.now)
	hrRequireErr[StaleLeaseError](t, err, "")
	_, err = DriverReleaseLease(context.Background(), db, first, clock.now)
	hrRequireErr[StaleLeaseError](t, err, "")
}

func TestNonexistentAndDisbandedRoomsCannotLeaseOrAdmit(t *testing.T) {
	db := hrDriverDB(t)
	ctx := context.Background()
	clock := hrClock()
	missing := hrOK(NewTaskIdentity("missing-room", "task", "thread", "turn"))
	_, err := DriverAcquireLease(ctx, db, "missing-room", "gateway-a", 1, "process-a", 30, clock.now)
	hrRequireErr[RoomUnavailableError](t, err, "does not exist")
	_, err = hrAdmit(db, missing, clock, nil)
	hrRequireErr[RoomUnavailableError](t, err, "does not exist")
	hrOK(DisbandRoom(ctx, db, "room-1", "gateway-a", 1, hrFloatPtr(100)))
	_, err = hrLease(db, clock, "", 1, "", 30)
	hrRequireErr[RoomUnavailableError](t, err, "disbanded")
	_, err = hrAdmit(db, hrIdentity("task-1", "turn-1"), clock, nil)
	hrRequireErr[RoomUnavailableError](t, err, "disbanded")
}

func TestAcquireRequiresCurrentRoomAuthority(t *testing.T) {
	for _, tc := range []struct {
		gateway string
		epoch   int64
	}{{"gateway-b", 1}, {"gateway-a", 2}} {
		t.Run(fmt.Sprintf("%s-%d", tc.gateway, tc.epoch), func(t *testing.T) {
			db := hrDriverDB(t)
			_, err := hrLease(db, hrClock(), tc.gateway, tc.epoch, "", 30)
			hrRequireErr[StaleLeaseError](t, err, "authority changed")
		})
	}
}

func TestSameProcessAcquireAndReleaseAreIdempotent(t *testing.T) {
	db := hrDriverDB(t)
	clock := hrClock()
	first := hrOK(hrLease(db, clock, "", 1, "", 30))
	repeated := hrOK(hrLease(db, clock, "", 1, "", 30))
	if repeated.LeaseGeneration != first.LeaseGeneration {
		t.Fatalf("repeated %+v", repeated)
	}
	released := hrOK(DriverReleaseLease(context.Background(), db, repeated, clock.now))
	again := hrOK(DriverReleaseLease(context.Background(), db, repeated, clock.now))
	if released["idempotent"] != false || again["idempotent"] != true {
		t.Fatalf("released %v again %v", released, again)
	}
}

func TestRenewExtendsOnlyTheCurrentLeaseGeneration(t *testing.T) {
	db := hrDriverDB(t)
	clock := hrClock()
	lease := hrOK(hrLease(db, clock, "", 1, "", 5))
	clock.advance(2)
	renewed := hrOK(DriverRenewLease(context.Background(), db, lease, 20, clock.now))
	if renewed.LeaseGeneration != lease.LeaseGeneration || renewed.ExpiresAt != 122 {
		t.Fatalf("renewed %+v", renewed)
	}
}

func TestAuthorityTransferFencesLeaseAndLateSettlement(t *testing.T) {
	db := hrDriverDB(t)
	ctx := context.Background()
	clock := hrClock()
	identity, queued := hrIdentity("task-1", "turn-1"), hrIdentity("task-2", "turn-2")
	oldLease := hrOK(hrLease(db, clock, "", 1, "", 30))
	hrOK(hrAdmit(db, identity, clock, nil))
	hrOK(hrAdmit(db, queued, clock, nil))
	oldAttempt := hrOK(hrStart(db, identity, oldLease, clock))
	hrOK(ClaimAuthority(ctx, db, "room-1", "gateway-a", 1, "gateway-b", "claim-gateway-b", hrFloatPtr(100)))
	_, err := DriverRenewLease(ctx, db, oldLease, 30, clock.now)
	hrRequireErr[StaleLeaseError](t, err, "authority changed")
	_, err = hrStart(db, queued, oldLease, clock)
	hrRequireErr[StaleLeaseError](t, err, "authority changed")
	_, err = DriverRecoverRoom(ctx, db, oldLease, clock.now)
	hrRequireErr[StaleLeaseError](t, err, "authority changed")
	_, err = hrSettle(db, oldAttempt, "late-settlement", "settled", "late", clock)
	hrRequireErr[StaleLeaseError](t, err, "authority changed")
	newLease := hrOK(hrLease(db, clock, "gateway-b", 2, "process-b", 30))
	recovery := hrOK(DriverRecoverRoom(ctx, db, newLease, clock.now))
	if newLease.LeaseGeneration != oldLease.LeaseGeneration+1 || len(recovery["indeterminate"]) != 1 || recovery["indeterminate"][0] != identity ||
		len(recovery["queued"]) != 1 || recovery["queued"][0] != queued {
		t.Fatalf("lease %+v recovery %v", newLease, recovery)
	}
}

func TestRoomDisbandFencesActiveLeaseOperations(t *testing.T) {
	db := hrDriverDB(t)
	ctx := context.Background()
	clock := hrClock()
	identity := hrIdentity("task-1", "turn-1")
	lease := hrOK(hrLease(db, clock, "", 1, "", 30))
	hrOK(hrAdmit(db, identity, clock, nil))
	hrOK(DisbandRoom(ctx, db, "room-1", "gateway-a", 1, hrFloatPtr(100)))
	_, err := DriverRenewLease(ctx, db, lease, 30, clock.now)
	hrRequireErr[RoomUnavailableError](t, err, "disbanded")
	_, err = hrStart(db, identity, lease, clock)
	hrRequireErr[RoomUnavailableError](t, err, "disbanded")
	_, err = DriverRecoverRoom(ctx, db, lease, clock.now)
	hrRequireErr[RoomUnavailableError](t, err, "disbanded")
	_, err = DriverReleaseLease(ctx, db, lease, clock.now)
	hrRequireErr[RoomUnavailableError](t, err, "disbanded")
}

func TestTaskAdmissionIsIdempotentAndIdentityConflictsFail(t *testing.T) {
	db := hrDriverDB(t)
	clock := hrClock()
	identity := hrIdentity("task-1", "turn-1")
	first := hrOK(hrAdmit(db, identity, clock, nil))
	repeated := hrOK(hrAdmit(db, identity, clock, nil))
	if first["status"] != "queued" || repeated["idempotent"] != true {
		t.Fatalf("first %v repeated %v", first, repeated)
	}
	_, err := hrAdmit(db, hrOK(NewTaskIdentity("room-1", "task-1", "thread-other", "turn-other")), clock, nil)
	hrRequireErr[TaskConflictError](t, err, "")
	_, err = hrAdmit(db, identity, clock, hrPayload("A different immutable prompt.", 1, ""))
	hrRequireErr[TaskConflictError](t, err, "different payload")
	_, err = hrAdmit(db, hrOK(NewTaskIdentity("room-1", "task-other", "thread-1", "turn-1")), clock, nil)
	hrRequireErr[TaskConflictError](t, err, "")
}

func TestConcurrentTaskStartHasOneWinner(t *testing.T) {
	db := hrDriverDB(t)
	clock := hrClock()
	identity := hrIdentity("task-1", "turn-1")
	lease := hrOK(hrLease(db, clock, "", 1, "", 30))
	hrOK(hrAdmit(db, identity, clock, nil))
	results := make([]*TaskAttempt, 2)
	var wg sync.WaitGroup
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			attempt, err := hrStart(db, identity, lease, clock)
			var invalid *InvalidTaskTransitionError
			if err != nil && !errors.As(err, &invalid) {
				t.Error(err)
			}
			if err == nil {
				results[i] = &attempt
			}
		}()
	}
	wg.Wait()
	var winners []*TaskAttempt
	for _, result := range results {
		if result != nil {
			winners = append(winners, result)
		}
	}
	if len(winners) != 1 || winners[0].ExecutionGeneration != 1 {
		t.Fatalf("winners %v", winners)
	}
}

func TestStaleLeaseCannotStartOrCommitTask(t *testing.T) {
	db := hrDriverDB(t)
	ctx := context.Background()
	clock := hrClock()
	identity := hrIdentity("task-1", "turn-1")
	first := hrOK(hrLease(db, clock, "", 1, "", 5))
	hrOK(hrAdmit(db, identity, clock, nil))
	attempt := hrOK(hrStart(db, identity, first, clock))
	clock.advance(5)
	second := hrOK(hrLease(db, clock, "", 1, "process-b", 30))
	hrOK(DriverRecoverRoom(ctx, db, second, clock.now))
	_, err := hrSettle(db, attempt, "settlement-old", "settled", "late", clock)
	hrRequireErr[StaleLeaseError](t, err, "")
	if status := hrOK(DriverGetTask(ctx, db, identity))["status"]; status != "indeterminate" {
		t.Fatal(status)
	}
}

func TestTerminalSettlementIsIdempotent(t *testing.T) {
	for _, status := range []string{"settled", "failed"} {
		t.Run(status, func(t *testing.T) {
			db := hrDriverDB(t)
			clock := hrClock()
			identity := hrIdentity("task-1", "turn-1")
			lease := hrOK(hrLease(db, clock, "", 1, "", 30))
			hrOK(hrAdmit(db, identity, clock, nil))
			attempt := hrOK(hrStart(db, identity, lease, clock))
			first := hrOK(hrSettle(db, attempt, "settlement-1", status, "done", clock))
			repeated := hrOK(hrSettle(db, attempt, "settlement-1", status, "done", clock))
			if first["status"] != status || repeated["idempotent"] != true {
				t.Fatalf("first %v repeated %v", first, repeated)
			}
			_, err := hrSettle(db, attempt, "settlement-2", status, "changed", clock)
			hrRequireErr[TaskConflictError](t, err, "")
		})
	}
}

func TestCancellationFencesLateSuccess(t *testing.T) {
	db := hrDriverDB(t)
	ctx := context.Background()
	clock := hrClock()
	identity := hrIdentity("task-1", "turn-1")
	lease := hrOK(hrLease(db, clock, "", 1, "", 30))
	hrOK(hrAdmit(db, identity, clock, nil))
	attempt := hrOK(hrStart(db, identity, lease, clock))
	stopping := hrOK(DriverBeginTaskCancel(ctx, db, identity, "cancel-1", 0, clock.now))
	cancelled := hrOK(DriverCompleteTaskCancel(ctx, db, identity, "cancel-1", 1, clock.now))
	repeated := hrOK(DriverCompleteTaskCancel(ctx, db, identity, "cancel-1", 1, clock.now))
	if stopping["status"] != "stopping" || cancelled["status"] != "cancelled" || cancelled["cancel_generation"] != int64(1) || repeated["idempotent"] != true {
		t.Fatalf("stopping %v cancelled %v repeated %v", stopping, cancelled, repeated)
	}
	_, err := hrSettle(db, attempt, "late-success", "settled", "too late", clock)
	hrRequireErr[StaleTaskError](t, err, "")
}

func TestReleaseFailsClosedWhileItsTaskIsRunning(t *testing.T) {
	db := hrDriverDB(t)
	ctx := context.Background()
	clock := hrClock()
	identity := hrIdentity("task-1", "turn-1")
	lease := hrOK(hrLease(db, clock, "", 1, "", 30))
	hrOK(hrAdmit(db, identity, clock, nil))
	hrOK(hrStart(db, identity, lease, clock))
	_, err := DriverReleaseLease(ctx, db, lease, clock.now)
	hrRequireErr[InvalidTaskTransitionError](t, err, "tasks are running")
	if status := hrOK(DriverGetTask(ctx, db, identity))["status"]; status != "running" {
		t.Fatal(status)
	}
	hrOK(DriverBeginTaskCancel(ctx, db, identity, "cancel-before-release", 0, clock.now))
	hrOK(DriverCompleteTaskCancel(ctx, db, identity, "cancel-before-release", 1, clock.now))
	if released := hrOK(DriverReleaseLease(ctx, db, lease, clock.now)); released["idempotent"] != false {
		t.Fatalf("released %v", released)
	}
}

func hrStatuses(t *testing.T, db string) []any {
	t.Helper()
	var out []any
	for _, task := range hrOK(DriverListTasks(context.Background(), db, "room-1", nil)) {
		out = append(out, task["status"])
	}
	return out
}

func TestRestartRecoveryNeverRequeuesIndeterminateWork(t *testing.T) {
	db := hrDriverDB(t)
	ctx := context.Background()
	clock := hrClock()
	running, queued := hrIdentity("task-1", "turn-1"), hrIdentity("task-2", "turn-2")
	first := hrOK(hrLease(db, clock, "", 1, "", 5))
	hrOK(hrAdmit(db, running, clock, nil))
	hrOK(hrAdmit(db, queued, clock, nil))
	hrOK(hrStart(db, running, first, clock))
	_, err := hrLease(db, clock, "gateway-a", 1, "new-process", 30)
	hrRequireErr[LeaseHeldError](t, err, "")
	clock.advance(5)
	recovered := hrOK(hrLease(db, clock, "gateway-a", 1, "new-process", 30))
	recovery := hrOK(DriverRecoverRoom(ctx, db, recovered, clock.now))
	repeated := hrOK(DriverRecoverRoom(ctx, db, recovered, clock.now))
	want := map[string][]TaskIdentity{"queued": {queued}, "indeterminate": {running}}
	if fmt.Sprint(recovery) != fmt.Sprint(want) || fmt.Sprint(repeated) != fmt.Sprint(want) {
		t.Fatalf("recovery %v repeated %v", recovery, repeated)
	}
	_, err = hrStart(db, running, recovered, clock)
	hrRequireErr[InvalidTaskTransitionError](t, err, "")
	if statuses := hrStatuses(t, db); !pyjson.Equal(statuses, []any{"indeterminate", "queued"}) {
		t.Fatal(statuses)
	}
}

// Running first task and queued second task fenced by a recovered lease
func hrRecoveredRoom(t *testing.T, db string, clock *hrFakeClock, withQueued bool) (TaskIdentity, TaskIdentity, TaskAttempt, DriverLease) {
	t.Helper()
	running, queued := hrIdentity("task-1", "turn-1"), hrIdentity("task-2", "turn-2")
	first := hrOK(hrLease(db, clock, "", 1, "", 5))
	hrOK(hrAdmit(db, running, clock, hrPayload("", 1, "")))
	if withQueued {
		hrOK(hrAdmit(db, queued, clock, hrPayload("", 2, "")))
	}
	attempt := hrOK(hrStart(db, running, first, clock))
	clock.advance(5)
	recovered := hrOK(hrLease(db, clock, "", 1, "new-process", 30))
	return running, queued, attempt, recovered
}

func TestRecoveryIsRequiredBeforeStartingLaterWork(t *testing.T) {
	db := hrDriverDB(t)
	clock := hrClock()
	_, queued, _, recovered := hrRecoveredRoom(t, db, clock, true)
	_, err := hrStart(db, queued, recovered, clock)
	hrRequireErr[InvalidTaskTransitionError](t, err, "recovery must resolve")
}

func TestCurrentLeaseCanCommitVerifiedIndeterminateReceipt(t *testing.T) {
	db := hrDriverDB(t)
	ctx := context.Background()
	clock := hrClock()
	running, queued, attempt, recovered := hrRecoveredRoom(t, db, clock, true)
	hrOK(DriverRecoverRoom(ctx, db, recovered, clock.now))
	settled := hrOK(DriverResolveIndeterminateTask(ctx, db, running, recovered, attempt.ExecutionGeneration, attempt.CancelGeneration,
		"recovered-receipt", "settled", map[string]any{"text": "recovered"}, clock.now))
	next := hrOK(hrStart(db, queued, recovered, clock))
	if settled["status"] != "settled" || next.ExecutionGeneration != 1 {
		t.Fatalf("settled %v next %+v", settled, next)
	}
}

func TestCurrentLeaseCanCommitVerifiedIndeterminateCancellation(t *testing.T) {
	db := hrDriverDB(t)
	ctx := context.Background()
	clock := hrClock()
	running, _, attempt, recovered := hrRecoveredRoom(t, db, clock, false)
	hrOK(DriverRecoverRoom(ctx, db, recovered, clock.now))
	cancelled := hrOK(DriverResolveIndeterminateCancellation(ctx, db, running, recovered, attempt.ExecutionGeneration, attempt.CancelGeneration, "remote-cancel:1", clock.now))
	repeated := hrOK(DriverResolveIndeterminateCancellation(ctx, db, running, recovered, attempt.ExecutionGeneration, attempt.CancelGeneration, "remote-cancel:1", clock.now))
	if cancelled["status"] != "cancelled" || cancelled["execution_generation"] != attempt.ExecutionGeneration || repeated["idempotent"] != true {
		t.Fatalf("cancelled %v repeated %v", cancelled, repeated)
	}
}
