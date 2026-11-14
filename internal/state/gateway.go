package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/internal/fileutil"
)

const OrphanAdoptionMaxGap = 900.0

var bareBillingProviders = []string{"auto", "custom"}

var compressionLineageCTE = `
                    WITH RECURSIVE compression_lineage(id) AS (
                        SELECT ?
                        UNION
                        SELECT parent.id
                        FROM compression_lineage lineage
                        JOIN sessions child ON child.id = lineage.id
                        JOIN sessions parent ON parent.id = child.parent_session_id
                        WHERE parent.end_reason = 'compression'
                          AND ` + JSONExtractSQL("child.model_config", "$._branched_from") + ` IS NULL
                          AND ` + JSONExtractSQL("child.model_config", "$._delegate_from") + ` IS NULL
                          AND NOT (` + ResetChildSQL("child") + `)
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

const handoffOwnedRowSQL = "(s.handoff_state = 'completed')"

var peerByKeySQL = peerSelectHead + `                WHERE s.session_key = ?
                  AND s.source = ?
                  AND (s.ended_at IS NULL OR s.end_reason IN (` + sqlLiteralList(recoverableEndReasons) + `)
                       OR ` + handoffOwnedRowSQL + `)
                  AND NOT EXISTS (
                      SELECT 1 FROM sessions b
                      WHERE b.session_key = s.session_key
                        AND b.source = s.source
                        AND b.ended_at IS NOT NULL
                        AND b.end_reason IN (` + sqlLiteralList(resetEndReasons) + `)
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
                  AND (s.ended_at IS NULL OR s.end_reason IN (` + sqlLiteralList(recoverableEndReasons) + `)
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
                        AND b.end_reason IN (` + sqlLiteralList(resetEndReasons) + `)
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
                       ` + SessionLastActiveSQL("o") + ` AS last_active,
                       (SELECT COUNT(*) FROM messages m
                         WHERE m.session_id = o.id) AS message_count
                FROM sessions o
                WHERE o.session_key IS NULL
                  AND EXISTS (SELECT 1 FROM messages m
                               WHERE m.session_id = o.id)
                  AND COALESCE(o.source, '') != 'tool'
                  AND ` + JSONExtractSQL("o.model_config", "$._branched_from") + ` IS NULL
                  AND ` + JSONExtractSQL("o.model_config", "$._delegate_from") + ` IS NULL
                ORDER BY o.started_at ASC
                `

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
                        SELECT ` + orphanDonorColumns + `, ` + SessionLastActiveSQL("d") + ` AS last_active
                        FROM sessions d
                        WHERE d.session_key IS NOT NULL
                          AND d.id != ?
                          AND COALESCE(d.source, '') = COALESCE(?, '')
                          AND (COALESCE(d.user_id, '') = ''
                               OR COALESCE(?, '') = ''
                               OR d.user_id = ?)
                          AND ` + SessionLastActiveSQL("d") + ` BETWEEN ? AND ?
                          AND ` + SessionLastActiveSQL("d") + ` < ?
                          ` + orphanDonorProfileFenceSQL + `
                        ORDER BY last_active DESC
                        LIMIT 2
                        `

var optionalTableNames = []string{"telegram_dm_topic_mode", "telegram_dm_topic_bindings", "delivery_obligations"}

const handoffFailSQL = "UPDATE sessions SET handoff_state = 'failed', handoff_error = ? WHERE "

func optionalTableColumns(ctx context.Context, c Conn) (map[string][]string, error) {
	existing, err := queryStrings(ctx, c, "SELECT name FROM sqlite_master WHERE type='table' AND name IN (?, ?, ?)", stringArgs(optionalTableNames)...)
	if err != nil {
		return nil, err
	}
	out := map[string][]string{}
	for _, table := range optionalTableNames {
		if !containsString(existing, table) {
			continue
		}
		cols, err := tableInfo(ctx, c, table)
		if err != nil {
			return nil, err
		}
		for _, col := range cols {
			out[table] = append(out[table], col.name)
		}
	}
	return out, nil
}

func messagingPlatform(label string) bool {
	if label == "local" || label == "api_server" {
		return false
	}
	return containsString(messagingPlatformValues, label) || containsString(messagingPlatformDirs, label)
}

type GatewayPeer struct {
	Source                      string
	UserID                      *string
	SessionKey                  *string
	ChatID                      *string
	ChatType                    *string
	ThreadID                    *string
	DisplayName                 *string
	OriginJSON                  *string
	TransportProfile            *string
	IncludeCompressionAncestors bool
}

func (s *Store) ownProfileArg() any {
	if own := s.OwnProfileName(); own != "" {
		return own
	}
	return nil
}

func (s *Store) RecordGatewaySessionPeer(ctx context.Context, sessionID string, p GatewayPeer) error {
	if sessionID == "" || p.SessionKey == nil || *p.SessionKey == "" {
		return nil
	}
	identity := []any{*p.SessionKey, p.Source, textArg(p.UserID), textArg(p.ChatID), textArg(p.ChatType), textArg(p.ThreadID),
		textArg(p.DisplayName), textArg(p.OriginJSON), textArg(p.TransportProfile)}
	update := `
                   UPDATE sessions
                   SET session_key = ?, source = ?, user_id = ?, chat_id = ?,
                       chat_type = ?, thread_id = ?,
                       display_name = COALESCE(?, display_name),
                       origin_json = COALESCE(?, origin_json),
                       transport_profile = COALESCE(?, transport_profile)
                   `
	return s.Write(ctx, WritePatience, func(c Conn) error {
		if p.IncludeCompressionAncestors {
			if _, err := c.ExecContext(ctx, compressionLineageCTE+update+"WHERE id IN (SELECT id FROM compression_lineage)",
				append([]any{sessionID}, identity...)...); err != nil {
				return err
			}
			moved, err := queryStrings(ctx, c, compressionLineageCTE+" SELECT id FROM compression_lineage", sessionID)
			if err != nil {
				return err
			}
			receipts, err := MovedReceiptsIn(ctx, c, moved)
			if err != nil {
				return err
			}
			messaging := map[string]MovedReceipt{}
			for owner, found := range receipts {
				if messagingPlatform(found.Source) {
					messaging[owner] = found
				}
			}
			_, err = HandOffConversationsIn(ctx, c, messaging)
			return err
		}
		if _, err := c.ExecContext(ctx, update+"WHERE id = ?", append(identity, sessionID)...); err != nil {
			return err
		}
		exists, err := queryStrings(ctx, c, "SELECT 1 FROM sessions WHERE id = ? LIMIT 1", sessionID)
		if err != nil || len(exists) > 0 {
			return err
		}
		_, err = c.ExecContext(ctx, `INSERT INTO sessions (
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
			sessionID, p.Source, p.Source, textArg(p.UserID), *p.SessionKey, textArg(p.ChatID), textArg(p.ChatType), textArg(p.ThreadID),
			textArg(p.DisplayName), textArg(p.OriginJSON), s.ownProfileArg(), textArg(p.TransportProfile), nowEpoch())
		return err
	})
}

func routingInsertArgs(scope, key, entry string, now float64) []any {
	return append([]any{sql.Named("scope", scope), sql.Named("key", key)}, routeRetiredArgs(key, entry, now)...)
}

func (s *Store) SaveGatewayRoutingEntry(ctx context.Context, sessionKey, entryJSON, scope string) error {
	if sessionKey == "" || entryJSON == "" {
		return nil
	}
	return s.WriteSQL(ctx, WritePatience, `INSERT INTO gateway_routing (scope, session_key, entry_json, updated_at)
               SELECT :scope, :key, :entry, :now WHERE NOT `+routeRetiredSQL+`
               ON CONFLICT(scope, session_key) DO UPDATE SET
                   entry_json = excluded.entry_json,
                   updated_at = excluded.updated_at`, routingInsertArgs(scope, sessionKey, entryJSON, nowEpoch())...)
}

func (s *Store) ReplaceGatewayRoutingEntries(ctx context.Context, entries map[string]string, scope string) ([]string, error) {
	now := nowEpoch()
	keys := make([]string, 0, len(entries))
	for k, v := range entries {
		if k != "" && v != "" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	var skipped []string
	err := s.Write(ctx, WritePatience, func(c Conn) error {
		skipped = nil
		if _, err := c.ExecContext(ctx, "DELETE FROM gateway_routing WHERE scope = ?", scope); err != nil {
			return err
		}
		for _, k := range keys {
			if _, err := c.ExecContext(ctx, "INSERT INTO gateway_routing (scope, session_key, entry_json, updated_at) "+
				"SELECT :scope, :key, :entry, :now WHERE NOT "+routeRetiredSQL, routingInsertArgs(scope, k, entries[k], now)...); err != nil {
				return err
			}
		}
		kept, err := queryStrings(ctx, c, "SELECT session_key FROM gateway_routing WHERE scope = ?", scope)
		if err != nil {
			return err
		}
		for _, k := range keys {
			if !containsString(kept, k) {
				skipped = append(skipped, k)
			}
		}
		return nil
	})
	return skipped, err
}

func (s *Store) LoadGatewayRoutingEntries(ctx context.Context, scope string) (map[string]string, error) {
	out := map[string]string{}
	err := s.Read(ctx, func(c Conn) error {
		clear(out)
		rows, err := c.QueryContext(ctx, "SELECT session_key, entry_json FROM gateway_routing WHERE scope = ?", scope)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var key string
			var entry sql.NullString
			if err := rows.Scan(&key, &entry); err != nil {
				return err
			}
			out[key] = entry.String
		}
		return rows.Err()
	})
	return out, err
}

type NeverActiveSession struct {
	ID         string
	SessionKey string
	Source     string
	ChatID     sql.Null[string]
	ChatType   sql.Null[string]
	UserID     sql.Null[string]
	StartedAt  float64
}

func (s *Store) ListNeverActiveKeyedSessions(ctx context.Context, olderThanDays float64) ([]NeverActiveSession, error) {
	if olderThanDays < 0 {
		return nil, fmt.Errorf("older_than_days must be >= 0, got %s: a negative "+
			"retention builds a future cutoff that matches every never-active keyed row.", shortestFloatText(olderThanDays))
	}
	cutoff := nowEpoch() - olderThanDays*86400.0
	var out []NeverActiveSession
	err := s.Read(ctx, func(c Conn) error {
		out = nil
		rows, err := c.QueryContext(ctx, `
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
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r NeverActiveSession
			var chatID, chatType, userID sql.NullString
			if err := rows.Scan(&r.ID, &r.SessionKey, &r.Source, &chatID, &chatType, &userID, &r.StartedAt); err != nil {
				return err
			}
			r.ChatID = sql.Null[string]{V: chatID.String, Valid: chatID.Valid}
			r.ChatType = sql.Null[string]{V: chatType.String, Valid: chatType.Valid}
			r.UserID = sql.Null[string]{V: userID.String, Valid: userID.Valid}
			out = append(out, r)
		}
		return rows.Err()
	})
	return out, err
}

type routeRef struct {
	scope     string
	key       string
	sessionID string
}

func entrySessionOf(entryJSON sql.NullString) (string, json.RawMessage, bool) {
	text := entryJSON.String
	if text == "" {
		text = "{}"
	}
	members, ok := objectMembers(json.RawMessage(text))
	if !ok {
		return "", nil, false
	}
	v, has := memberValue(members, "session_id")
	if !has || !jsonIsString(v) {
		return "", json.RawMessage(text), true
	}
	return jsonString(v), json.RawMessage(text), true
}

func (s *Store) GatewayRoutingEntryForSession(ctx context.Context, sessionID string) (json.RawMessage, bool, error) {
	var found json.RawMessage
	err := s.Read(ctx, func(c Conn) error {
		found = nil
		rows, err := c.QueryContext(ctx, "SELECT entry_json FROM gateway_routing")
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var entry sql.NullString
			if err := rows.Scan(&entry); err != nil {
				return err
			}
			sid, raw, ok := entrySessionOf(entry)
			if ok && raw != nil && sid == sessionID && jsonIsStringMember(raw) {
				found = raw
				return nil
			}
		}
		return rows.Err()
	})
	return found, found != nil, err
}

func jsonIsStringMember(raw json.RawMessage) bool {
	members, _ := objectMembers(raw)
	v, has := memberValue(members, "session_id")
	return has && jsonIsString(v)
}

func (s *Store) routingEntriesForSessions(ctx context.Context, sessionIDs []string) ([]routeRef, error) {
	var found []routeRef
	if len(sessionIDs) == 0 {
		return nil, nil
	}
	err := s.Read(ctx, func(c Conn) error {
		found = nil
		rows, err := c.QueryContext(ctx, "SELECT scope, session_key, entry_json FROM gateway_routing")
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var scope, key string
			var entry sql.NullString
			if err := rows.Scan(&scope, &key, &entry); err != nil {
				return err
			}
			sid, raw, ok := entrySessionOf(entry)
			if ok && jsonIsStringMember(raw) && containsString(sessionIDs, sid) {
				found = append(found, routeRef{scope, key, sid})
			}
		}
		return rows.Err()
	})
	return found, err
}

func (s *Store) deleteRoutingEntriesForSessions(ctx context.Context, sessionIDs []string) (int, error) {
	doomed, err := s.routingEntriesForSessions(ctx, sessionIDs)
	if err != nil || len(doomed) == 0 {
		return 0, err
	}
	err = s.Write(ctx, WritePatience, func(c Conn) error {
		for _, d := range doomed {
			if _, err := c.ExecContext(ctx, "DELETE FROM gateway_routing WHERE scope = ? AND session_key = ?", d.scope, d.key); err != nil {
				return err
			}
		}
		return nil
	})
	return len(doomed), err
}

func (s *Store) RetireGatewayRoutes(ctx context.Context, sessionIDs []string, namespace string, expiring []string) error {
	ids := UIDList(sessionIDs)
	sort.Strings(ids)
	if len(ids) == 0 {
		return nil
	}
	return s.Write(ctx, WritePatience, func(c Conn) error { return fenceSharedRoutes(ctx, c, namespace, ids, expiring) })
}

type NeverActivePrune struct {
	Deleted        int
	RoutingDeleted int
	Skipped        int
	DeletedIDs     []string
}

func (s *Store) PruneNeverActiveKeyedSessions(ctx context.Context, olderThanDays float64, sessionsDir string) (NeverActivePrune, error) {
	candidates, err := s.ListNeverActiveKeyedSessions(ctx, olderThanDays)
	if err != nil {
		return NeverActivePrune{}, err
	}
	var ids []string
	err = s.Read(ctx, func(c Conn) error {
		ids = nil
		for _, row := range candidates {
			owned, err := ownedLineageIDs(ctx, c, row.ID)
			if err != nil {
				return err
			}
			if len(owned) == 1 && owned[0] == row.ID && !containsString(ids, row.ID) {
				ids = append(ids, row.ID)
			}
		}
		return nil
	})
	if err != nil || len(ids) == 0 {
		return NeverActivePrune{}, err
	}
	sort.Strings(ids)
	routedBefore, err := s.routingEntriesForSessions(ctx, ids)
	if err != nil {
		return NeverActivePrune{}, err
	}
	deleted, skipped, err := s.DeleteSessions(ctx, ids, DeleteManyOptions{SessionsDir: sessionsDir, ExcludeActiveWriteGuards: true})
	if err != nil {
		return NeverActivePrune{}, err
	}
	var gone []string
	for _, id := range ids {
		if !containsString(skipped, id) {
			gone = append(gone, id)
		}
	}
	if _, err := s.deleteRoutingEntriesForSessions(ctx, gone); err != nil {
		return NeverActivePrune{}, err
	}
	routingDeleted := 0
	for _, r := range routedBefore {
		if !containsString(skipped, r.sessionID) {
			routingDeleted++
		}
	}
	return NeverActivePrune{Deleted: deleted, RoutingDeleted: routingDeleted, Skipped: len(skipped), DeletedIDs: gone}, nil
}

func (s *Store) ListGatewaySessions(ctx context.Context, platform string, activeOnly bool) ([]SessionListing, error) {
	s.tokenBarrier(ctx, true)
	query := `
            SELECT sessions.*,
                   COALESCE(sp.prompt, sessions.system_prompt)
                       AS _system_prompt_resolved,
                   ` + SessionLastActiveSQL("sessions") + ` AS last_active
            FROM sessions
            LEFT JOIN system_prompts sp
              ON sp.hash = sessions.system_prompt_hash
            WHERE session_key IS NOT NULL
              AND started_at = (
                  SELECT MAX(s2.started_at) FROM sessions s2
                  WHERE s2.session_key = sessions.session_key
              )
        `
	var args []any
	if platform != "" {
		query += " AND LOWER(source) = LOWER(?)"
		args = append(args, platform)
	}
	if activeOnly {
		query += " AND ended_at IS NULL"
	}
	query += " ORDER BY last_active DESC"
	var out []SessionListing
	err := s.Read(ctx, func(c Conn) error {
		var err error
		out, err = queryListings(c.QueryContext(ctx, query, args...))
		return err
	})
	return out, err
}

type PeerQuery struct {
	Source     string
	UserID     *string
	SessionKey *string
	ChatID     *string
	ChatType   *string
	ThreadID   *string
}

func (s *Store) FindLatestGatewaySessionForPeer(ctx context.Context, q PeerQuery) (Session, bool, error) {
	if q.SessionKey == nil || *q.SessionKey == "" {
		return Session{}, false, nil
	}
	var found []Session
	err := s.Read(ctx, func(c Conn) error {
		var err error
		if found, err = querySessions(c.QueryContext(ctx, peerByKeySQL, *q.SessionKey, q.Source)); err != nil || len(found) > 0 {
			return err
		}
		if q.ChatID == nil || q.ChatType == nil {
			return nil
		}
		owner := s.ownProfileArg()
		found, err = querySessions(c.QueryContext(ctx, peerByTupleSQL, q.Source, textArg(q.UserID), *q.ChatID, *q.ChatType,
			textArg(q.ThreadID), owner, owner, owner, owner, owner, owner))
		return err
	})
	if err != nil || len(found) == 0 {
		return Session{}, false, err
	}
	return found[0], true, nil
}

type OrphanRecord struct {
	OrphanID     string
	Source       sql.Null[string]
	MessageCount int64
	StartedAt    sql.Null[float64]
	LastActive   sql.Null[float64]
	DonorID      sql.Null[string]
	SessionKey   sql.Null[string]
	Evidence     string
	Adoptable    bool
	Reason       string
}

func nullFloatOf(v sql.NullFloat64) sql.Null[float64] {
	return sql.Null[float64]{V: v.Float64, Valid: v.Valid}
}

func nullTextOf(v sql.NullString) sql.Null[string] {
	return sql.Null[string]{V: v.String, Valid: v.Valid}
}

func scanDonors(rows *sql.Rows, err error, withLastActive bool) ([][2]string, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][2]string
	for rows.Next() {
		var id string
		var key sql.NullString
		var skip [7]any
		dest := []any{&id, &key, &skip[0], &skip[1], &skip[2], &skip[3], &skip[4], &skip[5], &skip[6]}
		if withLastActive {
			var last any
			dest = append(dest, &last)
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		out = append(out, [2]string{id, key.String})
	}
	return out, rows.Err()
}

func (s *Store) FindOrphanedGatewaySessions(ctx context.Context, maxGap *float64) ([]OrphanRecord, error) {
	gap := OrphanAdoptionMaxGap
	if maxGap != nil {
		gap = *maxGap
	}
	owner := s.ownProfileArg()
	var records []OrphanRecord
	err := s.Read(ctx, func(c Conn) error {
		records = nil
		rows, err := c.QueryContext(ctx, orphansSQL)
		if err != nil {
			return err
		}
		type orphan struct {
			id                                string
			source, userID, parentID, profile sql.NullString
			started, last                     sql.NullFloat64
			count                             int64
		}
		var orphans []orphan
		for rows.Next() {
			var o orphan
			if err := rows.Scan(&o.id, &o.source, &o.userID, &o.started, &o.parentID, &o.profile, &o.last, &o.count); err != nil {
				rows.Close()
				return err
			}
			orphans = append(orphans, o)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, o := range orphans {
			var donor *[2]string
			reason, evidence := "", ""
			orphanOwner := owner
			if o.profile.String != "" {
				orphanOwner = o.profile.String
			}
			if o.parentID.String != "" {
				evidence = "lineage"
				lineageRows, err := c.QueryContext(ctx, orphanLineageDonorSQL, o.parentID.String, nullTextArg(o.source), owner, owner, orphanOwner)
				donors, err := scanDonors(lineageRows, err, false)
				if err != nil {
					return err
				}
				if len(donors) == 0 {
					reason = "parent session carries no gateway identity of this source"
				} else {
					donor = &donors[0]
				}
			} else {
				evidence = "contiguity"
				started := 0.0
				if o.started.Valid {
					started = o.started.Float64
				}
				windowRows, err := c.QueryContext(ctx, orphanContiguityDonorsSQL, o.id, nullTextArg(o.source), nullTextArg(o.userID),
					nullTextArg(o.userID), started-gap, started+gap, nullFloatArg(o.last), owner, owner, orphanOwner)
				donors, err := scanDonors(windowRows, err, true)
				if err != nil {
					return err
				}
				switch {
				case len(donors) == 0:
					reason = fmt.Sprintf("no keyed predecessor fell quiet within %.0fs of this session's start", gap)
				case len(donors) > 1:
					reason = "ambiguous: more than one keyed predecessor matches this window"
				default:
					donor = &donors[0]
				}
			}
			r := OrphanRecord{OrphanID: o.id, Source: nullTextOf(o.source), MessageCount: o.count, StartedAt: nullFloatOf(o.started),
				LastActive: nullFloatOf(o.last), Adoptable: donor != nil, Reason: reason}
			if donor != nil {
				r.DonorID = sql.Null[string]{V: donor[0], Valid: true}
				r.SessionKey = sql.Null[string]{V: donor[1], Valid: true}
				r.Evidence = evidence
			}
			records = append(records, r)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	claims := map[string]int{}
	for _, r := range records {
		if r.DonorID.Valid {
			claims[r.DonorID.V]++
		}
	}
	contested := map[string]bool{}
	for _, r := range records {
		if r.Adoptable && claims[r.DonorID.V] > 1 {
			contested[r.DonorID.V] = true
		}
	}
	for i := range records {
		if records[i].DonorID.Valid && contested[records[i].DonorID.V] {
			records[i].Adoptable = false
			records[i].Reason = "ambiguous: more than one unkeyed session claims this predecessor"
		}
	}
	return records, nil
}

func nullTextArg(v sql.NullString) any {
	if !v.Valid {
		return nil
	}
	return v.String
}

func nullFloatArg(v sql.NullFloat64) any {
	if !v.Valid {
		return nil
	}
	return v.Float64
}

func (s *Store) AdoptOrphanedGatewaySession(ctx context.Context, orphanID, donorID string) (bool, error) {
	if orphanID == "" || donorID == "" || orphanID == donorID {
		return false, nil
	}
	owner := s.OwnProfileName()
	adopted := false
	err := s.Write(ctx, WritePatience, func(c Conn) error {
		adopted = false
		var key, chatID, chatType, threadID, userID, originJSON, displayName, source, profile sql.NullString
		err := c.QueryRowContext(ctx, "SELECT session_key, chat_id, chat_type, thread_id, user_id, "+
			"origin_json, display_name, source, profile_name FROM sessions WHERE id = ?", donorID).
			Scan(&key, &chatID, &chatType, &threadID, &userID, &originJSON, &displayName, &source, &profile)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		var orphanKey, orphanSource, orphanProfile sql.NullString
		err = c.QueryRowContext(ctx, "SELECT session_key, source, profile_name FROM sessions WHERE id = ?", orphanID).
			Scan(&orphanKey, &orphanSource, &orphanProfile)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		effective := func(p sql.NullString) string {
			if p.String != "" {
				return p.String
			}
			return owner
		}
		if key.String == "" || orphanKey.String != "" || source.String != orphanSource.String ||
			(owner != "" && effective(profile) != effective(orphanProfile)) {
			return nil
		}
		if _, err := c.ExecContext(ctx, `UPDATE sessions
                      SET session_key = ?,
                          chat_id = COALESCE(chat_id, ?),
                          chat_type = COALESCE(chat_type, ?),
                          thread_id = COALESCE(thread_id, ?),
                          user_id = COALESCE(user_id, ?),
                          origin_json = COALESCE(origin_json, ?),
                          display_name = COALESCE(display_name, ?),
                          parent_session_id = COALESCE(parent_session_id, ?)
                    WHERE id = ? AND session_key IS NULL`, key.String, nullTextArg(chatID), nullTextArg(chatType), nullTextArg(threadID),
			nullTextArg(userID), nullTextArg(originJSON), nullTextArg(displayName), donorID, orphanID); err != nil {
			return err
		}
		if _, err := c.ExecContext(ctx, "UPDATE sessions SET ended_at = COALESCE(ended_at, ?), "+
			"end_reason = 'superseded_by_repair' WHERE id = ?", nowEpoch(), donorID); err != nil {
			return err
		}
		adopted = true
		return nil
	})
	return adopted, err
}

func (s *Store) IncrementHygieneFailureStreak(ctx context.Context, sessionKey string) (int64, error) {
	if sessionKey == "" {
		return 1, nil
	}
	var streak int64
	err := s.Write(ctx, WritePatience, func(c Conn) error {
		if _, err := c.ExecContext(ctx, `INSERT INTO gateway_hygiene_state (session_key, failure_streak)
                   VALUES (?, 1)
                   ON CONFLICT(session_key) DO UPDATE SET
                       failure_streak = gateway_hygiene_state.failure_streak + 1`, sessionKey); err != nil {
			return err
		}
		return c.QueryRowContext(ctx, "SELECT failure_streak FROM gateway_hygiene_state WHERE session_key = ?", sessionKey).Scan(&streak)
	})
	return streak, err
}

func (s *Store) ResetHygieneFailureStreak(ctx context.Context, sessionKey string) error {
	if sessionKey == "" {
		return nil
	}
	return s.WriteSQL(ctx, WritePatience, "DELETE FROM gateway_hygiene_state WHERE session_key = ?", sessionKey)
}

type ProfileCollisionError struct{ msg string }

func (e *ProfileCollisionError) Error() string { return e.msg }

func spacedUnicodeJSON(members []configMember) (string, error) {
	enc, err := fileutil.EncodeJSON(rawObject(members), fileutil.JSONStyle{})
	return string(enc), err
}

func execCount(ctx context.Context, c Conn, counts map[string]int64, name, query string, args ...any) error {
	n, err := rowsChanged(c.ExecContext(ctx, query, args...))
	if err == nil {
		counts[name] = n
	}
	return err
}

func (s *Store) RekeyProfileState(ctx context.Context, oldName, newName string) (map[string]int64, error) {
	old, renamed := strings.TrimSpace(oldName), strings.TrimSpace(newName)
	counts := map[string]int64{}
	if old == "" || renamed == "" || old == renamed {
		return counts, nil
	}
	oldNS, newNS := "agent:"+old+":", "agent:"+renamed+":"
	nsLen := len([]rune(oldNS))
	err := s.Write(ctx, WritePatience, func(c Conn) error {
		clear(counts)
		existing, err := queryStrings(ctx, c, "SELECT name FROM sqlite_master WHERE type='table'")
		if err != nil {
			return err
		}
		topic, err := optionalTableColumns(ctx, c)
		if err != nil {
			return err
		}
		var scope, target string
		err = c.QueryRowContext(ctx, "SELECT old.scope, ? || substr(old.session_key, ?) "+
			"FROM gateway_routing AS old JOIN gateway_routing AS target "+
			"ON target.scope = old.scope "+
			"AND target.session_key = ? || substr(old.session_key, ?) "+
			"WHERE substr(old.session_key, 1, ?) = ? LIMIT 1", newNS, nsLen+1, newNS, nsLen+1, nsLen, oldNS).Scan(&scope, &target)
		if err == nil {
			return &ProfileCollisionError{msg: "profile routing collision in scope " + quotedText(scope) + ": " + quotedText(target)}
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		for _, t := range []struct {
			table   string
			columns []string
		}{{"telegram_dm_topic_mode", []string{"chat_id"}}, {"telegram_dm_topic_bindings", []string{"chat_id", "thread_id"}}} {
			if !containsString(topic[t.table], "profile_name") {
				continue
			}
			var parts []string
			for _, col := range t.columns {
				parts = append(parts, "target."+col+" = old."+col)
			}
			hit, err := queryStrings(ctx, c, "SELECT 1 FROM "+t.table+" AS old JOIN "+t.table+" AS target "+
				"ON target.profile_name = ? AND "+strings.Join(parts, " AND ")+" "+
				"WHERE old.profile_name = ? LIMIT 1", renamed, old)
			if err != nil {
				return err
			}
			if len(hit) > 0 {
				return &ProfileCollisionError{msg: "profile identity collision in " + t.table}
			}
		}
		if err := execCount(ctx, c, counts, "sessions_profile_name", "UPDATE sessions SET profile_name = ? WHERE profile_name = ?", renamed, old); err != nil {
			return err
		}
		if err := execCount(ctx, c, counts, "gateway_heartbeats_profile", "UPDATE gateway_heartbeats SET profile = ? WHERE profile = ?", renamed, old); err != nil {
			return err
		}
		if err := execCount(ctx, c, counts, "sessions_session_key", "UPDATE sessions SET session_key = ? || substr(session_key, ?) "+
			"WHERE substr(session_key, 1, ?) = ?", newNS, nsLen+1, nsLen, oldNS); err != nil {
			return err
		}
		origins, err := c.QueryContext(ctx, "SELECT id, origin_json FROM sessions WHERE origin_json IS NOT NULL")
		if err != nil {
			return err
		}
		type pending struct{ id, text string }
		var rewrites []pending
		for origins.Next() {
			var id string
			var origin any
			if err := origins.Scan(&id, &origin); err != nil {
				origins.Close()
				return err
			}
			text, ok := origin.(string)
			if !ok {
				continue
			}
			members, isObject := objectMembers(json.RawMessage(text))
			profile, _ := memberValue(members, "profile")
			if !isObject || !jsonIsString(profile) || jsonString(profile) != old {
				continue
			}
			encodedName, _ := json.Marshal(renamed)
			updated, err := spacedUnicodeJSON(setConfigMember(members, "profile", encodedName))
			if err != nil {
				origins.Close()
				return err
			}
			rewrites = append(rewrites, pending{id, updated})
		}
		if err := origins.Close(); err != nil {
			return err
		}
		for _, r := range rewrites {
			if _, err := c.ExecContext(ctx, "UPDATE sessions SET origin_json = ? WHERE id = ?", r.text, r.id); err != nil {
				return err
			}
		}
		counts["sessions_origin_json"] = int64(len(rewrites))
		if containsString(existing, "delivery_obligations") {
			if containsString(topic["delivery_obligations"], "adapter_profile") {
				if err := execCount(ctx, c, counts, "delivery_obligations_adapter_profile",
					"UPDATE delivery_obligations SET adapter_profile = ? WHERE adapter_profile = ?", renamed, old); err != nil {
					return err
				}
			}
			if err := execCount(ctx, c, counts, "delivery_obligations_session_key", "UPDATE delivery_obligations SET session_key = ? || substr(session_key, ?) "+
				"WHERE substr(session_key, 1, ?) = ?", newNS, nsLen+1, nsLen, oldNS); err != nil {
				return err
			}
		}
		for _, table := range []string{"telegram_dm_topic_mode", "telegram_dm_topic_bindings"} {
			if containsString(topic[table], "profile_name") {
				if err := execCount(ctx, c, counts, table+"_profile_name", "UPDATE "+table+" SET profile_name = ? WHERE profile_name = ?", renamed, old); err != nil {
					return err
				}
			}
		}
		if containsString(topic["telegram_dm_topic_bindings"], "session_key") {
			if err := execCount(ctx, c, counts, "telegram_dm_topic_bindings_session_key", "UPDATE telegram_dm_topic_bindings "+
				"SET session_key = ? || substr(session_key, ?) "+
				"WHERE substr(session_key, 1, ?) = ?", newNS, nsLen+1, nsLen, oldNS); err != nil {
				return err
			}
		}
		routing, err := c.QueryContext(ctx, "SELECT rowid, session_key, entry_json FROM gateway_routing "+
			"WHERE substr(session_key, 1, ?) = ?", nsLen, oldNS)
		if err != nil {
			return err
		}
		type route struct {
			rowid int64
			key   string
			entry any
		}
		var routes []route
		for routing.Next() {
			var r route
			if err := routing.Scan(&r.rowid, &r.key, &r.entry); err != nil {
				routing.Close()
				return err
			}
			routes = append(routes, r)
		}
		if err := routing.Close(); err != nil {
			return err
		}
		for _, r := range routes {
			newKey := newNS + string([]rune(r.key)[nsLen:])
			newEntry := r.entry
			if text, ok := r.entry.(string); ok && text != "" {
				if payload, isObject := objectMembers(json.RawMessage(text)); isObject {
					if v, has := memberValue(payload, "session_key"); has && jsonIsString(v) && strings.HasPrefix(jsonString(v), oldNS) {
						rekeyed, _ := json.Marshal(newNS + string([]rune(jsonString(v))[nsLen:]))
						payload = setConfigMember(payload, "session_key", rekeyed)
					}
					if originRaw, has := memberValue(payload, "origin"); has {
						if origin, isOrigin := objectMembers(originRaw); isOrigin {
							if p, hasProfile := memberValue(origin, "profile"); hasProfile && jsonIsString(p) && jsonString(p) == old {
								encodedName, _ := json.Marshal(renamed)
								originText, err := spacedUnicodeJSON(setConfigMember(origin, "profile", encodedName))
								if err != nil {
									return err
								}
								payload = setConfigMember(payload, "origin", json.RawMessage(originText))
							}
						}
					}
					updated, err := spacedUnicodeJSON(payload)
					if err != nil {
						return err
					}
					newEntry = updated
				}
			}
			if _, err := c.ExecContext(ctx, "UPDATE gateway_routing SET session_key = ?, entry_json = ? WHERE rowid = ?", newKey, newEntry, r.rowid); err != nil {
				return err
			}
		}
		counts["gateway_routing"] = int64(len(routes))
		return nil
	})
	return counts, err
}

func (s *Store) PurgeProfileState(ctx context.Context, profile string) (map[string]int64, error) {
	name := strings.TrimSpace(profile)
	counts := map[string]int64{}
	if name == "" {
		return counts, nil
	}
	ns := "agent:" + name + ":"
	nsLen := len([]rune(ns))
	err := s.Write(ctx, WritePatience, func(c Conn) error {
		clear(counts)
		existing, err := queryStrings(ctx, c, "SELECT name FROM sqlite_master WHERE type='table'")
		if err != nil {
			return err
		}
		topic, err := optionalTableColumns(ctx, c)
		if err != nil {
			return err
		}
		if containsString(existing, "gateway_routing") {
			if err := execCount(ctx, c, counts, "gateway_routing", "DELETE FROM gateway_routing WHERE substr(session_key, 1, ?) = ?", nsLen, ns); err != nil {
				return err
			}
		}
		if containsString(existing, "gateway_heartbeats") {
			if err := execCount(ctx, c, counts, "gateway_heartbeats", "DELETE FROM gateway_heartbeats WHERE profile = ?", name); err != nil {
				return err
			}
		}
		if containsString(existing, "delivery_obligations") {
			byProfile, args := "", []any{nowEpoch(), nsLen, ns}
			if containsString(topic["delivery_obligations"], "adapter_profile") {
				byProfile, args = "adapter_profile = ? OR ", []any{nowEpoch(), name, nsLen, ns}
			}
			if err := execCount(ctx, c, counts, "delivery_obligations", "UPDATE delivery_obligations SET state='abandoned', updated_at=? "+
				"WHERE ("+byProfile+"substr(session_key, 1, ?) = ?) "+
				"AND state NOT IN ('delivered', 'abandoned')", args...); err != nil {
				return err
			}
		}
		if containsString(topic["telegram_dm_topic_mode"], "profile_name") {
			if err := execCount(ctx, c, counts, "telegram_dm_topic_mode", "DELETE FROM telegram_dm_topic_mode WHERE profile_name = ?", name); err != nil {
				return err
			}
		}
		bindings := topic["telegram_dm_topic_bindings"]
		if containsString(bindings, "session_key") {
			byProfile, args := "", []any{nsLen, ns}
			if containsString(bindings, "profile_name") {
				byProfile, args = "profile_name = ? OR ", []any{name, nsLen, ns}
			}
			if err := execCount(ctx, c, counts, "telegram_dm_topic_bindings", "DELETE FROM telegram_dm_topic_bindings "+
				"WHERE "+byProfile+"substr(session_key, 1, ?) = ?", args...); err != nil {
				return err
			}
		}
		return nil
	})
	return counts, err
}

func SessionGatewayRuntime(modelConfig sql.Null[string], billingProvider, billingBaseURL sql.Null[string]) json.RawMessage {
	cfg, _ := objectMembers(json.RawMessage(modelConfig.V))
	withoutNulls := func(members []configMember) json.RawMessage {
		var out []configMember
		for _, m := range members {
			if !jsonIsNull(m.value) {
				out = append(out, m)
			}
		}
		return rawObject(out)
	}
	runtimeRaw, hasRuntime := memberValue(cfg, "gateway_runtime")
	runtime, runtimeIsObject := objectMembers(runtimeRaw)
	if provider, has := memberValue(runtime, "provider"); runtimeIsObject && has && jsonTruthy(provider) {
		return withoutNulls(runtime)
	}
	var top []configMember
	for _, key := range []string{"provider", "base_url", "api_mode"} {
		if v, has := memberValue(cfg, key); has && jsonTruthy(v) {
			top = append(top, configMember{key, v})
		}
	}
	billing := strings.TrimSpace(billingProvider.V)
	_, hasProvider := memberValue(top, "provider")
	if billing != "" && !containsString(bareBillingProviders, strings.ToLower(billing)) {
		if !hasProvider {
			encoded, _ := json.Marshal(billing)
			top = append(top, configMember{"provider", encoded})
		}
	} else if strings.ToLower(billing) == "custom" && !hasProvider {
		if base := strings.TrimSpace(billingBaseURL.V); base != "" {
			if _, hasBase := memberValue(top, "base_url"); !hasBase {
				encoded, _ := json.Marshal(base)
				top = append(top, configMember{"base_url", encoded})
			}
			top = setConfigMember(top, "provider", json.RawMessage(`"custom"`))
		}
	}
	if len(top) > 0 {
		return rawObject(top)
	}
	if !hasRuntime || !runtimeIsObject {
		return json.RawMessage("{}")
	}
	return withoutNulls(runtime)
}

type BackendHeartbeat struct {
	BackendID     string
	PID           int64
	StartedAt     float64
	LastHeartbeat float64
	Profile       string
	Host          string
}

func (s *Store) RegisterBackendHeartbeat(ctx context.Context, h BackendHeartbeat, lastHeartbeat *float64) error {
	if h.BackendID == "" {
		return nil
	}
	ts := nowEpoch()
	if lastHeartbeat != nil {
		ts = *lastHeartbeat
	}
	return s.WriteSQL(ctx, WritePatience, "INSERT INTO gateway_heartbeats (backend_id, pid, started_at, last_heartbeat, profile, host)"+
		" VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT(backend_id) DO UPDATE SET pid = excluded.pid,"+
		" started_at = excluded.started_at, last_heartbeat = excluded.last_heartbeat,"+
		" profile = excluded.profile, host = excluded.host", h.BackendID, h.PID, h.StartedAt, ts, h.Profile, h.Host)
}

func (s *Store) ClearBackendHeartbeat(ctx context.Context, backendID string) (bool, error) {
	if backendID == "" {
		return false, nil
	}
	n, err := s.WriteRowcount(ctx, WritePatience, "DELETE FROM gateway_heartbeats WHERE backend_id = ?", backendID)
	return n > 0, err
}

func (s *Store) PruneStaleHeartbeats(ctx context.Context, maxAgeSeconds float64) ([]string, error) {
	if maxAgeSeconds <= 0 {
		return nil, nil
	}
	cutoff := nowEpoch() - maxAgeSeconds
	var removed []string
	err := s.Write(ctx, WritePatience, func(c Conn) error {
		var err error
		removed, err = queryStrings(ctx, c, "DELETE FROM gateway_heartbeats WHERE last_heartbeat < ? RETURNING backend_id", cutoff)
		return err
	})
	return removed, err
}

func (s *Store) ListBackendHeartbeats(ctx context.Context) ([]BackendHeartbeat, error) {
	var out []BackendHeartbeat
	err := s.Read(ctx, func(c Conn) error {
		out = nil
		rows, err := c.QueryContext(ctx, "SELECT backend_id, pid, started_at, last_heartbeat, profile, host FROM gateway_heartbeats"+
			" ORDER BY last_heartbeat DESC")
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var h BackendHeartbeat
			var profile, host sql.NullString
			if err := rows.Scan(&h.BackendID, &h.PID, &h.StartedAt, &h.LastHeartbeat, &profile, &host); err != nil {
				return err
			}
			h.Profile, h.Host = profile.String, host.String
			out = append(out, h)
		}
		return rows.Err()
	})
	return out, err
}

func (s *Store) RequestHandoff(ctx context.Context, sessionID, platform string) (bool, error) {
	n, err := s.WriteRowcount(ctx, WritePatience, "UPDATE sessions SET handoff_state = 'pending',     handoff_platform = ?, "+
		"    handoff_error = NULL WHERE id = ? AND (handoff_state IS NULL "+
		"                  OR handoff_state IN ('completed', 'failed'))", platform, sessionID)
	return n > 0, err
}

type HandoffState struct {
	State    sql.Null[string]
	Platform sql.Null[string]
	Error    sql.Null[string]
}

func (s *Store) GetHandoffState(ctx context.Context, sessionID string) (HandoffState, bool, error) {
	var h HandoffState
	found := false
	err := s.Read(ctx, func(c Conn) error {
		var state, platform, failure sql.NullString
		err := c.QueryRowContext(ctx, "SELECT handoff_state, handoff_platform, handoff_error FROM sessions WHERE id = ?", sessionID).
			Scan(&state, &platform, &failure)
		if errors.Is(err, sql.ErrNoRows) {
			found = false
			return nil
		}
		if err != nil {
			return err
		}
		h, found = HandoffState{nullTextOf(state), nullTextOf(platform), nullTextOf(failure)}, true
		return nil
	})
	return h, found, err
}

func (s *Store) ListPendingHandoffs(ctx context.Context) ([]Session, error) {
	var out []Session
	err := s.Read(ctx, func(c Conn) error {
		var err error
		out, err = querySessions(c.QueryContext(ctx, "SELECT s.*, COALESCE(sp.prompt, s.system_prompt) AS _system_prompt_resolved FROM sessions s "+
			"LEFT JOIN system_prompts sp ON sp.hash = s.system_prompt_hash "+
			"WHERE s.handoff_state = 'pending' "+
			"ORDER BY s.started_at ASC"))
		return err
	})
	return out, err
}

func (s *Store) ClaimHandoff(ctx context.Context, sessionID string) (bool, error) {
	n, err := s.WriteRowcount(ctx, WritePatience, "UPDATE sessions SET handoff_state = 'running' WHERE id = ? AND handoff_state = 'pending'", sessionID)
	return n > 0, err
}

func (s *Store) HasPendingHandoffs(ctx context.Context) (bool, error) {
	found := false
	err := s.Read(ctx, func(c Conn) error {
		hits, err := queryStrings(ctx, c, "SELECT 1 FROM sessions WHERE handoff_state = 'pending' LIMIT 1")
		found = len(hits) > 0
		return err
	})
	return found, err
}

func (s *Store) CompleteHandoff(ctx context.Context, sessionID string) error {
	return s.WriteSQL(ctx, WritePatience, "UPDATE sessions SET handoff_state = 'completed', handoff_error = NULL WHERE id = ?", sessionID)
}

func clipRunes(text string, limit int) string {
	r := []rune(text)
	if len(r) > limit {
		return string(r[:limit])
	}
	return text
}

func (s *Store) FailHandoff(ctx context.Context, sessionID, failure string, onlyStates ...string) (bool, error) {
	query := handoffFailSQL + "id = ?"
	args := []any{clipRunes(failure, 500), sessionID}
	if len(onlyStates) > 0 {
		query += " AND handoff_state IN (" + strings.Join(strings.Split(strings.Repeat("?", len(onlyStates)), ""), ", ") + ")"
		args = append(args, stringArgs(onlyStates)...)
	}
	n, err := s.WriteRowcount(ctx, WritePatience, query, args...)
	return n > 0, err
}

func (s *Store) ReclaimStaleRunningHandoffs(ctx context.Context, failure string) []string {
	var ids []string
	err := s.Write(ctx, WritePatience, func(c Conn) error {
		var err error
		if ids, err = queryStrings(ctx, c, "SELECT id FROM sessions WHERE handoff_state = 'running'"); err != nil || len(ids) == 0 {
			return err
		}
		_, err = c.ExecContext(ctx, handoffFailSQL+"handoff_state = 'running'", clipRunes(failure, 500))
		return err
	})
	if err != nil {
		slog.Warn("reclaim_stale_running_handoffs failed; stranded 'running' handoff rows (if any) were left in place", "error", errText(err))
		return nil
	}
	return ids
}
