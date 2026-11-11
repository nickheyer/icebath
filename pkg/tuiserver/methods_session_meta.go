package tuiserver

import (
	"context"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Declares the title, archive, hidden and reaction methods on the session module registry
func registerSessionMetaHandlers() {
	sessionRegistry.method("session.title", withSession(withSessionDB(5007, sessionTitle)))
	sessionRegistry.method("session.archive", sessionArchive)
	sessionRegistry.method("session.set_hidden", sessionSetHidden)
	sessionMethod("message.react", false, messageReact)
}

// Stored title of a session, applying a queued pending title when the row now takes it
func titleRead(ctx context.Context, session *tuigateway.Session, db *state.DB, key string) (resolved string) {
	fallback := ""
	if truthy(session.Get("pending_title")) {
		fallback = strOr(session.Get("pending_title"))
	}
	if failure := catchPanic(func() {
		title, _, err := db.GetSessionTitle(ctx, key)
		panicIf(err)
		resolved = title
		if fallback == "" {
			if resolved != "" {
				session.Set("pending_title", nil)
			}
			return
		}
		written, err := db.SetSessionTitle(ctx, key, fallback)
		panicIf(err)
		if !written {
			row, err := db.GetSession(ctx, key)
			panicIf(err)
			written = row != nil && pystr.Strip(strOr(rowOr(row, "title", ""))) == fallback
		}
		if written {
			session.Set("pending_title", nil)
			resolved = fallback
		} else if resolved == "" {
			resolved = fallback
		}
	}); failure != nil {
		resolved = fallback
	}
	return resolved
}

// Reads or sets a session title, queueing it as pending when no row can take it yet
func sessionTitle(ctx context.Context, rid any, params map[string]any, session *tuigateway.Session, db *state.DB) map[string]any {
	key := strOr(session.Get("session_key"))
	var result map[string]any
	if _, present := params["title"]; !present {
		result = map[string]any{"title": titleRead(ctx, session, db, key), "session_key": key}
	} else {
		raw := params["title"]
		if !truthy(raw) {
			raw = ""
		}
		text, isText := raw.(string)
		if !isText {
			panic(&pyint.Error{Kind: "AttributeError", Msg: "'" + pyrepr.TypeName(raw) + "' object has no attribute 'strip'"})
		}
		title := pystr.Strip(text)
		if title == "" {
			return rpcErr(rid, 4021, "title required", nil)
		}
		pending, value := false, title
		if failure := catchPanic(func() {
			written, err := db.SetSessionTitle(ctx, key, title)
			panicIf(err)
			if written {
				return
			}
			existing, err := db.GetSession(ctx, key)
			panicIf(err)
			if existing != nil && existing.Len() > 0 {
				value = strOr(rowOr(existing, "title", title))
				return
			}
			ensureSessionDBRow(ctx, session)
			scoped, release := sessionDB(ctx, session)
			defer release()
			applied := false
			if scoped != nil {
				applied, err = scoped.SetSessionTitle(ctx, key, title)
				panicIf(err)
			}
			pending = !applied
		}); failure != nil {
			err := failureError(failure)
			if isValueError(err) {
				return rpcErr(rid, 4022, err.Error(), nil)
			}
			return rpcErr(rid, 5007, err.Error(), nil)
		}
		if pending {
			session.Set("pending_title", value)
		} else {
			session.Set("pending_title", nil)
		}
		result = map[string]any{"pending": pending, "title": value}
	}
	emitSessionInfoForSession(ctx, strOr(params["session_id"]), session)
	return ok(rid, result)
}

// Db of a live session, else the writable params profile db, with its release
func liveOrProfileDB(ctx context.Context, session *tuigateway.Session, params map[string]any) (*state.DB, func()) {
	if session != nil {
		return sessionDB(ctx, session)
	}
	db, release, err := profileDB(ctx, params, true)
	panicIf(err)
	return db, release
}

// Sets a lineage flag on a live session key, else on a stored id the profile db resolves
func setLineageFlag(ctx context.Context, rid any, params map[string]any, session *tuigateway.Session, target, field string, value bool,
	set func(db *state.DB, key string) (bool, error)) map[string]any {
	db, release := liveOrProfileDB(ctx, session, params)
	defer release()
	if db == nil {
		return dbUnavailableError(ctx, rid, 5007)
	}
	key, notFound := "", false
	if failure := catchPanic(func() {
		if session != nil {
			key = strOr(session.Get("session_key"))
			applied, err := set(db, key)
			panicIf(err)
			if !applied {
				session.Set("pending_"+field, value)
			}
			return
		}
		resolved, err := db.ResolveSessionID(target)
		panicIf(err)
		key = resolved
		if key == "" {
			notFound = true
			return
		}
		_, err = set(db, key)
		panicIf(err)
	}); failure != nil {
		return rpcErr(rid, 5007, failureText(failure), nil)
	}
	if notFound {
		return rpcErr(rid, 4001, "session not found", nil)
	}
	return ok(rid, map[string]any{field: value, "session_key": key})
}

// Archives or unarchives a session and its lineage, live runtime first then a stored id
func sessionArchive(ctx context.Context, rid any, params map[string]any) map[string]any {
	raw, present := params["archived"]
	if !present {
		raw = true
	}
	archived := isTruthyValue(raw)
	target := ""
	switch {
	case truthy(params["session_id"]):
		target = strOr(params["session_id"])
	case truthy(params["session_key"]):
		target = strOr(params["session_key"])
	}
	if target == "" {
		return rpcErr(rid, 4006, "session_id required", nil)
	}
	return setLineageFlag(ctx, rid, params, tuigateway.SessionRecord(target), target, "archived", archived, func(db *state.DB, key string) (bool, error) {
		return db.SetSessionArchived(ctx, key, archived, true)
	})
}

// Hides or unhides a session and its lineage, the flag required, live runtime first then a stored id
func sessionSetHidden(ctx context.Context, rid any, params map[string]any) map[string]any {
	raw, present := params["hidden"]
	if !present {
		return rpcErr(rid, 4021, "hidden required", nil)
	}
	hidden := isTruthyValue(raw)
	sid := ""
	if truthy(params["session_id"]) {
		sid = strOr(params["session_id"])
	}
	return setLineageFlag(ctx, rid, params, tuigateway.SessionRecord(sid), strParam(params, "session_id", ""), "hidden", hidden, func(db *state.DB, key string) (bool, error) {
		return db.SetSessionHidden(ctx, key, hidden)
	})
}

// Sets or clears one author's emoji reaction on a stored message, else on the newest of a role
func messageReact(ctx context.Context, rid any, params map[string]any, session *tuigateway.Session) map[string]any {
	newestRole := strParam(params, "newest_role", "")
	rowID := params["row_id"]
	if rowID == nil && newestRole != "user" && newestRole != "assistant" {
		return rpcErr(rid, 4023, "row_id or newest_role required", nil)
	}
	var emoji *string
	if params["emoji"] != nil {
		text := pystr.Strip(pyrepr.Str(params["emoji"]))
		if text == "" {
			return rpcErr(rid, 4024, "emoji must be a non-empty string or null", nil)
		}
		emoji = &text
	}
	author := "user"
	if truthy(params["author"]) {
		author = pystr.Strip(pyrepr.Str(params["author"]))
	}
	if author != "user" && author != "agent" {
		return rpcErr(rid, 4025, "author must be 'user' or 'agent'", nil)
	}
	db, release := sessionDB(ctx, session)
	defer release()
	if db == nil {
		return dbUnavailableError(ctx, rid, 5007)
	}
	rowSession := submitRowTargetKey(session)
	var reactions []any
	found := false
	var resolvedRow int64
	var early map[string]any
	if failure := catchPanic(func() {
		if rowID == nil {
			latest, exists, err := db.LatestMessageRowID(ctx, rowSession, newestRole, 0, true)
			panicIf(err)
			if !exists {
				early = rpcErr(rid, 4040, "no message to react to yet", nil)
				return
			}
			rowID = latest
		}
		n, err := pyint.Of(rowID)
		panicIf(err)
		if !n.IsInt64() {
			panic(&pyint.Error{Kind: "OverflowError", Msg: "Python int too large to convert to SQLite INTEGER"})
		}
		resolvedRow = n.Int64()
		reactions, found, err = db.SetMessageReaction(ctx, rowSession, resolvedRow, emoji, author)
		panicIf(err)
	}); failure != nil {
		return rpcErr(rid, 5007, failureText(failure), nil)
	}
	if early != nil {
		return early
	}
	if !found {
		return rpcErr(rid, 4040, "message not found in this session", nil)
	}
	return ok(rid, map[string]any{"row_id": resolvedRow, "reactions": reactions})
}
