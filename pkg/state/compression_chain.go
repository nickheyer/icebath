package state

import (
	"context"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Maximum compression hops any chain walk follows
const chainCap = 1000

// Preferred continuation child of a compression ended parent
var chainStepSQL = `
                    SELECT child.id
                    FROM sessions parent
                    JOIN sessions child ON child.parent_session_id = parent.id
                    WHERE parent.id = ?
                      AND parent.end_reason = 'compression'
                      AND ` + sqlJSONExtract("child.model_config", "$._branched_from") + ` IS NULL
                      AND ` + sqlJSONExtract("child.model_config", "$._delegate_from") + ` IS NULL
                      AND NOT (` + resetChildSQL("child") + `)
                      AND COALESCE(child.source, '') != 'tool'
                    ORDER BY
                      CASE
                        WHEN child.end_reason = 'compression' THEN 0
                        WHEN child.ended_at IS NULL THEN 1
                        ELSE 2
                      END,
                      ` + sqlSessionLastActive("child") + ` DESC,
                      child.started_at DESC,
                      child.id DESC
                    LIMIT 1
                    `

// Whether a created_source value is unset, the accounting placeholder or the routing label
func unsetCreatedSource(v any) bool {
	if v == nil {
		return true
	}
	s, ok := v.(string)
	return ok && (s == "" || s == "unknown" || s == "local")
}

// SQL predicate of an unset created_source column
func unsetCreatedSQL(col string) string {
	return "COALESCE(" + col + ", '') IN ('', 'unknown', 'local')"
}

const createdRowSQL = "SELECT parent_session_id, created_source FROM sessions WHERE id = ?"

var unsetAncestorSQL = `
    WITH RECURSIVE up(pid, n) AS (
        SELECT parent_session_id, 1 FROM sessions WHERE id = ?
        UNION ALL
        SELECT s.parent_session_id, up.n + 1 FROM up JOIN sessions s ON s.id = up.pid
        WHERE s.end_reason = 'compression' AND s.created_source = ? AND up.n < ?
    )
    SELECT 1 FROM up CROSS JOIN sessions s ON s.id = up.pid
    WHERE s.end_reason = 'compression' AND ` + unsetCreatedSQL("s.created_source") + `
    LIMIT 1
`

// Copies a session's created_source onto unset compression ancestors along the preferred continuation chain
func stampCompressionAncestors(ctx context.Context, conn SQLiteConn, sessionID string) error {
	rows, err := queryObjects(ctx, conn, createdRowSQL, sessionID)
	if err != nil {
		return err
	}
	var row *pyjson.Object
	if len(rows) > 0 {
		row = rows[0]
	}
	surface := rowValue(row, "created_source")
	if unsetCreatedSource(surface) {
		return nil
	}
	unset, err := queryObjects(ctx, conn, unsetAncestorSQL, sessionID, surface, chainCap)
	if err != nil || len(unset) == 0 {
		return err
	}
	current, parentID := any(sessionID), rowValue(row, "parent_session_id")
	for range chainCap {
		if !pyTruthy(parentID) {
			return nil
		}
		parents, err := queryObjects(ctx, conn, createdRowSQL, parentID)
		if err != nil {
			return err
		}
		if len(parents) == 0 {
			return nil
		}
		parent := parents[0]
		created := rowValue(parent, "created_source")
		if !unsetCreatedSource(created) && created != surface {
			return nil
		}
		steps, err := queryObjects(ctx, conn, chainStepSQL, parentID)
		if err != nil {
			return err
		}
		if len(steps) == 0 || rowValue(steps[0], "id") != current {
			return nil
		}
		if created != surface {
			if _, err := conn.ExecContext(ctx, "UPDATE sessions SET created_source = ? WHERE id = ?", surface, parentID); err != nil {
				return err
			}
		}
		current, parentID = parentID, rowValue(parent, "parent_session_id")
	}
	return nil
}

// Whether a child row is a genuine compression continuation of a parent that ended by compression
func (d *SessionDB) isCompressionChildRow(ctx context.Context, child *pyjson.Object) (bool, error) {
	parentID, _ := rowValue(child, "parent_session_id").(string)
	if parentID == "" || isExplicitForkChildRow(child, true) {
		return false, nil
	}
	parent, err := d.GetSession(ctx, parentID)
	if err != nil || parent == nil {
		return false, err
	}
	return pyEquals(rowValue(parent, "end_reason"), "compression"), nil
}
