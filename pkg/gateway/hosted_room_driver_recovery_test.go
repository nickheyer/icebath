package gateway

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

const hrDriverSchemaChildEnv = "ICEBATH_TEST_HOSTED_DRIVER_SCHEMA"

func init() {
	if path := os.Getenv(hrDriverSchemaChildEnv); path != "" {
		tasks, err := DriverListTasks(context.Background(), path, "room-1", nil)
		if err != nil {
			fmt.Println("error", err)
			os.Exit(1)
		}
		fmt.Println(len(tasks))
		os.Exit(0)
	}
}

func TestIndeterminateRetryIsExplicitAndAdvancesExecutionGeneration(t *testing.T) {
	db := hrDriverDB(t)
	ctx := context.Background()
	clock := hrClock()
	identity, _, original, recovered := hrRecoveredRoom(t, db, clock, false)
	hrOK(DriverRecoverRoom(ctx, db, recovered, clock.now))
	requeued := hrOK(DriverRequeueIndeterminateTask(ctx, db, identity, recovered, original.ExecutionGeneration, original.CancelGeneration, clock.now))
	retried := hrOK(hrStart(db, identity, recovered, clock))
	if requeued["status"] != "queued" || retried.ExecutionGeneration != original.ExecutionGeneration+1 {
		t.Fatalf("requeued %v retried %+v", requeued, retried)
	}
}

func TestIndeterminateTaskCanBeDeferredRetriedAndCancelled(t *testing.T) {
	db := hrDriverDB(t)
	ctx := context.Background()
	clock := hrClock()
	identity := hrIdentity("task-1", "turn-1")
	first := hrOK(hrLease(db, clock, "", 1, "", 5))
	hrOK(hrAdmit(db, identity, clock, nil))
	original := hrOK(hrStart(db, identity, first, clock))
	clock.advance(5)
	recovered := hrOK(hrLease(db, clock, "", 1, "new-process", 5))
	hrOK(DriverRecoverRoom(ctx, db, recovered, clock.now))
	deferred := hrOK(DriverDeferIndeterminateTask(ctx, db, identity, recovered, original.ExecutionGeneration, original.CancelGeneration, "member_unavailable", clock.now))
	repeated := hrOK(DriverDeferIndeterminateTask(ctx, db, identity, recovered, original.ExecutionGeneration, original.CancelGeneration, "member_unavailable", clock.now))
	requeued := hrOK(DriverRequeueDeferredTask(ctx, db, identity, recovered, original.ExecutionGeneration, original.CancelGeneration, clock.now))
	retried := hrOK(hrStart(db, identity, recovered, clock))
	if deferred["status"] != "deferred" || !pyjson.Equal(deferred["result"], map[string]any{"reason": "member_unavailable", "retryable": true}) ||
		repeated["idempotent"] != true || requeued["status"] != "queued" || retried.ExecutionGeneration != original.ExecutionGeneration+1 {
		t.Fatalf("deferred %v repeated %v requeued %v retried %+v", deferred, repeated, requeued, retried)
	}
	clock.advance(5)
	next := hrOK(hrLease(db, clock, "", 1, "third-process", 30))
	hrOK(DriverRecoverRoom(ctx, db, next, clock.now))
	hrOK(DriverDeferIndeterminateTask(ctx, db, identity, next, retried.ExecutionGeneration, retried.CancelGeneration, "member_unavailable", clock.now))
	if cancelled := hrOK(DriverCancelTask(ctx, db, identity, "cancel-deferred", 0, clock.now)); cancelled["status"] != "cancelled" {
		t.Fatalf("cancelled %v", cancelled)
	}
}

func TestProvenNotAdmittedAttemptReturnsToQueueUnderExactFence(t *testing.T) {
	db := hrDriverDB(t)
	ctx := context.Background()
	clock := hrClock()
	identity := hrIdentity("task-1", "turn-1")
	lease := hrOK(hrLease(db, clock, "", 1, "", 30))
	admitted := hrOK(hrAdmit(db, identity, clock, nil))
	attempt := hrOK(DriverStartTask(ctx, db, identity, lease, admitted["cancel_generation"].(int64), clock.now))
	queued := hrOK(DriverRequeueNotAdmittedTask(ctx, db, attempt, clock.now))
	repeated := hrOK(DriverRequeueNotAdmittedTask(ctx, db, attempt, clock.now))
	if queued["status"] != "queued" || queued["execution_generation"] != attempt.ExecutionGeneration || !pyjson.Equal(queued["payload"], admitted["payload"]) ||
		queued["run_gateway_id"] != nil || queued["run_process_generation"] != nil || queued["run_lease_generation"] != nil || repeated["idempotent"] != true {
		t.Fatalf("queued %v repeated %v", queued, repeated)
	}
}

func TestNotAdmittedRequeueRejectsStaleLeaseAndTaskGeneration(t *testing.T) {
	db := hrDriverDB(t)
	ctx := context.Background()
	clock := hrClock()
	identity := hrIdentity("task-1", "turn-1")
	lease := hrOK(hrLease(db, clock, "", 1, "", 5))
	hrOK(hrAdmit(db, identity, clock, nil))
	attempt := hrOK(hrStart(db, identity, lease, clock))
	stale := TaskAttempt{Identity: identity, Lease: lease, ExecutionGeneration: attempt.ExecutionGeneration + 1, CancelGeneration: attempt.CancelGeneration}
	_, err := DriverRequeueNotAdmittedTask(ctx, db, stale, clock.now)
	hrRequireErr[StaleTaskError](t, err, "lost its fence")
	clock.advance(5)
	_, err = DriverRequeueNotAdmittedTask(ctx, db, attempt, clock.now)
	hrRequireErr[StaleLeaseError](t, err, "")
	if status := hrOK(DriverGetTask(ctx, db, identity))["status"]; status != "running" {
		t.Fatal(status)
	}
}

func TestStateSurvivesSqliteReopenAndConcurrentDuplicateAdmission(t *testing.T) {
	db := hrDriverDB(t)
	ctx := context.Background()
	clock := hrClock()
	identity := hrIdentity("task-1", "turn-1")
	fresh := make([]bool, 8)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := hrAdmit(db, identity, clock, nil)
			if err != nil {
				t.Error(err)
				return
			}
			fresh[i] = result["idempotent"] == false
		}()
	}
	wg.Wait()
	count := 0
	for _, isFresh := range fresh {
		if isFresh {
			count++
		}
	}
	if count != 1 || hrInt(hrRawRow(t, db, "SELECT COUNT(*) AS n FROM hosted_room_driver_tasks"), "n") != 1 {
		t.Fatalf("fresh admissions %d", count)
	}
	reopened := hrOK(DriverGetTask(ctx, db, identity))
	listed := hrOK(DriverListTasks(ctx, db, "room-1", nil))
	if reopened["identity"] != identity || !pyjson.Equal(reopened["payload"], hrPayload("", 1, "")) || !pyjson.Equal(listed[0]["payload"], hrPayload("", 1, "")) {
		t.Fatalf("reopened %v", reopened)
	}
}

func TestPruneRemovesOnlyOldPublishedTerminalTasks(t *testing.T) {
	db := hrDriverDB(t)
	ctx := context.Background()
	clock := hrClock()
	lease := hrOK(hrLease(db, clock, "", 1, "", 30))
	for _, identity := range []TaskIdentity{hrIdentity("task-published", "turn-published"), hrIdentity("task-unpublished", "turn-unpublished")} {
		hrOK(hrAdmit(db, identity, clock, nil))
		attempt := hrOK(hrStart(db, identity, lease, clock))
		hrOK(hrSettle(db, attempt, "result:"+identity.TaskID, "settled", "done", clock))
	}
	hrRawExec(t, db, `CREATE TABLE hosted_room_policy_publications (
                room_id TEXT NOT NULL,
                task_id TEXT NOT NULL,
                kind TEXT NOT NULL,
                execution_generation INTEGER NOT NULL DEFAULT 0,
                seq INTEGER NOT NULL,
                PRIMARY KEY(room_id, task_id, kind, execution_generation)
            )`, `INSERT INTO hosted_room_policy_publications
               VALUES ('room-1', 'task-published', 'turn.settled', 0, 3)`)
	clock.advance(DriverTerminalTaskRetentionSeconds + 1)
	if pruned := hrOK(DriverPrunePublishedTerminalTasks(ctx, db, "room-1", clock.now, DriverTerminalTaskRetentionSeconds, DriverMaxRetainedTerminalTasks)); pruned != 1 {
		t.Fatalf("pruned %d", pruned)
	}
	listed := hrOK(DriverListTasks(ctx, db, "room-1", nil))
	if len(listed) != 1 || listed[0]["identity"].(TaskIdentity).TaskID != "task-unpublished" {
		t.Fatalf("listed %v", listed)
	}
}

func TestUnpublishedLegacyDriverSchemaFailsClosed(t *testing.T) {
	db := hrDriverDB(t)
	hrRawExec(t, db, "DROP TABLE IF EXISTS hosted_room_driver_tasks", "DROP TABLE IF EXISTS hosted_room_driver_leases", `CREATE TABLE hosted_room_driver_leases (
                room_id TEXT PRIMARY KEY,
                gateway_id TEXT NOT NULL,
                process_generation TEXT NOT NULL,
                lease_generation INTEGER NOT NULL,
                expires_at REAL NOT NULL,
                acquired_at REAL NOT NULL,
                updated_at REAL NOT NULL,
                released_at REAL
            )`, `CREATE TABLE hosted_room_driver_tasks (
                room_id TEXT NOT NULL,
                task_id TEXT NOT NULL,
                thread_id TEXT NOT NULL,
                turn_id TEXT NOT NULL,
                status TEXT NOT NULL,
                execution_generation INTEGER NOT NULL,
                cancel_generation INTEGER NOT NULL,
                run_gateway_id TEXT,
                run_process_generation TEXT,
                run_lease_generation INTEGER,
                cancel_id TEXT,
                settlement_id TEXT,
                settlement_status TEXT,
                result_json TEXT,
                created_at REAL NOT NULL,
                updated_at REAL NOT NULL,
                started_at REAL,
                terminal_at REAL,
                indeterminate_at REAL
            )`, `INSERT INTO hosted_room_driver_leases
               VALUES ('room-1', 'gateway-a', 'old-process', 1,
                       200, 100, 100, NULL)`, `INSERT INTO hosted_room_driver_tasks
               (room_id, task_id, thread_id, turn_id, status,
                execution_generation, cancel_generation,
                run_gateway_id, run_process_generation, run_lease_generation,
                created_at, updated_at, started_at)
               VALUES ('room-1', 'task-1', 'thread-1', 'turn-1', 'running',
                       1, 0, 'gateway-a', 'old-process', 1, 100, 100, 100)`)
	_, err := DriverGetTask(context.Background(), db, hrIdentity("task-1", "turn-1"))
	hrRequireErr[DriverStateError](t, err, "unsupported unpublished")
}

// Rebuilds the task table under its pre migration CHECK without one status
func hrDowngradeTaskStatus(t *testing.T, db, status string) {
	t.Helper()
	current := hrStr(hrRawRow(t, db, "SELECT sql FROM sqlite_master WHERE name='hosted_room_driver_tasks'"), "sql")
	columns := strings.Join(driverTaskColumnOrder, ", ")
	hrRawExec(t, db, "DROP INDEX idx_hosted_room_driver_tasks_status", "ALTER TABLE hosted_room_driver_tasks RENAME TO hosted_room_driver_tasks_current",
		strings.ReplaceAll(current, ", '"+status+"'", ""), "INSERT INTO hosted_room_driver_tasks ("+columns+") SELECT "+columns+" FROM hosted_room_driver_tasks_current",
		"DROP TABLE hosted_room_driver_tasks_current", `CREATE INDEX idx_hosted_room_driver_tasks_status
               ON hosted_room_driver_tasks(
                   room_id, status, source_event_seq, created_at, task_id
               )`)
}

func TestPreStoppingSchemaIsMigratedWithoutLosingTasks(t *testing.T) {
	db := hrDriverDB(t)
	ctx := context.Background()
	clock := hrClock()
	identity := hrIdentity("task-1", "turn-1")
	hrOK(hrAdmit(db, identity, clock, nil))
	hrDowngradeTaskStatus(t, db, "stopping")
	if status := hrOK(DriverGetTask(ctx, db, identity))["status"]; status != "queued" {
		t.Fatal(status)
	}
	lease := hrOK(hrLease(db, clock, "", 1, "", 30))
	attempt := hrOK(hrStart(db, identity, lease, clock))
	if stopping := hrOK(DriverBeginTaskCancel(ctx, db, identity, "cancel-after-upgrade", attempt.CancelGeneration, clock.now)); stopping["status"] != "stopping" {
		t.Fatal(stopping)
	}
	if sqlText := hrStr(hrRawRow(t, db, "SELECT sql FROM sqlite_master WHERE name='hosted_room_driver_tasks'"), "sql"); !strings.Contains(sqlText, "'stopping'") {
		t.Fatal(sqlText)
	}
}

func TestPreDeferredSchemaIsMigratedWithoutLosingTasks(t *testing.T) {
	db := hrDriverDB(t)
	identity := hrIdentity("task-1", "turn-1")
	hrOK(hrAdmit(db, identity, hrClock(), nil))
	hrDowngradeTaskStatus(t, db, "deferred")
	if status := hrOK(DriverGetTask(context.Background(), db, identity))["status"]; status != "queued" {
		t.Fatal(status)
	}
	if sqlText := hrStr(hrRawRow(t, db, "SELECT sql FROM sqlite_master WHERE name='hosted_room_driver_tasks'"), "sql"); !strings.Contains(sqlText, "'deferred'") {
		t.Fatal(sqlText)
	}
}

func TestFirstSchemaCreationIsSafeAcrossProcesses(t *testing.T) {
	db := hrDriverDB(t)
	hrRawExec(t, db, "DROP TABLE IF EXISTS hosted_room_driver_tasks", "DROP TABLE IF EXISTS hosted_room_driver_leases")
	commands := make([]*exec.Cmd, 4)
	outputs := make([]*bytes.Buffer, 4)
	for i := range commands {
		outputs[i] = &bytes.Buffer{}
		commands[i] = exec.Command(os.Args[0], "-test.run=^$")
		commands[i].Env = append(os.Environ(), hrDriverSchemaChildEnv+"="+db)
		commands[i].Stdout, commands[i].Stderr = outputs[i], outputs[i]
		if err := commands[i].Start(); err != nil {
			t.Fatal(err)
		}
	}
	for i, command := range commands {
		if err := command.Wait(); err != nil || !strings.HasSuffix(strings.TrimSpace(outputs[i].String()), "0") {
			t.Fatalf("child %d: %v %s", i, err, outputs[i])
		}
	}
}

func TestTasksFollowSourceEventOrderNotAdmissionTime(t *testing.T) {
	db := hrDriverDB(t)
	clock := hrClock()
	later, earlier := hrIdentity("task-2", "turn-2"), hrIdentity("task-1", "turn-1")
	hrOK(hrAdmit(db, later, clock, hrPayload("", 2, "")))
	hrOK(hrAdmit(db, earlier, clock, hrPayload("", 1, "")))
	lease := hrOK(hrLease(db, clock, "", 1, "", 30))
	listed := hrOK(DriverListTasks(context.Background(), db, "room-1", nil))
	if listed[0]["identity"] != earlier || listed[1]["identity"] != later {
		t.Fatalf("listed %v", listed)
	}
	_, err := hrStart(db, later, lease, clock)
	hrRequireErr[InvalidTaskTransitionError](t, err, "event order")
	if started := hrOK(hrStart(db, earlier, lease, clock)); started.Identity != earlier {
		t.Fatalf("started %+v", started)
	}
}

func TestPayloadDigestIsVerifiedOnRead(t *testing.T) {
	db := hrDriverDB(t)
	identity := hrIdentity("task-1", "turn-1")
	hrOK(hrAdmit(db, identity, hrClock(), nil))
	hrRawExec(t, db, `UPDATE hosted_room_driver_tasks
               SET payload_json=REPLACE(payload_json, 'Inspect', 'Replace')
               WHERE room_id='room-1' AND task_id='task-1'`)
	_, err := DriverGetTask(context.Background(), db, identity)
	hrRequireErr[TaskConflictError](t, err, "integrity")
}

func TestOptionalTargetMemberIdIsDurableAndDigestBound(t *testing.T) {
	db := hrDriverDB(t)
	identity := hrIdentity("task-1", "turn-1")
	hrOK(hrAdmit(db, identity, hrClock(), hrPayload("", 1, "member-remote")))
	if member := hrOK(DriverGetTask(context.Background(), db, identity))["payload"].(map[string]any)["target_member_id"]; member != "member-remote" {
		t.Fatal(member)
	}
}

func TestInvalidTaskPayloadIsRejected(t *testing.T) {
	with := func(key string, value any) map[string]any {
		payload := hrPayload("", 1, "")
		payload[key] = value
		return payload
	}
	cases := []struct {
		payload map[string]any
		match   string
	}{
		{map[string]any{"target_profile": "ops", "prompt": "hello"}, "missing payload fields"},
		{with("unexpected", true), "unknown payload fields"},
		{with("target_profile", "bad profile"), "invalid target_profile"},
		{with("prompt", " "), "prompt must not be empty"},
		{with("prompt", strings.Repeat("x", driverMaxPromptBytes+1)), "prompt is too large"},
		{with("source_event_seq", 0), "source_event_seq"},
		{with("source_event_seq", true), "source_event_seq"},
	}
	for _, tc := range cases {
		t.Run(tc.match, func(t *testing.T) {
			db := hrDriverDB(t)
			_, err := hrAdmit(db, hrIdentity("task-1", "turn-1"), hrClock(), tc.payload)
			hrRequireErr[DriverValidationError](t, err, tc.match)
		})
	}
}

func TestInvalidTaskIdentityIsRejected(t *testing.T) {
	_, err := NewTaskIdentity("bad room", "task", "thread", "turn")
	hrRequireErr[DriverValidationError](t, err, "invalid room_id")
	_, err = NewTaskIdentity("room", "", "thread", "turn")
	hrRequireErr[DriverValidationError](t, err, "invalid task_id")
}

func TestInvalidLeaseClockTtlAndSettlementSchemaAreRejected(t *testing.T) {
	db := hrDriverDB(t)
	ctx := context.Background()
	clock := hrClock()
	_, err := hrLease(db, clock, "", 1, "", 0)
	hrRequireErr[DriverValidationError](t, err, "ttl_seconds")
	_, err = DriverAcquireLease(ctx, db, "room-1", "gateway-a", 1, "process-a", 30, func() any { return math.NaN() })
	hrRequireErr[DriverValidationError](t, err, "clock")
	_, err = hrLease(db, &hrFakeClock{value: 1e308}, "", 1, "", 1e308)
	hrRequireErr[DriverValidationError](t, err, "expiry")
	identity := hrIdentity("task-1", "turn-1")
	lease := hrOK(hrLease(db, clock, "", 1, "", 30))
	hrOK(hrAdmit(db, identity, clock, nil))
	attempt := hrOK(hrStart(db, identity, lease, clock))
	_, err = DriverSettleTask(ctx, db, attempt, "settlement-1", "settled", map[string]any{"bad": make(chan int)}, clock.now)
	hrRequireErr[DriverValidationError](t, err, "JSON-serializable")
}

func TestRenewalNeverShortensAnActiveLease(t *testing.T) {
	db := hrDriverDB(t)
	clock := hrClock()
	lease := hrOK(hrLease(db, clock, "", 1, "", 30))
	clock.advance(1)
	if renewed := hrOK(DriverRenewLease(context.Background(), db, lease, 2, clock.now)); renewed.ExpiresAt != lease.ExpiresAt {
		t.Fatalf("renewed %+v lease %+v", renewed, lease)
	}
}
