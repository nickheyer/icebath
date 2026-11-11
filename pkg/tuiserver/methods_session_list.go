package tuiserver

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Declares the stored session listing methods on the session module registry
func registerSessionListHandlers() {
	sessionRegistry.method("session.list", withProfileDB(5006, sessionList))
	sessionRegistry.method("session.most_recent", sessionMostRecent)
	sessionRegistry.method("project.facts", projectFacts)
	sessionRegistry.scopedMethod("verification.status", verificationStatusMethod)
}

// Live message count field of a stored transcript read, empty when the store cannot count it
func liveCountField(ctx context.Context, db *state.DB, sessionID string) map[string]any {
	count, err := db.DisplayMessageCount(ctx, sessionID)
	if err != nil {
		return map[string]any{}
	}
	return map[string]any{"live_message_count": count}
}

// Value of a row column or the fallback when the column is falsy
func rowOr(row *pyjson.Object, key string, fallback any) any {
	if value := row.GetOr(key, nil); truthy(value) {
		return value
	}
	return fallback
}

// Compact session.list row, the tip row supplying preview and count and db adding the live count
func sessionRowSummary(ctx context.Context, row, tipRow *pyjson.Object, resolvedID any, db *state.DB) map[string]any {
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
		for key, value := range liveCountField(ctx, db, countID) {
			summary[key] = value
		}
	}
	return summary
}

// Lowered source of a stored row
func rowSource(row *pyjson.Object) string {
	return pystr.Lower(pystr.Strip(strOr(rowOr(row, "source", ""))))
}

// Whether a row's source is hidden from human listings
func deniedSource(row *pyjson.Object) bool {
	return slices.Contains(gateway.InternalListingSources, rowSource(row))
}

// Hidden source or an unknown source guard placeholder, never a target for auto resume
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

// Whether sessions.show_subagents of a home's own config is truthy, false when unreadable
func showSubagentSessions(ctx context.Context, profileHome string) bool {
	homeCtx := home.WithOverride(ctx, profileHome)
	tree, err := config.LoadReadonly(homeCtx, home.Dir(homeCtx), config.EnvRefLookupContext(homeCtx))
	if err != nil {
		return false
	}
	sessions, isDict := tree["sessions"].(map[string]any)
	return isDict && isTruthyValue(sessions["show_subagents"])
}

// Unarchives a recoverable row, a read only listing handle escalating to a short lived writer
func unarchiveRecoverable(ctx context.Context, db *state.DB, sessionID string) (bool, error) {
	if !db.IsReadOnly() {
		return db.UnarchiveRecoverableSession(ctx, sessionID)
	}
	writer, err := state.Acquire(ctx, db.Path())
	if err != nil {
		logger().Warn(fmt.Sprintf("Bot Chat unarchive skipped: writer unavailable for %s", db.Path()), "exc_info", err.Error())
		return false, nil
	}
	defer suppress(func() { _ = writer.Close() })
	return writer.UnarchiveRecoverableSession(ctx, sessionID)
}

// Exact title lookup resolving to the live compression tip, archived and denied rows answering none
func sessionListByTitle(ctx context.Context, rid any, db *state.DB, titleLookup string) map[string]any {
	row, err := db.GetSessionByTitle(ctx, titleLookup)
	panicIf(err)
	if row != nil && truthy(row.GetOr("archived", nil)) && titleLookup == state.CanonicalBotChatTitle {
		recovered, err := unarchiveRecoverable(ctx, db, strOr(row.GetOr("id", nil)))
		panicIf(err)
		if recovered {
			row, err = db.GetSession(ctx, strOr(row.GetOr("id", nil)))
			panicIf(err)
		}
	}
	if row == nil || truthy(row.GetOr("archived", nil)) || deniedSource(row) {
		return ok(rid, map[string]any{"sessions": []any{}})
	}
	rowID := strOr(row.GetOr("id", nil))
	tip := rowID
	suppress(func() {
		resolved, err := db.GetCompressionTip(ctx, rowID)
		panicIf(err)
		if resolved != "" {
			tip = resolved
		}
	})
	tipRow := row
	if tip != rowID {
		fetched, err := db.GetSession(ctx, tip)
		panicIf(err)
		if fetched != nil {
			tipRow = fetched
		}
	}
	return ok(rid, map[string]any{"sessions": []any{sessionRowSummary(ctx, row, tipRow, tip, db)}})
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
	panicIf(err)
	if !parsed.IsInt64() {
		panic(&pyint.Error{Kind: "OverflowError", Msg: "Python int too large to convert to C ssize_t"})
	}
	limit := int(parsed.Int64())
	includeSubagents := db.Path() != "" && showSubagentSessions(ctx, filepath.Dir(db.Path()))
	rows, err := listingRows(ctx, db, max(limit*2, 200), flagParam(params, "include_hidden"), includeSubagents)
	panicIf(err)
	rows = rows[:sliceStop(limit, len(rows))]
	sessions := make([]any, len(rows))
	for i, row := range rows {
		sessions[i] = sessionRowSummary(ctx, row, nil, nil, nil)
	}
	return ok(rid, map[string]any{"sessions": sessions})
}

// Most recent human facing session that auto resume may land on, null when none or on failure
func sessionMostRecent(ctx context.Context, rid any, params map[string]any) map[string]any {
	db, release, err := profileDB(ctx, params, false)
	panicIf(err)
	defer release()
	var found map[string]any
	if failure := catchPanic(func() {
		if db == nil {
			return
		}
		rows, err := listingRows(ctx, db, 200, false, false)
		panicIf(err)
		for _, row := range rows {
			if autoResumeDeniedSource(row) {
				continue
			}
			found = map[string]any{"session_id": row.GetOr("id", nil), "title": rowOr(row, "title", ""),
				"started_at": rowOr(row, "started_at", 0), "source": rowOr(row, "source", "")}
			return
		}
	}); failure != nil {
		logger().Error("session.most_recent failed", "exc_info", failureText(failure))
	}
	if found != nil {
		return ok(rid, found)
	}
	return ok(rid, map[string]any{"session_id": nil})
}

// Coding context facts of a cwd the system prompt would detect, null when not code
func projectFacts(ctx context.Context, rid any, params map[string]any) map[string]any {
	var facts any
	if failure := catchPanic(func() {
		if detected := tools.ProjectFactsFor(strOr(params["cwd"])); detected != nil {
			facts = detected
		}
	}); failure != nil {
		logger().Error("project.facts failed", "exc_info", failureText(failure))
		facts = nil
	}
	return ok(rid, map[string]any{"facts": facts})
}

// Best known verification evidence of a cwd or session, read only
func verificationStatusMethod(ctx context.Context, rid any, params map[string]any) map[string]any {
	sessionID := strOr(params["session_id"])
	if !truthy(params["session_id"]) {
		sessionID = strOr(params["session_key"])
	}
	var status map[string]any
	if failure := catchPanic(func() {
		resolved, err := agent.VerificationStatus(ctx, &tools.Env{Home: home.Dir(ctx)}, sessionID, strOr(params["cwd"]))
		panicIf(err)
		status = resolved
	}); failure != nil {
		logger().Error("verification.status failed", "exc_info", failureText(failure))
		return ok(rid, map[string]any{"verification": map[string]any{"status": "unknown", "evidence": nil}})
	}
	return ok(rid, map[string]any{"verification": status})
}
