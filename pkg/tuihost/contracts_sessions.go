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





























