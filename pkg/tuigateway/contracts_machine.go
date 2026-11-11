package tuigateway

import (
	"github.com/nickheyer/icebath/pkg/internal/pydantic"
)

var MachineInfo = pydantic.NewModel("MachineInfo", ClosedConfig,
	pydantic.Def("os_family", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("os_release", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("native_arch", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("cpu_model", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("ram_gb", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("gpu_class", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("vendor", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("wsl", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("container", pydantic.Nullable(pydantic.Bool), nil),
)

var MachineFactsResult = pydantic.NewModel("MachineFactsResult", ClosedConfig,
	pydantic.Req("machine", MachineInfo),
	pydantic.Req("machine_kind", pydantic.Str),
	pydantic.Req("has_nvidia_gpu", pydantic.Bool),
	pydantic.Def("nvidia_platform", pydantic.Nullable(pydantic.Literal("rtx_spark", "dgx_spark", "discrete")), nil),
	pydantic.Def("locale", pydantic.Nullable(pydantic.Str), nil),
)

var _ = Method("machine.facts", Params, MachineFactsResult, "Facts about the machine running this backend and its OS account, for the desktop first run.")
