package tuihost

import "github.com/nickheyer/icebath/pkg/internal/pydantic"

var LinesResult = pydantic.NewModel("LinesResult", ClosedConfig,
	pydantic.Req("lines", pydantic.List(pydantic.Str)),
).Described("Text lines in the profile's language, shown as they come.")

var ToolsListParams = pydantic.NewModel("ToolsListParams", ParamsConfig,
	pydantic.Def("platform", pydantic.Str, "cli"),
)

var _ = Method("tools.list", ToolsListParams, LinesResult, "Every toolset and MCP tool of a platform with its enabled or disabled status.")
