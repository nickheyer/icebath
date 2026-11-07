package state

import (
	"context"
	"database/sql"
	"log/slog"
	"sort"
	"strconv"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Recursive CTE naming a session plus the compression ancestors a resume keeps on one routing peer
var compressionLineageCTE = `
                    WITH RECURSIVE compression_lineage(id) AS (
                        SELECT ?
                        UNION
                        SELECT parent.id
                        FROM compression_lineage lineage
                        JOIN sessions child ON child.id = lineage.id
                        JOIN sessions parent ON parent.id = child.parent_session_id
                        WHERE parent.end_reason = 'compression'
                          AND ` + sqlJSONExtract("child.model_config", "$._branched_from") + ` IS NULL
                          AND ` + sqlJSONExtract("child.model_config", "$._delegate_from") + ` IS NULL
                          AND NOT (` + resetChildSQL("child") + `)
                          AND COALESCE(child.source, '') != 'tool'
                    )
                `

const peerSelectHead = `
                SELECT s.*,
                       COALESCE(sp.prompt, s.system_prompt)
                           AS _system_prompt_resolved,
                       (COALESCE(s.message_count, 0) > 0 OR EXISTS (
                           SELECT 1 FROM messages WHERE messages.session_id = s.id LIMIT 1
                       )) AS _has_messages
                FROM sessions s
                LEFT JOIN system_prompts sp ON sp.hash = s.system_prompt_hash
`

// A row a completed handoff owns stays recoverable whatever end reason the source teardown stamped
const handoffOwnedRowSQL = "(s.handoff_state = 'completed')"

var peerByKeySQL = peerSelectHead + `                WHERE s.session_key = ?
                  AND s.source = ?
                  AND (s.ended_at IS NULL OR s.end_reason IN (` + recoverableEndReasonsSQL + `)
                       OR ` + handoffOwnedRowSQL + `)
                  AND NOT EXISTS (
                      SELECT 1 FROM sessions b
                      WHERE b.session_key = s.session_key
                        AND b.source = s.source
                        AND b.ended_at IS NOT NULL
                        AND b.end_reason IN (` + resetEndReasonsSQL + `)
                        AND b.ended_at
                            > COALESCE(s.last_activity_at, s.started_at)
                  )
                ORDER BY _has_messages DESC,
                         COALESCE(s.last_activity_at, s.started_at) DESC
                LIMIT 1
                `

var peerByTupleSQL = peerSelectHead + `                WHERE s.source = ?
                  AND COALESCE(s.user_id, '') = COALESCE(?, '')
                  AND COALESCE(s.chat_id, '') = COALESCE(?, '')
                  AND COALESCE(s.chat_type, '') = COALESCE(?, '')
                  AND COALESCE(s.thread_id, '') = COALESCE(?, '')
                  AND (? IS NULL OR COALESCE(s.profile_name, ?) = ?)
                  AND (s.ended_at IS NULL OR s.end_reason IN (` + recoverableEndReasonsSQL + `)
                       OR ` + handoffOwnedRowSQL + `)
                  AND (COALESCE(s.message_count, 0) > 0 OR EXISTS (
                      SELECT 1 FROM messages WHERE messages.session_id = s.id LIMIT 1
                  ))
                  AND NOT EXISTS (
                      SELECT 1 FROM sessions b
                      WHERE b.source = s.source
                        AND COALESCE(b.user_id, '') = COALESCE(s.user_id, '')
                        AND COALESCE(b.chat_id, '') = COALESCE(s.chat_id, '')
                        AND COALESCE(b.chat_type, '') = COALESCE(s.chat_type, '')
                        AND COALESCE(b.thread_id, '') = COALESCE(s.thread_id, '')
                        AND (? IS NULL OR COALESCE(b.profile_name, ?) = ?)
                        AND b.ended_at IS NOT NULL
                        AND b.end_reason IN (` + resetEndReasonsSQL + `)
                        AND b.ended_at
                            > COALESCE(s.last_activity_at, s.started_at)
                  )
                ORDER BY COALESCE(s.last_activity_at, s.started_at) DESC
                LIMIT 1
                `

const orphanDonorColumns = "d.id, d.session_key, d.chat_id, d.chat_type, d.thread_id, " +
	"d.user_id, d.origin_json, d.display_name, d.end_reason"

var orphansSQL = `
                SELECT o.id, o.source, o.user_id, o.started_at,
                       o.parent_session_id, o.profile_name,
                       ` + sqlSessionLastActive("o") + ` AS last_active,
                       (SELECT COUNT(*) FROM messages m
                         WHERE m.session_id = o.id) AS message_count
                FROM sessions o
                WHERE o.session_key IS NULL
                  AND EXISTS (SELECT 1 FROM messages m
                               WHERE m.session_id = o.id)
                  AND COALESCE(o.source, '') != 'tool'
                  AND ` + sqlJSONExtract("o.model_config", "$._branched_from") + ` IS NULL
                  AND ` + sqlJSONExtract("o.model_config", "$._delegate_from") + ` IS NULL
                ORDER BY o.started_at ASC
                `

// The donor's effective profile, NULL reading as the store's own, must equal the orphan's
const orphanDonorProfileFenceSQL = "AND (? IS NULL OR COALESCE(d.profile_name, ?) = ?)"

var orphanLineageDonorSQL = `
                        SELECT ` + orphanDonorColumns + `
                        FROM sessions d
                        WHERE d.id = ?
                          AND d.session_key IS NOT NULL
                          AND COALESCE(d.source, '') = COALESCE(?, '')
                          ` + orphanDonorProfileFenceSQL + `
                        `

var orphanContiguityDonorsSQL = `
                        SELECT ` + orphanDonorColumns + `, ` + sqlSessionLastActive("d") + ` AS last_active
                        FROM sessions d
                        WHERE d.session_key IS NOT NULL
                          AND d.id != ?
                          AND COALESCE(d.source, '') = COALESCE(?, '')
                          AND (COALESCE(d.user_id, '') = ''
                               OR COALESCE(?, '') = ''
                               OR d.user_id = ?)
                          AND ` + sqlSessionLastActive("d") + ` BETWEEN ? AND ?
                          AND ` + sqlSessionLastActive("d") + ` < ?
                          ` + orphanDonorProfileFenceSQL + `
                        ORDER BY last_active DESC
                        LIMIT 2
                        `

// Seconds within which a keyed predecessor must fall quiet for a contiguity adoption
const orphanAdoptionMaxGapS = 900.0

const handoffFailSQL = "UPDATE sessions SET handoff_state = 'failed', handoff_error = ? WHERE "

type GatewayPeer struct {
	Source                      string
	UserID                      *string
	SessionKey                  *string
	ChatID                      *string
	ChatType                    *string
	ThreadID                    *string
	DisplayName                 *string
	OriginJSON                  *string
	IncludeCompressionAncestors bool
	TransportProfile            *string
}

// Persists the routing peer on the row or its compression lineage, inserting a missing row with full identity
func (d *SessionDB) RecordGatewaySessionPeer(ctx context.Context, sessionID string, p GatewayPeer) error {
	if sessionID == "" || p.SessionKey == nil || *p.SessionKey == "" {
		return nil
	}
	identity := []any{*p.SessionKey, p.Source, optStr(p.UserID), optStr(p.ChatID), optStr(p.ChatType), optStr(p.ThreadID),
		optStr(p.DisplayName), optStr(p.OriginJSON), optStr(p.TransportProfile)}
	ancestors := p.IncludeCompressionAncestors
	head, where := "", "WHERE id = ?"
	params := append(append([]any{}, identity...), sessionID)
	if ancestors {
		requireHook(MessagingPlatformSource != nil, "MessagingPlatformSource", "pkg/gateway session_route_recovery messaging platform")
		head, where = compressionLineageCTE, "WHERE id IN (SELECT id FROM compression_lineage)"
		params = append([]any{sessionID}, identity...)
	}
	return d.executeWrite(ctx, func(conn SQLiteConn) error {
		if _, err := conn.ExecContext(ctx, head+`
                   UPDATE sessions
                   SET session_key = ?, source = ?, user_id = ?, chat_id = ?,
                       chat_type = ?, thread_id = ?,
                       display_name = COALESCE(?, display_name),
                       origin_json = COALESCE(?, origin_json),
                       transport_profile = COALESCE(?, transport_profile)
                   `+where, params...); err != nil {
			return err
		}
		if ancestors {
			rows, err := connRows(ctx, conn, compressionLineageCTE+" SELECT id FROM compression_lineage", sessionID)
			if err != nil {
				return err
			}
			moved := map[string]bool{}
			for _, row := range rows {
				moved[pyStr(row["id"])] = true
			}
			found, err := movedReceipts(ctx, conn, moved)
			if err != nil {
				return err
			}
			var lane []movedReceipt
			for _, m := range found {
				if label, ok := m.Source.(string); ok && MessagingPlatformSource(label) {
					lane = append(lane, m)
				}
			}
			_, err = handOffConversations(ctx, conn, lane)
			return err
		}
		row, err := connRow(ctx, conn, "SELECT 1 AS one FROM sessions WHERE id = ? LIMIT 1", sessionID)
		if err != nil || row != nil {
			return err
		}
		_, err = conn.ExecContext(ctx, `INSERT INTO sessions (
                               id, source, created_source, user_id, session_key, chat_id,
                               chat_type, thread_id, display_name, origin_json,
                               profile_name, transport_profile, started_at
                           )
                           VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
                           ON CONFLICT(id) DO UPDATE SET
                               session_key = COALESCE(sessions.session_key, excluded.session_key),
                               chat_id = COALESCE(sessions.chat_id, excluded.chat_id),
                               chat_type = COALESCE(sessions.chat_type, excluded.chat_type),
                               thread_id = COALESCE(sessions.thread_id, excluded.thread_id),
                               display_name = COALESCE(sessions.display_name, excluded.display_name),
                               origin_json = COALESCE(sessions.origin_json, excluded.origin_json),
                               transport_profile = COALESCE(sessions.transport_profile, excluded.transport_profile),
                               created_source = COALESCE(sessions.created_source, excluded.created_source)`,
			sessionID, p.Source, p.Source, optStr(p.UserID), *p.SessionKey, optStr(p.ChatID), optStr(p.ChatType), optStr(p.ThreadID),
			optStr(p.DisplayName), optStr(p.OriginJSON), nullStr(d.ownProfileName()), optStr(p.TransportProfile), now())
		return err
	}, nil)
}

// Upserts one routing entry in the scope unless its route is retired
func (d *SessionDB) SaveGatewayRoutingEntry(ctx context.Context, sessionKey, entryJSON, scope string) error {
	if sessionKey == "" || entryJSON == "" {
		return nil
	}
	params := append([]any{sql.Named("scope", scope), sql.Named("key", sessionKey)}, routeRetiredParams(sessionKey, entryJSON, now())...)
	return d.writeSQL(ctx, `INSERT INTO gateway_routing (scope, session_key, entry_json, updated_at)
               SELECT :scope, :key, :entry, :now WHERE NOT `+routeRetiredSQL+`
               ON CONFLICT(scope, session_key) DO UPDATE SET
                   entry_json = excluded.entry_json,
                   updated_at = excluded.updated_at`, params, false, nil)
}

// Atomically replaces the routing index of one scope, returning the keys skipped as retired routes
func (d *SessionDB) ReplaceGatewayRoutingEntries(ctx context.Context, entries map[string]string, scope string) (map[string]bool, error) {
	at := now()
	keys := make([]string, 0, len(entries))
	for k, v := range entries {
		if k != "" && v != "" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	skipped := map[string]bool{}
	err := d.executeWrite(ctx, func(conn SQLiteConn) error {
		if _, err := conn.ExecContext(ctx, "DELETE FROM gateway_routing WHERE scope = ?", scope); err != nil {
			return err
		}
		for _, k := range keys {
			params := append([]any{sql.Named("scope", scope), sql.Named("key", k)}, routeRetiredParams(k, entries[k], at)...)
			if _, err := conn.ExecContext(ctx, "INSERT INTO gateway_routing (scope, session_key, entry_json, updated_at) "+
				"SELECT :scope, :key, :entry, :now WHERE NOT "+routeRetiredSQL, params...); err != nil {
				return err
			}
		}
		rows, err := connRows(ctx, conn, "SELECT session_key FROM gateway_routing WHERE scope = ?", scope)
		if err != nil {
			return err
		}
		kept := map[string]bool{}
		for _, row := range rows {
			kept[pyStr(row["session_key"])] = true
		}
		clear(skipped)
		for _, k := range keys {
			if !kept[k] {
				skipped[k] = true
			}
		}
		return nil
	}, nil)
	return skipped, err
}

// Routing entries of one scope keyed by session key
func (d *SessionDB) LoadGatewayRoutingEntries(ctx context.Context, scope string) (map[string]string, error) {
	rows, err := d.readAll(ctx, "SELECT session_key, entry_json FROM gateway_routing WHERE scope = ?", scope)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(rows))
	for _, r := range rows {
		out[pyStr(rowValue(r, "session_key"))] = pyStr(rowValue(r, "entry_json"))
	}
	return out, nil
}

// Keyed open rows older than the cutoff with no message, token, call, activity, title or keep intent
func (d *SessionDB) ListNeverActiveKeyedSessions(ctx context.Context, olderThanDays float64) ([]*pyjson.Object, error) {
	if olderThanDays < 0 {
		repr, _ := pyReprFloat(olderThanDays)
		return nil, pyValueError("older_than_days must be >= 0, got " + repr + ": a negative " +
			"retention builds a future cutoff that matches every never-active keyed row.")
	}
	cutoff := now() - olderThanDays*86400.0
	return d.readAll(ctx, `
            SELECT s.id, s.session_key, s.source, s.chat_id,
                   s.chat_type, s.user_id, s.started_at
              FROM sessions s
             WHERE s.session_key IS NOT NULL
               AND s.ended_at IS NULL
               AND s.title IS NULL
               AND s.last_activity_at IS NULL
               AND COALESCE(s.message_count, 0) = 0
               AND COALESCE(s.tool_call_count, 0) = 0
               AND COALESCE(s.api_call_count, 0) = 0
               AND COALESCE(s.input_tokens, 0) = 0
               AND COALESCE(s.output_tokens, 0) = 0
               AND COALESCE(s.pinned, 0) = 0
               AND COALESCE(s.archived, 0) = 0
               AND s.started_at IS NOT NULL
               AND s.started_at < ?
               AND NOT EXISTS (
                       SELECT 1 FROM messages m WHERE m.session_id = s.id
                   )
             ORDER BY s.started_at
            `, cutoff)
}

// Decoded routing entry object, nil for a malformed, non text or non object entry
func routingEntryObject(raw any) *pyjson.Object {
	if !pyTruthy(raw) {
		return pyjson.NewObject()
	}
	return localReceiptObject(raw)
}

// Routing entry in any scope whose current owner is the session, nil when none names it
func (d *SessionDB) GatewayRoutingEntryForSession(ctx context.Context, sessionID string) (*pyjson.Object, error) {
	rows, err := d.readAll(ctx, "SELECT entry_json FROM gateway_routing")
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		entry := routingEntryObject(rowValue(r, "entry_json"))
		if entry == nil {
			continue
		}
		if v, _ := entry.Get("session_id"); v == any(sessionID) {
			return entry, nil
		}
	}
	return nil, nil
}

type routingRef struct {
	Scope, Key, SessionID string
}

// Scope, key and target of every routing row pointing at one of the sessions
func (d *SessionDB) routingEntriesForSessions(ctx context.Context, sessionIDs map[string]bool) ([]routingRef, error) {
	if len(sessionIDs) == 0 {
		return nil, nil
	}
	rows, err := d.readAll(ctx, "SELECT scope, session_key, entry_json FROM gateway_routing")
	if err != nil {
		return nil, err
	}
	var found []routingRef
	for _, r := range rows {
		entry := routingEntryObject(rowValue(r, "entry_json"))
		if entry == nil {
			continue
		}
		target, _ := entry.Get("session_id")
		hit, err := hashableIn(target, sessionIDs)
		if err != nil {
			return nil, err
		}
		if hit {
			found = append(found, routingRef{pyStr(rowValue(r, "scope")), pyStr(rowValue(r, "session_key")), target.(string)})
		}
	}
	return found, nil
}

// Drops the routing rows pointing at any of the sessions and returns how many went
func (d *SessionDB) deleteRoutingEntriesForSessions(ctx context.Context, sessionIDs map[string]bool) (int, error) {
	refs, err := d.routingEntriesForSessions(ctx, sessionIDs)
	if err != nil || len(refs) == 0 {
		return 0, err
	}
	doomed := make([]any, len(refs))
	for i, ref := range refs {
		doomed[i] = []any{ref.Scope, ref.Key}
	}
	if err := d.writeSQL(ctx, "DELETE FROM gateway_routing WHERE scope = ? AND session_key = ?", doomed, true, nil); err != nil {
		return 0, err
	}
	return len(refs), nil
}

// Publishes routing fences for sessions retired in the namespace's profile and drops its routes naming them
func (d *SessionDB) RetireGatewayRoutes(ctx context.Context, sessionIDs []string, namespace string, expiring []string) error {
	set := map[string]bool{}
	for _, sid := range sessionIDs {
		if sid != "" {
			set[sid] = true
		}
	}
	if len(set) == 0 {
		return nil
	}
	ids := make([]string, 0, len(set))
	for sid := range set {
		ids = append(ids, sid)
	}
	sort.Strings(ids)
	return d.executeWrite(ctx, func(conn SQLiteConn) error {
		return fenceSharedRoutes(ctx, conn, namespace, ids, expiring)
	}, nil)
}

type NeverActivePruneOptions struct {
	OlderThanDays float64
	SessionsDir   string
	DeletedIDs    *[]string
}

// Deletes never active keyed rows outside live guards and the routing entries naming them
func (d *SessionDB) PruneNeverActiveKeyedSessions(ctx context.Context, o NeverActivePruneOptions) (int, int, int, error) {
	list := d.ListNeverActiveKeyedSessions
	if d.neverActiveListSeam != nil {
		list = d.neverActiveListSeam
	}
	candidates, err := list(ctx, o.OlderThanDays)
	if err != nil {
		return 0, 0, 0, err
	}
	ids := map[string]bool{}
	err = d.readCtx(ctx, false, func(conn SQLiteConn) error {
		for _, row := range candidates {
			sid := pyStr(rowValue(row, "id"))
			owned, err := ownedLineageIDs(ctx, conn, sid)
			if err != nil {
				return err
			}
			if len(owned) == 1 && owned[0] == sid {
				ids[sid] = true
			}
		}
		return nil
	})
	if err != nil || len(ids) == 0 {
		return 0, 0, 0, err
	}
	routedBefore, err := d.routingEntriesForSessions(ctx, ids)
	if err != nil {
		return 0, 0, 0, err
	}
	sorted := make([]string, 0, len(ids))
	for sid := range ids {
		sorted = append(sorted, sid)
	}
	sort.Strings(sorted)
	var skipped []string
	deleted, err := d.DeleteSessions(ctx, sorted, DeleteSessionsOptions{SessionsDir: o.SessionsDir, ExcludeActiveWriteGuards: true, SkippedIDs: &skipped})
	if err != nil {
		return 0, 0, 0, err
	}
	kept := map[string]bool{}
	for _, sid := range skipped {
		kept[sid] = true
	}
	rest := map[string]bool{}
	var gone []string
	for _, sid := range sorted {
		if !kept[sid] {
			rest[sid] = true
			gone = append(gone, sid)
		}
	}
	if _, err := d.deleteRoutingEntriesForSessions(ctx, rest); err != nil {
		return 0, 0, 0, err
	}
	routingDeleted := 0
	for _, ref := range routedBefore {
		if !kept[ref.SessionID] {
			routingDeleted++
		}
	}
	if o.DeletedIDs != nil {
		*o.DeletedIDs = append(*o.DeletedIDs, gone...)
	}
	return deleted, routingDeleted, len(skipped), nil
}

// Newest row per routing key, filtered by source platform and open rows unless every row is asked for
func (d *SessionDB) ListGatewaySessions(ctx context.Context, platform string, activeOnly bool) ([]*pyjson.Object, error) {
	d.FlushForRead(ctx)
	query := `
            SELECT sessions.*,
                   COALESCE(sp.prompt, sessions.system_prompt)
                       AS _system_prompt_resolved,
                   ` + sqlSessionLastActive("sessions") + ` AS last_active
            FROM sessions
            LEFT JOIN system_prompts sp
              ON sp.hash = sessions.system_prompt_hash
            WHERE session_key IS NOT NULL
              AND started_at = (
                  SELECT MAX(s2.started_at) FROM sessions s2
                  WHERE s2.session_key = sessions.session_key
              )
        `
	var params []any
	if platform != "" {
		query += " AND LOWER(source) = LOWER(?)"
		params = append(params, platform)
	}
	if activeOnly {
		query += " AND ended_at IS NULL"
	}
	rows, err := d.readAll(ctx, query+" ORDER BY last_active DESC", params...)
	if err != nil {
		return nil, err
	}
	out := make([]*pyjson.Object, len(rows))
	for i, r := range rows {
		out[i] = d.sessionRowDict(r)
	}
	return out, nil
}

type GatewayPeerQuery struct {
	Source     string
	UserID     *string
	SessionKey *string
	ChatID     *string
	ChatType   *string
	ThreadID   *string
}

// Latest recoverable gateway row for the peer by exact key, then by the profile fenced full peer tuple
func (d *SessionDB) FindLatestGatewaySessionForPeer(ctx context.Context, q GatewayPeerQuery) (*pyjson.Object, error) {
	if q.SessionKey == nil || *q.SessionKey == "" {
		return nil, nil
	}
	var found *pyjson.Object
	err := d.readCtx(ctx, false, func(conn SQLiteConn) error {
		rows, err := queryObjects(ctx, conn, peerByKeySQL, *q.SessionKey, q.Source)
		if err != nil {
			return err
		}
		if len(rows) > 0 {
			found = rows[0]
			return nil
		}
		if q.ChatID == nil || q.ChatType == nil {
			return nil
		}
		owner := nullStr(d.ownProfileName())
		rows, err = queryObjects(ctx, conn, peerByTupleSQL, q.Source, optStr(q.UserID), *q.ChatID, *q.ChatType, optStr(q.ThreadID),
			owner, owner, owner, owner, owner, owner)
		if err != nil {
			return err
		}
		if len(rows) > 0 {
			found = rows[0]
		}
		return nil
	})
	if err != nil || found == nil {
		return nil, err
	}
	return d.sessionRowDict(found), nil
}

// Message bearing rows that lost their routing identity, adoptable only when exactly one keyed predecessor is named
func (d *SessionDB) FindOrphanedGatewaySessions(ctx context.Context, maxGapS *float64) ([]*pyjson.Object, error) {
	gap := orphanAdoptionMaxGapS
	if maxGapS != nil {
		gap = *maxGapS
	}
	owner := nullStr(d.ownProfileName())
	var records []*pyjson.Object
	err := d.readCtx(ctx, false, func(conn SQLiteConn) error {
		orphans, err := queryObjects(ctx, conn, orphansSQL)
		if err != nil {
			return err
		}
		for _, orphan := range orphans {
			var donor *pyjson.Object
			reason, evidence := "", ""
			orphanOwner := rowValue(orphan, "profile_name")
			if !pyTruthy(orphanOwner) {
				orphanOwner = owner
			}
			if pyTruthy(rowValue(orphan, "parent_session_id")) {
				evidence = "lineage"
				rows, err := queryObjects(ctx, conn, orphanLineageDonorSQL, rowValue(orphan, "parent_session_id"), rowValue(orphan, "source"), owner, owner, orphanOwner)
				if err != nil {
					return err
				}
				if len(rows) > 0 {
					donor = rows[0]
				} else {
					reason = "parent session carries no gateway identity of this source"
				}
			} else {
				evidence = "contiguity"
				started := 0.0
				if v := rowValue(orphan, "started_at"); pyTruthy(v) {
					if started, err = pyFloatOf(v); err != nil {
						return err
					}
				}
				candidates, err := queryObjects(ctx, conn, orphanContiguityDonorsSQL, rowValue(orphan, "id"), rowValue(orphan, "source"),
					rowValue(orphan, "user_id"), rowValue(orphan, "user_id"), started-gap, started+gap, rowValue(orphan, "last_active"),
					owner, owner, orphanOwner)
				if err != nil {
					return err
				}
				switch {
				case len(candidates) == 0:
					reason = "no keyed predecessor fell quiet within " + strconv.FormatFloat(gap, 'f', 0, 64) + "s of this session's start"
				case len(candidates) > 1:
					reason = "ambiguous: more than one keyed predecessor matches this window"
				default:
					donor = candidates[0]
				}
			}
			record := pyjson.NewObject()
			record.Set("orphan_id", rowValue(orphan, "id"))
			record.Set("source", rowValue(orphan, "source"))
			record.Set("message_count", rowValue(orphan, "message_count"))
			record.Set("started_at", rowValue(orphan, "started_at"))
			record.Set("last_active", rowValue(orphan, "last_active"))
			var donorID, donorKey any
			if donor != nil {
				donorID, donorKey = rowValue(donor, "id"), rowValue(donor, "session_key")
			} else {
				evidence = ""
			}
			record.Set("donor_id", donorID)
			record.Set("session_key", donorKey)
			record.Set("evidence", evidence)
			record.Set("adoptable", donor != nil)
			record.Set("reason", reason)
			records = append(records, record)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	claims := map[any]int{}
	for _, r := range records {
		claims[rowValue(r, "donor_id")]++
	}
	contested := map[any]bool{}
	for _, r := range records {
		if rowValue(r, "adoptable") == true && claims[rowValue(r, "donor_id")] > 1 {
			contested[rowValue(r, "donor_id")] = true
		}
	}
	for _, r := range records {
		if contested[rowValue(r, "donor_id")] {
			r.Set("adoptable", false)
			r.Set("reason", "ambiguous: more than one unkeyed session claims this predecessor")
		}
	}
	return records, nil
}

// Stamps the orphan with the donor's routing identity and retires the donor, rechecking the pair inside the write
func (d *SessionDB) AdoptOrphanedGatewaySession(ctx context.Context, orphanID, donorID string) (bool, error) {
	if orphanID == "" || donorID == "" || orphanID == donorID {
		return false, nil
	}
	owner := d.ownProfileName()
	adopted := false
	err := d.executeWrite(ctx, func(conn SQLiteConn) error {
		donor, err := connRow(ctx, conn, "SELECT session_key, chat_id, chat_type, thread_id, user_id, "+
			"origin_json, display_name, source, profile_name FROM sessions WHERE id = ?", donorID)
		if err != nil {
			return err
		}
		orphan, err := connRow(ctx, conn, "SELECT session_key, source, profile_name FROM sessions WHERE id = ?", orphanID)
		if err != nil {
			return err
		}
		effective := func(v any) any {
			if pyTruthy(v) {
				return v
			}
			return owner
		}
		if donor == nil || orphan == nil || !pyTruthy(donor["session_key"]) || pyTruthy(orphan["session_key"]) ||
			pyStrOr(donor["source"]) != pyStrOr(orphan["source"]) ||
			(owner != "" && effective(donor["profile_name"]) != effective(orphan["profile_name"])) {
			return nil
		}
		if _, err := conn.ExecContext(ctx, `UPDATE sessions
                      SET session_key = ?,
                          chat_id = COALESCE(chat_id, ?),
                          chat_type = COALESCE(chat_type, ?),
                          thread_id = COALESCE(thread_id, ?),
                          user_id = COALESCE(user_id, ?),
                          origin_json = COALESCE(origin_json, ?),
                          display_name = COALESCE(display_name, ?),
                          parent_session_id = COALESCE(parent_session_id, ?)
                    WHERE id = ? AND session_key IS NULL`,
			donor["session_key"], donor["chat_id"], donor["chat_type"], donor["thread_id"],
			donor["user_id"], donor["origin_json"], donor["display_name"], donorID, orphanID); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, "UPDATE sessions SET ended_at = COALESCE(ended_at, ?), "+
			"end_reason = 'superseded_by_repair' WHERE id = ?", now(), donorID); err != nil {
			return err
		}
		adopted = true
		return nil
	}, nil)
	return adopted, err
}

// Text of a value with None and empty reading as the empty string
func pyStrOr(v any) string {
	if !pyTruthy(v) {
		return ""
	}
	return pyStr(v)
}

// Atomically bumps the hygiene failure streak of one chat and returns the new count
func (d *SessionDB) IncrementHygieneFailureStreak(ctx context.Context, sessionKey string) (int, error) {
	if sessionKey == "" {
		return 1, nil
	}
	streak := 0
	err := d.executeWrite(ctx, func(conn SQLiteConn) error {
		if _, err := conn.ExecContext(ctx, `INSERT INTO gateway_hygiene_state (session_key, failure_streak)
                   VALUES (?, 1)
                   ON CONFLICT(session_key) DO UPDATE SET
                       failure_streak = gateway_hygiene_state.failure_streak + 1`, sessionKey); err != nil {
			return err
		}
		return conn.QueryRowContext(ctx, "SELECT failure_streak FROM gateway_hygiene_state WHERE session_key = ?", sessionKey).Scan(&streak)
	}, nil)
	return streak, err
}

// Clears the persisted hygiene failure streak of one chat
func (d *SessionDB) ResetHygieneFailureStreak(ctx context.Context, sessionKey string) error {
	if sessionKey == "" {
		return nil
	}
	return d.writeSQL(ctx, "DELETE FROM gateway_hygiene_state WHERE session_key = ?", []any{sessionKey}, false, nil)
}

// Copy of a runtime object without its None valued keys
func withoutNone(o *pyjson.Object) *pyjson.Object {
	out := pyjson.NewObject()
	for _, k := range o.Keys {
		if v, _ := o.Get(k); v != nil {
			out.Set(k, v)
		}
	}
	return out
}

// Persisted runtime route of a session row, nested gateway runtime first, then the top level keys and billing provider
func SessionGatewayRuntime(sessionMeta *pyjson.Object) *pyjson.Object {
	meta := sessionMeta
	if meta == nil {
		meta = pyjson.NewObject()
	}
	rawValue, _ := meta.Get("model_config")
	if text, ok := rawValue.(string); ok {
		decoded, err := pyjson.Loads(text)
		if err != nil {
			decoded = pyjson.NewObject()
		}
		rawValue = decoded
	}
	raw, ok := rawValue.(*pyjson.Object)
	if !ok {
		raw = pyjson.NewObject()
	}
	runtimeValue, _ := raw.Get("gateway_runtime")
	runtime, isDict := runtimeValue.(*pyjson.Object)
	if isDict {
		if provider, _ := runtime.Get("provider"); pyTruthy(provider) {
			return withoutNone(runtime)
		}
	}
	top := pyjson.NewObject()
	for _, key := range []string{"provider", "base_url", "api_mode"} {
		if v, _ := raw.Get(key); pyTruthy(v) {
			top.Set(key, v)
		}
	}
	billingValue, _ := meta.Get("billing_provider")
	billing := pyStrip(pyStrOr(billingValue))
	lowered := pyLower(billing)
	if billing != "" && !bareBillingProviders[lowered] {
		if _, has := top.Get("provider"); !has {
			top.Set("provider", billing)
		}
	} else if provider, _ := top.Get("provider"); lowered == "custom" && !pyTruthy(provider) {
		baseValue, _ := meta.Get("billing_base_url")
		if base := pyStrip(pyStrOr(baseValue)); base != "" {
			if _, has := top.Get("base_url"); !has {
				top.Set("base_url", base)
			}
			top.Set("provider", "custom")
		}
	}
	if top.Len() > 0 {
		return top
	}
	if !isDict {
		return pyjson.NewObject()
	}
	return withoutNone(runtime)
}

// Marks the session pending handoff to the platform, false while another handoff is in flight
func (d *SessionDB) RequestHandoff(ctx context.Context, sessionID, platform string) (bool, error) {
	n, err := d.writeRowcount(ctx, "UPDATE sessions SET handoff_state = 'pending',     handoff_platform = ?, "+
		"    handoff_error = NULL WHERE id = ? AND (handoff_state IS NULL "+
		"                  OR handoff_state IN ('completed', 'failed'))", []any{platform, sessionID}, nil)
	return n > 0, err
}

// State, platform and error of the session's handoff record, nil when it has none or the read fails
func (d *SessionDB) GetHandoffState(ctx context.Context, sessionID string) *pyjson.Object {
	row, err := d.readOne(ctx, "SELECT handoff_state, handoff_platform, handoff_error FROM sessions WHERE id = ?", sessionID)
	if err != nil || row == nil {
		return nil
	}
	out := pyjson.NewObject()
	out.Set("state", rowValue(row, "handoff_state"))
	out.Set("platform", rowValue(row, "handoff_platform"))
	out.Set("error", rowValue(row, "handoff_error"))
	return out
}

// Every session pending handoff, oldest first, empty when the read fails
func (d *SessionDB) ListPendingHandoffs(ctx context.Context) []*pyjson.Object {
	rows, err := d.readAll(ctx, "SELECT s.*, COALESCE(sp.prompt, s.system_prompt) AS _system_prompt_resolved FROM sessions s "+
		"LEFT JOIN system_prompts sp ON sp.hash = s.system_prompt_hash "+
		"WHERE s.handoff_state = 'pending' "+
		"ORDER BY s.started_at ASC")
	if err != nil {
		return []*pyjson.Object{}
	}
	out := make([]*pyjson.Object, len(rows))
	for i, r := range rows {
		out[i] = d.sessionRowDict(r)
	}
	return out
}

// Atomically moves a pending handoff to running and reports whether this caller claimed it
func (d *SessionDB) ClaimHandoff(ctx context.Context, sessionID string) (bool, error) {
	n, err := d.writeRowcount(ctx, "UPDATE sessions SET handoff_state = 'running' WHERE id = ? AND handoff_state = 'pending'", []any{sessionID}, nil)
	return n > 0, err
}

// True when any session waits in the pending handoff state
func (d *SessionDB) HasPendingHandoffs(ctx context.Context) (bool, error) {
	row, err := d.readOne(ctx, "SELECT 1 AS one FROM sessions WHERE handoff_state = 'pending' LIMIT 1")
	return row != nil, err
}

// Marks the session's handoff completed and clears its error
func (d *SessionDB) CompleteHandoff(ctx context.Context, sessionID string) error {
	return d.writeSQL(ctx, "UPDATE sessions SET handoff_state = 'completed', handoff_error = NULL WHERE id = ?", []any{sessionID}, false, nil)
}

// First 500 code points of a handoff error
func handoffErrorText(text string) string {
	runes := []rune(text)
	if len(runes) > 500 {
		return string(runes[:500])
	}
	return text
}

// Marks the handoff failed with the reason, a compare and swap on the state when only states are given
func (d *SessionDB) FailHandoff(ctx context.Context, sessionID, reason string, onlyStates []string) (bool, error) {
	query := handoffFailSQL + "id = ?"
	params := []any{handoffErrorText(reason), sessionID}
	if len(onlyStates) > 0 {
		query += " AND handoff_state IN (" + placeholders(len(onlyStates)) + ")"
		for _, s := range onlyStates {
			params = append(params, s)
		}
	}
	n, err := d.writeRowcount(ctx, query, params, nil)
	return n > 0, err
}

// Fails every handoff stuck running from a dead gateway and returns their ids, logging a failed reclaim
func (d *SessionDB) ReclaimStaleRunningHandoffs(ctx context.Context, reason string) []string {
	var ids []string
	err := d.executeWrite(ctx, func(conn SQLiteConn) error {
		ids = nil
		rows, err := connRows(ctx, conn, "SELECT id FROM sessions WHERE handoff_state = 'running'")
		if err != nil {
			return err
		}
		for _, row := range rows {
			ids = append(ids, pyStr(row["id"]))
		}
		if len(ids) == 0 {
			return nil
		}
		_, err = conn.ExecContext(ctx, handoffFailSQL+"handoff_state = 'running'", handoffErrorText(reason))
		return err
	}, nil)
	if err != nil {
		slog.Warn("reclaim_stale_running_handoffs failed; stranded 'running' " +
			"handoff rows (if any) were left in place\n" + errText(err))
		return []string{}
	}
	if ids == nil {
		return []string{}
	}
	return ids
}
