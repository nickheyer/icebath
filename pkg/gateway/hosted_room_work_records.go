package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/state"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

const (
	WorkRecordPermission     = "work_records"
	workMaxTasks             = 128
	workMaxReceipts          = 256
	workMaxBytes             = 128 * 1024
	workMaxStoreRows         = 512
	WorkSourceTable          = "hosted_room_work_records_source"
	WorkTargetTable          = "hosted_room_work_records_target"
	WorkPendingTable         = "hosted_room_work_records_pending"
	workInvalidTable         = "hosted_room_work_records_invalid"
	workIncompletenessReason = "prior_authority_work_unknown"
)

// Evidence store byte bound, a package variable like the module global it ports
var workMaxStoreBytes int64 = 4 * 1024 * 1024

var (
	workRecordDisclosures = []any{"process_local_approvals_not_captured", "field_journals_not_captured", "external_effects_not_captured", "absent_record_is_not_non_admission", "not_execution_checkpoint"}
	workPhases            = map[string]bool{"queued": true, "running": true, "indeterminate": true, "stopping": true, "deferred": true, "settled": true, "failed": true, "cancelled": true}
	workTaskFields        = []string{"task_id", "thread_id", "turn_id", "source_event_seq", "payload_sha256", "member_id", "profile", "execution_generation", "cancel_generation", "phase", "settlement_id", "cancel_id"}
	workReceiptFields     = []string{"room_id", "home_install_id", "authority_gateway_id", "authority_epoch", "member_id", "target_install_id", "target_profile", "task_id", "execution_generation", "run_id", "session_id"}
	workRecordFields      = []string{"version", "room_id", "home_install_id", "authority", "roster_sha256", "history", "revision", "digest", "availability", "reason", "tasks", "receipts", "stop", "limitations"}
	workSHARE             = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// Controlled invalid or unavailable work evidence, a ValueError
type WorkRecordError struct{ Msg string }

func (e *WorkRecordError) Error() string { return e.Msg }

// Retained evidence bytes that cannot be reused
type InvalidStoredWorkRecord struct{ *WorkRecordError }

func (e *InvalidStoredWorkRecord) Unwrap() error { return e.WorkRecordError }

// A full bounded evidence store
type WorkRecordCapacityError struct{ *WorkRecordError }

func (e *WorkRecordCapacityError) Unwrap() error { return e.WorkRecordError }

// A matching canonical history prefix that is not retained
type WorkRecordPrefixError struct{ *WorkRecordError }

func (e *WorkRecordPrefixError) Unwrap() error { return e.WorkRecordError }

func workRecordError(msg string) error { return &WorkRecordError{msg} }
func workRecordPrefixError(msg string) error {
	return &WorkRecordPrefixError{&WorkRecordError{msg}}
}
func invalidStoredWorkRecord(msg string) error {
	return &InvalidStoredWorkRecord{&WorkRecordError{msg}}
}

// Sorted compact JSON that refuses NaN, the evidence canonical form
func WorkEncode(value any) (string, error) {
	return jsonutil.Encode(value, jsonutil.SortKeys())
}

// SHA-256 hex of the canonical encoding
func workDigest(value any) (string, error) {
	encoded, err := WorkEncode(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(encoded))
	return hex.EncodeToString(sum[:]), nil
}

// Refuses anything but an object with exactly the named fields
func workFields(value any, fields []string) (map[string]any, error) {
	object, ok := value.(map[string]any)
	if !ok || len(object) != len(fields) {
		return nil, workRecordError("work record fields are invalid")
	}
	for _, field := range fields {
		if _, has := object[field]; !has {
			return nil, workRecordError("work record fields are invalid")
		}
	}
	return object, nil
}

// Refuses anything but an int64 range integer at or above minimum
func workInteger(value any, minimum int64) error {
	n, ok := hostedStrictInt(value)
	if !ok || n < minimum {
		return workRecordError("work record integer is invalid")
	}
	return nil
}

// Refuses a non canonical record identity
func workID(value any) error {
	result, err := roomIdentifier(value, roomIdentifierSpec{label: "record identity", maxChars: 256}, workRecordError)
	if err != nil {
		return err
	}
	if text, _ := value.(string); text != result {
		return workRecordError("work record identity is not canonical")
	}
	return nil
}

// Refuses anything but a lowercase sha256 hex digest
func workSHA(value any) error {
	text, ok := value.(string)
	if !ok || !workSHARE.MatchString(text) {
		return workRecordError("work record digest is invalid")
	}
	return nil
}

// Validates each task entry against the record prefix
func workValidateTasks(record map[string]any, tasks []any) error {
	taskIDs := map[string]bool{}
	historySeq, _ := hostedStrictInt(record["history"].(map[string]any)["seq"])
	for _, item := range tasks {
		task, err := workFields(item, workTaskFields)
		if err != nil {
			return err
		}
		for _, name := range []string{"task_id", "thread_id", "turn_id", "member_id", "profile"} {
			if err := workID(task[name]); err != nil {
				return err
			}
		}
		for _, name := range []string{"settlement_id", "cancel_id"} {
			if task[name] != nil {
				if err := workID(task[name]); err != nil {
					return err
				}
			}
		}
		for _, name := range []string{"execution_generation", "cancel_generation"} {
			if err := workInteger(task[name], 0); err != nil {
				return err
			}
		}
		if err := workInteger(task["source_event_seq"], 1); err != nil {
			return err
		}
		if err := workSHA(task["payload_sha256"]); err != nil {
			return err
		}
		seq, _ := hostedStrictInt(task["source_event_seq"])
		phase, _ := task["phase"].(string)
		taskID := task["task_id"].(string)
		if seq > historySeq || !workPhases[phase] || taskIDs[taskID] {
			return workRecordError("work record task conflicts")
		}
		taskIDs[taskID] = true
	}
	return nil
}

// Validates each receipt entry and its lineage
func workValidateReceipts(record map[string]any, receipts []any, v2 bool) error {
	seen := map[string]bool{}
	var keyFields []string
	for _, field := range workReceiptFields {
		if field != "run_id" && field != "session_id" {
			keyFields = append(keyFields, field)
		}
	}
	sort.Strings(keyFields)
	recordEpoch := int64(1)
	for _, item := range receipts {
		receipt, err := workFields(item, workReceiptFields)
		if err != nil {
			return err
		}
		for _, name := range workReceiptFields {
			if name != "authority_epoch" && name != "execution_generation" {
				if err := workID(receipt[name]); err != nil {
					return err
				}
			}
		}
		if err := workInteger(receipt["execution_generation"], 1); err != nil {
			return err
		}
		if err := workInteger(receipt["authority_epoch"], 1); err != nil {
			return err
		}
		epoch, _ := hostedStrictInt(receipt["authority_epoch"])
		if receipt["room_id"] != record["room_id"] || receipt["authority_gateway_id"] != receipt["home_install_id"] ||
			(!v2 && (receipt["home_install_id"] != record["home_install_id"] || epoch != recordEpoch)) {
			return workRecordError("work record receipt lineage conflicts")
		}
		key := make([]any, len(keyFields))
		for i, field := range keyFields {
			key[i] = receipt[field]
		}
		encoded, err := WorkEncode(key)
		if err != nil {
			return err
		}
		if seen[encoded] {
			return workRecordError("work record receipt is duplicated")
		}
		seen[encoded] = true
	}
	return nil
}

// Validates the closing facts of a record
func workValidateStop(record map[string]any) error {
	stop, err := workFields(record["stop"], []string{"closing", "revocation_complete", "seq", "cancel_id"})
	if err != nil {
		return err
	}
	for _, key := range []string{"closing", "revocation_complete"} {
		if _, ok := stop[key].(bool); !ok {
			return workRecordError("work record closing fact is invalid")
		}
	}
	if err := workInteger(stop["seq"], 0); err != nil {
		return err
	}
	seq, _ := hostedStrictInt(stop["seq"])
	historySeq, _ := hostedStrictInt(record["history"].(map[string]any)["seq"])
	if seq > historySeq {
		return workRecordError("work record stop is beyond its prefix")
	}
	if stop["cancel_id"] != nil {
		return workID(stop["cancel_id"])
	}
	return nil
}

// Validates the availability claim and list bounds of a record
func workValidateAvailability(record map[string]any) ([]any, []any, error) {
	availability, _ := record["availability"].(string)
	reason := record["reason"]
	reasonText, reasonIsText := reason.(string)
	validReason := reason == nil || (reasonIsText && (reasonText == "task_store_missing" || reasonText == "unsupported_task" || reasonText == "bounds_exceeded"))
	if (availability != "available" && availability != "unavailable") || !validReason {
		return nil, nil, workRecordError("work record availability is invalid")
	}
	if (availability == "available") != (reason == nil) {
		return nil, nil, workRecordError("work record availability conflicts")
	}
	tasks, tasksOK := record["tasks"].([]any)
	receipts, receiptsOK := record["receipts"].([]any)
	if !tasksOK || len(tasks) > workMaxTasks || !receiptsOK || len(receipts) > workMaxReceipts {
		return nil, nil, workRecordError("work record list exceeds its bound")
	}
	if availability == "unavailable" && (len(tasks) > 0 || len(receipts) > 0) {
		return nil, nil, workRecordError("unavailable work record must not appear complete")
	}
	return tasks, receipts, nil
}

// Validates one passive work record completely, returning it unchanged
func ValidateWorkRecord(value any) (map[string]any, error) {
	record, isMap := value.(map[string]any)
	version, isInt := hostedStrictInt(record["version"])
	v2 := isMap && isInt && version == 2
	fields := workRecordFields
	if v2 {
		fields = append(append([]string{}, workRecordFields...), "lineage_sha256", "incompleteness")
	}
	if _, err := workFields(value, fields); err != nil {
		return nil, err
	}
	if !isInt || (version != 1 && version != 2) {
		return nil, workRecordError("work record version is unsupported")
	}
	if err := workID(record["room_id"]); err != nil {
		return nil, err
	}
	if err := workID(record["home_install_id"]); err != nil {
		return nil, err
	}
	authority, err := workFields(record["authority"], []string{"gateway_id", "epoch"})
	if err != nil {
		return nil, err
	}
	if err := workInteger(authority["epoch"], 1); err != nil {
		return nil, err
	}
	epoch, _ := hostedStrictInt(authority["epoch"])
	if authority["gateway_id"] != record["home_install_id"] || (!v2 && epoch != 1) {
		return nil, workRecordError("work record authority is unsupported")
	}
	if v2 {
		if err := workSHA(record["lineage_sha256"]); err != nil {
			return nil, err
		}
		if !anyutil.Equal(record["incompleteness"], []any{workIncompletenessReason}) {
			return nil, workRecordError("work record incompleteness is required")
		}
	}
	if err := workSHA(record["roster_sha256"]); err != nil {
		return nil, err
	}
	if err := workInteger(record["revision"], 1); err != nil {
		return nil, err
	}
	history, err := workFields(record["history"], []string{"seq", "event_sha256"})
	if err != nil {
		return nil, err
	}
	if err := workInteger(history["seq"], 0); err != nil {
		return nil, err
	}
	if err := workSHA(history["event_sha256"]); err != nil {
		return nil, err
	}
	if !anyutil.Equal(record["limitations"], workRecordDisclosures) {
		return nil, workRecordError("work record limitations are required")
	}
	tasks, receipts, err := workValidateAvailability(record)
	if err != nil {
		return nil, err
	}
	if err := workValidateTasks(record, tasks); err != nil {
		return nil, err
	}
	if err := workValidateReceipts(record, receipts, v2); err != nil {
		return nil, err
	}
	if err := workValidateStop(record); err != nil {
		return nil, err
	}
	if err := workSHA(record["digest"]); err != nil {
		return nil, err
	}
	content := map[string]any{}
	for key, item := range record {
		if key != "revision" && key != "digest" {
			content[key] = item
		}
	}
	expected, err := workDigest(content)
	if err != nil {
		return nil, err
	}
	if record["digest"] != expected {
		return nil, workRecordError("work record content digest conflicts")
	}
	encoded, err := WorkEncode(record)
	if err != nil {
		return nil, err
	}
	if len(encoded) > workMaxBytes {
		return nil, workRecordError("work record exceeds its byte bound")
	}
	return record, nil
}

// Refuses a source evidence write that would exceed the store byte or row bound
func workBudget(ctx context.Context, conn state.SQLiteConn, keys []string, values []any) error {
	totalSQL, countSQL := workUsageSQL([]string{WorkSourceTable, WorkPendingTable, workInvalidTable})
	var total, count int64
	if err := conn.QueryRowContext(ctx, "SELECT "+totalSQL+", "+countSQL).Scan(&total, &count); err != nil {
		return err
	}
	proposed := map[string]any{}
	for i, key := range keys {
		proposed[key] = values[i]
	}
	scopeKeys := []string{"room_id", "producer_gateway_id", "producer_epoch"}
	where := strings.Join(mapStrings(scopeKeys, func(k string) string { return k + "=?" }), " AND ")
	scopeArgs := make([]any, len(scopeKeys))
	for i, key := range scopeKeys {
		scopeArgs[i] = proposed[key]
	}
	old, err := state.QueryObjects(ctx, conn, "SELECT "+workRowSizeSQL(WorkSourceTable, "")+" AS size FROM "+WorkSourceTable+" WHERE "+where, scopeArgs...)
	if err != nil {
		return err
	}
	selects := mapStrings(keys, func(k string) string { return "? AS " + k })
	var size int64
	if err := conn.QueryRowContext(ctx, "SELECT "+workRowSizeSQL(WorkSourceTable, "")+" FROM (SELECT "+strings.Join(selects, ",")+")", values...).Scan(&size); err != nil {
		return err
	}
	var oldSize, oldCount int64
	if len(old) > 0 {
		oldSize, oldCount = hrInt(old[0], "size"), 1
	}
	if total-oldSize+size > workMaxStoreBytes || count-oldCount+1 > workMaxStoreRows {
		return &WorkRecordCapacityError{&WorkRecordError{"work record storage is full"}}
	}
	return nil
}

// Digest anchoring the history prefix through seq of one log table
func workHistoryAnchor(ctx context.Context, conn state.SQLiteConn, table, roomID string, seq int64) (string, error) {
	if seq == 0 {
		return workDigest([]any{})
	}
	row, err := state.QueryRowObject(ctx, conn, "SELECT seq,event_id,kind,actor_json,authority_epoch,payload_json,created_at\n        FROM "+table+" WHERE room_id=? AND seq=?", roomID, seq)
	if err != nil {
		return "", err
	}
	if row == nil {
		return "", workRecordPrefixError("work record history prefix is unavailable")
	}
	actor, err := roomLoads(hrStr(row, "actor_json"))
	if err != nil {
		return "", err
	}
	payload, err := roomLoads(hrStr(row, "payload_json"))
	if err != nil {
		return "", err
	}
	return workDigest([]any{row.Value("seq"), row.Value("event_id"), row.Value("kind"), actor, row.Value("authority_epoch"), payload, hrFloat(row, "created_at")})
}

// Outstanding task facts and remote receipts of a room, or the reason they are unavailable
func workCaptureTasks(ctx context.Context, conn state.SQLiteConn, roomID string) ([]any, []any, any, error) {
	exists, err := hostedTableExists(ctx, conn, "hosted_room_driver_tasks")
	if err != nil {
		return nil, nil, nil, err
	}
	if !exists {
		return []any{}, []any{}, "task_store_missing", nil
	}
	published := ""
	publications, err := hostedTableExists(ctx, conn, "hosted_room_policy_publications")
	if err != nil {
		return nil, nil, nil, err
	}
	if publications {
		published = `NOT EXISTS (SELECT 1 FROM hosted_room_policy_publications AS p
            WHERE p.room_id=t.room_id AND p.task_id=t.task_id AND p.kind IN ('turn.settled','turn.failed','turn.cancelled'))`
	}
	taskFilter, receiptFilter := "", ""
	if published != "" {
		taskFilter = " AND (status NOT IN ('settled','failed','cancelled') OR " + published + ")"
		receiptFilter = " AND " + published
	}
	rows, err := state.QueryObjects(ctx, conn, `SELECT task_id,thread_id,turn_id,source_event_seq,payload_json,payload_digest,
        status,execution_generation,cancel_generation,settlement_id,cancel_id
        FROM hosted_room_driver_tasks AS t WHERE room_id=?`+taskFilter+" ORDER BY task_id LIMIT ?", roomID, workMaxTasks+1)
	if err != nil {
		return nil, nil, nil, err
	}
	receiptColumns := append([]string{}, workReceiptFields...)
	sort.Strings(receiptColumns)
	receiptRows, err := state.QueryObjects(ctx, conn, "SELECT "+strings.Join(receiptColumns, ",")+" FROM hosted_room_remote_runs AS t WHERE room_id=?"+
		receiptFilter+" ORDER BY task_id,execution_generation,member_id LIMIT ?", roomID, workMaxReceipts+1)
	if err != nil {
		return nil, nil, nil, err
	}
	if len(rows) > workMaxTasks || len(receiptRows) > workMaxReceipts {
		return []any{}, []any{}, "bounds_exceeded", nil
	}
	receipts := make([]any, len(receiptRows))
	for i, row := range receiptRows {
		receipts[i] = hrDict(row)
	}
	tasks := make([]any, 0, len(rows))
	for _, row := range rows {
		decoded, err := roomLoads(hrStr(row, "payload_json"))
		if err != nil {
			return []any{}, []any{}, "unsupported_task", nil
		}
		payload, encoded, payloadDigest, err := driverTaskPayload(decoded)
		if err != nil {
			if isDriverStateError(err) {
				return []any{}, []any{}, "unsupported_task", nil
			}
			return nil, nil, nil, err
		}
		seq, _ := hostedStrictInt(payload["source_event_seq"])
		if payloadDigest != hrStr(row, "payload_digest") || encoded != hrStr(row, "payload_json") || seq != hrInt(row, "source_event_seq") {
			return []any{}, []any{}, "unsupported_task", nil
		}
		task := map[string]any{}
		for _, key := range []string{"task_id", "thread_id", "turn_id", "source_event_seq", "execution_generation", "cancel_generation", "settlement_id", "cancel_id"} {
			task[key] = row.Value(key)
		}
		task["payload_sha256"] = payloadDigest
		task["phase"] = row.Value("status")
		task["profile"] = payload["target_profile"]
		if member, has := payload["target_member_id"]; has {
			task["member_id"] = member
		} else {
			task["member_id"] = payload["target_profile"]
		}
		tasks = append(tasks, task)
	}
	return tasks, receipts, nil, nil
}

// Observes committed driver facts inside the caller's transaction without gating execution on the evidence
func captureTransitionLocked(ctx context.Context, conn state.SQLiteConn, roomID string) (err error) {
	owner, err := state.QueryRowObject(ctx, conn, "SELECT authority_gateway_id FROM hosted_rooms WHERE room_id=? AND disbanded_at IS NULL", roomID)
	if err != nil || owner == nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "SAVEPOINT work_transition_capture"); err != nil {
		return err
	}
	defer func() {
		if _, releaseErr := conn.ExecContext(ctx, "RELEASE work_transition_capture"); releaseErr != nil && err == nil {
			err = releaseErr
		}
	}()
	_, captureErr := CaptureWorkRecordLocked(ctx, conn, roomID, fmt.Sprint(owner.Value("authority_gateway_id")), nil)
	if captureErr == nil {
		return nil
	}
	var invalid *InvalidStoredWorkRecord
	if errors.As(captureErr, &invalid) {
		pkgLog().Warn(fmt.Sprintf("Hosted work evidence invalid for room %s", roomID))
		return nil
	}
	var workErr *WorkRecordError
	_, isSQLite := state.SQLiteClassOf(captureErr)
	if errors.As(captureErr, &workErr) || isSQLite {
		if _, err := conn.ExecContext(ctx, "ROLLBACK TO work_transition_capture"); err != nil {
			return err
		}
		pkgLog().Warn(fmt.Sprintf("Hosted work evidence unavailable for room %s", roomID), "error", captureErr)
		return nil
	}
	return captureErr
}

// Captures the source evidence of a room inside the caller's transaction
func CaptureWorkRecordLocked(ctx context.Context, conn state.SQLiteConn, roomID, localGatewayID string, throughSeq *int64) (map[string]any, error) {
	if err := workStorageInitialize(ctx, conn); err != nil {
		return nil, err
	}
	room, err := state.QueryRowObject(ctx, conn, "SELECT * FROM hosted_rooms WHERE room_id=?", roomID)
	if err != nil {
		return nil, err
	}
	if room == nil || hrStr(room, "authority_gateway_id") != localGatewayID || room.Value("disbanded_at") != nil {
		return nil, workRecordError("work record source is unavailable")
	}
	seq := hrInt(room, "next_seq") - 1
	if throughSeq != nil && seq > *throughSeq {
		return nil, workRecordPrefixError("history delivery must precede work records")
	}
	tasks, receipts, reason, err := workCaptureTasks(ctx, conn, roomID)
	if err != nil {
		return nil, err
	}
	stop, err := state.QueryRowObject(ctx, conn, `SELECT seq,payload_json FROM hosted_room_events
        WHERE room_id=? AND kind='room.stop_requested' ORDER BY seq DESC LIMIT 1`, roomID)
	if err != nil {
		return nil, err
	}
	members, err := roomLoads(hrStr(room, "members_json"))
	if err != nil {
		return nil, err
	}
	rosterDigest, err := WorkRosterDigest(members)
	if err != nil {
		return nil, err
	}
	anchor, err := workHistoryAnchor(ctx, conn, "hosted_room_events", roomID, seq)
	if err != nil {
		return nil, err
	}
	epoch := hrInt(room, "authority_epoch")
	version := int64(2)
	if epoch == 1 {
		version = 1
	}
	availability := "available"
	if reason != nil {
		availability = "unavailable"
	}
	stopFacts := map[string]any{"closing": false, "revocation_complete": false, "seq": int64(0), "cancel_id": nil}
	if stop != nil {
		stopPayload, err := roomLoads(hrStr(stop, "payload_json"))
		if err != nil {
			return nil, err
		}
		stopFacts["seq"] = hrInt(stop, "seq")
		cancelID, _ := stopPayload.(map[string]any)["cancel_id"]
		stopFacts["cancel_id"] = cancelID
	}
	content := map[string]any{
		"version": version, "room_id": roomID, "home_install_id": localGatewayID,
		"authority":     map[string]any{"gateway_id": localGatewayID, "epoch": epoch},
		"roster_sha256": rosterDigest, "history": map[string]any{"seq": seq, "event_sha256": anchor},
		"availability": availability, "reason": reason, "tasks": tasks, "receipts": receipts,
		"limitations": workRecordDisclosures, "stop": stopFacts,
	}
	var spans []AuthoritySpan
	if version == 2 {
		var lineage string
		if spans, lineage, err = workSourcePrefixLocked(ctx, conn, roomID, content["authority"].(map[string]any), seq); err != nil {
			return nil, err
		}
		content["lineage_sha256"] = lineage
		content["incompleteness"] = []any{workIncompletenessReason}
	}
	previous, err := state.QueryRowObject(ctx, conn, "SELECT * FROM "+WorkSourceTable+" WHERE room_id=? AND producer_gateway_id=? AND producer_epoch=?", workScope(content)...)
	if err != nil {
		return nil, err
	}
	var previousRecord map[string]any
	if previous != nil {
		if previousRecord, err = workValidateStoredLocked(ctx, conn, WorkSourceTable, previous); err != nil {
			return nil, err
		}
	}
	revision := int64(1)
	if previousRecord != nil {
		previousRevision, _ := hostedStrictInt(previousRecord["revision"])
		revision = previousRevision + 1
	}
	build := func() (map[string]any, error) {
		digest, err := workDigest(content)
		if err != nil {
			return nil, err
		}
		record := map[string]any{"revision": revision, "digest": digest}
		for key, item := range content {
			record[key] = item
		}
		return record, nil
	}
	record, err := build()
	if err != nil {
		return nil, err
	}
	encoded, err := WorkEncode(record)
	if err != nil {
		return nil, err
	}
	if len(encoded) > workMaxBytes {
		content["tasks"], content["receipts"], content["availability"], content["reason"] = []any{}, []any{}, "unavailable", "bounds_exceeded"
		if record, err = build(); err != nil {
			return nil, err
		}
	}
	verify := func() error {
		if _, err := ValidateWorkRecord(record); err != nil {
			return err
		}
		if err := workValidateRoster(record, members); err != nil {
			return err
		}
		if spans != nil {
			return workValidateProvenance(record, spans)
		}
		return nil
	}
	if err := verify(); err != nil {
		var workErr *WorkRecordError
		if !errors.As(err, &workErr) || reason != nil {
			return nil, err
		}
		content["tasks"], content["receipts"], content["availability"], content["reason"] = []any{}, []any{}, "unavailable", "unsupported_task"
		if record, err = build(); err != nil {
			return nil, err
		}
		if record, err = ValidateWorkRecord(record); err != nil {
			return nil, err
		}
	}
	if previousRecord != nil && previousRecord["digest"] == record["digest"] {
		return previousRecord, nil
	}
	if err := workSaveLocked(ctx, conn, record); err != nil {
		return nil, err
	}
	return record, nil
}

// Refuses tasks or receipts whose member targets differ from the room roster
func workValidateRoster(record map[string]any, members any) error {
	digest, err := WorkRosterDigest(members)
	if err != nil {
		return err
	}
	if digest != record["roster_sha256"] {
		return workRecordError("work record roster conflicts")
	}
	roster := map[string]map[string]any{}
	list, _ := members.([]any)
	for _, item := range list {
		member, _ := item.(map[string]any)
		key, has := member["member_id"]
		if !has {
			key = member["profile"]
		}
		roster[fmt.Sprint(key)] = member
	}
	for _, task := range workList(record["tasks"]) {
		if roster[fmt.Sprint(task["member_id"])]["profile"] != task["profile"] {
			return workRecordError("work record task target conflicts")
		}
	}
	for _, receipt := range workList(record["receipts"]) {
		target, _ := roster[fmt.Sprint(receipt["member_id"])]["target"].(map[string]any)
		if target["kind"] != "peer" || target["installation_id"] != receipt["target_install_id"] || target["profile"] != receipt["target_profile"] {
			return workRecordError("work record receipt target conflicts")
		}
	}
	return nil
}

// SHA-256 of the canonical member roster
func WorkRosterDigest(members any) (string, error) {
	_, encoded, err := hostedValidateMembers(members)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(encoded))
	return hex.EncodeToString(sum[:]), nil
}
