package tuihost

import (
	"github.com/nickheyer/icebath/pkg/internal/pydantic"
)

var Usage = pydantic.NewModel("Usage", OpenConfig,
	pydantic.Def("model", pydantic.Str, ""),
	pydantic.Def("input", pydantic.Int, 0),
	pydantic.Def("output", pydantic.Int, 0),
	pydantic.Def("reasoning", pydantic.Int, 0),
	pydantic.Def("prompt", pydantic.Int, 0),
	pydantic.Def("completion", pydantic.Int, 0),
	pydantic.Def("total", pydantic.Int, 0),
	pydantic.Def("calls", pydantic.Int, 0),
	pydantic.Def("compressions", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("context_used", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("context_max", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("context_percent", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("context_source", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("context_estimated", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("cache_hit_pct", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("cache_read", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("cache_write", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("avg_latency_s", pydantic.Nullable(pydantic.Float), nil),
	pydantic.Def("avg_tps", pydantic.Nullable(pydantic.Float), nil),
	pydantic.Def("active_subagents", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("cost_usd", pydantic.Nullable(pydantic.Float), nil),
	pydantic.Def("cost_status", pydantic.Nullable(pydantic.Str), nil),
).Described("``tui_gateway/server.py::_get_usage`` + ``agent/context_breakdown.py::context_usage_fields``.")

var ProjectRef = pydantic.NewModel("ProjectRef", ClosedConfig,
	pydantic.Req("id", pydantic.Str),
	pydantic.Req("slug", pydantic.Str),
	pydantic.Req("name", pydantic.Str),
	pydantic.Def("primary_path", pydantic.Nullable(pydantic.Str), nil),
).Described("``tui_gateway/server.py::_project_info_for_cwd``.")

var McpServerStatus = pydantic.NewModel("McpServerStatus", OpenConfig,
	pydantic.Def("name", pydantic.Str, ""),
	pydantic.Def("status", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("tool_count", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("error", pydantic.Nullable(pydantic.Str), nil),
)

var SessionLiveInfo = pydantic.NewModel("SessionLiveInfo", OpenConfig,
	pydantic.Def("model", pydantic.Str, ""),
	pydantic.Def("provider", pydantic.Str, ""),
	pydantic.Def("reasoning_effort", pydantic.Str, ""),
	pydantic.Def("reasoning_effort_wire", pydantic.Str, ""),
	pydantic.Def("service_tier", pydantic.Str, ""),
	pydantic.Def("fast", pydantic.Bool, false),
	pydantic.Def("yolo", pydantic.Bool, false),
	pydantic.Def("approval_mode", pydantic.Str, "manual"),
	pydantic.Fac("tools", pydantic.Dict(pydantic.List(pydantic.Str)), pydantic.NewDict),
	pydantic.Fac("skills", pydantic.Dict(pydantic.List(pydantic.Str)), pydantic.NewDict),
	pydantic.Def("cwd", pydantic.Str, ""),
	pydantic.Def("branch", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("project", pydantic.Nullable(ProjectRef), nil),
	pydantic.Def("terminal_backend", pydantic.Str, ""),
	pydantic.Def("personality", pydantic.Str, ""),
	pydantic.Def("running", pydantic.Bool, false),
	pydantic.Def("turn_started_at", pydantic.Nullable(pydantic.Float), nil),
	pydantic.Def("title", pydantic.Str, ""),
	pydantic.Def("stored_session_id", pydantic.Str, ""),
	pydantic.Def("version", pydantic.Str, ""),
	pydantic.Def("release_date", pydantic.Str, ""),
	pydantic.Def("update_behind", pydantic.Nullable(pydantic.Any), nil),
	pydantic.Def("update_command", pydantic.Str, ""),
	pydantic.Def("usage", pydantic.Nullable(Usage), nil),
	pydantic.Def("profile_name", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("redact_secrets", pydantic.Bool, true),
	pydantic.Fac("mcp_servers", pydantic.List(McpServerStatus), pydantic.NewList),
	pydantic.Def("system_prompt", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("credential_warning", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("lazy", pydantic.Nullable(pydantic.Bool), nil),
).Described("``tui_gateway/server.py::_session_info`` — the ``session.info`` event and the ``info`` field of\n``session.create`` / ``session.resume`` / ``session.activate`` results.")

var ToolLabelKind = pydantic.Enum("ToolLabelKind", "connector", "connector", "mcp", "mcp", "tool", "tool").Described("Which surface one inner call of a bridged ``tool_call`` runs on.")

var ToolLabel = pydantic.NewModel("ToolLabel", ClosedConfig,
	pydantic.Req("kind", ToolLabelKind),
	pydantic.Req("app", pydantic.Str),
	pydantic.Req("action", pydantic.Str),
	pydantic.Req("emoji", pydantic.Str),
	pydantic.Req("text", pydantic.Str),
	pydantic.Req("name", pydantic.Str),
	pydantic.Def("preview", pydantic.Str, ""),
).Described("``tools.tool_labels.ToolLabel`` — what one call executed through the tool_search bridge is,\nin words. Clients render ``text`` (or ``app``/``action`` in their own columns) and never parse\nthe tool name themselves.")


var SubagentStatus = pydantic.Enum("SubagentStatus", "queued", "queued", "running", "running", "completed", "completed", "failed", "failed", "error", "error", "timeout", "timeout", "interrupted", "interrupted").Described("Lifecycle of one delegated child (``tools/delegate_tool_child_run.py``); ``failed`` /\n``error`` / ``timeout`` / ``interrupted`` / ``completed`` are terminal.")

var TerminalSubagentStatuses = map[string]bool{
	SubagentStatus.Member("completed"): true, SubagentStatus.Member("failed"): true, SubagentStatus.Member("error"): true,
	SubagentStatus.Member("timeout"): true, SubagentStatus.Member("interrupted"): true,
}

var PendingApproval = pydantic.NewModel("PendingApproval", OpenConfig,
	pydantic.Def("request_id", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("command", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("description", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("pattern_key", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("pattern_keys", pydantic.Nullable(pydantic.List(pydantic.Str)), nil),
	pydantic.Def("allow_permanent", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("allow_session", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("smart_denied", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("choices", pydantic.Nullable(pydantic.List(pydantic.Str)), nil),
	pydantic.Def("tool_name", pydantic.Nullable(pydantic.Str), nil),
).Described("One unresolved ``tools/approval.py`` gateway queue entry as ``server._approval_request_payload``\nrenders it (command redacted; ``choices`` precomputed). The key set is owned by the approval tool.")

var MessageReaction = pydantic.NewModel("MessageReaction", OpenConfig,
	pydantic.Req("emoji", pydantic.Str),
	pydantic.Req("author", pydantic.Str),
	pydantic.Def("at", pydantic.Nullable(pydantic.Float), nil),
	pydantic.Def("seen", pydantic.Nullable(pydantic.Bool), nil),
).Described("One persisted reaction row (``hermes_state_messages.set_message_reaction``); ``seen`` is\nstamped once announced.")

var SessionParams = pydantic.NewModel("SessionParams", ParamsConfig,
	pydantic.Req("session_id", pydantic.Str),
	pydantic.Def("profile", pydantic.Nullable(pydantic.Str), nil),
).Described("Any method addressed at one live session.")

var ProfileParams = pydantic.NewModel("ProfileParams", ParamsConfig,
	pydantic.Def("profile", pydantic.Nullable(pydantic.Str), nil),
).Described("Any method a client may route to a named profile with ``profile``.")

var SessionOwner = pydantic.NewModel("SessionOwner", ParamsConfig,
	pydantic.Req("type", pydantic.Literal("session")),
	pydantic.Req("session_id", pydantic.ConStr(pydantic.MinLen(1))),
)

var OkResult = pydantic.NewModel("OkResult", ClosedConfig,
	pydantic.Def("ok", pydantic.Bool, true),
)
