package tuigateway

import "github.com/nickheyer/icebath/pkg/internal/pydantic"

var SetupHostedStartParams = pydantic.NewModel("SetupHostedStartParams", ParamsConfig,
	pydantic.Req("flow", pydantic.Literal("model", "gateway", "import", "quick", "blank")),
	pydantic.Def("entry", pydantic.Str, ""),
).Described("``flow`` names the section: ``model`` the model and provider section, ``gateway`` the messaging platforms section,\n``import`` the agent import step, ``quick`` Quick Setup and ``blank`` Blank Slate, the ``/setup`` sections of the terminal UI.\n``entry`` opens the model section at one row of the provider list, a provider slug, ``custom``, ``llamacpp``,\n``aux-config`` or ``reasoning``; empty opens the list. The other flows take no entry.")

var SetupHostedStartResult = pydantic.NewModel("SetupHostedStartResult", ClosedConfig,
	pydantic.Req("job", pydantic.Str),
)

var _ = Method("setup.hosted.start", SetupHostedStartParams, SetupHostedStartResult, "Run a section or first run mode of setup on the gateway with its questions put to the terminal UI, stopping a hosted section still running.")

var SetupHostedHeading = pydantic.NewModel("SetupHostedHeading", ClosedConfig,
	pydantic.Req("at", pydantic.Int),
	pydantic.Req("text", pydantic.Str),
)

var SetupHostedQuestion = pydantic.NewModel("SetupHostedQuestion", ClosedConfig,
	pydantic.Req("id", pydantic.Int),
	pydantic.Req("kind", pydantic.Literal("choice", "checklist", "text", "secret", "command")),
	pydantic.Req("title", pydantic.Str),
	pydantic.Req("description", pydantic.Str),
	pydantic.Req("rows", pydantic.List(pydantic.Str)),
	pydantic.Req("headings", pydantic.List(SetupHostedHeading)),
	pydantic.Req("default", pydantic.Int),
	pydantic.Req("picked", pydantic.List(pydantic.Int)),
	pydantic.Req("argv", pydantic.List(pydantic.Str)),
).Described("A ``choice`` picks one of ``rows`` starting at ``default``, a ``checklist`` ticks rows starting from ``picked``,\n``text`` and ``secret`` take a typed line, a ``command`` runs ``argv`` on the user's terminal.\n``headings`` name the group starting at row ``at``.")

var SetupHostedPollParams = pydantic.NewModel("SetupHostedPollParams", ParamsConfig,
	pydantic.Req("job", pydantic.Str),
	pydantic.Req("after", pydantic.Int),
)

var SetupHostedPollResult = pydantic.NewModel("SetupHostedPollResult", ClosedConfig,
	pydantic.Req("lines", pydantic.List(pydantic.Str)),
	pydantic.Req("question", pydantic.Nullable(SetupHostedQuestion)),
	pydantic.Req("done", pydantic.Bool),
	pydantic.Req("ok", pydantic.Bool),
	pydantic.Req("model", pydantic.Str),
	pydantic.Req("provider", pydantic.Str),
	pydantic.Req("changed", pydantic.Bool),
	pydantic.Req("progress", pydantic.Str),
).Described("``lines`` is the output past the first ``after`` lines, ``question`` the one the section waits on, ``progress`` the line a progress bar is rewriting.\nOnce ``done``, ``model`` and ``provider`` are the default model and provider config.yaml holds and ``changed``\ntells whether the section changed the model, provider or base URL.")

var _ = Method("setup.hosted.poll", SetupHostedPollParams, SetupHostedPollResult, "Wait up to two seconds for output, a question or the end of a hosted setup section.")

var SetupHostedAnswerParams = pydantic.NewModel("SetupHostedAnswerParams", ParamsConfig,
	pydantic.Req("job", pydantic.Str),
	pydantic.Req("question", pydantic.Int),
	pydantic.Def("cancelled", pydantic.Bool, false),
	pydantic.Def("index", pydantic.Int, 0),
	pydantic.Fac("picked", pydantic.List(pydantic.Int), pydantic.NewList),
	pydantic.Def("text", pydantic.Str, ""),
	pydantic.Def("ran", pydantic.Bool, false),
	pydantic.Def("error", pydantic.Str, ""),
).Described("``cancelled`` backs out of the question; else ``index`` answers a choice, ``picked`` a checklist,\n``text`` a typed line, and ``ran`` a command that exited on its own or ``error`` one that could not start.")

var SetupHostedAnswerResult = pydantic.NewModel("SetupHostedAnswerResult", ClosedConfig)

var _ = Method("setup.hosted.answer", SetupHostedAnswerParams, SetupHostedAnswerResult, "Answer the question a hosted setup section waits on.")

var SetupHostedCancelParams = pydantic.NewModel("SetupHostedCancelParams", ParamsConfig,
	pydantic.Req("job", pydantic.Str),
)

var SetupHostedCancelResult = pydantic.NewModel("SetupHostedCancelResult", ClosedConfig)

var _ = Method("setup.hosted.cancel", SetupHostedCancelParams, SetupHostedCancelResult, "Stop a hosted setup section, its open question backed out of and its sign in waits ended.")
