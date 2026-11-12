package tuigateway

import "github.com/nickheyer/icebath/pkg/internal/pydantic"

// The model_config combinations the contract classes declare
var (
	ParamsConfig     = pydantic.Config{Extra: pydantic.ExtraForbid, PopulateByName: true}
	ClosedConfig     = pydantic.Config{Extra: pydantic.ExtraForbid}
	OpenConfig       = pydantic.Config{Extra: pydantic.ExtraAllow}
	OpenParamsConfig = pydantic.Config{Extra: pydantic.ExtraAllow, PopulateByName: true}
	AttributesConfig = pydantic.Config{Extra: pydantic.ExtraForbid, FromAttributes: true}
)

// The base classes every params, result and payload model derives from
var (
	Params  = pydantic.NewModel("Params", ParamsConfig).Described("Client→server method params / server→client request params. Unknown keys are rejected.")
	Result  = pydantic.NewModel("Result", ClosedConfig)
	Payload = pydantic.NewModel("Payload", ClosedConfig)
)
