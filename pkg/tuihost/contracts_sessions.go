package tuihost

import (
	"github.com/nickheyer/icebath/pkg/internal/pydantic"
)




























































var ContextCategory = pydantic.NewModel("ContextCategory", ClosedConfig,
	pydantic.Req("id", pydantic.Str),
	pydantic.Req("label", pydantic.Str),
	pydantic.Req("tokens", pydantic.Int),
)

var ContextFileSource = pydantic.NewModel("ContextFileSource", ClosedConfig,
	pydantic.Req("label", pydantic.Str),
	pydantic.Req("path", pydantic.Str),
	pydantic.Req("chars", pydantic.Int),
	pydantic.Req("est_tokens", pydantic.Int),
	pydantic.Req("loaded", pydantic.Bool),
	pydantic.Req("status", pydantic.Str),
).Described("One row of ``agent.context_file_sources.list_context_file_sources``.")



var CompressionSummary = pydantic.NewModel("CompressionSummary", OpenConfig,
	pydantic.Def("noop", pydantic.Bool, false),
	pydantic.Def("aborted", pydantic.Bool, false),
	pydantic.Def("refused_would_grow", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("fallback_used", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("headline", pydantic.Str, ""),
	pydantic.Def("token_line", pydantic.Str, ""),
	pydantic.Def("note", pydantic.Nullable(pydantic.Str), nil),
).Described("``agent.manual_compression_feedback.summarize_manual_compression``.")




var SessionInterruptParams = pydantic.Extend(SessionParams, "SessionInterruptParams", ParamsConfig,
	pydantic.Def("expected_hosted_task_id", pydantic.Nullable(pydantic.Str), nil),
)

var InterruptStatus = pydantic.Enum("InterruptStatus", "interrupted", "interrupted", "not_interrupted", "not_interrupted")

var SessionInterruptResult = pydantic.NewModel("SessionInterruptResult", ClosedConfig,
	pydantic.Req("status", InterruptStatus),
	pydantic.Def("interrupted", pydantic.Nullable(pydantic.Bool), nil),
)

var _ = Method("session.interrupt", SessionInterruptParams, SessionInterruptResult, "Stop the running turn (and streaming TTS); retires the crash-recovery marker.")

var CorrectionStatus = pydantic.Enum("CorrectionStatus", "queued", "queued", "redirected", "redirected", "rejected", "rejected")

var SessionCorrectionParams = pydantic.Extend(SessionParams, "SessionCorrectionParams", ParamsConfig,
	pydantic.Req("text", pydantic.Str),
)

var SessionCorrectionResult = pydantic.NewModel("SessionCorrectionResult", ClosedConfig,
	pydantic.Req("status", CorrectionStatus),
	pydantic.Req("text", pydantic.Str),
)

var _ = Method("session.steer", SessionCorrectionParams, SessionCorrectionResult, "Inject text into the next tool result without interrupting the turn.")

var _ = Method("session.redirect", SessionCorrectionParams, SessionCorrectionResult, "Redirect the active turn (queued for the next turn while the agent is still building).")

















