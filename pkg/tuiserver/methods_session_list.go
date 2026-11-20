package tuiserver

import (
	"context"
	"fmt"
	"maps"
	"path/filepath"
	"slices"

	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
)

// Declares the stored session listing methods on the session module registry
func registerSessionListHandlers() {
	sessionRegistry.method("session.list", withProfileDB(5006, sessionList))
	sessionRegistry.method("session.most_recent", sessionMostRecent)
}

// Live message count field of a stored transcript read, a count the store cannot answer returned as its error
func liveCountField(ctx context.Context, db *state.DB, sessionID string) (map[string]any, error) {
	count, err := db.DisplayMessageCount(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	return map[string]any{"live_message_count": count}, nil
}

// Value of a row column or the fallback when the column is falsy
func rowOr(row *pyjson.Object, key string, fallback any) any {
	if value := row.GetOr(key, nil); truthy(value) {
		return value
	}
	return fallback
}

// Compact session.list row, the tip row supplying preview and count and db adding the live count
func sessionRowSummary(ctx context.Context, row, tipRow *pyjson.Object, resolvedID any, db *state.DB) (map[string]any, error) {
	if tipRow == nil {
		tipRow = row
	}
	summary := map[string]any{"id": row.GetOr("id", nil), "title": rowOr(row, "title", ""), "preview": rowOr(tipRow, "preview", ""),
		"started_at": rowOr(row, "started_at", 0), "message_count": rowOr(tipRow, "message_count", 0),
		"source": rowOr(row, "source", ""), "_lineage_root_id": row.GetOr("_lineage_root_id", nil)}
	if resolvedID != nil {
		summary["resolved_id"] = resolvedID
	}
	if db != nil {
		countID := strOr(row.GetOr("id", nil))
		if resolvedID != nil {
			countID = strOr(resolvedID)
		}
		counted, err := liveCountField(ctx, db, countID)
		if err != nil {
			return nil, err
		}
		maps.Copy(summary, counted)
	}
	return summary, nil
}

// Lowered source of a stored row
func rowSource(row *pyjson.Object) string {
	return pystr.Lower(pystr.Strip(strOr(rowOr(row, "source", ""))))
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

// Unarchives a recoverable row, a read only listing handle escalating to a short lived writer
func unarchiveRecoverable(ctx context.Context, db *state.DB, sessionID string) (bool, error) {
	if !db.IsReadOnly() {
		return db.UnarchiveRecoverableSession(ctx, sessionID)
	}
	writer, err := state.Acquire(ctx, db.Path())
	if err != nil {
		return false, storeUnavailable(ctx, db.Path(), err)
	}
	defer suppress(func() { _ = writer.Close() })
	return writer.UnarchiveRecoverableSession(ctx, sessionID)
}

// Exact title lookup resolving to the live compression tip, archived and denied rows answering none
func sessionListByTitle(ctx context.Context, rid any, db *state.DB, titleLookup string) map[string]any {
	summary, err := titledSessionSummary(ctx, db, titleLookup)
	if err != nil {
		return storeErrorReply(ctx, rid, 5006, err)
	}
	if summary == nil {
		return ok(rid, map[string]any{"sessions": []any{}})
	}
	return ok(rid, map[string]any{"sessions": []any{summary}})
}

// Summary of the title's row at its live tip, nil when absent, archived or hidden from listings
func titledSessionSummary(ctx context.Context, db *state.DB, titleLookup string) (map[string]any, error) {
	row, err := db.GetSessionByTitle(ctx, titleLookup)
	if err != nil {
		return nil, err
	}
	if row != nil && truthy(row.GetOr("archived", nil)) && titleLookup == state.CanonicalBotChatTitle {
		recovered, err := unarchiveRecoverable(ctx, db, strOr(row.GetOr("id", nil)))
		if err != nil {
			return nil, err
		}
		if recovered {
			if row, err = db.GetSession(ctx, strOr(row.GetOr("id", nil))); err != nil {
				return nil, err
			}
		}
	}
	if row == nil || truthy(row.GetOr("archived", nil)) || deniedSource(row) {
		return nil, nil
	}
	rowID := strOr(row.GetOr("id", nil))
	tip, err := db.GetCompressionTip(ctx, rowID)
	if err != nil {
		return nil, err
	}
	if tip == "" {
		tip = rowID
	}
	tipRow := row
	if tip != rowID {
		fetched, err := db.GetSession(ctx, tip)
		if err != nil {
			return nil, err
		}
		if fetched != nil {
			tipRow = fetched
		}
	}
	return sessionRowSummary(ctx, row, tipRow, tip, db)
}

// Python slice bound of a stop index over a sequence length
func sliceStop(stop, length int) int {
	if stop < 0 {
		stop += length
		if stop < 0 {
			return 0
		}
	}
	return min(stop, length)
}

// Human facing stored sessions, most recent first, an exact title lookup when title is given
func sessionList(ctx context.Context, rid any, params map[string]any, db *state.DB) (out map[string]any) {
	defer func() {
		if failure := recover(); failure != nil {
			out = rpcErr(rid, 5006, failureText(failure), nil)
		}
	}()
	if titleLookup := strParam(params, "title", ""); titleLookup != "" {
		return sessionListByTitle(ctx, rid, db, titleLookup)
	}
	raw, present := params["limit"]
	if !present || !truthy(raw) {
		raw = 200
	}
	parsed, err := pyint.Of(raw)
	if err != nil {
		panic(errchain.Reword(err, "the limit must be a whole number, got %s", errchain.Show(raw)))
	}
	if !parsed.IsInt64() {
		panic(&pyint.Error{Kind: "OverflowError", Msg: fmt.Sprintf("the limit %s is larger than a 64 bit integer holds", parsed.String())})
	}
	limit := int(parsed.Int64())
	includeSubagents := db.Path() != "" && state.ShowSubagentSessions(ctx, filepath.Dir(db.Path()))
	rows, err := listingRows(ctx, db, max(limit*2, 200), flagParam(params, "include_hidden"), includeSubagents)
	if err != nil {
		return storeErrorReply(ctx, rid, 5006, err)
	}
	rows = rows[:sliceStop(limit, len(rows))]
	sessions := make([]any, len(rows))
	for i, row := range rows {
		sessions[i], _ = sessionRowSummary(ctx, row, nil, nil, nil)
	}
	return ok(rid, map[string]any{"sessions": sessions})
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
