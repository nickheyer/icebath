package tuigateway

import (
	"github.com/nickheyer/icebath/pkg/internal/pydantic"
)

var CanonicalSessionRef = pydantic.NewModel("CanonicalSessionRef", ClosedConfig,
	pydantic.Req("profile_id", pydantic.Str),
	pydantic.Req("session_id", pydantic.Str),
)

var AdmissionStatus = pydantic.NewModel("AdmissionStatus", ClosedConfig,
	pydantic.Req("admission_id", pydantic.Str),
	pydantic.Req("ref", CanonicalSessionRef),
	pydantic.Req("sequence", pydantic.Int),
	pydantic.Req("status", pydantic.Literal("queued", "started", "unknown", "terminal")),
	pydantic.Req("outcome", pydantic.Nullable(pydantic.Str)),
	pydantic.Req("authority_epoch", pydantic.Int),
	pydantic.Req("execution_generation", pydantic.Nullable(pydantic.Int)),
).Described("``gateway/session_contract.py::AdmissionReceipt`` as ``dataclasses.asdict`` sends it.")

var PromptReceiptParams = pydantic.NewModel("PromptReceiptParams", ParamsConfig,
	pydantic.Req("session_id", pydantic.Str),
	pydantic.Req("admission_id", pydantic.Str),
	pydantic.Def("include_result", pydantic.Nullable(pydantic.Bool), nil),
)

var PromptReceiptResult = pydantic.Extend(AdmissionStatus, "PromptReceiptResult", ClosedConfig,
	pydantic.Def("result", pydantic.Nullable(pydantic.Any), nil),
	pydantic.Def("usage", pydantic.Nullable(pydantic.Dict(pydantic.Any)), nil),
).Described("``result`` / ``usage``: the structured turn result committed at settlement, only with\n``include_result`` on a terminal admission that saved one.")

var _ = CanonicalMethod("prompt.receipt", PromptReceiptParams, PromptReceiptResult, "Current receipt of one admission the caller submitted (or controls).")

var PromptCancelParams = pydantic.NewModel("PromptCancelParams", ParamsConfig,
	pydantic.Req("session_id", pydantic.Str),
	pydantic.Req("admission_id", pydantic.Str),
)

var _ = CanonicalMethod("prompt.cancel", PromptCancelParams, AdmissionStatus, "Retire a queued admission; a started one is interrupted in the same call.")

var PromptResolveUnknownParams = pydantic.NewModel("PromptResolveUnknownParams", ParamsConfig,
	pydantic.Req("session_id", pydantic.Str),
	pydantic.Req("admission_id", pydantic.Str),
	pydantic.Req("execution_generation", pydantic.Int),
)

var _ = CanonicalMethod("prompt.resolve_unknown", PromptResolveUnknownParams, AdmissionStatus, "Acknowledge a turn lost across an owner restart; the FIFO behind it resumes. Never requeues the lost input.")

var SessionMutateParams = pydantic.NewModel("SessionMutateParams", ParamsConfig,
	pydantic.Req("session_id", pydantic.Str),
	pydantic.Req("request_id", pydantic.Str),
	pydantic.Req("expected_revision", pydantic.Int),
	pydantic.Req("operation", pydantic.Str),
	pydantic.Req("payload", pydantic.Dict(pydantic.Any)),
	pydantic.Def("expected_generation", pydantic.Nullable(pydantic.Int), nil),
).Described("``operation`` + ``payload`` are validated by ``hermes_state_mutations.validate_action``;\n``expected_generation`` is required for delete/rewind/reset/branch/model/compress/reasoning/workspace.\n``reasoning`` (``{effort}``) and ``workspace`` (``{cwd}``, refused ``session_busy`` mid-turn) rewrite\nthe session's frozen policy: session-scope ``/reasoning`` and Desktop's Move to project. ``edit_policy``\n(``{policy}``: ask / workspace_session / session) is an ACP session's edit approval (main's ACP modes).")

var SessionMutateResult = pydantic.NewModel("SessionMutateResult", OpenConfig,
	pydantic.Req("session_id", pydantic.Str),
	pydantic.Req("operation", pydantic.Str),
	pydantic.Def("revision", pydantic.Nullable(pydantic.Int), nil),
).Described("The committed mutation receipt plus the operation's projection (``title``, ``archived``,\n``branched_session_id``, compress ``status``/``lines``, …). ``revision`` is absent only on a\nread-only compress preview.")

var _ = CanonicalMethod("session.mutate", SessionMutateParams, SessionMutateResult, "Revision-fenced, retry-idempotent session edit (rename, archive, sidebar, branch, delete, rewind, reset, model, reasoning, workspace, edit_policy, compress, import).")

var SessionPruneParams = pydantic.NewModel("SessionPruneParams", ParamsConfig,
	pydantic.Def("filters", pydantic.Nullable(pydantic.Dict(pydantic.Any)), nil),
	pydantic.Def("never_active_days", pydantic.Nullable(pydantic.Float), nil),
).Described("Exactly one of ``filters`` (``SessionDB.prune_sessions`` keyword filters, as\n``icebath sessions prune`` builds them) or ``never_active_days`` (``--never-active``).")

var SessionPruneResult = pydantic.NewModel("SessionPruneResult", ClosedConfig,
	pydantic.Req("deleted", pydantic.Int),
	pydantic.Def("routing_deleted", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("skipped", pydantic.Nullable(pydantic.Int), nil),
)

var _ = CanonicalMethod("session.prune", SessionPruneParams, SessionPruneResult, "Local operator only: the owner deletes the ended sessions `icebath sessions prune` selected, skipping busy sessions and retained reset owners.")

var CommandObserveParams = pydantic.NewModel("CommandObserveParams", ParamsConfig,
	pydantic.Req("session_id", pydantic.Str),
	pydantic.Req("name", pydantic.Str),
	pydantic.Def("arg", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("profile", pydantic.Nullable(pydantic.Str), nil),
)

var CommandObserveResult = pydantic.NewModel("CommandObserveResult", ClosedConfig,
	pydantic.Req("queued", pydantic.Bool),
)

var _ = CanonicalMethod("command.observe", CommandObserveParams, CommandObserveResult, "Fire-only: a recognized command the classic CLI or the TUI runs itself; the gateway fires `pre_command` for it in the session's profile after replying (`queued` false: not fired).")

var CanonicalSessionInfoParams = pydantic.NewModel("CanonicalSessionInfoParams", ParamsConfig,
	pydantic.Req("session_id", pydantic.Str),
)

var SessionInventoryFields = pydantic.NewModel("SessionInventoryFields", ClosedConfig,
	pydantic.Def("provider", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("tools", pydantic.Nullable(pydantic.Dict(pydantic.List(pydantic.Str))), nil),
	pydantic.Def("skills", pydantic.Nullable(pydantic.Dict(pydantic.List(pydantic.Str))), nil),
	pydantic.Def("mcp_servers", pydantic.Nullable(pydantic.List(McpServerStatus)), nil),
	pydantic.Def("version", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("release_date", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("update_behind", pydantic.Nullable(pydantic.Any), nil),
	pydantic.Def("update_command", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("branch", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("project", pydantic.Nullable(ProjectRef), nil),
	pydantic.Def("system_prompt", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("context_length", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("enabled_toolsets", pydantic.Nullable(pydantic.List(pydantic.Str)), nil),
	pydantic.Def("tool_availability", pydantic.Nullable(pydantic.Dict(pydantic.Any)), nil),
	pydantic.Def("tool_availability_warnings", pydantic.Nullable(pydantic.List(pydantic.Str)), nil),
	pydantic.Def("unknown_toolsets", pydantic.Nullable(pydantic.List(pydantic.Str)), nil),
	pydantic.Def("usage", pydantic.Nullable(Usage), nil),
)

var CanonicalSessionInfo = pydantic.NewModel("CanonicalSessionInfo", OpenConfig,
	pydantic.Def("provider", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("tools", pydantic.Nullable(pydantic.Dict(pydantic.List(pydantic.Str))), nil),
	pydantic.Def("skills", pydantic.Nullable(pydantic.Dict(pydantic.List(pydantic.Str))), nil),
	pydantic.Def("mcp_servers", pydantic.Nullable(pydantic.List(McpServerStatus)), nil),
	pydantic.Def("version", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("release_date", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("update_behind", pydantic.Nullable(pydantic.Any), nil),
	pydantic.Def("update_command", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("branch", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("project", pydantic.Nullable(ProjectRef), nil),
	pydantic.Def("system_prompt", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("context_length", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("enabled_toolsets", pydantic.Nullable(pydantic.List(pydantic.Str)), nil),
	pydantic.Def("tool_availability", pydantic.Nullable(pydantic.Dict(pydantic.Any)), nil),
	pydantic.Def("tool_availability_warnings", pydantic.Nullable(pydantic.List(pydantic.Str)), nil),
	pydantic.Def("unknown_toolsets", pydantic.Nullable(pydantic.List(pydantic.Str)), nil),
	pydantic.Def("usage", pydantic.Nullable(Usage), nil),
	pydantic.Req("source", pydantic.Str),
	pydantic.Def("model", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Req("lazy", pydantic.Bool),
	pydantic.Req("profile_id", pydantic.Str),
	pydantic.Req("desktop_protocol", pydantic.Str),
	pydantic.Req("profile_name", pydantic.Str),
	pydantic.Def("cwd", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("edit_approval_policy", pydantic.Nullable(pydantic.Literal("ask", "workspace_session", "session")), nil),
	pydantic.Def("launch_request", pydantic.Nullable(pydantic.Dict(pydantic.Any)), nil),
	pydantic.Def("reasoning_effort", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("reasoning_effort_wire", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("service_tier", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("fast", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("title", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("started_at", pydantic.Nullable(pydantic.Float), nil),
	pydantic.Def("last_active_at", pydantic.Nullable(pydantic.Float), nil),
	pydantic.Def("yolo", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("api_mode", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("tool_count", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("credential_hint", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("config_warning", pydantic.Nullable(pydantic.Str), nil),
).Described("``gateway/session_local_inventory.py::snapshot_info``: ``local_session_info`` + the inventory.")

var _ = CanonicalMethod("session.info", CanonicalSessionInfoParams, CanonicalSessionInfo, "Frozen launch policy projection of one local session.")

var SessionStatsParams = pydantic.NewModel("SessionStatsParams", ParamsConfig,
	pydantic.Req("session_id", pydantic.Str),
)

var SessionStats = pydantic.NewModel("SessionStats", ClosedConfig,
	pydantic.Req("input_tokens", pydantic.Int),
	pydantic.Req("output_tokens", pydantic.Int),
	pydantic.Req("cache_read_tokens", pydantic.Int),
	pydantic.Req("cache_write_tokens", pydantic.Int),
	pydantic.Req("prompt_tokens", pydantic.Int),
	pydantic.Req("completion_tokens", pydantic.Int),
	pydantic.Req("total_tokens", pydantic.Int),
	pydantic.Req("reasoning_tokens", pydantic.Int),
	pydantic.Req("api_calls", pydantic.Int),
	pydantic.Req("last_prompt_tokens", pydantic.Int),
	pydantic.Req("context_tokens", pydantic.Int),
	pydantic.Def("context_length", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("config_context_length", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("context_source", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Req("compression_count", pydantic.Int),
	pydantic.Def("avg_latency_s", pydantic.Nullable(pydantic.Float), nil),
	pydantic.Def("tokens_per_s", pydantic.Nullable(pydantic.Float), nil),
	pydantic.Req("last_turn_tokens", pydantic.Int),
	pydantic.Def("rate_limit", pydantic.Nullable(pydantic.Dict(pydantic.Any)), nil),
	pydantic.Req("active_subagents", pydantic.Int),
	pydantic.Req("model", pydantic.Str),
	pydantic.Req("provider", pydantic.Str),
	pydantic.Def("fallback_model", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("plan_gauge", pydantic.Nullable(PlanGauge), nil),
).Described("``gateway/session_usage.py::usage_stats``: session-cumulative counters (the agent's ``session_*``),\ncontext / compressor figures, rolling latency and throughput, rate-limit state, live subagents.")

var _ = CanonicalMethod("session.stats", SessionStatsParams, SessionStats, "The session.usage dict on demand: what main's classic CLI read off its agent.")

var RollbackSessionDiffParams = pydantic.NewModel("RollbackSessionDiffParams", ParamsConfig,
	pydantic.Req("session_id", pydantic.Str),
	pydantic.Def("profile", pydantic.Nullable(pydantic.Str), nil),
)

var RollbackSessionDiffResult = pydantic.NewModel("RollbackSessionDiffResult", ClosedConfig,
	pydantic.Req("enabled", pydantic.Bool),
	pydantic.Def("success", pydantic.Bool, false),
	pydantic.Def("stat", pydantic.Str, ""),
	pydantic.Def("diff", pydantic.Str, ""),
	pydantic.Def("diff_lines", pydantic.Int, 0),
	pydantic.Def("empty", pydantic.Bool, false),
	pydantic.Def("error", pydantic.Nullable(pydantic.Str), nil),
).Described("``gateway/session_tui_ops.py::_session_diff``: ``CheckpointManager.session_diff`` under the\nsession's frozen checkpoint policy (``enabled: false`` when that policy does not checkpoint).\n``diff`` is bounded to fit one frame; ``diff_lines`` counts the whole body.")

var _ = CanonicalMethod("rollback.session_diff", RollbackSessionDiffParams, RollbackSessionDiffResult, "The classic CLI's /diff session: everything changed since the session's earliest retained checkpoint, as data for its terminal renderer.")

var SessionExportParams = pydantic.NewModel("SessionExportParams", ParamsConfig,
	pydantic.Req("session_id", pydantic.Str),
	pydantic.Def("profile", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("format", pydantic.Str, "json"),
	pydantic.Def("redact", pydantic.Bool, false),
).Described("``format``: json (default) / md / markdown / html / snapshot (``normalize_save_format``).")

var SessionExportResult = pydantic.NewModel("SessionExportResult", ClosedConfig,
	pydantic.Req("format", pydantic.Str),
	pydantic.Req("filename", pydantic.Str),
	pydantic.Req("content", pydantic.Str),
).Described("``gateway/session_tui_ops.py::export_session``: the rendered ``/save`` body and its default\nfile name. The client writes it (the gateway may be remote); nothing is written on the host.")

var _ = CanonicalMethod("session.export", SessionExportParams, SessionExportResult, "The /save transcript rendered for the client to write (the gateway writes nothing).")

var SessionAutomationParams = pydantic.NewModel("SessionAutomationParams", ParamsConfig,
	pydantic.Req("session_id", pydantic.Str),
	pydantic.Req("kind", pydantic.Literal("goal", "loop", "heartbeat")),
	pydantic.Req("call", pydantic.Str),
	pydantic.Def("args", pydantic.Nullable(pydantic.List(pydantic.Any)), nil),
	pydantic.Def("kwargs", pydantic.Nullable(pydantic.Dict(pydantic.Any)), nil),
)

var SessionAutomationResult = pydantic.NewModel("SessionAutomationResult", ClosedConfig,
	pydantic.Req("state", pydantic.Nullable(pydantic.Str)),
	pydantic.Def("value", pydantic.Nullable(pydantic.Any), nil),
	pydantic.Def("error", pydantic.Nullable(pydantic.Dict(pydantic.Str)), nil),
).Described("``gateway/session_viewer_automation.py``: the manager's persisted state after the call (its own\nJSON, ``null`` when none) and the call's value, or the refusal main's manager raised.")

var _ = CanonicalMethod("session.automation", SessionAutomationParams, SessionAutomationResult, "The classic CLI's /goal, /subgoal, /loop, /heartbeat manager calls (state writes, the goal judge, quality gates), run by the owner on the session's row.")

var SessionDetachParams = pydantic.NewModel("SessionDetachParams", ParamsConfig,
	pydantic.Req("session_id", pydantic.Str),
	pydantic.Req("subscription_id", pydantic.Str),
	pydantic.Def("end_reason", pydantic.Nullable(pydantic.Str), nil),
)

var SessionDetachResult = pydantic.NewModel("SessionDetachResult", ClosedConfig,
	pydantic.Req("session_id", pydantic.Str),
	pydantic.Req("subscription_id", pydantic.Str),
	pydantic.Req("detached", pydantic.Bool),
)

var _ = CanonicalMethod("session.detach", SessionDetachParams, SessionDetachResult, "Drop this connection's subscription; a stale subscription id answers detached=false. end_reason (snake_case): the viewer is closing the conversation; the last viewer leaving an idle local session ends its row with that reason.")

var RuntimeDescribeParams = pydantic.NewModel("RuntimeDescribeParams", ParamsConfig)

var SessionCreateDescriptor = pydantic.NewModel("SessionCreateDescriptor", ClosedConfig,
	pydantic.Req("sources", pydantic.List(pydantic.Str)),
	pydantic.Req("parameters", pydantic.List(pydantic.Str)),
)

var RuntimeDescribeResult = pydantic.NewModel("RuntimeDescribeResult", ClosedConfig,
	pydantic.Req("instance_id", pydantic.Str),
	pydantic.Req("profile_id", pydantic.Str),
	pydantic.Req("authority_epoch", pydantic.Int),
	pydantic.Req("capabilities", pydantic.List(pydantic.Str)),
	pydantic.Req("session_create", SessionCreateDescriptor),
	pydantic.Def("skin", pydantic.Nullable(SkinPayloadModel), nil),
)

var _ = CanonicalMethod("runtime.describe", RuntimeDescribeParams, RuntimeDescribeResult, "Owner identity and the canonical capabilities a client may rely on.")

var ClarifyRespondParams = pydantic.NewModel("ClarifyRespondParams", ParamsConfig,
	pydantic.Req("session_id", pydantic.Str),
	pydantic.Req("execution_generation", pydantic.Int),
	pydantic.Req("prompt_id", pydantic.Str),
	pydantic.Req("answer", pydantic.Str),
)

var PromptResponseResult = pydantic.NewModel("PromptResponseResult", ClosedConfig,
	pydantic.Req("status", pydantic.Literal("resolved", "already_resolved")),
	pydantic.Req("prompt_id", pydantic.Str),
)

var _ = CanonicalMethod("clarify.respond", ClarifyRespondParams, PromptResponseResult, "Generation-fenced answer to a pending clarify prompt (empty answer = skipped).")

var _ = CanonicalMethod("clarify.lock", ClarifyLockParams, ClarifyLockResult, "Lock one answer of the owner's batch clarify request (editable until every question is locked); only a viewer attached to that session with session:respond may lock.")

var CronRunParams = pydantic.NewModel("CronRunParams", ParamsConfig,
	pydantic.Req("job_id", pydantic.Str),
	pydantic.Req("request_id", pydantic.Str),
	pydantic.Req("extra_prompt", pydantic.Nullable(pydantic.Str)),
)

var CronSubmitResult = pydantic.NewModel("CronSubmitResult", ClosedConfig,
	pydantic.Req("session_id", pydantic.Str),
	pydantic.Req("admission_id", pydantic.Str),
)

var _ = CanonicalMethod("cron.submit", CronRunParams, CronSubmitResult, "Admit one cron firing into the owning profile's durable FIFO.")

var CronStatusResult = pydantic.NewModel("CronStatusResult", OpenConfig,
	pydantic.Req("status", pydantic.Str),
	pydantic.Req("result", pydantic.Nullable(pydantic.List(pydantic.Any))),
	pydantic.Def("job_flags", pydantic.Nullable(pydantic.Dict(pydantic.Any)), nil),
	pydantic.Def("job", pydantic.Nullable(pydantic.Dict(pydantic.Any)), nil),
).Described("``result`` is the ``run_job`` tuple once terminal; ``recover`` adds the frozen ``job`` and\nanswers ``status='missing'`` for a firing that was never admitted.")

var _ = CanonicalMethod("cron.recover", CronRunParams, CronStatusResult, "Re-observe a firing after a scheduler restart (same request identity).")

var CronAdmissionParams = pydantic.NewModel("CronAdmissionParams", ParamsConfig,
	pydantic.Req("session_id", pydantic.Str),
	pydantic.Req("admission_id", pydantic.Str),
)

var _ = CanonicalMethod("cron.status", CronAdmissionParams, CronStatusResult, "Status and, once terminal, the result of one cron admission.")

var CronCancelResult = pydantic.NewModel("CronCancelResult", ClosedConfig,
	pydantic.Req("ok", pydantic.Bool),
)

var _ = CanonicalMethod("cron.cancel", CronAdmissionParams, CronCancelResult, "Cancel a queued cron admission or latch cancellation on a started one.")

var KanbanRunParams = pydantic.NewModel("KanbanRunParams", ParamsConfig,
	pydantic.Req("board", pydantic.Str),
	pydantic.Req("task_id", pydantic.Str),
	pydantic.Req("run_id", pydantic.Int),
	pydantic.Req("claim_lock", pydantic.Str),
	pydantic.Def("db", pydantic.Nullable(pydantic.Str), nil),
)

var KanbanRunResult = pydantic.NewModel("KanbanRunResult", ClosedConfig,
	pydantic.Req("session_id", pydantic.Str),
	pydantic.Req("receipt", AdmissionStatus),
)

var _ = CanonicalMethod("kanban.run", KanbanRunParams, KanbanRunResult, "Native-owner only: admit the dispatcher's current claim on a kanban task.")

var A2aForwardParams = pydantic.NewModel("A2aForwardParams", ParamsConfig,
	pydantic.Req("agent", pydantic.Str),
	pydantic.Req("tenant", pydantic.Str),
	pydantic.Req("peer", pydantic.Str),
	pydantic.Req("context_id", pydantic.Str),
	pydantic.Req("input_id", pydantic.Str),
	pydantic.Req("text", pydantic.Str),
)

var A2aForwardResult = pydantic.Extend(AdmissionStatus, "A2aForwardResult", ClosedConfig,
	pydantic.Req("session_id", pydantic.Str),
	pydantic.Req("result", pydantic.Nullable(pydantic.Any)),
)

var _ = CanonicalMethod("a2a.forward", A2aForwardParams, A2aForwardResult, "Forward one A2A input into the conversation its identity tuple names.")

var WorkerScopeParams = pydantic.NewModel("WorkerScopeParams", ParamsConfig,
	pydantic.Req("profile_id", pydantic.Str),
	pydantic.Req("session_id", pydantic.Str),
	pydantic.Req("execution_id", pydantic.Str),
	pydantic.Req("generation", pydantic.Int),
	pydantic.Req("pid", pydantic.Int),
	pydantic.Req("birth", pydantic.Float),
	pydantic.Req("secret", pydantic.Str),
).Described("The producer claim every worker verb proves (``gateway/session_worker.py::_SCOPE``).")

var WorkerRegisterParams = pydantic.Extend(WorkerScopeParams, "WorkerRegisterParams", ParamsConfig,
	pydantic.Req("kind", pydantic.Str),
)

var WorkerExecution = pydantic.NewModel("WorkerExecution", OpenConfig,
	pydantic.Req("execution_id", pydantic.Str),
	pydantic.Req("session_id", pydantic.Str),
	pydantic.Req("kind", pydantic.Str),
	pydantic.Req("owner_epoch", pydantic.Int),
	pydantic.Req("generation", pydantic.Int),
	pydantic.Req("status", pydantic.Str),
	pydantic.Req("last_sequence", pydantic.Int),
).Described("A ``worker_executions`` row without its adoption digest.")

var _ = CanonicalMethod("worker.register", WorkerRegisterParams, WorkerExecution, "Register a compute worker execution on an idle session.")

var _ = CanonicalMethod("worker.adopt", WorkerScopeParams, WorkerExecution, "Adopt a registered execution after verifying the live producer claim.")

var WorkerPersistParams = pydantic.Extend(WorkerScopeParams, "WorkerPersistParams", ParamsConfig,
	pydantic.Req("epoch", pydantic.Int),
	pydantic.Req("sequence", pydantic.Int),
	pydantic.Req("operation", pydantic.Str),
	pydantic.Req("payload", pydantic.Dict(pydantic.Any)),
)

var WorkerPersistResult = pydantic.NewModel("WorkerPersistResult", OpenConfig).Described("The operation's durable receipt (``message_id`` for an append, delegation results, …).")

var _ = CanonicalMethod("worker.persist", WorkerPersistParams, WorkerPersistResult, "Sequence-fenced typed persistence write from an adopted worker.")

var GroupsAttachmentUploadParams = pydantic.Extend(RoomParams, "GroupsAttachmentUploadParams", ParamsConfig,
	pydantic.Req("upload_id", pydantic.Str),
	pydantic.Req("kind", pydantic.Str),
	pydantic.Req("name", pydantic.Str),
	pydantic.Req("mime", pydantic.Str),
	pydantic.Req("data_base64", pydantic.Str),
)

var RoomAttachment = pydantic.NewModel("RoomAttachment", OpenConfig,
	pydantic.Req("attachment_id", pydantic.Str),
	pydantic.Req("kind", pydantic.Str),
	pydantic.Req("name", pydantic.Str),
	pydantic.Req("size", pydantic.Int),
	pydantic.Req("mime", pydantic.Str),
	pydantic.Req("sha256", pydantic.Str),
	pydantic.Req("state", pydantic.Str),
	pydantic.Req("created_at", pydantic.Float),
	pydantic.Req("idempotent", pydantic.Bool),
	pydantic.Def("event_id", pydantic.Nullable(pydantic.Str), nil),
)

var _ = CanonicalMethod("groups.attachment.upload", GroupsAttachmentUploadParams, RoomAttachment, "Store one attachment for a later groups.send manifest (idempotent per upload_id).")

var _ = Method("groups.attachment.upload", GroupsAttachmentUploadParams, RoomAttachment, "Store one attachment for a later groups.send manifest (idempotent per upload_id).")

var GroupsAttachmentDownloadParams = pydantic.Extend(RoomParams, "GroupsAttachmentDownloadParams", ParamsConfig,
	pydantic.Req("event_id", pydantic.Str),
	pydantic.Req("attachment_id", pydantic.Str),
)

var RoomAttachmentBytes = pydantic.Extend(RoomAttachment, "RoomAttachmentBytes", OpenConfig,
	pydantic.Req("data_base64", pydantic.Str),
)

var _ = CanonicalMethod("groups.attachment.download", GroupsAttachmentDownloadParams, RoomAttachmentBytes, "Read one committed attachment for a live room viewer.")

var _ = Method("groups.attachment.download", GroupsAttachmentDownloadParams, RoomAttachmentBytes, "Read one committed attachment for a live room viewer.")

var GroupsDiscardParams = pydantic.Extend(RoomParams, "GroupsDiscardParams", ParamsConfig,
	pydantic.Req("member_id", pydantic.Str),
	pydantic.Req("task_id", pydantic.Str),
	pydantic.Req("execution_generation", pydantic.Int),
)

var GroupsDiscardResult = pydantic.NewModel("GroupsDiscardResult", ClosedConfig,
	pydantic.Def("discarded", pydantic.Bool, true),
	pydantic.Req("task", RoomTaskReceipt),
)

var _ = CanonicalMethod("groups.discard", GroupsDiscardParams, GroupsDiscardResult, "Discard one indeterminate room task after explicit user confirmation.")

var _ = Method("groups.discard", GroupsDiscardParams, GroupsDiscardResult, "Discard one indeterminate room task after explicit user confirmation.")
