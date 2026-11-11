package tuigateway

import (
	"github.com/nickheyer/icebath/pkg/internal/pydantic"
)

var LeaseHolder = pydantic.Enum("LeaseHolder", "agent", "agent", "human", "human")

var DisplayLease = pydantic.NewModel("DisplayLease", ClosedConfig,
	pydantic.Req("holder", LeaseHolder),
	pydantic.Def("viewer_id", pydantic.NoneT, nil),
	pydantic.Def("viewer_hash", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Req("since", pydantic.Float),
	pydantic.Req("epoch", pydantic.Int),
	pydantic.Def("reason", pydantic.Str, ""),
)

var DisplayImageSwitch = pydantic.NewModel("DisplayImageSwitch", ClosedConfig,
	pydantic.Req("current_image", pydantic.Str),
	pydantic.Req("target_image", pydantic.Str),
	pydantic.Req("containers", pydantic.Int),
)

var DisplayStatus = pydantic.NewModel("DisplayStatus", ClosedConfig,
	pydantic.Req("profile", pydantic.Str),
	pydantic.Req("supported", pydantic.Bool),
	pydantic.Req("installed", pydantic.Bool),
	pydantic.Req("missing", pydantic.List(pydantic.Str)),
	pydantic.Req("running", pydantic.Bool),
	pydantic.Def("pid", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("display", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("socket", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Req("geometry", pydantic.Str),
	pydantic.Def("install_command", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("browser", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("blocker", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("memory_available_mb", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("memory_limit_mb", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("placement", pydantic.Str, "gateway"),
	pydantic.Def("image_switch", pydantic.Nullable(DisplayImageSwitch), nil),
	pydantic.Req("lease", DisplayLease),
	pydantic.Req("profile_key", pydantic.Str),
)

var _ = Method("display.status", ProfileParams, DisplayStatus, "Runtime + lease snapshot for this profile's screen.")

var DisplayThumbnailResult = pydantic.NewModel("DisplayThumbnailResult", ClosedConfig,
	pydantic.Def("data_url", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("suppressed", pydantic.Nullable(pydantic.Str), nil),
)

var _ = Method("display.thumbnail", ProfileParams, DisplayThumbnailResult, "One JPEG grab of the bot's screen; read-only, never changes the lease.")

var _ = Method("display.start", ProfileParams, DisplayStatus, "Start this profile's Xvnc + Xfce (idempotent); blocks until the display is published.")

var DisplayStopParams = pydantic.Extend(ProfileParams, "DisplayStopParams", ParamsConfig,
	pydantic.Def("force", pydantic.Nullable(pydantic.Bool), nil),
)

var DisplayStopResult = pydantic.Extend(DisplayStatus, "DisplayStopResult", ClosedConfig,
	pydantic.Req("stopped", pydantic.Bool),
)

var _ = Method("display.stop", DisplayStopParams, DisplayStopResult, "Stop the screen. Refused (5300, code viewer_mismatch) while a human holds unless force.")

var DisplayObserveParams = pydantic.Extend(ProfileParams, "DisplayObserveParams", ParamsConfig,
	pydantic.Def("viewer_id", pydantic.Nullable(pydantic.Str), nil),
)

var DisplayObserveResult = pydantic.Extend(DisplayStatus, "DisplayObserveResult", ClosedConfig,
	pydantic.Req("ticket", pydantic.Str),
	pydantic.Req("path", pydantic.Str),
	pydantic.Req("viewer_id", pydantic.Str),
)

var _ = Method("display.observe", DisplayObserveParams, DisplayObserveResult, "Mint a single-use ticket for /api/display/ws and the server-minted viewer id for this connection.")

var DisplayInstallResult = pydantic.NewModel("DisplayInstallResult", ClosedConfig,
	pydantic.Req("started", pydantic.Bool),
	pydantic.Def("command", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Req("profile_key", pydantic.Str),
)

var DisplaySwitchSandboxImageParams = pydantic.Extend(ProfileParams, "DisplaySwitchSandboxImageParams", ParamsConfig,
	pydantic.Def("approve", pydantic.Bool, true),
)

var DisplaySwitchSandboxImageResult = pydantic.Extend(DisplayStatus, "DisplaySwitchSandboxImageResult", ClosedConfig,
	pydantic.Req("docker_image", pydantic.Str),
)

var _ = Method("display.switchSandboxImage", DisplaySwitchSandboxImageParams, DisplaySwitchSandboxImageResult, "Decide the pending default sandbox image switch for this profile; refused when none is pending.")

var _ = Method("display.install", ProfileParams, DisplayInstallResult, "Run the distro package install on the gateway host; progress streams as display.install.log/.done.")

var DisplayLeaseAcquireParams = pydantic.Extend(ProfileParams, "DisplayLeaseAcquireParams", ParamsConfig,
	pydantic.Req("viewer_id", pydantic.Str),
	pydantic.Def("reason", pydantic.Nullable(pydantic.Str), nil),
)

var DisplayLeaseResult = pydantic.NewModel("DisplayLeaseResult", ClosedConfig,
	pydantic.Req("lease", DisplayLease),
)

var _ = Method("display.lease.acquire", DisplayLeaseAcquireParams, DisplayLeaseResult, "Take over: the human named by a viewer id this connection minted controls the screen.")

var DisplayLeaseReleaseParams = pydantic.Extend(ProfileParams, "DisplayLeaseReleaseParams", ParamsConfig,
	pydantic.Def("viewer_id", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("force", pydantic.Nullable(pydantic.Bool), nil),
)

var _ = Method("display.lease.release", DisplayLeaseReleaseParams, DisplayLeaseResult, "Hand back. Without a viewer id the release is refused while a human holds unless force.")

var DisplayInstallSudoParams = pydantic.Extend(ServerRequestParams, "DisplayInstallSudoParams", ParamsConfig,
	pydantic.Req("profile_key", pydantic.Str),
)

var _ = ServerRequestOf("display.install.sudo", DisplayInstallSudoParams, ValueResult, "Masked sudo password for the Bot Screen package install; app-level (empty session).")

var DisplayStatusPayload = pydantic.NewModel("DisplayStatusPayload", ClosedConfig,
	pydantic.Req("profile", pydantic.Str),
	pydantic.Req("supported", pydantic.Bool),
	pydantic.Req("installed", pydantic.Bool),
	pydantic.Req("missing", pydantic.List(pydantic.Str)),
	pydantic.Req("running", pydantic.Bool),
	pydantic.Def("pid", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("display", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("socket", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Req("geometry", pydantic.Str),
	pydantic.Def("install_command", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("browser", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("blocker", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("memory_available_mb", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("memory_limit_mb", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("placement", pydantic.Str, "gateway"),
	pydantic.Def("image_switch", pydantic.Nullable(DisplayImageSwitch), nil),
	pydantic.Req("lease", DisplayLease),
	pydantic.Req("profile_key", pydantic.Str),
)

var _ = Event("display.status", DisplayStatusPayload, "This profile's screen started or stopped (also for transitions made outside hermes serve).")

var DisplayLeasePayload = pydantic.NewModel("DisplayLeasePayload", ClosedConfig,
	pydantic.Req("profile_key", pydantic.Str),
	pydantic.Req("lease", DisplayLease),
)

var _ = Event("display.lease", DisplayLeasePayload, "The takeover lease changed hands; every client repaints.")

var DisplayInstallLogPayload = pydantic.NewModel("DisplayInstallLogPayload", ClosedConfig,
	pydantic.Req("profile_key", pydantic.Str),
	pydantic.Req("line", pydantic.Str),
)

var _ = Event("display.install.log", DisplayInstallLogPayload, "One line of package-manager output.")

var DisplayInstallDonePayload = pydantic.NewModel("DisplayInstallDonePayload", ClosedConfig,
	pydantic.Req("profile_key", pydantic.Str),
	pydantic.Req("code", pydantic.Int),
	pydantic.Req("status", DisplayStatus),
)

var _ = Event("display.install.done", DisplayInstallDonePayload, "The install ended (0 ok, -1 cancelled, -2 no sudo: the command to run by hand was streamed).")
