package tuihost

import (
	"github.com/nickheyer/icebath/pkg/internal/pydantic"
)

var ProjectFolder = pydantic.NewModel("ProjectFolder", ClosedConfig,
	pydantic.Req("path", pydantic.Str),
	pydantic.Def("label", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("is_primary", pydantic.Bool, false),
	pydantic.Def("added_at", pydantic.Nullable(pydantic.Int), nil),
).Described("``hermes_cli/projects_db.py::ProjectFolder.to_dict``.")

