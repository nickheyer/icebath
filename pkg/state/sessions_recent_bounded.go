package state

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pysqlite"
)

// Bounded recent browse knobs, nil values taking the hermes defaults of 20 rows and a 3 second deadline
type RecentSessionsBoundedOptions struct {
	Limit          *int
	ExcludeSources []string
	TimeoutSeconds *float64
	CandidateLimit *int
	LineageLimit   *int
}

// Compression continuation edge between a parent and child alias, branch, delegate and tool children excluded
const recentCompressionParentEdge = `
            parent.end_reason = 'compression'
            AND child.parent_session_id = parent.id
            AND json_extract(
                COALESCE(child.model_config, '{}'), '$._branched_from'
            ) IS NULL
            AND ` + "%s" + ` IS NULL
            AND COALESCE(child.source, '') != 'tool'
        `

// Recent conversations resolved across compression lineage under a row ceiling and a cooperative deadline
func (d *SessionDB) ListRecentSessionsBounded(ctx context.Context, o RecentSessionsBoundedOptions) ([]*pyjson.Object, error) {
	limit := 20
	if o.Limit != nil {
		limit = *o.Limit
	}
	limit = max(1, limit)
	timeout := 3.0
	if o.TimeoutSeconds != nil {
		timeout = *o.TimeoutSeconds
	}
	timeout = max(0.0, timeout)
	candidateLimit := max(128, limit*8)
	if o.CandidateLimit != nil {
		candidateLimit = *o.CandidateLimit
	}
	candidateLimit = max(limit, min(candidateLimit, 2048))
	lineageLimit := min(8192, candidateLimit*8)
	if o.LineageLimit != nil {
		lineageLimit = *o.LineageLimit
	}
	lineageLimit = max(candidateLimit, min(lineageLimit, 8192))
	clauses := []string{"s.archived = 0", "s.hidden = 0", delegateFromJSON("s.model_config") + " IS NULL"}
	params := []any{}
	if len(o.ExcludeSources) > 0 {
		clauses = append(clauses, "s.source NOT IN ("+placeholders(len(o.ExcludeSources))+")")
		params = append(params, stringsToAny(o.ExcludeSources)...)
	}
	edge := fmt.Sprintf(recentCompressionParentEdge, delegateFromJSON("child.model_config"))
	query := `
            WITH RECURSIVE
            recent_candidates(id) AS (
                SELECT s.id
                FROM sessions s
                WHERE ` + strings.Join(clauses, " AND ") + `
                ORDER BY COALESCE(s.last_activity_at, s.started_at) DESC,
                         s.started_at DESC, s.id DESC
                LIMIT ?
            ),
            ancestors(candidate_id, cur_id) AS (
                SELECT id, id FROM recent_candidates
                UNION
                SELECT a.candidate_id, parent.id
                FROM ancestors a
                JOIN sessions child ON child.id = a.cur_id
                JOIN sessions parent ON ` + edge + `
                LIMIT ?
            ),
            candidate_roots(root_id) AS (
                SELECT DISTINCT a.cur_id
                FROM ancestors a
                JOIN sessions child ON child.id = a.cur_id
                WHERE NOT EXISTS (
                    SELECT 1
                    FROM sessions parent
                    WHERE ` + edge + `
                )
            ),
            chain(root_id, cur_id) AS (
                SELECT root_id, root_id FROM candidate_roots
                UNION
                SELECT c.root_id, child.id
                FROM chain c
                JOIN sessions parent ON parent.id = c.cur_id
                JOIN sessions child ON ` + edge + `
                LIMIT ?
            ),
            chain_rows AS (
                SELECT
                    c.root_id,
                    c.cur_id,
                    ` + sqlSessionLastActiveByID("c.cur_id") + ` AS activity,
                    CASE WHEN EXISTS (
                        SELECT 1
                        FROM sessions parent
                        JOIN sessions child ON ` + edge + `
                        WHERE parent.id = c.cur_id
                    ) THEN 0 ELSE 1 END AS is_tip
                FROM chain c
            ),
            ranked_tips AS (
                SELECT root_id, cur_id, activity,
                       ROW_NUMBER() OVER (
                           PARTITION BY root_id
                           ORDER BY activity DESC, cur_id DESC
                       ) AS rank_in_root
                FROM chain_rows
                WHERE is_tip = 1
            )
            SELECT
                tip.id,
                tip.source,
                tip.model,
                COALESCE(tip.title, s.title) AS title,
                s.started_at AS started_at,
                tip.ended_at,
                tip.end_reason,
                tip.message_count,
                tip.tool_call_count,
                rt.activity AS last_active,
                ` + sqlPreviewRaw("tip.id") + `,
                CASE WHEN s.id != tip.id THEN s.id ELSE NULL END
                    AS _lineage_root_id
            FROM ranked_tips rt
            JOIN sessions s ON s.id = rt.root_id
            JOIN sessions tip ON tip.id = rt.cur_id
            WHERE rt.rank_in_root = 1
              AND s.archived = 0
              AND s.hidden = 0
              AND ` + listableChildSQL + `
              AND ` + delegateFromJSON("s.model_config") + ` IS NULL
            ORDER BY rt.activity DESC, s.started_at DESC, tip.id DESC
            LIMIT ?
        `
	params = append(params, candidateLimit, lineageLimit, lineageLimit, limit)
	deadline := time.Now().Add(time.Duration(timeout * float64(time.Second)))
	interrupted := false
	var rows []*pyjson.Object
	err := d.readCtx(ctx, true, func(conn SQLiteConn) error {
		raw, ok := rawConnOf(conn)
		if !ok {
			return fmt.Errorf("connection %T exposes no driver connection for a progress handler", conn)
		}
		if err := pysqlite.SetProgressHandler(raw, func() int {
			if !time.Now().Before(deadline) {
				interrupted = true
				return 1
			}
			return 0
		}, 1000); err != nil {
			return err
		}
		defer pysqlite.SetProgressHandler(raw, nil, 0)
		var err error
		rows, err = queryObjects(ctx, conn, query, params...)
		return err
	})
	if err != nil {
		if interrupted && strings.Contains(strings.ToLower(errText(err)), "interrupt") {
			return nil, &pyConvError{"TimeoutError", "recent-session browse exceeded " + strconv.FormatFloat(timeout, 'g', 6, 64) + "s deadline"}
		}
		return nil, err
	}
	sessions := make([]*pyjson.Object, 0, len(rows))
	for _, row := range rows {
		session := sessionRowDict(row)
		rawPreview, _ := session.Pop("_preview_raw")
		session.Set("preview", shapePreview(rawPreview))
		unread, err := sessionUnread(session)
		if err != nil {
			return nil, err
		}
		session.Set("unread", unread)
		sessions = append(sessions, session)
	}
	return sessions, nil
}
