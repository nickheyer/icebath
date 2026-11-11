package gateway

import (
	"context"
	"math"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

const (
	maxAuthoritySpans   = 1024
	maxTaskInputEvents  = 128
	hostedTransferKinds = "('authority.claimed', 'authority.lost')"
)

// Committed authority transfers, each the first event of its span
var authorityTransferKinds = map[string]bool{"authority.claimed": true, "authority.lost": true}

// A recorded authority sequence that cannot explain the current room
type AuthorityHistoryError struct{ Msg string }

func (e *AuthorityHistoryError) Error() string { return e.Msg }

// Python class name of the error
func (e *AuthorityHistoryError) PyTypeName() string { return "AuthorityHistoryError" }

func authorityHistoryError(msg string) error { return &AuthorityHistoryError{msg} }

// One authority epoch and the first sequence it owns
type AuthoritySpan struct {
	GatewayID string
	Epoch     int64
	FromSeq   int64
}

// Wire mapping of the span
func (s AuthoritySpan) AsMapping() map[string]any {
	return map[string]any{"gateway_id": s.GatewayID, "epoch": s.Epoch, "from_seq": s.FromSeq}
}

// Canonical authority gateway id
func authorityGateway(value any) (string, error) {
	result, err := roomIdentifier(value, roomIdentifierSpec{label: "authority gateway", maxChars: 128}, authorityHistoryError)
	if err != nil {
		return "", err
	}
	if text, _ := value.(string); text != result {
		return "", authorityHistoryError("authority gateway must be canonical")
	}
	return result, nil
}

// Contextual epochs of a recorded history ending at the current owner, never a grant of control
func ValidateAuthorityHistory(value any, gatewayID string, epoch int64) ([]AuthoritySpan, error) {
	if value == nil {
		return []AuthoritySpan{{gatewayID, epoch, 0}}, nil
	}
	list, ok := pyjson.Plain(value).([]any)
	if !ok || len(list) < 1 || len(list) > maxAuthoritySpans {
		return nil, authorityHistoryError("authority history exceeds its bound or is absent")
	}
	var spans []AuthoritySpan
	for _, item := range list {
		fields, ok := item.(map[string]any)
		if !ok || len(fields) != 3 {
			return nil, authorityHistoryError("authority history fields are invalid")
		}
		for _, key := range []string{"gateway_id", "epoch", "from_seq"} {
			if _, has := fields[key]; !has {
				return nil, authorityHistoryError("authority history fields are invalid")
			}
		}
		source, err := authorityGateway(fields["gateway_id"])
		if err != nil {
			return nil, err
		}
		generation, okGen := hostedStrictInt(fields["epoch"])
		start, okStart := hostedStrictInt(fields["from_seq"])
		if !okGen || generation < 1 || !okStart || start < 0 {
			return nil, authorityHistoryError("authority history coordinates are invalid")
		}
		if len(spans) == 0 {
			if generation != 1 || start != 0 {
				return nil, authorityHistoryError("authority history has no verified starting point")
			}
		} else if last := spans[len(spans)-1]; generation != last.Epoch+1 || start <= last.FromSeq {
			return nil, authorityHistoryError("authority history has a gap or unordered boundary")
		}
		spans = append(spans, AuthoritySpan{source, generation, start})
	}
	if last := spans[len(spans)-1]; last.GatewayID != gatewayID || last.Epoch != epoch {
		return nil, authorityHistoryError("authority history does not reach the current owner")
	}
	return spans, nil
}

// Span that owns sequence seq
func AuthorityAtSequence(history []AuthoritySpan, seq int64) (AuthoritySpan, error) {
	for i := len(history) - 1; i >= 0; i-- {
		if seq >= history[i].FromSeq {
			return history[i], nil
		}
	}
	return AuthoritySpan{}, authorityHistoryError("event precedes the known authority history")
}

// Original coordinator of a room, stable across authority changes
func OriginGatewayID(room map[string]any) (string, error) {
	epoch, _ := hostedStrictInt(room["authority_epoch"])
	gatewayID, _ := room["authority_gateway_id"].(string)
	history, err := ValidateAuthorityHistory(room["authority_history"], gatewayID, epoch)
	if err != nil {
		return "", err
	}
	return history[0].GatewayID, nil
}

// Committed authority transfers read inside the caller's room state transaction, nil for none to certify
func ReadAuthorityHistoryLocked(ctx context.Context, conn state.SQLiteConn, roomID, gatewayID string, epoch int64) ([]any, error) {
	rows, err := state.QueryObjects(ctx, conn, `SELECT seq,authority_epoch,actor_json,payload_json FROM hosted_room_events
           WHERE room_id=? AND kind IN `+hostedTransferKinds+` ORDER BY seq LIMIT ?`, roomID, maxAuthoritySpans)
	if err != nil {
		return nil, err
	}
	if len(rows) >= maxAuthoritySpans {
		return nil, authorityHistoryError("authority history exceeds its bound")
	}
	if len(rows) == 0 {
		if epoch == 1 {
			return nil, nil
		}
		return nil, authorityHistoryError("current authority has no committed lineage")
	}
	decoded, err := roomLoads(hrStr(rows[0], "payload_json"))
	if err != nil {
		return nil, err
	}
	first, isMap := decoded.(map[string]any)
	if isMap && first["previous_gateway_id"] == "legacy" {
		return nil, nil
	}
	if _, has := first["previous_gateway_id"]; !isMap || !has {
		return nil, authorityHistoryError("authority history has no previous owner")
	}
	origin, err := authorityGateway(first["previous_gateway_id"])
	if err != nil {
		return nil, err
	}
	history := []AuthoritySpan{{origin, 1, 0}}
	for _, row := range rows {
		actor, err := roomLoads(hrStr(row, "actor_json"))
		if err != nil {
			return nil, err
		}
		decodedPayload, err := roomLoads(hrStr(row, "payload_json"))
		if err != nil {
			return nil, err
		}
		payload, isMap := decodedPayload.(map[string]any)
		if !pyjson.Equal(actor, map[string]any{"kind": "system", "id": "authority-control"}) || !isMap {
			return nil, authorityHistoryError("authority claim provenance is invalid")
		}
		last := history[len(history)-1]
		expected := last.Epoch + 1
		claimed, isInt := hostedStrictInt(payload["authority_epoch"])
		rowEpoch, rowIsInt := row.Vals["authority_epoch"].(int64)
		if payload["previous_gateway_id"] != last.GatewayID || !isInt || claimed != expected || !rowIsInt || rowEpoch != expected {
			return nil, authorityHistoryError("authority claim does not extend its predecessor")
		}
		next, err := authorityGateway(payload["authority_gateway_id"])
		if err != nil {
			return nil, err
		}
		history = append(history, AuthoritySpan{next, expected, hrInt(row, "seq")})
	}
	result := make([]any, len(history))
	for i, span := range history {
		result[i] = span.AsMapping()
	}
	if _, err := ValidateAuthorityHistory(result, gatewayID, epoch); err != nil {
		return nil, err
	}
	return result, nil
}

// Bounded immutable input references of a compacting policy task
func ValidateTaskInput(value any) (map[string]any, error) {
	fields, ok := hostedMapping(value)
	if !ok || len(fields) != 2 {
		return nil, roomPlainValueError("invalid task input context")
	}
	watermarkValue, hasWatermark := fields["watermark"]
	seqsValue, hasSeqs := fields["event_seqs"]
	if !hasWatermark || !hasSeqs {
		return nil, roomPlainValueError("invalid task input context")
	}
	watermark, isInt := hostedStrictInt(watermarkValue)
	if !isInt || watermark < 0 {
		return nil, roomPlainValueError("invalid task input watermark")
	}
	seqs, ok := pyjson.Plain(seqsValue).([]any)
	if !ok || len(seqs) < 1 || len(seqs) > maxTaskInputEvents {
		return nil, roomPlainValueError("task input context exceeded its bound")
	}
	previous := watermark
	for _, item := range seqs {
		seq, isInt := hostedStrictInt(item)
		if !isInt || !(previous < seq && seq <= math.MaxInt64) {
			return nil, roomPlainValueError("task input sequences must be increasing after the watermark")
		}
		previous = seq
	}
	return map[string]any{"watermark": watermark, "event_seqs": seqs}, nil
}

// A plain ValueError raised by a hosted room validator
type RoomValueError struct{ Msg string }

func (e *RoomValueError) Error() string { return e.Msg }

// Python class name of the error
func (e *RoomValueError) PyTypeName() string { return "ValueError" }

func roomPlainValueError(msg string) error { return &RoomValueError{msg} }

// Verified authority spans of the source prefix through seq and the digest of its lineage
func workSourcePrefixLocked(ctx context.Context, conn state.SQLiteConn, roomID string, authority map[string]any, seq int64) ([]AuthoritySpan, string, error) {
	gatewayID, _ := authority["gateway_id"].(string)
	epoch, _ := hostedStrictInt(authority["epoch"])
	history, err := ReadAuthorityHistoryLocked(ctx, conn, roomID, gatewayID, epoch)
	if err != nil {
		return nil, "", err
	}
	if history == nil {
		return nil, "", workRecordPrefixError("successor work evidence has no verified lineage")
	}
	spans, err := ValidateAuthorityHistory(history, gatewayID, epoch)
	if err != nil {
		return nil, "", err
	}
	rows, err := state.QueryObjects(ctx, conn, "SELECT seq,authority_epoch,actor_json,kind FROM hosted_room_events WHERE room_id=? AND seq<=? ORDER BY seq", roomID, seq)
	if err != nil {
		return nil, "", err
	}
	var count int64
	for _, event := range rows {
		count++
		span, err := AuthorityAtSequence(spans, count)
		if err != nil {
			return nil, "", err
		}
		decoded, err := roomLoads(hrStr(event, "actor_json"))
		if err != nil {
			return nil, "", err
		}
		actor, _ := decoded.(map[string]any)
		boundary := span.Epoch > 1 && count == span.FromSeq
		eventEpoch, isInt := event.Vals["authority_epoch"].(int64)
		if hrInt(event, "seq") != count || !isInt || eventEpoch != span.Epoch ||
			(actor["kind"] == "gateway" && actor["id"] != span.GatewayID) || authorityTransferKinds[hrStr(event, "kind")] != boundary {
			return nil, "", workRecordPrefixError("work record history prefix is not verified")
		}
	}
	if count != seq {
		return nil, "", workRecordPrefixError("work record history prefix is unavailable")
	}
	lineage, err := workDigest(history)
	return spans, lineage, err
}

// Refuses a task or receipt that no committed authority span explains
func workValidateProvenance(record map[string]any, spans []AuthoritySpan) error {
	for _, task := range workList(record["tasks"]) {
		seq, _ := hostedStrictInt(task["source_event_seq"])
		if _, err := AuthorityAtSequence(spans, seq); err != nil {
			return err
		}
	}
	for _, receipt := range workList(record["receipts"]) {
		epoch, _ := hostedStrictInt(receipt["authority_epoch"])
		found := false
		for _, span := range spans {
			if span.GatewayID == receipt["home_install_id"] && receipt["home_install_id"] == receipt["authority_gateway_id"] && span.Epoch == epoch {
				found = true
				break
			}
		}
		if !found {
			return workRecordError("work record receipt has no original authority span")
		}
	}
	return nil
}

// Origin authority of each recorded task by its source event
func WorkTaskOrigins(record map[string]any, spans []AuthoritySpan) (map[string]any, error) {
	out := map[string]any{}
	for _, task := range workList(record["tasks"]) {
		seq, _ := hostedStrictInt(task["source_event_seq"])
		origin, err := AuthorityAtSequence(spans, seq)
		if err != nil {
			return nil, err
		}
		out[task["task_id"].(string)] = map[string]any{"gateway_id": origin.GatewayID, "epoch": origin.Epoch}
	}
	return out, nil
}

// Object items of a record list
func workList(value any) []map[string]any {
	list, _ := value.([]any)
	out := make([]map[string]any, 0, len(list))
	for _, item := range list {
		if fields, ok := item.(map[string]any); ok {
			out = append(out, fields)
		}
	}
	return out
}
