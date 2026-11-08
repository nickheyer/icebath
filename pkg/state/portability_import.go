package state

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/big"
	"sort"

	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

var (
	importSessionTextFields = []string{"source", "user_id", "model", "system_prompt", "end_reason", "cwd", "git_branch", "git_repo_root",
		"billing_provider", "billing_base_url", "billing_mode", "cost_status", "cost_source", "pricing_version", "title"}
	importMessageTextFields = []string{"tool_call_id", "tool_name", "effect_disposition", "finish_reason",
		"reasoning", "reasoning_content", "platform_message_id", "message_id"}
	importMessageJSONFields = []string{"reasoning_details", "codex_reasoning_items", "codex_message_items", "absorbed_message_uids", "tool_call_uids"}
	importPassthroughCols   = []string{"user_id", "model", "model_config", "end_reason", "cwd", "git_branch", "git_repo_root", "billing_provider",
		"billing_base_url", "billing_mode", "cost_status", "cost_source", "pricing_version", "title"}
	importIntCols   = []string{"input_tokens", "output_tokens", "cache_read_tokens", "cache_write_tokens", "reasoning_tokens", "api_call_count"}
	importFloatCols = []string{"ended_at", "estimated_cost_usd", "actual_cost_usd"}
	importFlagCols  = []string{"archived", "auto_archived", "pinned", "hidden"}
	sqliteIntMin    = new(big.Int).Neg(new(big.Int).Lsh(big.NewInt(1), 63))
	sqliteIntMax    = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 63), big.NewInt(1))
)

const importSessionInsertSQL = `INSERT INTO sessions (
                           id, source, user_id, model, model_config, system_prompt,
                           system_prompt_hash,
                           parent_session_id, started_at, ended_at, end_reason,
                           message_count, tool_call_count, input_tokens, output_tokens,
                           cache_read_tokens, cache_write_tokens, reasoning_tokens,
                           cwd, git_branch, git_repo_root,
                           billing_provider, billing_base_url, billing_mode,
                           estimated_cost_usd, actual_cost_usd, cost_status, cost_source,
                           pricing_version, title, api_call_count, archived, auto_archived, pinned, hidden
                       )
                       VALUES (
                           ?, ?, ?, ?, ?,
                           NULL, ?, NULL, ?, ?,
                           ?, 0, 0, ?, ?,
                           ?, ?,
                           ?, ?, ?, ?,
                           ?, ?, ?,
                           ?, ?, ?,
                           ?, ?, ?,
                           ?, ?, ?, ?, ?
                       )`

// Whether a Python int lies inside the signed 64 bit range SQLite binds
func withinSQLiteInt(n *big.Int) bool {
	return n.Cmp(sqliteIntMin) >= 0 && n.Cmp(sqliteIntMax) <= 0
}

// Python int of a decoded value, nil for one int refuses without an OverflowError
func importBigInt(value any) (*big.Int, error) {
	n, err := pyIntOf(value)
	if err != nil {
		var conv *pyConvError
		if errors.As(err, &conv) && conv.Class == "OverflowError" {
			return nil, err
		}
		return nil, nil
	}
	return n, nil
}

// The value when nil or a string, ValueError naming the field otherwise
func importTextOrNone(value any, field string) (any, error) {
	if value == nil {
		return nil, nil
	}
	if _, ok := value.(string); ok {
		return value, nil
	}
	return nil, pyValueError(field + " must be a string")
}

// int of the value, nil for nil and ValueError naming the field for an unparsable one
func importIntOrNone(value any, field string) (*big.Int, error) {
	if value == nil {
		return nil, nil
	}
	n, err := importBigInt(value)
	if err != nil {
		return nil, err
	}
	if n == nil {
		return nil, pyValueError(field + " must be an integer")
	}
	return n, nil
}

// First path of a NaN or infinite number anywhere in decoded JSON data, walked last child first
func importNonFinitePath(value any, path string) (string, bool) {
	type pending struct {
		value any
		path  string
	}
	stack := []pending{{value, path}}
	for len(stack) > 0 {
		current := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		switch v := current.value.(type) {
		case float64:
			if math.IsNaN(v) || math.IsInf(v, 0) {
				return current.path, true
			}
		case *pyjson.Object:
			for _, key := range v.Keys {
				stack = append(stack, pending{v.Vals[key], current.path + "." + key})
			}
		case map[string]any:
			keys := make([]string, 0, len(v))
			for key := range v {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				stack = append(stack, pending{v[key], current.path + "." + key})
			}
		case []any:
			for i, child := range v {
				stack = append(stack, pending{child, fmt.Sprintf("%s[%d]", current.path, i)})
			}
		}
	}
	return "", false
}

// json.loads refusing NaN and Infinity anywhere in the decoded value
func importLoadJSON(value, path string) (any, error) {
	parsed, err := pyjson.Loads(value)
	if err != nil {
		return nil, err
	}
	if at, found := importNonFinitePath(parsed, path); found {
		return nil, pyValueError("non-finite number at " + at)
	}
	return parsed, nil
}

// True for a JSON syntax error as opposed to a non finite refusal
func isJSONDecodeError(err error) bool {
	var decodeErr *pyjson.DecodeError
	return errors.As(err, &decodeErr)
}

// JSON object text of a stored or decoded object column, nil for nil and ValueError for anything else
func importJSONObjectOrNone(value any, field string) (any, error) {
	if value == nil {
		return nil, nil
	}
	path := "session." + field
	if text, ok := value.(string); ok {
		parsed, err := importLoadJSON(text, path)
		if err != nil {
			if isJSONDecodeError(err) {
				return nil, pyValueError(field + " must be valid JSON")
			}
			return nil, err
		}
		if _, isObj := parsed.(*pyjson.Object); !isObj {
			return nil, pyValueError(field + " must be a JSON object")
		}
		return value, nil
	}
	obj, ok := value.(*pyjson.Object)
	if !ok {
		if plain, isMap := value.(map[string]any); isMap {
			obj = pyjson.NewObject()
			keys := make([]string, 0, len(plain))
			for key := range plain {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				obj.Set(key, plain[key])
			}
		} else {
			return nil, pyValueError(field + " must be a JSON object")
		}
	}
	if at, found := importNonFinitePath(obj, path); found {
		return nil, pyValueError("non-finite number at " + at)
	}
	text, err := pyDumps(obj, pyJSONDefault)
	if err != nil {
		return nil, pyValueError(field + " must be JSON serializable")
	}
	return text, nil
}

// int of the value with the default for nil or an unparsable value, an OverflowError propagating
func coerceOrInt(value any, def *big.Int) (*big.Int, error) {
	if value == nil {
		return def, nil
	}
	n, err := importBigInt(value)
	if err != nil {
		return nil, err
	}
	if n == nil {
		return def, nil
	}
	return n, nil
}

// Imported int column inside the SQLite range, the default otherwise
func importIntOrBounded(value any, def int64) (int64, error) {
	n, err := coerceOrInt(value, big.NewInt(def))
	if err != nil {
		return 0, err
	}
	if !withinSQLiteInt(n) {
		return def, nil
	}
	return n.Int64(), nil
}

// Refuses scalar message content SQLite cannot bind or JSON cannot serialize later
func validateImportMessageContent(content any, messagePath string) error {
	switch v := content.(type) {
	case *big.Int:
		if !withinSQLiteInt(v) {
			return pyValueError(messagePath + ".content is outside SQLite's integer range")
		}
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return pyValueError("non-finite number at " + messagePath + ".content")
		}
	case []byte:
		return pyValueError(messagePath + ".content must be a string, list, object, finite number, or null")
	}
	return nil
}

// Finite float of an imported float column, nil when missing, unparsable or not finite
func importFloatOrFinite(value any) any {
	if value == nil {
		return nil
	}
	f, err := pyFloatOf(value)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return nil
	}
	return f
}

// Type checks one payload message, ValueError naming the offending field
func normalizeImportMessage(message *pyjson.Object, index int) (*pyjson.Object, error) {
	clean := message.Copy()
	role, _ := clean.Get("role")
	if text, ok := role.(string); !ok || text == "" {
		return nil, pyValueError(fmt.Sprintf("messages[%d].role must be a non-empty string", index))
	}
	for _, field := range importMessageTextFields {
		value, _ := clean.Get(field)
		text, err := importTextOrNone(value, field)
		if err != nil {
			return nil, err
		}
		clean.Set(field, text)
	}
	tokenRaw, _ := clean.Get("token_count")
	tokenCount, err := importIntOrNone(tokenRaw, "token_count")
	if err != nil {
		return nil, err
	}
	if tokenCount != nil && !withinSQLiteInt(tokenCount) {
		return nil, pyValueError("token_count is outside SQLite's integer range")
	}
	if tokenCount == nil {
		clean.Set("token_count", nil)
	} else {
		clean.Set("token_count", tokenCount.Int64())
	}
	messagePath := fmt.Sprintf("session.messages[%d]", index)
	if timestamp, _ := clean.Get("timestamp"); timestamp != nil {
		if _, ok := pydatetime.CoerceEpoch(timestamp, "", "messages[].timestamp"); !ok {
			return nil, pyValueError(messagePath + ".timestamp must be a supported Unix timestamp")
		}
	}
	content, _ := clean.Get("content")
	if err := validateImportMessageContent(content, messagePath); err != nil {
		return nil, err
	}
	for _, field := range importMessageJSONFields {
		jsonValue, _ := clean.Get(field)
		if text, ok := jsonValue.(string); ok {
			if _, err := importLoadJSON(text, messagePath+"."+field); err != nil && !isJSONDecodeError(err) {
				return nil, err
			}
		} else if jsonValue != nil {
			if at, found := importNonFinitePath(jsonValue, messagePath+"."+field); found {
				return nil, pyValueError("non-finite number at " + at)
			}
		}
	}
	return clean, nil
}

// Type checks one payload session and its messages, ValueError naming the offending field
func normalizeImportSession(raw *pyjson.Object, sessionID string, messages []any) (*pyjson.Object, error) {
	clean := raw.Copy()
	clean.Set("id", sessionID)
	modelConfig, _ := clean.Get("model_config")
	normalizedConfig, err := importJSONObjectOrNone(modelConfig, "model_config")
	if err != nil {
		return nil, err
	}
	clean.Set("model_config", normalizedConfig)
	for _, field := range append([]string{"parent_session_id"}, importSessionTextFields...) {
		value, _ := clean.Get(field)
		text, err := importTextOrNone(value, field)
		if err != nil {
			return nil, err
		}
		clean.Set(field, text)
	}
	if startedAt, _ := clean.Get("started_at"); startedAt != nil {
		if _, ok := pydatetime.CoerceEpoch(startedAt, "", "started_at"); !ok {
			return nil, pyValueError("session.started_at must be a supported Unix timestamp")
		}
	}
	for _, field := range importFloatCols {
		if value, _ := clean.Get(field); value != nil {
			if f, ok := value.(float64); ok && (math.IsNaN(f) || math.IsInf(f, 0)) {
				return nil, pyValueError("non-finite number at session." + field)
			}
		}
	}
	for _, field := range importIntCols {
		if value, _ := clean.Get(field); value != nil {
			if n, ok := value.(*big.Int); ok && !withinSQLiteInt(n) {
				return nil, pyValueError(field + " is outside SQLite's integer range")
			}
		}
	}
	cleanMessages := make([]any, 0, len(messages))
	for index, message := range messages {
		cleanMessage, err := normalizeImportMessage(message.(*pyjson.Object), index)
		if err != nil {
			return nil, err
		}
		cleanMessages = append(cleanMessages, cleanMessage)
	}
	return pyjson.ObjectOf("session", clean, "messages", cleanMessages), nil
}

type importTotals struct{ messages, bytes int }

// Size, shape and type validation of the whole payload, every rejected entry reported
func (d *SessionDB) validateImportPayload(sessions []any) ([]*pyjson.Object, []any, error) {
	normalized := []*pyjson.Object{}
	errs := []any{}
	seen := map[string]bool{}
	totals := &importTotals{}
	for index, raw := range sessions {
		sessionID := ""
		if obj, ok := raw.(*pyjson.Object); ok {
			id, _ := obj.Get("id")
			if pyTruthy(id) {
				sessionID = pystr.Strip(pyStr(id))
			}
		}
		item, err := d.validateImportSession(raw, sessionID, seen, totals)
		if err != nil {
			if !isPyValueError(err) {
				return nil, nil, err
			}
			entry := pyjson.ObjectOf("index", int64(index), "error", err.Error())
			if sessionID != "" {
				entry.Set("session_id", sessionID)
			}
			errs = append(errs, entry)
			continue
		}
		seen[sessionID] = true
		withIndex := pyjson.ObjectOf("index", int64(index))
		withIndex.Update(item)
		normalized = append(normalized, withIndex)
	}
	return normalized, errs, nil
}

// One payload session normalized, ValueError on rejection, totals counted before their limit check
func (d *SessionDB) validateImportSession(raw any, sessionID string, seen map[string]bool, totals *importTotals) (*pyjson.Object, error) {
	obj, ok := raw.(*pyjson.Object)
	if !ok {
		return nil, pyValueError("session must be an object")
	}
	if sessionID == "" {
		return nil, pyValueError("session id is required")
	}
	if seen[sessionID] {
		return nil, pyValueError("duplicate session id")
	}
	rawMessages, _ := obj.Get("messages")
	var messages []any
	if pyTruthy(rawMessages) {
		list, ok := rawMessages.([]any)
		if !ok {
			return nil, pyValueError("messages must be a list")
		}
		messages = list
	} else {
		messages = []any{}
	}
	if len(messages) > importMaxMessagesPerSession {
		return nil, pyValueError("messages exceeds the per-session import limit")
	}
	for _, msg := range messages {
		if _, ok := msg.(*pyjson.Object); !ok {
			return nil, pyValueError("messages must contain only objects")
		}
	}
	measured := pyjson.NewObject()
	for _, key := range obj.Keys {
		if key != "timings" {
			measured.Set(key, obj.Vals[key])
		}
	}
	text, err := pyDumps(measured, pyJSON{allowNaN: true, itemSep: ",", keySep: ":"})
	if err != nil {
		return nil, pyValueError("session must be JSON serializable")
	}
	sessionBytes, err := utf8StrictLen(text)
	if err != nil {
		return nil, err
	}
	if sessionBytes > importMaxSessionBytes {
		return nil, pyValueError("session exceeds the import size limit")
	}
	totals.bytes += sessionBytes
	if totals.bytes > importMaxTotalBytes {
		return nil, pyValueError("import exceeds the total size limit")
	}
	item, err := normalizeImportSession(obj, sessionID, messages)
	if err != nil {
		return nil, err
	}
	cleanMessages, _ := rowGet(item, "messages").([]any)
	totals.messages += len(cleanMessages)
	if totals.messages > importMaxTotalMessages {
		return nil, pyValueError("messages exceeds the total import limit")
	}
	return item, nil
}

// Byte length of the text under the strict utf-8 codec, a UnicodeEncodeError for a lone surrogate
func utf8StrictLen(text string) (int, error) {
	for i := 0; i < len(text); i++ {
		if pystr.SurrogateAt(text, i) {
			r := pystr.SurrogateRune(text, i)
			return 0, &stateTypedError{"UnicodeEncodeError", fmt.Sprintf("'utf-8' codec can't encode character '\\u%04x' in position %d: surrogates not allowed", r, len([]rune(text[:i])))}
		}
	}
	return len(text), nil
}

// Parsed JSON of a stored text column, the text itself when it does not parse
func jsonValueOrText(value any) any {
	text, ok := value.(string)
	if !ok {
		return value
	}
	parsed, err := pyjson.Loads(text)
	if err != nil {
		return value
	}
	return parsed
}

// Inserts one normalized session with its messages, archived rows restored archived and counters fixed after
func (d *SessionDB) importSessionRow(ctx context.Context, conn SQLiteConn, raw *pyjson.Object, messages any, sessionID string) error {
	startedRaw, _ := raw.Get("started_at")
	var startedAt any = now()
	if ts, ok := pydatetime.CoerceEpoch(startedRaw, sessionID, "started_at"); ok {
		startedAt = ts
	}
	source := "import"
	if v, _ := raw.Get("source"); pyTruthy(v) {
		source = pyStr(v)
	}
	promptRaw, _ := raw.Get("system_prompt")
	promptHash, err := storeSystemPrompt(ctx, conn, promptRaw)
	if err != nil {
		return err
	}
	flags := map[string]int64{}
	for _, col := range importFlagCols {
		value, _ := raw.Get(col)
		n, err := coerceOrInt(value, big.NewInt(0))
		if err != nil {
			return err
		}
		if n.Sign() != 0 {
			flags[col] = 1
		} else {
			flags[col] = 0
		}
	}
	flags["auto_archived"] &= flags["archived"]
	passthrough := func(col string) any { v, _ := raw.Get(col); return v }
	floatCol := func(col string) any { v, _ := raw.Get(col); return importFloatOrFinite(v) }
	intCols := map[string]int64{}
	for _, col := range importIntCols {
		value, _ := raw.Get(col)
		n, err := importIntOrBounded(value, 0)
		if err != nil {
			return err
		}
		intCols[col] = n
	}
	if _, err := conn.ExecContext(ctx, importSessionInsertSQL,
		sessionID, source, passthrough("user_id"), passthrough("model"), passthrough("model_config"),
		promptHash, startedAt, floatCol("ended_at"),
		passthrough("end_reason"), intCols["input_tokens"], intCols["output_tokens"],
		intCols["cache_read_tokens"], intCols["cache_write_tokens"],
		intCols["reasoning_tokens"], passthrough("cwd"), passthrough("git_branch"), passthrough("git_repo_root"),
		passthrough("billing_provider"), passthrough("billing_base_url"), passthrough("billing_mode"),
		floatCol("estimated_cost_usd"), floatCol("actual_cost_usd"), passthrough("cost_status"),
		passthrough("cost_source"), passthrough("pricing_version"), passthrough("title"),
		intCols["api_call_count"], flags["archived"], flags["auto_archived"], flags["pinned"], flags["hidden"]); err != nil {
		return err
	}
	list, _ := messages.([]any)
	sanitized := make([]map[string]any, 0, len(list))
	for _, item := range list {
		obj, ok := item.(*pyjson.Object)
		if !ok {
			return &stateTypedError{"TypeError", "message must be a dict"}
		}
		row := pyjson.Plain(obj).(map[string]any)
		for _, key := range importMessageJSONFields {
			row[key] = pyjson.Plain(jsonValueOrText(row[key]))
		}
		sanitized = append(sanitized, row)
	}
	live := []map[string]any{}
	archived := []map[string]any{}
	for _, msg := range sanitized {
		active, err := coerceOrInt(msg["active"], big.NewInt(1))
		if err != nil {
			return err
		}
		if active.Sign() != 0 {
			live = append(live, msg)
		} else {
			archived = append(archived, msg)
		}
	}
	if _, _, err := d.insertMessageRows(ctx, conn, sessionID, sanitized, false); err != nil {
		return err
	}
	for _, msg := range archived {
		compacted, err := coerceOrInt(msg["compacted"], big.NewInt(0))
		if err != nil {
			return err
		}
		flag := int64(0)
		if compacted.Sign() != 0 {
			flag = 1
		}
		if _, err := conn.ExecContext(ctx, "UPDATE messages SET active = 0, compacted = ? WHERE id = ?", flag, msg["_row_id"]); err != nil {
			return err
		}
	}
	if err := pruneShadowedCheckpoints(ctx, conn, sessionID, live); err != nil {
		return err
	}
	toolCalls := 0
	for _, msg := range live {
		toolCalls += toolCallsCount(parseToolCalls(msg["tool_calls"]))
	}
	_, err = conn.ExecContext(ctx, "UPDATE sessions SET message_count = ?, tool_call_count = ? WHERE id = ?", int64(len(live)), int64(toolCalls), sessionID)
	return err
}

// Reattaches imported children to parents that exist without creating a cycle, returning the detached count
func attachImportParents(ctx context.Context, conn SQLiteConn, parentUpdates [][2]string) (int, error) {
	parentByChild := map[string]string{}
	for _, link := range parentUpdates {
		parentByChild[link[0]] = link[1]
	}
	wouldCreateCycle := func(sessionID, parentID string) (bool, error) {
		seen := map[string]bool{sessionID: true}
		current := parentID
		for current != "" {
			if seen[current] {
				return true, nil
			}
			seen[current] = true
			if next, ok := parentByChild[current]; ok {
				current = next
				continue
			}
			row, err := connRow(ctx, conn, "SELECT parent_session_id FROM sessions WHERE id = ? LIMIT 1", current)
			if err != nil {
				return false, err
			}
			if row == nil {
				return false, nil
			}
			current = ""
			if parent, ok := row["parent_session_id"].(string); ok {
				current = parent
			}
		}
		return false, nil
	}
	detached := 0
	for _, link := range parentUpdates {
		sessionID, parentID := link[0], link[1]
		exists, err := connRow(ctx, conn, "SELECT 1 FROM sessions WHERE id = ? LIMIT 1", parentID)
		if err != nil {
			return 0, err
		}
		cycle := false
		if exists != nil {
			if cycle, err = wouldCreateCycle(sessionID, parentID); err != nil {
				return 0, err
			}
		}
		if exists != nil && !cycle {
			if _, err := conn.ExecContext(ctx, "UPDATE sessions SET parent_session_id = ? WHERE id = ?", parentID, sessionID); err != nil {
				return 0, err
			}
		} else {
			delete(parentByChild, sessionID)
			detached++
		}
	}
	return detached, nil
}

// Imports exported sessions, skipping existing ids, refusing deleted ids by name and detaching foreign parents
func (d *SessionDB) ImportSessions(ctx context.Context, sessions []any) (*pyjson.Object, error) {
	if sessions == nil {
		return nil, pyValueError("sessions must be a list")
	}
	if len(sessions) > importMaxSessions {
		return nil, pyValueError(fmt.Sprintf("sessions must contain at most %d entries", importMaxSessions))
	}
	normalized, errs, err := d.validateImportPayload(sessions)
	if err != nil {
		return nil, err
	}
	if len(errs) > 0 {
		return pyjson.ObjectOf("ok", false, "imported", int64(0), "skipped", int64(0), "detached", int64(0), "errors", errs), nil
	}
	var result *pyjson.Object
	err = d.executeWrite(ctx, func(conn SQLiteConn) error {
		ids := make([]string, 0, len(normalized))
		for _, item := range normalized {
			id, _ := rowGet(item, "session").(*pyjson.Object).Get("id")
			ids = append(ids, pystr.Strip(pyStr(pyOr(id, ""))))
		}
		deleted := []any{}
		for _, sid := range ids {
			retired, err := RetiredIn(ctx, conn, sid)
			if err != nil {
				return err
			}
			if retired {
				deleted = append(deleted, fmt.Sprintf("session %s was deleted here; a deleted session is not restored", sid))
			}
		}
		if len(deleted) > 0 {
			result = pyjson.ObjectOf("ok", false, "imported", int64(0), "skipped", int64(0), "detached", int64(0), "errors", deleted)
			return nil
		}
		imported, skipped := []string{}, []string{}
		var parentUpdates [][2]string
		for _, item := range normalized {
			raw := rowGet(item, "session").(*pyjson.Object)
			id, _ := raw.Get("id")
			sessionID := pystr.Strip(pyStr(pyOr(id, "")))
			exists, err := connRow(ctx, conn, "SELECT 1 FROM sessions WHERE id = ? LIMIT 1", sessionID)
			if err != nil {
				return err
			}
			if exists != nil {
				skipped = append(skipped, sessionID)
				continue
			}
			if err := d.importSessionRow(ctx, conn, raw, rowGet(item, "messages"), sessionID); err != nil {
				return err
			}
			parentRaw, _ := raw.Get("parent_session_id")
			if parentID := pystr.Strip(pyStr(pyOr(parentRaw, ""))); parentID != "" {
				parentUpdates = append(parentUpdates, [2]string{sessionID, parentID})
			}
			imported = append(imported, sessionID)
		}
		detached, err := attachImportParents(ctx, conn, parentUpdates)
		if err != nil {
			return err
		}
		result = pyjson.ObjectOf("ok", true, "imported", int64(len(imported)), "skipped", int64(len(skipped)),
			"detached", int64(detached), "imported_ids", anyStrings(imported), "skipped_ids", anyStrings(skipped), "errors", []any{})
		return nil
	}, nil)
	if err != nil {
		return nil, err
	}
	return result, nil
}
