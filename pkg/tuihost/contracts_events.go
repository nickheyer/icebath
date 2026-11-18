package tuihost

import (
	"github.com/nickheyer/icebath/pkg/internal/pydantic"
)





var ErrorPayload = pydantic.NewModel("ErrorPayload", ClosedConfig,
	pydantic.Req("message", pydantic.Str),
).Described("Every ``_emit(\"error\", …)`` site sets exactly ``message``.")


var NoticePayload = pydantic.NewModel("NoticePayload", ClosedConfig,
	pydantic.Req("message", pydantic.Str),
).Described("``tui_gateway/model_switch.py`` capability-refresh notice.")

var _ = Event("notice", NoticePayload, "Informational one-liner for the session (capabilities refreshed).")













var BillingBlock = pydantic.NewModel("BillingBlock", ClosedConfig,
	pydantic.Req("provider", pydantic.Str),
	pydantic.Req("provider_label", pydantic.Str),
	pydantic.Req("model", pydantic.Str),
	pydantic.Req("billing_url", pydantic.Nullable(pydantic.Str)),
	pydantic.Req("message", pydantic.Str),
	pydantic.Def("unverified", pydantic.Nullable(pydantic.Bool), nil),
).Described("``agent/billing_links.py::BillingBlock.to_dict`` (+ ``unverified`` from conversation_loop).")
















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
