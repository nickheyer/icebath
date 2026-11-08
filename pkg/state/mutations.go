package state

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Refusal detail clients match when the rewind target was summarized by compaction
const CompactedTargetDetail = "compacted: target user message was summarized when the session was compressed"

var metadataFields = map[string]string{"title": "str", "archived": "bool", "hidden": "bool", "pinned": "bool", "unread": "bool"}

var compressFields = map[string]string{"focus": "str", "preview": "bool", "partial": "bool", "keep_last": "int"}

// Whether the value has exactly the Python type the field table names, bool never counting as int
func exactPyType(value any, kind string) bool {
	switch kind {
	case "str":
		_, ok := value.(string)
		return ok
	case "bool":
		_, ok := value.(bool)
		return ok
	case "int":
		_, ok := exactInt(value)
		return ok
	}
	return false
}

func payloadKeys(payload *pyjson.Object) map[string]bool {
	keys := make(map[string]bool, payload.Len())
	for _, key := range payload.Keys {
		keys[key] = true
	}
	return keys
}

func keysWithin(keys map[string]bool, allowed ...string) bool {
	permitted := map[string]bool{}
	for _, key := range allowed {
		permitted[key] = true
	}
	for key := range keys {
		if !permitted[key] {
			return false
		}
	}
	return true
}

func keysExactly(keys map[string]bool, want ...string) bool {
	return len(keys) == len(want) && keysWithin(keys, want...)
}

func nonBlankStr(v any) bool {
	text, ok := v.(string)
	return ok && pystr.Strip(text) != ""
}

// Refuses as invalid_params every operation and payload outside the closed mutation set
func ValidateAction(operation any, payloadValue any) error {
	payload, isObj := payloadValue.(*pyjson.Object)
	op, isStr := operation.(string)
	if !isObj || !isStr {
		return runtimeRefusal("invalid_params")
	}
	keys := payloadKeys(payload)
	if op == "model" && keys["model"] && keysWithin(keys, "model", "provider", "confirm", "reasoning") {
		allText := true
		for _, key := range payload.Keys {
			allText = allText && nonBlankStr(rowGet(payload, key))
		}
		if allText && (!keys["reasoning"] || config.ParseReasoningEffort(rowGet(payload, "reasoning")) != nil) {
			return nil
		}
	}
	if op == "branch" && validBranchPayload(payload) {
		return nil
	}
	if op == "reasoning" && keysExactly(keys, "effort") {
		effort := rowGet(payload, "effort")
		if effort == nil {
			return nil
		}
		if _, ok := effort.(string); ok && config.ParseReasoningEffort(effort) != nil {
			return nil
		}
	}
	if op == "workspace" && keysExactly(keys, "cwd") && nonBlankStr(rowGet(payload, "cwd")) {
		return nil
	}
	if op == "editor_mcp" && keysExactly(keys, "mcp_servers") && validEditorFingerprint(payload) {
		return nil
	}
	if op == "edit_policy" && keysExactly(keys, "policy") {
		if policy, ok := rowGet(payload, "policy").(string); ok && (policy == "ask" || policy == "workspace_session" || policy == "session") {
			return nil
		}
	}
	if op == "compress" && validCompressPayload(payload) {
		return nil
	}
	if (op == "delete" || op == "reset") && payload.Len() == 0 {
		return nil
	}
	if op == "import" && keysExactly(keys, "sessions") {
		if _, ok := rowGet(payload, "sessions").([]any); ok {
			return nil
		}
	}
	if op == "rewind" && keys["target_message_id"] && keysWithin(keys, "target_message_id", "preserve_compaction_handoff") {
		target, isInt := exactInt(rowGet(payload, "target_message_id"))
		preserve, has := payload.Get("preserve_compaction_handoff")
		_, isBool := preserve.(bool)
		if isInt && target > 0 && (!has || isBool) {
			return nil
		}
	}
	valid := false
	switch op {
	case "sidebar":
		valid = payload.Len() > 0 && keysWithin(keys, "title", "archived", "hidden", "pinned", "unread")
	case "rename":
		valid = keysExactly(keys, "title")
	case "archive":
		valid = keysExactly(keys, "archived")
	}
	if !valid {
		return runtimeRefusal("invalid_params")
	}
	for _, key := range payload.Keys {
		if !exactPyType(rowGet(payload, key), metadataFields[key]) {
			return runtimeRefusal("invalid_params")
		}
	}
	return nil
}

func validBranchPayload(payload *pyjson.Object) bool {
	keys := payloadKeys(payload)
	if !keysWithin(keys, "title", "through_message_id", "mcp_servers") {
		return false
	}
	if keys["mcp_servers"] && !validEditorFingerprint(payload) {
		return false
	}
	if title, has := payload.Get("title"); has {
		if _, ok := title.(string); !ok {
			return false
		}
	}
	through, has := payload.Get("through_message_id")
	id, isInt := exactInt(through)
	return !has || (isInt && id > 0)
}

func validEditorFingerprint(payload *pyjson.Object) bool {
	servers, ok := rowGet(payload, "mcp_servers").(*pyjson.Object)
	if !ok || !keysExactly(payloadKeys(servers), "fingerprint") {
		return false
	}
	_, isStr := rowGet(servers, "fingerprint").(string)
	return isStr
}

func validCompressPayload(payload *pyjson.Object) bool {
	for _, key := range payload.Keys {
		kind, known := compressFields[key]
		if !known || !exactPyType(rowGet(payload, key), kind) {
			return false
		}
	}
	keepLast, has := payload.Get("keep_last")
	if !has {
		return true
	}
	n, _ := exactInt(keepLast)
	return rowGet(payload, "partial") == true && n >= 1
}

// Runs the closed mutation handler and returns the affected session ids and the receipt projection
func (d *SessionDB) applyAction(ctx context.Context, conn SQLiteConn, sessionID, operation string, payload, prepared *pyjson.Object) ([]string, *pyjson.Object, error) {
	switch operation {
	case "compress":
		return compressInTransaction(d, ctx, conn, sessionID, payload, prepared)
	case "model", "reasoning", "workspace", "editor_mcp", "edit_policy":
		return policyHandlers[operation](d, ctx, conn, sessionID, payload, prepared)
	case "branch":
		return branchInTransaction(d, ctx, conn, sessionID, payload, prepared)
	case "delete":
		return deleteInTransaction(d, ctx, conn, sessionID, payload)
	case "import":
		return d.importMutation(ctx, conn, sessionID, payload)
	case "rewind":
		return d.rewindMutation(ctx, conn, sessionID, payload)
	case "reset":
		return resetInTransaction(d, ctx, conn, sessionID, payload)
	}
	var affected []string
	result := pyjson.NewObject()
	for _, key := range payload.Keys {
		value := rowGet(payload, key)
		if key == "title" {
			title, _ := value.(string)
			touched, err := d.setSessionTitleInTransaction(ctx, conn, sessionID, title, TitleSourceUser)
			if err != nil {
				return nil, nil, err
			}
			affected = append(affected, touched...)
			stored, err := requiredScalar(ctx, conn, "SELECT title FROM sessions WHERE id=?", sessionID)
			if err != nil {
				return nil, nil, err
			}
			result.Set(key, stored)
			continue
		}
		column := key
		var stored any
		if key == "unread" {
			column = "last_read_at"
			stored = nowSeconds()
			if value == true {
				stored = 0.0
			}
		} else if value == true {
			stored = int64(1)
		} else {
			stored = int64(0)
		}
		touched, err := setLineageColumnInTransaction(ctx, conn, column, sessionID, stored, "")
		if err != nil {
			return nil, nil, err
		}
		affected = append(affected, touched...)
		result.Set(key, value)
	}
	return distinctSessionIDs(affected...), result, nil
}

// Rewinds the physical target to an active user row, keeping a compaction carrier's handoff
func (d *SessionDB) rewindMutation(ctx context.Context, conn SQLiteConn, sessionID string, payload *pyjson.Object) ([]string, *pyjson.Object, error) {
	physical, err := localPhysicalTarget(ctx, conn, sessionID)
	if err != nil {
		return nil, nil, err
	}
	if err := d.RequireIdle(ctx, conn, distinctSessionIDs(sessionID, physical)); err != nil {
		return nil, nil, err
	}
	targetID, _ := exactInt(rowGet(payload, "target_message_id"))
	role, ok, err := fetchScalar(ctx, conn, "SELECT role FROM messages WHERE id=? AND session_id=? AND active=1", targetID, physical)
	if err != nil {
		return nil, nil, err
	}
	field := "target_message_id"
	if !ok || role != "user" {
		summarized, err := summarizedByCompaction(ctx, conn, physical, targetID)
		if err != nil {
			return nil, nil, err
		}
		detail := "target user message is no longer in session history"
		if summarized {
			detail = CompactedTargetDetail
		}
		return nil, nil, NewRuntimeStoreError("invalid_params", &field, &detail)
	}
	live, err := d.carrierLiveView(ctx, conn, physical, targetID)
	if err != nil {
		return nil, nil, err
	}
	preserve := live != nil || rowGet(payload, "preserve_compaction_handoff") == true
	target, ids, head, replacement, err := d.rewindInTransaction(ctx, conn, physical, targetID, RewindOptions{PreserveCompactionHandoff: preserve})
	if err != nil {
		return nil, nil, err
	}
	message, err := d.rowToMessageDict(target, "rewind receipt", true)
	if err != nil {
		return nil, nil, err
	}
	if live != nil {
		message.Set("content", live["content"])
	}
	message.Set("content", TypedPromptContent(rowGet(message, "content"), message))
	if _, err := bumpRuntimeGeneration(ctx, conn, sessionID); err != nil {
		return nil, nil, err
	}
	var replacementID any
	if replacement != nil {
		replacementID = *replacement
	}
	result := pyjson.ObjectOf("rewound_count", int64(len(ids)), "target_message", message, "new_head_id", head, "replacement_message_id", replacementID)
	return distinctSessionIDs(sessionID, physical), result, nil
}

// Whether compaction took the user turn out of the physical live transcript rather than a rewind
func summarizedByCompaction(ctx context.Context, conn SQLiteConn, physical string, messageID int64) (bool, error) {
	row, ok, err := fetchRow(ctx, conn, "SELECT session_id, role, active, compacted FROM messages WHERE id=?", messageID)
	if err != nil || !ok || row[1] != "user" {
		return false, err
	}
	if row[0] == physical {
		return pyjson.Equal(row[2], int64(0)) && pyjson.Equal(row[3], int64(1)), nil
	}
	if !pyjson.Equal(row[2], int64(1)) && !pyjson.Equal(row[3], int64(1)) {
		return false, nil
	}
	lineage := map[string]bool{physical: true}
	frontier := []string{physical}
	for range chainCap {
		expanded, err := expandCompressionLineageIDs(ctx, conn, frontier)
		if err != nil {
			return false, err
		}
		frontier = frontier[:0]
		for _, sid := range expanded {
			if !lineage[sid] {
				frontier = append(frontier, sid)
			}
		}
		if len(frontier) == 0 {
			break
		}
		for _, sid := range frontier {
			lineage[sid] = true
		}
	}
	return lineage[pyStr(row[0])], nil
}

// Live ask of a composite compaction carrier, nil for an ordinary user row
func (d *SessionDB) carrierLiveView(ctx context.Context, conn SQLiteConn, sessionID string, messageID int64) (map[string]any, error) {
	rows, err := queryObjects(ctx, conn, "SELECT * FROM messages WHERE id=? AND session_id=? AND active=1", messageID, sessionID)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	message, err := d.rowToMessageDict(rows[0], "rewind target", true)
	if err != nil {
		return nil, err
	}
	plain, _ := plainDictTree(message).(map[string]any)
	handoff, live := SplitUserOriginatedTurn(plain)
	if handoff == nil {
		return nil, nil
	}
	return live, nil
}

// Imports a validated payload whose first session is the mutated id, detaching foreign parent links
func (d *SessionDB) importMutation(ctx context.Context, conn SQLiteConn, sessionID string, payload *pyjson.Object) ([]string, *pyjson.Object, error) {
	rows, _ := rowGet(payload, "sessions").([]any)
	if len(rows) > importMaxSessions {
		return nil, nil, runtimeRefusal("invalid_params")
	}
	normalized, errs, err := d.validateImportPayload(rows)
	if err != nil {
		return nil, nil, err
	}
	if len(errs) > 0 || len(normalized) == 0 || sessionID != pyStr(rowGet(rowGet(normalized[0], "session").(*pyjson.Object), "id")) {
		return nil, nil, runtimeRefusal("invalid_params")
	}
	var imported, skipped []string
	type link struct{ sid, parent string }
	var parents []link
	for _, item := range normalized {
		raw := rowGet(item, "session").(*pyjson.Object)
		sid := pyStr(rowGet(raw, "id"))
		if _, retired, err := fetchScalar(ctx, conn, "SELECT 1 FROM state_meta WHERE key=?", RetiredPrefix+sid); err != nil {
			return nil, nil, err
		} else if retired {
			field, detail := "sessions", fmt.Sprintf("session %s was deleted here; a deleted session is not restored", sid)
			return nil, nil, NewRuntimeStoreError("admission_conflict", &field, &detail)
		}
		if _, exists, err := fetchScalar(ctx, conn, "SELECT 1 FROM sessions WHERE id=?", sid); err != nil {
			return nil, nil, err
		} else if exists {
			skipped = append(skipped, sid)
			continue
		}
		if err := d.importSessionRow(ctx, conn, raw, rowGet(item, "messages"), sid); err != nil {
			return nil, nil, err
		}
		if parent := rowGet(raw, "parent_session_id"); pyTruthy(parent) {
			parents = append(parents, link{sid, pyStr(parent)})
		}
		imported = append(imported, sid)
	}
	fresh := map[string]bool{}
	for _, sid := range imported {
		fresh[sid] = true
	}
	var kept [][2]string
	foreign := 0
	for _, l := range parents {
		parentBinding, err := HistoryBinding(ctx, conn, l.parent)
		if err != nil {
			return nil, nil, err
		}
		childBinding, err := HistoryBinding(ctx, conn, l.sid)
		if err != nil {
			return nil, nil, err
		}
		if !fresh[l.parent] && (parentBinding == nil || !pyjson.Equal(parentBinding, childBinding)) {
			foreign++
			continue
		}
		kept = append(kept, [2]string{l.sid, l.parent})
	}
	detached, err := attachImportParents(ctx, conn, kept)
	if err != nil {
		return nil, nil, err
	}
	result := pyjson.ObjectOf("ok", true, "imported", int64(len(imported)), "skipped", int64(len(skipped)),
		"imported_ids", stringsToAny(imported), "skipped_ids", stringsToAny(skipped), "detached", int64(detached+foreign), "errors", []any{})
	return imported, result, nil
}
