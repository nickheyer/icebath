package tuihost

import (
	"github.com/nickheyer/icebath/pkg/internal/pydantic"
)

var BatteryCategory = pydantic.Enum("BatteryCategory", "good", "good", "warn", "warn", "bad", "bad", "critical", "critical", "dim", "dim").Described("``agent/battery.py::battery_category`` colour bucket.")

var SystemBatteryParams = pydantic.NewModel("SystemBatteryParams", ParamsConfig,
	pydantic.Def("profile", pydantic.Nullable(pydantic.Str), nil),
)

var SystemBatteryResult = pydantic.NewModel("SystemBatteryResult", ClosedConfig,
	pydantic.Req("available", pydantic.Bool),
	pydantic.Def("percent", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("plugged", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("category", BatteryCategory, "dim"),
)

var _ = Method("system.battery", SystemBatteryParams, SystemBatteryResult, "Host battery for the status bar; always resolves, ``available: false`` when unreadable.")

var ProcessStopParams = pydantic.NewModel("ProcessStopParams", ParamsConfig,
	pydantic.Def("session_id", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("profile", pydantic.Nullable(pydantic.Str), nil),
)

var ProcessStopResult = pydantic.NewModel("ProcessStopResult", ClosedConfig,
	pydantic.Req("killed", pydantic.Int),
	pydantic.Def("delegations", pydantic.Int, 0),
)

var _ = Method("process.stop", ProcessStopParams, ProcessStopResult, "``/stop``: on the shared owner, kill the background processes ``session_id`` owns (required); a standalone sidecar kills every process it holds. Answers the count killed.")

var ProcessListParams = pydantic.NewModel("ProcessListParams", ParamsConfig,
	pydantic.Req("session_id", pydantic.Str),
	pydantic.Def("profile", pydantic.Nullable(pydantic.Str), nil),
)

var ProcessEntry = pydantic.NewModel("ProcessEntry", OpenConfig,
	pydantic.Req("session_id", pydantic.Str),
	pydantic.Def("command", pydantic.Str, ""),
	pydantic.Def("cwd", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("pid", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("owner_task_id", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("started_at", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("uptime_seconds", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("status", pydantic.Str, "running"),
	pydantic.Def("output_preview", pydantic.Str, ""),
	pydantic.Def("output_tail", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("session_scoped", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("watch_patterns", pydantic.Nullable(pydantic.List(pydantic.Str)), nil),
	pydantic.Def("watch_hit", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("notify_on_complete", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("exit_code", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("exited_at", pydantic.Nullable(pydantic.Float), nil),
	pydantic.Def("started_epoch", pydantic.Nullable(pydantic.Float), nil),
	pydantic.Def("completion_reason", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("detached", pydantic.Nullable(pydantic.Bool), nil),
).Described("``tools/process_registry.py::list_sessions`` row plus the gateway's ``output_tail``.")

var ProcessListResult = pydantic.NewModel("ProcessListResult", ClosedConfig,
	pydantic.Fac("processes", pydantic.List(ProcessEntry), pydantic.NewList),
)

var _ = Method("process.list", ProcessListParams, ProcessListResult, "Background processes owned by the caller's session.")

var ProcessKillParams = pydantic.NewModel("ProcessKillParams", ParamsConfig,
	pydantic.Req("session_id", pydantic.Str),
	pydantic.Req("process_id", pydantic.Str),
	pydantic.Def("profile", pydantic.Nullable(pydantic.Str), nil),
)

var ProcessKillStatus = pydantic.Enum("ProcessKillStatus", "killed", "killed", "already_exited", "already_exited", "not_found", "not_found", "error", "error")

var ProcessKillResult = pydantic.NewModel("ProcessKillResult", OpenConfig,
	pydantic.Req("status", ProcessKillStatus),
	pydantic.Def("session_id", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("command", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("exit_code", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("completion_reason", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("termination_source", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("output", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("error", pydantic.Nullable(pydantic.Str), nil),
).Described("``tools/process_registry.py::kill_process`` snapshot; ``error`` rides on the failure statuses.")

var _ = Method("process.kill", ProcessKillParams, ProcessKillResult, "Kill one background process the caller's session owns and return its output snapshot.")

var ShellExecParams = pydantic.NewModel("ShellExecParams", ParamsConfig,
	pydantic.Req("command", pydantic.Str),
	pydantic.Def("profile", pydantic.Nullable(pydantic.Str), nil),
)

var ShellExecResult = pydantic.NewModel("ShellExecResult", ClosedConfig,
	pydantic.Req("stdout", pydantic.Str),
	pydantic.Req("stderr", pydantic.Str),
	pydantic.Req("code", pydantic.Int),
)

var _ = Method("shell.exec", ShellExecParams, ShellExecResult, "Run a safe (non-dangerous) shell command captured for ``!cmd`` / inline substitution.")

var CanonicalShellExecParams = pydantic.NewModel("CanonicalShellExecParams", ParamsConfig,
	pydantic.Req("session_id", pydantic.Str),
	pydantic.Req("command", pydantic.Str),
	pydantic.Def("profile", pydantic.Nullable(pydantic.Str), nil),
)

var _ = CanonicalMethod("shell.exec", CanonicalShellExecParams, ShellExecResult, "Ink ``!cmd`` / ``{!cmd}`` on the shared owner: one safe command for a local session, in its frozen launch cwd (``gateway/session_shell.py``).")

var CommandsCatalogParams = pydantic.NewModel("CommandsCatalogParams", ParamsConfig,
	pydantic.Def("session_id", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("profile", pydantic.Nullable(pydantic.Str), nil),
)

var CommandCategory = pydantic.NewModel("CommandCategory", ClosedConfig,
	pydantic.Req("name", pydantic.Str),
	pydantic.Fac("pairs", pydantic.List(pydantic.List(pydantic.Str)), pydantic.NewList),
)

var SkillCatalogEntry = pydantic.NewModel("SkillCatalogEntry", ClosedConfig,
	pydantic.Def("usage", pydantic.Int, 0),
	pydantic.Def("origin", pydantic.Str, "local"),
)

var CommandsCatalogResult = pydantic.NewModel("CommandsCatalogResult", ClosedConfig,
	pydantic.Fac("pairs", pydantic.List(pydantic.List(pydantic.Str)), pydantic.NewList),
	pydantic.Fac("sub", pydantic.Dict(pydantic.List(pydantic.Str)), pydantic.NewDict),
	pydantic.Fac("canon", pydantic.Dict(pydantic.Str), pydantic.NewDict),
	pydantic.Fac("categories", pydantic.List(CommandCategory), pydantic.NewList),
	pydantic.Fac("skills", pydantic.Dict(SkillCatalogEntry), pydantic.NewDict),
	pydantic.Def("skill_count", pydantic.Int, 0),
	pydantic.Def("warning", pydantic.Str, ""),
	pydantic.Def("plugin_discovery_error", pydantic.Str, ""),
	pydantic.Fac("plugin_commands", pydantic.Dict(pydantic.Str), pydantic.NewDict),
)

var _ = Method("commands.catalog", CommandsCatalogParams, CommandsCatalogResult, "Categorized slash metadata (registry, quick, plugin, skill) for completion menus.")

var DispatchType = pydantic.Enum("DispatchType", "exec", "exec", "alias", "alias", "plugin", "plugin", "send", "send", "skill", "skill", "prefill", "prefill").Described("``apps/shared/src/slash.ts::parseCommandDispatch`` branches on this.")

var CommandDispatchParams = pydantic.NewModel("CommandDispatchParams", ParamsConfig,
	pydantic.Req("name", pydantic.Str),
	pydantic.Def("arg", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("session_id", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("profile", pydantic.Nullable(pydantic.Str), nil),
)

var CommandDispatchResult = pydantic.NewModel("CommandDispatchResult", ClosedConfig,
	pydantic.Req("type", DispatchType),
	pydantic.Def("output", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("target", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("message", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("notice", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("display", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("name", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("status", pydantic.Nullable(pydantic.Str), nil),
).Described("One structured directive: ``exec``/``plugin`` carry ``output``; ``alias`` a ``target``;\n``send``/``prefill``/``skill`` a ``message`` (UIs render ``display``, never ``message``).")

var _ = Method("command.dispatch", CommandDispatchParams, CommandDispatchResult, "Run a quick/plugin/bundle/skill/built-in slash command and answer a structured directive.")

var SlashExecParams = pydantic.NewModel("SlashExecParams", ParamsConfig,
	pydantic.Req("session_id", pydantic.Str),
	pydantic.Req("command", pydantic.Str),
	pydantic.Def("profile", pydantic.Nullable(pydantic.Str), nil),
)

var SlashExecResult = pydantic.NewModel("SlashExecResult", ClosedConfig,
	pydantic.Def("output", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("warning", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("type", pydantic.Nullable(DispatchType), nil),
	pydantic.Def("target", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("message", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("notice", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("display", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("name", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("status", pydantic.Nullable(pydantic.Str), nil),
).Described("Plain worker/plugin text in ``output`` (+ ``warning``), or — when the command was rerouted to\n``command.dispatch`` — that method's directive fields with ``type`` set.")

var _ = Method("slash.exec", SlashExecParams, SlashExecResult, "Execute a slash command against the session's slash worker (or a live/plugin shortcut).")

var RollbackListParams = pydantic.NewModel("RollbackListParams", ParamsConfig,
	pydantic.Req("session_id", pydantic.Str),
	pydantic.Def("profile", pydantic.Nullable(pydantic.Str), nil),
)

var RollbackCheckpoint = pydantic.NewModel("RollbackCheckpoint", ClosedConfig,
	pydantic.Def("hash", pydantic.Str, ""),
	pydantic.Def("timestamp", pydantic.Str, ""),
	pydantic.Def("message", pydantic.Str, ""),
)

var RollbackListResult = pydantic.NewModel("RollbackListResult", ClosedConfig,
	pydantic.Req("enabled", pydantic.Bool),
	pydantic.Fac("checkpoints", pydantic.List(RollbackCheckpoint), pydantic.NewList),
)

var _ = Method("rollback.list", RollbackListParams, RollbackListResult, "Checkpoints for the session's cwd; ``enabled: false`` when checkpointing is off.")

var RollbackRestoreParams = pydantic.NewModel("RollbackRestoreParams", ParamsConfig,
	pydantic.Req("session_id", pydantic.Str),
	pydantic.Req("hash", pydantic.Str),
	pydantic.Def("file_path", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("profile", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("all", pydantic.Nullable(pydantic.Bool), nil),
)

var RollbackRestoreResult = pydantic.NewModel("RollbackRestoreResult", OpenConfig,
	pydantic.Req("success", pydantic.Bool),
	pydantic.Def("restored_to", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("reason", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("directory", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("file", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("restored_files", pydantic.Nullable(pydantic.List(pydantic.Str)), nil),
	pydantic.Def("skipped_user_edits", pydantic.Nullable(pydantic.List(pydantic.Str)), nil),
	pydantic.Def("skipped_oversize", pydantic.Nullable(pydantic.List(pydantic.Str)), nil),
	pydantic.Def("failed_deletes", pydantic.Nullable(pydantic.List(pydantic.Str)), nil),
	pydantic.Def("history_removed", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("error", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("debug", pydantic.Nullable(pydantic.Any), nil),
).Described("``tools/checkpoint_manager.py::restore`` outcome; ``history_removed`` is added for a full\n(non-file) restore that also rewound the live transcript.")

var _ = Method("rollback.restore", RollbackRestoreParams, RollbackRestoreResult, "Restore the working tree (or one file) to a checkpoint by hash or 1-based index.")

var RollbackDiffParams = pydantic.NewModel("RollbackDiffParams", ParamsConfig,
	pydantic.Req("session_id", pydantic.Str),
	pydantic.Req("hash", pydantic.Str),
	pydantic.Def("profile", pydantic.Nullable(pydantic.Str), nil),
)

var RollbackDiffResult = pydantic.NewModel("RollbackDiffResult", ClosedConfig,
	pydantic.Def("stat", pydantic.Str, ""),
	pydantic.Def("diff", pydantic.Str, ""),
	pydantic.Def("rendered", pydantic.Nullable(pydantic.Str), nil),
)

var _ = Method("rollback.diff", RollbackDiffParams, RollbackDiffResult, "Diff between a checkpoint and the working tree, with an ANSI rendering sized to the TUI.")

var BrowserAction = pydantic.Enum("BrowserAction", "status", "status", "connect", "connect", "disconnect", "disconnect")

var BrowserManageParams = pydantic.NewModel("BrowserManageParams", ParamsConfig,
	pydantic.Def("action", BrowserAction, "status"),
	pydantic.Def("url", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("session_id", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("profile", pydantic.Nullable(pydantic.Str), nil),
)

var BrowserManageResult = pydantic.NewModel("BrowserManageResult", ClosedConfig,
	pydantic.Req("connected", pydantic.Bool),
	pydantic.Def("url", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("messages", pydantic.Nullable(pydantic.List(pydantic.Str)), nil),
)

var _ = Method("browser.manage", BrowserManageParams, BrowserManageResult, "Inspect, attach to, or drop the CDP browser the tools use; ``messages`` narrate a connect.")
