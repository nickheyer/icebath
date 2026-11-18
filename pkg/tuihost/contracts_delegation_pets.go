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

var PetCancelParams = pydantic.Extend(ProfileParams, "PetCancelParams", ParamsConfig,
	pydantic.Def("token", pydantic.Nullable(pydantic.Str), nil),
)

var PetCancelResult = pydantic.NewModel("PetCancelResult", ClosedConfig,
	pydantic.Req("ok", pydantic.Bool),
)

var _ = Method("pet.cancel", PetCancelParams, PetCancelResult, "Stop an in-flight pet generate/hatch by token (idempotent).")

var PetGenerateParams = pydantic.Extend(ProfileParams, "PetGenerateParams", ParamsConfig,
	pydantic.Def("prompt", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("referenceImage", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("count", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("style", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("provider", pydantic.Nullable(pydantic.Str), nil),
).Described("``prompt`` or a ``referenceImage`` data URL is required (the handler answers 4004 without one).")

var PetDraft = pydantic.NewModel("PetDraft", ClosedConfig,
	pydantic.Req("index", pydantic.Int),
	pydantic.Req("dataUri", pydantic.Str),
)

var PetGenerateResult = pydantic.NewModel("PetGenerateResult", ClosedConfig,
	pydantic.Req("ok", pydantic.Bool),
	pydantic.Req("token", pydantic.Str),
	pydantic.Req("drafts", pydantic.List(PetDraft)),
)

var _ = Method("pet.generate", PetGenerateParams, PetGenerateResult, "Candidate base looks for a new pet (draft step); drafts also stream via pet.generate.progress.")

var PetHatchParams = pydantic.Extend(ProfileParams, "PetHatchParams", ParamsConfig,
	pydantic.Req("token", pydantic.Str),
	pydantic.Req("name", pydantic.Str),
	pydantic.Def("cancelToken", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("index", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("description", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("prompt", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("style", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("provider", pydantic.Nullable(pydantic.Str), nil),
)

var PetSpritePayload = pydantic.NewModel("PetSpritePayload", ClosedConfig,
	pydantic.Def("slug", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("displayName", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("mime", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("spritesheetBase64", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("spritesheetRevision", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("frameW", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("frameH", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("framesPerState", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("framesByState", pydantic.Nullable(pydantic.Dict(pydantic.Int)), nil),
	pydantic.Def("framesByRow", pydantic.Nullable(pydantic.Dict(pydantic.Int)), nil),
	pydantic.Def("loopMs", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("scale", pydantic.Nullable(pydantic.Float), nil),
	pydantic.Def("stateRows", pydantic.Nullable(pydantic.List(pydantic.Str)), nil),
)

var PetHatchResult = pydantic.NewModel("PetHatchResult", ClosedConfig,
	pydantic.Req("ok", pydantic.Bool),
	pydantic.Req("slug", pydantic.Str),
	pydantic.Req("displayName", pydantic.Str),
	pydantic.Fac("warnings", pydantic.List(pydantic.Any), pydantic.NewList),
	pydantic.Req("pet", PetSpritePayload),
).Described("The hatched pet is installed but NOT active (``pet.select`` adopts, ``pet.remove`` discards).")

var _ = Method("pet.hatch", PetHatchParams, PetHatchResult, "Turn a base draft into a full spritesheet pet; progress streams via pet.hatch.progress.")

var ProjectFacts = pydantic.NewModel("ProjectFacts", ClosedConfig,
	pydantic.Req("root", pydantic.Str),
	pydantic.Req("manifests", pydantic.List(pydantic.Str)),
	pydantic.Req("packageManagers", pydantic.List(pydantic.Str)),
	pydantic.Req("verifyCommands", pydantic.List(pydantic.Str)),
	pydantic.Req("contextFiles", pydantic.List(pydantic.Str)),
).Described("``agent/coding_context.py::project_facts_for`` — the system prompt's coding-context detection.")
