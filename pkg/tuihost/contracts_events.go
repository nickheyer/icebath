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

var ChangeSignalPayload = pydantic.NewModel("ChangeSignalPayload", OpenConfig).Described("``change_watcher._CHANGE_WATCHES`` payload fn — ``{}`` for every watch except pet.changed.")

var _ = Event("sessions.changed", ChangeSignalPayload, "state.db moved; refetch the session list.")

var _ = Event("skills.changed", ChangeSignalPayload, "A skill root moved (installed/removed elsewhere); refetch commands.catalog from the gateway.")
