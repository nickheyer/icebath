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
).Described("Measured hardware; a key that was not measured is absent.")

var MachineFactsResult = pydantic.NewModel("MachineFactsResult", ClosedConfig,
	pydantic.Req("machine", MachineInfo),
	pydantic.Req("machine_kind", pydantic.Str),
	pydantic.Req("has_nvidia_gpu", pydantic.Bool),
	pydantic.Def("nvidia_platform", pydantic.Nullable(pydantic.Literal("rtx_spark", "dgx_spark", "discrete")), nil),
	pydantic.Def("locale", pydantic.Nullable(pydantic.Str), nil),
).Described("``machine_kind`` is ``Spark``, ``Mac``, ``PC`` or ``computer``. ``nvidia_platform`` is\n``products.nvidia_platform()``: ``rtx_spark``, ``dgx_spark``, ``discrete`` or null. ``locale`` is the OS\naccount's first UI language, null when the OS has none.")

var _ = Method("machine.facts", Params, MachineFactsResult, "Facts about the machine running this backend and its OS account, for the desktop first run.")
