package tuihost

import (
	"github.com/nickheyer/icebath/pkg/internal/pydantic"
)

var ActiveSubagent = pydantic.NewModel("ActiveSubagent", OpenConfig,
	pydantic.Req("subagent_id", pydantic.Str),
	pydantic.Def("parent_id", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("depth", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("goal", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("delegation_id", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("model", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("started_at", pydantic.Nullable(pydantic.Float), nil),
	pydantic.Def("status", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("tool_count", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("owner_agent_session_id", pydantic.Nullable(pydantic.Str), nil),
).Described("One live child from ``tools/delegate_tool_registry.py::list_active_subagents`` (the record\nis extended by the child runner — ``missed_steer`` etc. — so it stays open).")

var DelegationStatusResult = pydantic.NewModel("DelegationStatusResult", ClosedConfig,
	pydantic.Req("active", pydantic.List(ActiveSubagent)),
	pydantic.Req("paused", pydantic.Bool),
	pydantic.Req("max_spawn_depth", pydantic.Int),
	pydantic.Req("max_concurrent_children", pydantic.Int),
)

var _ = Method("delegation.status", ProfileParams, DelegationStatusResult, "Running subagent tree plus the spawn pause flag and limits.")

var DelegationPauseParams = pydantic.Extend(ProfileParams, "DelegationPauseParams", ParamsConfig,
	pydantic.Def("paused", pydantic.Bool, true),
)

var DelegationPauseResult = pydantic.NewModel("DelegationPauseResult", ClosedConfig,
	pydantic.Req("paused", pydantic.Bool),
)

var _ = Method("delegation.pause", DelegationPauseParams, DelegationPauseResult, "Block/unblock NEW spawns globally (active children keep running); returns the new state.")

var SubagentSteerParams = pydantic.Extend(SessionParams, "SubagentSteerParams", ParamsConfig,
	pydantic.Req("subagent_id", pydantic.Str),
	pydantic.Req("text", pydantic.Str),
)

var SteerStatus = pydantic.Enum("SteerStatus", "queued", "queued", "rejected", "rejected")

var SubagentSteerResult = pydantic.NewModel("SubagentSteerResult", ClosedConfig,
	pydantic.Req("status", SteerStatus),
	pydantic.Req("subagent_id", pydantic.Str),
	pydantic.Req("text", pydantic.Str),
).Described("``queued`` is not ``delivered``: a child past its final tool batch surfaces ``missed_steer``.")

var _ = Method("subagent.steer", SubagentSteerParams, SubagentSteerResult, "Queue steering text into a live delegated child owned by this session.")

var HandoffRequestParams = pydantic.Extend(SessionParams, "HandoffRequestParams", ParamsConfig,
	pydantic.Req("platform", pydantic.Str),
)

var HandoffRequestResult = pydantic.NewModel("HandoffRequestResult", ClosedConfig,
	pydantic.Req("queued", pydantic.Bool),
	pydantic.Req("session_key", pydantic.Str),
	pydantic.Req("platform", pydantic.Str),
	pydantic.Req("home_name", pydantic.Str),
)

var _ = Method("handoff.request", HandoffRequestParams, HandoffRequestResult, "Queue a handoff to a messaging platform's home channel; the gateway watcher claims it.")

var HandoffStateResult = pydantic.NewModel("HandoffStateResult", ClosedConfig,
	pydantic.Req("state", pydantic.Str),
	pydantic.Req("platform", pydantic.Str),
	pydantic.Req("error", pydantic.Str),
).Described("``state`` is pending | running | completed | failed, or '' when nothing was requested.")

var _ = Method("handoff.state", SessionParams, HandoffStateResult, "Poll the handoff row for this session.")

var HandoffFailParams = pydantic.Extend(SessionParams, "HandoffFailParams", ParamsConfig,
	pydantic.Def("error", pydantic.Nullable(pydantic.Str), nil),
)

var HandoffFailResult = pydantic.NewModel("HandoffFailResult", ClosedConfig,
	pydantic.Req("failed", pydantic.Bool),
	pydantic.Req("state", pydantic.Str),
).Described("``failed`` false when the watcher already claimed the row; ``state`` is what it is now.")

var _ = Method("handoff.fail", HandoffFailParams, HandoffFailResult, "Fail a not-yet-claimed handoff (client poll timeout); CAS against the watcher.")

var ProjectFacts = pydantic.NewModel("ProjectFacts", ClosedConfig,
	pydantic.Req("root", pydantic.Str),
	pydantic.Req("manifests", pydantic.List(pydantic.Str)),
	pydantic.Req("packageManagers", pydantic.List(pydantic.Str)),
	pydantic.Req("verifyCommands", pydantic.List(pydantic.Str)),
	pydantic.Req("contextFiles", pydantic.List(pydantic.Str)),
).Described("``agent/coding_context.py::project_facts_for`` — the system prompt's coding-context detection.")
