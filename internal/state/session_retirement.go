package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
)

// Meta key prefix of a last viewer's close stored until the owner's idle point
const ViewerClosePrefix = "gateway.viewer_close.v1:"

// Meta key prefixes of another profile's routing fence and of the per session API settings
const (
	routeFencePrefix  = "gateway.retired_route.v1."
	apiSettingsPrefix = "gateway.api.settings.v1."
)

// Seconds an exact retry gets the settled reply and a sweep marker fences its id
const retryHorizonSeconds = 7 * 86400

// Filter selecting retired markers older than a bound cutoff
const expiredSQL = "json_valid(value) AND json_extract(value, '$.retired_at') < ?"

// Probe for queued, running or unknown work on one session
const liveLedgerSQL = `SELECT 1 FROM session_admissions WHERE target_session_id=? AND status IN ('queued','started','unknown')
    UNION ALL SELECT 1 FROM worker_executions WHERE session_id=? AND status IN ('registered','running','unknown') LIMIT 1`

// Filter selecting API sessions whose ids a door derives, hosted room members and generated api ids
var derivedAPISQL = "(source='bot_room' OR (source='api_server' AND id GLOB 'api-" + strings.Repeat("[0-9a-f]", 16) + "'))"

// Request id prefixes of the local conversations a door derives and walks by generation
var derivedLocalPrefixes = []string{"bot-chat:", "a2a:"}

// Turn result members that copy the transcript
var transcriptResultFields = []string{"messages", "last_reasoning", "tools", "_turn_user_row"}

// Mutation result members that copy user content
var mutationTranscriptFields = []string{"target_message", "summary", "title"}

// JSON paths of every session a mutation receipt result can name
var receiptSessionPaths = []string{"$.result.session_id", "$.result.target_message.session_id",
	"$.result.target_session_id", "$.result.previous_target_session_id"}

// True when a routing save must skip an entry, bound by the names routeRetiredArgs supplies
const routeRetiredSQL = `(
    EXISTS (SELECT 1 FROM state_meta m
             WHERE m.key = :retired_prefix || json_extract(:entry, '$.session_id')
               AND (:ns = 'main' OR NOT CASE WHEN json_valid(m.value)
                                             THEN json_extract(m.value, '$.own') IS NOT NULL ELSE 0 END))
    OR EXISTS (SELECT 1 FROM state_meta f
                WHERE f.key = :fence_prefix || :ns || ':' || json_extract(:entry, '$.session_id')
                  AND NOT CASE WHEN json_valid(f.value)
                               THEN coalesce(json_extract(f.value, '$.expires')
                                             AND json_extract(f.value, '$.retired_at') < :now - :horizon, 0)
                               ELSE 0 END
                  AND (:ns != 'main' OR NOT EXISTS (
                          SELECT 1 FROM sessions s WHERE s.id = json_extract(:entry, '$.session_id')))))`

// Reports whether a session id carries this store's deletion fence
func retiredIn(ctx context.Context, c Conn, sessionID string) (bool, error) {
	return metaExists(ctx, c, retiredSessionPrefix+sessionID)
}

// Reports whether a session id was deleted from this store
func (s *Store) RetiredSession(ctx context.Context, sessionID string) (bool, error) {
	retired := false
	err := s.Read(ctx, func(c Conn) error {
		var err error
		retired, err = retiredIn(ctx, c, sessionID)
		return err
	})
	return retired, err
}

// Refusal for creating an explicit session id that is live or was deleted, nil when the id is free
func CreateConflictIn(ctx context.Context, c Conn, sessionID string) error {
	var one int
	err := c.QueryRowContext(ctx, "SELECT 1 FROM sessions WHERE id=?", sessionID).Scan(&one)
	if err == nil {
		return runtimeErrAt(ReasonSessionExists, "", "Session already exists")
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	retired, err := retiredIn(ctx, c, sessionID)
	if err != nil || !retired {
		return err
	}
	return runtimeErrAt(ReasonSessionDeleted, "", "Session was deleted; its id cannot be reused")
}

// Verdict of a generation walk on one derived id, read on the walk's connection
type GenerationCheck func(ctx context.Context, c Conn, sessionID string) (bool, error)

// First generation of a derived id a new request may use, pinned keeping a retired one and spent skipping a live one
func (s *Store) FirstGeneration(ctx context.Context, derive func(generation int) string, pinned, spent GenerationCheck) (int, string, error) {
	if derive == nil {
		return 0, "", &MissingHookError{Name: "derived session id function"}
	}
	generation, sid := 0, ""
	err := s.Read(ctx, func(c Conn) error {
		for generation = 0; ; generation++ {
			sid = derive(generation)
			retired, err := retiredIn(ctx, c, sid)
			if err != nil {
				return err
			}
			if retired {
				if pinned == nil {
					continue
				}
				keep, err := pinned(ctx, c, sid)
				if err != nil {
					return err
				}
				if keep {
					return nil
				}
				continue
			}
			if spent == nil {
				return nil
			}
			used, err := spent(ctx, c, sid)
			if err != nil {
				return err
			}
			if !used {
				return nil
			}
		}
	})
	return generation, sid, err
}

// Makes permanent every fence a generation walk passed before sessionID, false when one lost its marker
func KeepPassedFencesIn(ctx context.Context, c Conn, derive func(generation int) string, sessionID string) (bool, error) {
	if derive == nil {
		return false, &MissingHookError{Name: "derived session id function"}
	}
	for generation := 0; ; generation++ {
		sid := derive(generation)
		if sid == sessionID {
			return true, nil
		}
		marked, err := retiredIn(ctx, c, sid)
		if err != nil || !marked {
			return false, err
		}
		if _, err := c.ExecContext(ctx, "UPDATE state_meta SET value=json_remove(value, '$.expires') WHERE key=? AND json_valid(value)",
			retiredSessionPrefix+sid); err != nil {
			return false, err
		}
	}
}

// Every row of a query as its column names and cell values, refusing a cell JSON cannot carry
func queryRowMaps(ctx context.Context, c Conn, query string, args ...any) ([]map[string]any, error) {
	rows, err := c.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	columns, err := rows.Columns()
	if err != nil {
		rows.Close()
		return nil, err
	}
	out := []map[string]any{}
	for rows.Next() {
		cells := make([]any, len(columns))
		dest := make([]any, len(columns))
		for i := range cells {
			dest[i] = &cells[i]
		}
		if err := rows.Scan(dest...); err != nil {
			rows.Close()
			return nil, err
		}
		row := make(map[string]any, len(columns))
		for i, name := range columns {
			if _, blob := cells[i].([]byte); blob {
				rows.Close()
				return nil, runtimeErr(ReasonInvalidParams)
			}
			row[name] = cells[i]
		}
		out = append(out, row)
	}
	return out, rowsDone(rows)
}

// Text cell of a scanned row, empty when the column is absent or not text
func rowText(row map[string]any, column string) string {
	text, _ := row[column].(string)
	return text
}

// Shrinks a retired admission's stored result to its exact retry outcome without the transcript copies
func compactRetiredResult(ctx context.Context, c Conn, admissionID string) error {
	saved, ok, err := metaValue(ctx, c, resultPrefix+admissionID)
	if err != nil || !ok {
		return err
	}
	value, isObject := loadResult(saved)
	if !isObject {
		return runtimeErr(ReasonStorageUnavailable)
	}
	raw, _ := memberValue(value, "result")
	if result, nested := objectMembers(raw); nested {
		kept := result[:0:0]
		for _, m := range result {
			if !containsString(transcriptResultFields, m.key) {
				kept = append(kept, m)
			}
		}
		kept = append(kept, configMember{"messages", json.RawMessage("[]")})
		value = withMember(value, "result", rawObject(kept))
	}
	encoded, err := canonicalObject(rawObject(encodeResult(value)))
	if err != nil {
		return err
	}
	_, err = c.ExecContext(ctx, "UPDATE state_meta SET value=? WHERE key=?", encoded, resultPrefix+admissionID)
	return err
}

// Moves the terminal admissions and workers of deleted sessions into tombstones, refusing while any work is live
func retireTerminalReceipts(ctx context.Context, c Conn, sessionIDs []string) error {
	for _, sid := range sessionIDs {
		admissions, err := queryRowMaps(ctx, c, "SELECT * FROM session_admissions WHERE target_session_id=?", sid)
		if err != nil {
			return err
		}
		workers, err := queryRowMaps(ctx, c, "SELECT * FROM worker_executions WHERE session_id=?", sid)
		if err != nil {
			return err
		}
		live, unknown := false, false
		for _, rows := range [][]map[string]any{admissions, workers} {
			for _, row := range rows {
				status := rowText(row, "status")
				live = live || status != "terminal"
				unknown = unknown || status == "unknown"
			}
		}
		if unknown {
			return runtimeErr(ReasonUnknownExecution)
		}
		if live {
			return runtimeErr(ReasonSessionBusy)
		}
		for _, row := range admissions {
			admissionID, requestID := rowText(row, "admission_id"), rowText(row, "request_id")
			if err := retireMedia(ctx, c, rowText(row, "payload_json"), requestID); err != nil {
				return err
			}
			row["payload_json"], row["lineage_json"], row["retired_at"] = "{}", "[]", nowEpoch()
			if err := compactRetiredResult(ctx, c, admissionID); err != nil {
				return err
			}
			tombstone, err := canonicalJSON(row)
			if err != nil {
				return runtimeErr(ReasonInvalidParams)
			}
			if _, err := c.ExecContext(ctx, "INSERT INTO state_meta(key,value) VALUES(?,?)", terminalAdmissionPrefix+admissionID, tombstone); err != nil {
				return err
			}
			identity, err := terminalIdentityKey(rowText(row, "principal_id"), sid, requestID)
			if err != nil {
				return err
			}
			pointer, err := asciiJSONString(admissionID)
			if err != nil {
				return err
			}
			if _, err := c.ExecContext(ctx, "INSERT INTO state_meta(key,value) VALUES(?,?)", identity, pointer); err != nil {
				return err
			}
		}
		closing, err := AdmissionFingerprint(sid, map[string]any{"operation": "execution.finish", "payload": map[string]any{}})
		if err != nil {
			return err
		}
		for _, row := range workers {
			executionID := rowText(row, "execution_id")
			receipts, err := queryRowMaps(ctx, c, "SELECT sequence,payload_digest,result_json FROM worker_receipts WHERE execution_id=? ORDER BY sequence", executionID)
			if err != nil {
				return err
			}
			for _, receipt := range receipts {
				if rowText(receipt, "payload_digest") != closing {
					receipt["result_json"] = nil
				}
			}
			row["receipts"], row["retired_at"] = receipts, nowEpoch()
			tombstone, err := canonicalJSON(row)
			if err != nil {
				return runtimeErr(ReasonInvalidParams)
			}
			if _, err := c.ExecContext(ctx, "INSERT INTO state_meta(key,value) VALUES(?,?)", terminalWorkerPrefix+executionID, tombstone); err != nil {
				return err
			}
			if _, err := c.ExecContext(ctx, "DELETE FROM worker_receipts WHERE execution_id=?", executionID); err != nil {
				return err
			}
		}
		if _, err := c.ExecContext(ctx, "DELETE FROM worker_executions WHERE session_id=?", sid); err != nil {
			return err
		}
		if _, err := c.ExecContext(ctx, "DELETE FROM session_admissions WHERE target_session_id=?", sid); err != nil {
			return err
		}
	}
	return nil
}

// Publishes the whole deletion fence of ids deleted in this transaction, markers of expiring ids aging out at the retry horizon
func retireSessions(ctx context.Context, c Conn, sessionIDs, expiring []string) error {
	if err := retireSpillover(ctx, c, sessionIDs); err != nil {
		return err
	}
	if err := retireTerminalReceipts(ctx, c, sessionIDs); err != nil {
		return err
	}
	if err := retireMutationReceipts(ctx, c, sessionIDs); err != nil {
		return err
	}
	if err := retireRoutes(ctx, c, sessionIDs, expiring); err != nil {
		return err
	}
	return retireLocalReceipts(ctx, c, sessionIDs)
}

// Nulls the user content in every mutation receipt naming a deleted session, the digest and ids staying for exact retries
func retireMutationReceipts(ctx context.Context, c Conn, sessionIDs []string) error {
	if len(sessionIDs) == 0 {
		return nil
	}
	named := make([]string, len(receiptSessionPaths))
	for i, path := range receiptSessionPaths {
		named[i] = "json_extract(value,'" + path + "') IN (SELECT value FROM json_each(?1))"
	}
	ids, err := asciiJSONList(sessionIDs)
	if err != nil {
		return err
	}
	receipts, err := metaEntries(ctx, c, "SELECT key,value FROM state_meta WHERE key GLOB 'gateway.mutation.v1.*' AND "+
		"CASE WHEN json_valid(value) THEN ("+strings.Join(named, " OR ")+") END", ids)
	if err != nil {
		return err
	}
	for _, e := range receipts {
		receipt, _ := objectMembers(json.RawMessage(e.Value))
		raw, _ := memberValue(receipt, "result")
		result, _ := objectMembers(raw)
		carries := false
		for i := range result {
			if containsString(mutationTranscriptFields, result[i].key) {
				result[i].value, carries = json.RawMessage("null"), true
			}
		}
		if !carries {
			continue
		}
		encoded, err := canonicalObject(rawObject(withMember(receipt, "result", rawObject(result))))
		if err != nil {
			return err
		}
		if _, err := c.ExecContext(ctx, "UPDATE state_meta SET value=? WHERE key=?", encoded, e.Key); err != nil {
			return err
		}
	}
	return nil
}

// Key and value of every meta entry a query selects
func metaEntries(ctx context.Context, c Conn, query string, args ...any) ([]MetaEntry, error) {
	rows, err := c.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	var out []MetaEntry
	for rows.Next() {
		var k, v sql.NullString
		if err := rows.Scan(&k, &v); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, MetaEntry{Key: k.String, Value: v.String})
	}
	return out, rowsDone(rows)
}

// Ids of each local conversation by member and the ids of every door derived one, malformed receipts naming nothing
func localConversations(ctx context.Context, c Conn) (map[string][]string, map[string]bool, error) {
	members, derived := map[string][]string{}, map[string]bool{}
	policies, err := metaGlob(ctx, c, localPolicyPrefix+"*")
	if err != nil {
		return nil, nil, err
	}
	for _, e := range policies {
		receipt, ok := objectMembers(json.RawMessage(e.Value))
		if !ok {
			continue
		}
		var ids []string
		if id, isText := memberText(receipt, "session_id"); isText {
			ids = append(ids, id)
		}
		rawLineage, _ := memberValue(receipt, "lineage")
		lineage, _ := arrayElements(rawLineage)
		for _, segment := range lineage {
			if jsonIsString(segment) {
				ids = append(ids, jsonString(segment))
			}
		}
		rawEntry, _ := memberValue(receipt, "entry")
		if entry, isObject := objectMembers(rawEntry); isObject {
			if id, isText := memberText(entry, "session_id"); isText {
				ids = append(ids, id)
			}
		}
		requestID, _ := memberText(receipt, "request_id")
		door := false
		for _, prefix := range derivedLocalPrefixes {
			door = door || strings.HasPrefix(requestID, prefix)
		}
		for _, id := range ids {
			members[id] = ids
			if door {
				derived[id] = true
			}
		}
	}
	return members, derived, nil
}

// Ids among sessionIDs of API sessions whose ids a door derives
func derivedAPISessions(ctx context.Context, c Conn, sessionIDs []string) ([]string, error) {
	ids, err := asciiJSONList(sessionIDs)
	if err != nil {
		return nil, err
	}
	return queryStrings(ctx, c, "SELECT id FROM sessions WHERE "+derivedAPISQL+" AND id IN (SELECT value FROM json_each(?))", ids)
}

// Fences the idle sessions of a sweep and returns them, skipping live work and local conversations not swept whole
func retirePrunable(ctx context.Context, c Conn, sessionIDs []string) ([]string, error) {
	swept := make(map[string]bool, len(sessionIDs))
	for _, sid := range sessionIDs {
		swept[sid] = true
	}
	conversations, derived, err := localConversations(ctx, c)
	if err != nil {
		return nil, err
	}
	var quiet []string
	for _, sid := range sessionIDs {
		whole := conversations[sid]
		if whole == nil {
			whole = []string{sid}
		}
		removable := true
		for _, member := range whole {
			removable = removable && swept[member]
		}
		for i := 0; removable && i < len(whole); i++ {
			var one int
			err := c.QueryRowContext(ctx, liveLedgerSQL, whole[i], whole[i]).Scan(&one)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return nil, err
			}
			removable = err != nil
		}
		if removable {
			quiet = append(quiet, sid)
		}
	}
	doors, err := derivedAPISessions(ctx, c, quiet)
	if err != nil {
		return nil, err
	}
	for _, sid := range doors {
		derived[sid] = true
	}
	var expiring []string
	for _, sid := range quiet {
		if !derived[sid] {
			expiring = append(expiring, sid)
		}
	}
	if err := retireSessions(ctx, c, quiet, expiring); err != nil {
		return nil, err
	}
	return quiet, nil
}

// Session id a stored routing or receipt object names, false for anything that names none
func entrySessionID(raw string) (string, bool) {
	members, ok := objectMembers(json.RawMessage(raw))
	if !ok {
		return "", false
	}
	return memberText(members, "session_id")
}

// Writes a retired marker, a sweep's never replacing a permanent one and an unscoped permanent one staying unscoped
func markRetired(ctx context.Context, c Conn, key string, expires, own bool) error {
	stamp, err := patchValueJSON(nowEpoch())
	if err != nil {
		return err
	}
	members := []configMember{{"retired_at", json.RawMessage(stamp)}}
	if own {
		members = append(members, configMember{"own", json.RawMessage("true")})
	}
	query := "INSERT INTO state_meta(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=CASE " +
		"WHEN NOT json_valid(state_meta.value) THEN json_remove(excluded.value, '$.own') " +
		"WHEN json_extract(state_meta.value, '$.own') IS NOT NULL " +
		"OR json_extract(state_meta.value, '$.expires') THEN excluded.value " +
		"ELSE json_remove(excluded.value, '$.own') END"
	if expires {
		members = append(members, configMember{"expires", json.RawMessage("true")})
		query = "INSERT INTO state_meta(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE " +
			"SET value=excluded.value WHERE json_valid(state_meta.value) " +
			"AND json_extract(state_meta.value, '$.expires')"
	}
	value, err := spacedJSON(rawObject(members))
	if err != nil {
		return err
	}
	_, err = c.ExecContext(ctx, query, key, value)
	return err
}

// Deletes every routing row whose entry and key the predicate selects
func dropRoutes(ctx context.Context, c Conn, doomed func(sessionKey, target string) bool) error {
	rows, err := c.QueryContext(ctx, "SELECT scope,session_key,entry_json FROM gateway_routing")
	if err != nil {
		return err
	}
	var gone [][2]string
	for rows.Next() {
		var scope, key, entry sql.NullString
		if err := rows.Scan(&scope, &key, &entry); err != nil {
			rows.Close()
			return err
		}
		if target, named := entrySessionID(entry.String); named && doomed(key.String, target) {
			gone = append(gone, [2]string{scope.String, key.String})
		}
	}
	if err := rowsDone(rows); err != nil {
		return err
	}
	for _, route := range gone {
		if _, err := c.ExecContext(ctx, "DELETE FROM gateway_routing WHERE scope=? AND session_key=?", route[0], route[1]); err != nil {
			return err
		}
	}
	return nil
}

// Marks each id retired in this store and drops its API settings, stored viewer close and routes
func retireRoutes(ctx context.Context, c Conn, sessionIDs, expiring []string) error {
	aging := make(map[string]bool, len(expiring))
	for _, sid := range expiring {
		aging[sid] = true
	}
	targets := make(map[string]bool, len(sessionIDs))
	for _, sid := range sessionIDs {
		targets[sid] = true
		if err := markRetired(ctx, c, retiredSessionPrefix+sid, aging[sid], true); err != nil {
			return err
		}
		if err := deleteMeta(ctx, c, apiSettingsPrefix+sid); err != nil {
			return err
		}
		if err := deleteMeta(ctx, c, ViewerClosePrefix+sid); err != nil {
			return err
		}
	}
	return dropRoutes(ctx, c, func(_, target string) bool { return targets[target] })
}

// Namespace slot of an agent routing key, main for the launch profile and for a key outside that shape
func routeNamespace(sessionKey string) string {
	parts := strings.Split(sessionKey, ":")
	if len(parts) > 2 && parts[0] == "agent" && parts[1] != "" {
		return parts[1]
	}
	return "main"
}

// Meta key of the shared routing fence of a session retired in the profile a namespace names
func routeFenceKey(namespace, sessionID string) string {
	return routeFencePrefix + namespace + ":" + sessionID
}

// Named arguments of routeRetiredSQL for one routing entry saved at now
func routeRetiredArgs(sessionKey, entryJSON string, now float64) []any {
	return []any{
		sql.Named("entry", entryJSON), sql.Named("ns", routeNamespace(sessionKey)), sql.Named("now", now),
		sql.Named("horizon", retryHorizonSeconds), sql.Named("retired_prefix", retiredSessionPrefix),
		sql.Named("fence_prefix", routeFencePrefix),
	}
}

// Reports whether a routing save or a legacy routing import must skip this entry
func (s *Store) RouteRetired(ctx context.Context, sessionKey, entryJSON string) (bool, error) {
	var retired int64
	err := s.Read(ctx, func(c Conn) error {
		return c.QueryRowContext(ctx, "SELECT "+routeRetiredSQL, routeRetiredArgs(sessionKey, entryJSON, nowEpoch())...).Scan(&retired)
	})
	return retired != 0, err
}

// Ids among sessionIDs whose own marker is a sweep's and ages out at the retry horizon
func (s *Store) ExpiringRetirements(ctx context.Context, sessionIDs []string) ([]string, error) {
	ids, err := asciiJSONList(sessionIDs)
	if err != nil {
		return nil, err
	}
	var out []string
	err = s.Read(ctx, func(c Conn) error {
		keys, err := queryStrings(ctx, c, "SELECT key FROM state_meta WHERE key IN (SELECT ? || value FROM json_each(?)) "+
			"AND json_valid(value) AND json_extract(value, '$.expires')", retiredSessionPrefix, ids)
		if err != nil {
			return err
		}
		out = make([]string, len(keys))
		for i, key := range keys {
			out[i] = strings.TrimPrefix(key, retiredSessionPrefix)
		}
		return nil
	})
	return out, err
}

// Fences and drops the shared index routes of sessions retired in the profile a namespace names, reclaiming aged sweep fences
func fenceSharedRoutes(ctx context.Context, c Conn, namespace string, sessionIDs, expiring []string) error {
	if err := reclaimExpired(ctx, c, routeFencePrefix, nowEpoch()-retryHorizonSeconds); err != nil {
		return err
	}
	aging := make(map[string]bool, len(expiring))
	for _, sid := range expiring {
		aging[sid] = true
	}
	owned := map[string][]string{}
	for _, sid := range sessionIDs {
		names := []string{namespace}
		var one int
		err := c.QueryRowContext(ctx, "SELECT 1 FROM sessions WHERE id=?", sid).Scan(&one)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err != nil && namespace != "main" {
			names = append(names, "main")
		}
		for _, name := range names {
			if err := markRetired(ctx, c, routeFenceKey(name, sid), aging[sid], false); err != nil {
				return err
			}
		}
		owned[sid] = names
	}
	return dropRoutes(ctx, c, func(sessionKey, target string) bool {
		return containsString(owned[target], routeNamespace(sessionKey))
	})
}

// Drops the sweep markers under a prefix retired before cutoff
func reclaimExpired(ctx context.Context, c Conn, prefix string, cutoff float64) error {
	_, err := c.ExecContext(ctx, "DELETE FROM state_meta WHERE key GLOB ? AND "+expiredSQL+" AND json_extract(value, '$.expires')", prefix+"*", cutoff)
	return err
}

// Reclaims deletion tombstones past the retry horizon, a user delete's marker staying and unstamped tombstones aging from now
func pruneRetiredTombstones(ctx context.Context, c Conn) error {
	now := nowEpoch()
	for _, prefix := range []string{terminalAdmissionPrefix, terminalWorkerPrefix} {
		if _, err := c.ExecContext(ctx, "UPDATE state_meta SET value=json_set(value, '$.retired_at', ?) WHERE key GLOB ? "+
			"AND json_valid(value) AND json_extract(value, '$.retired_at') IS NULL", now, prefix+"*"); err != nil {
			return err
		}
	}
	cutoff := now - retryHorizonSeconds
	admissions, err := metaEntries(ctx, c, "SELECT key,value FROM state_meta WHERE key GLOB ? AND "+expiredSQL, terminalAdmissionPrefix+"*", cutoff)
	if err != nil {
		return err
	}
	for _, e := range admissions {
		row, _ := objectMembers(json.RawMessage(e.Value))
		admissionID, hasID := memberText(row, "admission_id")
		principalID, hasPrincipal := memberText(row, "principal_id")
		target, hasTarget := memberText(row, "target_session_id")
		requestID, hasRequest := memberText(row, "request_id")
		if !hasID || !hasPrincipal || !hasTarget || !hasRequest {
			return runtimeErr(ReasonStorageUnavailable)
		}
		identity, err := terminalIdentityKey(principalID, target, requestID)
		if err != nil {
			return err
		}
		if err := deleteMeta(ctx, c, e.Key, resultPrefix+admissionID, identity); err != nil {
			return err
		}
	}
	if _, err := c.ExecContext(ctx, "DELETE FROM state_meta WHERE key GLOB ? AND "+expiredSQL, terminalWorkerPrefix+"*", cutoff); err != nil {
		return err
	}
	for _, prefix := range []string{retiredSessionPrefix, routeFencePrefix} {
		if err := reclaimExpired(ctx, c, prefix, cutoff); err != nil {
			return err
		}
	}
	return nil
}
