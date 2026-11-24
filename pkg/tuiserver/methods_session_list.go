package tuiserver

import (
	"strings"

	"context"
	"slices"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

// Declares the stored session listing methods on the session module registry
func registerSessionListHandlers() {
	sessionRegistry.method("session.most_recent", sessionMostRecent)
}

// Value of a row column or the fallback when the column is falsy
func rowOr(row *pyjson.Object, key string, fallback any) any {
	if value := row.GetOr(key, nil); truthy(value) {
		return value
	}
	return fallback
}

// Lowered source of a stored row
func rowSource(row *pyjson.Object) string {
	return strings.ToLower(strings.TrimSpace(strOr(rowOr(row, "source", ""))))
}

// Whether a row's source is hidden from human listings
func deniedSource(row *pyjson.Object) bool {
	return slices.Contains(gateway.InternalListingSources, rowSource(row))
}

// Hidden source or the unknown source of a guard row, never a target for auto resume
func autoResumeDeniedSource(row *pyjson.Object) bool {
	source := rowSource(row)
	return slices.Contains(gateway.InternalListingSources, source) || source == "unknown"
}

// Human facing rows most recent first with the deny list applied
func listingRows(ctx context.Context, db *state.DB, limit int, includeHidden, includeSubagents bool) ([]*pyjson.Object, error) {
	rows, err := db.ListSessionsRich(ctx, state.ListSessionsRichOptions{Limit: &limit, OrderByLastActive: true, CompactRows: true,
		IncludeHidden: includeHidden, IncludeSubagents: includeSubagents})
	if err != nil {
		return nil, err
	}
	out := make([]*pyjson.Object, 0, len(rows))
	for _, row := range rows {
		if !deniedSource(row) {
			out = append(out, row)
		}
	}
	return out, nil
}

// Most recent human facing session that auto resume may land on, null when none, a failing store answered with its cause
func sessionMostRecent(ctx context.Context, rid any, params map[string]any) map[string]any {
	db, release, err := profileDB(ctx, params, false)
	if err != nil {
		return storeFailure(ctx, rid, 5000, err)
	}
	defer release()
	rows, err := listingRows(ctx, db, 200, false, false)
	if err != nil {
		pkgLog().Error("session.most_recent failed", "exc_info", err.Error())
		return storeErrorReply(ctx, rid, 5000, err)
	}
	for _, row := range rows {
		if !autoResumeDeniedSource(row) {
			return ok(rid, map[string]any{"session_id": row.GetOr("id", nil), "title": rowOr(row, "title", ""),
				"started_at": rowOr(row, "started_at", 0), "source": rowOr(row, "source", "")})
		}
	}
	return ok(rid, map[string]any{"session_id": nil})
}
