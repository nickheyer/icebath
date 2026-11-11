package tuigateway

import (
	"github.com/nickheyer/icebath/pkg/internal/pydantic"
)

var LocaleSurface = pydantic.Enum("LocaleSurface", "core", "core", "tui", "tui", "desktop", "desktop")

var LanguageOption = pydantic.NewModel("LanguageOption", ClosedConfig,
	pydantic.Req("id", pydantic.Str),
	pydantic.Req("endonym", pydantic.Str),
	pydantic.Req("rtl", pydantic.Bool),
	pydantic.Req("source", pydantic.Str),
	pydantic.Req("surfaces", pydantic.List(LocaleSurface)),
)

var I18nLanguagesResult = pydantic.NewModel("I18nLanguagesResult", ClosedConfig,
	pydantic.Req("languages", pydantic.List(LanguageOption)),
)

var I18nCatalogParams = pydantic.Extend(ProfileParams, "I18nCatalogParams", ParamsConfig,
	pydantic.Req("lang", pydantic.Str),
	pydantic.Def("surface", LocaleSurface, "core"),
)

var I18nCatalogResult = pydantic.NewModel("I18nCatalogResult", ClosedConfig,
	pydantic.Req("lang", pydantic.Str),
	pydantic.Req("surface", LocaleSurface),
	pydantic.Req("messages", pydantic.Dict(pydantic.Str)),
)

var _ = Method("i18n.languages", ProfileParams, I18nLanguagesResult, "Every language some layer supplies (bundled ∪ user overlay ∪ plugin packs), en first.")

var _ = Method("i18n.catalog", I18nCatalogParams, I18nCatalogResult, "Pack + overlay messages for one language and surface; the renderer merges them over its bundled catalog.")
