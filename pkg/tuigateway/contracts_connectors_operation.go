package tuigateway

import (
	"github.com/nickheyer/icebath/pkg/internal/pydantic"
)

var ConnectionTargetKind = pydantic.Enum("ConnectionTargetKind", "connector", "connector", "mcp", "mcp", "plugin", "plugin", "skill", "skill")

var ConnectionTargetAction = pydantic.Enum("ConnectionTargetAction", "authorize", "authorize", "connect", "connect", "enable", "enable", "install", "install", "reconnect", "reconnect")

var ConnectionTargetState = pydantic.Enum("ConnectionTargetState", "pending", "pending", "initiated", "initiated", "connected", "connected", "skipped", "skipped", "failed", "failed", "expired", "expired", "not_connected", "not_connected").Described("``tools/connectors/contract.py::TargetState``.")

var ConnectionActor = pydantic.Enum("ConnectionActor", "user", "user", "backend_watcher", "backend_watcher", "clock", "clock").Described("``tools/connectors/contract.py::Actor``.")

var ConnectionSettleReason = pydantic.Enum("ConnectionSettleReason", "all_resolved", "all_resolved", "continue_", "continue", "deadline", "deadline", "interrupt", "interrupt").Described("``tools/connectors/contract.py::SettleReason``.")

var ConnectionTargetEnvField = pydantic.NewModel("ConnectionTargetEnvField", ClosedConfig,
	pydantic.Req("name", pydantic.Str),
	pydantic.Req("required", pydantic.Bool),
	pydantic.Req("secret", pydantic.Bool),
	pydantic.Req("default", pydantic.Str),
	pydantic.Def("prompt", pydantic.Nullable(pydantic.Str), nil),
).Described("One credential an MCP install still needs; the card renders a field per entry and sends the\nvalues back with the approval.")

var CatalogTier = pydantic.Enum("CatalogTier", "official", "official", "verified", "verified", "community", "community")

var CatalogAppState = pydantic.Enum("CatalogAppState", "present", "present", "missing_app", "missing_app", "app_not_running", "app_not_running", "unknown", "unknown").Described("The desktop app a catalog plugin drives, from its ``hermes_platform`` declaration.")

var CatalogScanStatus = pydantic.Enum("CatalogScanStatus", "passed", "passed", "warnings", "warnings", "failed", "failed")

var CatalogScan = pydantic.NewModel("CatalogScan", ClosedConfig,
	pydantic.Req("status", CatalogScanStatus),
	pydantic.Req("summary", pydantic.Str),
).Described("The catalog's security scan of the pinned commit; read-only on the card.")

var CatalogApproved = pydantic.NewModel("CatalogApproved", ClosedConfig,
	pydantic.Req("force", pydantic.Bool),
	pydantic.Req("enable", pydantic.Bool),
	pydantic.Def("ref", pydantic.Nullable(pydantic.Str), nil),
).Described("The non-secret Advanced choices the user approved on a catalog row; a Try again after the\noperation settled repeats them.")

var CatalogServerError = pydantic.NewModel("CatalogServerError", ClosedConfig,
	pydantic.Req("name", pydantic.Str),
	pydantic.Req("error", pydantic.Str),
).Described("An MCP server an installed plugin brought that did not connect, with the raw reason.")

var ConnectionOperationTarget = pydantic.NewModel("ConnectionOperationTarget", ClosedConfig,
	pydantic.Req("name", pydantic.Str),
	pydantic.Req("kind", ConnectionTargetKind),
	pydantic.Req("action", ConnectionTargetAction),
	pydantic.Req("state", ConnectionTargetState),
	pydantic.Def("resolved", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("detail", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("instructions", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("discovery_error", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("connect_url", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("connection_id", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("alias", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("attempt", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("required_env", pydantic.Nullable(pydantic.List(ConnectionTargetEnvField)), nil),
	pydantic.Def("tools", pydantic.Nullable(pydantic.List(pydantic.Str)), nil),
	pydantic.Def("hint", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("display", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("description", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("tier", pydantic.Nullable(CatalogTier), nil),
	pydantic.Def("platforms", pydantic.Nullable(pydantic.List(pydantic.Str)), nil),
	pydantic.Def("repo", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("sha", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("subdir", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("scan", pydantic.Nullable(CatalogScan), nil),
	pydantic.Def("requires_hermes", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("has_desktop_half", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("target_profile", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("app_state", pydantic.Nullable(CatalogAppState), nil),
	pydantic.Def("skill", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("phase", pydantic.Nullable(InstallPhase), nil),
	pydantic.Def("approved", pydantic.Nullable(CatalogApproved), nil),
	pydantic.Def("enabled", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("missing_env", pydantic.Nullable(pydantic.List(pydantic.Str)), nil),
	pydantic.Def("server_errors", pydantic.Nullable(pydantic.List(CatalogServerError)), nil),
	pydantic.Def("already_installed", pydantic.Nullable(pydantic.Bool), nil),
).Described("``Target.snapshot``: the link minted up front rides here, never in the model result. ``extra``\nkeys a leg records (``tools``, ``hint``) are typed here as they appear.")

var ConnectionDisplay = pydantic.NewModel("ConnectionDisplay", ClosedConfig,
	pydantic.Req("suggested", pydantic.List(pydantic.Str)),
).Described("``manage_connections`` action ``display``: the directory card, with these slugs pinned as Suggested.")

var ConnectionRequestPayload = pydantic.NewModel("ConnectionRequestPayload", ClosedConfig,
	pydantic.Req("op_id", pydantic.Str),
	pydantic.Req("seq", pydantic.Int),
	pydantic.Req("deadline_at", pydantic.Float),
	pydantic.Req("timeout_seconds", pydantic.Float),
	pydantic.Req("targets", pydantic.List(ConnectionOperationTarget)),
	pydantic.Def("tool_call_id", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("display", pydantic.Nullable(ConnectionDisplay), nil),
).Described("``ConnectionOperation.request_payload``: opens the card; also the ``pending_connection`` resume\nsnapshot so a client that missed the event restores the card with the server's deadline.")

var _ = Event("connection.request", ConnectionRequestPayload, "A connection operation opened on this session; the desktop renders its card.")

var ConnectionOperationStatus = pydantic.NewModel("ConnectionOperationStatus", ClosedConfig,
	pydantic.Req("op_id", pydantic.Str),
	pydantic.Req("seq", pydantic.Int),
	pydantic.Req("deadline_at", pydantic.Float),
	pydantic.Req("settled", pydantic.Bool),
	pydantic.Def("settled_at", pydantic.Nullable(pydantic.Float), nil),
	pydantic.Def("settled_by", pydantic.Nullable(ConnectionSettleReason), nil),
	pydantic.Req("targets", pydantic.List(ConnectionOperationTarget)),
).Described("``methods_connectors._operation_view``: the operation's full snapshot.")

var ConnectionUpdatePayload = pydantic.NewModel("ConnectionUpdatePayload", ClosedConfig,
	pydantic.Req("op_id", pydantic.Str),
	pydantic.Req("seq", pydantic.Int),
	pydantic.Req("deadline_at", pydantic.Float),
	pydantic.Req("settled", pydantic.Bool),
	pydantic.Def("settled_at", pydantic.Nullable(pydantic.Float), nil),
	pydantic.Def("settled_by", pydantic.Nullable(ConnectionSettleReason), nil),
	pydantic.Req("targets", pydantic.List(ConnectionOperationTarget)),
	pydantic.Req("owner", ConnectorOwner),
	pydantic.Def("target", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("from_", pydantic.Nullable(ConnectionTargetState), nil).Alias("from"),
	pydantic.Def("to", pydantic.Nullable(ConnectionTargetState), nil),
	pydantic.Def("actor", pydantic.Nullable(ConnectionActor), nil),
	pydantic.Def("detail", pydantic.Nullable(pydantic.Str), nil),
).Described("``methods_connectors._connection_update``: one target transition (``target``/``from``/``to``/\n``actor``) or the settlement (none of those), with the full snapshot.")

var _ = Event("connection.update", ConnectionUpdatePayload, "One transition or the settlement of an open connection operation.")

var ConnectionOperationParams = pydantic.Extend(ProfileParams, "ConnectionOperationParams", ParamsConfig,
	pydantic.Req("owner", ConnectorOwner),
	pydantic.Req("op_id", pydantic.Str),
)

var _ = Method("connectors.operation.status", ConnectionOperationParams, ConnectionOperationStatus, "The current snapshot of one open session or account operation.")

var ConnectionWakeResult = pydantic.NewModel("ConnectionWakeResult", ClosedConfig,
	pydantic.Req("status", pydantic.Literal("ok")),
)

var _ = Method("connectors.operation.wake", ConnectionOperationParams, ConnectionWakeResult, "The browser leg came back (icebath://connections/done): read the accounts now, not at the next tick.")

var ConnectionAnswerStatus = pydantic.Enum("ConnectionAnswerStatus", "approved", "approved", "skipped", "skipped").Described("What the card says about one row: ``tools/connectors/mcp.py::apply_answer``.")

var ConnectionAnswerTarget = pydantic.NewModel("ConnectionAnswerTarget", ParamsConfig,
	pydantic.Req("name", pydantic.Str),
	pydantic.Req("status", ConnectionAnswerStatus),
	pydantic.Def("detail", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("env", pydantic.Nullable(pydantic.Dict(pydantic.Str)), nil),
).Described("One row's answer from the card. ``env`` carries the credential values an install asked for\nthrough ``required_env``.")

var ConnectionAnswer = pydantic.NewModel("ConnectionAnswer", ParamsConfig,
	pydantic.Fac("targets", pydantic.List(ConnectionAnswerTarget), pydantic.NewList),
	pydantic.Def("settled_by", pydantic.Nullable(ConnectionSettleReason), nil),
	pydantic.Def("said", pydantic.Nullable(pydantic.Str), nil),
).Described("The card's answer: per-target outcomes and an optional Continue\n(``settled_by: \"continue\"``). Settlement is derived from target states afterwards.")

var ConnectionRespondParams = pydantic.Extend(ConnectionOperationParams, "ConnectionRespondParams", ParamsConfig,
	pydantic.Req("result", ConnectionAnswer),
)

var ConnectionRespondResult = pydantic.NewModel("ConnectionRespondResult", ClosedConfig,
	pydantic.Req("status", pydantic.Literal("ok")),
	pydantic.Req("settled", pydantic.Bool),
)

var _ = Method("connection.respond", ConnectionRespondParams, ConnectionRespondResult, "Per-target outcomes from the card, and an optional Continue.")

var InstallPhase = pydantic.Enum("InstallPhase", "downloading", "downloading", "python_packages", "python_packages", "loading_tools", "loading_tools").Described("The slow steps of an install, as ids; the desktop catalog card words them in its own language.")
