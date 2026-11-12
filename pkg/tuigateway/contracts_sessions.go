package tuigateway

import (
	"github.com/nickheyer/icebath/pkg/internal/pydantic"
)

var OpenRequestEntry = pydantic.NewModel("OpenRequestEntry", ClosedConfig,
	pydantic.Req("id", pydantic.Str),
	pydantic.Req("method", pydantic.Str),
	pydantic.Req("params", pydantic.Dict(pydantic.Any)),
).Described("One unanswered server→client request (``server_requests.Request.snapshot``); the reconnecting\nclient re-delivers it to its request handlers.")

var InflightTurn = pydantic.NewModel("InflightTurn", ClosedConfig,
	pydantic.Def("assistant", pydantic.Str, ""),
	pydantic.Def("streaming", pydantic.Bool, false),
	pydantic.Def("user", pydantic.Str, ""),
	pydantic.Def("display_kind", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("display_metadata", pydantic.Nullable(pydantic.Dict(pydantic.Any)), nil),
	pydantic.Def("corrections", pydantic.Nullable(pydantic.List(pydantic.Str)), nil),
	pydantic.Def("correction_offsets", pydantic.Nullable(pydantic.List(pydantic.Int)), nil),
	pydantic.Def("error", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("status", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("recoverable", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("error_surface", pydantic.Nullable(pydantic.Dict(pydantic.Any)), nil),
).Described("``session_auto_continue._inflight_snapshot``: the live (or retained failed) turn a reconnecting\nclient rebuilds its bubbles from.")

var QueuedPrompt = pydantic.NewModel("QueuedPrompt", ClosedConfig,
	pydantic.Req("user", pydantic.Str),
)

var TodoState = pydantic.NewModel("TodoState", ClosedConfig,
	pydantic.Req("todos", pydantic.List(pydantic.Dict(pydantic.Any))),
	pydantic.Req("revision", pydantic.Int),
).Described("``tool_progress._normalize_todo_state``: the authoritative todo snapshot.")

var AutoContinue = pydantic.NewModel("AutoContinue", ClosedConfig,
	pydantic.Req("attempt", pydantic.Int),
	pydantic.Req("interrupted_at", pydantic.Float),
).Described("A crash-interrupted turn was scheduled to continue right after this resume.")

var LiveSessionStatus = pydantic.Enum("LiveSessionStatus", "idle", "idle", "starting", "starting", "waiting", "waiting", "working", "working", "streaming", "streaming", "resuming", "resuming")

var LiveSessionSnapshot = pydantic.NewModel("LiveSessionSnapshot", ClosedConfig,
	pydantic.Req("session_id", pydantic.Str),
	pydantic.Req("message_count", pydantic.Int),
	pydantic.Req("messages", pydantic.List(TranscriptMessage)),
	pydantic.Req("info", SessionLiveInfo),
	pydantic.Def("stored_session_id", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("resumed", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("session_key", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("messages_omitted", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("hydrating", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("running", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("turn_started_at", pydantic.Nullable(pydantic.Float), nil),
	pydantic.Def("started_at", pydantic.Nullable(pydantic.Float), nil),
	pydantic.Def("status", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("inflight", pydantic.Nullable(InflightTurn), nil),
	pydantic.Def("queued", pydantic.Nullable(QueuedPrompt), nil),
	pydantic.Def("pending_approval", pydantic.Nullable(PendingApproval), nil),
	pydantic.Def("open_requests", pydantic.Nullable(pydantic.List(OpenRequestEntry)), nil),
	pydantic.Def("pending_connection", pydantic.Nullable(ConnectionRequestPayload), nil),
	pydantic.Def("todo_state", pydantic.Nullable(TodoState), nil),
	pydantic.Def("auto_continue", pydantic.Nullable(AutoContinue), nil),
)

var SeedMessage = pydantic.NewModel("SeedMessage", OpenParamsConfig,
	pydantic.Req("role", pydantic.Str),
	pydantic.Def("content", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("text", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("display_kind", pydantic.Nullable(pydantic.Str), nil),
).Described("One create-time transcript row (``session_history._coerce_seed_history``); ``text`` is the\nlegacy alias of ``content``; only ``display_kind: \"hidden\"`` is accepted from the wire. Clients\nforward stored rows verbatim (``_row_id``, ``timestamp``, …) and the coercer drops what it does\nnot use, so the row stays open.")

var SessionCreateParams = pydantic.Extend(ProfileParams, "SessionCreateParams", ParamsConfig,
	pydantic.Def("cols", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("source", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("cwd", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("cwd_explicit", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("messages", pydantic.Nullable(pydantic.List(SeedMessage)), nil),
	pydantic.Def("parent_session_id", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("title", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("model", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("provider", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("reasoning_effort", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("fast", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("service_tier", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("close_on_disconnect", pydantic.Bool, false),
	pydantic.Def("hidden", pydantic.Bool, false),
	pydantic.Def("room_plumbing", pydantic.Bool, false),
	pydantic.Def("follow_profile_config", pydantic.Bool, false),
	pydantic.Def("idempotency_key", pydantic.Nullable(pydantic.Str), nil),
)

var SessionCreateResult = pydantic.NewModel("SessionCreateResult", ClosedConfig,
	pydantic.Req("session_id", pydantic.Str),
	pydantic.Req("stored_session_id", pydantic.Str),
	pydantic.Req("message_count", pydantic.Int),
	pydantic.Req("messages", pydantic.List(TranscriptMessage)),
	pydantic.Req("info", SessionLiveInfo),
)

var _ = Method("session.create", SessionCreateParams, SessionCreateResult, "Mint a live session (agent builds after the reply); a DB row appears on the first prompt unless seeded.")

var SessionBranchStoredParams = pydantic.Extend(ProfileParams, "SessionBranchStoredParams", ParamsConfig,
	pydantic.Req("parent_session_id", pydantic.ConStr(pydantic.MinLen(1))),
	pydantic.Def("cols", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("source", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("cwd", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("idempotency_key", pydantic.Nullable(pydantic.Str), nil),
)

var SessionBranchStoredResult = pydantic.NewModel("SessionBranchStoredResult", ClosedConfig,
	pydantic.Req("session_id", pydantic.Str),
	pydantic.Req("stored_session_id", pydantic.Str),
	pydantic.Req("message_count", pydantic.Int),
	pydantic.Req("messages_omitted", pydantic.Bool),
	pydantic.Req("info", SessionLiveInfo),
)

var _ = Method("session.branch_stored", SessionBranchStoredParams, SessionBranchStoredResult, "Whole-session branch of a stored parent: the owning backend reads and copies the transcript, which never crosses the wire (a separate method so an older gateway fails loudly, not with an empty branch).")

var SessionResumeParams = pydantic.Extend(SessionParams, "SessionResumeParams", ParamsConfig,
	pydantic.Def("cols", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("source", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("lazy", pydantic.Bool, false),
	pydantic.Def("defer_history", pydantic.Bool, false),
	pydantic.Def("omit_messages", pydantic.Bool, false),
	pydantic.Def("eager_build", pydantic.Bool, false),
	pydantic.Def("close_on_disconnect", pydantic.Bool, false),
	pydantic.Def("inline_images", pydantic.Bool, true),
).Described("``session_id`` is the STORED id (or an exact title); the reply's ``session_id`` is the runtime id.")

var SessionResumeResult = pydantic.Extend(LiveSessionSnapshot, "SessionResumeResult", ClosedConfig)

var _ = Method("session.resume", SessionResumeParams, SessionResumeResult, "Attach to a stored session: reuse it if live here, else lazy / deferred / cold / eager rebuild.")

var SessionActivateParams = pydantic.Extend(SessionParams, "SessionActivateParams", ParamsConfig,
	pydantic.Def("cols", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("omit_messages", pydantic.Bool, false),
)

var SessionActivateResult = pydantic.Extend(LiveSessionSnapshot, "SessionActivateResult", ClosedConfig)

var _ = Method("session.activate", SessionActivateParams, SessionActivateResult, "Attach the frontend to a live session without closing the previously focused one.")

var SessionListParams = pydantic.Extend(ProfileParams, "SessionListParams", ParamsConfig,
	pydantic.Def("title", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("limit", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("include_hidden", pydantic.Bool, false),
)

var SessionListRow = pydantic.NewModel("SessionListRow", ClosedConfig,
	pydantic.Req("id", pydantic.Str),
	pydantic.Def("resolved_id", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("title", pydantic.Str, ""),
	pydantic.Def("preview", pydantic.Str, ""),
	pydantic.Def("started_at", pydantic.Float, 0),
	pydantic.Def("message_count", pydantic.Int, 0),
	pydantic.Def("live_message_count", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("source", pydantic.Str, ""),
	pydantic.Def("lineage_root_id", pydantic.Nullable(pydantic.Str), nil).Alias("_lineage_root_id"),
).Described("``methods_session._session_row_summary``; ``resolved_id`` only on a title lookup that followed a\ncompression lineage to its tip.")

var SessionListResult = pydantic.NewModel("SessionListResult", ClosedConfig,
	pydantic.Req("sessions", pydantic.List(SessionListRow)),
)

var _ = Method("session.list", SessionListParams, SessionListResult, "Human-facing stored sessions, most recent first (sub-agent / kanban sources denied).")

var SessionMostRecentParams = pydantic.Extend(ProfileParams, "SessionMostRecentParams", ParamsConfig)

var SessionMostRecentResult = pydantic.NewModel("SessionMostRecentResult", ClosedConfig,
	pydantic.Req("session_id", pydantic.Nullable(pydantic.Str)),
	pydantic.Def("title", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("started_at", pydantic.Nullable(pydantic.Float), nil),
	pydantic.Def("source", pydantic.Nullable(pydantic.Str), nil),
)

var _ = Method("session.most_recent", SessionMostRecentParams, SessionMostRecentResult, "Most recent human-facing session; errors fold into a null session_id.")

var SessionActiveListParams = pydantic.Extend(ProfileParams, "SessionActiveListParams", ParamsConfig,
	pydantic.Def("current_session_id", pydantic.Nullable(pydantic.Str), nil),
)

var SessionActiveItem = pydantic.NewModel("SessionActiveItem", ClosedConfig,
	pydantic.Req("current", pydantic.Bool),
	pydantic.Req("id", pydantic.Str),
	pydantic.Req("last_active", pydantic.Float),
	pydantic.Req("message_count", pydantic.Int),
	pydantic.Req("model", pydantic.Str),
	pydantic.Req("preview", pydantic.Str),
	pydantic.Req("session_key", pydantic.Str),
	pydantic.Def("source", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Req("started_at", pydantic.Float),
	pydantic.Req("status", LiveSessionStatus),
	pydantic.Req("title", pydantic.Str),
).Described("``server._session_live_item``.")

var SessionActiveListResult = pydantic.NewModel("SessionActiveListResult", ClosedConfig,
	pydantic.Req("sessions", pydantic.List(SessionActiveItem)),
)

var _ = Method("session.active_list", SessionActiveListParams, SessionActiveListResult, "Live sessions in this process, insertion order (not a DB browser).")

var SessionDeleteParams = pydantic.Extend(SessionParams, "SessionDeleteParams", ParamsConfig).Described("``session_id`` is the STORED id.")

var SessionDeleteResult = pydantic.NewModel("SessionDeleteResult", ClosedConfig,
	pydantic.Req("deleted", pydantic.Str),
)

var _ = Method("session.delete", SessionDeleteParams, SessionDeleteResult, "Delete a stored session + transcripts; refused while it is live here.")

var SessionTitleParams = pydantic.Extend(SessionParams, "SessionTitleParams", ParamsConfig,
	pydantic.Def("title", pydantic.Nullable(pydantic.Str), nil),
)

var SessionTitleResult = pydantic.NewModel("SessionTitleResult", ClosedConfig,
	pydantic.Req("title", pydantic.Str),
	pydantic.Def("session_key", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("pending", pydantic.Nullable(pydantic.Bool), nil),
)

var _ = Method("session.title", SessionTitleParams, SessionTitleResult, "Read or set a live session's title; a title set before the row exists is queued.")

var SessionSetHiddenParams = pydantic.NewModel("SessionSetHiddenParams", ParamsConfig,
	pydantic.Req("session_id", pydantic.Str),
	pydantic.Req("hidden", pydantic.Bool),
	pydantic.Def("profile", pydantic.Nullable(pydantic.Str), nil),
).Described("``session_id`` is a live runtime id first, else a stored id / key / title.")

var SessionSetHiddenResult = pydantic.NewModel("SessionSetHiddenResult", ClosedConfig,
	pydantic.Req("hidden", pydantic.Bool),
	pydantic.Req("session_key", pydantic.Str),
)

var _ = Method("session.set_hidden", SessionSetHiddenParams, SessionSetHiddenResult, "Set/clear hidden (out of the default list, still resumable by its owner) on a session + lineage.")

var SessionArchiveParams = pydantic.NewModel("SessionArchiveParams", ParamsConfig,
	pydantic.Def("session_id", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("session_key", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("archived", pydantic.Bool, true),
	pydantic.Def("profile", pydantic.Nullable(pydantic.Str), nil),
).Described("``session_id`` (or its ``session_key`` alias) is a live runtime id first, else a stored id / key / title.")

var SessionArchiveResult = pydantic.NewModel("SessionArchiveResult", ClosedConfig,
	pydantic.Req("archived", pydantic.Bool),
	pydantic.Req("session_key", pydantic.Str),
)

var _ = Method("session.archive", SessionArchiveParams, SessionArchiveResult, "Set/clear archived (soft-hide, messages kept) on a session + lineage; Desktop PATCH parity.")

var SessionWorkspaceMoveParams = pydantic.Extend(ProfileParams, "SessionWorkspaceMoveParams", ParamsConfig,
	pydantic.Req("session_key", pydantic.Str),
	pydantic.Req("cwd", pydantic.Str),
)

var SessionWorkspaceMoveResult = pydantic.NewModel("SessionWorkspaceMoveResult", ClosedConfig,
	pydantic.Req("cwd", pydantic.Str),
	pydantic.Def("branch", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("git_repo_root", pydantic.Nullable(pydantic.Str), nil),
)

var _ = Method("session.workspace.move", SessionWorkspaceMoveParams, SessionWorkspaceMoveResult, "Re-home a stored session's workspace; git identity is replaced and a live agent follows.")

var SessionCwdSetParams = pydantic.Extend(SessionParams, "SessionCwdSetParams", ParamsConfig,
	pydantic.Req("cwd", pydantic.Str),
)

var SessionCwdSetResult = pydantic.Extend(SessionLiveInfo, "SessionCwdSetResult", OpenConfig).Described("The refreshed ``session.info`` view (full agent view, or the lazy shape).")

var _ = Method("session.cwd.set", SessionCwdSetParams, SessionCwdSetResult, "Change a live, idle session's working directory.")

var SessionCloseParams = pydantic.Extend(SessionParams, "SessionCloseParams", ParamsConfig)

var SessionCloseResult = pydantic.NewModel("SessionCloseResult", ClosedConfig,
	pydantic.Req("closed", pydantic.Bool),
	pydantic.Fac("messages", pydantic.List(pydantic.Str), pydantic.NewList),
)

var _ = Method("session.close", SessionCloseParams, SessionCloseResult, "Tear down a live session (its stored row stays resumable).")

var SessionBranchParams = pydantic.Extend(SessionParams, "SessionBranchParams", ParamsConfig,
	pydantic.Def("name", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("count", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("idempotency_key", pydantic.Nullable(pydantic.Str), nil),
)

var SessionBranchResult = pydantic.NewModel("SessionBranchResult", ClosedConfig,
	pydantic.Req("session_id", pydantic.Str),
	pydantic.Req("stored_session_id", pydantic.Str),
	pydantic.Req("title", pydantic.Str),
	pydantic.Req("parent", pydantic.Str),
	pydantic.Req("message_count", pydantic.Int),
	pydantic.Req("messages", pydantic.List(TranscriptMessage)),
	pydantic.Req("info", SessionLiveInfo),
)

var _ = Method("session.branch", SessionBranchParams, SessionBranchResult, "Fork a live session into a new stored child that shares the parent's history so far.")

var SessionBranchWholeParams = pydantic.Extend(SessionParams, "SessionBranchWholeParams", ParamsConfig,
	pydantic.Def("name", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("idempotency_key", pydantic.Nullable(pydantic.Str), nil),
)

var SessionBranchWholeResult = pydantic.NewModel("SessionBranchWholeResult", ClosedConfig,
	pydantic.Req("session_id", pydantic.Str),
	pydantic.Req("stored_session_id", pydantic.Str),
	pydantic.Req("title", pydantic.Str),
	pydantic.Req("parent", pydantic.Str),
	pydantic.Req("message_count", pydantic.Int),
	pydantic.Req("messages_omitted", pydantic.Bool),
	pydantic.Req("info", SessionLiveInfo),
)

var _ = Method("session.branch_whole", SessionBranchWholeParams, SessionBranchWholeResult, "session.branch of the whole history without echoing the copied transcript back.")

var UndoIntent = pydantic.Enum("UndoIntent", "RETRY", "retry", "UNDO", "undo")

var SessionUndoParams = pydantic.Extend(SessionParams, "SessionUndoParams", ParamsConfig,
	pydantic.Def("intent", pydantic.Nullable(UndoIntent), nil),
)

var SessionUndoResult = pydantic.NewModel("SessionUndoResult", ClosedConfig,
	pydantic.Req("removed", pydantic.Int),
)

var _ = Method("session.undo", SessionUndoParams, SessionUndoResult, "Drop the last user turn (and everything after it) from an idle session.")

var SessionSaveParams = pydantic.Extend(SessionParams, "SessionSaveParams", ParamsConfig)

var SessionSaveResult = pydantic.NewModel("SessionSaveResult", OpenConfig,
	pydantic.Def("file", pydantic.Nullable(pydantic.Str), nil),
).Described("Under turn isolation the compute host's result passes through verbatim.")

var _ = Method("session.save", SessionSaveParams, SessionSaveResult, "Export the transcript to ~/.icebath/sessions/saved (classic /save).")

var SessionStatusParams = pydantic.Extend(SessionParams, "SessionStatusParams", ParamsConfig)

var SessionStatusResult = pydantic.NewModel("SessionStatusResult", ClosedConfig,
	pydantic.Req("output", pydantic.Str),
)

var _ = Method("session.status", SessionStatusParams, SessionStatusResult, "Rendered /status text for the session.")

var SessionHistoryParams = pydantic.Extend(SessionParams, "SessionHistoryParams", ParamsConfig)

var SessionHistoryResult = pydantic.NewModel("SessionHistoryResult", ClosedConfig,
	pydantic.Req("count", pydantic.Int),
	pydantic.Req("messages", pydantic.List(TranscriptMessage)),
)

var _ = Method("session.history", SessionHistoryParams, SessionHistoryResult, "The durable display transcript (ancestors included, row ids attached).")

var SessionUsageParams = pydantic.Extend(SessionParams, "SessionUsageParams", ParamsConfig)

var SessionUsageResult = pydantic.Extend(Usage, "SessionUsageResult", OpenConfig,
	pydantic.Def("credits_lines", pydantic.Nullable(pydantic.List(pydantic.Str)), nil),
	pydantic.Def("credits_usage_window", pydantic.Nullable(pydantic.Bool), nil),
)

var _ = Method("session.usage", SessionUsageParams, SessionUsageResult, "Token / context / cost counters for the session (+ Nous credit lines when available).")

var SessionContextBreakdownParams = pydantic.Extend(SessionParams, "SessionContextBreakdownParams", ParamsConfig)

var ContextCategory = pydantic.NewModel("ContextCategory", ClosedConfig,
	pydantic.Req("color", pydantic.Str),
	pydantic.Req("id", pydantic.Str),
	pydantic.Req("label", pydantic.Str),
	pydantic.Req("tokens", pydantic.Int),
)

var ContextFileSource = pydantic.NewModel("ContextFileSource", ClosedConfig,
	pydantic.Req("label", pydantic.Str),
	pydantic.Req("path", pydantic.Str),
	pydantic.Req("chars", pydantic.Int),
	pydantic.Req("est_tokens", pydantic.Int),
	pydantic.Req("loaded", pydantic.Bool),
	pydantic.Req("status", pydantic.Str),
).Described("One row of ``agent.context_file_sources.list_context_file_sources``.")

var SessionContextBreakdownResult = pydantic.NewModel("SessionContextBreakdownResult", ClosedConfig,
	pydantic.Req("categories", pydantic.List(ContextCategory)),
	pydantic.Req("context_max", pydantic.Int),
	pydantic.Req("context_percent", pydantic.Int),
	pydantic.Req("context_used", pydantic.Int),
	pydantic.Req("estimated_total", pydantic.Int),
	pydantic.Req("context_estimated", pydantic.Bool),
	pydantic.Req("context_source", pydantic.Str),
	pydantic.Req("model", pydantic.Str),
	pydantic.Def("context_files", pydantic.List(ContextFileSource), []any{}),
).Described("``agent.context_breakdown.compute_session_context_breakdown`` (empty categories before the agent builds)\nplus the per-file context manifest (empty until the agent exists).")

var _ = Method("session.context_breakdown", SessionContextBreakdownParams, SessionContextBreakdownResult, "Cursor-style split of the context window by category.")

var CompressionSummary = pydantic.NewModel("CompressionSummary", OpenConfig,
	pydantic.Def("noop", pydantic.Bool, false),
	pydantic.Def("aborted", pydantic.Bool, false),
	pydantic.Def("refused_would_grow", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("fallback_used", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("headline", pydantic.Str, ""),
	pydantic.Def("token_line", pydantic.Str, ""),
	pydantic.Def("note", pydantic.Nullable(pydantic.Str), nil),
).Described("``agent.manual_compression_feedback.summarize_manual_compression``.")

var SessionCompressParams = pydantic.Extend(SessionParams, "SessionCompressParams", ParamsConfig,
	pydantic.Def("focus_topic", pydantic.Nullable(pydantic.Str), nil),
)

var SessionCompressResult = pydantic.NewModel("SessionCompressResult", OpenConfig,
	pydantic.Def("status", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("removed", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("before_messages", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("after_messages", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("before_tokens", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("after_tokens", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("summary", pydantic.Nullable(CompressionSummary), nil),
	pydantic.Def("usage", pydantic.Nullable(Usage), nil),
	pydantic.Def("info", pydantic.Nullable(SessionLiveInfo), nil),
	pydantic.Def("messages", pydantic.Nullable(pydantic.List(TranscriptMessage)), nil),
	pydantic.Def("compressed", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("lock_held", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("message", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("turn_isolation", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("host_ack", pydantic.Nullable(pydantic.Dict(pydantic.Any)), nil),
).Described("In-process: the before/after summary + replacement transcript. Compute host: its result passes\nthrough (hence open) with ``turn_isolation``; a lock held elsewhere answers ``compressed: false``.")

var _ = Method("session.compress", SessionCompressParams, SessionCompressResult, "Manual /compress of an idle session, optionally focused on a topic.")

var SessionInterruptParams = pydantic.Extend(SessionParams, "SessionInterruptParams", ParamsConfig,
	pydantic.Def("expected_hosted_task_id", pydantic.Nullable(pydantic.Str), nil),
)

var InterruptStatus = pydantic.Enum("InterruptStatus", "interrupted", "interrupted", "not_interrupted", "not_interrupted")

var SessionInterruptResult = pydantic.NewModel("SessionInterruptResult", ClosedConfig,
	pydantic.Req("status", InterruptStatus),
	pydantic.Def("interrupted", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("turn_isolation", pydantic.Nullable(pydantic.Bool), nil),
)

var _ = Method("session.interrupt", SessionInterruptParams, SessionInterruptResult, "Stop the running turn (and streaming TTS); retires the crash-recovery marker.")

var CorrectionStatus = pydantic.Enum("CorrectionStatus", "queued", "queued", "redirected", "redirected", "rejected", "rejected")

var SessionCorrectionParams = pydantic.Extend(SessionParams, "SessionCorrectionParams", ParamsConfig,
	pydantic.Req("text", pydantic.Str),
)

var SessionCorrectionResult = pydantic.NewModel("SessionCorrectionResult", ClosedConfig,
	pydantic.Req("status", CorrectionStatus),
	pydantic.Req("text", pydantic.Str),
)

var _ = Method("session.steer", SessionCorrectionParams, SessionCorrectionResult, "Inject text into the next tool result without interrupting the turn.")

var _ = Method("session.redirect", SessionCorrectionParams, SessionCorrectionResult, "Redirect the active turn (queued for the next turn while the agent is still building).")

var SpawnTreeSaveParams = pydantic.Extend(ProfileParams, "SpawnTreeSaveParams", ParamsConfig,
	pydantic.Req("subagents", pydantic.List(pydantic.Dict(pydantic.Any))),
	pydantic.Def("session_id", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("started_at", pydantic.Nullable(pydantic.Float), nil),
	pydantic.Def("finished_at", pydantic.Nullable(pydantic.Float), nil),
	pydantic.Def("label", pydantic.Nullable(pydantic.Str), nil),
)

var SpawnTreeSaveResult = pydantic.NewModel("SpawnTreeSaveResult", ClosedConfig,
	pydantic.Req("path", pydantic.Str),
	pydantic.Req("session_id", pydantic.Str),
)

var _ = Method("spawn_tree.save", SpawnTreeSaveParams, SpawnTreeSaveResult, "Persist a finished delegation tree snapshot under the session's spawn-trees dir.")

var SpawnTreeListParams = pydantic.Extend(ProfileParams, "SpawnTreeListParams", ParamsConfig,
	pydantic.Def("session_id", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("cross_session", pydantic.Bool, false),
	pydantic.Def("limit", pydantic.Nullable(pydantic.Int), nil),
)

var SpawnTreeEntry = pydantic.NewModel("SpawnTreeEntry", OpenConfig,
	pydantic.Req("path", pydantic.Str),
	pydantic.Def("session_id", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("started_at", pydantic.Nullable(pydantic.Float), nil),
	pydantic.Def("finished_at", pydantic.Nullable(pydantic.Float), nil),
	pydantic.Def("label", pydantic.Str, ""),
	pydantic.Def("count", pydantic.Int, 0),
).Described("Index row (``server._append_spawn_tree_index``) or a legacy file scan.")

var SpawnTreeListResult = pydantic.NewModel("SpawnTreeListResult", ClosedConfig,
	pydantic.Req("entries", pydantic.List(SpawnTreeEntry)),
)

var _ = Method("spawn_tree.list", SpawnTreeListParams, SpawnTreeListResult, "Saved spawn-tree snapshots, newest first.")

var SpawnTreeLoadParams = pydantic.Extend(ProfileParams, "SpawnTreeLoadParams", ParamsConfig,
	pydantic.Req("path", pydantic.Str),
)

var SpawnTreeLoadResult = pydantic.NewModel("SpawnTreeLoadResult", OpenConfig,
	pydantic.Def("session_id", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("started_at", pydantic.Nullable(pydantic.Float), nil),
	pydantic.Def("finished_at", pydantic.Nullable(pydantic.Float), nil),
	pydantic.Def("label", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Fac("subagents", pydantic.List(pydantic.Dict(pydantic.Any)), pydantic.NewList),
).Described("The snapshot file as written by ``spawn_tree.save`` (open: the file is the contract).")

var _ = Method("spawn_tree.load", SpawnTreeLoadParams, SpawnTreeLoadResult, "Read one saved spawn-tree snapshot (path must be under the spawn-trees root).")

var TerminalResizeParams = pydantic.Extend(SessionParams, "TerminalResizeParams", ParamsConfig,
	pydantic.Def("cols", pydantic.Nullable(pydantic.Int), nil),
)

var TerminalResizeResult = pydantic.NewModel("TerminalResizeResult", ClosedConfig,
	pydantic.Req("cols", pydantic.Int),
)

var _ = Method("terminal.resize", TerminalResizeParams, TerminalResizeResult, "Record the client's column width for server-side rendering.")

var SessionEventsSinceParams = pydantic.Extend(SessionParams, "SessionEventsSinceParams", ParamsConfig,
	pydantic.Def("last_seen", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("replay_epoch", pydantic.Nullable(pydantic.Str), nil),
)

var SessionEventsSinceResult = pydantic.NewModel("SessionEventsSinceResult", ClosedConfig,
	pydantic.Req("events", pydantic.List(pydantic.Dict(pydantic.Any))),
	pydantic.Req("latest_seq", pydantic.Int),
	pydantic.Req("truncated", pydantic.Bool),
	pydantic.Req("count", pydantic.Int),
	pydantic.Req("epoch", pydantic.Str),
	pydantic.Req("open_requests", pydantic.List(OpenRequestEntry)),
)

var _ = Method("session.events.since", SessionEventsSinceParams, SessionEventsSinceResult, "Replay events after a seq watermark on WS reconnect; truncated means refetch state.")

var SessionEventsStatsParams = pydantic.Extend(ProfileParams, "SessionEventsStatsParams", ParamsConfig)

var SessionEventsStatsResult = pydantic.NewModel("SessionEventsStatsResult", ClosedConfig,
	pydantic.Req("sessions", pydantic.Int),
	pydantic.Req("events", pydantic.Int),
	pydantic.Req("bytes", pydantic.Int),
	pydantic.Req("max_per_session", pydantic.Int),
	pydantic.Req("max_bytes_per_session", pydantic.Int),
	pydantic.Req("max_bytes_process", pydantic.Int),
).Described("``event_replay.replay_stats``.")

var _ = Method("session.events.stats", SessionEventsStatsParams, SessionEventsStatsResult, "Replay-buffer occupancy telemetry (ops/debug).")

var LlmOneshotParams = pydantic.Extend(ProfileParams, "LlmOneshotParams", ParamsConfig,
	pydantic.Def("template", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("instructions", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("input", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("variables", pydantic.Nullable(pydantic.Dict(pydantic.Any)), nil),
	pydantic.Def("task", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("temperature", pydantic.Nullable(pydantic.Float), nil),
	pydantic.Def("max_tokens", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("session_id", pydantic.Nullable(pydantic.Str), nil),
).Described("Needs a ``template`` or ``instructions`` / ``input``; a live ``session_id`` lends its model.")

var LlmOneshotResult = pydantic.NewModel("LlmOneshotResult", ClosedConfig,
	pydantic.Req("text", pydantic.Str),
)

var _ = Method("llm.oneshot", LlmOneshotParams, LlmOneshotResult, "Stateless one-shot LLM completion (titles, ideas) on the session's or the task backend.")
