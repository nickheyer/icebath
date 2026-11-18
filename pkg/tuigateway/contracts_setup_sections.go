package tuigateway

import "github.com/nickheyer/icebath/pkg/internal/pydantic"

var SetupSectionLoadParams = pydantic.NewModel("SetupSectionLoadParams", ParamsConfig,
	pydantic.Fac("config_keys", pydantic.List(pydantic.Str), pydantic.NewList),
	pydantic.Fac("env_keys", pydantic.List(pydantic.Str), pydantic.NewList),
	pydantic.Fac("secret_config_keys", pydantic.List(pydantic.Str), pydantic.NewList),
	pydantic.Fac("secret_env_keys", pydantic.List(pydantic.Str), pydantic.NewList),
	pydantic.Fac("facts", pydantic.List(pydantic.Str), pydantic.NewList),
	pydantic.Fac("values", pydantic.List(pydantic.Str), pydantic.NewList),
)

var SetupSectionLoadResult = pydantic.NewModel("SetupSectionLoadResult", ClosedConfig,
	pydantic.Req("config", pydantic.Dict(pydantic.Any)),
	pydantic.Req("env", pydantic.Dict(pydantic.Str)),
	pydantic.Req("secrets", pydantic.List(pydantic.Str)),
	pydantic.Req("facts", pydantic.Dict(pydantic.Bool)),
	pydantic.Req("values", pydantic.Dict(pydantic.Any)),
).Described("``config`` and ``env`` hold each asked key that has a value, config keys with the defaults merged.\n``secrets`` names the asked secret keys that hold a value and never carries one.\n``facts`` answers each asked fact: ``env:KEY`` a non empty key, ``extra:NAME`` an engine this platform supports, ``module:NAME`` an installed engine, ``binary:NAME`` a program on the path, ``nous.managed_tools``, ``xai.oauth``.\n``values`` holds each asked named value a section probes on the gateway side, a backend list or a found program.")

var _ = Method("setup.section.load", SetupSectionLoadParams, SetupSectionLoadResult, "Read the values a setup section of the terminal UI starts from.")

var SetupSectionWrite = pydantic.NewModel("SetupSectionWrite", ClosedConfig,
	pydantic.Req("target", pydantic.Literal("config", "env")),
	pydantic.Req("key", pydantic.Str),
	pydantic.Req("kind", pydantic.Literal("bool", "whole", "decimal", "text", "held")),
	pydantic.Req("value", pydantic.Any),
).Described("One answer: a dotted ``config.yaml`` key or a ``.env`` key, with the type ``value`` is stored as.\nAn ``env`` target takes ``text`` only, or ``held`` that saves the value the key already resolves to from the environment or ``.env``.")

var SetupSectionRemoval = pydantic.NewModel("SetupSectionRemoval", ClosedConfig,
	pydantic.Req("target", pydantic.Literal("config", "env")),
	pydantic.Req("key", pydantic.Str),
)

var SetupSectionSaveParams = pydantic.NewModel("SetupSectionSaveParams", ParamsConfig,
	pydantic.Fac("writes", pydantic.List(SetupSectionWrite), pydantic.NewList),
	pydantic.Fac("removals", pydantic.List(SetupSectionRemoval), pydantic.NewList),
	pydantic.Def("full_save", pydantic.Bool, false),
)

var SetupSectionSaveResult = pydantic.NewModel("SetupSectionSaveResult", ClosedConfig,
	pydantic.Req("config", pydantic.Dict(pydantic.Any)),
	pydantic.Req("env", pydantic.List(pydantic.Str)),
	pydantic.Req("removed", pydantic.List(pydantic.Str)),
).Described("``config`` is what ``config.yaml`` holds for each written key after the save, ``env`` the ``.env`` keys saved,\n``removed`` the keys of either file that existed and are gone.")

var _ = Method("setup.section.save", SetupSectionSaveParams, SetupSectionSaveResult, "Write the answers of one setup section to config.yaml and .env; the shared metrics consent keys go through the consent writer.\n``full_save`` rewrites config.yaml through the whole config save after a pre-setup backup, defaults stripped as the setup wizard saved them, even with no answer.")

var SetupActionStartParams = pydantic.NewModel("SetupActionStartParams", ParamsConfig,
	pydantic.Req("action", pydantic.Literal("xai.oauth.login", "tts.install.neutts", "tts.install.kittentts", "terminal.ssh.test", "terminal.backend.plugin")),
	pydantic.Fac("params", pydantic.Dict(pydantic.Str), pydantic.NewDict),
)

var SetupActionStartResult = pydantic.NewModel("SetupActionStartResult", ClosedConfig,
	pydantic.Req("job", pydantic.Str),
)

var _ = Method("setup.action.start", SetupActionStartParams, SetupActionStartResult, "Start a side effect of a setup section, a login or an engine install, and return the job that follows it.")

var SetupActionPollParams = pydantic.NewModel("SetupActionPollParams", ParamsConfig,
	pydantic.Req("job", pydantic.Str),
	pydantic.Req("after", pydantic.Int),
)

var SetupActionPollResult = pydantic.NewModel("SetupActionPollResult", ClosedConfig,
	pydantic.Req("lines", pydantic.List(pydantic.Str)),
	pydantic.Req("done", pydantic.Bool),
	pydantic.Req("ok", pydantic.Bool),
).Described("``lines`` is the output the job printed past the first ``after`` lines; ``ok`` is its outcome once ``done`` is true.")

var _ = Method("setup.action.poll", SetupActionPollParams, SetupActionPollResult, "Wait up to two seconds for new output of a setup job and return it with the end state.")
