package tuiserver

import (
	"context"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tuihost"
)

// Declares the title, archive, hidden and reaction methods on the session module registry
func registerSessionMetaHandlers() {
	sessionRegistry.method("session.title", withSession(withSessionDB(5007, sessionTitle)))
}

// Stored title of a session, applying a queued pending title when the row now takes it
func titleRead(ctx context.Context, session *tuihost.Session, db *state.DB, key string) (resolved string) {
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
func sessionTitle(ctx context.Context, rid any, params map[string]any, session *tuihost.Session, db *state.DB) map[string]any {
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
