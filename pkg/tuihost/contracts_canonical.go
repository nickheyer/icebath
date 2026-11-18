package tuihost

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

var CommandObserveParams = pydantic.NewModel("CommandObserveParams", ParamsConfig,
	pydantic.Req("session_id", pydantic.Str),
	pydantic.Req("name", pydantic.Str),
	pydantic.Def("arg", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("profile", pydantic.Nullable(pydantic.Str), nil),
)

var CommandObserveResult = pydantic.NewModel("CommandObserveResult", ClosedConfig,
	pydantic.Req("queued", pydantic.Bool),
)

var _ = CanonicalMethod("command.observe", CommandObserveParams, CommandObserveResult, "Fire-only: a recognized command the terminal UI or the headless chat runs itself; the gateway fires `pre_command` for it in the session's profile after replying (`queued` false: not fired).")

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

var _ = CanonicalMethod("rollback.session_diff", RollbackSessionDiffParams, RollbackSessionDiffResult, "/diff session: everything changed since the session's earliest retained checkpoint, as data for a terminal renderer.")

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

