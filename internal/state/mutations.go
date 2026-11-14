package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
)

const compactedTargetDetail = "compacted: target user message was summarized when the session was compressed"

var metadataFieldKinds = map[string]string{"title": "string", "archived": "bool", "hidden": "bool", "pinned": "bool", "unread": "bool"}

var validReasoningEfforts = []string{"minimal", "low", "medium", "high", "xhigh", "max", "ultra"}

var generationFencedOperations = []string{"delete", "rewind", "reset", "branch", "model", "compress"}

func jsonKind(raw json.RawMessage) string {
	t := strings.TrimSpace(string(raw))
	switch {
	case t == "":
		return ""
	case t == "null":
		return "null"
	case t == "true" || t == "false":
		return "bool"
	case t[0] == '"':
		return "string"
	case t[0] == '{':
		return "object"
	case t[0] == '[':
		return "array"
	}
	if _, ok := jsonInt(raw); ok {
		return "int"
	}
	return "float"
}

func memberKeys(members []configMember) []string {
	keys := make([]string, len(members))
	for i, m := range members {
		keys[i] = m.key
	}
	return keys
}

func keysWithinSet(members []configMember, allowed ...string) bool {
	for _, m := range members {
		if !slices.Contains(allowed, m.key) {
			return false
		}
	}
	return true
}

func keysExactly(members []configMember, want ...string) bool {
	return len(members) == len(want) && keysWithinSet(members, want...)
}

func reasoningLevelValid(level string) bool {
	t := strings.ToLower(strings.TrimSpace(level))
	return t == "none" || t == "false" || t == "disabled" || slices.Contains(validReasoningEfforts, t)
}

func validEditorFingerprint(members []configMember) bool {
	raw, _ := memberValue(members, "mcp_servers")
	servers, ok := objectMembers(raw)
	fingerprint, _ := memberValue(servers, "fingerprint")
	return ok && keysExactly(servers, "fingerprint") && jsonKind(fingerprint) == "string"
}

func validBranchPayload(members []configMember) bool {
	if !keysWithinSet(members, "title", "through_message_id", "mcp_servers") {
		return false
	}
	if _, has := memberValue(members, "mcp_servers"); has && !validEditorFingerprint(members) {
		return false
	}
	if title, has := memberValue(members, "title"); has && jsonKind(title) != "string" {
		return false
	}
	through, has := memberValue(members, "through_message_id")
	if !has {
		return true
	}
	n, ok := jsonInt(through)
	return ok && jsonKind(through) == "int" && n > 0
}

func validCompressPayload(members []configMember) bool {
	kinds := map[string]string{"focus": "string", "preview": "bool", "partial": "bool", "keep_last": "int"}
	for _, m := range members {
		kind, known := kinds[m.key]
		if !known || jsonKind(m.value) != kind {
			return false
		}
	}
	keep, has := memberValue(members, "keep_last")
	if !has {
		return true
	}
	partial, _ := memberValue(members, "partial")
	n, _ := jsonInt(keep)
	return strings.TrimSpace(string(partial)) == "true" && n >= 1
}

func validateAction(operation string, payload json.RawMessage) error {
	members, ok := objectMembers(payload)
	if !ok {
		return runtimeErr(ReasonInvalidParams)
	}
	text := func(key string) (string, bool) {
		v, has := memberValue(members, key)
		return jsonString(v), has && jsonKind(v) == "string"
	}
	if _, has := memberValue(members, "model"); operation == "model" && has && keysWithinSet(members, "model", "provider", "confirm", "reasoning") {
		allText := true
		for _, m := range members {
			allText = allText && jsonKind(m.value) == "string" && strings.TrimSpace(jsonString(m.value)) != ""
		}
		if allText {
			level, hasLevel := text("reasoning")
			if !hasLevel || reasoningLevelValid(level) {
				return nil
			}
		}
	}
	if operation == "branch" && validBranchPayload(members) {
		return nil
	}
	if operation == "reasoning" && keysExactly(members, "effort") {
		effort, _ := memberValue(members, "effort")
		if jsonKind(effort) == "null" || (jsonKind(effort) == "string" && reasoningLevelValid(jsonString(effort))) {
			return nil
		}
	}
	if cwd, isText := text("cwd"); operation == "workspace" && keysExactly(members, "cwd") && isText && strings.TrimSpace(cwd) != "" {
		return nil
	}
	if operation == "editor_mcp" && keysExactly(members, "mcp_servers") && validEditorFingerprint(members) {
		return nil
	}
	if policy, isText := text("policy"); operation == "edit_policy" && keysExactly(members, "policy") && isText &&
		slices.Contains([]string{"ask", "workspace_session", "session"}, policy) {
		return nil
	}
	if operation == "compress" && validCompressPayload(members) {
		return nil
	}
	if (operation == "delete" || operation == "reset") && len(members) == 0 {
		return nil
	}
	if sessions, _ := memberValue(members, "sessions"); operation == "import" && keysExactly(members, "sessions") && jsonKind(sessions) == "array" {
		return nil
	}
	if operation == "rewind" && keysWithinSet(members, "target_message_id", "preserve_compaction_handoff") {
		target, has := memberValue(members, "target_message_id")
		id, _ := jsonInt(target)
		preserve, hasPreserve := memberValue(members, "preserve_compaction_handoff")
		if has && jsonKind(target) == "int" && id > 0 && (!hasPreserve || jsonKind(preserve) == "bool") {
			return nil
		}
	}
	valid := false
	switch operation {
	case "sidebar":
		valid = len(members) > 0 && keysWithinSet(members, "title", "archived", "hidden", "pinned", "unread")
	case "rename":
		valid = keysExactly(members, "title")
	case "archive":
		valid = keysExactly(members, "archived")
	}
	for _, m := range members {
		valid = valid && jsonKind(m.value) == metadataFieldKinds[m.key]
	}
	if !valid {
		return runtimeErr(ReasonInvalidParams)
	}
	return nil
}

type PreparedMutation struct {
	Snapshot LocalSnapshot
	Values   json.RawMessage
	Messages []*Message
}

type MutationRequest struct {
	Epoch              int64
	PrincipalID        string
	SessionID          string
	RequestID          string
	ExpectedRevision   int64
	ExpectedGeneration *int64
	Operation          string
	Payload            json.RawMessage
	LiveGuard          func(targets []string) error
	AuthorizeWrite     func(ctx context.Context, c Conn) error
	PrepareOnly        bool
	Prepared           *PreparedMutation
}

type MutationOutcome struct {
	Result   json.RawMessage
	Snapshot *LocalSnapshot
}

type sessionGone struct {
	ids    []string
	reason string
}

func rawInt(n int64) json.RawMessage { return json.RawMessage(strconv.FormatInt(n, 10)) }

func (s *Store) MutateRuntimeSession(ctx context.Context, r MutationRequest) (MutationOutcome, error) {
	if err := checkText(r.PrincipalID, r.SessionID, r.RequestID); err != nil {
		return MutationOutcome{}, err
	}
	if r.ExpectedRevision < 0 {
		return MutationOutcome{}, runtimeErr(ReasonInvalidParams)
	}
	if err := validateAction(r.Operation, r.Payload); err != nil {
		return MutationOutcome{}, err
	}
	if r.ExpectedGeneration != nil && *r.ExpectedGeneration < 0 {
		return MutationOutcome{}, runtimeErr(ReasonInvalidParams)
	}
	if slices.Contains(generationFencedOperations, r.Operation) && r.ExpectedGeneration == nil {
		return MutationOutcome{}, runtimeErr(ReasonInvalidParams)
	}
	snapshot, err := canonicalObject(r.Payload)
	if err != nil {
		return MutationOutcome{}, err
	}
	payload := json.RawMessage(snapshot)
	key, digest, err := mutationReceiptKey(r.PrincipalID, r.SessionID, r.RequestID, r.Operation, payload, r.ExpectedRevision, r.ExpectedGeneration)
	if err != nil {
		return MutationOutcome{}, err
	}
	var out MutationOutcome
	var gone *sessionGone
	err = s.Write(ctx, WritePatience, func(c Conn) error {
		out, gone = MutationOutcome{}, nil
		if err := checkEpoch(ctx, c, r.Epoch); err != nil {
			return err
		}
		replay, err := replayMutationReceipt(ctx, c, key, digest)
		if err != nil {
			return err
		}
		if replay != nil {
			out.Result = replay
			return nil
		}
		var revision, generation int64
		var archived any
		err = c.QueryRowContext(ctx, "SELECT runtime_revision, runtime_generation, archived FROM sessions WHERE id=?", r.SessionID).
			Scan(&revision, &generation, &archived)
		present := err == nil
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if !present && r.Operation != "import" {
			return runtimeErr(ReasonNotFound)
		}
		if revision != r.ExpectedRevision {
			return runtimeErr(ReasonRevisionConflict)
		}
		if r.ExpectedGeneration != nil && (!present || generation != *r.ExpectedGeneration) {
			return runtimeErr(ReasonStaleGeneration)
		}
		if r.LiveGuard != nil {
			targets := []string{r.SessionID}
			if r.Operation == "delete" {
				if targets, err = s.deleteTargets(ctx, c, r.SessionID); err != nil {
					return err
				}
			}
			if err := r.LiveGuard(targets); err != nil {
				return err
			}
		}
		if r.AuthorizeWrite != nil {
			if err := r.AuthorizeWrite(ctx, c); err != nil {
				return err
			}
		}
		if r.PrepareOnly {
			snap, err := s.localSnapshot(ctx, c, r.SessionID)
			if err != nil {
				return err
			}
			if r.Operation == "branch" {
				if err := s.requireIdle(ctx, c, uniqueIDs(r.SessionID, snap.Target)); err != nil {
					return err
				}
			}
			out.Snapshot = &snap
			return nil
		}
		affected, projection, err := s.applyAction(ctx, c, r.SessionID, r.Operation, payload, r.Prepared)
		if err != nil {
			return err
		}
		members, _ := objectMembers(payload)
		if r.Operation == "delete" {
			ids := []string{r.SessionID}
			if raw, has := memberValue(projection, "deleted_ids"); has {
				if listed, ok := stringList(raw); ok && len(listed) > 0 {
					ids = listed
				}
			}
			gone = &sessionGone{ids: ids, reason: "delete"}
		} else if flag, _ := memberValue(members, "archived"); strings.TrimSpace(string(flag)) == "true" && present && !cellTruthy(archived) {
			ids := slices.Clone(affected)
			if len(ids) == 0 {
				ids = []string{r.SessionID}
			}
			gone = &sessionGone{ids: ids, reason: "archive"}
		}
		for _, target := range affected {
			if _, err := c.ExecContext(ctx, "UPDATE sessions SET runtime_revision=runtime_revision+1 WHERE id=?", target); err != nil {
				return err
			}
		}
		result := []configMember{{"session_id", rawText(r.SessionID)}}
		var updatedRevision, updatedGeneration int64
		err = c.QueryRowContext(ctx, "SELECT runtime_revision, runtime_generation FROM sessions WHERE id=?", r.SessionID).
			Scan(&updatedRevision, &updatedGeneration)
		switch {
		case err == nil:
			result = append(result, configMember{"revision", rawInt(updatedRevision)}, configMember{"execution_generation", rawInt(updatedGeneration)})
		case errors.Is(err, sql.ErrNoRows):
			executed := json.RawMessage("null")
			if r.ExpectedGeneration != nil {
				executed = rawInt(*r.ExpectedGeneration)
			}
			result = append(result, configMember{"revision", rawInt(r.ExpectedRevision + 1)}, configMember{"execution_generation", executed})
		default:
			return err
		}
		result = append(result, configMember{"operation", rawText(r.Operation)})
		for _, m := range projection {
			result = setConfigMember(result, m.key, m.value)
		}
		encoded, err := canonicalObject(rawObject(result))
		if err != nil {
			return err
		}
		stored, err := canonicalObject(rawObject([]configMember{{"digest", rawText(digest)}, {"result", json.RawMessage(encoded)}}))
		if err != nil {
			return err
		}
		if _, err := c.ExecContext(ctx, "INSERT INTO state_meta(key,value) VALUES(?,?)", key, stored); err != nil {
			return err
		}
		out.Result = json.RawMessage(encoded)
		return nil
	})
	if err != nil {
		return MutationOutcome{}, err
	}
	if gone != nil {
		s.emitSessionDeleted(ctx, gone.ids, gone.reason, r.SessionID, "")
	}
	return out, nil
}

func (s *Store) applyAction(ctx context.Context, c Conn, sessionID, operation string, payload json.RawMessage, prepared *PreparedMutation) ([]string, []configMember, error) {
	members, _ := objectMembers(payload)
	switch operation {
	case "compress":
		return s.compressIn(ctx, c, sessionID, prepared)
	case "model":
		return s.modelIn(ctx, c, sessionID, members, prepared)
	case "reasoning":
		return s.reasoningIn(ctx, c, sessionID, members, prepared)
	case "workspace":
		return s.workspaceIn(ctx, c, sessionID, prepared)
	case "editor_mcp":
		return s.editorMCPIn(ctx, c, sessionID, prepared)
	case "edit_policy":
		return s.editPolicyIn(ctx, c, sessionID, members, prepared)
	case "branch":
		return s.branchIn(ctx, c, sessionID, members, prepared)
	case "delete":
		return s.deleteIn(ctx, c, sessionID)
	case "import":
		return s.importIn(ctx, c, sessionID, members)
	case "rewind":
		return s.rewindMutationIn(ctx, c, sessionID, members)
	case "reset":
		return s.resetIn(ctx, c, sessionID)
	}
	affected := map[string]bool{}
	var result []configMember
	for _, m := range members {
		var ids []string
		var err error
		if m.key == "title" {
			if ids, err = s.setSessionTitleIn(ctx, c, sessionID, jsonString(m.value), TitleSourceUser); err != nil {
				return nil, nil, err
			}
			var title sql.NullString
			if err := c.QueryRowContext(ctx, "SELECT title FROM sessions WHERE id=?", sessionID).Scan(&title); err != nil {
				return nil, nil, err
			}
			stored := json.RawMessage("null")
			if title.Valid {
				stored = rawText(title.String)
			}
			result = append(result, configMember{"title", stored})
		} else {
			truthy := strings.TrimSpace(string(m.value)) == "true"
			column, stored := m.key, any(int64(boolInt(truthy)))
			if m.key == "unread" {
				column, stored = "last_read_at", 0.0
				if !truthy {
					stored = nowEpoch()
				}
			}
			if ids, err = setLineageColumnIn(ctx, c, column, sessionID, stored, ""); err != nil {
				return nil, nil, err
			}
			result = append(result, configMember{m.key, m.value})
		}
		for _, id := range ids {
			affected[id] = true
		}
	}
	return sortedKeys(affected), result, nil
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	slices.Sort(out)
	return out
}

func (s *Store) deleteIn(ctx context.Context, c Conn, sessionID string) ([]string, []configMember, error) {
	targets, err := s.deleteTargets(ctx, c, sessionID)
	if err != nil {
		return nil, nil, err
	}
	if err := s.requireIdle(ctx, c, targets); err != nil {
		return nil, nil, err
	}
	if err := retireSessions(ctx, c, targets, nil); err != nil {
		return nil, nil, err
	}
	for _, sid := range targets {
		if err := bumpConversationGeneration(ctx, c, sid, "session_reset"); err != nil {
			return nil, nil, err
		}
		if _, err := c.ExecContext(ctx, "UPDATE sessions SET parent_session_id=NULL, runtime_revision=runtime_revision+1 WHERE parent_session_id=?", sid); err != nil {
			return nil, nil, err
		}
		if _, err := c.ExecContext(ctx, "DELETE FROM messages WHERE session_id=?", sid); err != nil {
			return nil, nil, err
		}
	}
	for _, sid := range targets {
		if _, err := c.ExecContext(ctx, "DELETE FROM sessions WHERE id=?", sid); err != nil {
			return nil, nil, err
		}
	}
	if err := deleteUnreferencedSystemPrompts(ctx, c); err != nil {
		return nil, nil, err
	}
	return nil, []configMember{{"deleted_ids", rawTexts(targets)}}, nil
}

func summarizedByCompaction(ctx context.Context, c Conn, physical string, messageID int64) (bool, error) {
	var sessionID, role sql.NullString
	var active, compacted sql.Null[int64]
	err := c.QueryRowContext(ctx, "SELECT session_id, role, active, compacted FROM messages WHERE id=?", messageID).Scan(&sessionID, &role, &active, &compacted)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil || role.String != "user" {
		return false, err
	}
	if sessionID.String == physical {
		return active.V == 0 && compacted.V == 1, nil
	}
	if active.V != 1 && compacted.V != 1 {
		return false, nil
	}
	lineage := map[string]bool{physical: true}
	frontier := []string{physical}
	for range chainCap {
		next, err := expandCompressionLineageIDs(ctx, c, frontier)
		if err != nil {
			return false, err
		}
		frontier = nil
		for _, id := range next {
			if !lineage[id] {
				frontier = append(frontier, id)
			}
		}
		if len(frontier) == 0 {
			break
		}
		for _, id := range frontier {
			lineage[id] = true
		}
	}
	return lineage[sessionID.String], nil
}

func messageRowObject(r *msgRow, warnContext string) ([]configMember, error) {
	var out []configMember
	cols := make([]string, 0, len(r.has))
	for col := range r.has {
		cols = append(cols, col)
	}
	slices.Sort(cols)
	for _, col := range cols {
		var value json.RawMessage
		var err error
		switch col {
		case "display_identity", "display_order":
			continue
		case "id":
			value = rawInt(r.id)
		case "_compressed_summary":
			if !cellTruthy(r.compressedSummary) {
				continue
			}
			value = json.RawMessage("true")
		case "content":
			value, err = decodeContent(r.content).MarshalJSON()
		case "tool_calls":
			value = json.RawMessage("null")
			if cellTruthy(r.toolCalls) {
				if value = jsonCell(r.toolCalls, "Failed to deserialize tool_calls in "+warnContext+", falling back to []"); value == nil {
					value = json.RawMessage("[]")
				}
			} else if r.toolCalls != nil {
				value, err = cellJSON(r.toolCalls)
			}
		case "display_metadata":
			value = json.RawMessage("null")
			if r.displayMetadata != nil {
				if decoded := decodeDisplayMetadata(r.displayMetadata); decoded != nil {
					value = decoded
				}
			}
		default:
			cell := r.col(col)
			if _, isBytes := cell.([]byte); isBytes && r.field(col) == nil {
				continue
			}
			value, err = cellJSON(cell)
		}
		if err != nil {
			return nil, err
		}
		out = append(out, configMember{col, value})
	}
	return out, nil
}

func (s *Store) rewindMutationIn(ctx context.Context, c Conn, sessionID string, payload []configMember) ([]string, []configMember, error) {
	physical, err := localPhysicalTarget(ctx, c, sessionID)
	if err != nil {
		return nil, nil, err
	}
	if err := s.requireIdle(ctx, c, uniqueIDs(sessionID, physical)); err != nil {
		return nil, nil, err
	}
	rawTarget, _ := memberValue(payload, "target_message_id")
	messageID, _ := jsonInt(rawTarget)
	var role sql.NullString
	err = c.QueryRowContext(ctx, "SELECT role FROM messages WHERE id=? AND session_id=? AND active=1", messageID, physical).Scan(&role)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, nil, err
	}
	if err != nil || role.String != "user" {
		summarized, err := summarizedByCompaction(ctx, c, physical, messageID)
		if err != nil {
			return nil, nil, err
		}
		if summarized {
			return nil, nil, runtimeErrAt(ReasonInvalidParams, "target_message_id", compactedTargetDetail)
		}
		return nil, nil, runtimeErrAt(ReasonInvalidParams, "target_message_id", "target user message is no longer in session history")
	}
	carrier, err := queryMsgRow(c.QueryContext(ctx, "SELECT * FROM messages WHERE id=? AND session_id=? AND active=1", messageID, physical))
	if err != nil {
		return nil, nil, err
	}
	var live *Content
	if carrier != nil {
		split := splitUserOriginatedTurn(userTurn{content: decodeContent(carrier.content), displayKind: textCell(carrier.displayKind),
			summary: cellTruthy(carrier.compressedSummary)})
		if split.handoff && split.live {
			live = &split.liveContent
		}
	}
	preserve, _ := memberValue(payload, "preserve_compaction_handoff")
	out, err := s.rewindIn(ctx, c, physical, messageID, RewindPins{PreserveCompactionHandoff: live != nil || strings.TrimSpace(string(preserve)) == "true"})
	if err != nil {
		return nil, nil, err
	}
	target, err := messageRowObject(out.target, "rewind receipt")
	if err != nil {
		return nil, nil, err
	}
	content := decodeContent(out.target.content)
	if live != nil {
		content = *live
	}
	content = typedPromptContent(content, &Message{DisplayMetadata: decodeDisplayMetadata(out.target.displayMetadata)})
	encoded, err := content.MarshalJSON()
	if err != nil {
		return nil, nil, err
	}
	target = setConfigMember(target, "content", encoded)
	if _, err := c.ExecContext(ctx, "UPDATE sessions SET runtime_generation=runtime_generation+1 WHERE id=?", sessionID); err != nil {
		return nil, nil, err
	}
	head, replacement := json.RawMessage("null"), json.RawMessage("null")
	if out.newHead.Valid {
		head = rawInt(out.newHead.V)
	}
	if out.replacementID.Valid {
		replacement = rawInt(out.replacementID.V)
	}
	return uniqueIDs(sessionID, physical), []configMember{{"rewound_count", rawInt(int64(len(out.ids)))},
		{"target_message", rawObject(target)}, {"new_head_id", head}, {"replacement_message_id", replacement}}, nil
}

func (s *Store) importIn(ctx context.Context, c Conn, sessionID string, payload []configMember) ([]string, []configMember, error) {
	raw, _ := memberValue(payload, "sessions")
	rows, _ := arrayElements(raw)
	if len(rows) > importMaxSessions {
		return nil, nil, runtimeErr(ReasonInvalidParams)
	}
	normalized, problems := ValidateImportPayload(rows)
	if len(problems) > 0 || len(normalized) == 0 || normalized[0].ID != sessionID {
		return nil, nil, runtimeErr(ReasonInvalidParams)
	}
	var imported, skipped []string
	var parents [][2]string
	for _, item := range normalized {
		sid := item.ID
		retired, err := retiredIn(ctx, c, sid)
		if err != nil {
			return nil, nil, err
		}
		if retired {
			return nil, nil, runtimeErrAt(ReasonAdmissionConflict, "sessions", "session "+sid+" was deleted here; a deleted session is not restored")
		}
		exists, err := sessionExists(ctx, c, sid)
		if err != nil {
			return nil, nil, err
		}
		if exists {
			skipped = append(skipped, sid)
			continue
		}
		if err := s.importSessionRow(ctx, c, item, sid); err != nil {
			return nil, nil, err
		}
		if item.ParentID.Valid && item.ParentID.V != "" {
			parents = append(parents, [2]string{sid, item.ParentID.V})
		}
		imported = append(imported, sid)
	}
	fresh := map[string]bool{}
	for _, sid := range imported {
		fresh[sid] = true
	}
	var kept [][2]string
	foreign := 0
	for _, link := range parents {
		if fresh[link[1]] {
			kept = append(kept, link)
			continue
		}
		parentBinding, err := historyBinding(ctx, c, link[1])
		if err != nil {
			return nil, nil, err
		}
		childBinding, err := historyBinding(ctx, c, link[0])
		if err != nil {
			return nil, nil, err
		}
		if !parentBinding.Valid || parentBinding != childBinding {
			foreign++
			continue
		}
		kept = append(kept, link)
	}
	detached, err := attachImportParents(ctx, c, kept)
	if err != nil {
		return nil, nil, err
	}
	return imported, []configMember{{"ok", json.RawMessage("true")}, {"imported", rawInt(int64(len(imported)))},
		{"skipped", rawInt(int64(len(skipped)))}, {"imported_ids", rawTexts(imported)}, {"skipped_ids", rawTexts(skipped)},
		{"detached", rawInt(int64(detached + foreign))}, {"errors", json.RawMessage("[]")}}, nil
}

func sessionExists(ctx context.Context, c Conn, sessionID string) (bool, error) {
	var one int
	err := c.QueryRowContext(ctx, "SELECT 1 FROM sessions WHERE id=?", sessionID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

type ChatPickParams struct {
	RequestID          string
	Operation          string
	Payload            json.RawMessage
	ExpectedRevision   int64
	ExpectedGeneration int64
}

type ChatPickPrepared struct {
	Receipt json.RawMessage
	Row     Session
}

func ChatPickKey(principalID, sessionID string, p ChatPickParams) (string, string, error) {
	if err := checkText(principalID, sessionID, p.RequestID); err != nil {
		return "", "", err
	}
	if p.ExpectedRevision < 0 || p.ExpectedGeneration < 0 {
		return "", "", runtimeErr(ReasonInvalidParams)
	}
	payload, err := canonicalObject(p.Payload)
	if err != nil {
		return "", "", err
	}
	generation := p.ExpectedGeneration
	return mutationReceiptKey(principalID, sessionID, p.RequestID, p.Operation, json.RawMessage(payload), p.ExpectedRevision, &generation)
}

func fencedChatRow(ctx context.Context, c Conn, sessionID string, p ChatPickParams) (Session, error) {
	rows, err := querySessions(c.QueryContext(ctx, "SELECT * FROM sessions WHERE id=?", sessionID))
	if err != nil {
		return Session{}, err
	}
	if len(rows) == 0 {
		return Session{}, runtimeErr(ReasonNotFound)
	}
	if rows[0].RuntimeRevision != p.ExpectedRevision {
		return Session{}, runtimeErr(ReasonRevisionConflict)
	}
	if rows[0].RuntimeGeneration != p.ExpectedGeneration {
		return Session{}, runtimeErr(ReasonStaleGeneration)
	}
	return rows[0], nil
}

func (s *Store) PrepareChatPick(ctx context.Context, epoch int64, key, digest, sessionID string, p ChatPickParams) (ChatPickPrepared, error) {
	var out ChatPickPrepared
	err := s.Read(ctx, func(c Conn) error {
		out = ChatPickPrepared{}
		if err := checkEpoch(ctx, c, epoch); err != nil {
			return err
		}
		replay, err := replayMutationReceipt(ctx, c, key, digest)
		if err != nil || replay != nil {
			out.Receipt = replay
			return err
		}
		out.Row, err = fencedChatRow(ctx, c, sessionID, p)
		return err
	})
	return out, err
}

func (s *Store) CommitChatPick(ctx context.Context, epoch int64, key, digest, sessionID string, p ChatPickParams, result json.RawMessage) (json.RawMessage, error) {
	base, ok := objectMembers(result)
	if !ok {
		return nil, runtimeErr(ReasonInvalidParams)
	}
	var committed json.RawMessage
	err := s.Write(ctx, WritePatience, func(c Conn) error {
		committed = nil
		if err := checkEpoch(ctx, c, epoch); err != nil {
			return err
		}
		replay, err := replayMutationReceipt(ctx, c, key, digest)
		if err != nil || replay != nil {
			committed = replay
			return err
		}
		if _, err := fencedChatRow(ctx, c, sessionID, p); err != nil {
			return err
		}
		if _, err := c.ExecContext(ctx, "UPDATE sessions SET runtime_revision=runtime_revision+1 WHERE id=?", sessionID); err != nil {
			return err
		}
		var revision int64
		if err := c.QueryRowContext(ctx, "SELECT runtime_revision FROM sessions WHERE id=?", sessionID).Scan(&revision); err != nil {
			return err
		}
		encoded, err := canonicalObject(rawObject(withMember(base, "revision", rawInt(revision))))
		if err != nil {
			return err
		}
		stored, err := canonicalObject(rawObject([]configMember{{"digest", rawText(digest)}, {"result", json.RawMessage(encoded)}}))
		if err != nil {
			return err
		}
		if _, err := c.ExecContext(ctx, "INSERT INTO state_meta(key,value) VALUES(?,?)", key, stored); err != nil {
			return err
		}
		committed = json.RawMessage(encoded)
		return nil
	})
	return committed, err
}
