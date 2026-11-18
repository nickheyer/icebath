package tuigateway

import "github.com/nickheyer/icebath/pkg/internal/pydantic"

var SetupFlowStartParams = pydantic.NewModel("SetupFlowStartParams", ParamsConfig,
	pydantic.Req("flow", pydantic.Literal("tools")),
	pydantic.Fac("params", pydantic.Dict(pydantic.Str), pydantic.NewDict),
)

var SetupFlowStartResult = pydantic.NewModel("SetupFlowStartResult", ClosedConfig,
	pydantic.Req("session", pydantic.Str),
)

var _ = Method("setup.flow.start", SetupFlowStartParams, SetupFlowStartResult, "Start a setup flow of the terminal UI on the gateway, one at a time, and return the session that follows it.")

var SetupFlowAnswer = pydantic.NewModel("SetupFlowAnswer", ClosedConfig,
	pydantic.Req("question", pydantic.Int),
	pydantic.Def("index", pydantic.Int, -1),
	pydantic.Fac("indices", pydantic.List(pydantic.Int), pydantic.NewList),
	pydantic.Def("text", pydantic.Str, ""),
	pydantic.Def("cancel", pydantic.Bool, false),
).Described("Answer to the open question: ``index`` of a choice, ``indices`` of a multi choice, ``text`` of a prompt, empty for its default.\n``cancel`` keeps the default of a choice and the preselection of a multi choice, and ends the flow from a prompt.")

var SetupFlowStepParams = pydantic.NewModel("SetupFlowStepParams", ParamsConfig,
	pydantic.Req("session", pydantic.Str),
	pydantic.Def("answer", pydantic.Nullable(SetupFlowAnswer), nil),
)

var SetupFlowStepResult = pydantic.NewModel("SetupFlowStepResult", ClosedConfig,
	pydantic.Req("lines", pydantic.List(pydantic.Str)),
	pydantic.Req("question", pydantic.Nullable(pydantic.Dict(pydantic.Any))),
	pydantic.Req("done", pydantic.Bool),
	pydantic.Req("ok", pydantic.Bool),
	pydantic.Req("error", pydantic.Str),
).Described("``lines`` is the output printed since the last step. ``question`` is the open question, ``kind`` choose, check or text with its ``id``,\n``question`` text, ``options`` and ``default``, ``items``, ``selected`` and ``tokens``, or ``default`` and ``secret``. ``ok`` and ``error`` hold the outcome once ``done``.")

var _ = Method("setup.flow.step", SetupFlowStepParams, SetupFlowStepResult, "Answer the open question of a setup flow and wait up to two seconds for its next output, question or end.")
