package tuihost

import (
	"github.com/nickheyer/icebath/pkg/internal/pydantic"
)

var CanonicalSessionParams = pydantic.Extend(ProfileParams, "CanonicalSessionParams", ParamsConfig,
	pydantic.Req("session_id", pydantic.Str),
)



var CanonicalSessionHandle = pydantic.NewModel("CanonicalSessionHandle", ClosedConfig,
	pydantic.Req("ref", CanonicalSessionRef),
	pydantic.Req("instance_id", pydantic.Str),
	pydantic.Req("authority_epoch", pydantic.Int),
	pydantic.Req("revision", pydantic.Int),
	pydantic.Req("execution_generation", pydantic.Int),
	pydantic.Req("execution_state", pydantic.Literal("idle", "running", "waiting", "unknown", "terminal")),
)








var CanonicalAttachment = pydantic.NewModel("CanonicalAttachment", ParamsConfig,
	pydantic.Req("path", pydantic.Str),
	pydantic.Req("mime", pydantic.Str),
)

var CanonicalTurnAuthor = pydantic.NewModel("CanonicalTurnAuthor", ParamsConfig,
	pydantic.Def("id", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("name", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("is_bot", pydantic.Bool, false),
	pydantic.Def("origin", pydantic.Nullable(pydantic.Str), nil),
)

var CanonicalModelOverride = pydantic.NewModel("CanonicalModelOverride", ParamsConfig,
	pydantic.Req("model", pydantic.Str),
	pydantic.Def("provider", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("reasoning", pydantic.Nullable(pydantic.Str), nil),
)

var CanonicalSubmitParams = pydantic.Extend(CanonicalSessionParams, "CanonicalSubmitParams", ParamsConfig,
	pydantic.Req("text", pydantic.Str),
	pydantic.Def("submission_id", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("input_id", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("queued", pydantic.Bool, false),
	pydantic.Def("attachments", pydantic.Nullable(pydantic.List(CanonicalAttachment)), nil),
	pydantic.Def("finite", pydantic.Bool, false),
	pydantic.Def("unattended", pydantic.Bool, false),
	pydantic.Def("surface", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("voice_context", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("interrupted", pydantic.Bool, false),
	pydantic.Def("voice_turn", pydantic.Bool, false),
	pydantic.Def("display_kind", pydantic.Nullable(pydantic.Literal("hidden")), nil),
	pydantic.Def("title_preview", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("turn_author", pydantic.Nullable(CanonicalTurnAuthor), nil),
	pydantic.Def("api_text", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("model_override", pydantic.Nullable(CanonicalModelOverride), nil),
	pydantic.Def("clarify_batch", pydantic.Bool, false),
)

var CanonicalVoiceStopped = pydantic.NewModel("CanonicalVoiceStopped", ClosedConfig,
	pydantic.Req("voice_stopped", pydantic.Literal(true)),
).Described("A bare stop phrase typed while the session's voice chat is on: the chat ended, nothing was\nadmitted (TUI-26).")

var CanonicalSubmitResult = pydantic.NewRootModel("CanonicalSubmitResult", pydantic.Union(AdmissionStatus, CanonicalVoiceStopped)).Described("``prompt.submit``: the admission receipt, or ``{voice_stopped: true}``.")


var CanonicalReplaceQueuedParams = pydantic.Extend(CanonicalSessionParams, "CanonicalReplaceQueuedParams", ParamsConfig,
	pydantic.Req("admission_id", pydantic.Str),
	pydantic.Req("submission_id", pydantic.Str),
	pydantic.Req("text", pydantic.Str),
	pydantic.Def("attachments", pydantic.Nullable(pydantic.List(CanonicalAttachment)), nil),
	pydantic.Def("display_kind", pydantic.Nullable(pydantic.Literal("hidden")), nil),
	pydantic.Def("title_preview", pydantic.Nullable(pydantic.Str), nil),
).Described("The edited prompt: admitted as a NEW admission (``submission_id``) at the FIFO tail.")

var CanonicalControlParams = pydantic.Extend(CanonicalSessionParams, "CanonicalControlParams", ParamsConfig,
	pydantic.Req("execution_generation", pydantic.Int),
)

var CanonicalCorrectionParams = pydantic.Extend(CanonicalControlParams, "CanonicalCorrectionParams", ParamsConfig,
	pydantic.Req("text", pydantic.Str),
)

var CanonicalCorrectionAck = pydantic.NewModel("CanonicalCorrectionAck", ClosedConfig,
	pydantic.Req("status", pydantic.Literal("queued", "redirected", "rejected")),
	pydantic.Req("text", pydantic.Str),
	pydantic.Req("execution_generation", pydantic.Int),
	pydantic.Req("authority_epoch", pydantic.Int),
	pydantic.Def("delivered", pydantic.Nullable(pydantic.Literal(false)), nil),
)

var CanonicalCorrectionResult = pydantic.NewRootModel("CanonicalCorrectionResult", pydantic.Union(CanonicalCorrectionAck, CanonicalVoiceStopped)).Described("``session.steer`` / ``session.redirect``: the correction verdict, or ``{voice_stopped: true}``\nwhen the busy-time text was a bare stop phrase that ended the LOCAL voice chat.")

var CanonicalApprovalParams = pydantic.Extend(CanonicalControlParams, "CanonicalApprovalParams", ParamsConfig,
	pydantic.Req("prompt_id", pydantic.Str),
	pydantic.Req("choice", pydantic.Str),
)







var _ = CanonicalMethod("session.interrupt", CanonicalControlParams, CanonicalSessionHandle, "")

var _ = CanonicalMethod("session.steer", CanonicalCorrectionParams, CanonicalCorrectionResult, "")

var _ = CanonicalMethod("session.redirect", CanonicalCorrectionParams, CanonicalCorrectionResult, "")

var _ = CanonicalMethod("prompt.submit", CanonicalSubmitParams, CanonicalSubmitResult, "")

var _ = CanonicalMethod("approval.respond", CanonicalApprovalParams, PromptResponseResult, "")

var _ = CanonicalMethod("prompt.replace_queued", CanonicalReplaceQueuedParams, AdmissionStatus, "Cancel a still-queued admission and admit the edited prompt as a new admission at the back of the FIFO, in one write bound to the replaced admission. Refusals: admission_started, admission_conflict (the submission_id replaced another admission), queue_full, unsupported_control, not_found.")
