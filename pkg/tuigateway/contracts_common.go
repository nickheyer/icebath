package tuigateway

import (
	"github.com/nickheyer/icebath/pkg/internal/pydantic"
)

var OpenModel = pydantic.NewModel("OpenModel", OpenConfig)

var PlanGauge = pydantic.NewModel("PlanGauge", ClosedConfig,
	pydantic.Req("id", pydantic.Str),
	pydantic.Req("label", pydantic.Str),
	pydantic.Req("used_bp", pydantic.Int),
	pydantic.Def("resets_at_ms", pydantic.Nullable(pydantic.Int), nil),
).Described("``gateway/session_usage.py::plan_gauge``: a window plan's allowance for the status bar.")

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
	pydantic.Def("dev_credits_spent_micros", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("cost_usd", pydantic.Nullable(pydantic.Float), nil),
	pydantic.Def("cost_status", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("plan_gauge", pydantic.Nullable(PlanGauge), nil),
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
	pydantic.Def("desktop_contract", pydantic.Nullable(pydantic.Union(pydantic.Int, pydantic.Str)), nil),
	pydantic.Def("version", pydantic.Str, ""),
	pydantic.Def("release_date", pydantic.Str, ""),
	pydantic.Def("update_behind", pydantic.Nullable(pydantic.Any), nil),
	pydantic.Def("update_command", pydantic.Str, ""),
	pydantic.Def("usage", pydantic.Nullable(Usage), nil),
	pydantic.Def("profile_name", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Fac("mcp_servers", pydantic.List(McpServerStatus), pydantic.NewList),
	pydantic.Def("system_prompt", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("credential_warning", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("lazy", pydantic.Nullable(pydantic.Bool), nil),
).Described("``tui_gateway/server.py::_session_info`` — the ``session.info`` event and the ``info`` field of\n``session.create`` / ``session.resume`` / ``session.activate`` results.")

var StoredSessionRow = pydantic.NewModel("StoredSessionRow", OpenConfig,
	pydantic.Req("id", pydantic.Str),
	pydantic.Def("title", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("preview", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("source", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("model", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("started_at", pydantic.Nullable(pydantic.Float), nil),
	pydantic.Def("ended_at", pydantic.Nullable(pydantic.Float), nil),
	pydantic.Def("last_active", pydantic.Nullable(pydantic.Float), nil),
	pydantic.Def("message_count", pydantic.Int, 0),
	pydantic.Def("tool_call_count", pydantic.Int, 0),
	pydantic.Def("input_tokens", pydantic.Int, 0),
	pydantic.Def("output_tokens", pydantic.Int, 0),
	pydantic.Def("is_active", pydantic.Bool, false),
	pydantic.Def("cwd", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("git_branch", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("git_repo_root", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("parent_session_id", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("pinned", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("unread", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("archived", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("actual_cost_usd", pydantic.Nullable(pydantic.Float), nil),
	pydantic.Def("estimated_cost_usd", pydantic.Nullable(pydantic.Float), nil),
	pydantic.Def("handoff_platform", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("handoff_state", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("lineage_root_id", pydantic.Nullable(pydantic.Str), nil).Alias("_lineage_root_id"),
	pydantic.Def("lineage_ids", pydantic.Nullable(pydantic.List(pydantic.Str)), nil).Alias("_lineage_ids"),
	pydantic.Def("continuation_kind", pydantic.Nullable(pydantic.Str), nil),
)

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

var TranscriptMessage = pydantic.NewModel("TranscriptMessage", OpenConfig,
	pydantic.Req("role", pydantic.Str),
	pydantic.Def("text", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("content", pydantic.Nullable(pydantic.Any), nil),
	pydantic.Def("tool_call_id", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("timestamp", pydantic.Nullable(pydantic.Float), nil),
	pydantic.Def("row_id", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("display_kind", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("display_metadata", pydantic.Nullable(pydantic.Any), nil),
	pydantic.Def("name", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("context", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("args", pydantic.Nullable(pydantic.Dict(pydantic.Any)), nil),
	pydantic.Def("labels", pydantic.Nullable(pydantic.List(ToolLabel)), nil),
	pydantic.Def("reasoning", pydantic.Nullable(pydantic.Str), nil),
).Described("One transcript row as the gateway PROJECTS it for renderers (``session_history._project_history``):\n``text``, display-only ``timestamp`` / ``display_kind`` / ``display_metadata``, the durable ``row_id``\nrewind targets, and for tool rows raw ``content``, ``tool_call_id``, ``name``, ``context`` and ``args``.\nAssistant detail sidecars (``reasoning``, …) ride as extra keys.")

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
).Described("Any method the desktop may route to a named profile (``requestGatewayForProfile`` adds ``profile``).")

var SessionOwner = pydantic.NewModel("SessionOwner", ParamsConfig,
	pydantic.Req("type", pydantic.Literal("session")),
	pydantic.Req("session_id", pydantic.ConStr(pydantic.MinLen(1))),
)

var AccountOwner = pydantic.NewModel("AccountOwner", ParamsConfig,
	pydantic.Req("type", pydantic.Literal("account")),
)

var ConnectorOwner = pydantic.Tagged("type", SessionOwner, AccountOwner)

var OkResult = pydantic.NewModel("OkResult", ClosedConfig,
	pydantic.Def("ok", pydantic.Bool, true),
)

var StatusResult = pydantic.NewModel("StatusResult", OpenConfig,
	pydantic.Req("status", pydantic.Str),
)

var EmptyResult = pydantic.NewModel("EmptyResult", ClosedConfig)

var EmptyPayload = pydantic.NewModel("EmptyPayload", ClosedConfig)

var PlanUsageGauge = pydantic.NewModel("PlanUsageGauge", ClosedConfig,
	pydantic.Req("label", pydantic.Str),
	pydantic.Req("left_fraction", pydantic.Float),
	pydantic.Req("style", pydantic.Str),
	pydantic.Def("resets_at_ms", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("scope", pydantic.Str, "all"),
	pydantic.Def("title", pydantic.Str, ""),
).Described("One server allowance on ``ModelOptionProvider.plan_usage``, in the server's order. ``label`` is the\nserver's id (``day`` / ``week`` / ``month`` / ``top_tier`` have client words; any other id shows\n``title``). ``scope``: which models a full one stops (``all`` / ``top_tier``).")

var PlanUsage = pydantic.NewModel("PlanUsage", ClosedConfig,
	pydantic.Req("kind", pydantic.Str),
	pydantic.Def("plan", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Fac("gauges", pydantic.List(PlanUsageGauge), pydantic.NewList),
	pydantic.Fac("top_tier_model_ids", pydantic.List(pydantic.Str), pydantic.NewList),
	pydantic.Fac("subscription_model_ids", pydantic.List(pydantic.Str), pydantic.NewList),
	pydantic.Fac("featured_model_ids", pydantic.List(pydantic.Str), pydantic.NewList),
	pydantic.Def("paused_reason", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("credits_pay", pydantic.Nullable(pydantic.Bool), nil),
).Described("``hermes_cli/model_picker_usage.py::PickerUsage.to_payload`` — usage-window plan chrome.")
