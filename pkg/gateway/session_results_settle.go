package gateway

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Display kind of the Hermes authored boundary that ends a failed turn
const failedTurnDisplayKind = "failed_turn"

// Boundary copy of a turn whose side effects are unknown
const partialFailedTurnNotice = "This turn did not complete. Some actions may already have run; verify their effects before resending."

type executionResultKey struct{}

// Context carrying the exact structured result the executing admission captures
func withExecutionResult(ctx context.Context, captured *pyjson.Object) context.Context {
	return context.WithValue(ctx, executionResultKey{}, captured)
}

// Captured result of the executing admission, nil outside an execution
func executionResultOf(ctx context.Context) *pyjson.Object {
	captured, _ := ctx.Value(executionResultKey{}).(*pyjson.Object)
	return captured
}

// Commits a failed result for the executing admission of a turn that never ran, returning the reply unchanged
func recordUnexecutedFailure(ctx context.Context, reply string, reason string, failure any) string {
	captured := executionResultOf(ctx)
	if captured == nil {
		return reply
	}
	if _, has := captured.Get("result"); has {
		return reply
	}
	text := "The admitted turn failed."
	if pyTruthy(failure) {
		text = pyrepr.Str(failure)
	} else if reply != "" {
		text = reply
	}
	result := pyjson.ObjectOf("final_response", "", "messages", []any{}, "failed", true, "completed", false, "error", text)
	captured.Set("result", result)
	if reason != "" {
		stampTurnFailure(result, reason, false)
	}
	return reply
}

// Retains an owner side failure even if an abandoned executor returns while delivery unwinds
func recordTerminalFailure(ctx context.Context, result *pyjson.Object) *pyjson.Object {
	if captured := executionResultOf(ctx); captured != nil {
		captured.Set("terminal_result", result)
	}
	return result
}

// Copy of a stored result with every string redacted at the storage boundary
func redactedResult(value any) any {
	switch v := value.(type) {
	case string:
		return tools.RedactSensitiveText(v)
	case *pyjson.Object:
		out := pyjson.NewObject()
		for _, key := range v.Keys {
			item, _ := v.Get(key)
			out.Set(key, redactedResult(item))
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			out[key] = redactedResult(item)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = redactedResult(item)
		}
		return out
	}
	return value
}

// Settles a claim completed with its redacted result
func retainResult(ctx context.Context, db *state.SessionDB, epoch int64, row, result *pyjson.Object) (*pyjson.Object, error) {
	stored, _ := redactedResult(result).(*pyjson.Object)
	return db.SettleSessionInput(ctx, state.SettleInput{Epoch: epoch, AdmissionID: rowString(row, "admission_id"),
		Generation: rowGeneration(row), Outcome: "completed", Result: stored})
}

// Generation an admission row was claimed under, zero for a row never claimed
func rowGeneration(row *pyjson.Object) int64 {
	if stamped := rowInt(row, "generation"); stamped != nil {
		return *stamped
	}
	return 0
}

// Outcome a result's own flags imply and the compact redacted copy settlement stores
func prepareResult(row *pyjson.Object, response any, outcome string, captured *pyjson.Object) (*state.CapturedResult, error) {
	if captured == nil {
		final := response
		if !pyTruthy(final) {
			final = ""
		}
		captured = pyjson.ObjectOf("result", pyjson.ObjectOf("final_response", final, "messages", []any{}), "usage", pyjson.NewObject())
	}
	raw, ok := captured.Get("result")
	if !ok {
		return nil, &pyKeyError{"result"}
	}
	value, ok := raw.(*pyjson.Object)
	if !ok {
		return nil, errors.New("'" + pyrepr.TypeName(raw) + "' object has no attribute 'get'")
	}
	interrupted, _ := value.Get("interrupted")
	failed, _ := value.Get("failed")
	failure, _ := value.Get("error")
	if pyTruthy(interrupted) {
		outcome = "interrupted"
	} else if pyTruthy(failed) || pyTruthy(failure) {
		outcome = "failed"
	}
	if outcome == "failed" || outcome == "interrupted" {
		value.Set(outcome, true)
		value.Set("completed", false)
	}
	text, _ := rowPayload(row).Get("text")
	compact, err := state.CompactResult(captured, text)
	if err != nil {
		return nil, err
	}
	stored, _ := redactedResult(compact).(*pyjson.Object)
	return &state.CapturedResult{Outcome: outcome, Stored: stored}, nil
}

// Commits a claim's outcome with its result, a row already terminal answering with what it committed
func finishResult(ctx context.Context, db *state.SessionDB, epoch int64, row *pyjson.Object, response any, outcome string, result *pyjson.Object,
	prepared *state.CapturedResult, transfers map[string]bool) (*pyjson.Object, any, error) {
	admissionID := rowString(row, "admission_id")
	var committed *pyjson.Object
	var saved string
	err := db.ReadCtx(ctx, func(conn state.SQLiteConn) error {
		committed, saved = nil, ""
		if err := state.RuntimeEpoch(ctx, conn, epoch); err != nil {
			return err
		}
		current, err := state.RuntimeAdmission(ctx, conn, admissionID)
		if err != nil {
			return err
		}
		owner := rowInt(current, "owner_epoch")
		if owner == nil || *owner != epoch || rowGeneration(current) != rowGeneration(row) {
			return runtimeRefusal("stale_generation")
		}
		if rowString(current, "status") != AdmissionTerminal {
			return nil
		}
		scanErr := conn.QueryRowContext(ctx, "SELECT value FROM state_meta WHERE key=?", state.TerminalResultPrefix+admissionID).Scan(&saved)
		if errors.Is(scanErr, sql.ErrNoRows) {
			return runtimeRefusal("storage_unavailable")
		}
		if scanErr != nil {
			return scanErr
		}
		committed = current.Copy()
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	if committed != nil {
		loaded, err := state.LoadTerminalResult(saved)
		if err != nil {
			return nil, nil, err
		}
		stored, _ := loaded.(*pyjson.Object)
		if stored == nil {
			return nil, nil, runtimeRefusal("storage_unavailable")
		}
		return committed, capturedResponse(stored), nil
	}
	if prepared == nil {
		if prepared, err = prepareResult(row, response, outcome, result); err != nil {
			return nil, nil, err
		}
	}
	notice := ownerStopNotice(result, prepared.Outcome, row)
	var displayMetadata *pyjson.Object
	if notice == "" {
		if refusal, metadata := gatewayRefusalBoundary(result, prepared.Outcome); metadata != nil {
			notice, displayMetadata = refusal, metadata
		}
	}
	var terminalWrite func(ctx context.Context, conn state.SQLiteConn, current *pyjson.Object) error
	if notice != "" {
		mark, _ := row.Get("transcript_mark")
		terminalWrite = func(writeCtx context.Context, conn state.SQLiteConn, _ *pyjson.Object) error {
			_, err := closeDiscardedTurn(writeCtx, db, conn, row, notice, mark, displayMetadata)
			return err
		}
	}
	settled, err := db.SettleSessionInput(ctx, state.SettleInput{Epoch: epoch, AdmissionID: admissionID, Generation: rowGeneration(row),
		Outcome: prepared.Outcome, Result: prepared.Stored, Transfers: transfers, TerminalWrite: terminalWrite})
	if err != nil {
		return nil, nil, err
	}
	return settled, response, nil
}

// Closer of a discarded turn's transcript tail, replaced by tests that fail the closure after it wrote
var closeDiscardedTurn func(ctx context.Context, db *state.SessionDB, conn state.SQLiteConn, row *pyjson.Object, notice any, replaceAfter any, displayMetadata *pyjson.Object) (bool, error)

func init() { closeDiscardedTurn = closeDiscardedTail }

// Closes a discarded turn's open transcript tail with a Hermes authored boundary on the resolution's own transaction
func closeDiscardedTail(ctx context.Context, db *state.SessionDB, conn state.SQLiteConn, row *pyjson.Object, notice any, replaceAfter any, displayMetadata *pyjson.Object) (bool, error) {
	physical, err := state.LocalPhysicalTarget(ctx, conn, rowString(row, "target_session_id"))
	if err != nil {
		return false, err
	}
	chain, err := state.CanonicalChain(ctx, conn, physical)
	if err != nil {
		return false, err
	}
	if len(chain) == 0 {
		return false, errors.New("list index out of range")
	}
	target := chain[len(chain)-1]
	var role string
	var toolCalls, displayKind any
	var tailID int64
	tailErr := conn.QueryRowContext(ctx, "SELECT role, tool_calls, id, display_kind FROM messages WHERE session_id=? AND active=1 "+
		"AND role NOT IN ('session_meta','system') ORDER BY id DESC LIMIT 1", target).Scan(&role, &toolCalls, &tailID, &displayKind)
	hasTail := tailErr == nil
	if tailErr != nil && !errors.Is(tailErr, sql.ErrNoRows) {
		return false, tailErr
	}
	text, hasNotice := notice.(string)
	mark, exact := replaceAfter.(int64)
	if n, isInt := replaceAfter.(int); isInt {
		mark, exact = int64(n), true
	}
	if hasNotice && exact && hasTail && role == "assistant" && columnText(displayKind) == failedTurnDisplayKind && tailID > mark {
		encoded, err := state.EncodeDisplayMetadata(metadataValue(displayMetadata))
		if err != nil {
			return false, err
		}
		_, err = conn.ExecContext(ctx, "UPDATE messages SET content=?, display_metadata=? WHERE id=?", text, encoded, tailID)
		return err == nil, err
	}
	openCalls := toolCalls != nil && columnText(toolCalls) != "" && columnText(toolCalls) != "[]"
	if !hasNotice && (!hasTail || !(role == "user" || (role == "assistant" && openCalls))) {
		return false, nil
	}
	content := partialFailedTurnNotice
	if hasNotice && text != "" {
		content = text
	}
	boundary := map[string]any{"role": "assistant", "content": content, "timestamp": float64(time.Now().UnixNano()) / 1e9,
		"display_kind": failedTurnDisplayKind}
	if displayMetadata != nil && displayMetadata.Len() > 0 {
		boundary["display_metadata"] = displayMetadata
	}
	if _, err := db.AppendMessagesInTransaction(ctx, conn, target, []map[string]any{boundary}, state.AppendGuards{}); err != nil {
		return false, err
	}
	return true, nil
}

// Text of a nullable text column read through a generic scan
func columnText(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case []byte:
		return string(v)
	}
	return ""
}

// Display metadata as the encoder takes it, nil when a boundary carries none
func metadataValue(displayMetadata *pyjson.Object) any {
	if displayMetadata == nil {
		return nil
	}
	return displayMetadata
}
