package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

const (
	importMaxSessions           = 500
	importMaxMessagesPerSession = 10_000
	importMaxTotalMessages      = 50_000
	importMaxSessionBytes       = 5 * 1024 * 1024
	importMaxTotalBytes         = 25 * 1024 * 1024
)

var ErrInvalidImport = errors.New("invalid session import")

var importSessionTextFields = []string{
	"source", "user_id", "model", "system_prompt", "end_reason", "cwd", "git_branch", "git_repo_root",
	"billing_provider", "billing_base_url", "billing_mode", "cost_status", "cost_source", "pricing_version", "title",
}

var importMessageTextFields = []string{
	"tool_call_id", "tool_name", "effect_disposition", "finish_reason",
	"reasoning", "reasoning_content", "platform_message_id", "message_id",
}

var importMessageJSONFields = []string{
	"reasoning_details", "codex_reasoning_items", "codex_message_items", "absorbed_message_uids", "tool_call_uids",
}

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
                           :id, :source, :user_id, :model, :model_config,
                           NULL, :system_prompt_hash, NULL, :started_at, :ended_at,
                           :end_reason, 0, 0, :input_tokens, :output_tokens,
                           :cache_read_tokens, :cache_write_tokens,
                           :reasoning_tokens, :cwd, :git_branch, :git_repo_root,
                           :billing_provider, :billing_base_url, :billing_mode,
                           :estimated_cost_usd, :actual_cost_usd, :cost_status,
                           :cost_source, :pricing_version, :title,
                           :api_call_count, :archived, :auto_archived, :pinned, :hidden
                       )`

var importPassthroughTextCols = []string{
	"user_id", "model", "end_reason", "cwd", "git_branch", "git_repo_root", "billing_provider",
	"billing_base_url", "billing_mode", "cost_status", "cost_source", "pricing_version", "title",
}

var importIntCols = []string{
	"input_tokens", "output_tokens", "cache_read_tokens", "cache_write_tokens", "reasoning_tokens", "api_call_count",
}

var importFloatCols = []string{"ended_at", "estimated_cost_usd", "actual_cost_usd"}

var importFlagCols = []string{"archived", "auto_archived", "pinned", "hidden"}

type ImportProblem struct {
	Index     int    `json:"index"`
	Error     string `json:"error"`
	SessionID string `json:"session_id,omitempty"`
}

type ImportItem struct {
	Index       int
	ID          string
	ParentID    sql.Null[string]
	node        *jnode
	texts       map[string]sql.Null[string]
	modelConfig sql.Null[string]
	messages    []importMessage
}

type importMessage struct {
	node       *jnode
	tokenCount sql.Null[int64]
}

type ImportResult struct {
	OK          bool
	Imported    int
	Skipped     int
	Detached    int
	ImportedIDs []string
	SkippedIDs  []string
	Problems    []ImportProblem
	Refusals    []string
}

func ValidateImportPayload(rows []json.RawMessage) ([]ImportItem, []ImportProblem) {
	var items []ImportItem
	var problems []ImportProblem
	seen := map[string]bool{}
	totalMessages, totalBytes := 0, 0
	for index, raw := range rows {
		node, err := parseImportJSON(raw)
		if err != nil {
			node = nil
		}
		id := ""
		if node != nil && node.kind == '{' {
			id = importSessionID(node.member("id"))
		}
		item, problem := validateImportSession(node, id, seen, &totalMessages, &totalBytes)
		if problem != "" {
			problems = append(problems, ImportProblem{Index: index, Error: problem, SessionID: id})
			continue
		}
		seen[id] = true
		item.Index = index
		items = append(items, item)
	}
	return items, problems
}

func importSessionID(n *jnode) string {
	if !n.truthy() {
		return ""
	}
	switch n.kind {
	case '"':
		return strings.TrimSpace(n.text())
	case '0':
		return strings.TrimSpace(importNumberText(n))
	case 't':
		return "True"
	}
	return strings.TrimSpace(string(n.raw))
}

func validateImportSession(node *jnode, id string, seen map[string]bool, totalMessages, totalBytes *int) (ImportItem, string) {
	if node == nil || node.kind != '{' {
		return ImportItem{}, "session must be an object"
	}
	if id == "" {
		return ImportItem{}, "session id is required"
	}
	if seen[id] {
		return ImportItem{}, "duplicate session id"
	}
	var messages []*jnode
	if m := node.member("messages"); m.truthy() {
		if m.kind != '[' {
			return ImportItem{}, "messages must be a list"
		}
		messages = m.kids
	}
	if len(messages) > importMaxMessagesPerSession {
		return ImportItem{}, "messages exceeds the per-session import limit"
	}
	for _, m := range messages {
		if m.kind != '{' {
			return ImportItem{}, "messages must contain only objects"
		}
	}
	var measured strings.Builder
	if err := encodeImportJSON(&measured, node, importJSONStyle{skip: "timings"}, true); err != nil {
		return ImportItem{}, "session must be JSON serializable"
	}
	size := measured.Len()
	if size > importMaxSessionBytes {
		return ImportItem{}, "session exceeds the import size limit"
	}
	*totalBytes += size
	if *totalBytes > importMaxTotalBytes {
		return ImportItem{}, "import exceeds the total size limit"
	}
	item, problem := normalizeImportSession(node, id, messages)
	if problem != "" {
		return ImportItem{}, problem
	}
	*totalMessages += len(item.messages)
	if *totalMessages > importMaxTotalMessages {
		return ImportItem{}, "messages exceeds the total import limit"
	}
	return item, ""
}

func importModelConfig(n *jnode) (sql.Null[string], string) {
	const path = "session.model_config"
	if n.isNull() {
		return sql.Null[string]{}, ""
	}
	switch n.kind {
	case '"':
		value := n.text()
		parsed, err := parseImportJSON([]byte(value))
		if err != nil {
			return sql.Null[string]{}, "model_config must be valid JSON"
		}
		if bad, found := importNonFinitePath(parsed, path); found {
			return sql.Null[string]{}, "non-finite number at " + bad
		}
		if parsed.kind != '{' {
			return sql.Null[string]{}, "model_config must be a JSON object"
		}
		return sql.Null[string]{V: value, Valid: true}, ""
	case '{':
		if _, found := importNonFinitePath(n, path); found {
			return sql.Null[string]{}, "non-finite number at " + path
		}
		var b strings.Builder
		if err := encodeImportJSON(&b, n, importJSONStyle{spaced: true, ascii: true}, false); err != nil {
			return sql.Null[string]{}, "model_config must be JSON serializable"
		}
		return sql.Null[string]{V: b.String(), Valid: true}, ""
	}
	return sql.Null[string]{}, "model_config must be a JSON object"
}

func importText(n *jnode, field string) (sql.Null[string], string) {
	if n.isNull() {
		return sql.Null[string]{}, ""
	}
	if n.kind != '"' {
		return sql.Null[string]{}, field + " must be a string"
	}
	return sql.Null[string]{V: n.text(), Valid: true}, ""
}

func normalizeImportSession(node *jnode, id string, messages []*jnode) (ImportItem, string) {
	item := ImportItem{ID: id, node: node, texts: map[string]sql.Null[string]{}}
	var problem string
	if item.modelConfig, problem = importModelConfig(node.member("model_config")); problem != "" {
		return ImportItem{}, problem
	}
	for _, field := range append([]string{"parent_session_id"}, importSessionTextFields...) {
		value, problem := importText(node.member(field), field)
		if problem != "" {
			return ImportItem{}, problem
		}
		item.texts[field] = value
	}
	item.ParentID = item.texts["parent_session_id"]
	if started := node.member("started_at"); !started.isNull() {
		if _, ok := importEpochOf(started, "started_at"); !ok {
			return ImportItem{}, "session.started_at must be a supported Unix timestamp"
		}
	}
	for _, field := range importFloatCols {
		if v := node.member(field); v != nil && v.nonFinite() {
			return ImportItem{}, "non-finite number at session." + field
		}
	}
	for _, field := range importIntCols {
		if v := node.member(field); v != nil && v.kind == '0' && !v.float {
			if _, outcome := importIntOf(v); outcome == importIntOutOfRange {
				return ImportItem{}, field + " is outside SQLite's integer range"
			}
		}
	}
	item.messages = make([]importMessage, 0, len(messages))
	for i, m := range messages {
		msg, problem := normalizeImportMessage(m, i)
		if problem != "" {
			return ImportItem{}, problem
		}
		item.messages = append(item.messages, msg)
	}
	return item, ""
}

func normalizeImportMessage(n *jnode, index int) (importMessage, string) {
	if role := n.member("role"); role == nil || role.kind != '"' || role.text() == "" {
		return importMessage{}, fmt.Sprintf("messages[%d].role must be a non-empty string", index)
	}
	for _, field := range importMessageTextFields {
		if _, problem := importText(n.member(field), field); problem != "" {
			return importMessage{}, problem
		}
	}
	msg := importMessage{node: n}
	if tc := n.member("token_count"); !tc.isNull() {
		v, outcome := importIntOf(tc)
		switch outcome {
		case importIntInvalid:
			return importMessage{}, "token_count must be an integer"
		case importIntOutOfRange:
			return importMessage{}, "token_count is outside SQLite's integer range"
		}
		msg.tokenCount = sql.Null[int64]{V: v, Valid: true}
	}
	path := fmt.Sprintf("session.messages[%d]", index)
	if ts := n.member("timestamp"); !ts.isNull() {
		if _, ok := importEpochOf(ts, "messages[].timestamp"); !ok {
			return importMessage{}, path + ".timestamp must be a supported Unix timestamp"
		}
	}
	if content := n.member("content"); content != nil && content.kind == '0' {
		if !content.float {
			if _, outcome := importIntOf(content); outcome == importIntOutOfRange {
				return importMessage{}, path + ".content is outside SQLite's integer range"
			}
		} else if content.nonFinite() {
			return importMessage{}, "non-finite number at " + path + ".content"
		}
	}
	for _, field := range importMessageJSONFields {
		v := n.member(field)
		if v.isNull() {
			continue
		}
		target, root := v, path+"."+field
		if v.kind == '"' {
			parsed, err := parseImportJSON([]byte(v.text()))
			if err != nil {
				continue
			}
			target = parsed
		}
		if bad, found := importNonFinitePath(target, root); found {
			return importMessage{}, "non-finite number at " + bad
		}
	}
	return msg, ""
}

func importFlag(n *jnode, fallback bool) bool {
	if n.isNull() {
		return fallback
	}
	v, outcome := importIntOf(n)
	switch outcome {
	case importIntValid:
		return v != 0
	case importIntOutOfRange:
		return true
	}
	return fallback
}

func importBoundedInt(n *jnode) int64 {
	if n.isNull() {
		return 0
	}
	if v, outcome := importIntOf(n); outcome == importIntValid {
		return v
	}
	return 0
}

func importFiniteFloat(n *jnode) any {
	if n.isNull() {
		return nil
	}
	f, ok := importFloatOf(n)
	if !ok || math.IsNaN(f) || math.IsInf(f, 0) {
		return nil
	}
	return f
}

func importJSONFieldValue(n *jnode) json.RawMessage {
	if n.isNull() {
		return nil
	}
	if n.kind == '"' {
		inner := n.text()
		if parsed, err := parseImportJSON([]byte(inner)); err == nil && !parsed.isNull() {
			return json.RawMessage(strings.TrimSpace(inner))
		}
		if strings.TrimSpace(inner) == "null" {
			return nil
		}
	}
	return json.RawMessage(n.raw)
}

func importNullText(n *jnode) sql.Null[string] {
	if n == nil || n.kind != '"' {
		return sql.Null[string]{}
	}
	return sql.Null[string]{V: n.text(), Valid: true}
}

func importStrings(n *jnode) []string {
	if n == nil || n.kind != '[' {
		return nil
	}
	var out []string
	for _, kid := range n.kids {
		if kid.kind == '"' {
			out = append(out, kid.text())
		}
	}
	return out
}

func (m importMessage) message() *Message {
	n := m.node
	msg := &Message{Role: n.member("role").text(), TokenCount: m.tokenCount}
	if content := n.member("content"); content != nil {
		msg.Content = JSONContent(json.RawMessage(content.raw))
	}
	msg.ToolCallID = importNullText(n.member("tool_call_id"))
	msg.ToolName = importNullText(n.member("tool_name"))
	msg.EffectDisposition = importNullText(n.member("effect_disposition"))
	msg.FinishReason = importNullText(n.member("finish_reason"))
	msg.Reasoning = importNullText(n.member("reasoning"))
	msg.ReasoningContent = importNullText(n.member("reasoning_content"))
	msg.APIContent = importNullText(n.member("api_content"))
	msg.DisplayKind = importNullText(n.member("display_kind"))
	msg.PlatformMessageID = importNullText(n.member("platform_message_id"))
	if !n.member("platform_message_id").truthy() {
		msg.PlatformMessageID = importNullText(n.member("message_id"))
	}
	if tc := n.member("tool_calls"); !tc.isNull() {
		msg.ToolCalls = json.RawMessage(tc.raw)
	}
	msg.ReasoningDetails = importJSONFieldValue(n.member("reasoning_details"))
	msg.CodexReasoningItems = importJSONFieldValue(n.member("codex_reasoning_items"))
	msg.CodexMessageItems = importJSONFieldValue(n.member("codex_message_items"))
	if dm := n.member("display_metadata"); !dm.isNull() {
		msg.DisplayMetadata = json.RawMessage(dm.raw)
	}
	if du := n.member("data_use"); du.truthy() {
		msg.DataUse = importStrings(du)
	}
	if ts := n.member("timestamp"); !ts.isNull() {
		if f, ok := importEpochOf(ts, "messages[].timestamp"); ok {
			msg.Timestamp = sql.Null[float64]{V: f, Valid: true}
		}
	}
	msg.Observed = n.member("observed").truthy()
	msg.CompressedSummary = n.member("_compressed_summary").truthy()
	if uid := n.member("message_uid"); uid != nil && uid.kind == '"' {
		msg.UID = uid.text()
	}
	if uid := n.member("_tool_call_uid"); uid != nil && uid.kind == '"' {
		msg.ToolCallUID = uid.text()
	}
	if uids := n.member("_tool_call_uids"); uids != nil && uids.kind == '{' {
		msg.ToolCallUIDs = DecodeToolCallUIDs(string(uids.raw))
	}
	msg.AbsorbedUIDs = UIDList(importStrings(n.member("_absorbed_message_uids")))
	return msg
}

func (s *Store) importSessionRow(ctx context.Context, c Conn, item ImportItem, sessionID string) error {
	node := item.node
	startedAt := nowEpoch()
	if f, ok := importEpochOf(node.member("started_at"), "started_at"); ok {
		startedAt = f
	}
	source := "import"
	if v := item.texts["source"]; v.Valid && v.V != "" {
		source = v.V
	}
	var prompt *string
	if v := item.texts["system_prompt"]; v.Valid {
		prompt = &v.V
	}
	promptHash, err := storeSystemPrompt(ctx, c, prompt)
	if err != nil {
		return err
	}
	flags := map[string]int{}
	for _, col := range importFlagCols {
		flags[col] = boolInt(importFlag(node.member(col), false))
	}
	flags["auto_archived"] &= flags["archived"]
	args := []any{sql.Named("id", sessionID), sql.Named("source", source), sql.Named("system_prompt_hash", promptHash),
		sql.Named("started_at", startedAt), sql.Named("model_config", nullText(item.modelConfig))}
	for _, col := range importFlagCols {
		args = append(args, sql.Named(col, flags[col]))
	}
	for _, col := range importPassthroughTextCols {
		args = append(args, sql.Named(col, nullText(item.texts[col])))
	}
	for _, col := range importFloatCols {
		args = append(args, sql.Named(col, importFiniteFloat(node.member(col))))
	}
	for _, col := range importIntCols {
		args = append(args, sql.Named(col, importBoundedInt(node.member(col))))
	}
	if _, err := c.ExecContext(ctx, importSessionInsertSQL, args...); err != nil {
		return err
	}
	type archivedRow struct {
		msg       *Message
		compacted bool
	}
	all := make([]*Message, 0, len(item.messages))
	var live []*Message
	var archived []archivedRow
	for _, m := range item.messages {
		msg := m.message()
		all = append(all, msg)
		if importFlag(m.node.member("active"), true) {
			live = append(live, msg)
		} else {
			archived = append(archived, archivedRow{msg, importFlag(m.node.member("compacted"), false)})
		}
	}
	if _, _, err := s.insertMessageRows(ctx, c, sessionID, all, false); err != nil {
		return err
	}
	for _, a := range archived {
		if _, err := c.ExecContext(ctx, "UPDATE messages SET active = 0, compacted = ? WHERE id = ?", boolInt(a.compacted), a.msg.RowID); err != nil {
			return err
		}
	}
	if err := s.pruneShadowedCheckpoints(ctx, c, sessionID, live); err != nil {
		return err
	}
	toolCalls := 0
	for _, msg := range live {
		toolCalls += toolCallsCount(parseToolCalls(msg.ToolCalls))
	}
	_, err = c.ExecContext(ctx, "UPDATE sessions SET message_count = ?, tool_call_count = ? WHERE id = ?", len(live), toolCalls, sessionID)
	return err
}

func attachImportParents(ctx context.Context, c Conn, updates [][2]string) (int, error) {
	parentByChild := map[string]string{}
	for _, u := range updates {
		parentByChild[u[0]] = u[1]
	}
	wouldCycle := func(sessionID, parentID string) (bool, error) {
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
			var parent sql.NullString
			err := c.QueryRowContext(ctx, "SELECT parent_session_id FROM sessions WHERE id = ? LIMIT 1", current).Scan(&parent)
			if errors.Is(err, sql.ErrNoRows) {
				return false, nil
			}
			if err != nil {
				return false, err
			}
			current = parent.String
		}
		return false, nil
	}
	detached := 0
	for _, u := range updates {
		exists, err := sessionExists(ctx, c, u[1])
		if err != nil {
			return 0, err
		}
		cycle := false
		if exists {
			if cycle, err = wouldCycle(u[0], u[1]); err != nil {
				return 0, err
			}
		}
		if exists && !cycle {
			if _, err := c.ExecContext(ctx, "UPDATE sessions SET parent_session_id = ? WHERE id = ?", u[1], u[0]); err != nil {
				return 0, err
			}
			continue
		}
		delete(parentByChild, u[0])
		detached++
	}
	return detached, nil
}

func (s *Store) ImportSessions(ctx context.Context, rows []json.RawMessage) (ImportResult, error) {
	if len(rows) > importMaxSessions {
		return ImportResult{}, fmt.Errorf("%w: sessions must contain at most %d entries", ErrInvalidImport, importMaxSessions)
	}
	normalized, problems := ValidateImportPayload(rows)
	if len(problems) > 0 {
		return ImportResult{Problems: problems}, nil
	}
	var result ImportResult
	err := s.Write(ctx, WritePatience, func(c Conn) error {
		result = ImportResult{}
		var deleted []string
		for _, item := range normalized {
			retired, err := retiredIn(ctx, c, item.ID)
			if err != nil {
				return err
			}
			if retired {
				deleted = append(deleted, "session "+item.ID+" was deleted here; a deleted session is not restored")
			}
		}
		if len(deleted) > 0 {
			result.Refusals = deleted
			return nil
		}
		var parents [][2]string
		for _, item := range normalized {
			exists, err := sessionExists(ctx, c, item.ID)
			if err != nil {
				return err
			}
			if exists {
				result.SkippedIDs = append(result.SkippedIDs, item.ID)
				continue
			}
			if err := s.importSessionRow(ctx, c, item, item.ID); err != nil {
				return err
			}
			if parent := strings.TrimSpace(item.ParentID.V); parent != "" {
				parents = append(parents, [2]string{item.ID, parent})
			}
			result.ImportedIDs = append(result.ImportedIDs, item.ID)
		}
		detached, err := attachImportParents(ctx, c, parents)
		if err != nil {
			return err
		}
		result.OK, result.Imported, result.Skipped, result.Detached = true, len(result.ImportedIDs), len(result.SkippedIDs), detached
		return nil
	})
	if err != nil {
		return ImportResult{}, err
	}
	return result, nil
}

type ForeignOrigin struct {
	Tool             string
	Path             string
	ForeignSessionID string
}

type ForeignImport struct {
	SessionID       string
	AlreadyImported bool
}

func findForeignImportIn(ctx context.Context, c Conn, origin ForeignOrigin) (string, bool, error) {
	rows, err := c.QueryContext(ctx, "SELECT id, origin_json FROM sessions WHERE source = ? AND origin_json IS NOT NULL", origin.Tool)
	if err != nil {
		return "", false, err
	}
	type candidate struct{ id, origin string }
	var found []candidate
	for rows.Next() {
		var cand candidate
		var raw sql.NullString
		if err := rows.Scan(&cand.id, &raw); err != nil {
			rows.Close()
			return "", false, err
		}
		cand.origin = raw.String
		found = append(found, cand)
	}
	if err := rowsDone(rows); err != nil {
		return "", false, err
	}
	for _, cand := range found {
		members, _ := objectMembers(json.RawMessage(cand.origin))
		importedRaw, _ := memberValue(members, "imported_from")
		imported, _ := objectMembers(importedRaw)
		tool, _ := memberValue(imported, "tool")
		if !jsonIsString(tool) || jsonString(tool) != origin.Tool {
			continue
		}
		foreignID, _ := memberValue(imported, "foreign_session_id")
		path, _ := memberValue(imported, "path")
		if origin.ForeignSessionID != "" && jsonIsString(foreignID) && jsonString(foreignID) == origin.ForeignSessionID {
			return cand.id, true, nil
		}
		if origin.ForeignSessionID == "" && jsonIsString(path) && jsonString(path) == origin.Path {
			return cand.id, true, nil
		}
	}
	return "", false, nil
}

func (s *Store) FindForeignImport(ctx context.Context, origin ForeignOrigin) (string, bool, error) {
	var id string
	var ok bool
	err := s.Read(ctx, func(c Conn) error {
		var err error
		id, ok, err = findForeignImportIn(ctx, c, origin)
		return err
	})
	return id, ok, err
}

func (s *Store) ImportForeignHistory(ctx context.Context, origin ForeignOrigin, messages []json.RawMessage, title, cwd, profile string) (ForeignImport, error) {
	sessionID, err := NewSessionID(time.Now(), 12)
	if err != nil {
		return ForeignImport{}, err
	}
	entry := []ConfigMember{{"id", sessionID}, {"source", origin.Tool}, {"title", title}, {"cwd", emptyNull(cwd)},
		{"messages", json.RawMessage(rawArray(messages))}}
	payload, err := objectJSON(entry...)
	if err != nil {
		return ForeignImport{}, err
	}
	normalized, problems := ValidateImportPayload([]json.RawMessage{payload})
	if len(problems) > 0 {
		return ForeignImport{}, fmt.Errorf("%w: %s", ErrInvalidImport, problems[0].Error)
	}
	foreignID := any(origin.ForeignSessionID)
	if origin.ForeignSessionID == "" {
		foreignID = nil
	}
	inner, err := objectJSON(ConfigMember{"tool", origin.Tool}, ConfigMember{"path", origin.Path}, ConfigMember{"foreign_session_id", foreignID})
	if err != nil {
		return ForeignImport{}, err
	}
	originJSON, err := objectJSON(ConfigMember{"imported_from", inner})
	if err != nil {
		return ForeignImport{}, err
	}
	var out ForeignImport
	err = s.Write(ctx, WritePatience, func(c Conn) error {
		existing, found, err := findForeignImportIn(ctx, c, origin)
		if err != nil {
			return err
		}
		if found {
			out = ForeignImport{SessionID: existing, AlreadyImported: true}
			return nil
		}
		item := normalized[0]
		texts := make(map[string]sql.Null[string], len(item.texts))
		for k, v := range item.texts {
			texts[k] = v
		}
		item.texts = texts
		var one int
		err = c.QueryRowContext(ctx, "SELECT 1 FROM sessions WHERE title = ?", title).Scan(&one)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil {
			item.texts["title"] = sql.Null[string]{V: title + " (" + sessionID[max(0, len(sessionID)-12):] + ")", Valid: true}
		}
		if err := s.importSessionRow(ctx, c, item, sessionID); err != nil {
			return err
		}
		if _, err := c.ExecContext(ctx, "UPDATE sessions SET origin_json = ?, profile_name = ? WHERE id = ?", string(originJSON), emptyNull(profile), sessionID); err != nil {
			return err
		}
		out = ForeignImport{SessionID: sessionID}
		return nil
	})
	return out, err
}
