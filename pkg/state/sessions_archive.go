package state

import (
	"context"
)

// Deactivates still marked busy queue accept rows of a session on the caller's connection, returning how many
func retireUndrainedQueueRowsConn(ctx context.Context, conn SQLiteConn, sessionID string) (int64, error) {
	res, err := conn.ExecContext(ctx, "UPDATE messages SET active = 0 "+
		"WHERE session_id = ? AND role = 'user' AND active = 1 "+
		"AND COALESCE("+sqlJSONExtract("display_metadata", "$."+QueuedPromptMetadataKey)+", 0) = 1", sessionID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// Retires busy queue accept rows a restart left undrained, keeping them as inactive history
func (d *SessionDB) RetireUndrainedQueueRows(ctx context.Context, sessionID string) (int64, error) {
	var n int64
	err := d.executeWrite(ctx, func(conn SQLiteConn) error {
		var err error
		n, err = retireUndrainedQueueRowsConn(ctx, conn, sessionID)
		return err
	}, nil)
	return n, err
}

// Clears a session's end so it resumes, freezing markerless legacy reset children and dropping a sweep archive
func (d *SessionDB) ReopenSession(ctx context.Context, sessionID string) error {
	return d.executeWrite(ctx, func(conn SQLiteConn) error {
		if _, err := retireUndrainedQueueRowsConn(ctx, conn, sessionID); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, "UPDATE sessions AS child SET model_config = json_set("+
			"COALESCE(child.model_config, '{}'), '$._reset_from', child.parent_session_id) "+
			"WHERE child.parent_session_id = ? AND "+sqlJSONExtract("child.model_config", "$._reset_from")+" IS NULL "+
			"AND "+sqlJSONExtract("child.model_config", "$._branched_from")+" IS NULL "+
			"AND "+sqlJSONExtract("child.model_config", "$._delegate_from")+" IS NULL "+
			"AND COALESCE(child.source, '') != 'tool' "+
			"AND child.started_at >= (SELECT p.started_at FROM sessions p WHERE p.id = child.parent_session_id) "+
			"AND "+legacyResetChildSQL("child", placeholders(len(resetEndReasons))),
			append([]any{sessionID}, stringsToAny(resetEndReasons)...)...); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, "UPDATE sessions SET ended_at = NULL, end_reason = NULL WHERE id = ?", sessionID); err != nil {
			return err
		}
		_, err := unarchiveAutoArchivedLineage(ctx, conn, sessionID)
		return err
	}, nil)
}

// Sets one sessions column across the whole compression lineage, reporting whether any row changed
func (d *SessionDB) setLineageColumn(ctx context.Context, column, sessionID string, value any, extraSetSQL string) (bool, error) {
	var ids []string
	err := d.executeWrite(ctx, func(conn SQLiteConn) error {
		var err error
		ids, err = setLineageColumnInTransaction(ctx, conn, column, sessionID, value, extraSetSQL)
		return err
	}, nil)
	return len(ids) > 0, err
}

// Lineage wide column update on the caller's transaction returning the affected ids, a bare archive clearing sweep provenance
func setLineageColumnInTransaction(ctx context.Context, conn SQLiteConn, column, sessionID string, value any, extraSetSQL string) ([]string, error) {
	if column == "archived" && extraSetSQL == "" {
		extraSetSQL = ", auto_archived = 0"
	}
	rows, err := queryObjects(ctx, conn, lineageCTESQL+`
            UPDATE sessions
            SET `+column+` = ?`+extraSetSQL+`
            WHERE id IN (SELECT id FROM lineage)
            RETURNING id
            `, sessionID, sessionID, value)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, pyStr(rowValue(row, "id")))
	}
	return ids, nil
}

// Deliberately archives or unarchives a lineage, firing the delete hook on a real archive flip when notify holds
func (d *SessionDB) SetSessionArchived(ctx context.Context, sessionID string, archived, notify bool) (bool, error) {
	var ids []string
	newlyArchived := false
	flag := 0
	if archived {
		flag = 1
	}
	err := d.executeWrite(ctx, func(conn SQLiteConn) error {
		row, err := connRow(ctx, conn, "SELECT archived FROM sessions WHERE id = ?", sessionID)
		if err != nil {
			return err
		}
		if ids, err = setLineageColumnInTransaction(ctx, conn, "archived", sessionID, flag, ", auto_archived = 0"); err != nil {
			return err
		}
		newlyArchived = archived && len(ids) > 0 && row != nil && !pyTruthy(row["archived"])
		return nil
	}, nil)
	if err != nil {
		return false, err
	}
	if newlyArchived && notify {
		d.EmitSessionDeleted(ctx, ids, "archive", sessionID, "")
	}
	return len(ids) > 0, nil
}

// Idle sweep archive of a lineage stamping sweep provenance only on rows it newly hides
func (d *SessionDB) autoArchiveLineage(ctx context.Context, sessionID string) (bool, error) {
	return d.setLineageColumn(ctx, "archived", sessionID, 1, ", auto_archived = CASE WHEN archived = 0 THEN 1 ELSE auto_archived END")
}
