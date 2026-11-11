package dashboard

import (
	"context"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

// Session row stamped with its owning profile and, for a cron run, its scheduler ownership
func getSessionDetail(ctx context.Context, sessionID, profile string) (any, error) {
	return withSessionDB(ctx, profile, true, func(db *state.DB) (*pyjson.Object, error) {
		sid, err := resolveSessionIDOrStatus(db, sessionID)
		if err != nil {
			return nil, err
		}
		var session *pyjson.Object
		if sid != "" {
			if session, err = db.GetSession(ctx, sid); err != nil {
				return nil, err
			}
		}
		if session == nil || len(session.Keys) == 0 {
			return nil, NewHTTPException(404, sessionNotFound)
		}
		owner, err := servingProfile(ctx, profile)
		if err != nil {
			return nil, err
		}
		session.Set("profile", owner)
		session.Set("is_default_profile", owner == "default")
		if source, _ := session.Get("source"); source == "cron" {
			identity, err := db.CronRunIdentity(ctx, sid)
			if err != nil {
				return nil, err
			}
			if identity != nil {
				for _, key := range identity.Keys {
					v, _ := identity.Get(key)
					session.Set(key, v)
				}
			}
		}
		owned, err := cronRunSchedulerOwned(ctx, session, profile)
		if err != nil {
			return nil, err
		}
		if owned != nil {
			session.Set("scheduler_owned", *owned)
		}
		return session, nil
	})
}

// Optional integer query value, nil when absent
func callOptInt(c *Call, name string) *int64 {
	n, ok := c.Value(name).(int64)
	if !ok {
		return nil
	}
	return &n
}

// Mutation and detail router of the sessions page, literal paths declared before the templated session id family
func sessionsManageRouter() *APIRouter {
	r := NewAPIRouter("")
	profileParam := QueryParam("profile", Optional(Str), nil)
	sessionIDParam := PathParam("session_id", Str)
	r.Post("/api/sessions/bulk-delete", &Endpoint{
		Name:   "bulk_delete_sessions_endpoint",
		Doc:    "Delete every session in ``body.ids`` in one transaction (POST: many\n    clients refuse a DELETE body).\n\n    Per :meth:`SessionDB.delete_sessions`: unknown ids are skipped (``deleted``\n    reports what really happened), children are orphaned, active/archived rows\n    ARE deleted (hand-picked), on-disk cleanup is left to the next prune.\n\n    Compression chains are deleted whole: the sessions list shows one row per\n    logical conversation carrying the chain *tip's* id, so deleting only that\n    row would leave the root to resurface as the previous chain link on the\n    next reload (#57543). ``deleted`` still counts the selected rows, not the\n    expanded chain links.\n    ",
		Body:   []BodyParam{Body("body", bulkDeleteSessionsModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) { return bulkDeleteSessions(ctx, c.Object("body")) },
	})
	r.Post("/api/sessions/import", &Endpoint{
		Name:   "import_sessions_endpoint",
		Doc:    "Import sessions exported from the dashboard or CLI (session rows only —\n    ``/api/ops/import`` restores a whole backup archive).",
		Handle: func(ctx context.Context, c *Call) (any, error) { return importSessions(ctx, c.Request) },
	})
	r.Get("/api/sessions/empty/count", &Endpoint{
		Name:   "count_empty_sessions_endpoint",
		Doc:    "Count of empty, ended, non-archived sessions (the \"Delete empty (N)\" button).",
		Params: []Param{profileParam},
		Handle: func(ctx context.Context, c *Call) (any, error) { return countEmptySessions(ctx, c.Str("profile")) },
	})
	r.Delete("/api/sessions/empty", &Endpoint{
		Name:   "delete_empty_sessions_endpoint",
		Doc:    "Delete every empty, ended, non-archived session in one transaction.\n\n    \"Empty\" means NO ``messages`` rows at all — a rewound/compacted chat reads\n    ``message_count == 0`` while its soft-archived rows are the only transcript\n    copy (see :meth:`SessionDB.delete_empty_sessions`).\n\n    * Active sessions are skipped (``ended_at IS NULL``) so a live agent isn't yanked mid-handshake. *\n    Archived sessions are skipped — the user explicitly chose to keep those rows. * Children of deleted\n    parents are orphaned, not cascade-deleted. See #95868.\n    ",
		Params: []Param{profileParam},
		Handle: func(ctx context.Context, c *Call) (any, error) { return deleteEmptySessions(ctx, c.Str("profile")) },
	})
	r.Get("/api/sessions/stats", &Endpoint{
		Name:   "get_session_stats",
		Doc:    "Session-store statistics (mirrors `icebath sessions stats`).",
		Params: []Param{profileParam},
		Handle: func(ctx context.Context, c *Call) (any, error) { return getSessionStats(ctx, c.Str("profile")) },
	})
	r.Get("/api/sessions/{session_id}", &Endpoint{
		Name:   "get_session_detail",
		Params: []Param{sessionIDParam, profileParam},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return getSessionDetail(ctx, c.Str("session_id"), c.Str("profile"))
		},
	})
	r.Get("/api/sessions/{session_id}/mutation-snapshot", &Endpoint{
		Name:   "get_session_mutation_snapshot",
		Params: []Param{sessionIDParam, profileParam},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return getSessionMutationSnapshot(ctx, c.Request, c.Str("session_id"), c.Str("profile"))
		},
	})
	r.Get("/api/sessions/{session_id}/latest-descendant", &Endpoint{
		Name:   "get_session_latest_descendant",
		Params: []Param{sessionIDParam, profileParam},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return getSessionLatestDescendant(ctx, c.Str("session_id"), c.Str("profile"))
		},
	})
	r.Get("/api/sessions/{session_id}/messages", &Endpoint{
		Name: "get_session_messages",
		Params: []Param{sessionIDParam, profileParam, QueryParam("limit", Optional(IntRange(Bound(0), nil)), nil),
			QueryParam("offset", IntRange(Bound(0), nil), int64(0)), QueryParam("order", Optional(Str), nil),
			QueryParam("include_compacted", Bool, false), QueryParam("inline_images", Bool, true)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return getSessionMessages(ctx, c.Str("session_id"), c.Str("profile"), callOptInt(c, "limit"), c.Int("offset"),
				callOptStr(c, "order"), c.Bool("include_compacted"), c.Bool("inline_images"))
		},
	})
	r.Get("/api/sessions/{session_id}/timeline", &Endpoint{
		Name: "get_session_timeline",
		Doc:  "Prompt metadata only, including compacted display history (never rewind rows).\n\n    ``next_cursor`` is a stable logical first-row id; pass it as ``after_row_id``.\n    Entry ``row_id`` addresses the current representative for /messages/around.\n    ",
		Params: []Param{sessionIDParam, profileParam, QueryParam("limit", IntRange(Bound(1), Bound(500)), int64(500)),
			QueryParam("after_row_id", IntRange(Bound(0), nil), int64(0))},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return getSessionTimeline(ctx, c.Str("session_id"), c.Str("profile"), c.Int("limit"), c.Int("after_row_id"))
		},
	})
	r.Get("/api/sessions/{session_id}/messages/around", &Endpoint{
		Name: "get_session_messages_around",
		Doc:  "Bounded display page starting at a timeline prompt; no intervening payloads.",
		Params: []Param{sessionIDParam, RequiredQuery("row_id", IntRange(Bound(1), nil)), profileParam,
			QueryParam("limit", IntRange(Bound(1), Bound(120)), int64(120))},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return getSessionMessagesAround(ctx, c.Str("session_id"), c.Int("row_id"), c.Str("profile"), c.Int("limit"))
		},
	})
	r.Delete("/api/sessions/{session_id}", &Endpoint{
		Name: "delete_session_endpoint",
		Params: []Param{sessionIDParam, profileParam, QueryParam("request_id", Optional(Str), nil),
			QueryParam("expected_revision", Optional(Int), nil), QueryParam("expected_generation", Optional(Int), nil)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return deleteSession(ctx, c.Request, c.Str("session_id"), c.Str("profile"), callOptStr(c, "request_id"),
				callOptInt(c, "expected_revision"), callOptInt(c, "expected_generation"))
		},
	})
	r.Post("/api/sessions/owner-backfill", &Endpoint{
		Name: "backfill_session_owner_profiles",
		Doc:  "Stamp legacy ``profile_name = NULL`` rows with the serving-profile identity.\n\n    A multi-connection Desktop fails closed on unowned rows.  Each ``state.db``\n    belongs to exactly one profile, so this is a single-match, idempotent\n    backfill (non-NULL owners are never overwritten).\n\n    That was fine while one backend served everything, but a Desktop with registry topology (≥2 registered\n    connections) fails closed on unowned rows by design — leaving every pre-campaign session unresumable\n    with no migration path. Each profile's ``state.db`` belongs to exactly one profile, so stamping that\n    store's own name is a single-match backfill, never a guess; the value written is the SAME\n    serving-profile identity the list endpoints already stamp onto outgoing rows (``row_profile`` in\n    ``get_sessions``). See #95407.\n    ",
		Body: []BodyParam{Body("body", sessionOwnerBackfillModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return backfillSessionOwnerProfiles(ctx, c.Request, c.Object("body"))
		},
	})
	r.Patch("/api/sessions/{session_id}", &Endpoint{
		Name:   "rename_session_endpoint",
		Doc:    "Title and sidebar flags share one authoritative revision and receipt.",
		Params: []Param{sessionIDParam},
		Body:   []BodyParam{Body("body", sessionRenameModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return renameSession(ctx, c.Request, c.Str("session_id"), c.Object("body"))
		},
	})
	r.Get("/api/sessions/{session_id}/export", &Endpoint{
		Name:   "export_session_endpoint",
		Doc:    "Stream a single session (metadata + messages) as JSON.",
		Params: []Param{sessionIDParam, profileParam},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return exportSession(ctx, c.Str("session_id"), c.Str("profile"))
		},
	})
	r.Post("/api/sessions/prune", &Endpoint{
		Name: "prune_sessions_endpoint",
		Doc:  "Delete ended sessions matching filters without blocking the event loop.",
		Body: []BodyParam{Body("body", sessionPruneModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return pruneSessionsEndpoint(ctx, c.Object("body"), c.FieldsSet("body", sessionPruneModel))
		},
	})
	return r
}
