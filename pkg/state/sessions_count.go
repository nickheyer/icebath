package state

import (
	"context"
	"errors"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

type SessionCountOptions struct {
	Source              string
	Sources             []string
	CwdPrefix           string
	MinMessageCount     int
	IncludeArchived     bool
	ArchivedOnly        bool
	ExcludeChildren     bool
	ExcludeSources      []string
	IncludeSubagents    bool
	LocalCreatedSources []string
}

// Counts sessions under the list filters so a paired load more total matches the listing
func (d *SessionDB) SessionCount(ctx context.Context, o SessionCountOptions) (int, error) {
	clauses, params := sessionFilterWhere(sessionFilter{
		excludeChildren: o.ExcludeChildren, source: o.Source, sources: o.Sources, excludeSources: o.ExcludeSources,
		cwdPrefix: o.CwdPrefix, minMessageCount: o.MinMessageCount, archivedOnly: o.ArchivedOnly,
		includeArchived: o.IncludeArchived, includeSubagents: o.IncludeSubagents, localCreatedSources: o.LocalCreatedSources,
	})
	if o.ExcludeChildren {
		if _, err := d.excludeSupersededSegments(ctx, &clauses, &params); err != nil {
			return 0, err
		}
	}
	row, err := d.readOne(ctx, "SELECT COUNT(*) AS n FROM sessions s"+whereSQL(clauses, " "), params...)
	if err != nil || row == nil {
		return 0, err
	}
	n, _ := rowInt(row, "n")
	return int(n), nil
}

// Reports whether at least n sessions exist with archived rows included
func (d *SessionDB) SessionCountGE(ctx context.Context, n int) (bool, error) {
	rows, err := d.readAll(ctx, "SELECT 1 FROM sessions LIMIT ?", n)
	return len(rows) >= n, err
}

// Session counts keyed by source in one grouped read, an empty source counted as cli
func (d *SessionDB) SessionCountBySource(ctx context.Context, includeArchived, archivedOnly, excludeChildren bool) (*pyjson.Object, error) {
	clauses, params := sessionFilterWhere(sessionFilter{excludeChildren: excludeChildren, archivedOnly: archivedOnly, includeArchived: includeArchived})
	if excludeChildren {
		if _, err := d.excludeSupersededSegments(ctx, &clauses, &params); err != nil {
			return nil, err
		}
	}
	var rows []*pyjson.Object
	err := d.readCtx(ctx, true, func(conn SQLiteConn) error {
		if !d.hasWriterConn() {
			return errors.New("SessionDB connection is closed")
		}
		var qerr error
		rows, qerr = queryObjects(ctx, conn, "SELECT COALESCE(NULLIF(s.source, ''), 'cli') AS source, COUNT(*) AS count "+
			"FROM sessions s"+whereSQL(clauses, " ")+" GROUP BY COALESCE(NULLIF(s.source, ''), 'cli') ORDER BY count DESC", params...)
		return qerr
	})
	if err != nil {
		return nil, err
	}
	out := pyjson.NewObject()
	for _, row := range rows {
		count, _ := rowInt(row, "count")
		out.Set(pyStr(rowValue(row, "source")), count)
	}
	return out, nil
}

// Fork child flag and trimmed source of one session from a single read, false and empty when missing
func (d *SessionDB) DeclaredScopeIdentity(ctx context.Context, sessionID string) (bool, string, error) {
	session, err := d.GetSession(ctx, sessionID)
	if err != nil || session == nil {
		return false, "", err
	}
	source := ""
	if v := rowValue(session, "source"); v != nil {
		source = strings.TrimSpace(pyStr(v))
	}
	return isExplicitForkChildRow(session, false), source, nil
}
