package tuigateway

import (
	"github.com/nickheyer/icebath/pkg/internal/pydantic"
)

var OpenPayload = pydantic.NewModel("OpenPayload", OpenConfig)

var SkinPayloadModel = pydantic.NewModel("SkinPayload", OpenConfig,
	pydantic.Def("name", pydantic.Str, ""),
	pydantic.Def("description", pydantic.Str, ""),
	pydantic.Fac("colors", pydantic.Dict(pydantic.Str), pydantic.NewDict),
	pydantic.Fac("light_colors", pydantic.Dict(pydantic.Str), pydantic.NewDict),
	pydantic.Fac("dark_colors", pydantic.Dict(pydantic.Str), pydantic.NewDict),
	pydantic.Fac("branding", pydantic.Dict(pydantic.Str), pydantic.NewDict),
	pydantic.Def("banner_logo", pydantic.Str, ""),
	pydantic.Def("banner_hero", pydantic.Str, ""),
	pydantic.Def("tool_prefix", pydantic.Str, ""),
	pydantic.Def("help_header", pydantic.Str, ""),
).Described("``tui_gateway/change_watcher.py::resolve_skin`` — the resolved active skin (``HermesSkin``).\n``{}`` when the skin engine failed to load. Colour maps are token → colour string.")

var GatewayReadyPayload = pydantic.NewModel("GatewayReadyPayload", ClosedConfig,
	pydantic.Req("skin", SkinPayloadModel),
	pydantic.Req("change_events", pydantic.Bool),
	pydantic.Req("replay_epoch", pydantic.Str),
	pydantic.Def("heartbeat", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("session_authority", pydantic.Nullable(pydantic.Bool), nil),
).Described("``tui_gateway/entry.py`` (stdio) / ``tui_gateway/ws.py`` (WebSocket) first frame.")

var _ = Event("gateway.ready", GatewayReadyPayload, "First frame of a connection: the resolved skin, the change-event capability and the replay epoch.")

var _ = Event("skin.changed", SkinPayloadModel, "The active skin moved (name switch or live colour edit); repaint from this palette.")

var SetupReadyPayload = pydantic.NewModel("SetupReadyPayload", OpenConfig,
	pydantic.Req("provider_configured", pydantic.Bool),
	pydantic.Req("inference_provider", pydantic.Str),
	pydantic.Req("free_tier_account", pydantic.Bool),
	pydantic.Req("free_tier_route", pydantic.Bool),
	pydantic.Req("has_identity", pydantic.Bool),
	pydantic.Req("other_providers", pydantic.Bool),
	pydantic.Def("error", pydantic.Str, ""),
	pydantic.Def("error_code", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("retryable", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("retry_after", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Req("finished_at", pydantic.Float),
).Described("``hermes_cli/free_tier_bootstrap.py::SetupRecord.as_payload``.")

var OnboardingOutcome = pydantic.Enum("OnboardingOutcome", "done", "done", "skipped", "skipped", "replaced", "replaced", "deleted", "deleted")

var OnboardingEndedPayload = pydantic.NewModel("OnboardingEndedPayload", ClosedConfig,
	pydantic.Req("run_id", pydantic.Str),
	pydantic.Req("stored_session_id", pydantic.Str),
	pydantic.Req("outcome", OnboardingOutcome),
).Described("``agent/onboarding_record.py::_transition``: the run ``run_id`` stopped being in progress.\n``stored_session_id`` is its chat at the compression tip. ``done``: the first-task card was answered;\n``skipped``: Skip setup; ``replaced``: a rerun opened a new run; ``deleted``: its chat was deleted.")

var _ = Event("onboarding.ended", OnboardingEndedPayload, "A first-run onboarding run ended; sent to every connected window.")

var _ = Event("setup.ready", SetupReadyPayload, "The free-tier bootstrap finished (broadcast); the desktop's setup gate reads the record.")

var _ = Event("free_tier.challenge", FreeTierChallengePayload, "The account service wants a browser challenge cleared before the free-tier token exchange (broadcast); the desktop loads ``url`` in a hidden window.")

var ErrorPayload = pydantic.NewModel("ErrorPayload", ClosedConfig,
	pydantic.Req("message", pydantic.Str),
).Described("Every ``_emit(\"error\", …)`` site sets exactly ``message``.")

var _ = Event("error", ErrorPayload, "A session-level failure outside a turn (agent init, model switch, compression, resume).")

var NoticePayload = pydantic.NewModel("NoticePayload", ClosedConfig,
	pydantic.Req("message", pydantic.Str),
).Described("``tui_gateway/model_switch.py`` capability-refresh notice.")

var _ = Event("notice", NoticePayload, "Informational one-liner for the session (capabilities refreshed).")

var MessageStartPayload = pydantic.NewModel("MessageStartPayload", ClosedConfig,
	pydantic.Def("display_kind", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("display_text", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("notification_category", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("text", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("turn_id", pydantic.Nullable(pydantic.Str), nil),
).Described("``message.start``. Empty from the TUI gateway's emitters and for an ordinary admission;\n``gateway/session_authority._stamp_claim`` adds an automation admission's async-result card\n(``session_automation.admission_display``: a background-process completion) and its model\ntext, so a viewer that did not submit the turn paints the same header the submitter would.")

var _ = Event("message.start", MessageStartPayload, "A turn began streaming; an automation turn carries its async-result card.")

var StreamDeltaPayload = pydantic.NewModel("StreamDeltaPayload", ClosedConfig,
	pydantic.Req("text", pydantic.Str),
	pydantic.Def("rendered", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("verbose", pydantic.Nullable(pydantic.Bool), nil),
).Described("``prompt_turn._invoke_agent._stream`` (message.delta: ``text`` + optional ``rendered``),\n``agent_callbacks._agent_cbs`` (reasoning.delta / thinking.delta), ``tool_progress._progress_reasoning``\n(reasoning.available). ``verbose`` rides only when the session's verbose reasoning mode is on.")

var MessageDeltaPayload = pydantic.Extend(StreamDeltaPayload, "MessageDeltaPayload", ClosedConfig,
	pydantic.Req("text", pydantic.Nullable(pydantic.Str)),
	pydantic.Def("turn_id", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("segment", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("offset", pydantic.Nullable(pydantic.Int), nil),
).Described("``message.delta``. A session-authority local-viewer turn also publishes ``text: null``: the\nreply segment boundary (the agent's ``stream_delta_callback(None)``, before tool lines) that\ncloses the classic CLI's reply box. Every other viewer ignores an empty chunk.\n\nA prompt turn stamps ``turn_id`` + ``segment`` (interim seals before it) + ``offset`` (UTF-16 code\nunits of that segment already sent): a client drops a re-sent chunk it already applied.")

var _ = Event("message.delta", MessageDeltaPayload, "One streamed chunk of the assistant reply (null text: segment boundary).")

var _ = Event("reasoning.delta", StreamDeltaPayload, "One streamed chunk of the model's reasoning.")

var _ = Event("reasoning.available", StreamDeltaPayload, "A completed reasoning block (non-streaming providers).")

var _ = Event("thinking.delta", StreamDeltaPayload, "Legacy thinking-text chunk (thinking_callback).")

var MessageInterimPayload = pydantic.NewModel("MessageInterimPayload", ClosedConfig,
	pydantic.Req("text", pydantic.Str),
	pydantic.Req("already_streamed", pydantic.Bool),
	pydantic.Def("turn_id", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("segment", pydantic.Nullable(pydantic.Int), nil),
).Described("``prompt_turn._interim_assistant_cb`` / ``agent_callbacks`` interim_assistant_callback.")

var _ = Event("message.interim", MessageInterimPayload, "Interim assistant commentary (text beside tool calls) sealed as its own segment.")

var TurnStatus = pydantic.Enum("TurnStatus", "complete", "complete", "error", "error", "interrupted", "interrupted").Described("``prompt_turn._result_status``.")

var UsageLimitSuggestion = pydantic.NewModel("UsageLimitSuggestion", ClosedConfig,
	pydantic.Req("model", pydantic.Str),
	pydantic.Req("label", pydantic.Str),
	pydantic.Req("note", pydantic.Str),
	pydantic.Def("provider", pydantic.Str, "nous"),
).Described("``agent/plan_limits.py::ModelSuggestion`` — a recommended model that still works past the limit.")

var UsageLimitCredits = pydantic.NewModel("UsageLimitCredits", ClosedConfig,
	pydantic.Req("action", pydantic.Str),
).Described("``agent/plan_limits.py::CreditsOption`` — ``use_credits`` (turn on Keep going), ``add_credits``, or\n``add_credits_and_keep_going`` (both: Keep going is off and there is no balance).")

var UsageLimit = pydantic.NewModel("UsageLimit", ClosedConfig,
	pydantic.Req("gauge_id", pydantic.Str),
	pydantic.Req("scope", pydantic.Str),
	pydantic.Req("limit_name", pydantic.Str),
	pydantic.Def("resets_at_ms", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Req("message", pydantic.Str),
	pydantic.Fac("suggestions", pydantic.List(UsageLimitSuggestion), pydantic.NewList),
	pydantic.Def("credits", pydantic.Nullable(UsageLimitCredits), nil),
	pydantic.Def("current_model", pydantic.Str, ""),
).Described("``agent/plan_limits.py::LimitHit`` — a usage-window plan's limit: what ran out, when it resets, and\nthe one-click ways on (switch to a recommended model, use / add credits).")

var ErrorSurface = pydantic.NewModel("ErrorSurface", OpenConfig,
	pydantic.Req("layer", pydantic.Str),
	pydantic.Req("code", pydantic.Str),
	pydantic.Req("retryable", pydantic.Bool),
	pydantic.Def("provider", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("model", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("resets_at", pydantic.Nullable(pydantic.Float), nil),
	pydantic.Def("usage_limit", pydantic.Nullable(UsageLimit), nil),
).Described("``agent/error_surface.py::_surface`` — advisory {layer, code, retryable} (+ identity, + auth hint,\n+ ``resets_at`` epoch seconds when the provider named when its limit lifts, + ``usage_limit`` on a\nusage-window plan's limit).")

var BillingBlock = pydantic.NewModel("BillingBlock", ClosedConfig,
	pydantic.Req("provider", pydantic.Str),
	pydantic.Req("provider_label", pydantic.Str),
	pydantic.Req("model", pydantic.Str),
	pydantic.Req("billing_url", pydantic.Nullable(pydantic.Str)),
	pydantic.Req("is_nous", pydantic.Bool),
	pydantic.Req("message", pydantic.Str),
	pydantic.Def("unverified", pydantic.Nullable(pydantic.Bool), nil),
).Described("``agent/billing_links.py::BillingBlock.to_dict`` (+ ``unverified`` from conversation_loop).")

var PersistedTurn = pydantic.NewModel("PersistedTurn", ClosedConfig,
	pydantic.Req("row_ids", pydantic.List(pydantic.Int)),
	pydantic.Req("complete", pydantic.Bool),
	pydantic.Def("user_row_id", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("final_assistant_row_id", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("user_row_ids", pydantic.Nullable(pydantic.List(pydantic.Int)), nil),
	pydantic.Def("submission_id", pydantic.Nullable(pydantic.Str), nil),
).Described("Committed SQLite row addresses for the agent's current-turn suffix. Missing ids are\nunproven, never negative acknowledgements. ``complete`` permits retiring the whole local\nturn only when the original turn boundary, every row and final body are still accounted\nfor; compaction, redirects and partial writes conservatively leave it false. Row ids are\nscoped to the owning profile's store, as in ``SessionMessage.row_id``.")

var MessageCompletePayload = pydantic.NewModel("MessageCompletePayload", ClosedConfig,
	pydantic.Def("text", pydantic.Union(pydantic.Str, pydantic.Any), ""),
	pydantic.Def("usage", pydantic.Nullable(Usage), nil),
	pydantic.Def("status", pydantic.Nullable(TurnStatus), nil),
	pydantic.Def("reasoning", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("warning", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("response_previewed", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("response_reused", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("response_transformed", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("billing", pydantic.Nullable(BillingBlock), nil),
	pydantic.Def("failure_reason", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("rendered", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("error", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("recoverable", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("error_surface", pydantic.Nullable(ErrorSurface), nil),
	pydantic.Def("partial", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("persisted_turn", pydantic.Nullable(PersistedTurn), nil),
).Described("``prompt_turn._complete_turn_payload`` / ``session_auto_continue._emit_terminal_turn_error`` /\n``agent_callbacks._mirror_subagent_to_child`` (child watch mirror: ``text`` only) /\n``compute_host_bridge`` (``text`` + ``status``).")

var _ = Event("message.complete", MessageCompletePayload, "The turn ended: final text, usage and outcome.")

var StatusUpdatePayload = pydantic.NewModel("StatusUpdatePayload", ClosedConfig,
	pydantic.Req("kind", pydantic.Str),
	pydantic.Req("text", pydantic.Str),
).Described("``server._status_update`` and the direct emitters (goal / loop / heartbeat / process).")

var _ = Event("status.update", StatusUpdatePayload, "Transient status line (kind: status, lifecycle, compacting, goal, loop, heartbeat, process, …).")

var SessionUsagePayload = pydantic.NewModel("SessionUsagePayload", ClosedConfig,
	pydantic.Def("usage", pydantic.Nullable(Usage), nil),
	pydantic.Def("input_tokens", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("output_tokens", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("cache_read_tokens", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("cache_write_tokens", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("prompt_tokens", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("completion_tokens", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("total_tokens", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("reasoning_tokens", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("api_calls", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("last_prompt_tokens", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("context_tokens", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("context_length", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("config_context_length", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("context_source", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("compression_count", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("avg_latency_s", pydantic.Nullable(pydantic.Float), nil),
	pydantic.Def("tokens_per_s", pydantic.Nullable(pydantic.Float), nil),
	pydantic.Def("last_turn_tokens", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("rate_limit", pydantic.Nullable(pydantic.Dict(pydantic.Any)), nil),
	pydantic.Def("active_subagents", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("model", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("provider", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("fallback_model", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("plan_gauge", pydantic.Nullable(PlanGauge), nil),
).Described("``usage``: the status-bar view (``server._get_usage``), sent by the embedded backend's ticker\n(``server._start_usage_ticker``) and by a canonical local viewer's gateway after every API call\nand at turn end (``gateway/session_usage.py::usage_frame``). Read this.\n\nDeprecated: the top-level session-cumulative counters (``usage_stats``, the ``session.stats``\ndict) that the gateway frame also carries. They are kept for existing readers; new readers use\n``usage``, or ``session.stats`` for the raw counters. The embedded ticker never sends them.")

var _ = Event("session.usage", SessionUsagePayload, "Mid-turn usage tick; message.complete carries the authoritative final usage.")

var SessionTitlePayload = pydantic.NewModel("SessionTitlePayload", ClosedConfig,
	pydantic.Req("session_id", pydantic.Str),
	pydantic.Req("title", pydantic.Str),
).Described("``prompt_turn._invoke_agent`` ``_on_session_title`` hook.")

var _ = Event("session.title", SessionTitlePayload, "Auto-titling renamed the session (``session_id`` is the stored key).")

var ReactionPayload = pydantic.NewModel("ReactionPayload", ClosedConfig,
	pydantic.Req("kind", pydantic.Str),
).Described("``agent_callbacks`` reaction_callback.")

var _ = Event("reaction", ReactionPayload, "Affection reaction detected in the user's message (hearts etc.).")

var ReviewSummaryPayload = pydantic.NewModel("ReviewSummaryPayload", ClosedConfig,
	pydantic.Req("text", pydantic.Str),
).Described("``server`` background_review_callback.")

var _ = Event("review.summary", ReviewSummaryPayload, "Background review of the last turn finished.")

var AgentPrintPayload = pydantic.NewModel("AgentPrintPayload", ClosedConfig,
	pydantic.Req("text", pydantic.Str),
).Described("A session-authority local-viewer turn: one line the agent printed through ``_print_fn``\n(subagent completion, compaction, errors, budget notices), which main's classic CLI rendered\nthrough ``_agent_status_print``.")

var _ = Event("agent.print", AgentPrintPayload, "A status line the agent printed during a local viewer's turn.")

var ToolStartPayload = pydantic.NewModel("ToolStartPayload", ClosedConfig,
	pydantic.Req("tool_id", pydantic.Str),
	pydantic.Req("name", pydantic.Str),
	pydantic.Def("context", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("args", pydantic.Nullable(pydantic.Dict(pydantic.Any)), nil),
	pydantic.Def("args_text", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("preview", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("labels", pydantic.Nullable(pydantic.List(ToolLabel)), nil),
).Described("``tool_progress._on_tool_start`` (+ ``agent_callbacks._mirror_subagent_to_child`` rows with\n``preview`` and empty ``args``). ``todos``/``revision`` are NOT set by the emitter; kept optional\nbecause tool.start rows may pass through connector redaction unchanged.")

var _ = Event("tool.start", ToolStartPayload, "A tool call began (stable id + full args).")

var ToolCompletePayload = pydantic.NewModel("ToolCompletePayload", ClosedConfig,
	pydantic.Req("tool_id", pydantic.Str),
	pydantic.Req("name", pydantic.Str),
	pydantic.Def("args", pydantic.Nullable(pydantic.Dict(pydantic.Any)), nil),
	pydantic.Def("duration_s", pydantic.Nullable(pydantic.Float), nil),
	pydantic.Def("result", pydantic.Any, nil),
	pydantic.Def("summary", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("result_text", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("inline_diff", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("todos", pydantic.Nullable(pydantic.List(pydantic.Any)), nil),
	pydantic.Def("revision", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("labels", pydantic.Nullable(pydantic.List(ToolLabel)), nil),
).Described("``tool_progress._on_tool_complete``; ``todos``/``revision`` merged in for the todo tools.")

var _ = Event("tool.complete", ToolCompletePayload, "A tool call finished: parsed result, summary, optional diff / todo snapshot.")

var ToolGeneratingPayload = pydantic.NewModel("ToolGeneratingPayload", ClosedConfig,
	pydantic.Req("name", pydantic.Str),
).Described("``agent_callbacks`` tool_gen_callback.")

var _ = Event("tool.generating", ToolGeneratingPayload, "The model is emitting a tool call's arguments.")

var ToolOutputRiskPayload = pydantic.NewModel("ToolOutputRiskPayload", ClosedConfig,
	pydantic.Req("tool_id", pydantic.Str),
	pydantic.Req("name", pydantic.Str),
	pydantic.Req("risk", pydantic.Str),
	pydantic.Req("findings", pydantic.List(pydantic.Str)),
	pydantic.Req("redacted", pydantic.Bool),
).Described("``tool_progress._progress_output_risk``.")

var _ = Event("tool.output_risk", ToolOutputRiskPayload, "Tool output was classified as risky (prompt-injection / secret findings).")

var ToolProgressPayload = pydantic.NewModel("ToolProgressPayload", ClosedConfig,
	pydantic.Req("tool_id", pydantic.Str),
	pydantic.Req("name", pydantic.Str),
	pydantic.Req("progress", pydantic.Float),
	pydantic.Def("total", pydantic.Nullable(pydantic.Float), nil),
	pydantic.Def("percent", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("message", pydantic.Nullable(pydantic.Str), nil),
).Described("``agent.tool_progress_relay.tool_progress_payload`` — an MCP server's\n``notifications/progress`` for a running call, throttled to one per 500 ms per call.")

var _ = Event("tool.progress", ToolProgressPayload, "Live progress of a running tool call (MCP notifications/progress).")

var TodoUpdatedPayload = pydantic.NewModel("TodoUpdatedPayload", ClosedConfig,
	pydantic.Req("todos", pydantic.List(pydantic.Any)),
	pydantic.Req("revision", pydantic.Int),
).Described("``tool_progress._normalize_todo_state`` — full task snapshot.")

var _ = Event("todo.updated", TodoUpdatedPayload, "Full todo snapshot after a todo tool ran.")

var NotificationShowPayload = pydantic.NewModel("NotificationShowPayload", ClosedConfig,
	pydantic.Req("text", pydantic.Str),
	pydantic.Req("level", pydantic.Str),
	pydantic.Req("kind", pydantic.Str),
	pydantic.Def("ttl_ms", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("key", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("id", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("detail", pydantic.Nullable(pydantic.Str), nil),
).Described("``agent/credits_tracker.py::AgentNotice`` via notice_callback, and ``server._await_agent_ready``'s\nslow-build notice. ``level``: info | warn | error | success; ``kind``: sticky | ttl | agent.\n``detail``: the full text behind a short ``text`` (the low-disk auto-prune question): Desktop shows\nit in the toast's expandable body, the Ink TUI prints it into the transcript.")

var NotificationClearPayload = pydantic.NewModel("NotificationClearPayload", ClosedConfig,
	pydantic.Req("key", pydantic.Str),
)

var _ = Event("notification.show", NotificationShowPayload, "Show / replace a keyed out-of-band notice (toast or status bar).")

var _ = Event("notification.clear", NotificationClearPayload, "Withdraw the notice with this key.")

var TipShowPayload = pydantic.NewModel("TipShowPayload", ClosedConfig,
	pydantic.Req("selector", pydantic.Str),
	pydantic.Req("text", pydantic.Str),
	pydantic.Def("title", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("side", pydantic.Nullable(pydantic.Str), nil),
).Described("``tools/tip_tool.py``.")

var _ = Event("tip.show", TipShowPayload, "Point at a desktop element with a one-line tip bubble.")

var _ = Event("session.info", SessionLiveInfo, "Live session settings snapshot (``server._session_info``); also the ``info`` of create/resume/activate.")

var ResumePhaseStatus = pydantic.Enum("ResumePhaseStatus", "loading", "loading", "complete", "complete", "failed", "failed")

var SessionResumeProgressPayload = pydantic.NewModel("SessionResumeProgressPayload", ClosedConfig,
	pydantic.Req("phase", pydantic.Str),
	pydantic.Req("status", ResumePhaseStatus),
	pydantic.Def("message_count", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("message", pydantic.Nullable(pydantic.Str), nil),
).Described("``server._hydrate_resume_history``.")

var _ = Event("session.resume_progress", SessionResumeProgressPayload, "Deferred resume hydration progress.")

var SessionReclaimedPayload = pydantic.NewModel("SessionReclaimedPayload", ClosedConfig,
	pydantic.Req("session_id", pydantic.Str),
	pydantic.Req("stored_session_id", pydantic.Str),
	pydantic.Req("reason", pydantic.Str),
).Described("``session_lifecycle._announce_session_reclaimed`` (broadcast).")

var _ = Event("session.reclaimed", SessionReclaimedPayload, "The backend reclaimed a live session out from under its clients.")

var ApprovalCancelledPayload = pydantic.NewModel("ApprovalCancelledPayload", ClosedConfig,
	pydantic.Req("session_id", pydantic.Str),
	pydantic.Req("stored_session_id", pydantic.Str),
	pydantic.Req("reason", pydantic.Str),
	pydantic.Req("cancelled_count", pydantic.Int),
	pydantic.Req("request_ids", pydantic.List(pydantic.Str)),
).Described("``session_lifecycle._announce_cancelled_gateway_approvals`` (broadcast).\n\nOne frame for every pending approval dropped by an interrupt / reap / teardown (#106678) — the\ndeny-resolve is silent without it, so a reconnecting client's prompt looks lost rather than cancelled.\n``cancelled_count`` is the number of dropped entries; ``request_ids`` omits empty/missing ids, so the\ntwo can disagree when an entry has no request_id.")

var _ = Event("approval.cancelled", ApprovalCancelledPayload, "Pending gateway approvals were dropped by interrupt/reap/teardown; the wait resolved as deny (not a user refusal).")

var SessionControlUpdatePayload = pydantic.NewModel("SessionControlUpdatePayload", ClosedConfig,
	pydantic.Req("control", SessionControlSnapshot),
)

var _ = Event("session.control.update", SessionControlUpdatePayload, "Persisted goal / loop / heartbeat state changed.")

var BillingStepUpVerificationPayload = pydantic.NewModel("BillingStepUpVerificationPayload", ClosedConfig,
	pydantic.Req("verification_url", pydantic.Str),
	pydantic.Req("user_code", pydantic.Str),
).Described("``methods_session`` billing.step_up on_verification.")

var _ = Event("billing.step_up.verification", BillingStepUpVerificationPayload, "Device-flow URL + code for the billing scope step-up; the client opens the browser.")

var SideAgentCompletePayload = pydantic.NewModel("SideAgentCompletePayload", ClosedConfig,
	pydantic.Req("task_id", pydantic.Str),
	pydantic.Req("text", pydantic.Str),
	pydantic.Def("question", pydantic.Nullable(pydantic.Str), nil),
).Described("``methods_prompt._spawn_side_agent``: ``{task_id, **extra, text}``; btw adds ``question``.")

var _ = Event("background.complete", SideAgentCompletePayload, "A /background side agent finished.")

var _ = Event("btw.complete", SideAgentCompletePayload, "A /btw side question was answered.")

var _ = Event("preview.restart.complete", SideAgentCompletePayload, "The hidden preview-restart agent finished.")

var PreviewRestartProgressPayload = pydantic.NewModel("PreviewRestartProgressPayload", ClosedConfig,
	pydantic.Req("task_id", pydantic.Str),
	pydantic.Req("text", pydantic.Str),
	pydantic.Def("level", pydantic.Nullable(pydantic.Str), nil),
).Described("``methods_prompt`` restart body + ``agent_callbacks._preview_restart_callbacks``.")

var _ = Event("preview.restart.progress", PreviewRestartProgressPayload, "Progress line from the preview-restart agent.")

var SubagentOutputTailEntry = pydantic.NewModel("SubagentOutputTailEntry", ClosedConfig,
	pydantic.Req("tool", pydantic.Str),
	pydantic.Req("preview", pydantic.Str),
	pydantic.Req("is_error", pydantic.Bool),
).Described("``tools/delegate_tool_results.py::_extract_output_tail`` row.")

var SubagentEventPayload = pydantic.NewModel("SubagentEventPayload", ClosedConfig,
	pydantic.Req("goal", pydantic.Str),
	pydantic.Req("task_count", pydantic.Int),
	pydantic.Req("task_index", pydantic.Int),
	pydantic.Def("subagent_id", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("parent_id", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("child_session_id", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("delegation_id", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("depth", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("model", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("tool_count", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("toolsets", pydantic.Nullable(pydantic.List(pydantic.Str)), nil),
	pydantic.Def("input_tokens", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("output_tokens", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("reasoning_tokens", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("api_calls", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("files_read", pydantic.Nullable(pydantic.List(pydantic.Str)), nil),
	pydantic.Def("files_written", pydantic.Nullable(pydantic.List(pydantic.Str)), nil),
	pydantic.Def("output_tail", pydantic.Nullable(pydantic.List(SubagentOutputTailEntry)), nil),
	pydantic.Def("tool_name", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("text", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("status", pydantic.Nullable(SubagentStatus), nil),
	pydantic.Def("summary", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("duration_seconds", pydantic.Nullable(pydantic.Float), nil),
	pydantic.Def("tool_preview", pydantic.Nullable(pydantic.Str), nil),
).Described("``tool_progress._progress_subagent`` — every ``subagent.*`` frame; identity fields are optional\nbecause older emitters omit them and the TUI spawn tree falls back to flat rendering.")

var _ = Event("subagent.spawn_requested", SubagentEventPayload, "delegate_task accepted a child goal (before the child starts).")

var _ = Event("subagent.start", SubagentEventPayload, "A delegated child started running.")

var _ = Event("subagent.progress", SubagentEventPayload, "Batched tool-name progress from a child.")

var _ = Event("subagent.thinking", SubagentEventPayload, "A child's reasoning chunk.")

var _ = Event("subagent.tool", SubagentEventPayload, "A child called a tool.")

var _ = Event("subagent.complete", SubagentEventPayload, "A child finished (status + observability rollup).")

var MoaReferencePayload = pydantic.NewModel("MoaReferencePayload", ClosedConfig,
	pydantic.Req("label", pydantic.Str),
	pydantic.Req("text", pydantic.Str),
	pydantic.Def("index", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("count", pydantic.Nullable(pydantic.Int), nil),
).Described("``tool_progress._progress_moa_reference``.")

var MoaAggregatingPayload = pydantic.NewModel("MoaAggregatingPayload", ClosedConfig,
	pydantic.Req("aggregator", pydantic.Str),
)

var MoaProgressPayload = pydantic.NewModel("MoaProgressPayload", ClosedConfig,
	pydantic.Req("label", pydantic.Str),
	pydantic.Req("refs_done", pydantic.Int),
	pydantic.Req("refs_total", pydantic.Int),
)

var MoaPhasePayload = pydantic.NewModel("MoaPhasePayload", ClosedConfig,
	pydantic.Req("phase", pydantic.Str),
	pydantic.Def("refs_done", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("refs_total", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("aggregator", pydantic.Nullable(pydantic.Str), nil),
)

var _ = Event("moa.reference", MoaReferencePayload, "One MoA reference model's output.")

var _ = Event("moa.aggregating", MoaAggregatingPayload, "The MoA aggregator started.")

var _ = Event("moa.progress", MoaProgressPayload, "MoA reference fan-out progress (n/total).")

var _ = Event("moa.phase", MoaPhasePayload, "MoA phase transition (currently only ``aggregator``).")

var PreviewOpenPayload = pydantic.NewModel("PreviewOpenPayload", ClosedConfig,
	pydantic.Req("url", pydantic.Str),
	pydantic.Def("label", pydantic.Str, ""),
).Described("``tools/open_preview_tool.py``.")

var PreviewClosePayload = pydantic.NewModel("PreviewClosePayload", ClosedConfig,
	pydantic.Def("url", pydantic.Str, ""),
).Described("``tools/preview_tool.py`` / ``tools/close_preview_tool.py``; ``url`` '' closes every tab.")

var _ = Event("preview.open", PreviewOpenPayload, "Open a URL / file in the desktop preview pane.")

var _ = Event("preview.close", PreviewClosePayload, "Close the preview pane or one tab.")

var PaneRevealPayload = pydantic.NewModel("PaneRevealPayload", OpenConfig,
	pydantic.Req("pane", pydantic.Str),
).Described("``tools/focus_pane_tool.py``.")

var _ = Event("pane.reveal", PaneRevealPayload, "Focus / reveal a named desktop pane.")

var MessageReactionPayload = pydantic.NewModel("MessageReactionPayload", ClosedConfig,
	pydantic.Req("row_id", pydantic.Int),
	pydantic.Req("reactions", pydantic.List(MessageReaction)),
	pydantic.Req("role", pydantic.Str),
).Described("``tools/react_to_message_tool.py``.")

var _ = Event("message.reaction", MessageReactionPayload, "The agent reacted to a message; paint it live.")

var TerminalOutputPayload = pydantic.NewModel("TerminalOutputPayload", ClosedConfig,
	pydantic.Req("process_id", pydantic.Str),
	pydantic.Req("chunk", pydantic.Str),
).Described("``session_notifications`` process_registry.on_output.")

var TerminalClosePayload = pydantic.NewModel("TerminalClosePayload", ClosedConfig,
	pydantic.Req("process_id", pydantic.Str),
)

var _ = Event("agent.terminal.output", TerminalOutputPayload, "Output chunk from an agent-owned background process.")

var _ = Event("terminal.close", TerminalClosePayload, "An agent-owned background process closed.")

var BrowserProgressPayload = pydantic.NewModel("BrowserProgressPayload", ClosedConfig,
	pydantic.Req("message", pydantic.Str),
	pydantic.Req("level", pydantic.Str),
).Described("``methods_browser`` announce(); ``level``: info | warn | error.")

var _ = Event("browser.progress", BrowserProgressPayload, "Browser (CDP) connect / install progress line.")

var _ = Event("voice.interrupted", nil, "Barge-in: the spoken interjection interrupted the turn; no payload.")

var VoiceStatusPayload = pydantic.NewModel("VoiceStatusPayload", ClosedConfig,
	pydantic.Req("state", pydantic.Str),
).Described("``methods_voice._vr_on_status``; states come from the recorder (idle / listening / transcribing …).")

var VoicePartialPayload = pydantic.NewModel("VoicePartialPayload", ClosedConfig,
	pydantic.Req("text", pydantic.Str),
).Described("``methods_voice`` voice.record ``on_partial`` — live STT text so far (``stt.streaming``).")

var VoiceTranscriptPayload = pydantic.NewModel("VoiceTranscriptPayload", ClosedConfig,
	pydantic.Def("text", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("stop_phrase", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("typed", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("no_speech_limit", pydantic.Nullable(pydantic.Bool), nil),
).Described("``methods_voice._vr_transcript`` / ``_deliver_fd_transcript`` / typed stop phrase in methods_prompt.")

var WakeDetectedPayload = pydantic.NewModel("WakeDetectedPayload", ClosedConfig,
	pydantic.Req("phrase", pydantic.Str),
	pydantic.Def("profile", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Req("start_new_session", pydantic.Bool),
).Described("``methods_voice`` wake detector ``_on_detect``.")

var _ = Event("voice.status", VoiceStatusPayload, "Voice recorder state changed.")

var _ = Event("voice.partial", VoicePartialPayload, "Live STT text so far while the user is still speaking.")

var _ = Event("voice.transcript", VoiceTranscriptPayload, "A voice capture produced text (or a stop phrase / silence limit).")

var _ = Event("wake.detected", WakeDetectedPayload, "A wake phrase fired.")

var PetChangedPayload = pydantic.NewModel("PetChangedPayload", OpenConfig,
	pydantic.Req("enabled", pydantic.Bool),
	pydantic.Def("slug", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("displayName", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("scale", pydantic.Nullable(pydantic.Float), nil),
	pydantic.Def("spritesheetRevision", pydantic.Nullable(pydantic.Str), nil),
).Described("``change_watcher._pet_changed_payload`` — ``pet.info.meta``-shaped; ``{enabled: false}`` when off.")

var PetGenerateProgressPayload = pydantic.NewModel("PetGenerateProgressPayload", OpenConfig,
	pydantic.Req("token", pydantic.Str),
	pydantic.Req("count", pydantic.Int),
	pydantic.Def("index", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("dataUri", pydantic.Nullable(pydantic.Str), nil),
).Described("``methods_session`` pet.generate: token-only init frame, then one per draft.")

var PetHatchProgressPayload = pydantic.NewModel("PetHatchProgressPayload", OpenConfig,
	pydantic.Req("event", pydantic.Str),
	pydantic.Def("detail", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("state", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("done", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("total", pydantic.Nullable(pydantic.Str), nil),
).Described("``methods_session`` pet.hatch ``_on_progress``: ``{event, detail}`` or the parsed row form.")

var _ = Event("pet.changed", PetChangedPayload, "The active pet / its spritesheet changed (watcher).")

var _ = Event("pet.generate.progress", PetGenerateProgressPayload, "Pet base-draft generation progress.")

var _ = Event("pet.hatch.progress", PetHatchProgressPayload, "Pet hatch (row drawing) progress.")

var ChangeSignalPayload = pydantic.NewModel("ChangeSignalPayload", OpenConfig).Described("``change_watcher._CHANGE_WATCHES`` payload fn — ``{}`` for every watch except pet.changed.")

var _ = Event("cron.changed", ChangeSignalPayload, "cron/jobs.json moved; refetch the cron list.")

var _ = Event("sessions.changed", ChangeSignalPayload, "state.db moved; refetch the session list.")

var _ = Event("platforms.changed", ChangeSignalPayload, "gateway_state.json moved; refetch platform status.")

var _ = Event("projects.changed", ChangeSignalPayload, "projects.db moved; refetch the project list + tree.")

var _ = Event("pairing.changed", ChangeSignalPayload, "Pairing state moved; refetch pairing.")

var _ = Event("skills.changed", ChangeSignalPayload, "A skill root moved (installed/removed elsewhere); refetch commands.catalog from the gateway.")

var _ = Event("bot_relay.outbox.pending", ChangeSignalPayload, "A bot-relay outbox envelope is queued; drain it.")
