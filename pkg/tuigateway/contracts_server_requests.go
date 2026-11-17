package tuigateway

import (
	"github.com/nickheyer/icebath/pkg/internal/pydantic"
)

var ServerRequestParams = pydantic.NewModel("ServerRequestParams", ParamsConfig,
	pydantic.Req("session_id", pydantic.Str),
)

var ValueResult = pydantic.NewModel("ValueResult", ClosedConfig,
	pydantic.Req("value", pydantic.Str),
).Described("The answer to any one-string prompt (sudo, secret, vault prompts):\n``''`` means skipped / declined.")

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
).Described("``{answers}`` for the whole set (early locks go through the ``clarify.lock`` RPC); a response\nwithout ``answers`` is cancel-all. ``outcome`` (optional): how the viewer's own panel ended\n(``timed_out`` keeps the answers locked before its deadline).")

var _ = ServerRequestOf("clarify", ClarifyRequestParams, ClarifyResult, "The clarify tool: ask the user 1-5 questions.")

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

var RequestCancelReason = pydantic.Enum("RequestCancelReason", "timeout", "timeout", "interrupted", "interrupted", "shutdown", "shutdown", "resolved", "resolved", "session_closed", "session_closed")

var RequestCancelPayload = pydantic.NewModel("RequestCancelPayload", ClosedConfig,
	pydantic.Req("id", pydantic.Str),
	pydantic.Req("method", pydantic.Str),
	pydantic.Req("reason", pydantic.Str),
)

var _ = Event("request.cancel", RequestCancelPayload, "The backend withdrew an open server→client request; clear the matching card only.")
