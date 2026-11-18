package tuihost

import (
	"github.com/nickheyer/icebath/pkg/internal/pydantic"
)

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
).Described("``message.delta``. A session-authority local-viewer turn also publishes ``text: null``: the\nreply segment boundary (the agent's ``stream_delta_callback(None)``, before tool lines) that\nends the headless chat's reply segment. Every other viewer ignores an empty chunk.\n\nA prompt turn stamps ``turn_id`` + ``segment`` (interim seals before it) + ``offset`` (UTF-16 code\nunits of that segment already sent): a client drops a re-sent chunk it already applied.")

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

var ErrorSurface = pydantic.NewModel("ErrorSurface", OpenConfig,
	pydantic.Req("layer", pydantic.Str),
	pydantic.Req("code", pydantic.Str),
	pydantic.Req("retryable", pydantic.Bool),
	pydantic.Def("provider", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("model", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("resets_at", pydantic.Nullable(pydantic.Float), nil),
).Described("``agent/error_surface.py::_surface`` — advisory {layer, code, retryable} (+ identity, + auth hint,\n+ ``resets_at`` epoch seconds when the provider named when its limit lifts).")

var BillingBlock = pydantic.NewModel("BillingBlock", ClosedConfig,
	pydantic.Req("provider", pydantic.Str),
	pydantic.Req("provider_label", pydantic.Str),
	pydantic.Req("model", pydantic.Str),
	pydantic.Req("billing_url", pydantic.Nullable(pydantic.Str)),
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
).Described("``prompt_turn._complete_turn_payload`` / ``session_auto_continue._emit_terminal_turn_error`` /\n``agent_callbacks._mirror_subagent_to_child`` (child watch mirror: ``text`` only).")

var _ = Event("message.complete", MessageCompletePayload, "The turn ended: final text, usage and outcome.")

var StatusUpdatePayload = pydantic.NewModel("StatusUpdatePayload", ClosedConfig,
	pydantic.Req("kind", pydantic.Str),
	pydantic.Req("text", pydantic.Str),
).Described("``server._status_update`` and the direct emitters (goal / loop / heartbeat / process).")

var _ = Event("status.update", StatusUpdatePayload, "Transient status line (kind: status, lifecycle, compacting, goal, loop, heartbeat, process, …).")





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
).Described("A session-authority local-viewer turn: one line the agent printed through ``_print_fn``\n(subagent completion, compaction, errors, budget notices), which the headless chat prints\nas a status line.")

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
).Described("``agent/credits_tracker.py::AgentNotice`` via notice_callback, and ``server._await_agent_ready``'s\nslow-build notice. ``level``: info | warn | error | success; ``kind``: sticky | ttl | agent.\n``detail``: the full text behind a short ``text`` (the low-disk auto-prune question): the TUI prints\nit into the transcript.")

var NotificationClearPayload = pydantic.NewModel("NotificationClearPayload", ClosedConfig,
	pydantic.Req("key", pydantic.Str),
)

var _ = Event("notification.show", NotificationShowPayload, "Show / replace a keyed out-of-band notice (toast or status bar).")

var _ = Event("notification.clear", NotificationClearPayload, "Withdraw the notice with this key.")







var ApprovalCancelledPayload = pydantic.NewModel("ApprovalCancelledPayload", ClosedConfig,
	pydantic.Req("session_id", pydantic.Str),
	pydantic.Req("stored_session_id", pydantic.Str),
	pydantic.Req("reason", pydantic.Str),
	pydantic.Req("cancelled_count", pydantic.Int),
	pydantic.Req("request_ids", pydantic.List(pydantic.Str)),
).Described("``session_lifecycle._announce_cancelled_gateway_approvals`` (broadcast).\n\nOne frame for every pending approval dropped by an interrupt / reap / teardown (#106678) — the\ndeny-resolve is silent without it, so a reconnecting client's prompt looks lost rather than cancelled.\n``cancelled_count`` is the number of dropped entries; ``request_ids`` omits empty/missing ids, so the\ntwo can disagree when an entry has no request_id.")

var _ = Event("approval.cancelled", ApprovalCancelledPayload, "Pending gateway approvals were dropped by interrupt/reap/teardown; the wait resolved as deny (not a user refusal).")



var SideAgentCompletePayload = pydantic.NewModel("SideAgentCompletePayload", ClosedConfig,
	pydantic.Req("task_id", pydantic.Str),
	pydantic.Req("text", pydantic.Str),
	pydantic.Def("question", pydantic.Nullable(pydantic.Str), nil),
).Described("``methods_prompt._spawn_side_agent``: ``{task_id, **extra, text}``; btw adds ``question``.")

var _ = Event("background.complete", SideAgentCompletePayload, "A /background side agent finished.")

var _ = Event("btw.complete", SideAgentCompletePayload, "A /btw side question was answered.")

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

var _ = Event("sessions.changed", ChangeSignalPayload, "state.db moved; refetch the session list.")

var _ = Event("skills.changed", ChangeSignalPayload, "A skill root moved (installed/removed elsewhere); refetch commands.catalog from the gateway.")
