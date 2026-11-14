package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
)

var syntheticPrompt = regexp.MustCompile(`(?i)^\s*(?:\[IMPORTANT: Background process |\[ASYNC (?:DELEGATION )?(?:BATCH )?COMPLETE\b|` +
	`A background fan-out of \d+ subagent\(s\) you dispatched earlier has finished\.|` +
	`A background subagent you dispatched earlier has finished\.)`)

func promptPreview(cell any, displayKind string, summary bool) string {
	c := decodeContent(cell)
	if summary || isSummaryContent(c) {
		stripped, ok := stripSummaryHandoff(c)
		if !ok {
			return ""
		}
		c, displayKind, summary = stripped, "", false
	}
	if split := splitUserOriginatedTurn(userTurn{content: c, displayKind: displayKind, summary: summary}); !split.live {
		return ""
	}
	text, isText := c.Text()
	if parts, isList := contentParts(c); isList {
		chunks := make([]string, 0, len(parts))
		for _, p := range parts {
			if p.isText || p.isObj {
				t, _ := p.partText()
				chunks = append(chunks, t)
			}
		}
		text, isText = strings.Join(chunks, " "), true
	}
	if !isText {
		return ""
	}
	text = strings.Join(previewFields(text), " ")
	if text == "" || syntheticPrompt.MatchString(text) {
		return ""
	}
	runes := []rune(text)
	if len(runes) <= 120 {
		return text
	}
	return strings.TrimRightFunc(string(runes[:119]), previewSpace) + "…"
}

func identityContent(cell any, displayKind string) (any, error) {
	split := splitUserOriginatedTurn(userTurn{content: decodeContent(cell), displayKind: displayKind})
	if split.handoff && split.live {
		return split.liveContent.encode()
	}
	return cell, nil
}

func snapshotRead(ctx context.Context, c Conn, fn func() error) (err error) {
	if _, err := c.ExecContext(ctx, "BEGIN"); err != nil {
		return err
	}
	defer func() {
		if _, rerr := c.ExecContext(context.WithoutCancel(ctx), "ROLLBACK"); err == nil {
			err = rerr
		}
	}()
	return fn()
}

func displayRowsSQL(ctx context.Context, c Conn, sessionID string, usersOnly bool) (string, []any, error) {
	filters := displayVisibleSQL
	if usersOnly {
		filters = " AND role = 'user'" + displayVisibleSQL
	}
	unindexed, err := queryStrings(ctx, c, "SELECT 1 FROM messages WHERE session_id = ? AND (active = 1 OR compacted = 1)"+
		filters+" AND (display_order IS NULL OR display_identity IS NULL) LIMIT 1", sessionID)
	if err != nil {
		return "", nil, err
	}
	if len(unindexed) == 0 {
		return `WITH display_rows AS (
            SELECT (SELECT candidate.id FROM messages candidate
                    WHERE candidate.session_id = :sid
                      AND candidate.display_order = m.display_order
                      AND (candidate.active = 1 OR candidate.compacted = 1)` + strings.ReplaceAll(displayVisibleSQL, "display_metadata", "candidate.display_metadata") +
			notALaterToolCopySQL("candidate") + `
                    ORDER BY candidate.active DESC, candidate.id DESC LIMIT 1) AS row_id,
                   m.display_order AS sort_id
            FROM messages m WHERE session_id = :sid AND (active = 1 OR compacted = 1)` + filters + notALaterToolCopySQL("m") + `
            GROUP BY m.display_order
        )`, []any{sql.Named("sid", sessionID)}, nil
	}
	rows, err := c.QueryContext(ctx, "SELECT id, content, display_kind FROM messages WHERE session_id = ? AND (active = 1 OR compacted = 1)"+
		" AND role = 'user'"+displayVisibleSQL, sessionID)
	if err != nil {
		return "", nil, err
	}
	var identities []configMember
	for rows.Next() {
		var id int64
		var content any
		var kind sql.NullString
		if err := rows.Scan(&id, &content, &kind); err != nil {
			rows.Close()
			return "", nil, err
		}
		identity, err := identityContent(content, kind.String)
		if err != nil {
			rows.Close()
			return "", nil, err
		}
		value, err := cellJSON(identity)
		if err != nil {
			rows.Close()
			return "", nil, err
		}
		identities = append(identities, configMember{strconv.FormatInt(id, 10), value})
	}
	if err := rowsDone(rows); err != nil {
		return "", nil, err
	}
	return `WITH ranked AS (
        SELECT id, MIN(id) OVER identity AS sort_id,
               ROW_NUMBER() OVER (identity ORDER BY active DESC, id DESC) AS preference
        FROM messages WHERE session_id = :sid AND (active = 1 OR compacted = 1)` + filters + `
        WINDOW identity AS (PARTITION BY role,
            CASE WHEN role = 'user' THEN json_extract(:identities, '$."' || id || '"') ELSE content END,
            timestamp, tool_call_id, tool_calls, tool_name)
    ), display_rows AS (SELECT id AS row_id, sort_id FROM ranked WHERE preference = 1)`,
		[]any{sql.Named("sid", sessionID), sql.Named("identities", string(rawObject(identities)))}, nil
}

type MessagesAround struct {
	Messages []json.RawMessage
	RowID    int64
	Limit    int
	Offset   int64
	Total    int64
	HasOlder bool
	HasNewer bool
}

func (s *Store) GetSessionMessagesAround(ctx context.Context, sessionID string, rowID int64, limit int) (*MessagesAround, error) {
	var out *MessagesAround
	err := s.Read(ctx, func(c Conn) error {
		out = nil
		return snapshotRead(ctx, c, func() error {
			var content any
			var kind sql.NullString
			var summary sql.Null[int64]
			err := c.QueryRowContext(ctx, "SELECT content, display_kind, _compressed_summary FROM messages "+
				"WHERE session_id = ? AND id = ? AND role = 'user' AND (active = 1 OR compacted = 1)", sessionID, rowID).Scan(&content, &kind, &summary)
			if err == sql.ErrNoRows {
				return nil
			}
			if err != nil || promptPreview(content, kind.String, summary.V != 0) == "" {
				return err
			}
			cte, args, err := displayRowsSQL(ctx, c, sessionID, false)
			if err != nil {
				return err
			}
			args = append(args, sql.Named("row_id", rowID), sql.Named("limit", limit))
			var start int64
			err = c.QueryRowContext(ctx, cte+"\n            SELECT sort_id FROM display_rows WHERE row_id = :row_id", args...).Scan(&start)
			if err == sql.ErrNoRows {
				return nil
			}
			if err != nil {
				return err
			}
			args = append(args, sql.Named("start", start))
			page := &MessagesAround{RowID: rowID, Limit: limit}
			if err := c.QueryRowContext(ctx, cte+"\n            SELECT COUNT(*) AS total, COALESCE(SUM(sort_id < :start), 0) AS offset FROM display_rows",
				args...).Scan(&page.Total, &page.Offset); err != nil {
				return err
			}
			rows, err := queryMsgRows(c.QueryContext(ctx, cte+`
            SELECT m.* FROM (SELECT row_id, sort_id FROM display_rows
                            WHERE sort_id >= :start ORDER BY sort_id LIMIT :limit) AS page
            JOIN messages m ON m.id = page.row_id ORDER BY page.sort_id`, args...))
			if err != nil {
				return err
			}
			for _, r := range rows {
				members, err := messageRowObject(r, "timeline jump")
				if err != nil {
					return err
				}
				page.Messages = append(page.Messages, rawObject(members))
			}
			page.HasOlder = page.Offset > 0
			page.HasNewer = page.Offset+int64(len(page.Messages)) < page.Total
			out = page
			return nil
		})
	})
	return out, err
}

type TimelineEntry struct {
	RowID     int64
	Preview   string
	Timestamp sql.Null[float64]
}

type Timeline struct {
	Entries    []TimelineEntry
	Limit      int
	AfterRowID int64
	Total      int
	HasMore    bool
	NextCursor sql.Null[int64]
}

func (s *Store) GetSessionTimeline(ctx context.Context, sessionID string, limit int, afterRowID int64) (Timeline, error) {
	out := Timeline{Limit: limit, AfterRowID: afterRowID}
	err := s.Read(ctx, func(c Conn) error {
		out = Timeline{Limit: limit, AfterRowID: afterRowID}
		return snapshotRead(ctx, c, func() error {
			cte, args, err := displayRowsSQL(ctx, c, sessionID, true)
			if err != nil {
				return err
			}
			rows, err := c.QueryContext(ctx, cte+`
            SELECT row_id, sort_id, m.timestamp, m.content, m.display_kind, m._compressed_summary
            FROM display_rows JOIN messages m ON m.id = row_id ORDER BY sort_id`, args...)
			if err != nil {
				return err
			}
			type eligible struct {
				entry TimelineEntry
				sort  int64
			}
			var all []eligible
			for rows.Next() {
				var e eligible
				var content any
				var kind sql.NullString
				var summary sql.Null[int64]
				if err := rows.Scan(&e.entry.RowID, &e.sort, &e.entry.Timestamp, &content, &kind, &summary); err != nil {
					rows.Close()
					return err
				}
				if e.entry.Preview = promptPreview(content, kind.String, summary.V != 0); e.entry.Preview != "" {
					all = append(all, e)
				}
			}
			if err := rowsDone(rows); err != nil {
				return err
			}
			out.Total = len(all)
			var page []eligible
			for _, e := range all {
				if e.sort > afterRowID && len(page) < limit+1 {
					page = append(page, e)
				}
			}
			out.HasMore = len(page) > limit
			if out.HasMore {
				page = page[:limit]
				out.NextCursor = sql.Null[int64]{V: page[len(page)-1].sort, Valid: true}
			}
			for _, e := range page {
				out.Entries = append(out.Entries, e.entry)
			}
			return nil
		})
	})
	return out, err
}
