package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"
)

type LocalSessionHooks struct {
	NormaliseClientEnv func(env, rules json.RawMessage) (json.RawMessage, error)
}

var ErrNoLocalSessionPolicy = errors.New("store was opened without the client environment normaliser")

type LocalCreateEffect interface {
	Read() error
	Publish()
	Withdraw()
}

type LocalCommit struct {
	Epoch         int64
	Receipt       json.RawMessage
	ClientRequest json.RawMessage
	OnCreate      LocalCreateEffect
}

type MovedReceipt struct {
	Target string
	Source string
}

const laneIdentityColumns = "session_key, source, user_id, chat_id, chat_type, thread_id, display_name, origin_json, transport_profile"

var legacyLocalSources = []string{"cli", "tui", "gui", "desktop", "oneshot", "tool", "acp"}

var notClientLabelSet = []string{"cron", "subagent", "kanban", "bot_room", "a2a", "webhook", "api_server",
	"import", "recovered", "curator", "batch", "homeassistant", "webui", "local", "unknown"}

func (s *Store) localHooks() LocalSessionHooks { return s.tables.state.options.LocalSessions }

func jsonCellValue(raw json.RawMessage) any {
	if b, ok := jsonBool(raw); ok {
		if b {
			return int64(1)
		}
		return int64(0)
	}
	return wireBind(raw)
}

func memberIn(members []configMember, key string) (json.RawMessage, error) {
	v, ok := memberValue(members, key)
	if !ok {
		return nil, errors.New("local receipt has no " + key)
	}
	return v, nil
}

func objectIn(members []configMember, key string) ([]configMember, error) {
	v, err := memberIn(members, key)
	if err != nil {
		return nil, err
	}
	out, ok := objectMembers(v)
	if !ok {
		return nil, errors.New("local receipt " + key + " is not an object")
	}
	return out, nil
}

func parseISOTime(text string) (time.Time, error) {
	layouts := []string{"2006-01-02T15:04:05.999999999Z07:00", "2006-01-02 15:04:05.999999999Z07:00",
		"2006-01-02T15:04:05Z07:00", "2006-01-02 15:04:05Z07:00", "2006-01-02T15:04Z07:00", "2006-01-02 15:04Z07:00"}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, text); err == nil {
			return t, nil
		}
	}
	naive := []string{"2006-01-02T15:04:05.999999999", "2006-01-02 15:04:05.999999999", "2006-01-02T15:04:05",
		"2006-01-02 15:04:05", "2006-01-02T15:04", "2006-01-02 15:04", "2006-01-02T15", "2006-01-02"}
	for _, layout := range naive {
		if t, err := time.ParseInLocation(layout, text, time.Local); err == nil {
			return t, nil
		}
	}
	return time.Time{}, errors.New("invalid isoformat string: " + quotedText(text))
}

func dropKeys(members []configMember, keys ...string) []configMember {
	for _, k := range keys {
		members = dropMember(members, k)
	}
	return members
}

func clientRequestOf(requestJSON string, derived json.RawMessage) ([]configMember, error) {
	request, ok := objectMembers(json.RawMessage(requestJSON))
	if !ok {
		return nil, errors.New("create request is not a JSON object")
	}
	request = dropMember(request, "desktop_local")
	plain := derived == nil || jsonIsNull(derived)
	if plain {
		request = dropMember(request, "route_derived")
	}
	if _, has := memberValue(request, "model_requested"); has {
		request = dropMember(request, "model")
		if plain {
			request = dropMember(request, "base_url")
		} else if names, isList := arrayElements(derived); isList {
			for _, n := range names {
				request = dropMember(request, jsonString(n))
			}
		} else if jsonIsString(derived) {
			for _, r := range jsonString(derived) {
				request = dropMember(request, string(r))
			}
		}
	}
	return request, nil
}

func SameCreateRequest(savedJSON, retryJSON string) (bool, error) {
	saved, ok := objectMembers(json.RawMessage(savedJSON))
	if !ok {
		return false, errors.New("saved create request is not a JSON object")
	}
	derived, _ := memberValue(saved, "route_derived")
	a, err := clientRequestOf(savedJSON, derived)
	if err != nil {
		return false, err
	}
	b, err := clientRequestOf(retryJSON, derived)
	if err != nil {
		return false, err
	}
	if sameJSONValue(rawObject(a), rawObject(b)) {
		return true, nil
	}
	retry, _ := objectMembers(json.RawMessage(retryJSON))
	requested, hasRequested := memberValue(retry, "model_requested")
	model, hasModel := memberValue(retry, "model")
	if !hasRequested {
		requested = json.RawMessage("null")
	}
	if !hasModel {
		model = json.RawMessage("null")
	}
	if sameJSONValue(requested, model) {
		retry = dropKeys(retry, "model_requested", "route_derived")
	}
	return sameJSONValue(json.RawMessage(savedJSON), rawObject(retry)), nil
}

func (s *Store) retryMatches(policy []configMember, clientRequest []configMember) (bool, error) {
	stored, hasStored := memberValue(policy, "client_env_json")
	storedSet := hasStored && !jsonIsNull(stored)
	env, sent := memberValue(clientRequest, "client_env")
	if !storedSet || !sent {
		return !storedSet && !sent, nil
	}
	var rules json.RawMessage
	if r, has := memberValue(policy, "client_env_rules_json"); has && !jsonIsNull(r) {
		rules = json.RawMessage(jsonString(r))
		if !json.Valid(rules) {
			return false, errors.New("stored client environment rules are not JSON")
		}
	}
	normalise := s.localHooks().NormaliseClientEnv
	if normalise == nil {
		return false, ErrNoLocalSessionPolicy
	}
	normalised, err := normalise(env, rules)
	if err != nil {
		return false, nil
	}
	want := json.RawMessage(jsonString(stored))
	if !json.Valid(want) {
		return false, errors.New("stored client environment is not JSON")
	}
	return sameJSONValue(normalised, want), nil
}

func (s *Store) SameCreateRetry(savedPolicy json.RawMessage, retryJSON string, clientRequest json.RawMessage) (bool, error) {
	policy, ok := objectMembers(savedPolicy)
	if !ok {
		return false, errors.New("saved policy is not a JSON object")
	}
	request, _ := objectMembers(clientRequest)
	saved, err := memberIn(policy, "request_json")
	if err != nil {
		return false, err
	}
	if !jsonIsString(saved) {
		return false, errors.New("saved policy request_json is not text")
	}
	same, err := SameCreateRequest(jsonString(saved), retryJSON)
	if err != nil || !same {
		return false, err
	}
	return s.retryMatches(policy, request)
}

func rowCwd(policy []configMember) (any, error) {
	var request []configMember
	if raw, has := memberValue(policy, "request_json"); has && jsonIsString(raw) && jsonString(raw) != "" {
		request, _ = objectMembers(json.RawMessage(jsonString(raw)))
	}
	if value, has := memberValue(request, "row_cwd"); has && jsonIsString(value) {
		if jsonString(value) == "" {
			return nil, nil
		}
		return jsonString(value), nil
	}
	cwd, err := memberIn(policy, "cwd")
	if err != nil {
		return nil, err
	}
	return jsonCellValue(cwd), nil
}

func (s *Store) CommitLocalSession(ctx context.Context, o LocalCommit) (json.RawMessage, error) {
	receiptMembers, ok := objectMembers(o.Receipt)
	if !ok {
		return nil, runtimeErr(ReasonInvalidParams)
	}
	request := o.ClientRequest
	if request == nil {
		policy, _ := memberValue(receiptMembers, "policy")
		policyMembers, _ := objectMembers(policy)
		if env, has := memberValue(policyMembers, "client_env_json"); has && !jsonIsNull(env) {
			return nil, runtimeErr(ReasonInvalidParams)
		}
		request = json.RawMessage("{}")
	}
	if o.OnCreate != nil {
		if err := o.OnCreate.Read(); err != nil {
			return nil, err
		}
	}
	encoded, err := canonicalObject(o.Receipt)
	if err != nil {
		return nil, err
	}
	receipt, _ := objectMembers(json.RawMessage(encoded))
	sidRaw, err := memberIn(receipt, "session_id")
	if err != nil {
		return nil, err
	}
	routeRaw, err := memberIn(receipt, "route")
	if err != nil {
		return nil, err
	}
	sid, route := jsonString(sidRaw), jsonCellValue(routeRaw)
	key := localPolicyPrefix + sid
	var out json.RawMessage
	err = s.Write(ctx, WritePatience, func(c Conn) error {
		out = nil
		if err := checkEpoch(ctx, c, o.Epoch); err != nil {
			return err
		}
		old, found, err := metaValue(ctx, c, key)
		if err != nil {
			return err
		}
		if found {
			same, err := s.sameCommittedReceipt(old, receipt, request)
			if errors.Is(err, ErrNoLocalSessionPolicy) {
				return err
			}
			if err != nil {
				return runtimeErr(ReasonStorageUnavailable)
			}
			if !same {
				return runtimeErr(ReasonInvalidParams)
			}
			out = json.RawMessage(old)
			return nil
		}
		retired, err := metaExists(ctx, c, retiredSessionPrefix+sid)
		if err != nil {
			return err
		}
		if retired {
			return runtimeErr(ReasonNotFound)
		}
		_, legacy := memberValue(receipt, "legacy_session_id")
		if !legacy {
			exists, err := queryStrings(ctx, c, "SELECT 1 FROM sessions WHERE id=?", sid)
			if err != nil {
				return err
			}
			if len(exists) > 0 {
				return runtimeErr(ReasonStorageUnavailable)
			}
		}
		routed, err := queryStrings(ctx, c, "SELECT 1 FROM gateway_routing WHERE scope='' AND session_key=?", route)
		if err != nil {
			return err
		}
		if len(routed) > 0 {
			return runtimeErr(ReasonAdmissionConflict)
		}
		if legacy {
			if err := s.bindLegacyTarget(ctx, c, receipt); err != nil {
				return err
			}
		}
		policy, err := objectIn(receipt, "policy")
		if err != nil {
			return err
		}
		entry, err := objectIn(receipt, "entry")
		if err != nil {
			return err
		}
		createdRaw, err := memberIn(entry, "created_at")
		if err != nil {
			return err
		}
		created, err := parseISOTime(jsonString(createdRaw))
		if err != nil {
			return err
		}
		started := float64(created.UnixNano()) / 1e9
		origin, err := objectIn(entry, "origin")
		if err != nil {
			return err
		}
		chatID, err := memberIn(origin, "chat_id")
		if err != nil {
			return err
		}
		sourceRaw, err := memberIn(policy, "source")
		if err != nil {
			return err
		}
		model, err := memberIn(policy, "model")
		if err != nil {
			return err
		}
		principal, err := memberIn(receipt, "principal_id")
		if err != nil {
			return err
		}
		cwd, err := rowCwd(policy)
		if err != nil {
			return err
		}
		var surfaced any
		if v := createdSurfaceOf(jsonString(sourceRaw)); v != "" && jsonIsString(sourceRaw) {
			surfaced = v
		}
		var profile any
		if own := s.OwnProfileName(); own != "" {
			profile = own
		}
		originJSON, err := canonicalObject(rawObject(origin))
		if err != nil {
			return err
		}
		entryJSON, err := canonicalObject(rawObject(entry))
		if err != nil {
			return err
		}
		if _, err := c.ExecContext(ctx, `INSERT INTO sessions(id,source,created_source,user_id,session_key,chat_id,chat_type,
            model,cwd,profile_name,origin_json,started_at) VALUES(?,?,?,?,?,?,'dm',?,?,?,?,?)
            ON CONFLICT(id) DO NOTHING`, sid, jsonCellValue(sourceRaw), surfaced, jsonCellValue(principal), route,
			jsonCellValue(chatID), jsonCellValue(model), cwd, profile, originJSON, started); err != nil {
			return err
		}
		if _, err := c.ExecContext(ctx, "INSERT INTO gateway_routing(scope,session_key,entry_json,updated_at) VALUES('',?,?,?)",
			route, entryJSON, started); err != nil {
			return err
		}
		if _, err := c.ExecContext(ctx, "INSERT INTO state_meta(key,value) VALUES(?,?)", key, encoded); err != nil {
			return err
		}
		if o.OnCreate != nil {
			o.OnCreate.Publish()
		}
		out = json.RawMessage(encoded)
		return nil
	})
	if err != nil {
		if o.OnCreate != nil {
			o.OnCreate.Withdraw()
		}
		return nil, err
	}
	return out, nil
}

func (s *Store) sameCommittedReceipt(old string, receipt []configMember, request json.RawMessage) (bool, error) {
	saved, ok := objectMembers(json.RawMessage(old))
	if !ok {
		return false, errors.New("stored local receipt is not an object")
	}
	for _, k := range []string{"profile_id", "principal_id", "request_id", "session_id", "route"} {
		a, err := memberIn(saved, k)
		if err != nil {
			return false, err
		}
		b, err := memberIn(receipt, k)
		if err != nil {
			return false, err
		}
		if !sameJSONValue(a, b) {
			return false, nil
		}
	}
	savedPolicy, err := memberIn(saved, "policy")
	if err != nil {
		return false, err
	}
	policy, err := objectIn(receipt, "policy")
	if err != nil {
		return false, err
	}
	retryJSON, err := memberIn(policy, "request_json")
	if err != nil {
		return false, err
	}
	if !jsonIsString(retryJSON) {
		return false, errors.New("receipt request_json is not text")
	}
	return s.SameCreateRetry(savedPolicy, jsonString(retryJSON), request)
}

func MovedReceiptsIn(ctx context.Context, c Conn, targetIDs []string) (map[string]MovedReceipt, error) {
	rows, err := c.QueryContext(ctx, "SELECT substr(m.key, ?), s.id, s.source FROM state_meta m JOIN sessions s "+
		"ON s.id = CASE WHEN json_valid(m.value) THEN json_extract(m.value, '$.entry.session_id') END "+
		"WHERE m.key GLOB ? AND s.session_key IS NOT NULL "+
		"AND s.session_key IS NOT CASE WHEN json_valid(m.value) THEN json_extract(m.value, '$.route') END",
		len(localPolicyPrefix)+1, localPolicyPrefix+"*")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]MovedReceipt{}
	for rows.Next() {
		var owner, target string
		var source sql.NullString
		if err := rows.Scan(&owner, &target, &source); err != nil {
			return nil, err
		}
		if targetIDs == nil || containsString(targetIDs, target) {
			out[owner] = MovedReceipt{Target: target, Source: source.String}
		}
	}
	return out, rows.Err()
}

func releaseReceipts(ctx context.Context, c Conn, owners []string) error {
	for _, owner := range owners {
		if _, err := c.ExecContext(ctx, "DELETE FROM state_meta WHERE key IN (?,?)", localPolicyPrefix+owner, localLegacyPrefix+owner); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) HandoffHead(ctx context.Context, sessionID string) (string, string, error) {
	var owner, target string
	err := s.Read(ctx, func(c Conn) error {
		var err error
		if owner, err = LocalLineageOwnerIn(ctx, c, sessionID); err != nil {
			return err
		}
		saved, found, err := metaValue(ctx, c, localPolicyPrefix+owner)
		target = ""
		if err == nil && found {
			target = policyTarget(saved)
		}
		return err
	})
	if err != nil {
		return "", "", err
	}
	start := target
	if start == "" {
		start, owner = sessionID, ""
	}
	tip, err := s.CompressionTip(ctx, start)
	if err != nil {
		return "", "", err
	}
	if tip == "" {
		tip = start
	}
	return owner, tip, nil
}

func HandOffConversationsIn(ctx context.Context, c Conn, moved map[string]MovedReceipt) (map[string]MovedReceipt, error) {
	owners := make([]string, 0, len(moved))
	for owner := range moved {
		owners = append(owners, owner)
	}
	sort.Strings(owners)
	handed := map[string]MovedReceipt{}
	var released []string
	for _, owner := range owners {
		found := moved[owner]
		lineage := []string{owner}
		saved, ok, err := metaValue(ctx, c, localPolicyPrefix+owner)
		if err != nil {
			return nil, err
		}
		if ok {
			members, _ := objectMembers(json.RawMessage(saved))
			if raw, has := memberValue(members, "lineage"); has {
				ids, valid := stringList(raw)
				if !valid {
					continue
				}
				lineage = ids
			}
		}
		segments := UIDList(append([]string{owner}, lineage...))
		encoded, err := asciiJSONList(segments)
		if err != nil {
			return nil, err
		}
		if _, err := c.ExecContext(ctx, "UPDATE sessions SET ("+laneIdentityColumns+") = (SELECT "+laneIdentityColumns+" FROM sessions WHERE id=?) "+
			"WHERE id IN (SELECT value FROM json_each(?)) AND id != ?", found.Target, encoded, found.Target); err != nil {
			return nil, err
		}
		root, err := segmentRoot(ctx, c, segments, found.Target)
		if err != nil {
			return nil, err
		}
		if err := titleFollowsHead(ctx, c, owner, root); err != nil {
			return nil, err
		}
		handed[owner] = found
		released = append(released, owner)
	}
	return handed, releaseReceipts(ctx, c, released)
}

func segmentRoot(ctx context.Context, c Conn, segments []string, target string) (string, error) {
	index := -1
	for i, sid := range segments {
		if sid == target {
			index = i
			break
		}
	}
	if index < 0 {
		return target, nil
	}
	for index > 0 {
		reason, err := firstText(ctx, c, "SELECT end_reason FROM sessions WHERE id=?", segments[index-1])
		if err != nil {
			return "", err
		}
		if reason != "compression" {
			break
		}
		index--
	}
	return segments[index], nil
}

func titleFollowsHead(ctx context.Context, c Conn, owner, head string) error {
	var title, source sql.NullString
	var hidden any
	err := c.QueryRowContext(ctx, "SELECT title, title_source, hidden FROM sessions WHERE id=?", owner).Scan(&title, &source, &hidden)
	if errors.Is(err, sql.ErrNoRows) || head == owner || (err == nil && (!title.Valid || title.String == "")) {
		return nil
	}
	if err != nil {
		return err
	}
	var keptTitle, keptSource sql.NullString
	err = c.QueryRowContext(ctx, "SELECT title, title_source FROM sessions WHERE id=?", head).Scan(&keptTitle, &keptSource)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if keptTitle.Valid && keptTitle.String != "" && titleRank(keptSource) >= titleRank(source) {
		return nil
	}
	if _, err := c.ExecContext(ctx, "UPDATE sessions SET title=NULL WHERE id IN (?,?)", owner, head); err != nil {
		return err
	}
	_, err = c.ExecContext(ctx, "UPDATE sessions SET title=?, title_source=?, hidden=? WHERE id=?", title, source, hidden, head)
	return err
}

func (s *Store) LocalReceipt(ctx context.Context, sessionID string) (json.RawMessage, error) {
	var out json.RawMessage
	err := s.Read(ctx, func(c Conn) error {
		saved, found, err := metaValue(ctx, c, localPolicyPrefix+sessionID)
		if err != nil {
			return err
		}
		if !found {
			return runtimeErr(ReasonStorageUnavailable)
		}
		if _, ok := objectMembers(json.RawMessage(saved)); !ok {
			return runtimeErr(ReasonStorageUnavailable)
		}
		out = json.RawMessage(saved)
		return nil
	})
	return out, err
}

func (s *Store) EndIdleLocalSession(ctx context.Context, epoch int64, sessionID, targetID, reason string) (int64, error) {
	var ended int64
	err := s.Write(ctx, WritePatience, func(c Conn) error {
		ended = 0
		if err := checkEpoch(ctx, c, epoch); err != nil {
			return err
		}
		pending, err := queryStrings(ctx, c, "SELECT 1 FROM session_admissions WHERE target_session_id=? AND status IN ('queued','started','unknown')", sessionID)
		if err != nil || len(pending) > 0 {
			return err
		}
		live, err := queryStrings(ctx, c, "SELECT 1 FROM worker_executions WHERE session_id IN (?,?) AND status IN ('registered','running','unknown')", sessionID, targetID)
		if err != nil || len(live) > 0 {
			return err
		}
		ended, err = EndAndBumpIn(ctx, c, "UPDATE sessions SET ended_at=?, end_reason=? WHERE id=? AND ended_at IS NULL",
			[]any{nowEpoch(), reason, targetID}, targetID, reason)
		return err
	})
	return ended, err
}

var messagingPlatformDirs = []string{"a2a", "bluebubbles", "buzz", "dingtalk", "discord", "email", "feishu", "google_chat",
	"irc", "line", "matrix", "mattermost", "ntfy", "photon", "qqbot", "raft", "signal", "simplex", "slack", "sms", "teams",
	"telegram", "wecom", "weixin", "whatsapp", "whatsapp_cloud", "yuanbao"}

var messagingPlatformValues = []string{"local", "telegram", "discord", "whatsapp", "whatsapp_cloud", "slack", "signal",
	"mattermost", "matrix", "email", "sms", "dingtalk", "api_server", "webhook", "msgraph_webhook", "feishu", "wecom",
	"wecom_callback", "weixin", "bluebubbles", "qqbot", "yuanbao", "relay"}

func NotClientLabels() []string {
	out := append(append(append([]string{}, legacyLocalSources...), notClientLabelSet...), messagingPlatformValues...)
	for _, dir := range messagingPlatformDirs {
		out = append(out, dir, dir+"-platform")
	}
	return UIDList(out)
}

func ClientLabel(source string) bool {
	return strings.TrimSpace(source) != "" && !containsString(NotClientLabels(), source)
}

func clientHistoryRow(row Session) bool {
	return ClientLabel(row.Source) && !routed(row)
}

func legacyHistoryRow(row Session) bool {
	return containsString(legacyLocalSources, row.Source) || clientHistoryRow(row)
}

func LegacyCreationSource(row Session) string {
	if clientHistoryRow(row) || row.Source == "desktop" {
		return "gui"
	}
	return row.Source
}

func legacyAccountLabel(row Session) bool {
	return row.Source == "desktop" && row.UserID.Valid && strings.HasPrefix(row.UserID.V, "portal:")
}

func (s *Store) loginOwnedBy(row Session, subject string) (bool, error) {
	if !row.UserID.Valid || subject == "" {
		return false, nil
	}
	return s.gatewayHooks().sameOwner(sql.NullString{String: row.UserID.V, Valid: true}, sql.NullString{String: subject, Valid: true})
}

func (s *Store) ValidateLegacyRow(row Session, subject string) error {
	history := legacyHistoryRow(row)
	stamped := false
	if row.UserID.V != "" && !legacyAccountLabel(row) {
		owned, err := s.loginOwnedBy(row, subject)
		if err != nil {
			return err
		}
		stamped = !owned
	}
	own := s.OwnProfileName()
	if own == "" {
		own = "default"
	}
	profileOK := !row.ProfileName.Valid || row.ProfileName.V == "" || row.ProfileName.V == own
	if !history || routed(row) || stamped || !profileOK {
		return runtimeErr(ReasonNotFound)
	}
	return nil
}

func (s *Store) bindLegacyTarget(ctx context.Context, c Conn, receipt []configMember) error {
	targetRaw, err := memberIn(receipt, "legacy_session_id")
	if err != nil {
		return err
	}
	sidRaw, err := memberIn(receipt, "session_id")
	if err != nil {
		return err
	}
	if !sameJSONValue(targetRaw, sidRaw) {
		return runtimeErr(ReasonStorageUnavailable)
	}
	target := jsonString(targetRaw)
	rows, err := querySessions(c.QueryContext(ctx, "SELECT * FROM sessions WHERE id=?", target))
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return runtimeErr(ReasonNotFound)
	}
	row := rows[0]
	principalRaw, err := memberIn(receipt, "principal_id")
	if err != nil {
		return err
	}
	profileRaw, err := memberIn(receipt, "profile_id")
	if err != nil {
		return err
	}
	principal, profileID := jsonString(principalRaw), jsonString(profileRaw)
	if err := s.ValidateLegacyRow(row, principal); err != nil {
		return err
	}
	imported, found, err := metaValue(ctx, c, historyBindingPrefix+target)
	if err != nil {
		return err
	}
	if found {
		named, err := s.bindingNames(imported, profileID, principal)
		if err != nil {
			return err
		}
		if !named {
			return runtimeErr(ReasonPermissionDenied)
		}
	}
	child, err := chainStep(ctx, c, target)
	if err != nil {
		return err
	}
	if child != "" {
		return runtimeErr(ReasonAdmissionConflict)
	}
	if err := s.requireIdle(ctx, c, []string{target}); err != nil {
		return err
	}
	root, err := sessionTurnLeaseKeyIn(ctx, c, target)
	if err != nil {
		return err
	}
	leased, err := queryStrings(ctx, c, "SELECT 1 FROM session_turn_leases WHERE conversation_id=? AND expires_at>?", root, nowEpoch())
	if err != nil {
		return err
	}
	if len(leased) > 0 {
		return runtimeErr(ReasonRuntimeCoordinationRequired)
	}
	key := localLegacyPrefix + target
	exists, err := metaExists(ctx, c, key)
	if err != nil {
		return err
	}
	if exists {
		return runtimeErr(ReasonStorageUnavailable)
	}
	entry, err := objectIn(receipt, "entry")
	if err != nil {
		return err
	}
	origin, err := memberIn(entry, "origin")
	if err != nil {
		return err
	}
	originJSON, err := canonicalObject(origin)
	if err != nil {
		return err
	}
	label := row.Source
	if clientHistoryRow(row) {
		label = "desktop"
	}
	route, err := memberIn(receipt, "route")
	if err != nil {
		return err
	}
	if _, err := c.ExecContext(ctx, "UPDATE sessions SET source=?,session_key=?,chat_id=?,user_id=?,chat_type=?,origin_json=? WHERE id=?",
		label, jsonCellValue(route), jsonCellValue(sidRaw), jsonCellValue(principalRaw), "dm", originJSON, target); err != nil {
		return err
	}
	marker, err := canonicalJSON(map[string]json.RawMessage{"session_id": sidRaw, "profile_id": profileRaw,
		"principal_id": principalRaw, "legacy_session_id": targetRaw})
	if err != nil {
		return err
	}
	_, err = c.ExecContext(ctx, "INSERT INTO state_meta(key,value) VALUES(?,?)", key, marker)
	return err
}
