package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"
)

var sidecarKeys = []string{"_usage_anchor", "_proactive_prune_rearm_tokens", "_anthropic_rejected_thinking"}

func emptyPayload(payload json.RawMessage) error {
	members, ok := objectMembers(payload)
	if !ok || len(members) > 0 {
		return runtimeErr(ReasonInvalidParams)
	}
	return nil
}

func workerContext(ctx context.Context, s *Store, c Conn, sessionID string, payload json.RawMessage) (json.RawMessage, error) {
	if err := emptyPayload(payload); err != nil {
		return nil, err
	}
	session, _, err := requireSessionObject(ctx, c, sessionID)
	if err != nil {
		return nil, err
	}
	w := &wireObject{}
	w.set("session", session)
	return w.raw()
}

func workerPrompt(ctx context.Context, s *Store, c Conn, sessionID string, payload json.RawMessage) (json.RawMessage, error) {
	members, keys, err := payloadKeys(payload)
	if err != nil {
		return nil, err
	}
	v, _ := memberValue(members, "system_prompt")
	if !exactKeys(keys, "system_prompt") || !(jsonIsNull(v) || jsonIsString(v)) {
		return nil, runtimeErr(ReasonInvalidParams)
	}
	var prompt *string
	if jsonIsString(v) {
		text := jsonString(v)
		prompt = &text
	}
	hash, err := storeSystemPrompt(ctx, c, prompt)
	if err != nil {
		return nil, err
	}
	if _, err := c.ExecContext(ctx, "UPDATE sessions SET system_prompt_hash=?, system_prompt=NULL WHERE id=?", hash, sessionID); err != nil {
		return nil, err
	}
	if err := deleteUnreferencedSystemPrompts(ctx, c); err != nil {
		return nil, err
	}
	return valueResult(nil)
}

func patchMembers(raw json.RawMessage) []ConfigMember {
	members, _ := objectMembers(raw)
	out := make([]ConfigMember, 0, len(members))
	for _, m := range members {
		if jsonIsNull(m.value) {
			out = append(out, ConfigMember{Key: m.key})
			continue
		}
		out = append(out, ConfigMember{Key: m.key, Value: m.value})
	}
	return out
}

func sidecarPatch(raw json.RawMessage) bool {
	members, ok := objectMembers(raw)
	if !ok {
		return false
	}
	for _, m := range members {
		if !containsString(sidecarKeys, m.key) {
			return false
		}
	}
	return true
}

func workerSidecars(ctx context.Context, s *Store, c Conn, sessionID string, payload json.RawMessage) (json.RawMessage, error) {
	members, keys, err := payloadKeys(payload)
	if err != nil {
		return nil, err
	}
	patch, _ := memberValue(members, "patch")
	if !exactKeys(keys, "patch") || !sidecarPatch(patch) {
		return nil, runtimeErr(ReasonInvalidParams)
	}
	merged, found, err := mergeModelConfigJSON(ctx, c, sessionID, patchMembers(patch))
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("session %s has no row to patch model_config on", sessionID)
	}
	if _, err := c.ExecContext(ctx, "UPDATE sessions SET model_config=? WHERE id=?", merged, sessionID); err != nil {
		return nil, err
	}
	return valueResult(nil)
}

func validToolPin(pin json.RawMessage) bool {
	if jsonIsNull(pin) {
		return true
	}
	if names, ok := arrayElements(pin); ok {
		for _, n := range names {
			if !jsonIsString(n) {
				return false
			}
			if l := len([]rune(jsonString(n))); l == 0 || l > 256 {
				return false
			}
		}
		return true
	}
	members, ok := objectMembers(pin)
	if !ok {
		return false
	}
	version, hasVersion := memberValue(members, "version")
	tools, hasTools := memberValue(members, "tools")
	if !hasVersion || !hasTools || !jsonIsString(version) {
		return false
	}
	if _, isList := arrayElements(tools); !isList {
		return false
	}
	for _, m := range members {
		if m.key != "version" && m.key != "tools" && m.key != "delegation_backend" {
			return false
		}
	}
	if backend, has := memberValue(members, "delegation_backend"); has && (!jsonIsString(backend) || len([]rune(jsonString(backend))) > 64) {
		return false
	}
	return true
}

func workerToolNames(ctx context.Context, s *Store, c Conn, sessionID string, payload json.RawMessage) (json.RawMessage, error) {
	members, keys, err := payloadKeys(payload)
	if err != nil {
		return nil, err
	}
	pin, _ := memberValue(members, "tool_names")
	if !exactKeys(keys, "tool_names") || !validToolPin(pin) {
		return nil, runtimeErr(ReasonInvalidParams)
	}
	var encoded *string
	if !jsonIsNull(pin) {
		text, err := spacedJSON(pin)
		if err != nil {
			return nil, runtimeErr(ReasonInvalidParams)
		}
		if len(text) > workerPayloadLimit {
			return nil, runtimeErr(ReasonInvalidParams)
		}
		encoded = &text
	}
	hash, err := storeSystemPrompt(ctx, c, encoded)
	if err != nil {
		return nil, err
	}
	if _, err := c.ExecContext(ctx, "UPDATE sessions SET tool_names=? WHERE id=?", hash, sessionID); err != nil {
		return nil, err
	}
	if err := deleteUnreferencedSystemPrompts(ctx, c); err != nil {
		return nil, err
	}
	return valueResult(nil)
}

func firstText(ctx context.Context, c Conn, query string, args ...any) (any, error) {
	values, err := queryStrings(ctx, c, query, args...)
	if err != nil || len(values) == 0 {
		return nil, err
	}
	return values[0], nil
}

func workerTailRole(ctx context.Context, s *Store, c Conn, sessionID string, payload json.RawMessage) (json.RawMessage, error) {
	if err := emptyPayload(payload); err != nil {
		return nil, err
	}
	role, err := firstText(ctx, c, "SELECT role FROM messages WHERE session_id=? AND active=1 "+
		"AND role NOT IN ('session_meta','system') ORDER BY id DESC LIMIT 1", sessionID)
	if err != nil {
		return nil, err
	}
	return valueResult(role)
}

func workerRowRole(ctx context.Context, s *Store, c Conn, sessionID string, payload json.RawMessage) (json.RawMessage, error) {
	members, keys, err := payloadKeys(payload)
	if err != nil {
		return nil, err
	}
	raw, _ := memberValue(members, "row_id")
	rowID, ok := jsonInt(raw)
	if !exactKeys(keys, "row_id") || !ok {
		return nil, runtimeErr(ReasonInvalidParams)
	}
	role, err := firstText(ctx, c, "SELECT role FROM messages WHERE id=? AND session_id=? AND active=1", rowID, sessionID)
	if err != nil {
		return nil, err
	}
	return valueResult(role)
}

var workerIdentityFields = []string{"user_id", "session_key", "chat_id", "chat_type", "thread_id",
	"parent_session_id", "cwd", "profile_name", "git_repo_root", "origin_json", "display_name"}

var workerCreateFields = append(append([]string{}, workerIdentityFields...), "source", "model", "model_config", "system_prompt")

func workerLifecycleHandlers() map[string]WorkerHandler {
	return map[string]WorkerHandler{
		"session.title":               workerTitle,
		"session.title_source":        workerTitleSource,
		"session.next_title":          workerNextTitle,
		"session.activity":            workerActivity,
		"session.activity_clear":      workerActivityClear,
		"session.billing_route":       workerBillingRoute,
		"session.api_content":         workerAPIContent,
		"session.message_api_content": workerMessageAPIContent,
		"session.user_content":        workerUserContent,
		"session.create":              workerCreate,
		"session.end":                 workerEnd,
		"session.lifecycle":           workerLifecycle,
	}
}

func rowColumnText(ctx context.Context, c Conn, sessionID, column string) (sql.NullString, error) {
	var v sql.NullString
	err := c.QueryRowContext(ctx, "SELECT "+column+" FROM sessions WHERE id=?", sessionID).Scan(&v)
	return v, err
}

func workerCreate(ctx context.Context, s *Store, c Conn, sessionID string, payload json.RawMessage) (json.RawMessage, error) {
	members, keys, err := payloadKeys(payload)
	if err != nil {
		return nil, err
	}
	if !keys["source"] || !keysWithin(keys, workerCreateFields...) {
		return nil, runtimeErr(ReasonInvalidParams)
	}
	var rowSource string
	err = c.QueryRowContext(ctx, "SELECT source FROM sessions WHERE id=?", sessionID).Scan(&rowSource)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, runtimeErr(ReasonPermissionDenied)
	}
	if err != nil {
		return nil, err
	}
	source, _ := memberValue(members, "source")
	if !jsonIsString(source) || jsonString(source) != rowSource {
		return nil, runtimeErr(ReasonPermissionDenied)
	}
	for _, key := range workerIdentityFields {
		v, has := memberValue(members, key)
		if !has || jsonIsNull(v) {
			continue
		}
		stored, err := rowColumnText(ctx, c, sessionID, key)
		if err != nil {
			return nil, err
		}
		if !jsonIsString(v) || !stored.Valid || stored.String != jsonString(v) {
			return nil, runtimeErr(ReasonPermissionDenied)
		}
	}
	texts := map[string]*string{}
	for _, key := range workerCreateFields {
		if key == "model_config" {
			continue
		}
		v, has := memberValue(members, key)
		if !has || jsonIsNull(v) {
			continue
		}
		if !jsonIsString(v) {
			return nil, runtimeErr(ReasonInvalidParams)
		}
		text := jsonString(v)
		texts[key] = &text
	}
	config, hasConfig := memberValue(members, "model_config")
	var configMembersIn []configMember
	if hasConfig && !jsonIsNull(config) {
		var ok bool
		if configMembersIn, ok = objectMembers(config); !ok {
			return nil, runtimeErr(ReasonInvalidParams)
		}
		for _, m := range configMembersIn {
			if m.key != "max_iterations" && m.key != "max_tokens" && m.key != "reasoning_config" && m.key != "yolo_mode" {
				return nil, runtimeErr(ReasonInvalidParams)
			}
		}
	}
	n := NewSession{ID: sessionID, Source: rowSource, Model: texts["model"], SystemPrompt: texts["system_prompt"],
		UserID: texts["user_id"], SessionKey: texts["session_key"], ChatID: texts["chat_id"], ChatType: texts["chat_type"],
		ThreadID: texts["thread_id"], ParentSessionID: texts["parent_session_id"], Cwd: texts["cwd"], ProfileName: texts["profile_name"],
		GitRepoRoot: texts["git_repo_root"], OriginJSON: texts["origin_json"], DisplayName: texts["display_name"]}
	for _, m := range configMembersIn {
		n.ModelConfig = append(n.ModelConfig, ConfigMember{Key: m.key, Value: m.value})
	}
	if err := s.InsertSessionRowIn(ctx, c, n); err != nil {
		return nil, err
	}
	if len(configMembersIn) > 0 {
		text, err := encodeConfigMembers(configMembersIn)
		if err != nil {
			return nil, err
		}
		if _, err := c.ExecContext(ctx, "UPDATE sessions SET model_config=json_patch(model_config,?) WHERE id=? "+
			"AND json_type(model_config,'$._delegate_from') IS NOT NULL", text, sessionID); err != nil {
			return nil, err
		}
	}
	return valueResult(sessionID)
}

func workerEnd(ctx context.Context, s *Store, c Conn, sessionID string, payload json.RawMessage) (json.RawMessage, error) {
	members, keys, err := payloadKeys(payload)
	if err != nil {
		return nil, err
	}
	reason, _ := memberValue(members, "end_reason")
	if !exactKeys(keys, "end_reason") || !jsonIsString(reason) || jsonString(reason) == "" {
		return nil, runtimeErr(ReasonInvalidParams)
	}
	text := jsonString(reason)
	if _, err := EndAndBumpIn(ctx, c, "UPDATE sessions SET ended_at=?,end_reason=? WHERE id=? AND ended_at IS NULL",
		[]any{nowEpoch(), text, sessionID}, sessionID, text); err != nil {
		return nil, err
	}
	return valueResult(nil)
}

func workerLifecycle(ctx context.Context, s *Store, c Conn, sessionID string, payload json.RawMessage) (json.RawMessage, error) {
	if err := emptyPayload(payload); err != nil {
		return nil, err
	}
	var role, finish sql.NullString
	var hasCalls bool
	err := c.QueryRowContext(ctx, "SELECT role,tool_calls IS NOT NULL AS has_tool_calls,finish_reason "+
		"FROM messages WHERE session_id=? ORDER BY id DESC LIMIT 1", sessionID).Scan(&role, &hasCalls, &finish)
	if errors.Is(err, sql.ErrNoRows) {
		return valueResult("empty")
	}
	if err != nil {
		return nil, err
	}
	return valueResult(ClassifySessionStatus(role.String, hasCalls, finish.String))
}

func titleSourceValid(v json.RawMessage) bool {
	switch jsonString(v) {
	case "user", "derived", "llm":
		return jsonIsString(v)
	}
	return false
}

func titleRefusal(err error) bool {
	var long *TitleTooLongError
	var conflict *TitleConflictError
	return errors.As(err, &long) || errors.As(err, &conflict) || errors.Is(err, ErrCanonicalBotChatRename) || errors.Is(err, ErrInvalidTitleSource)
}

func workerTitle(ctx context.Context, s *Store, c Conn, sessionID string, payload json.RawMessage) (json.RawMessage, error) {
	members, keys, err := payloadKeys(payload)
	if err != nil {
		return nil, err
	}
	title, _ := memberValue(members, "title")
	source, _ := memberValue(members, "source")
	if !exactKeys(keys, "title", "source") || !titleSourceValid(source) || !(jsonIsNull(title) || jsonIsString(title)) {
		return nil, runtimeErr(ReasonInvalidParams)
	}
	affected, err := s.setSessionTitleIn(ctx, c, sessionID, jsonString(title), jsonString(source))
	if titleRefusal(err) {
		w := &wireObject{}
		w.set("value", false)
		w.set("error", "Title unavailable or invalid")
		w.set("title_candidate", title)
		return w.raw()
	}
	if err != nil {
		return nil, err
	}
	return valueResult(len(affected) > 0)
}

func workerTitleSource(ctx context.Context, s *Store, c Conn, sessionID string, payload json.RawMessage) (json.RawMessage, error) {
	members, keys, err := payloadKeys(payload)
	if err != nil {
		return nil, err
	}
	source, _ := memberValue(members, "source")
	if !exactKeys(keys, "source") || !titleSourceValid(source) {
		return nil, runtimeErr(ReasonInvalidParams)
	}
	n, err := rowsChanged(c.ExecContext(ctx, "UPDATE sessions SET title_source=? WHERE id=? AND title IS NOT NULL", jsonString(source), sessionID))
	if err != nil {
		return nil, err
	}
	return valueResult(n > 0)
}

func workerNextTitle(ctx context.Context, s *Store, c Conn, sessionID string, payload json.RawMessage) (json.RawMessage, error) {
	members, keys, err := payloadKeys(payload)
	if err != nil {
		return nil, err
	}
	raw, _ := memberValue(members, "base_title")
	if !exactKeys(keys, "base_title") || !jsonIsString(raw) {
		return nil, runtimeErr(ReasonInvalidParams)
	}
	candidate := jsonString(raw)
	title, err := rowColumnText(ctx, c, sessionID, "title")
	if err != nil {
		return nil, err
	}
	attempted, err := queryStrings(ctx, c, "SELECT 1 FROM worker_receipts r JOIN worker_executions w ON w.execution_id=r.execution_id "+
		"WHERE w.session_id=? AND w.status='running' "+
		"AND json_extract(r.result_json,'$.title_candidate')=? LIMIT 1", sessionID, candidate)
	if err != nil {
		return nil, err
	}
	if (!title.Valid || candidate != title.String) && len(attempted) == 0 {
		return nil, runtimeErr(ReasonPermissionDenied)
	}
	base := candidate
	if m := numberedTitle.FindStringSubmatch(candidate); m != nil {
		base = m[1]
	}
	titles, err := queryStrings(ctx, c, "SELECT title FROM sessions WHERE title=? OR title LIKE ? ESCAPE '\\'", base, EscapeLike(base)+" #%")
	if err != nil {
		return nil, err
	}
	if len(titles) == 0 {
		return valueResult(base)
	}
	highest := big.NewInt(1)
	for _, t := range titles {
		m := numberedTitle.FindStringSubmatch(t)
		if m == nil {
			continue
		}
		if n, ok := decimalNumber(m[2]); ok && n.Cmp(highest) > 0 {
			highest = n
		}
	}
	return valueResult(base + " #" + new(big.Int).Add(highest, big.NewInt(1)).String())
}

func workerActivity(ctx context.Context, s *Store, c Conn, sessionID string, payload json.RawMessage) (json.RawMessage, error) {
	members, keys, err := payloadKeys(payload)
	if err != nil {
		return nil, err
	}
	description, _ := memberValue(members, "description")
	provenance, _ := memberValue(members, "provenance")
	if !exactKeys(keys, "ts", "description", "provenance") || !(jsonIsNull(description) || jsonIsString(description)) ||
		!(jsonIsNull(provenance) || jsonIsString(provenance)) {
		return nil, runtimeErr(ReasonInvalidParams)
	}
	ts, _ := memberValue(members, "ts")
	when := nowEpoch()
	if !jsonIsNull(ts) {
		f, ok := jsonNumber(ts)
		if !ok {
			return nil, runtimeErr(ReasonInvalidParams)
		}
		when = f
	}
	if _, err := c.ExecContext(ctx, "UPDATE sessions SET last_activity_at=?,last_activity_description=?,last_activity_provenance=? "+
		"WHERE id=? AND (last_activity_at IS NULL OR last_activity_at<?)", when, BoundActivityDescription(jsonString(description)),
		string(NormalizeActivityProvenance(jsonString(provenance))), sessionID, when); err != nil {
		return nil, err
	}
	return valueResult(nil)
}

func workerActivityClear(ctx context.Context, s *Store, c Conn, sessionID string, payload json.RawMessage) (json.RawMessage, error) {
	if err := emptyPayload(payload); err != nil {
		return nil, err
	}
	if _, err := c.ExecContext(ctx, "UPDATE sessions SET last_activity_description=?,last_activity_provenance=? WHERE id=?",
		"", string(ProvenanceUnknown), sessionID); err != nil {
		return nil, err
	}
	return valueResult(nil)
}

func nullableMember(members []configMember, key string) (any, bool) {
	v, _ := memberValue(members, key)
	if jsonIsNull(v) {
		return nil, true
	}
	if !jsonIsString(v) {
		return nil, false
	}
	return jsonString(v), true
}

func workerBillingRoute(ctx context.Context, s *Store, c Conn, sessionID string, payload json.RawMessage) (json.RawMessage, error) {
	members, keys, err := payloadKeys(payload)
	if err != nil {
		return nil, err
	}
	if !exactKeys(keys, "provider", "base_url", "billing_mode") {
		return nil, runtimeErr(ReasonInvalidParams)
	}
	args := []any{}
	for _, key := range []string{"provider", "base_url", "billing_mode"} {
		v, ok := nullableMember(members, key)
		if !ok {
			return nil, runtimeErr(ReasonInvalidParams)
		}
		args = append(args, v)
	}
	if _, err := c.ExecContext(ctx, "UPDATE sessions SET billing_provider=?,billing_base_url=?,billing_mode=COALESCE(?,billing_mode) "+
		"WHERE id=?", append(args, sessionID)...); err != nil {
		return nil, err
	}
	return valueResult(nil)
}

func wireContentCell(members []configMember) (any, error) {
	v, _ := memberValue(members, "content")
	return JSONContent(v).encode()
}

func positiveRowID(members []configMember) (int64, error) {
	v, _ := memberValue(members, "row_id")
	n, ok := jsonInt(v)
	if !ok || n <= 0 {
		return 0, runtimeErr(ReasonInvalidParams)
	}
	return n, nil
}

func workerAPIContent(ctx context.Context, s *Store, c Conn, sessionID string, payload json.RawMessage) (json.RawMessage, error) {
	members, keys, err := payloadKeys(payload)
	if err != nil {
		return nil, err
	}
	api, _ := memberValue(members, "api_content")
	if !exactKeys(keys, "content", "api_content") || !jsonIsString(api) {
		return nil, runtimeErr(ReasonInvalidParams)
	}
	content, err := wireContentCell(members)
	if err != nil {
		return nil, err
	}
	n, err := rowsChanged(c.ExecContext(ctx, "UPDATE messages SET api_content=? WHERE id=(SELECT id FROM messages WHERE session_id=? "+
		"AND role='user' AND active=1 ORDER BY id DESC LIMIT 1) AND content IS ?", replaceSurrogates(jsonString(api)), sessionID, content))
	if err != nil {
		return nil, err
	}
	return valueResult(n)
}

func workerMessageAPIContent(ctx context.Context, s *Store, c Conn, sessionID string, payload json.RawMessage) (json.RawMessage, error) {
	members, keys, err := payloadKeys(payload)
	if err != nil {
		return nil, err
	}
	api, _ := memberValue(members, "api_content")
	if !exactKeys(keys, "row_id", "content", "api_content") || !jsonIsString(api) {
		return nil, runtimeErr(ReasonInvalidParams)
	}
	rowID, err := positiveRowID(members)
	if err != nil {
		return nil, err
	}
	content, err := wireContentCell(members)
	if err != nil {
		return nil, err
	}
	n, err := rowsChanged(c.ExecContext(ctx, "UPDATE messages SET api_content=? WHERE id=? AND session_id=? AND role='user' AND active=1 AND content IS ?",
		replaceSurrogates(jsonString(api)), rowID, sessionID, content))
	if err != nil {
		return nil, err
	}
	return valueResult(n)
}

func workerUserContent(ctx context.Context, s *Store, c Conn, sessionID string, payload json.RawMessage) (json.RawMessage, error) {
	members, keys, err := payloadKeys(payload)
	if err != nil {
		return nil, err
	}
	withMeta := keys["display_metadata"]
	delete(keys, "display_metadata")
	if !exactKeys(keys, "row_id", "content") {
		return nil, runtimeErr(ReasonInvalidParams)
	}
	content, err := wireContentCell(members)
	if err != nil {
		return nil, err
	}
	rowID, err := positiveRowID(members)
	if err != nil {
		return nil, err
	}
	var n int64
	if !withMeta {
		n, err = rowsChanged(c.ExecContext(ctx, "UPDATE messages SET content=? WHERE id=? AND session_id=? AND role='user' AND active=1",
			content, rowID, sessionID))
	} else {
		meta, _ := memberValue(members, "display_metadata")
		encodedMeta, merr := encodeDisplayMetadata(meta)
		if merr != nil {
			return nil, merr
		}
		n, err = rowsChanged(c.ExecContext(ctx, "UPDATE messages SET content=?, api_content=NULL, display_metadata=? "+
			"WHERE id=? AND session_id=? AND role='user' AND active=1", content, encodedMeta, rowID, sessionID))
	}
	if err != nil {
		return nil, err
	}
	return valueResult(n)
}

func sessionIn(ctx context.Context, c Conn, sessionID string) (Session, error) {
	rows, err := querySessions(c.QueryContext(ctx, "SELECT * FROM sessions WHERE id = ?", sessionID))
	if err != nil {
		return Session{}, err
	}
	if len(rows) == 0 {
		return Session{}, fmt.Errorf("session %s has no row", sessionID)
	}
	return rows[0], nil
}

func rootToTipIn(ctx context.Context, c Conn, sessionID string) ([]string, error) {
	if sessionID == "" {
		return []string{sessionID}, nil
	}
	var chain []string
	current := sessionID
	for current != "" && !containsString(chain, current) && len(chain) < 100 {
		chain = append(chain, current)
		var parent sql.NullString
		err := c.QueryRowContext(ctx, "SELECT parent_session_id FROM sessions WHERE id = ?", current).Scan(&parent)
		if errors.Is(err, sql.ErrNoRows) {
			break
		}
		if err != nil {
			return nil, err
		}
		current = parent.String
	}
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}
	return chain, nil
}

func compressionChainIn(ctx context.Context, c Conn, sessionID string) ([]string, error) {
	return walkCompressionChain(sessionID, func(id string) (string, error) { return chainStep(ctx, c, id) })
}

func isCompressionChildIn(ctx context.Context, c Conn, child Session) (bool, error) {
	if !child.ParentSessionID.Valid || child.ParentSessionID.V == "" || isExplicitForkChildRow(child, true) {
		return false, nil
	}
	parent, err := sessionIn(ctx, c, child.ParentSessionID.V)
	return err == nil && parent.EndReason.Valid && parent.EndReason.V == "compression", err
}

func compressionLineageIn(ctx context.Context, c Conn, sessionID string) ([]string, error) {
	session, err := sessionIn(ctx, c, sessionID)
	if err != nil {
		return nil, err
	}
	if isExplicitForkChildRow(session, false) {
		return []string{sessionID}, nil
	}
	root := session
	ancestors := map[string]bool{root.ID: true}
	for {
		cont, err := isCompressionChildIn(ctx, c, root)
		if err != nil {
			return nil, err
		}
		if !cont {
			break
		}
		parent, err := sessionIn(ctx, c, root.ParentSessionID.V)
		if err != nil {
			return nil, err
		}
		if ancestors[parent.ID] {
			break
		}
		root = parent
		ancestors[root.ID] = true
	}
	lineage := []string{root.ID}
	seen := map[string]bool{root.ID: true}
	current := root
	for current.EndReason.Valid && current.EndReason.V == "compression" {
		children, err := querySessions(c.QueryContext(ctx, "\n                SELECT * FROM sessions\n                WHERE parent_session_id = ?\n                ORDER BY started_at ASC\n                ", current.ID))
		if err != nil {
			return nil, err
		}
		var next *Session
		for i := range children {
			cont, err := isCompressionChildIn(ctx, c, children[i])
			if err != nil {
				return nil, err
			}
			if cont {
				next = &children[i]
				break
			}
		}
		if next == nil || seen[next.ID] {
			break
		}
		lineage = append(lineage, next.ID)
		seen[next.ID] = true
		current = *next
	}
	if !containsString(lineage, sessionID) {
		return []string{sessionID}, nil
	}
	return lineage, nil
}

func resumeLineageIDsIn(ctx context.Context, c Conn, sessionID string) ([]string, error) {
	if sessionID == "" {
		return []string{sessionID}, nil
	}
	session, err := sessionIn(ctx, c, sessionID)
	if err != nil {
		return nil, err
	}
	if isExplicitForkChildRow(session, false) {
		return []string{sessionID}, nil
	}
	chain := []string{sessionID}
	seen := map[string]bool{sessionID: true}
	current := session
	for len(chain) < 100 {
		cont, err := isCompressionChildIn(ctx, c, current)
		if err != nil {
			return nil, err
		}
		if !cont {
			break
		}
		parent, err := sessionIn(ctx, c, current.ParentSessionID.V)
		if err != nil {
			return nil, err
		}
		if seen[parent.ID] {
			break
		}
		seen[parent.ID] = true
		chain = append(chain, parent.ID)
		current = parent
	}
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}
	return chain, nil
}

func resolveResumeIn(ctx context.Context, c Conn, sessionID string) (string, error) {
	if sessionID == "" {
		return sessionID, nil
	}
	if tip, err := CompressionTipIn(ctx, c, sessionID); err == nil && tip != "" {
		sessionID = tip
	}
	current, found := sessionID, ""
	seen := map[string]bool{current: true}
	for range 32 {
		var one int
		err := c.QueryRowContext(ctx, "SELECT 1 FROM messages WHERE session_id = ? LIMIT 1", current).Scan(&one)
		if err == nil {
			found = current
		} else if !errors.Is(err, sql.ErrNoRows) {
			return sessionID, nil
		}
		var child sql.NullString
		err = c.QueryRowContext(ctx, "SELECT id FROM sessions AS child WHERE child.parent_session_id = ? "+
			"  AND "+JSONExtractSQL("child.model_config", "$._branched_from")+" IS NULL "+
			"  AND "+JSONExtractSQL("child.model_config", "$._delegate_from")+" IS NULL "+
			"  AND "+JSONExtractSQL("child.model_config", "$._reset_from")+" IS NULL "+
			"  AND NOT "+legacyResetChildSQL("child", sqlLiteralList(resetEndReasons))+" "+
			"  AND COALESCE(child.source, '') != 'tool' "+
			"ORDER BY child.started_at DESC, child.id DESC LIMIT 1", current).Scan(&child)
		if errors.Is(err, sql.ErrNoRows) {
			break
		}
		if err != nil {
			return sessionID, nil
		}
		if child.String == "" || seen[child.String] {
			break
		}
		current = child.String
		seen[current] = true
	}
	if found != "" {
		return found, nil
	}
	return sessionID, nil
}

func authorizeLineage(ctx context.Context, c Conn, assigned string, targetRaw json.RawMessage) (string, error) {
	if !jsonIsString(targetRaw) {
		return "", runtimeErr(ReasonInvalidParams)
	}
	target := jsonString(targetRaw)
	if err := checkText(target); err != nil {
		return "", err
	}
	allowed, err := rootToTipIn(ctx, c, assigned)
	if err != nil {
		return "", err
	}
	chain, err := compressionChainIn(ctx, c, assigned)
	if err != nil {
		return "", err
	}
	if !containsString(append(allowed, chain...), target) {
		return "", runtimeErr(ReasonPermissionDenied)
	}
	return target, nil
}

func workerCompressionHandlers() map[string]WorkerHandler {
	h := map[string]WorkerHandler{
		"turn.cleanup":                 workerTurnCleanup,
		"compression.append":           workerCompressionAppend,
		"compression.reopen":           workerReopen,
		"compression.cleanup":          workerCompressionCleanup,
		"compression.watermark":        workerWatermark,
		"compression.archive":          workerArchive,
		"compression.publish":          workerPublish,
		"compression.context":          workerLineageContext,
		"compression.lineage":          workerLineage,
		"compression.boundary":         workerBoundary,
		"compression.history":          workerHistory,
		"compression.cooldown.record":  workerCooldownRecord,
		"compression.cooldown.restore": workerCooldownRestore,
		"compression.cooldown.clear":   workerCooldownClear,
	}
	for _, action := range []string{"acquire", "renew", "release", "holder"} {
		h["compression.lock."+action] = func(ctx context.Context, s *Store, c Conn, sid string, p json.RawMessage) (json.RawMessage, error) {
			return workerCompressionLock(ctx, s, c, sid, p, action)
		}
	}
	for name, column := range map[string]string{"fallback": "compression_fallback_streak", "ineffective": "compression_ineffective_count",
		"recovery": "compression_recovery_deadline"} {
		h["compression."+name] = func(ctx context.Context, s *Store, c Conn, sid string, p json.RawMessage) (json.RawMessage, error) {
			return workerCompressionCounter(ctx, c, sid, p, column)
		}
	}
	return h
}

func fieldsExact(payload json.RawMessage, required []string, optional ...string) ([]configMember, error) {
	members, keys, err := payloadKeys(payload)
	if err != nil {
		return nil, err
	}
	for _, k := range required {
		if !keys[k] {
			return nil, runtimeErr(ReasonInvalidParams)
		}
	}
	if !keysWithin(keys, append(append([]string{}, required...), optional...)...) {
		return nil, runtimeErr(ReasonInvalidParams)
	}
	return members, nil
}

func requiredText(members []configMember, key string) (string, error) {
	v, _ := memberValue(members, key)
	if !jsonIsString(v) {
		return "", runtimeErr(ReasonInvalidParams)
	}
	text := jsonString(v)
	return text, checkText(text)
}

func finiteNumber(raw json.RawMessage) (float64, error) {
	f, ok := jsonNumber(raw)
	if !ok {
		return 0, runtimeErr(ReasonInvalidParams)
	}
	return f, nil
}

func workerCompressionLock(ctx context.Context, s *Store, c Conn, sessionID string, payload json.RawMessage, action string) (json.RawMessage, error) {
	if action == "holder" {
		if _, err := fieldsExact(payload, nil); err != nil {
			return nil, err
		}
		holder, err := firstText(ctx, c, "SELECT holder FROM compression_locks WHERE session_id=? AND expires_at>?", sessionID, nowEpoch())
		if err != nil {
			return nil, err
		}
		return valueResult(holder)
	}
	var optional []string
	if action != "release" {
		optional = []string{"ttl_seconds"}
	}
	members, err := fieldsExact(payload, []string{"holder"}, optional...)
	if err != nil {
		return nil, err
	}
	holder, err := requiredText(members, "holder")
	if err != nil {
		return nil, err
	}
	now := nowEpoch()
	ttl := 300.0
	if v, has := memberValue(members, "ttl_seconds"); has {
		if ttl, err = finiteNumber(v); err != nil {
			return nil, err
		}
	}
	if ttl < 0.1 || ttl > 3600 {
		return nil, runtimeErr(ReasonInvalidParams)
	}
	switch action {
	case "acquire":
		acquired, _, err := claimLeaseRow(ctx, c, "compression_locks", "session_id", sessionID, holder, now, now+ttl,
			func(h string, e float64) bool { return e < now || s.eng.holderProcessGone(h) })
		if err != nil {
			return nil, err
		}
		return valueResult(acquired)
	case "renew":
		n, err := rowsChanged(c.ExecContext(ctx, "UPDATE compression_locks SET expires_at=? WHERE session_id=? AND holder=?", now+ttl, sessionID, holder))
		if err != nil {
			return nil, err
		}
		return valueResult(n > 0)
	}
	if _, err := c.ExecContext(ctx, "DELETE FROM compression_locks WHERE session_id=? AND holder=?", sessionID, holder); err != nil {
		return nil, err
	}
	return valueResult(nil)
}

func nullableErrorText(raw json.RawMessage) (any, error) {
	if jsonIsNull(raw) {
		return nil, nil
	}
	if !jsonIsString(raw) {
		return nil, runtimeErr(ReasonInvalidParams)
	}
	return jsonString(raw), nil
}

func workerCooldownRecord(ctx context.Context, s *Store, c Conn, sessionID string, payload json.RawMessage) (json.RawMessage, error) {
	members, err := fieldsExact(payload, []string{"cooldown_until", "error"})
	if err != nil {
		return nil, err
	}
	raw, _ := memberValue(members, "cooldown_until")
	deadline, err := finiteNumber(raw)
	if err != nil {
		return nil, err
	}
	errRaw, _ := memberValue(members, "error")
	errText, err := nullableErrorText(errRaw)
	if err != nil {
		return nil, err
	}
	if _, err := c.ExecContext(ctx, "UPDATE sessions SET compression_failure_cooldown_until = CASE "+
		"WHEN compression_failure_cooldown_until IS NOT NULL AND compression_failure_cooldown_until > ? "+
		"THEN compression_failure_cooldown_until ELSE ? END, compression_failure_error=? WHERE id=?",
		deadline, deadline, errText, sessionID); err != nil {
		return nil, err
	}
	return valueResult(nil)
}

func restoreCooldown(ctx context.Context, c Conn, sessionID string, snapshot json.RawMessage) error {
	members, err := fieldsExact(snapshot, []string{"session_exists", "cooldown_until", "error"})
	if err != nil {
		return err
	}
	existsRaw, _ := memberValue(members, "session_exists")
	exists, ok := jsonBool(existsRaw)
	if !ok {
		return runtimeErr(ReasonInvalidParams)
	}
	if !exists {
		return errors.New("cannot restore absent compression cooldown row: session now exists")
	}
	deadlineRaw, _ := memberValue(members, "cooldown_until")
	var deadline any
	if !jsonIsNull(deadlineRaw) {
		f, err := finiteNumber(deadlineRaw)
		if err != nil {
			return err
		}
		deadline = f
	}
	errRaw, _ := memberValue(members, "error")
	errText, err := nullableErrorText(errRaw)
	if err != nil {
		return err
	}
	if _, err := c.ExecContext(ctx, "UPDATE sessions SET compression_failure_cooldown_until=?, compression_failure_error=? WHERE id=?",
		deadline, errText, sessionID); err != nil {
		return err
	}
	var until sql.NullFloat64
	var stored sql.NullString
	err = c.QueryRowContext(ctx, "SELECT compression_failure_cooldown_until,compression_failure_error FROM sessions WHERE id=?", sessionID).Scan(&until, &stored)
	verified := err == nil && until.Valid == (deadline != nil) && stored.Valid == (errText != nil)
	if verified && deadline != nil {
		verified = until.Float64 == deadline.(float64)
	}
	if verified && errText != nil {
		verified = stored.String == errText.(string)
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if !verified {
		return errors.New("compression cooldown rollback verification failed")
	}
	return nil
}

func workerCooldownRestore(ctx context.Context, s *Store, c Conn, sessionID string, payload json.RawMessage) (json.RawMessage, error) {
	members, err := fieldsExact(payload, []string{"snapshot"})
	if err != nil {
		return nil, err
	}
	snapshot, _ := memberValue(members, "snapshot")
	if err := restoreCooldown(ctx, c, sessionID, snapshot); err != nil {
		return nil, err
	}
	return valueResult(nil)
}

func workerCooldownClear(ctx context.Context, s *Store, c Conn, sessionID string, payload json.RawMessage) (json.RawMessage, error) {
	if _, err := fieldsExact(payload, nil); err != nil {
		return nil, err
	}
	if err := restoreCooldown(ctx, c, sessionID, json.RawMessage(`{"session_exists":true,"cooldown_until":null,"error":null}`)); err != nil {
		return nil, err
	}
	return valueResult(nil)
}

func workerCompressionCounter(ctx context.Context, c Conn, sessionID string, payload json.RawMessage, column string) (json.RawMessage, error) {
	members, err := fieldsExact(payload, []string{"value"})
	if err != nil {
		return nil, err
	}
	raw, _ := memberValue(members, "value")
	var value any
	if !jsonIsNull(raw) {
		f, err := finiteNumber(raw)
		if err != nil {
			return nil, err
		}
		value = f
		if n, ok := jsonInt(raw); ok {
			value = n
		}
	}
	if column != "compression_recovery_deadline" {
		n, ok := jsonInt(raw)
		if !ok || n < 0 {
			return nil, runtimeErr(ReasonInvalidParams)
		}
	}
	if _, err := c.ExecContext(ctx, "UPDATE sessions SET "+column+"=? WHERE id=?", value, sessionID); err != nil {
		return nil, err
	}
	return valueResult(nil)
}

func workerLineageContext(ctx context.Context, s *Store, c Conn, sessionID string, payload json.RawMessage) (json.RawMessage, error) {
	members, err := fieldsExact(payload, []string{"target"})
	if err != nil {
		return nil, err
	}
	raw, _ := memberValue(members, "target")
	target, err := authorizeLineage(ctx, c, sessionID, raw)
	if err != nil {
		return nil, err
	}
	session, _, err := requireSessionObject(ctx, c, target)
	if err != nil {
		return nil, err
	}
	w := &wireObject{}
	w.set("session", session)
	return w.raw()
}

func workerLineage(ctx context.Context, s *Store, c Conn, sessionID string, payload json.RawMessage) (json.RawMessage, error) {
	members, err := fieldsExact(payload, []string{"target"})
	if err != nil {
		return nil, err
	}
	raw, _ := memberValue(members, "target")
	target, err := authorizeLineage(ctx, c, sessionID, raw)
	if err != nil {
		return nil, err
	}
	readable, err := rootToTipIn(ctx, c, sessionID)
	if err != nil {
		return nil, err
	}
	chain, err := compressionChainIn(ctx, c, sessionID)
	if err != nil {
		return nil, err
	}
	lineage, err := compressionLineageIn(ctx, c, target)
	if err != nil {
		return nil, err
	}
	roots, err := rootToTipIn(ctx, c, target)
	if err != nil {
		return nil, err
	}
	root := target
	if len(roots) > 0 && roots[0] != "" {
		root = roots[0]
	}
	tip, err := CompressionTipIn(ctx, c, target)
	if err != nil {
		return nil, err
	}
	resume, err := resolveResumeIn(ctx, c, target)
	if err != nil {
		return nil, err
	}
	targetRow, err := sessionIn(ctx, c, target)
	if err != nil {
		return nil, err
	}
	w := &wireObject{}
	w.set("readable_ids", append(readable, chain...))
	w.set("lineage", lineage)
	w.set("root", root)
	w.set("tip", tip)
	w.set("resume", resume)
	w.set("identity", []any{isExplicitForkChildRow(targetRow, false), strings.TrimSpace(targetRow.Source)})
	return w.raw()
}

func workerBoundary(ctx context.Context, s *Store, c Conn, sessionID string, payload json.RawMessage) (json.RawMessage, error) {
	members, err := fieldsExact(payload, []string{"session_key", "source"})
	if err != nil {
		return nil, err
	}
	_, row, err := requireSessionObject(ctx, c, sessionID)
	if err != nil {
		return nil, err
	}
	key, _ := memberValue(members, "session_key")
	source, _ := memberValue(members, "source")
	rowKey, _ := memberValue(row, "session_key")
	rowSource, _ := memberValue(row, "source")
	if !sameJSONScalar(rowKey, key) || !sameJSONScalar(rowSource, source) {
		return nil, runtimeErr(ReasonPermissionDenied)
	}
	keyText, sourceText := jsonString(key), jsonString(source)
	if keyText == "" || sourceText == "" {
		return valueResult(nil)
	}
	var generation sql.NullInt64
	err = c.QueryRowContext(ctx, "SELECT generation FROM conversation_generations WHERE source = ? AND session_key = ?", sourceText, keyText).Scan(&generation)
	if errors.Is(err, sql.ErrNoRows) {
		return valueResult(nil)
	}
	if err != nil {
		return nil, err
	}
	if !generation.Valid {
		return valueResult(nil)
	}
	return valueResult(generation.Int64)
}

func sameJSONScalar(a, b json.RawMessage) bool {
	if jsonIsNull(a) || len(strings.TrimSpace(string(a))) == 0 {
		return jsonIsNull(b)
	}
	if jsonIsString(a) {
		return jsonIsString(b) && jsonString(a) == jsonString(b)
	}
	return strings.TrimSpace(string(a)) == strings.TrimSpace(string(b))
}

func removedIdentitiesIn(ctx context.Context, c Conn, rows []*msgRow, sessionIDs []string) ([]*msgRow, error) {
	removed, err := queryMsgRows(c.QueryContext(ctx, "SELECT session_id, id, role, timestamp, tool_call_id, tool_calls, tool_name, display_kind, display_metadata, "+
		"message_uid, absorbed_message_uids, display_identity, active, compacted, "+
		"CASE WHEN display_identity IS NULL THEN content END AS content "+
		"FROM messages WHERE session_id IN ("+bindMarks(len(sessionIDs))+") AND active = 0 AND compacted = 0 "+
		"ORDER BY id", stringArgs(sessionIDs)...))
	if err != nil || len(removed) == 0 {
		return rows, err
	}
	merged := make([]*msgRow, 0, len(rows)+len(removed))
	i, j := 0, 0
	for i < len(rows) || j < len(removed) {
		if j >= len(removed) || (i < len(rows) && rows[i].id <= removed[j].id) {
			merged = append(merged, rows[i])
			i++
		} else {
			merged = append(merged, removed[j])
			j++
		}
	}
	return merged, nil
}

func workerHistory(ctx context.Context, s *Store, c Conn, sessionID string, payload json.RawMessage) (json.RawMessage, error) {
	flags := []string{"include_ancestors", "include_inactive", "repair_alternation", "include_row_ids", "include_compacted"}
	members, err := fieldsExact(payload, append([]string{"target"}, flags...))
	if err != nil {
		return nil, err
	}
	values := map[string]bool{}
	for _, f := range flags {
		raw, _ := memberValue(members, f)
		b, ok := jsonBool(raw)
		if !ok {
			return nil, runtimeErr(ReasonInvalidParams)
		}
		values[f] = b
	}
	raw, _ := memberValue(members, "target")
	target, err := authorizeLineage(ctx, c, sessionID, raw)
	if err != nil {
		return nil, err
	}
	ids := []string{target}
	if values["include_ancestors"] {
		if ids, err = resumeLineageIDsIn(ctx, c, target); err != nil {
			return nil, err
		}
	}
	active := activeClause(values["include_inactive"], values["include_compacted"])
	display := values["include_compacted"] && !values["include_inactive"]
	head := ""
	if display {
		head = "session_id, compacted, display_order, "
	}
	rows, err := queryMsgRows(c.QueryContext(ctx, "SELECT "+head+conversationRowColumns+" FROM messages "+
		"WHERE session_id IN ("+bindMarks(len(ids))+")"+active+" ORDER BY id", stringArgs(ids)...))
	if err != nil {
		return nil, err
	}
	if display {
		if rows, err = removedIdentitiesIn(ctx, c, rows, ids); err != nil {
			return nil, err
		}
	}
	if values["include_compacted"] {
		rows = dedupeDisplayGenerations(rows, display)
	}
	messages, err := s.rowsToConversation(rows, conversionOptions{sessionID: target, includeAncestors: values["include_ancestors"],
		repairAlternation: values["repair_alternation"], includeRowIDs: values["include_row_ids"]})
	if err != nil {
		return nil, err
	}
	encoded := make([]json.RawMessage, 0, len(messages))
	for _, m := range messages {
		e, err := encodeConversationMessage(m)
		if err != nil {
			return nil, err
		}
		encoded = append(encoded, e)
	}
	w := &wireObject{}
	w.set("messages", rawArray(encoded))
	return w.raw()
}

func workerWatermark(ctx context.Context, s *Store, c Conn, sessionID string, payload json.RawMessage) (json.RawMessage, error) {
	if _, err := fieldsExact(payload, nil); err != nil {
		return nil, err
	}
	var mark int64
	if err := c.QueryRowContext(ctx, "SELECT COALESCE(MAX(id),0) FROM messages WHERE session_id=? AND active=1", sessionID).Scan(&mark); err != nil {
		return nil, err
	}
	return valueResult(mark)
}

func watermarkValue(members []configMember, key string) (*int64, error) {
	v, has := memberValue(members, key)
	if !has || jsonIsNull(v) {
		return nil, nil
	}
	n, ok := jsonInt(v)
	if !ok || n < 0 {
		return nil, runtimeErr(ReasonInvalidParams)
	}
	return &n, nil
}

func objectList(raw json.RawMessage) bool {
	elements, ok := arrayElements(raw)
	if !ok {
		return false
	}
	for _, e := range elements {
		if _, isObject := objectMembers(e); !isObject {
			return false
		}
	}
	return true
}

func workerArchive(ctx context.Context, s *Store, c Conn, sessionID string, payload json.RawMessage) (json.RawMessage, error) {
	members, err := fieldsExact(payload, []string{"messages", "model_config_patch", "watermark", "lock_holder", "tail_count"},
		"carried_messages", "covered_ids", "unresolved_held", "held_for_cas")
	if err != nil {
		return nil, err
	}
	raw, _ := memberValue(members, "messages")
	messages, err := decodeWireMessages(ctx, c, sessionID, raw, true)
	if err != nil {
		return nil, err
	}
	o := CompactOptions{}
	if carried, has := memberValue(members, "carried_messages"); has && jsonTruthy(carried) {
		if !objectList(carried) {
			return nil, runtimeErr(ReasonInvalidParams)
		}
		if o.CarriedMessages, err = decodeWireMessageList(carried); err != nil {
			return nil, err
		}
	}
	if covered, has := memberValue(members, "covered_ids"); has && !jsonIsNull(covered) {
		elements, ok := arrayElements(covered)
		if !ok {
			return nil, runtimeErr(ReasonInvalidParams)
		}
		o.CoveredIDs = []int64{}
		for _, e := range elements {
			n, ok := jsonInt(e)
			if !ok || n <= 0 {
				return nil, runtimeErr(ReasonInvalidParams)
			}
			o.CoveredIDs = append(o.CoveredIDs, n)
		}
	}
	if unresolved, has := memberValue(members, "unresolved_held"); has && !jsonIsNull(unresolved) {
		if !objectList(unresolved) {
			return nil, runtimeErr(ReasonInvalidParams)
		}
		if o.UnresolvedHeld, err = decodeWireHeld(unresolved); err != nil {
			return nil, err
		}
	}
	if cas, has := memberValue(members, "held_for_cas"); has && !jsonIsNull(cas) {
		if !objectList(cas) {
			return nil, runtimeErr(ReasonInvalidParams)
		}
		if o.HeldForCAS, err = decodeWireMessageList(cas); err != nil {
			return nil, err
		}
	}
	if o.Watermark, err = watermarkValue(members, "watermark"); err != nil {
		return nil, err
	}
	if _, err := watermarkValue(members, "watermark_ceiling"); err != nil {
		return nil, err
	}
	if holder, has, err := optionalText(members, "lock_holder"); err != nil {
		return nil, err
	} else if has {
		o.LockHolder = holder
	}
	patch, _ := memberValue(members, "model_config_patch")
	if !jsonIsNull(patch) {
		if !sidecarPatch(patch) {
			return nil, runtimeErr(ReasonInvalidParams)
		}
		o.ModelConfigPatch = patchMembers(patch)
	}
	tail, _ := memberValue(members, "tail_count")
	tailCount, ok := jsonInt(tail)
	if !ok || tailCount < 0 {
		return nil, runtimeErr(ReasonInvalidParams)
	}
	o.TailCount = int(tailCount)
	value, err := s.ArchiveAndCompactIn(ctx, c, sessionID, messages, o)
	if err != nil {
		return nil, err
	}
	annotations, err := rowAnnotations(messages)
	if err != nil {
		return nil, err
	}
	w := &wireObject{}
	w.set("value", value)
	w.set("annotations", annotations)
	return w.raw()
}

func migrateWorkerAutomation(ctx context.Context, s *Store, c Conn, parent, child string) error {
	for _, family := range []string{"goal", "heartbeat", "loop"} {
		oldKey, newKey := family+":"+parent, family+":"+child
		raw, found, err := metaValue(ctx, c, oldKey)
		if err != nil {
			return err
		}
		if !found {
			continue
		}
		restate := s.tables.state.options.Automation.Restate
		if restate == nil {
			return ErrNoAutomationPolicy
		}
		status, encoded, cleared, err := restate(family, raw)
		if err != nil {
			return err
		}
		if status == "cleared" {
			continue
		}
		prior, hasPrior, err := metaValue(ctx, c, newKey)
		if err != nil {
			return err
		}
		if hasPrior {
			if family != "heartbeat" {
				continue
			}
			priorStatus, _, _, err := restate(family, prior)
			if err != nil {
				return err
			}
			if priorStatus != "cleared" {
				continue
			}
		}
		if err := SetMetaIn(ctx, c, newKey, encoded); err != nil {
			return err
		}
		if err := SetMetaIn(ctx, c, oldKey, cleared); err != nil {
			return err
		}
	}
	return nil
}

func workerPublish(ctx context.Context, s *Store, c Conn, sessionID string, payload json.RawMessage) (json.RawMessage, error) {
	members, err := fieldsExact(payload, []string{"child_session_id", "source", "messages", "model", "model_config", "system_prompt",
		"cwd", "profile_name", "compression_lock_holder", "require_compression_lease", "require_lease_refresh", "lease_ttl_seconds",
		"watermark", "watermark_ceiling"})
	if err != nil {
		return nil, err
	}
	child, err := requiredText(members, "child_session_id")
	if err != nil {
		return nil, err
	}
	holder, err := requiredText(members, "compression_lock_holder")
	if err != nil {
		return nil, err
	}
	watermark, err := watermarkValue(members, "watermark")
	if err != nil {
		return nil, err
	}
	ceiling, err := watermarkValue(members, "watermark_ceiling")
	if err != nil {
		return nil, err
	}
	requireLease, _ := memberValue(members, "require_compression_lease")
	refreshRaw, _ := memberValue(members, "require_lease_refresh")
	refresh, refreshOK := jsonBool(refreshRaw)
	if strings.TrimSpace(string(requireLease)) != "true" || !refreshOK {
		return nil, runtimeErr(ReasonInvalidParams)
	}
	ttlRaw, _ := memberValue(members, "lease_ttl_seconds")
	ttl, err := finiteNumber(ttlRaw)
	if err != nil {
		return nil, err
	}
	if ttl < 0.1 || ttl > 3600 {
		return nil, runtimeErr(ReasonInvalidParams)
	}
	raw, _ := memberValue(members, "messages")
	messages, err := decodeWireMessages(ctx, c, sessionID, raw, true)
	if err != nil {
		return nil, err
	}
	_, parent, err := requireSessionObject(ctx, c, sessionID)
	if err != nil {
		return nil, err
	}
	for _, key := range []string{"source", "cwd", "profile_name"} {
		v, _ := memberValue(members, key)
		stored, _ := memberValue(parent, key)
		if !jsonIsNull(v) && !sameJSONScalar(stored, v) {
			return nil, runtimeErr(ReasonPermissionDenied)
		}
	}
	texts := map[string]*string{}
	for _, key := range []string{"model", "system_prompt", "source", "cwd", "profile_name"} {
		v, _ := memberValue(members, key)
		if jsonIsNull(v) {
			continue
		}
		if !jsonIsString(v) {
			return nil, runtimeErr(ReasonInvalidParams)
		}
		text := jsonString(v)
		texts[key] = &text
	}
	cfgRaw, _ := memberValue(members, "model_config")
	var cfg []configMember
	if !jsonIsNull(cfgRaw) {
		var ok bool
		if cfg, ok = objectMembers(cfgRaw); !ok {
			return nil, runtimeErr(ReasonInvalidParams)
		}
	}
	storedConfig, _ := memberValue(parent, "model_config")
	prior := configMembers(jsonString(storedConfig))
	for _, key := range []string{"_branched_from", "_delegate_from", "_reset_from"} {
		given, has := memberValue(cfg, key)
		was, hadPrior := memberValue(prior, key)
		if has && !(hadPrior && sameJSONValue(given, was)) && !(!hadPrior && jsonIsNull(given)) {
			return nil, runtimeErr(ReasonPermissionDenied)
		}
		if hadPrior {
			cfg = setConfigMember(cfg, key, was)
		}
	}
	exists, err := queryStrings(ctx, c, "SELECT 1 FROM sessions WHERE id=?", child)
	if err != nil {
		return nil, err
	}
	if len(exists) > 0 {
		return nil, runtimeErr(ReasonPermissionDenied)
	}
	var worker workerRow
	err = c.QueryRowContext(ctx, "SELECT "+workerColumns+" FROM worker_executions WHERE session_id=? AND status IN ('registered','running')", sessionID).
		Scan(&worker.ExecutionID, &worker.SessionID, &worker.Kind, &worker.OwnerEpoch, &worker.Generation, &worker.Status, &worker.LastSequence, &worker.adoptionDigest)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, runtimeErr(ReasonStaleGeneration)
	}
	if err != nil {
		return nil, err
	}
	deref := func(key string) string {
		if texts[key] == nil {
			return ""
		}
		return *texts[key]
	}
	if err := s.PublishCompressionChildIn(ctx, c, CompressionChild{ParentSessionID: sessionID, ChildSessionID: child,
		Source: deref("source"), Messages: messages, Model: deref("model"), ModelConfig: rawObject(cfg),
		SystemPrompt: texts["system_prompt"], Cwd: deref("cwd"), ProfileName: deref("profile_name"), CompressionLockHolder: holder,
		RequireLeaseRefresh: refresh, LeaseTTL: time.Duration(ttl * float64(time.Second)), Watermark: watermark,
		WatermarkCeiling: ceiling}); err != nil {
		return nil, err
	}
	if err := migrateWorkerAutomation(ctx, s, c, sessionID, child); err != nil {
		return nil, err
	}
	if _, err := c.ExecContext(ctx, "UPDATE sessions SET runtime_generation=? WHERE id=?", worker.Generation, child); err != nil {
		return nil, err
	}
	if _, err := c.ExecContext(ctx, "UPDATE worker_executions SET session_id=? WHERE execution_id=?", child, worker.ExecutionID); err != nil {
		return nil, err
	}
	if _, err := c.ExecContext(ctx, "UPDATE session_admissions SET lineage_json=json_insert(lineage_json,'$[#]',?) "+
		"WHERE json_extract(lineage_json,'$[#-1]')=? AND generation=? AND owner_epoch=? "+
		"AND status IN ('started','unknown')", child, sessionID, worker.Generation, worker.OwnerEpoch); err != nil {
		return nil, err
	}
	annotations, err := rowAnnotations(messages)
	if err != nil {
		return nil, err
	}
	assignment := &wireObject{}
	assignment.set("parent", sessionID)
	assignment.set("session_id", child)
	assigned, err := assignment.raw()
	if err != nil {
		return nil, err
	}
	w := &wireObject{}
	w.set("value", nil)
	w.set("worker_assignment", assigned)
	w.set("annotations", annotations)
	return w.raw()
}

func sameJSONValue(a, b json.RawMessage) bool {
	x, errA := canonicalJSON(a)
	y, errB := canonicalJSON(b)
	return errA == nil && errB == nil && x == y
}

func reopenOnConnection(ctx context.Context, c Conn, sessionID string) (bool, error) {
	closed, err := endedByCompression(ctx, c, sessionID)
	if err != nil || !closed {
		return false, err
	}
	children, err := queryStrings(ctx, c, "\n                SELECT 1\n                FROM sessions\n                WHERE parent_session_id = ?\n                "+
		nonContinuationChildSQL("", "?")+"\n                LIMIT 1\n                ", sessionID, sessionID, sessionID, sessionID)
	if err != nil || len(children) > 0 {
		return false, err
	}
	now := nowEpoch()
	var holder string
	var expires sql.NullFloat64
	err = c.QueryRowContext(ctx, compressionLockRowSQL, sessionID).Scan(&holder, &expires)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return false, err
	default:
		if !expires.Valid || expires.Float64 >= now {
			return false, nil
		}
		n, err := rowsChanged(c.ExecContext(ctx, "DELETE FROM compression_locks WHERE session_id = ? AND holder = ? AND expires_at = ?",
			sessionID, holder, expires.Float64))
		if err != nil || n != 1 {
			return false, err
		}
	}
	n, err := rowsChanged(c.ExecContext(ctx, "UPDATE sessions SET ended_at = NULL, end_reason = NULL WHERE id = ? AND ended_at IS NOT NULL AND end_reason = 'compression'", sessionID))
	return n == 1, err
}

func workerReopen(ctx context.Context, s *Store, c Conn, sessionID string, payload json.RawMessage) (json.RawMessage, error) {
	if _, err := fieldsExact(payload, nil); err != nil {
		return nil, err
	}
	reopened, err := reopenOnConnection(ctx, c, sessionID)
	if err != nil {
		return nil, err
	}
	return valueResult(reopened)
}

func sameTurnKey(ctx context.Context, c Conn, a, b string) (string, error) {
	keyA, err := sessionTurnLeaseKeyIn(ctx, c, a)
	if err != nil {
		return "", err
	}
	keyB, err := sessionTurnLeaseKeyIn(ctx, c, b)
	if err != nil {
		return "", err
	}
	if keyA != keyB {
		return "", runtimeErr(ReasonPermissionDenied)
	}
	return keyA, nil
}

func workerCompressionCleanup(ctx context.Context, s *Store, c Conn, sessionID string, payload json.RawMessage) (json.RawMessage, error) {
	members, err := fieldsExact(payload, []string{"target", "holder"})
	if err != nil {
		return nil, err
	}
	raw, _ := memberValue(members, "target")
	if _, err := requiredText(members, "target"); err != nil {
		return nil, err
	}
	holder, err := requiredText(members, "holder")
	if err != nil {
		return nil, err
	}
	target, err := authorizeLineage(ctx, c, sessionID, raw)
	if err != nil {
		return nil, err
	}
	if _, err := sameTurnKey(ctx, c, sessionID, target); err != nil {
		return nil, err
	}
	if _, err := c.ExecContext(ctx, "DELETE FROM compression_locks WHERE session_id=? AND holder=?", target, holder); err != nil {
		return nil, err
	}
	return valueResult(nil)
}

func workerCompressionAppend(ctx context.Context, s *Store, c Conn, sessionID string, payload json.RawMessage) (json.RawMessage, error) {
	members, err := fieldsExact(payload, []string{"messages", "compression_lock_holder", "turn_lease_holder", "turn_lease_ttl_seconds"})
	if err != nil {
		return nil, err
	}
	raw, _ := memberValue(members, "messages")
	messages, err := decodeWireMessages(ctx, c, sessionID, raw, true)
	if err != nil {
		return nil, err
	}
	lockHolder, _, err := optionalText(members, "compression_lock_holder")
	if err != nil {
		return nil, err
	}
	turnHolder, _, err := optionalText(members, "turn_lease_holder")
	if err != nil {
		return nil, err
	}
	ttlRaw, _ := memberValue(members, "turn_lease_ttl_seconds")
	ttl, err := finiteNumber(ttlRaw)
	if err != nil {
		return nil, err
	}
	if ttl < 0.1 || ttl > 3600 {
		return nil, runtimeErr(ReasonInvalidParams)
	}
	count, err := s.AppendMessagesIn(ctx, c, sessionID, messages, AppendOptions{CompressionLockHolder: lockHolder,
		TurnLeaseHolder: turnHolder, TurnLeaseTTL: time.Duration(ttl * float64(time.Second))})
	if err != nil {
		return nil, err
	}
	annotations, err := rowAnnotations(messages)
	if err != nil {
		return nil, err
	}
	w := &wireObject{}
	w.set("count", count)
	w.set("annotations", annotations)
	return w.raw()
}

func workerTurnCleanup(ctx context.Context, s *Store, c Conn, sessionID string, payload json.RawMessage) (json.RawMessage, error) {
	members, err := fieldsExact(payload, []string{"target", "holder"})
	if err != nil {
		return nil, err
	}
	raw, _ := memberValue(members, "target")
	if _, err := requiredText(members, "target"); err != nil {
		return nil, err
	}
	holder, err := requiredText(members, "holder")
	if err != nil {
		return nil, err
	}
	target, err := authorizeLineage(ctx, c, sessionID, raw)
	if err != nil {
		return nil, err
	}
	key, err := sameTurnKey(ctx, c, sessionID, target)
	if err != nil {
		return nil, err
	}
	if _, err := c.ExecContext(ctx, "DELETE FROM session_turn_leases WHERE conversation_id=? AND holder=?", key, holder); err != nil {
		return nil, err
	}
	w := &wireObject{}
	w.set("released", true)
	return w.raw()
}
