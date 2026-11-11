package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

const (
	driverMaxPromptBytes                = 128 * 1024
	driverMaxResultJSONBytes            = 256 * 1024
	DriverTerminalTaskRetentionSeconds  = 30 * 24 * 60 * 60
	DriverMaxRetainedTerminalTasks      = 2048
	driverMaxTaskPruneBatch             = 1000
	driverTaskOrder                     = "ORDER BY source_event_seq, created_at, task_id"
	driverSelectLease                   = "SELECT * FROM hosted_room_driver_leases WHERE room_id=?"
	driverSelectTask                    = "SELECT * FROM hosted_room_driver_tasks WHERE room_id=? AND task_id=?"
	driverTaskIndexSQL                  = "CREATE INDEX %sidx_hosted_room_driver_tasks_status\n           ON hosted_room_driver_tasks(room_id, status, source_event_seq, created_at, task_id)"
	driverGenerationFence               = "execution_generation=? AND cancel_generation=?"
	driverRunFence                      = "run_gateway_id=? AND run_process_generation=? AND run_lease_generation=?"
	driverSettleSet                     = "status=?, settlement_id=?, settlement_status=?, result_json=?, terminal_at=?, updated_at=?"
	driverRequeueSet                    = "status='queued', run_gateway_id=NULL, run_process_generation=NULL, run_lease_generation=NULL"
	driverCancelSet                     = "status='cancelled', cancel_generation=?, cancel_id=?, terminal_at=?, updated_at=?"
	driverIndeterminateStale            = "indeterminate task generation changed"
	driverTaskPayloadUnknownFieldsLabel = "unknown payload fields: "
)

var (
	DriverTaskStatuses     = map[string]bool{"queued": true, "running": true, "settled": true, "failed": true, "cancelled": true, "indeterminate": true, "deferred": true, "stopping": true}
	DriverTerminalStatuses = map[string]bool{"settled": true, "failed": true, "cancelled": true}
	driverPayloadRequired  = []string{"prompt", "source_event_seq", "target_profile"}
	driverPayloadOptional  = map[string]bool{"target_member_id": true, "input_context": true, "attachments": true}
	driverLeaseColumns     = []string{"room_id", "gateway_id", "authority_epoch", "process_generation", "lease_generation", "expires_at", "acquired_at", "updated_at", "released_at"}
	driverTaskColumnOrder  = []string{"room_id", "task_id", "thread_id", "turn_id", "source_event_seq", "payload_json", "payload_digest", "status",
		"execution_generation", "cancel_generation", "run_gateway_id", "run_process_generation", "run_lease_generation",
		"cancel_id", "settlement_id", "settlement_status", "result_json", "created_at", "updated_at", "started_at",
		"terminal_at", "indeterminate_at"}
	driverSettleRunningSQL  = driverGenerationUpdate(driverSettleSet, "running") + " AND " + driverRunFence
	driverSettleStoppingSQL = driverGenerationUpdate(driverSettleSet, "stopping")
	driverRequeueRunningSQL = driverTaskUpdate(driverRequeueSet+", started_at=NULL, updated_at=?", "status='running' AND "+driverGenerationFence+" AND "+driverRunFence)
	driverCancelQueuedSQL   = driverTaskUpdate(driverCancelSet, "status IN ('queued', 'deferred') AND cancel_generation=?")
	driverBeginStopSQL      = driverTaskUpdate("status='stopping', cancel_generation=?, cancel_id=?, updated_at=?", "status IN ('running', 'indeterminate') AND cancel_generation=?")
	driverCompleteStopSQL   = driverTaskUpdate("status='cancelled', terminal_at=?, updated_at=?", "status='stopping' AND cancel_id=? AND cancel_generation=?")
	driverGenerationTrans   = map[string]struct{ status, set, generationStale, stale string }{
		"resolve":          {"indeterminate", driverSettleSet, driverIndeterminateStale, "indeterminate task changed during reconciliation"},
		"resolve_cancel":   {"indeterminate", driverCancelSet, "indeterminate cancellation proof is stale", "indeterminate cancellation proof lost its fence"},
		"requeue":          {"indeterminate", driverRequeueSet + ", started_at=NULL, indeterminate_at=NULL, updated_at=?", driverIndeterminateStale, "indeterminate task changed during requeue"},
		"defer":            {"indeterminate", "status='deferred', result_json=?, terminal_at=?, updated_at=?", driverIndeterminateStale, "indeterminate task changed during deferral"},
		"requeue_deferred": {"deferred", driverRequeueSet + ", result_json=NULL, started_at=NULL, terminal_at=NULL, indeterminate_at=NULL, updated_at=?", "deferred task generation changed", "deferred task changed during requeue"},
	}
)

// Fenced task UPDATE of one state machine transition
func driverTaskUpdate(set, fence string) string {
	return "UPDATE hosted_room_driver_tasks SET " + set + " WHERE room_id=? AND task_id=? AND " + fence
}

// Transition fenced on status and both generations
func driverGenerationUpdate(set, status string) string {
	return driverTaskUpdate(set, "status='"+status+"' AND "+driverGenerationFence)
}

// Invalid or conflicting driver state operation, a ValueError
type DriverStateError struct{ Msg string }

func (e *DriverStateError) Error() string { return e.Msg }

// Python class name of the error
func (e *DriverStateError) PyTypeName() string { return "DriverStateError" }

// Hosted room that does not exist or was disbanded
type RoomUnavailableError struct{ *DriverStateError }

func (e *RoomUnavailableError) Unwrap() error      { return e.DriverStateError }
func (e *RoomUnavailableError) PyTypeName() string { return "RoomUnavailableError" }

// Room lease another unexpired driver generation owns
type LeaseHeldError struct{ *DriverStateError }

func (e *LeaseHeldError) Unwrap() error      { return e.DriverStateError }
func (e *LeaseHeldError) PyTypeName() string { return "LeaseHeldError" }

// Lease generation that can no longer mutate room state
type StaleLeaseError struct{ *DriverStateError }

func (e *StaleLeaseError) Unwrap() error      { return e.DriverStateError }
func (e *StaleLeaseError) PyTypeName() string { return "StaleLeaseError" }

// Idempotency key reused for different task state
type TaskConflictError struct{ *DriverStateError }

func (e *TaskConflictError) Unwrap() error      { return e.DriverStateError }
func (e *TaskConflictError) PyTypeName() string { return "TaskConflictError" }

// Obsolete task attempt or cancellation trying to commit
type StaleTaskError struct{ *DriverStateError }

func (e *StaleTaskError) Unwrap() error      { return e.DriverStateError }
func (e *StaleTaskError) PyTypeName() string { return "StaleTaskError" }

// Requested task transition that is not allowed
type InvalidTaskTransitionError struct{ *DriverStateError }

func (e *InvalidTaskTransitionError) Unwrap() error      { return e.DriverStateError }
func (e *InvalidTaskTransitionError) PyTypeName() string { return "InvalidTaskTransitionError" }

// Python class name of the error
func (e *DriverValidationError) PyTypeName() string { return "DriverValidationError" }

func roomUnavailable(msg string) error { return &RoomUnavailableError{&DriverStateError{msg}} }
func leaseHeld(msg string) error       { return &LeaseHeldError{&DriverStateError{msg}} }
func staleLease(msg string) error      { return &StaleLeaseError{&DriverStateError{msg}} }
func taskConflict(msg string) error    { return &TaskConflictError{&DriverStateError{msg}} }
func staleTask(msg string) error       { return &StaleTaskError{&DriverStateError{msg}} }
func invalidTransition(msg string) error {
	return &InvalidTaskTransitionError{&DriverStateError{msg}}
}
func driverValidation(msg string) error { return &DriverValidationError{msg} }

// Whether err is any driver state error, validation included
func isDriverStateError(err error) bool {
	var base *DriverStateError
	var validation *DriverValidationError
	return errors.As(err, &base) || errors.As(err, &validation)
}

// Clock of the room driver, its value converted the way float converts it
type DriverClock func() any

// Stable identity of one admitted room turn
type TaskIdentity struct {
	RoomID   string `json:"room_id"`
	TaskID   string `json:"task_id"`
	ThreadID string `json:"thread_id"`
	TurnID   string `json:"turn_id"`
}

// Validated task identity
func NewTaskIdentity(roomID, taskID, threadID, turnID any) (TaskIdentity, error) {
	var out TaskIdentity
	for _, field := range []struct {
		value any
		label string
		dest  *string
	}{{roomID, "room_id", &out.RoomID}, {taskID, "task_id", &out.TaskID}, {threadID, "thread_id", &out.ThreadID}, {turnID, "turn_id", &out.TurnID}} {
		text, err := driverIdentifier(field.value, field.label)
		if err != nil {
			return TaskIdentity{}, err
		}
		*field.dest = text
	}
	return out, nil
}

// Field mapping of the identity, the dataclass asdict shape
func (t TaskIdentity) AsDict() map[string]any {
	return map[string]any{"room_id": t.RoomID, "task_id": t.TaskID, "thread_id": t.ThreadID, "turn_id": t.TurnID}
}

// Fenced lease held by one gateway process incarnation
type DriverLease struct {
	RoomID            string
	GatewayID         string
	AuthorityEpoch    int64
	ProcessGeneration string
	LeaseGeneration   int64
	ExpiresAt         float64
	Reclaimed         bool
}

// Exact running generation authorized to settle one task
type TaskAttempt struct {
	Identity            TaskIdentity
	Lease               DriverLease
	ExecutionGeneration int64
	CancelGeneration    int64
}

// Finite float of a computed value, optionally positive
func driverFinite(compute func() any, message string, positive bool) (float64, error) {
	value, err := pyint.Float(compute())
	if err != nil {
		return 0, driverValidation(message)
	}
	if math.IsNaN(value) || math.IsInf(value, 0) || (positive && value <= 0) {
		return 0, driverValidation(message)
	}
	return value, nil
}

// Finite current time of the clock
func driverTimestamp(clock DriverClock) (float64, error) {
	if clock == nil {
		return 0, driverValidation("clock must be callable")
	}
	return driverFinite(clock, "clock must return a finite number", false)
}

// Validated ttl then clock, the current time and a finite expiry
func driverLeaseWindow(ttlSeconds any, clock DriverClock) (float64, float64, error) {
	ttl, err := driverFinite(func() any { return ttlSeconds }, "ttl_seconds must be a finite positive number", true)
	if err != nil {
		return 0, 0, err
	}
	now, err := driverTimestamp(clock)
	if err != nil {
		return 0, 0, err
	}
	if math.IsInf(now+ttl, 0) {
		return 0, 0, driverValidation("lease expiry must be finite")
	}
	return now, now + ttl, nil
}

// Normalized task payload, its compact JSON and SHA-256 digest
func driverTaskPayload(value any) (map[string]any, string, string, error) {
	fields, ok := pyjson.Plain(value).(map[string]any)
	if !ok {
		return nil, "", "", driverValidation("payload must be an object")
	}
	var unknown, missing []string
	required := map[string]bool{}
	for _, key := range driverPayloadRequired {
		required[key] = true
		if _, has := fields[key]; !has {
			missing = append(missing, key)
		}
	}
	for key := range fields {
		if !required[key] && !driverPayloadOptional[key] {
			unknown = append(unknown, key)
		}
	}
	sort.Strings(unknown)
	if len(unknown) > 0 {
		return nil, "", "", driverValidation(driverTaskPayloadUnknownFieldsLabel + strings.Join(unknown, ", "))
	}
	if len(missing) > 0 {
		return nil, "", "", driverValidation("missing payload fields: " + strings.Join(missing, ", "))
	}
	targetProfile, err := driverIdentifier(fields["target_profile"], "target_profile")
	if err != nil {
		return nil, "", "", err
	}
	prompt, err := roomText(fields["prompt"], "prompt", driverMaxPromptBytes, false, driverValidation)
	if err != nil {
		return nil, "", "", err
	}
	seq, err := roomBoundedInt(fields["source_event_seq"], driverValidation, "source_event_seq must be a positive integer", 1, nil)
	if err != nil {
		return nil, "", "", err
	}
	normalized := map[string]any{"target_profile": targetProfile, "prompt": prompt, "source_event_seq": pyint.Value(seq)}
	if input, has := fields["input_context"]; has {
		validated, err := ValidateTaskInput(input)
		if err != nil {
			return nil, "", "", driverValidation(err.Error())
		}
		normalized["input_context"] = validated
	}
	if member, has := fields["target_member_id"]; has {
		if normalized["target_member_id"], err = driverIdentifier(member, "target_member_id"); err != nil {
			return nil, "", "", err
		}
	}
	if attachments, has := fields["attachments"]; has {
		manifest, err := validateBoundTaskManifest(attachments)
		if err != nil {
			return nil, "", "", err
		}
		items := make([]any, len(manifest))
		for i, item := range manifest {
			items[i] = item
		}
		normalized["attachments"] = items
	}
	encoded, err := roomCompactJSON(normalized, true)
	if err != nil {
		return nil, "", "", err
	}
	sum := sha256.Sum256([]byte(encoded))
	return normalized, encoded, hex.EncodeToString(sum[:]), nil
}

// Creates one task table under name
func driverCreateTaskTable(ctx context.Context, conn state.SQLiteConn, table string) error {
	_, err := conn.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS "+table+` (
            room_id TEXT NOT NULL, task_id TEXT NOT NULL, thread_id TEXT NOT NULL, turn_id TEXT NOT NULL,
            source_event_seq INTEGER NOT NULL CHECK (source_event_seq >= 1),
            payload_json TEXT NOT NULL, payload_digest TEXT NOT NULL,
            status TEXT NOT NULL CHECK (status IN (
                'queued', 'running', 'settled', 'failed', 'cancelled', 'indeterminate', 'deferred', 'stopping')),
            execution_generation INTEGER NOT NULL DEFAULT 0 CHECK (execution_generation >= 0),
            cancel_generation INTEGER NOT NULL DEFAULT 0 CHECK (cancel_generation >= 0),
            run_gateway_id TEXT, run_process_generation TEXT, run_lease_generation INTEGER, cancel_id TEXT,
            settlement_id TEXT, settlement_status TEXT, result_json TEXT, created_at REAL NOT NULL,
            updated_at REAL NOT NULL, started_at REAL, terminal_at REAL, indeterminate_at REAL,
            PRIMARY KEY (room_id, task_id), UNIQUE (room_id, thread_id, turn_id),
            FOREIGN KEY (room_id) REFERENCES hosted_rooms(room_id))`)
	return err
}

// Creates the lease and task tables with their index, validating the result
func driverInitializeSchema(ctx context.Context, conn state.SQLiteConn) error {
	if _, err := conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS hosted_room_driver_leases (
            room_id TEXT PRIMARY KEY, gateway_id TEXT NOT NULL,
            authority_epoch INTEGER NOT NULL CHECK (authority_epoch >= 1), process_generation TEXT NOT NULL,
            lease_generation INTEGER NOT NULL CHECK (lease_generation >= 1),
            expires_at REAL NOT NULL, acquired_at REAL NOT NULL, updated_at REAL NOT NULL, released_at REAL,
            FOREIGN KEY (room_id) REFERENCES hosted_rooms(room_id))`); err != nil {
		return err
	}
	if err := driverCreateTaskTable(ctx, conn, "hosted_room_driver_tasks"); err != nil {
		return err
	}
	if err := driverValidateSchema(ctx, conn); err != nil {
		return err
	}
	_, err := conn.ExecContext(ctx, fmt.Sprintf(driverTaskIndexSQL, "IF NOT EXISTS "))
	return err
}

// Whether a column set is exactly the expected names
func driverColumnsEqual(columns map[string]bool, expected []string) bool {
	if len(columns) != len(expected) {
		return false
	}
	for _, name := range expected {
		if !columns[name] {
			return false
		}
	}
	return true
}

// Refuses an unpublished driver schema draft or a table missing its hosted_rooms foreign key
func driverValidateSchema(ctx context.Context, conn state.SQLiteConn) error {
	leases, err := hostedTableColumns(ctx, conn, "hosted_room_driver_leases")
	if err != nil {
		return err
	}
	tasks, err := hostedTableColumns(ctx, conn, "hosted_room_driver_tasks")
	if err != nil {
		return err
	}
	if !driverColumnsEqual(leases, driverLeaseColumns) || !driverColumnsEqual(tasks, driverTaskColumnOrder) {
		return &DriverStateError{"unsupported unpublished hosted-room driver schema; recreate the driver tables before starting the driver"}
	}
	for _, table := range []string{"hosted_room_driver_leases", "hosted_room_driver_tasks"} {
		rows, err := conn.QueryContext(ctx, "PRAGMA foreign_key_list("+table+")")
		if err != nil {
			return err
		}
		scanned, err := hostedScanRows(rows)
		if err != nil {
			return err
		}
		found := false
		for _, row := range scanned {
			if len(row) > 4 && row[2] == "hosted_rooms" && row[3] == "room_id" && row[4] == "room_id" {
				found = true
			}
		}
		if !found {
			return &DriverStateError{table + " is missing its hosted_rooms foreign key"}
		}
	}
	return nil
}

// Whether both driver tables and the status index exist
func driverSchemaObjectsExist(ctx context.Context, conn state.SQLiteConn) (bool, error) {
	rows, err := state.QueryObjects(ctx, conn, `SELECT name FROM sqlite_master WHERE type='table'
           AND name IN ('hosted_room_driver_leases', 'hosted_room_driver_tasks')`)
	if err != nil {
		return false, err
	}
	names := map[string]bool{}
	for _, row := range rows {
		names[hrStr(row, "name")] = true
	}
	if len(names) != 2 {
		return false, nil
	}
	index, err := state.QueryRowObject(ctx, conn, "SELECT 1 FROM sqlite_master WHERE type='index' AND name='idx_hosted_room_driver_tasks_status'")
	return index != nil, err
}

// Expands the task status CHECK without losing durable work
func driverMigrateTaskStatusConstraint(ctx context.Context, conn state.SQLiteConn) error {
	columns := strings.Join(driverTaskColumnOrder, ", ")
	for _, step := range []func() error{
		func() error {
			_, err := conn.ExecContext(ctx, "DROP INDEX IF EXISTS idx_hosted_room_driver_tasks_status")
			return err
		},
		func() error { return driverCreateTaskTable(ctx, conn, "hosted_room_driver_tasks_next") },
		func() error {
			_, err := conn.ExecContext(ctx, "INSERT INTO hosted_room_driver_tasks_next ("+columns+") SELECT "+columns+" FROM hosted_room_driver_tasks")
			return err
		},
		func() error {
			_, err := conn.ExecContext(ctx, "DROP TABLE hosted_room_driver_tasks")
			return err
		},
		func() error {
			_, err := conn.ExecContext(ctx, "ALTER TABLE hosted_room_driver_tasks_next RENAME TO hosted_room_driver_tasks")
			return err
		},
		func() error {
			_, err := conn.ExecContext(ctx, fmt.Sprintf(driverTaskIndexSQL, ""))
			return err
		},
	} {
		if err := step(); err != nil {
			return err
		}
	}
	return nil
}

// Opens the driver store, migrating the status constraint or creating the schema, then validating existing tables
func driverConnect(ctx context.Context, dbPath string) (state.SQLiteConn, error) {
	var existing []bool
	ready := func(ctx context.Context, conn state.SQLiteConn) (bool, error) {
		exists, err := driverSchemaObjectsExist(ctx, conn)
		if err != nil {
			return false, err
		}
		existing = append(existing, exists)
		if !exists {
			return false, nil
		}
		row, err := state.QueryRowObject(ctx, conn, "SELECT sql FROM sqlite_master WHERE type='table' AND name='hosted_room_driver_tasks'")
		if err != nil {
			return false, err
		}
		sqlText := ""
		if row != nil {
			sqlText = strings.ToLower(hrStr(row, "sql"))
		}
		return strings.Contains(sqlText, "'stopping'") && strings.Contains(sqlText, "'deferred'"), nil
	}
	initialize := func(ctx context.Context, conn state.SQLiteConn) error {
		if existing[0] {
			return driverMigrateTaskStatusConstraint(ctx, conn)
		}
		return driverInitializeSchema(ctx, conn)
	}
	conn, err := roomConnect(ctx, dbPath, "state.db (hosted_room_driver)", ready, initialize, 1)
	if err != nil {
		return nil, err
	}
	if existing[0] {
		if err := driverValidateSchema(ctx, conn); err != nil {
			conn.Close()
			return nil, err
		}
	}
	return conn, nil
}

// Runs body in one IMMEDIATE transaction on a fresh driver connection
func driverTransaction(ctx context.Context, dbPath string, body func(conn state.SQLiteConn) error) error {
	conn, err := driverConnect(ctx, dbPath)
	if err != nil {
		return err
	}
	return state.Transaction(ctx, conn, true, body)
}

// Lease of a stored lease row
func driverLeaseFromRow(row *pyjson.Object, reclaimed bool) DriverLease {
	return DriverLease{RoomID: hrStr(row, "room_id"), GatewayID: hrStr(row, "gateway_id"), AuthorityEpoch: hrInt(row, "authority_epoch"),
		ProcessGeneration: hrStr(row, "process_generation"), LeaseGeneration: hrInt(row, "lease_generation"),
		ExpiresAt: hrFloat(row, "expires_at"), Reclaimed: reclaimed}
}

// Identity of a stored task row
func driverIdentityFromRow(row *pyjson.Object) (TaskIdentity, error) {
	return NewTaskIdentity(row.Vals["room_id"], row.Vals["task_id"], row.Vals["thread_id"], row.Vals["turn_id"])
}

// Public task view of a stored row after its payload integrity check
func driverTaskFromRow(row *pyjson.Object, idempotent bool) (map[string]any, error) {
	decoded, err := roomLoads(hrStr(row, "payload_json"))
	if err != nil {
		return nil, taskConflict("stored task payload is invalid")
	}
	payload, encoded, digest, err := driverTaskPayload(decoded)
	if err != nil {
		var validation *DriverValidationError
		if errors.As(err, &validation) {
			return nil, taskConflict("stored task payload is invalid")
		}
		return nil, err
	}
	seq, _ := hostedStrictInt(payload["source_event_seq"])
	if encoded != hrStr(row, "payload_json") || digest != hrStr(row, "payload_digest") || seq != hrInt(row, "source_event_seq") {
		return nil, taskConflict("stored task payload failed its integrity check")
	}
	identity, err := driverIdentityFromRow(row)
	if err != nil {
		return nil, err
	}
	task := map[string]any{"identity": identity, "payload": payload}
	optionalInt := func(key string) any {
		if row.Vals[key] == nil {
			return nil
		}
		return hrInt(row, key)
	}
	optionalFloat := func(key string) any {
		if row.Vals[key] == nil {
			return nil
		}
		return hrFloat(row, key)
	}
	var result any
	if row.Vals["result_json"] != nil {
		if result, err = roomLoads(hrStr(row, "result_json")); err != nil {
			return nil, err
		}
	}
	for _, key := range []string{"payload_digest", "status", "run_gateway_id", "run_process_generation", "cancel_id", "settlement_id", "settlement_status"} {
		task[key] = row.Vals[key]
	}
	task["execution_generation"] = hrInt(row, "execution_generation")
	task["cancel_generation"] = hrInt(row, "cancel_generation")
	task["run_lease_generation"] = optionalInt("run_lease_generation")
	task["result"] = result
	task["created_at"] = hrFloat(row, "created_at")
	task["updated_at"] = hrFloat(row, "updated_at")
	task["started_at"] = optionalFloat("started_at")
	task["terminal_at"] = optionalFloat("terminal_at")
	task["indeterminate_at"] = optionalFloat("indeterminate_at")
	task["idempotent"] = idempotent
	return task, nil
}

// Stored row of the task, its identity checked against the turn
func driverLoadTask(ctx context.Context, conn state.SQLiteConn, identity TaskIdentity, required bool) (*pyjson.Object, error) {
	row, err := state.QueryRowObject(ctx, conn, driverSelectTask, identity.RoomID, identity.TaskID)
	if err != nil {
		return nil, err
	}
	if row == nil {
		if required {
			return nil, taskConflict("task does not exist")
		}
		return nil, nil
	}
	stored, err := driverIdentityFromRow(row)
	if err != nil {
		return nil, err
	}
	if stored != identity {
		return nil, taskConflict("task_id is already bound to a different turn")
	}
	return row, nil
}

// Room task rows in admission order, optionally of one status
func driverTasksInOrder(ctx context.Context, conn state.SQLiteConn, roomID string, status *string) ([]*pyjson.Object, error) {
	if status == nil {
		return state.QueryObjects(ctx, conn, "SELECT * FROM hosted_room_driver_tasks WHERE room_id=? "+driverTaskOrder, roomID)
	}
	return state.QueryObjects(ctx, conn, "SELECT * FROM hosted_room_driver_tasks WHERE room_id=? AND status=? "+driverTaskOrder, roomID, *status)
}

// Live room row, or RoomUnavailableError for a missing, disbanded or uncreated room store
func driverLoadActiveRoom(ctx context.Context, conn state.SQLiteConn, roomID string) (*pyjson.Object, error) {
	row, err := state.QueryRowObject(ctx, conn, "SELECT room_id, authority_gateway_id, authority_epoch, disbanded_at FROM hosted_rooms WHERE room_id=?", roomID)
	if err != nil {
		if class, ok := state.SQLiteClassOf(err); ok && class == state.SQLiteOperationalError && strings.Contains(strings.ToLower(err.Error()), "no such table") {
			return nil, roomUnavailable("hosted room does not exist")
		}
		return nil, err
	}
	if row == nil {
		return nil, roomUnavailable("hosted room does not exist")
	}
	if row.Vals["disbanded_at"] != nil {
		return nil, roomUnavailable("hosted room is disbanded")
	}
	return row, nil
}

// Live room row whose authority is still gatewayID at epoch
func driverRequireRoomAuthority(ctx context.Context, conn state.SQLiteConn, roomID, gatewayID string, epoch int64) (*pyjson.Object, error) {
	room, err := driverLoadActiveRoom(ctx, conn, roomID)
	if err != nil {
		return nil, err
	}
	if hrStr(room, "authority_gateway_id") != gatewayID || hrInt(room, "authority_epoch") != epoch {
		return nil, staleLease("hosted room authority changed")
	}
	return room, nil
}

// Run fence bind values of a lease
func driverRunFenceArgs(lease DriverLease) []any {
	return []any{lease.GatewayID, lease.ProcessGeneration, lease.LeaseGeneration}
}

// Whether a stored lease row is exactly the lease generation
func driverLeaseRowMatches(row *pyjson.Object, lease DriverLease) bool {
	return row != nil && hrStr(row, "gateway_id") == lease.GatewayID && hrInt(row, "authority_epoch") == lease.AuthorityEpoch &&
		hrStr(row, "process_generation") == lease.ProcessGeneration && hrInt(row, "lease_generation") == lease.LeaseGeneration
}

// Stored lease row while the lease is current, unreleased and unexpired
func driverRequireActiveLease(ctx context.Context, conn state.SQLiteConn, lease DriverLease, now float64) (*pyjson.Object, error) {
	if _, err := driverRequireRoomAuthority(ctx, conn, lease.RoomID, lease.GatewayID, lease.AuthorityEpoch); err != nil {
		return nil, err
	}
	row, err := state.QueryRowObject(ctx, conn, driverSelectLease, lease.RoomID)
	if err != nil {
		return nil, err
	}
	if !driverLeaseRowMatches(row, lease) || row.Vals["released_at"] != nil || hrFloat(row, "expires_at") <= now {
		return nil, staleLease("driver lease is stale or expired")
	}
	return row, nil
}

// Refuses a lease and task of different rooms
func driverCheckSameRoom(lease DriverLease, identity TaskIdentity) error {
	if lease.RoomID != identity.RoomID {
		return driverValidation("lease and task belong to different rooms")
	}
	return nil
}

// Non negative cancel generation, a bool counting as an int
func driverCancelGeneration(value int64) error {
	if value < 0 {
		return driverValidation("expected_cancel_generation must be non-negative")
	}
	return nil
}

// Validates the room match and both expected generations
func driverExpectedGenerations(lease DriverLease, identity TaskIdentity, executionGeneration, cancelGeneration int64) error {
	if err := driverCheckSameRoom(lease, identity); err != nil {
		return err
	}
	if executionGeneration < 1 {
		return driverValidation("expected_execution_generation must be a positive integer")
	}
	return driverCancelGeneration(cancelGeneration)
}

// Validated terminal settlement id of a settled or failed status
func driverTerminalSettlementID(settlementID any, status string) (string, error) {
	id, err := driverIdentifier(settlementID, "settlement_id")
	if err != nil {
		return "", err
	}
	if status != "settled" && status != "failed" {
		return "", driverValidation("status must be 'settled' or 'failed'")
	}
	return id, nil
}

// Replay of a stored row, nil to proceed with the transition
type driverReplay func(row *pyjson.Object) (map[string]any, error)

// Validated terminal settlement, its time, idempotent replay and SET params
func driverSettlement(settlementID any, status string, result any, clock DriverClock) (float64, driverReplay, []any, error) {
	id, err := driverTerminalSettlementID(settlementID, status)
	if err != nil {
		return 0, nil, nil, err
	}
	resultJSON, err := roomCanonicalJSON(pyjson.Plain(result), "result", driverMaxResultJSONBytes, true, driverValidation)
	if err != nil {
		return 0, nil, nil, err
	}
	now, err := driverTimestamp(clock)
	if err != nil {
		return 0, nil, nil, err
	}
	replay := func(row *pyjson.Object) (map[string]any, error) {
		if row.Vals["settlement_id"] == nil {
			return nil, nil
		}
		if hrStr(row, "settlement_id") == id && hrStr(row, "settlement_status") == status && row.Vals["result_json"] != nil && hrStr(row, "result_json") == resultJSON {
			return driverTaskFromRow(row, true)
		}
		return nil, taskConflict("task already has a different terminal settlement")
	}
	return now, replay, []any{status, id, status, resultJSON, now, now}, nil
}

// Replay of the same cancel id already committed in status
func driverCancelReplay(cancelID, status string) driverReplay {
	return func(row *pyjson.Object) (map[string]any, error) {
		if hrStr(row, "status") == status && row.Vals["cancel_id"] != nil && hrStr(row, "cancel_id") == cancelID {
			return driverTaskFromRow(row, true)
		}
		return nil, nil
	}
}

// Whether a row is in status at both generations
func driverGenerationsMatch(row *pyjson.Object, status string, executionGeneration, cancelGeneration int64) bool {
	return hrStr(row, "status") == status && hrInt(row, "execution_generation") == executionGeneration && hrInt(row, "cancel_generation") == cancelGeneration
}

// Refuses a row whose cancel generation moved
func driverRequireCancelGeneration(row *pyjson.Object, expected int64) error {
	if hrInt(row, "cancel_generation") != expected {
		return staleTask("task cancellation generation changed")
	}
	return nil
}

// One fenced task transition binding set, room, task and fence params
type driverTransitionSpec struct {
	sql         string
	setParams   []any
	fenceParams []any
	stale       string
	now         float64
	lease       *DriverLease
	leaseAfter  bool
	replay      driverReplay
	guard       func(row *pyjson.Object) error
}

// Loads, replays, guards and applies one fenced transition, capturing work evidence after
func driverTransition(ctx context.Context, dbPath string, identity TaskIdentity, spec driverTransitionSpec) (map[string]any, error) {
	params := append(append(append([]any{}, spec.setParams...), identity.RoomID, identity.TaskID), spec.fenceParams...)
	var out map[string]any
	err := driverTransaction(ctx, dbPath, func(conn state.SQLiteConn) error {
		if spec.lease != nil && !spec.leaseAfter {
			if _, err := driverRequireActiveLease(ctx, conn, *spec.lease, spec.now); err != nil {
				return err
			}
		}
		row, err := driverLoadTask(ctx, conn, identity, true)
		if err != nil {
			return err
		}
		if spec.replay != nil {
			replayed, err := spec.replay(row)
			if err != nil {
				return err
			}
			if replayed != nil {
				out = replayed
				return nil
			}
		}
		if spec.lease != nil && spec.leaseAfter {
			if _, err := driverRequireActiveLease(ctx, conn, *spec.lease, spec.now); err != nil {
				return err
			}
		}
		if spec.guard != nil {
			if err := spec.guard(row); err != nil {
				return err
			}
		}
		if err := roomFencedUpdate(ctx, conn, spec.sql, params, staleTask(spec.stale)); err != nil {
			return err
		}
		if err := captureTransitionLocked(ctx, conn, identity.RoomID); err != nil {
			return err
		}
		reloaded, err := driverLoadTask(ctx, conn, identity, true)
		if err != nil {
			return err
		}
		out, err = driverTaskFromRow(reloaded, false)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Lease first transition of the generation table fenced on status and both generations
func driverGenerationTransition(ctx context.Context, dbPath string, identity TaskIdentity, lease DriverLease, name string, executionGeneration, cancelGeneration int64,
	now float64, setParams []any, replay driverReplay) (map[string]any, error) {
	spec := driverGenerationTrans[name]
	return driverTransition(ctx, dbPath, identity, driverTransitionSpec{lease: &lease, now: now, replay: replay,
		guard: func(row *pyjson.Object) error {
			if !driverGenerationsMatch(row, spec.status, executionGeneration, cancelGeneration) {
				return staleTask(spec.generationStale)
			}
			return nil
		}, sql: driverGenerationUpdate(spec.set, spec.status), setParams: setParams, fenceParams: []any{executionGeneration, cancelGeneration}, stale: spec.stale})
}

// Transition fenced on the attempt's running generation under its exact lease
func driverRunFenceTransition(ctx context.Context, dbPath string, attempt TaskAttempt, guardStale string, nullAsZero bool, spec driverTransitionSpec) (map[string]any, error) {
	lease := attempt.Lease
	spec.lease = &lease
	spec.guard = func(row *pyjson.Object) error {
		if !driverGenerationsMatch(row, "running", attempt.ExecutionGeneration, attempt.CancelGeneration) {
			return staleTask(guardStale)
		}
		leaseGeneration := int64(0)
		if row.Vals["run_lease_generation"] != nil {
			leaseGeneration = hrInt(row, "run_lease_generation")
		} else if !nullAsZero {
			return &pyjson.TypeError{Msg: "int() argument must be a string, a bytes-like object or a real number, not 'NoneType'"}
		}
		if row.Vals["run_gateway_id"] != lease.GatewayID || row.Vals["run_process_generation"] != lease.ProcessGeneration || leaseGeneration != lease.LeaseGeneration {
			return staleTask(guardStale)
		}
		return nil
	}
	spec.fenceParams = append([]any{attempt.ExecutionGeneration, attempt.CancelGeneration}, driverRunFenceArgs(lease)...)
	return driverTransition(ctx, dbPath, attempt.Identity, spec)
}
