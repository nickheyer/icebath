package state

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pysqlite"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Prompts the agent loop synthesizes itself, never shown as human timeline entries
var syntheticPromptRE = regexp.MustCompile(`(?i)^\s*(?:\[IMPORTANT: Background process |` +
	`\[ASYNC (?:DELEGATION )?(?:BATCH )?COMPLETE(?:$|[^\p{L}\p{N}_])|` +
	`A background fan-out of \p{Nd}+ subagent\(s\) you dispatched earlier has finished\.|` +
	`A background subagent you dispatched earlier has finished\.)`)

// Python type name of a content part for the join TypeError
func joinTypeName(v any) string {
	switch v.(type) {
	case string:
		return "str"
	case []any:
		return "list"
	case []byte:
		return "bytes"
	}
	if isMapping(v) {
		return "dict"
	}
	return pyTypeName(v)
}

// Whitespace collapsed human prompt text of a user row, empty for hidden, synthetic or non human rows
func promptPreview(content, displayKind, summary any) (string, error) {
	message := ProjectCompactionMessageForDisplay(map[string]any{
		"role": "user", "content": decodeMessageContent(content),
		"display_kind": displayKind, CompressedSummaryMetadataKey: pyTruthy(summary),
	})
	if message == nil || UserOriginatedTurnView(message) == nil {
		return "", nil
	}
	body := message["content"]
	if parts, ok := body.([]any); ok {
		texts := []string{}
		for _, part := range parts {
			if s, isStr := part.(string); isStr {
				texts = append(texts, s)
				continue
			}
			if !isMapping(part) {
				continue
			}
			text, has := mappingGet(part, "text")
			if !has {
				text = ""
			}
			s, isStr := text.(string)
			if !isStr {
				return "", &pyConvError{"TypeError", fmt.Sprintf("sequence item %d: expected str instance, %s found", len(texts), joinTypeName(text))}
			}
			texts = append(texts, s)
		}
		body = strings.Join(texts, " ")
	}
	raw, ok := body.(string)
	if !ok {
		return "", nil
	}
	text := strings.Join(pystr.SplitWhitespace(raw, -1), " ")
	if text == "" || syntheticPromptRE.MatchString(text) {
		return "", nil
	}
	if pystr.Len(text) <= 120 {
		return text, nil
	}
	return pystr.Rstrip(pystr.Slice(text, 0, 119)) + "…", nil
}

// Runs fn inside one read transaction so a count and its page see the same compaction generation
func (d *SessionDB) timelineSnapshot(ctx context.Context, fn func(conn SQLiteConn) error) error {
	return d.readCtx(ctx, true, func(conn SQLiteConn) error {
		if _, err := conn.ExecContext(ctx, "BEGIN"); err != nil {
			return err
		}
		ferr := fn(conn)
		if rerr := endTransaction(context.WithoutCancel(ctx), conn, "ROLLBACK"); rerr != nil && ferr == nil {
			return rerr
		}
		return ferr
	})
}

// CTE of representative display row ids with their stable first row order, bodies never selected
func displayRowsSQL(ctx context.Context, conn SQLiteConn, sessionID string, usersOnly bool) (string, error) {
	filters := DisplayVisibleSQL
	if usersOnly {
		filters = " AND role = 'user'" + DisplayVisibleSQL
	}
	unindexed, err := fetchOne(ctx, conn, "SELECT 1 FROM messages WHERE session_id = ? AND (active = 1 OR compacted = 1) "+
		filters+" AND (display_order IS NULL OR display_identity IS NULL) LIMIT 1", sessionID)
	if err != nil {
		return "", err
	}
	if unindexed == nil {
		return `WITH display_rows AS (
            SELECT (SELECT candidate.id FROM messages candidate
                    WHERE candidate.session_id = :sid
                      AND candidate.display_order = m.display_order
                      AND (candidate.active = 1 OR candidate.compacted = 1)` + DisplayVisibleSQL + notALaterToolCopySQL("candidate") + `
                    ORDER BY candidate.active DESC, candidate.id DESC LIMIT 1) AS row_id,
                   m.display_order AS sort_id
            FROM messages m WHERE session_id = :sid AND (active = 1 OR compacted = 1)` + filters + notALaterToolCopySQL("m") + `
            GROUP BY m.display_order
        )`, nil
	}
	return `WITH ranked AS (
        SELECT id, MIN(id) OVER identity AS sort_id,
               ROW_NUMBER() OVER (identity ORDER BY active DESC, id DESC) AS preference
        FROM messages WHERE session_id = :sid AND (active = 1 OR compacted = 1)` + filters + `
        WINDOW identity AS (PARTITION BY role,
            CASE WHEN role = 'user' THEN timeline_identity_content(content, display_kind) ELSE content END,
            timestamp, tool_call_id, tool_calls, tool_name)
    ), display_rows AS (SELECT id AS row_id, sort_id FROM ranked WHERE preference = 1)`, nil
}

// Driver level connection under the tracking and lazy transaction wrappers, false when none is exposed
func rawConnOf(conn SQLiteConn) (pysqlite.RawConn, bool) {
	for {
		switch c := conn.(type) {
		case *TrackedConn:
			conn = c.SQLiteConn
		case *lazyTxConn:
			conn = c.SQLiteConn
		default:
			raw, ok := conn.(pysqlite.RawConn)
			return raw, ok
		}
	}
}

// Installs the timeline SQL functions on the connection as hermes registers them before each read
func registerTimelineFunctions(conn SQLiteConn) error {
	raw, ok := rawConnOf(conn)
	if !ok {
		return fmt.Errorf("connection %T exposes no driver connection for SQL functions", conn)
	}
	identity := func(args []any) (any, error) {
		content := args[0]
		handoff, live := SplitUserOriginatedTurn(map[string]any{
			"role": "user", "content": decodeMessageContent(content), "display_kind": args[1]})
		if handoff != nil && live != nil {
			return encodeMessageContent(live["content"]), nil
		}
		return content, nil
	}
	if err := pysqlite.CreateFunction(raw, "timeline_identity_content", 2, true, identity); err != nil {
		return err
	}
	return pysqlite.CreateFunction(raw, "timeline_preview", 3, true, func(args []any) (any, error) {
		return promptPreview(args[0], args[1], args[2])
	})
}

// Display page of at most limit rows from an exact human prompt, nil when the row is no such prompt
func (d *SessionDB) GetSessionMessagesAround(ctx context.Context, sessionID string, rowID int64, limit int) (*pyjson.Object, error) {
	var rows []*pyjson.Object
	var total, offset int64
	found := false
	err := d.timelineSnapshot(ctx, func(conn SQLiteConn) error {
		if err := registerTimelineFunctions(conn); err != nil {
			return err
		}
		anchor, err := fetchOne(ctx, conn, "SELECT content, display_kind, _compressed_summary FROM messages "+
			"WHERE session_id = ? AND id = ? AND role = 'user' AND (active = 1 OR compacted = 1)", sessionID, rowID)
		if err != nil || anchor == nil {
			return err
		}
		preview, err := promptPreview(anchor[0], anchor[1], anchor[2])
		if err != nil || preview == "" {
			return err
		}
		cte, err := displayRowsSQL(ctx, conn, sessionID, false)
		if err != nil {
			return err
		}
		sid := sql.Named("sid", sessionID)
		selected, err := fetchOne(ctx, conn, cte+`
            SELECT sort_id FROM display_rows WHERE row_id = :row_id
        `, sid, sql.Named("row_id", rowID))
		if err != nil || selected == nil {
			return err
		}
		start := sql.Named("start", selected[0])
		counts, err := fetchOne(ctx, conn, cte+`
            SELECT COUNT(*) AS total, COALESCE(SUM(sort_id < :start), 0) AS offset FROM display_rows
        `, sid, start)
		if err != nil {
			return err
		}
		total, _ = exactInt(counts[0])
		offset, _ = exactInt(counts[1])
		rows, err = queryObjects(ctx, conn, cte+`
            SELECT m.* FROM (SELECT row_id, sort_id FROM display_rows
                            WHERE sort_id >= :start ORDER BY sort_id LIMIT :limit) AS page
            JOIN messages m ON m.id = page.row_id ORDER BY page.sort_id
        `, sid, start, sql.Named("limit", int64(limit)))
		found = err == nil
		return err
	})
	if err != nil || !found {
		return nil, err
	}
	messages := make([]any, 0, len(rows))
	for _, row := range rows {
		msg, err := rowToMessageDict(row, "timeline jump", true)
		if err != nil {
			return nil, err
		}
		messages = append(messages, msg)
	}
	returned := int64(len(messages))
	return pyjson.ObjectOf("messages", messages, "pagination", pyjson.ObjectOf(
		"row_id", rowID, "limit", limit, "returned", returned, "order", "oldest",
		"offset", offset, "total", total,
		"has_older", offset > 0,
		"has_newer", offset+returned < total,
	)), nil
}

// Chronological human prompts after a cursor, the cursor being the first physical row id of a logical turn
func (d *SessionDB) GetSessionTimeline(ctx context.Context, sessionID string, limit int, afterRowID int64) (*pyjson.Object, error) {
	var rows []*pyjson.Object
	var total any
	err := d.timelineSnapshot(ctx, func(conn SQLiteConn) error {
		if err := registerTimelineFunctions(conn); err != nil {
			return err
		}
		cte, err := displayRowsSQL(ctx, conn, sessionID, true)
		if err != nil {
			return err
		}
		query := cte + `,
            prompts AS MATERIALIZED (
                SELECT row_id, sort_id, m.timestamp,
                       timeline_preview(m.content, m.display_kind, m._compressed_summary) AS preview
                FROM display_rows JOIN messages m ON m.id = row_id
            ), eligible AS MATERIALIZED (SELECT * FROM prompts WHERE preview <> '')
        `
		sid := sql.Named("sid", sessionID)
		rows, err = queryObjects(ctx, conn, query+`
            SELECT row_id, sort_id, timestamp, preview, (SELECT COUNT(*) FROM eligible) AS total
            FROM eligible WHERE sort_id > :after ORDER BY sort_id LIMIT :limit
        `, sid, sql.Named("after", afterRowID), sql.Named("limit", int64(limit+1)))
		if err != nil {
			return err
		}
		if len(rows) > 0 {
			total = rowValue(rows[0], "total")
			return nil
		}
		count, err := fetchOne(ctx, conn, query+"SELECT COUNT(*) FROM eligible", sid)
		if err != nil {
			return err
		}
		total = count[0]
		return nil
	})
	if err != nil {
		return nil, err
	}
	hasMore := len(rows) > limit
	page := rows[:min(max(limit, 0), len(rows))]
	entries := make([]any, 0, len(page))
	for _, row := range page {
		entries = append(entries, pyjson.ObjectOf("row_id", rowValue(row, "row_id"), "preview", rowValue(row, "preview"), "timestamp", rowValue(row, "timestamp")))
	}
	var nextCursor any
	if hasMore {
		if len(page) == 0 {
			return nil, &pyConvError{"IndexError", "list index out of range"}
		}
		nextCursor = rowValue(page[len(page)-1], "sort_id")
	}
	return pyjson.ObjectOf("entries", entries, "pagination", pyjson.ObjectOf(
		"limit", limit, "after_row_id", afterRowID, "returned", len(page),
		"total", total, "has_more", hasMore, "next_cursor", nextCursor,
	)), nil
}
