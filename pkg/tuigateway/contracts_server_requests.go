package tuigateway

import (
	"github.com/nickheyer/icebath/pkg/internal/pydantic"
)

var ServerRequestParams = pydantic.NewModel("ServerRequestParams", ParamsConfig,
	pydantic.Req("session_id", pydantic.Str),
)

var ValueResult = pydantic.NewModel("ValueResult", ClosedConfig,
	pydantic.Req("value", pydantic.Str),
).Described("The answer to any one-string prompt (sudo, secret, vault prompts, desktop bridges):\n``''`` means skipped / declined.")

var ClarifyQuestion = pydantic.NewModel("ClarifyQuestion", ParamsConfig,
	pydantic.Req("qid", pydantic.Str),
	pydantic.Req("question", pydantic.Str),
	pydantic.Def("choices", pydantic.Nullable(pydantic.List(pydantic.Str)), nil),
	pydantic.Def("multi_select", pydantic.Bool, false),
)

var ClarifyRequestParams = pydantic.Extend(ServerRequestParams, "ClarifyRequestParams", ParamsConfig,
	pydantic.Req("questions", pydantic.List(ClarifyQuestion)),
	pydantic.Def("answers", pydantic.Nullable(pydantic.Dict(pydantic.Nullable(pydantic.Str))), nil),
).Described("``answers`` rides only on a reconnect replay (locks the server already accepted; null = skipped).")

var ClarifyResult = pydantic.NewModel("ClarifyResult", ClosedConfig,
	pydantic.Def("answers", pydantic.Nullable(pydantic.Dict(pydantic.Nullable(pydantic.Str))), nil),
	pydantic.Def("outcome", pydantic.Nullable(pydantic.Literal("submitted", "timed_out", "cancelled")), nil),
).Described("``{answers}`` for the whole set (early locks go through the ``clarify.lock`` RPC); a response\nwithout ``answers`` is cancel-all. ``outcome`` (optional): how the viewer's own panel ended, as\nmain's classic CLI reports it (``timed_out`` keeps the answers locked before its deadline).")

var _ = ServerRequestOf("clarify", ClarifyRequestParams, ClarifyResult, "The clarify tool: ask the user 1-5 questions.")

var SetupBeat = pydantic.Enum("SetupBeat", "name", "name", "theme", "theme", "layout", "layout", "connectors", "connectors", "plugins", "plugins", "tour", "tour", "build", "build")

var SetupOption = pydantic.NewModel("SetupOption", ParamsConfig,
	pydantic.Req("id", pydantic.Str),
	pydantic.Req("label", pydantic.Str),
)

var SetupNameCard = pydantic.NewModel("SetupNameCard", ParamsConfig,
	pydantic.Def("suggested", pydantic.Nullable(pydantic.Str), nil),
)

var SetupThemeCard = pydantic.NewModel("SetupThemeCard", ParamsConfig,
	pydantic.Req("picker", pydantic.Str),
)

var SetupOptionsCard = pydantic.NewModel("SetupOptionsCard", ParamsConfig,
	pydantic.Req("options", pydantic.List(SetupOption)),
)

var SetupConnectorsCard = pydantic.NewModel("SetupConnectorsCard", ParamsConfig,
	pydantic.Req("suggested", pydantic.List(pydantic.Str)),
)

var SetupPluginsCard = pydantic.NewModel("SetupPluginsCard", ParamsConfig,
	pydantic.Req("plugins", pydantic.List(pydantic.Str)),
)

var SetupBeatRequestParams = pydantic.Extend(ServerRequestParams, "SetupBeatRequestParams", ParamsConfig,
	pydantic.Req("beat", SetupBeat),
	pydantic.Def("question", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("tool_call_id", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Req("card", pydantic.Union(SetupNameCard, SetupThemeCard, SetupOptionsCard, SetupConnectorsCard, SetupPluginsCard)),
).Described("``card`` is the beat's data (``agent/onboarding_flow.py::BEATS``): name, theme, connectors and\nplugins have their own shape; layout, tour and build carry ``options``. ``tool_call_id`` is the\nsetup_beat call the card belongs to: only that transcript row draws it.")

var SetupNameAnswer = pydantic.NewModel("SetupNameAnswer", ClosedConfig,
	pydantic.Req("name", pydantic.Str),
)

var SetupThemeAnswer = pydantic.NewModel("SetupThemeAnswer", ClosedConfig,
	pydantic.Def("accent", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("mode", pydantic.Nullable(pydantic.Str), nil),
)

var SetupLayoutAnswer = pydantic.NewModel("SetupLayoutAnswer", ClosedConfig,
	pydantic.Req("layout", pydantic.Str),
)

var SetupConnectorsAnswer = pydantic.NewModel("SetupConnectorsAnswer", ClosedConfig,
	pydantic.Req("connected", pydantic.List(pydantic.Str)),
	pydantic.Fac("newly_connected", pydantic.List(pydantic.Str), pydantic.NewList),
)

var SetupPluginsAnswer = pydantic.NewModel("SetupPluginsAnswer", ClosedConfig,
	pydantic.Req("installed", pydantic.List(pydantic.Str)),
	pydantic.Def("label", pydantic.Nullable(pydantic.Str), nil),
)

var SetupTourAnswer = pydantic.NewModel("SetupTourAnswer", ClosedConfig,
	pydantic.Req("tour", pydantic.Str),
	pydantic.Def("label", pydantic.Nullable(pydantic.Str), nil),
)

var SetupBuildAnswer = pydantic.NewModel("SetupBuildAnswer", ClosedConfig,
	pydantic.Req("build", pydantic.Str),
	pydantic.Def("label", pydantic.Nullable(pydantic.Str), nil),
)

var SetupBeatResult = pydantic.NewModel("SetupBeatResult", ClosedConfig,
	pydantic.Def("answer", pydantic.Nullable(pydantic.Union(SetupNameAnswer, SetupThemeAnswer, SetupLayoutAnswer, SetupConnectorsAnswer, SetupPluginsAnswer, SetupTourAnswer, SetupBuildAnswer)), nil),
	pydantic.Def("said", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("wrote_instead", pydantic.Bool, false),
).Described("``answer`` holds the shown beat's picks; ids must be one of the card's options (the backend\nrejects any other answer). ``said`` is composer text typed while the name card waited: the name.\n``wrote_instead``: the user sent a message instead of answering; the backend ends the turn and the\nclient sends that message as the next one. A response with none of these, or none within 300 s,\ncloses the card unanswered.")

var _ = ServerRequestOf("setup_beat", SetupBeatRequestParams, SetupBeatResult, "The onboarding chat's card for one beat; blocks setup_beat until answered (300 s).")

var ApprovalChoice = pydantic.Enum("ApprovalChoice", "once", "once", "session", "session", "always", "always", "deny", "deny")

var ApprovalRequestParams = pydantic.Extend(ServerRequestParams, "ApprovalRequestParams", OpenParamsConfig,
	pydantic.Req("request_id", pydantic.Str),
	pydantic.Def("command", pydantic.Str, ""),
	pydantic.Def("description", pydantic.Str, ""),
	pydantic.Fac("choices", pydantic.List(ApprovalChoice), pydantic.NewList),
	pydantic.Def("allow_permanent", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("allow_session", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("smart_denied", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("tool_name", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("session_id_hint", pydantic.Nullable(pydantic.Str), nil).Alias("gateway_session_id"),
).Described("``tui_gateway/server.py::_approval_request_payload`` — the command is redacted server-side.")

var ApprovalResult = pydantic.NewModel("ApprovalResult", ClosedConfig,
	pydantic.Req("choice", ApprovalChoice),
	pydantic.Def("all", pydantic.Nullable(pydantic.Bool), nil),
)

var _ = ServerRequestOf("approval", ApprovalRequestParams, ApprovalResult, "A dangerous command awaits the user's decision.")

var EmptyRequestParams = pydantic.Extend(ServerRequestParams, "EmptyRequestParams", ParamsConfig)

var SudoRequestParams = pydantic.Extend(ServerRequestParams, "SudoRequestParams", ParamsConfig,
	pydantic.Def("command", pydantic.Str, ""),
).Described("Original command, redacted server-side before any password-injection rewrite.")

var _ = ServerRequestOf("sudo", SudoRequestParams, ValueResult, "Masked sudo password for the terminal tool.")

var SecretRequestParams = pydantic.Extend(ServerRequestParams, "SecretRequestParams", ParamsConfig,
	pydantic.Req("env_var", pydantic.Str),
	pydantic.Req("prompt", pydantic.Str),
	pydantic.Def("metadata", pydantic.Nullable(pydantic.Dict(pydantic.Any)), nil),
)

var _ = ServerRequestOf("secret", SecretRequestParams, ValueResult, "Masked value for a named env var (skills / setup flows).")

var VaultUnlockRequestParams = pydantic.Extend(ServerRequestParams, "VaultUnlockRequestParams", ParamsConfig,
	pydantic.Req("backend", pydantic.Str),
	pydantic.Req("display_name", pydantic.Str),
)

var _ = ServerRequestOf("vault.unlock_prompt", VaultUnlockRequestParams, ValueResult, "Master password to unlock an external password manager for this session.")

var VaultSaveLoginRequestParams = pydantic.Extend(ServerRequestParams, "VaultSaveLoginRequestParams", ParamsConfig,
	pydantic.Req("origin", pydantic.Str),
	pydantic.Req("site", pydantic.Str),
)

var _ = ServerRequestOf("vault.save_login", VaultSaveLoginRequestParams, ValueResult, "Save a login for a site the agent is about to fill; the answer is JSON {identifier, password}.")

var VaultCodeRequestParams = pydantic.Extend(ServerRequestParams, "VaultCodeRequestParams", ParamsConfig,
	pydantic.Def("site", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("hint", pydantic.Nullable(pydantic.Str), nil),
)

var _ = ServerRequestOf("vault.code", VaultCodeRequestParams, ValueResult, "A one-time / 2FA code the user reads from their device.")

var ReadRangeRequestParams = pydantic.Extend(ServerRequestParams, "ReadRangeRequestParams", ParamsConfig,
	pydantic.Def("start", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("count", pydantic.Nullable(pydantic.Int), nil),
)

var _ = ServerRequestOf("terminal.read", ReadRangeRequestParams, ValueResult, "Read the visible in-app terminal buffer (JSON text answer).")

var _ = ServerRequestOf("preview.read", ReadRangeRequestParams, ValueResult, "Read the in-app browser preview's text (JSON text answer).")

var _ = ServerRequestOf("window.read", EmptyRequestParams, ValueResult, "Enumerate the native window below the app (JSON text answer).")

var PreviewActRequestParams = pydantic.Extend(ServerRequestParams, "PreviewActRequestParams", ParamsConfig,
	pydantic.Req("action", pydantic.Str),
	pydantic.Def("ref", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("selector", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("text", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("key", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("submit", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("full", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("to", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("amount", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("max", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("allow_shortcut", pydantic.Nullable(pydantic.Bool), nil),
).Described("``tools/drive_preview_tool.py`` and ``tools/annotate_preview_tool.py`` field sets.")

var _ = ServerRequestOf("preview.act", PreviewActRequestParams, ValueResult, "Click / type / scroll / annotate inside the in-app browser preview.")

var TourStep = pydantic.NewModel("TourStep", OpenParamsConfig,
	pydantic.Def("selector", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("title", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("text", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("side", pydantic.Nullable(pydantic.Str), nil),
)

var TourRequestParams = pydantic.Extend(ServerRequestParams, "TourRequestParams", ParamsConfig,
	pydantic.Req("action", pydantic.Str),
	pydantic.Def("surface", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("selector", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("title", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("text", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("side", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("steps", pydantic.Nullable(pydantic.List(TourStep)), nil),
	pydantic.Def("step_index", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("preset", pydantic.Nullable(TourPreset), nil),
).Described("``tools/tour_tool.py`` field set.")

var _ = ServerRequestOf("tour", TourRequestParams, ValueResult, "Drive a guided tour highlight in the desktop renderer.")

var ThemeMode = pydantic.Enum("ThemeMode", "light", "light", "dark", "dark", "system", "system")

var ThemePicker = pydantic.Enum("ThemePicker", "accent", "accent", "layout", "layout")

var ThemeApplyRequestParams = pydantic.Extend(ServerRequestParams, "ThemeApplyRequestParams", ParamsConfig,
	pydantic.Def("accent", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("mode", pydantic.Nullable(ThemeMode), nil),
	pydantic.Def("layout", pydantic.Nullable(pydantic.Str), nil),
).Described("``tools/set_theme_tool.py`` values; accent is a swatch id, ``theme`` or ``#rrggbb``.")

var ThemePickRequestParams = pydantic.Extend(ServerRequestParams, "ThemePickRequestParams", ParamsConfig,
	pydantic.Req("picker", ThemePicker),
)

var ThemeValues = pydantic.NewModel("ThemeValues", ParamsConfig,
	pydantic.Def("accent", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("mode", pydantic.Nullable(ThemeMode), nil),
	pydantic.Def("layout", pydantic.Nullable(pydantic.Str), nil),
).Described("What the renderer applied. ``accent`` is a swatch id, ``custom:#rrggbb``, or null for the theme's own.")

var ThemePickResult = pydantic.NewModel("ThemePickResult", ClosedConfig,
	pydantic.Def("picked", pydantic.Nullable(ThemeValues), nil),
	pydantic.Def("said", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("cancelled", pydantic.Nullable(pydantic.Bool), nil),
).Described("``picked`` once the user chose (already applied); ``cancelled`` when the card was dismissed. A response\nwith neither is a cancel.")

var _ = ServerRequestOf("theme.apply", ThemeApplyRequestParams, ValueResult, "Apply set_theme values in the window showing the session (JSON text answer).")

var _ = ServerRequestOf("theme.pick", ThemePickRequestParams, ThemePickResult, "Show an accent or layout picker card in the chat and wait for the pick.")

var RequestCancelReason = pydantic.Enum("RequestCancelReason", "timeout", "timeout", "interrupted", "interrupted", "shutdown", "shutdown", "resolved", "resolved", "session_closed", "session_closed")

var RequestCancelPayload = pydantic.NewModel("RequestCancelPayload", ClosedConfig,
	pydantic.Req("id", pydantic.Str),
	pydantic.Req("method", pydantic.Str),
	pydantic.Req("reason", pydantic.Str),
)

var _ = Event("request.cancel", RequestCancelPayload, "The backend withdrew an open server→client request; clear the matching card only.")

var TourPreset = pydantic.Enum("TourPreset", "quick", "quick", "full", "full").Described("Which built-in tour ``start`` without steps runs.")
