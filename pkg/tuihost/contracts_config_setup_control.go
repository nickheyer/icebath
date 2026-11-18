package tuihost

import (
	"github.com/nickheyer/icebath/pkg/internal/pydantic"
)

var ConfigGetParams = pydantic.Extend(ProfileParams, "ConfigGetParams", ParamsConfig,
	pydantic.Req("key", pydantic.Str),
	pydantic.Def("cwd", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("session_id", pydantic.Nullable(pydantic.Str), nil),
).Described("``key`` selects one getter from ``_CONFIG_GETTERS``; ``cwd`` feeds the ``project`` getter,\n``session_id`` lets ``reasoning`` / ``fast`` answer with the session's live pin.")

var ConfigProviderRef = pydantic.NewModel("ConfigProviderRef", OpenConfig,
	pydantic.Req("id", pydantic.Str),
	pydantic.Req("label", pydantic.Str),
	pydantic.Fac("aliases", pydantic.List(pydantic.Str), pydantic.NewList),
	pydantic.Def("authenticated", pydantic.Bool, false),
).Described("``hermes_cli/models.py::list_available_providers`` row.")

var ConfigGetResult = pydantic.NewModel("ConfigGetResult", ClosedConfig,
	pydantic.Def("value", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("display", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("tool_progress", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("model", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("provider", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("providers", pydantic.Nullable(pydantic.List(ConfigProviderRef)), nil),
	pydantic.Def("home", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("cwd", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("branch", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("config", pydantic.Nullable(pydantic.Dict(pydantic.Any)), nil),
	pydantic.Def("prompt", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("mtime", pydantic.Nullable(pydantic.Float), nil),
	pydantic.Def("mcp_rev", pydantic.Nullable(pydantic.Str), nil),
).Described("Union of every getter's payload: ``value`` for the simple words, ``config`` for ``full``,\n``mtime`` / ``mcp_rev`` for the poller, ``model`` / ``provider`` / ``providers`` for ``provider``,\n``home`` / ``display`` for ``profile``, ``cwd`` / ``branch`` for ``project``, ``prompt``.")

var _ = Method("config.get", ConfigGetParams, ConfigGetResult, "Read one normalised config value (or the whole effective config) the way the UIs render it.")

var ConfigSetParams = pydantic.Extend(ProfileParams, "ConfigSetParams", ParamsConfig,
	pydantic.Req("key", pydantic.Str),
	pydantic.Def("value", pydantic.Any, ""),
	pydantic.Def("session_id", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("scope", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("confirm_expensive_model", pydantic.Bool, false),
).Described("``key`` picks the setter (``_CONFIG_SETTERS``, ``details_mode.<section>``, display toggles);\n``value`` is the raw word/string the setter normalises (falsy non-strings are reported back in\nthe error). ``scope`` applies to ``yolo`` / ``reasoning``; ``confirm_expensive_model`` to ``model``.")

var ConfigSetResult = pydantic.NewModel("ConfigSetResult", ClosedConfig,
	pydantic.Req("key", pydantic.Str),
	pydantic.Def("value", pydantic.Nullable(pydantic.Union(pydantic.Str, pydantic.Bool)), nil),
	pydantic.Def("warning", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("confirm_required", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("confirm_message", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("scope", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("deferred", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("tool_progress", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("cwd", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("branch", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("history_reset", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("info", pydantic.Nullable(SessionLiveInfo), nil),
).Described("``{key, value}`` plus the setter's extras: model switches add ``warning`` /\n``confirm_required`` / ``confirm_message`` / ``scope`` / ``deferred``; ``focus`` adds\n``tool_progress``; ``cwd`` adds ``cwd`` / ``branch``; ``personality`` adds ``history_reset`` /\n``info``; ``yolo`` reports its ``scope``. ``value`` is a bool only for the display toggles.")

var _ = Method("config.set", ConfigSetParams, ConfigSetResult, "Change one config key (persisted or session-scoped) and read back the normalised value.")


