package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

const deferredPickPrefix = "gateway.deferred_pick.v1."

const mutationReceiptsSQL = "FROM state_meta WHERE key GLOB 'gateway.mutation.v1.*' AND json_valid(value) "

type LocalSnapshot struct {
	Receipt        json.RawMessage
	Target         string
	TargetRevision int64
}

func (s *Store) localSnapshot(ctx context.Context, c Conn, sessionID string) (LocalSnapshot, error) {
	saved, found, err := metaValue(ctx, c, localPolicyPrefix+sessionID)
	if err != nil {
		return LocalSnapshot{}, err
	}
	if !found {
		return LocalSnapshot{}, runtimeErr(ReasonRuntimeCoordinationRequired)
	}
	receipt, ok := objectMembers(json.RawMessage(saved))
	if !ok {
		return LocalSnapshot{}, runtimeErr(ReasonStorageUnavailable)
	}
	target, err := validateLocalLineage(ctx, c, receipt)
	if err != nil {
		return LocalSnapshot{}, err
	}
	if err := s.requireTargetAdvanceable(ctx, c, uniqueIDs(sessionID, target)); err != nil {
		return LocalSnapshot{}, err
	}
	snap := LocalSnapshot{Receipt: json.RawMessage(saved), Target: target}
	err = c.QueryRowContext(ctx, "SELECT runtime_revision FROM sessions WHERE id=?", target).Scan(&snap.TargetRevision)
	return snap, err
}

func (s *Store) validatePrepared(ctx context.Context, c Conn, sessionID string, prepared *PreparedMutation) (LocalSnapshot, error) {
	if prepared == nil {
		return LocalSnapshot{}, runtimeErr(ReasonRuntimeCoordinationRequired)
	}
	current, err := s.localSnapshot(ctx, c, sessionID)
	if err != nil {
		return LocalSnapshot{}, err
	}
	want := prepared.Snapshot
	if current.Target != want.Target || current.TargetRevision != want.TargetRevision ||
		jsonCompareKey(current.Receipt) != jsonCompareKey(want.Receipt) {
		return LocalSnapshot{}, runtimeErr(ReasonRevisionConflict)
	}
	return current, nil
}

func preparedValue(prepared *PreparedMutation, keys ...string) (json.RawMessage, error) {
	members, ok := objectMembers(prepared.Values)
	if !ok {
		return nil, runtimeErr(ReasonInvalidParams)
	}
	var value json.RawMessage
	for i, key := range keys {
		v, has := memberValue(members, key)
		if !has {
			return nil, runtimeErr(ReasonInvalidParams)
		}
		value = v
		if i < len(keys)-1 {
			if members, ok = objectMembers(v); !ok {
				return nil, runtimeErr(ReasonInvalidParams)
			}
		}
	}
	return value, nil
}

func (s *Store) commitPolicy(ctx context.Context, c Conn, sessionID string, prepared *PreparedMutation) (LocalSnapshot, int64, error) {
	snap, err := s.validatePrepared(ctx, c, sessionID, prepared)
	if err != nil {
		return LocalSnapshot{}, 0, err
	}
	policy, err := preparedValue(prepared, "policy")
	if err != nil {
		return LocalSnapshot{}, 0, err
	}
	receipt, _ := objectMembers(snap.Receipt)
	stored, err := canonicalObject(rawObject(withMember(receipt, "policy", policy)))
	if err != nil {
		return LocalSnapshot{}, 0, err
	}
	if _, err := c.ExecContext(ctx, "UPDATE state_meta SET value=? WHERE key=?", stored, localPolicyPrefix+sessionID); err != nil {
		return LocalSnapshot{}, 0, err
	}
	generation, err := bumpRuntimeGeneration(ctx, c, sessionID)
	return snap, generation, err
}

func bumpRuntimeGeneration(ctx context.Context, c Conn, sessionID string) (int64, error) {
	if _, err := c.ExecContext(ctx, "UPDATE sessions SET runtime_generation=runtime_generation+1 WHERE id=?", sessionID); err != nil {
		return 0, err
	}
	var generation int64
	err := c.QueryRowContext(ctx, "SELECT runtime_generation FROM sessions WHERE id=?", sessionID).Scan(&generation)
	return generation, err
}

func (s *Store) modelIn(ctx context.Context, c Conn, sessionID string, payload []configMember, prepared *PreparedMutation) ([]string, []configMember, error) {
	snap, generation, err := s.commitPolicy(ctx, c, sessionID, prepared)
	if err != nil {
		return nil, nil, err
	}
	model, err := preparedValue(prepared, "policy", "model")
	if err != nil {
		return nil, nil, err
	}
	configJSON, err := preparedValue(prepared, "policy", "config_json")
	if err != nil {
		return nil, nil, err
	}
	config, ok := objectMembers(json.RawMessage(jsonString(configJSON)))
	modelConfig, _ := memberValue(config, "model")
	modelMembers, isObject := objectMembers(modelConfig)
	provider, hasProvider := memberValue(modelMembers, "provider")
	if !ok || !isObject || !hasProvider {
		return nil, nil, runtimeErr(ReasonInvalidParams)
	}
	var stored sql.NullString
	if err := c.QueryRowContext(ctx, "SELECT model_config FROM sessions WHERE id=?", snap.Target).Scan(&stored); err != nil {
		return nil, nil, err
	}
	text := stored.String
	if text == "" {
		text = "{}"
	}
	current, ok := objectMembers(json.RawMessage(text))
	if !ok {
		return nil, nil, runtimeErr(ReasonStorageUnavailable)
	}
	updated, err := canonicalObject(rawObject(withMember(current, "provider", provider)))
	if err != nil {
		return nil, nil, err
	}
	if _, err := c.ExecContext(ctx, "UPDATE sessions SET model=?,model_config=? WHERE id=?", wireBind(model), updated, snap.Target); err != nil {
		return nil, nil, err
	}
	key := deferredPickPrefix + sessionID
	saved, found, err := metaValue(ctx, c, key)
	if err != nil {
		return nil, nil, err
	}
	level := json.RawMessage(nil)
	if _, picked := memberValue(payload, "reasoning"); found && !picked {
		pick, _ := objectMembers(json.RawMessage(saved))
		if v, has := memberValue(pick, "reasoning"); has && !jsonIsNull(v) {
			level = v
		}
	}
	if level == nil {
		if err := deleteMeta(ctx, c, key); err != nil {
			return nil, nil, err
		}
	} else {
		value, err := canonicalObject(rawObject([]configMember{{"reasoning", level}}))
		if err != nil {
			return nil, nil, err
		}
		if _, err := c.ExecContext(ctx, "UPDATE state_meta SET value=? WHERE key=?", value, key); err != nil {
			return nil, nil, err
		}
	}
	projection := []configMember{{"model", model}, {"provider", provider}, {"execution_generation", rawInt(generation)}}
	if effort, has := memberValue(payload, "reasoning"); has {
		projection = append(projection, configMember{"effort", effort})
	}
	return uniqueIDs(sessionID, snap.Target), projection, nil
}

func (s *Store) reasoningIn(ctx context.Context, c Conn, sessionID string, payload []configMember, prepared *PreparedMutation) ([]string, []configMember, error) {
	snap, generation, err := s.commitPolicy(ctx, c, sessionID, prepared)
	if err != nil {
		return nil, nil, err
	}
	key := deferredPickPrefix + sessionID
	saved, found, err := metaValue(ctx, c, key)
	if err != nil {
		return nil, nil, err
	}
	if found {
		pick, ok := objectMembers(json.RawMessage(saved))
		if !ok {
			return nil, nil, runtimeErr(ReasonStorageUnavailable)
		}
		pick = dropMember(pick, "reasoning")
		if inner, has := memberValue(pick, "payload"); has {
			if members, isObject := objectMembers(inner); isObject {
				pick = withMember(pick, "payload", rawObject(dropMember(members, "reasoning")))
			}
		}
		if _, kept := memberValue(pick, "application_id"); kept {
			value, err := canonicalObject(rawObject(pick))
			if err != nil {
				return nil, nil, err
			}
			if _, err := c.ExecContext(ctx, "UPDATE state_meta SET value=? WHERE key=?", value, key); err != nil {
				return nil, nil, err
			}
		} else if err := deleteMeta(ctx, c, key); err != nil {
			return nil, nil, err
		}
	}
	effort, _ := memberValue(payload, "effort")
	return uniqueIDs(sessionID, snap.Target), []configMember{{"effort", effort}, {"execution_generation", rawInt(generation)}}, nil
}

func (s *Store) workspaceIn(ctx context.Context, c Conn, sessionID string, prepared *PreparedMutation) ([]string, []configMember, error) {
	snap, generation, err := s.commitPolicy(ctx, c, sessionID, prepared)
	if err != nil {
		return nil, nil, err
	}
	cwd, err := preparedValue(prepared, "policy", "cwd")
	if err != nil {
		return nil, nil, err
	}
	branch, err := preparedValue(prepared, "git", "branch")
	if err != nil {
		return nil, nil, err
	}
	root, err := preparedValue(prepared, "git", "root")
	if err != nil {
		return nil, nil, err
	}
	orNull := func(v json.RawMessage) json.RawMessage {
		if !jsonTruthy(v) {
			return json.RawMessage("null")
		}
		return v
	}
	branch, root = orNull(branch), orNull(root)
	ids, err := LocalConversationIDsIn(ctx, c, sessionID)
	if err != nil {
		return nil, nil, err
	}
	for _, sid := range uniqueIDs(append(ids, snap.Target)...) {
		if _, err := c.ExecContext(ctx, "UPDATE sessions SET cwd=?, git_branch=?, git_repo_root=?, "+
			"git_metadata_generation=COALESCE(git_metadata_generation, 0)+1 WHERE id=?", wireBind(cwd), wireBind(branch), wireBind(root), sid); err != nil {
			return nil, nil, err
		}
	}
	return uniqueIDs(sessionID, snap.Target), []configMember{{"cwd", cwd}, {"branch", branch}, {"git_repo_root", root},
		{"execution_generation", rawInt(generation)}}, nil
}

func (s *Store) editorMCPIn(ctx context.Context, c Conn, sessionID string, prepared *PreparedMutation) ([]string, []configMember, error) {
	snap, generation, err := s.commitPolicy(ctx, c, sessionID, prepared)
	if err != nil {
		return nil, nil, err
	}
	changed, err := preparedValue(prepared, "tools_changed")
	if err != nil {
		return nil, nil, err
	}
	servers, err := preparedValue(prepared, "mcp_servers")
	if err != nil {
		return nil, nil, err
	}
	return uniqueIDs(sessionID, snap.Target), []configMember{{"tools_changed", changed}, {"mcp_servers", servers},
		{"execution_generation", rawInt(generation)}}, nil
}

func (s *Store) editPolicyIn(ctx context.Context, c Conn, sessionID string, payload []configMember, prepared *PreparedMutation) ([]string, []configMember, error) {
	snap, generation, err := s.commitPolicy(ctx, c, sessionID, prepared)
	if err != nil {
		return nil, nil, err
	}
	policy, _ := memberValue(payload, "policy")
	return uniqueIDs(sessionID, snap.Target), []configMember{{"edit_approval_policy", policy}, {"execution_generation", rawInt(generation)}}, nil
}

func picksOn(ctx context.Context, c Conn, sessionID string, before *int64) ([]string, error) {
	query := "SELECT DISTINCT json_extract(value, '$.result.operation') " + mutationReceiptsSQL +
		"AND json_extract(value, '$.result.operation') IN ('model', 'reasoning', 'workspace') " +
		"AND COALESCE(json_extract(value, '$.result.status'), '') != 'deferred' " +
		"AND json_extract(value, '$.result.session_id')=?"
	args := []any{sessionID}
	if before != nil {
		query += " AND json_extract(value, '$.result.revision')<?"
		args = append(args, *before)
	}
	picked, err := queryStrings(ctx, c, query, args...)
	if err != nil {
		return nil, err
	}
	set := map[string]bool{}
	for _, p := range picked {
		set[p] = true
	}
	receipt, err := storedLocalReceipt(ctx, c, sessionID)
	if err != nil || receipt == nil {
		return sortedKeys(set), err
	}
	if raw, has := memberValue(receipt, "picks"); has {
		listed, ok := stringList(raw)
		if !ok {
			return nil, runtimeErr(ReasonStorageUnavailable)
		}
		for _, p := range listed {
			set[p] = true
		}
		return sortedKeys(set), nil
	}
	var parent sql.NullString
	var revision sql.Null[int64]
	err = c.QueryRowContext(ctx, "SELECT json_extract(value, '$.result.session_id'), json_extract(value, '$.result.revision') "+
		mutationReceiptsSQL+"AND json_extract(value, '$.result.operation')='branch' "+
		"AND json_extract(value, '$.result.branched_session_id')=?", sessionID).Scan(&parent, &revision)
	if errors.Is(err, sql.ErrNoRows) {
		return sortedKeys(set), nil
	}
	if err != nil {
		return nil, err
	}
	var bound *int64
	if revision.Valid {
		bound = &revision.V
	}
	inherited, err := picksOn(ctx, c, parent.String, bound)
	if err != nil {
		return nil, err
	}
	for _, p := range inherited {
		set[p] = true
	}
	return sortedKeys(set), nil
}

func (s *Store) PolicyPicks(ctx context.Context, sessionID string) ([]string, error) {
	var picks []string
	err := s.Read(ctx, func(c Conn) error {
		var err error
		picks, err = picksOn(ctx, c, sessionID, nil)
		return err
	})
	return picks, err
}

func (s *Store) CommitRefreshedPolicy(ctx context.Context, epoch int64, sessionID string, old, updated json.RawMessage, generation int64) (int64, error) {
	var committed int64
	err := s.Write(ctx, WritePatience, func(c Conn) error {
		if err := checkEpoch(ctx, c, epoch); err != nil {
			return err
		}
		snap, err := s.localSnapshot(ctx, c, sessionID)
		if err != nil {
			return err
		}
		if err := s.requireIdle(ctx, c, uniqueIDs(sessionID, snap.Target)); err != nil {
			return err
		}
		var current int64
		err = c.QueryRowContext(ctx, "SELECT runtime_generation FROM sessions WHERE id=?", sessionID).Scan(&current)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		receipt, _ := objectMembers(snap.Receipt)
		policy, _ := memberValue(receipt, "policy")
		if jsonCompareKey(policy) != jsonCompareKey(old) || err != nil || current != generation {
			return runtimeErr(ReasonRevisionConflict)
		}
		_, committed, err = s.commitPolicy(ctx, c, sessionID, &PreparedMutation{Snapshot: snap,
			Values: rawObject([]configMember{{"policy", updated}})})
		return err
	})
	return committed, err
}

func branchBoundary(ctx context.Context, c Conn, target string, through *int64) (int64, error) {
	if through == nil {
		var top int64
		err := c.QueryRowContext(ctx, "SELECT COALESCE(MAX(id), 0) FROM messages WHERE session_id=? AND active=1", target).Scan(&top)
		return top, err
	}
	boundary := *through
	var role sql.NullString
	err := c.QueryRowContext(ctx, "SELECT role FROM messages WHERE id=? AND session_id=? AND active=1", boundary, target).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, runtimeErr(ReasonInvalidParams)
	}
	if err != nil || role.String != "assistant" {
		return boundary, err
	}
	rows, err := queryMsgRows(c.QueryContext(ctx, "SELECT id, role FROM messages WHERE session_id=? AND active=1 AND id>? ORDER BY id", target, boundary))
	if err != nil {
		return 0, err
	}
	for _, r := range rows {
		if r.roleText() != "tool" {
			break
		}
		boundary = r.id
	}
	return boundary, nil
}

func childPolicy(policy []configMember, child string, prepared *PreparedMutation) ([]configMember, error) {
	if _, has := memberValue(policy, "edit_approval_policy"); has {
		policy = withMember(policy, "edit_approval_policy", rawText("ask"))
	}
	editor, _ := memberValue(policy, "editor_mcp_json")
	if prepared == nil {
		if jsonTruthy(editor) {
			return nil, runtimeErr(ReasonRuntimeCoordinationRequired)
		}
		return policy, nil
	}
	preparedChild, err := preparedValue(prepared, "child")
	if err != nil {
		return nil, err
	}
	parentEditor, err := preparedValue(prepared, "parent_editor_mcp_json")
	if err != nil {
		return nil, err
	}
	if editor == nil {
		editor = json.RawMessage("null")
	}
	if !jsonIsString(preparedChild) || jsonString(preparedChild) != child || jsonCompareKey(editor) != jsonCompareKey(parentEditor) {
		return nil, runtimeErr(ReasonRevisionConflict)
	}
	for _, key := range []string{"editor_mcp_json", "toolsets", "config_secret_ref"} {
		v, err := preparedValue(prepared, key)
		if err != nil {
			return nil, err
		}
		policy = withMember(policy, key, v)
	}
	return policy, nil
}

func policyText(policy []configMember, key string) string {
	text, _ := memberText(policy, key)
	return text
}

func (s *Store) branchIn(ctx context.Context, c Conn, sessionID string, payload []configMember, prepared *PreparedMutation) ([]string, []configMember, error) {
	hooks := s.gatewayHooks()
	if hooks.BranchEntry == nil {
		return nil, nil, ErrNoGatewayHooks
	}
	saved, err := storedLocalReceipt(ctx, c, sessionID)
	if err != nil {
		return nil, nil, err
	}
	if saved == nil {
		return nil, nil, runtimeErr(ReasonRuntimeCoordinationRequired)
	}
	target, err := validateLocalLineage(ctx, c, saved)
	if err != nil {
		return nil, nil, err
	}
	if err := s.requireIdle(ctx, c, uniqueIDs(sessionID, target)); err != nil {
		return nil, nil, err
	}
	var requestID string
	if prepared != nil {
		raw, err := preparedValue(prepared, "request_id")
		if err != nil {
			return nil, nil, err
		}
		requestID = jsonString(raw)
	} else {
		id, err := mintUID()
		if err != nil {
			return nil, nil, err
		}
		requestID = "branch:" + id
	}
	profileID, _ := memberText(saved, "profile_id")
	principalID, _ := memberText(saved, "principal_id")
	child, err := LocalIdentity(profileID, principalID, requestID)
	if err != nil {
		return nil, nil, err
	}
	rawPolicy, _ := memberValue(saved, "policy")
	parentPolicy, ok := objectMembers(rawPolicy)
	if !ok {
		return nil, nil, runtimeErr(ReasonStorageUnavailable)
	}
	policy, err := childPolicy(parentPolicy, child, prepared)
	if err != nil {
		return nil, nil, err
	}
	route, err := hooks.BranchEntry(rawObject(saved), child)
	if err != nil {
		return nil, nil, err
	}
	parent, err := readPublishParent(ctx, c, target)
	if err != nil {
		return nil, nil, err
	}
	if parent == nil {
		return nil, nil, runtimeErr(ReasonStorageUnavailable)
	}
	var promptHash, parentTitle sql.NullString
	if err := c.QueryRowContext(ctx, "SELECT system_prompt_hash, title FROM sessions WHERE id=?", target).Scan(&promptHash, &parentTitle); err != nil {
		return nil, nil, err
	}
	if err := s.publishChildSessionRow(ctx, c, parent, childRow{parentID: target, childID: child, source: policyText(policy, "source"),
		model: policyText(policy, "model"), modelConfig: []configMember{{"_branched_from", rawText(target)}},
		cwd: policyText(policy, "cwd"), profileName: textCell(parent.profileName)}); err != nil {
		return nil, nil, err
	}
	origin, err := canonicalObject(route.Origin)
	if err != nil {
		return nil, nil, err
	}
	if _, err := c.ExecContext(ctx, "UPDATE sessions SET session_key=?,chat_id=?,origin_json=?,system_prompt_hash=? WHERE id=?",
		route.Route, child, origin, promptHash, child); err != nil {
		return nil, nil, err
	}
	var through *int64
	if raw, has := memberValue(payload, "through_message_id"); has {
		n, _ := jsonInt(raw)
		through = &n
	}
	boundary, err := branchBoundary(ctx, c, target, through)
	if err != nil {
		return nil, nil, err
	}
	ids, toolCalls, err := tailRows(ctx, c, "SELECT id,tool_calls FROM messages WHERE session_id=? AND active=1 AND id<=? ORDER BY id", target, boundary)
	if err != nil {
		return nil, nil, err
	}
	if len(ids) > 0 {
		if err := s.cloneMessageRows(ctx, c, ids, child); err != nil {
			return nil, nil, err
		}
	}
	if _, err := c.ExecContext(ctx, "UPDATE sessions SET message_count=?,tool_call_count=? WHERE id=?", len(ids), toolCalls, child); err != nil {
		return nil, nil, err
	}
	if title, _ := memberText(payload, "title"); title != "" {
		if _, err := s.setSessionTitleIn(ctx, c, child, title, TitleSourceUser); err != nil {
			return nil, nil, err
		}
	} else {
		base := parentTitle.String
		if base == "" {
			base = "branch"
		}
		next, err := nextTitleInLineage(ctx, c, base)
		if err != nil {
			return nil, nil, err
		}
		if _, err := s.setSessionTitleIn(ctx, c, child, next, TitleSourceDerived); err != nil {
			return nil, nil, err
		}
	}
	picks, err := picksOn(ctx, c, sessionID, nil)
	if err != nil {
		return nil, nil, err
	}
	profileRaw, _ := memberValue(saved, "profile_id")
	principalRaw, _ := memberValue(saved, "principal_id")
	receipt := []configMember{{"profile_id", profileRaw}, {"principal_id", principalRaw}, {"request_id", rawText(requestID)},
		{"session_id", rawText(child)}, {"route", rawText(route.Route)}, {"entry", route.Entry}, {"policy", rawObject(policy)},
		{"picks", rawTexts(picks)}}
	entry, err := canonicalObject(route.Entry)
	if err != nil {
		return nil, nil, err
	}
	if _, err := c.ExecContext(ctx, "INSERT INTO gateway_routing(scope,session_key,entry_json,updated_at) VALUES('',?,?,?)",
		route.Route, entry, route.UpdatedAt); err != nil {
		return nil, nil, err
	}
	stored, err := canonicalObject(rawObject(receipt))
	if err != nil {
		return nil, nil, err
	}
	if _, err := c.ExecContext(ctx, "INSERT INTO state_meta(key,value) VALUES(?,?)", localPolicyPrefix+child, stored); err != nil {
		return nil, nil, err
	}
	return []string{sessionID}, []configMember{{"branched_session_id", rawText(child)}, {"copied_messages", rawInt(int64(len(ids)))}}, nil
}

func (s *Store) compressIn(ctx context.Context, c Conn, sessionID string, prepared *PreparedMutation) ([]string, []configMember, error) {
	snap, err := s.validatePrepared(ctx, c, sessionID, prepared)
	if err != nil {
		return nil, nil, err
	}
	receipt, _ := objectMembers(snap.Receipt)
	rawPolicy, _ := memberValue(receipt, "policy")
	policy, _ := objectMembers(rawPolicy)
	inPlace, err := preparedValue(prepared, "in_place")
	if err != nil {
		return nil, nil, err
	}
	child := snap.Target
	var affected []string
	if jsonTruthy(inPlace) {
		if _, err := s.ArchiveAndCompactIn(ctx, c, snap.Target, prepared.Messages, CompactOptions{}); err != nil {
			return nil, nil, err
		}
		affected = uniqueIDs(sessionID, snap.Target)
	} else {
		if child, err = mintUID(); err != nil {
			return nil, nil, err
		}
		if err := s.PublishCompressionChildIn(ctx, c, CompressionChild{ParentSessionID: snap.Target, ChildSessionID: child,
			Source: policyText(policy, "source"), Model: policyText(policy, "model"), Messages: prepared.Messages,
			ModelConfig: rawObject([]configMember{{"_compressed_from", rawText(snap.Target)}}), Cwd: policyText(policy, "cwd"),
			NoLeaseRequired: true}); err != nil {
			return nil, nil, err
		}
	}
	generation, err := bumpRuntimeGeneration(ctx, c, sessionID)
	if err != nil {
		return nil, nil, err
	}
	summary := json.RawMessage("null")
	if members, ok := objectMembers(prepared.Values); ok {
		if v, has := memberValue(members, "summary"); has {
			summary = v
		}
	}
	return affected, []configMember{{"target_session_id", rawText(child)}, {"previous_target_session_id", rawText(snap.Target)},
		{"execution_generation", rawInt(generation)}, {"message_count", rawInt(int64(len(prepared.Messages)))}, {"summary", summary}}, nil
}

func (s *Store) publishReset(ctx context.Context, c Conn, parentID, childID string, policy []configMember, entry []configMember) error {
	parent, err := readPublishParent(ctx, c, parentID)
	if err != nil {
		return err
	}
	if parent == nil {
		return runtimeErr(ReasonStorageUnavailable)
	}
	if err := s.publishChildSessionRow(ctx, c, parent, childRow{parentID: parentID, childID: childID, source: policyText(policy, "source"),
		model: policyText(policy, "model"), modelConfig: []configMember{{"_reset_from", rawText(parentID)}},
		cwd: policyText(policy, "cwd"), profileName: textCell(parent.profileName)}); err != nil {
		return err
	}
	if _, err := c.ExecContext(ctx, "UPDATE sessions SET ended_at=?,end_reason='session_reset' WHERE id=?", nowEpoch(), parentID); err != nil {
		return err
	}
	if err := bumpConversationGeneration(ctx, c, parentID, "session_reset"); err != nil {
		return err
	}
	return advanceLocalTarget(ctx, c, parentID, childID, entry)
}

func (s *Store) resetIn(ctx context.Context, c Conn, sessionID string) ([]string, []configMember, error) {
	hooks := s.gatewayHooks()
	if hooks.ResetEntry == nil {
		return nil, nil, ErrNoGatewayHooks
	}
	receipt, err := storedLocalReceipt(ctx, c, sessionID)
	if err != nil {
		return nil, nil, err
	}
	if receipt == nil {
		return nil, nil, runtimeErr(ReasonRuntimeCoordinationRequired)
	}
	target, err := validateLocalLineage(ctx, c, receipt)
	if err != nil {
		return nil, nil, err
	}
	lineage, err := runtimeLineage(ctx, c, sessionID)
	if err != nil {
		return nil, nil, err
	}
	if err := discardOrphanWorkersOnReset(ctx, c, lineage); err != nil {
		return nil, nil, err
	}
	if err := s.requireTargetAdvanceable(ctx, c, uniqueIDs(sessionID, target)); err != nil {
		return nil, nil, err
	}
	rawPolicy, _ := memberValue(receipt, "policy")
	policy, ok := objectMembers(rawPolicy)
	if !ok {
		return nil, nil, runtimeErr(ReasonStorageUnavailable)
	}
	child, err := mintUID()
	if err != nil {
		return nil, nil, err
	}
	previous, _ := memberValue(receipt, "entry")
	rawEntry, err := hooks.ResetEntry(previous, child)
	if err != nil {
		return nil, nil, err
	}
	entry, ok := objectMembers(rawEntry)
	if !ok {
		return nil, nil, runtimeErr(ReasonInvalidParams)
	}
	if err := s.publishReset(ctx, c, target, child, policy, entry); err != nil {
		return nil, nil, err
	}
	generation, err := bumpRuntimeGeneration(ctx, c, sessionID)
	if err != nil {
		return nil, nil, err
	}
	return nil, []configMember{{"target_session_id", rawText(child)}, {"previous_target_session_id", rawText(target)},
		{"execution_generation", rawInt(generation)}}, nil
}

func (s *Store) ResetLocalTarget(ctx context.Context, epoch int64, parentSessionID string, entry json.RawMessage) error {
	members, ok := objectMembers(entry)
	childID, hasChild := memberText(members, "session_id")
	if !ok || !hasChild {
		return runtimeErr(ReasonInvalidParams)
	}
	return s.Write(ctx, WritePatience, func(c Conn) error {
		if err := checkEpoch(ctx, c, epoch); err != nil {
			return err
		}
		var chatID, endReason sql.NullString
		err := c.QueryRowContext(ctx, "SELECT chat_id, end_reason FROM sessions WHERE id=?", parentSessionID).Scan(&chatID, &endReason)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && endReason.String == "compression") {
			return runtimeErr(ReasonAdmissionConflict)
		}
		if err != nil {
			return err
		}
		receipt, err := storedLocalReceipt(ctx, c, chatID.String)
		if err != nil {
			return err
		}
		if receipt == nil {
			return runtimeErr(ReasonStorageUnavailable)
		}
		logical, _ := memberText(receipt, "session_id")
		target, err := validateLocalLineage(ctx, c, receipt)
		if err != nil {
			return err
		}
		if target != parentSessionID {
			return runtimeErr(ReasonAdmissionConflict)
		}
		if err := requireNotExecuting(ctx, c, uniqueIDs(logical, parentSessionID)); err != nil {
			return err
		}
		rawPolicy, _ := memberValue(receipt, "policy")
		policy, ok := objectMembers(rawPolicy)
		if !ok {
			return runtimeErr(ReasonStorageUnavailable)
		}
		if err := s.publishReset(ctx, c, parentSessionID, childID, policy, members); err != nil {
			return err
		}
		_, err = c.ExecContext(ctx, "UPDATE sessions SET runtime_generation=runtime_generation+1 WHERE id=?", logical)
		return err
	})
}
