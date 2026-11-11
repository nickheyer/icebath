package dashboard

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Prune string filters forwarded to the store, an empty value meaning no filter
var pruneStrFilters = []string{"source", "title_like", "end_reason", "cwd_prefix", "model_like", "provider", "user_id", "chat_id",
	"chat_type", "branch_like"}

// Prune numeric filters forwarded to the store
var pruneNumFilters = []string{"min_messages", "max_messages", "min_tokens", "max_tokens", "min_cost", "max_cost", "min_tool_calls",
	"max_tool_calls"}

// Columns of a dry run prune preview row
var pruneRowKeys = []string{"id", "source", "title", "model", "started_at", "last_active", "message_count"}

// Number of a model field as a float pointer, nil when unset
func modelFloat(body *pyjson.Object, key string) (*float64, error) {
	v, _ := body.Get(key)
	if v == nil {
		return nil, nil
	}
	f, err := pyint.Float(v)
	if err != nil {
		return nil, err
	}
	return &f, nil
}

// Store filters of a prune body with the implicit retention window resolved
func pruneFiltersOf(body *pyjson.Object, fieldsSet map[string]bool) (*float64, string, state.PruneFilters, error) {
	var f state.PruneFilters
	startedBefore, err := modelFloat(body, "started_before")
	if err != nil {
		return nil, "", f, err
	}
	startedAfter, err := modelFloat(body, "started_after")
	if err != nil {
		return nil, "", f, err
	}
	olderThan, err := modelFloat(body, "older_than_days")
	if err != nil {
		return nil, "", f, err
	}
	hasWindow := startedBefore != nil || startedAfter != nil
	if olderThan != nil && *olderThan < 1 && !hasWindow {
		return nil, "", f, NewHTTPException(400, "older_than_days must be >= 1")
	}
	attrFiltersSet := false
	for _, key := range append(append([]string{}, pruneStrFilters...), pruneNumFilters...) {
		if v, _ := body.Get(key); v != nil {
			attrFiltersSet = true
		}
	}
	if hasWindow || (attrFiltersSet && !fieldsSet["older_than_days"]) {
		olderThan = nil
	}
	str := func(key string) string {
		v, _ := body.Get(key)
		s, _ := v.(string)
		return s
	}
	includeArchived, _ := body.Get("include_archived")
	if !pyTruthy(includeArchived) {
		archived := false
		f.Archived = &archived
	}
	f.StartedBefore, f.StartedAfter = startedBefore, startedAfter
	f.TitleLike, f.EndReason, f.CwdPrefix, f.ModelLike = str("title_like"), str("end_reason"), str("cwd_prefix"), str("model_like")
	f.Provider, f.UserID, f.ChatID, f.ChatType, f.BranchLike = str("provider"), str("user_id"), str("chat_id"), str("chat_type"),
		str("branch_like")
	for key, dst := range map[string]**float64{"min_messages": &f.MinMessages, "max_messages": &f.MaxMessages,
		"min_tokens": &f.MinTokens, "max_tokens": &f.MaxTokens, "min_cost": &f.MinCost, "max_cost": &f.MaxCost,
		"min_tool_calls": &f.MinToolCalls, "max_tool_calls": &f.MaxToolCalls} {
		v, err := modelFloat(body, key)
		if err != nil {
			return nil, "", f, err
		}
		*dst = v
	}
	return olderThan, str("source"), f, nil
}

// Deletes ended sessions matching the prune filters, or previews them on a dry run
func pruneSessions(ctx context.Context, body *pyjson.Object, fieldsSet map[string]bool) (*pyjson.Object, error) {
	olderThan, source, filters, err := pruneFiltersOf(body, fieldsSet)
	if err != nil {
		return nil, err
	}
	profileValue, _ := body.Get("profile")
	profile, _ := profileValue.(string)
	profileHome := home.Dir(ctx)
	if profile != "" {
		if _, profileHome, err = cronProfileHome(ctx, profile); err != nil {
			return nil, err
		}
	}
	dryRunValue, _ := body.Get("dry_run")
	dryRun := pyTruthy(dryRunValue)
	db, err := openSessionDBForProfile(ctx, optionalProfile(profile), dryRun)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	skippedOpen, err := db.CountOpenPruneMatches(ctx, olderThan, source, filters)
	if err != nil {
		return nil, err
	}
	if dryRun {
		rows, err := db.ListPruneCandidates(ctx, olderThan, source, true, filters)
		if err != nil {
			return nil, err
		}
		out := pyjson.ObjectOf("ok", true, "removed", 0, "matched", len(rows), "skipped_open", skippedOpen)
		if len(rows) == 0 {
			out.Set("oldest_last_active", nil)
			out.Set("newest_last_active", nil)
			out.Set("oldest_started_at", nil)
			out.Set("newest_started_at", nil)
			out.Set("sessions", []any{})
			return out, nil
		}
		first, _ := rows[0].Get("last_active")
		last, _ := rows[len(rows)-1].Get("last_active")
		out.Set("oldest_last_active", first)
		out.Set("newest_last_active", last)
		var oldest, newest any
		var oldestF, newestF float64
		sessions := make([]any, len(rows))
		for i, r := range rows {
			started, _ := r.Get("started_at")
			f, err := pyint.Float(started)
			if err != nil {
				return nil, err
			}
			if i == 0 || f < oldestF {
				oldest, oldestF = started, f
			}
			if i == 0 || f > newestF {
				newest, newestF = started, f
			}
			picked := pyjson.NewObject()
			for _, key := range pruneRowKeys {
				v, _ := r.Get(key)
				picked.Set(key, v)
			}
			sessions[i] = picked
		}
		out.Set("oldest_started_at", oldest)
		out.Set("newest_started_at", newest)
		out.Set("sessions", sessions)
		return out, nil
	}
	sessionsDir := filepath.Join(profileHome, "sessions")
	if _, err := os.Stat(sessionsDir); err != nil {
		sessionsDir = ""
	}
	removed, err := db.PruneSessions(ctx, state.PruneSessionsOptions{OlderThanDays: olderThan, Source: source, SessionsDir: sessionsDir,
		ExcludeActiveWriteGuards: true, Filters: filters})
	if err != nil {
		return nil, err
	}
	return pyjson.ObjectOf("ok", true, "removed", removed, "skipped_open", skippedOpen), nil
}

// Deletes every listed session in one transaction, compression chains whole, live rows reported as skipped
func bulkDeleteSessions(ctx context.Context, body *pyjson.Object) (any, error) {
	idsValue, _ := body.Get("ids")
	list, _ := idsValue.([]any)
	if len(list) > 500 {
		return nil, NewHTTPException(400, "ids must contain at most 500 entries")
	}
	ids := make([]string, len(list))
	for i, id := range list {
		ids[i], _ = id.(string)
	}
	profileValue, _ := body.Get("profile")
	requested, _ := profileValue.(string)
	profile, err := destructiveProfile(ctx, requested, "POST /api/sessions/bulk-delete")
	if err != nil {
		return nil, err
	}
	skipped := []string{}
	deleted, err := withSessionDB(ctx, profile, false, func(db *state.DB) (int, error) {
		return db.DeleteSessions(ctx, ids, state.DeleteSessionsOptions{ExcludeActiveWriteGuards: true, SkippedIDs: &skipped,
			IncludeCompressionChain: true})
	})
	if err != nil {
		return nil, err
	}
	skippedAny := make([]any, len(skipped))
	for i, s := range skipped {
		skippedAny[i] = s
	}
	return pyjson.ObjectOf("ok", true, "deleted", deleted, "skipped_active", skippedAny), nil
}

// Imports exported session rows through the owner's revision fenced import
func importSessions(ctx context.Context, req *Request) (any, error) {
	raw, err := readSessionImportBody(ctx, req)
	if err != nil {
		return nil, err
	}
	decoded, err := pyjson.Loads(string(raw))
	if err != nil {
		return nil, &causedHTTPException{NewHTTPException(400, "Invalid session import payload"), err}
	}
	validated, problems := sessionImportModel.Validate(decoded)
	if len(problems) > 0 {
		return nil, NewHTTPException(400, "Invalid session import payload")
	}
	body, _ := validated.(*pyjson.Object)
	sessionsValue, _ := body.Get("sessions")
	sessions, _ := sessionsValue.([]any)
	anchor := ""
	if len(sessions) > 0 {
		if first, ok := sessions[0].(*pyjson.Object); ok {
			id := first.GetOr("id", "")
			anchor, _ = id.(string)
		}
	}
	profileValue, _ := body.Get("profile")
	profile, _ := profileValue.(string)
	requestID, expectedRevision := optionalStringField(body, "request_id"), optionalIntField(body, "expected_revision")
	return mutateSessionRequest(ctx, req, profile, anchor, requestID, expectedRevision, "import",
		pyjson.ObjectOf("sessions", sessionsValue), nil)
}

// String field of a validated model, nil when unset
func optionalStringField(body *pyjson.Object, key string) *string {
	v, _ := body.Get(key)
	s, ok := v.(string)
	if !ok {
		return nil
	}
	return &s
}

// Integer field of a validated model, nil when unset
func optionalIntField(body *pyjson.Object, key string) *int64 {
	v, _ := body.Get(key)
	n, ok := v.(int64)
	if !ok {
		return nil
	}
	return &n
}

// Count of empty, ended, non archived sessions
func countEmptySessions(ctx context.Context, profile string) (any, error) {
	count, err := withSessionDB(ctx, profile, true, func(db *state.DB) (int, error) { return db.CountEmptySessions(ctx) })
	if err != nil {
		return nil, err
	}
	return pyjson.ObjectOf("count", count), nil
}

// Deletes every empty, ended, non archived session in one transaction
func deleteEmptySessions(ctx context.Context, requested string) (any, error) {
	profile, err := destructiveProfile(ctx, requested, "DELETE /api/sessions/empty")
	if err != nil {
		return nil, err
	}
	sessionsDir, err := sessionFilesDir(ctx, profile)
	if err != nil {
		return nil, err
	}
	deleted, err := withSessionDB(ctx, profile, false, func(db *state.DB) (int, error) {
		return db.DeleteEmptySessions(ctx, sessionsDir)
	})
	if err != nil {
		return nil, err
	}
	return pyjson.ObjectOf("ok", true, "deleted", deleted), nil
}

// Store statistics the sessions page pairs with its chain collapsed rows
func getSessionStats(ctx context.Context, profile string) (any, error) {
	return withSessionDB(ctx, profile, true, func(db *state.DB) (any, error) {
		total, err := db.SessionCount(ctx, state.SessionCountOptions{IncludeArchived: true, ExcludeChildren: true})
		if err != nil {
			return nil, err
		}
		active, err := db.SessionCount(ctx, state.SessionCountOptions{IncludeArchived: false, ExcludeChildren: true})
		if err != nil {
			return nil, err
		}
		archived, err := db.SessionCount(ctx, state.SessionCountOptions{ArchivedOnly: true, ExcludeChildren: true})
		if err != nil {
			return nil, err
		}
		messages, err := db.MessageCount(ctx, "")
		if err != nil {
			return nil, err
		}
		out := pyjson.ObjectOf("total", total, "active_store", active, "archived", archived, "messages", messages,
			"by_source", pyjson.NewObject())
		if bySource, err := db.SessionCountBySource(ctx, true, false, true); err == nil {
			out.Set("by_source", bySource)
		}
		return out, nil
	})
}

// Revision counters the offline receipt or the owner checks for one session
func getSessionMutationSnapshot(ctx context.Context, req *Request, sessionID, profile string) (any, error) {
	authority, _, err := sessionMutationContext(ctx, req, profile)
	if err != nil {
		return nil, err
	}
	var row *pyjson.Object
	if authority == nil {
		row, err = withSessionDB(ctx, profile, true, func(db *state.DB) (*pyjson.Object, error) { return db.GetSession(ctx, sessionID) })
	} else {
		row, err = authority.DB.GetSession(ctx, sessionID)
	}
	if err != nil {
		return nil, err
	}
	if row == nil {
		return pyjson.ObjectOf("session_id", sessionID, "exists", false, "runtime_revision", 0, "runtime_generation", nil), nil
	}
	revision, _ := row.Get("runtime_revision")
	generation, _ := row.Get("runtime_generation")
	return pyjson.ObjectOf("session_id", sessionID, "exists", true, "runtime_revision", revision, "runtime_generation", generation), nil
}

// Newest continuation leaf of a session with its path, 404 for an unknown session
func getSessionLatestDescendant(ctx context.Context, sessionID, profile string) (any, error) {
	type leaf struct {
		latest string
		path   []string
	}
	found, err := withSessionDB(ctx, profile, true, func(db *state.DB) (leaf, error) {
		latest, path, err := sessionLatestDescendant(ctx, sessionID, db)
		return leaf{latest, path}, err
	})
	if err != nil {
		return nil, err
	}
	if found.latest == "" {
		return nil, NewHTTPException(404, sessionNotFound)
	}
	requested := sessionID
	if len(found.path) > 0 {
		requested = found.path[0]
	}
	path := make([]any, len(found.path))
	for i, p := range found.path {
		path[i] = p
	}
	return pyjson.ObjectOf("requested_session_id", requested, "session_id", found.latest, "path", path,
		"changed", len(found.path) > 0 && found.latest != found.path[0]), nil
}

// Bridge tool call labels a stored assistant message carries, keyed by call id
func storedToolCallLabels(message map[string]any) map[string]any {
	out := map[string]any{}
	calls, _ := message["tool_calls"].([]any)
	for _, raw := range calls {
		call, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		fn, _ := call["function"].(map[string]any)
		if fn == nil {
			fn = map[string]any{}
		}
		callID, name := "", ""
		if pyTruthy(call["id"]) {
			callID = pyrepr.Str(call["id"])
		}
		if pyTruthy(fn["name"]) {
			name = pyrepr.Str(fn["name"])
		}
		if callID == "" || !tools.BridgeToolNames[name] {
			continue
		}
		args := map[string]any{}
		if text, ok := fn["arguments"].(string); ok && text != "" {
			if decoded, err := pyjson.Loads(text); err == nil {
				if m, ok := pyjson.Plain(decoded).(map[string]any); ok {
					args = m
				}
			}
		}
		labels := tuigateway.BridgedToolLabels(name, args)
		if len(labels) > 0 {
			payloads := make([]any, len(labels))
			for i, label := range labels {
				payloads[i] = label
			}
			out[callID] = payloads
		}
	}
	return out
}

// Shallow copy of a message
func copyMessage(message map[string]any) map[string]any {
	out := make(map[string]any, len(message)+1)
	for k, v := range message {
		out[k] = v
	}
	return out
}

// Message with its stored tool call labels attached when it has any
func withToolCallLabels(message map[string]any) map[string]any {
	labels := storedToolCallLabels(message)
	if len(labels) == 0 {
		return message
	}
	out := copyMessage(message)
	out["tool_call_labels"] = labels
	return out
}

// Home whose history a profile's reads project in, this process's home when none is named
func historyProfileHome(ctx context.Context, profile string) (string, error) {
	if profile != "" {
		_, profileHome, err := cronProfileHome(ctx, profile)
		return profileHome, err
	}
	return home.Dir(ctx), nil
}

// Transcript dir of the profile whose store a delete targets
func sessionFilesDir(ctx context.Context, profile string) (string, error) {
	profileHome, err := historyProfileHome(ctx, profile)
	if err != nil {
		return "", err
	}
	return filepath.Join(profileHome, "sessions"), nil
}

// Whether a message is an untyped System scaffold notice persisted as a user row
func isUntypedScaffoldNotice(message map[string]any) bool {
	if message["role"] != "user" || pyTruthy(message["display_kind"]) {
		return false
	}
	content, ok := message["content"].(string)
	return ok && strings.HasPrefix(strings.TrimLeft(content, " \t\n\r\x0b\x0c"), "[System:")
}

// Display projection of stored messages, compaction summaries replaced and scaffold notices hidden
func projectForDisplay(ctx context.Context, messages []map[string]any, profileHome string, inlineImages bool) ([]any, error) {
	projected := make([]any, 0, len(messages))
	for _, message := range messages {
		message = withToolCallLabels(message)
		if !inlineImages && message["content"] != nil {
			message = copyMessage(message)
			message["content"] = tuigateway.CoerceMessageText(message["content"], false)
		}
		if !pyTruthy(message["display_kind"]) {
			if kind, ok := state.UntypedFailedTurnDisplayKind(message["role"], message["content"]); ok && kind != "" {
				message = copyMessage(message)
				message["display_kind"] = kind
			}
		}
		if isUntypedScaffoldNotice(message) {
			hidden := copyMessage(message)
			hidden["display_kind"] = "hidden"
			projected = append(projected, hidden)
			continue
		}
		if message["role"] == "user" && message["display_kind"] == state.SteerDisplayKind {
			if steer, ok := state.ExtractSteerTextFromMessage(message); ok && steer != "" {
				message = copyMessage(message)
				message["display_content"] = steer
			}
		}
		if message["role"] == "user" {
			if typed, ok := state.WithheldPromptText(message); ok {
				message = copyMessage(message)
				message["display_content"] = typed
			}
		}
		if !state.IsCompactionSummaryMessage(message) {
			projected = append(projected, message)
			continue
		}
		view := state.ProjectCompactionMessageForDisplay(message)
		out := copyMessage(message)
		if view == nil {
			if !pyTruthy(out["display_kind"]) {
				out["display_kind"] = "hidden"
			}
		} else {
			out["display_content"] = view["content"]
			delete(out, "display_kind")
		}
		projected = append(projected, out)
	}
	return tuigateway.ProjectHistoryCommentary(ctx, projected, profileHome)
}

// Plain message dicts of stored rows
func plainMessages(rows []*pyjson.Object) []map[string]any {
	out := make([]map[string]any, len(rows))
	for i, row := range rows {
		out[i], _ = pyjson.Plain(row).(map[string]any)
	}
	return out
}

// One display page of a session transcript including compression ancestors, the latest page by default
func getSessionMessages(ctx context.Context, sessionID, profile string, limit *int64, offset int64, order *string,
	includeCompacted, inlineImages bool) (any, error) {
	if order != nil && *order != "oldest" && *order != "latest" {
		return nil, NewHTTPException(400, "order must be one of: oldest, latest")
	}
	type page struct {
		sid      string
		limit    int
		messages []*pyjson.Object
	}
	result, err := withSessionDB(ctx, profile, true, func(db *state.DB) (*page, error) {
		sid, err := resolveSessionIDOrStatus(db, sessionID)
		if err != nil || sid == "" {
			return nil, err
		}
		if sid, err = db.ResolveResumeSessionID(sid); err != nil {
			return nil, err
		}
		defaultPage := limit == nil
		latestPage := (order != nil && *order == "latest") || (order == nil && defaultPage)
		pageLimit := 500
		if !defaultPage {
			pageLimit = int(min(*limit, 500))
		}
		messages, err := db.GetMessages(ctx, sid, state.GetMessagesOptions{Limit: &pageLimit, Offset: int(offset), Latest: latestPage,
			IncludeCompacted: includeCompacted, IncludeAncestors: true})
		if err != nil {
			return nil, err
		}
		return &page{sid, pageLimit, messages}, nil
	})
	if err != nil {
		return nil, err
	}
	if result == nil {
		return nil, NewHTTPException(404, sessionNotFound)
	}
	profileHome, err := historyProfileHome(ctx, profile)
	if err != nil {
		return nil, err
	}
	projected, err := projectForDisplay(ctx, plainMessages(result.messages), profileHome, inlineImages)
	if err != nil {
		return nil, err
	}
	owner, err := servingProfile(ctx, profile)
	if err != nil {
		return nil, err
	}
	pageOrder := "oldest"
	if order != nil && *order != "" {
		pageOrder = *order
	} else if limit == nil {
		pageOrder = "latest"
	}
	return pyjson.ObjectOf("session_id", result.sid, "profile", owner, "messages", projected,
		"pagination", pyjson.ObjectOf("limit", result.limit, "offset", offset, "order", pageOrder, "returned", len(projected))), nil
}

// Exact session id of a durable jump address owned by this profile's store, 404 otherwise
func timelineSessionID(ctx context.Context, db *state.DB, sessionID, owner string) (string, error) {
	owned := func(sid string) (bool, error) {
		rows, err := db.ReadAll(ctx, "SELECT profile_name FROM sessions WHERE id = ?", sid)
		if err != nil || len(rows) == 0 {
			return false, err
		}
		name, _ := rows[0].Get("profile_name")
		return name == nil || name == owner, nil
	}
	ok, err := owned(sessionID)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", NewHTTPException(404, sessionNotFound)
	}
	sid, err := db.ResolveResumeSessionID(sessionID)
	if err != nil {
		return "", err
	}
	if ok, err = owned(sid); err != nil {
		return "", err
	}
	if !ok {
		return "", NewHTTPException(404, sessionNotFound)
	}
	return sid, nil
}

// Prompt metadata of a session including compacted display history, paged by a stable row cursor
func getSessionTimeline(ctx context.Context, sessionID, profile string, limit, afterRowID int64) (any, error) {
	owner, err := servingProfile(ctx, profile)
	if err != nil {
		return nil, err
	}
	return withSessionDB(ctx, profile, true, func(db *state.DB) (any, error) {
		sid, err := timelineSessionID(ctx, db, sessionID, owner)
		if err != nil {
			return nil, err
		}
		timeline, err := db.GetSessionTimeline(ctx, sid, int(limit), afterRowID)
		if err != nil {
			return nil, err
		}
		out := pyjson.ObjectOf("session_id", sid, "profile", owner)
		for _, key := range timeline.Keys {
			v, _ := timeline.Get(key)
			out.Set(key, v)
		}
		return out, nil
	})
}

// Bounded display page starting at a timeline prompt
func getSessionMessagesAround(ctx context.Context, sessionID string, rowID int64, profile string, limit int64) (any, error) {
	owner, err := servingProfile(ctx, profile)
	if err != nil {
		return nil, err
	}
	result, err := withSessionDB(ctx, profile, true, func(db *state.DB) (*pyjson.Object, error) {
		sid, err := timelineSessionID(ctx, db, sessionID, owner)
		if err != nil {
			return nil, err
		}
		page, err := db.GetSessionMessagesAround(ctx, sid, rowID, int(limit))
		if err != nil {
			return nil, err
		}
		if page == nil {
			return nil, NewHTTPException(404, "Prompt not found")
		}
		out := pyjson.ObjectOf("session_id", sid, "profile", owner)
		for _, key := range page.Keys {
			v, _ := page.Get(key)
			out.Set(key, v)
		}
		return out, nil
	})
	if err != nil {
		return nil, err
	}
	profileHome, err := historyProfileHome(ctx, profile)
	if err != nil {
		return nil, err
	}
	raw, _ := result.Get("messages")
	var messages []map[string]any
	switch list := raw.(type) {
	case []*pyjson.Object:
		messages = plainMessages(list)
	case []any:
		for _, m := range list {
			if plain, ok := pyjson.Plain(m).(map[string]any); ok {
				messages = append(messages, plain)
			}
		}
	}
	projected, err := projectForDisplay(ctx, messages, profileHome, true)
	if err != nil {
		return nil, err
	}
	result.Set("messages", projected)
	return result, nil
}

// Deletes a session through the revision fenced owner verb, an already absent id answering success
func deleteSession(ctx context.Context, req *Request, sessionID, profile string, requestID *string, expectedRevision,
	expectedGeneration *int64) (any, error) {
	result, err := mutateSessionRequest(ctx, req, profile, sessionID, requestID, expectedRevision, "delete", pyjson.NewObject(),
		expectedGeneration)
	if err != nil {
		if exc, ok := asHTTPException(err); ok && exc.StatusCode == 404 && exc.Detail == "not_found" {
			absent, aerr := withSessionDB(ctx, profile, true, func(db *state.DB) (bool, error) {
				row, err := db.GetSession(ctx, sessionID)
				return row == nil, err
			})
			if aerr != nil {
				return nil, aerr
			}
			if absent {
				return pyjson.ObjectOf("ok", true, "already_absent", true), nil
			}
		}
		return nil, err
	}
	sessionsDir, err := sessionFilesDir(ctx, profile)
	if err != nil {
		return nil, err
	}
	if ids, _ := result.Get("deleted_ids"); pyTruthy(ids) {
		list, _ := ids.([]any)
		for _, sid := range list {
			state.RemoveSessionFiles(sessionsDir, pyrepr.Str(sid))
		}
	}
	return result, nil
}

// Live owner's own store for a profile when a composed gateway serves it, nil otherwise
func ownerDBFor(ctx context.Context, req *Request, profile string) (*state.DB, error) {
	runner := req.App().State.Get("gateway_runner")
	if runner == nil {
		return nil, nil
	}
	profileHome, err := sessionMutationHome(ctx, profile)
	if err != nil {
		return nil, err
	}
	authority, err := gateway.AuthorityForHome(runner, profileHome)
	if err != nil || authority == nil {
		return nil, err
	}
	return authority.DB, nil
}

// Stamps legacy unowned rows of a store with its serving profile identity
func backfillSessionOwnerProfiles(ctx context.Context, req *Request, body *pyjson.Object) (any, error) {
	profileValue, _ := body.Get("profile")
	profile, _ := profileValue.(string)
	stamp, err := servingProfile(ctx, profile)
	if err != nil {
		return nil, err
	}
	stamped, err := func() (int, error) {
		ownerDB, err := ownerDBFor(ctx, req, profile)
		if err != nil {
			return 0, err
		}
		if ownerDB != nil {
			return ownerDB.BackfillNullSessionProfiles(ctx, stamp)
		}
		return withSessionDB(ctx, profile, false, func(db *state.DB) (int, error) { return db.BackfillNullSessionProfiles(ctx, stamp) })
	}()
	if err != nil {
		detail := "Internal server error"
		return nil, httpFailure(err, "POST /api/sessions/owner-backfill failed", 500, "", &detail)
	}
	if stamped > 0 {
		routersLog().Info(fmt.Sprintf("owner-backfill: stamped %d legacy NULL-profile session row(s) with profile %s", stamped,
			pyrepr.Repr(stamp)))
	}
	return pyjson.ObjectOf("ok", true, "stamped", stamped, "profile", stamp), nil
}

// Title and sidebar flags through one authoritative revision and receipt
func renameSession(ctx context.Context, req *Request, sessionID string, body *pyjson.Object) (any, error) {
	payload := pyjson.NewObject()
	for _, key := range []string{"title", "archived", "hidden", "pinned", "unread"} {
		if v, _ := body.Get(key); v != nil {
			payload.Set(key, v)
		}
	}
	profileValue, _ := body.Get("profile")
	profile, _ := profileValue.(string)
	return mutateSessionRequest(ctx, req, profile, sessionID, optionalStringField(body, "request_id"),
		optionalIntField(body, "expected_revision"), "sidebar", payload, optionalIntField(body, "expected_generation"))
}

// Compact JSON of an encodable value without ASCII escaping
func compactJSON(v any) (string, error) {
	return pyjson.Dumps(jsonableEncoder(v), pyjson.Separators(",", ":"), pyjson.EnsureASCII(false))
}

// Streams one session's metadata and every message row as JSON, keyset paged over row ids
func exportSession(ctx context.Context, sessionID, profile string) (any, error) {
	type prepared struct {
		sid     string
		session *pyjson.Object
	}
	found, err := withSessionDB(ctx, profile, true, func(db *state.DB) (*prepared, error) {
		sid, err := resolveSessionIDOrStatus(db, sessionID)
		if err != nil || sid == "" {
			return nil, err
		}
		session, err := db.GetSession(ctx, sid)
		return &prepared{sid, session}, err
	})
	if err != nil {
		return nil, err
	}
	if found == nil || found.session == nil {
		return nil, NewHTTPException(404, sessionNotFound)
	}
	stream := func(ctx context.Context, yield func([]byte) error) error {
		db, err := openSessionDBForProfile(ctx, optionalProfile(profile), true)
		if err != nil {
			return err
		}
		defer db.Close()
		head, err := compactJSON(found.session)
		if err != nil {
			return err
		}
		if err := yield([]byte(head[:len(head)-1] + `,"messages":[`)); err != nil {
			return err
		}
		var lastID int64
		first := true
		for {
			pageLimit := 500
			after := lastID
			messages, err := db.GetMessages(ctx, found.sid, state.GetMessagesOptions{Limit: &pageLimit, AfterID: &after,
				IncludeInactive: true})
			if err != nil {
				return err
			}
			for _, message := range messages {
				text, err := compactJSON(message)
				if err != nil {
					return err
				}
				sep := ","
				if first {
					sep = ""
				}
				if err := yield([]byte(sep + text)); err != nil {
					return err
				}
				first = false
			}
			if len(messages) != 500 {
				break
			}
			id, _ := messages[len(messages)-1].Get("id")
			n, ok := id.(int64)
			if !ok {
				break
			}
			lastID = n
		}
		return yield([]byte("]}"))
	}
	return NewStreamingResponse(stream, 200, "application/json"), nil
}

// Deletes or previews ended sessions matching filters, the destructive profile rule for a real prune
func pruneSessionsEndpoint(ctx context.Context, body *pyjson.Object, fieldsSet map[string]bool) (any, error) {
	dryRun, _ := body.Get("dry_run")
	if pyTruthy(dryRun) {
		return pruneSessions(ctx, body, fieldsSet)
	}
	profileValue, _ := body.Get("profile")
	requested, _ := profileValue.(string)
	profile, err := destructiveProfile(ctx, requested, "POST /api/sessions/prune")
	if err != nil {
		return nil, err
	}
	scoped := body.Copy()
	if profile == "" {
		scoped.Set("profile", nil)
	} else {
		scoped.Set("profile", profile)
	}
	return withSessionMaintenance(ctx, profile, func(ctx context.Context) (*pyjson.Object, error) {
		return pruneSessions(ctx, scoped, fieldsSet)
	})
}
