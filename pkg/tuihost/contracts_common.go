package tuihost

import (
	"github.com/nickheyer/icebath/pkg/internal/pydantic"
)

var McpServerStatus = pydantic.NewModel("McpServerStatus", OpenConfig,
	pydantic.Def("name", pydantic.Str, ""),
	pydantic.Def("status", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("tool_count", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("error", pydantic.Nullable(pydantic.Str), nil),
)

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

