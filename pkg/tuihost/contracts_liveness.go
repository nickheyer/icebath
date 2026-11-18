package tuihost

import (
	"github.com/nickheyer/icebath/pkg/internal/pydantic"
)

var PingParams = pydantic.NewModel("PingParams", ParamsConfig)

var PingResult = pydantic.NewModel("PingResult", ClosedConfig,
	pydantic.Req("pong", pydantic.Bool),
)

var _ = Method("ping", PingParams, PingResult, "Cheapest liveness probe; answered on the WS reader thread even while every agent is mid-turn.")

var ClientCapabilitiesParams = pydantic.NewModel("ClientCapabilitiesParams", ParamsConfig,
	pydantic.Def("server_requests", pydantic.Bool, false),
)

var ClientCapabilitiesResult = pydantic.NewModel("ClientCapabilitiesResult", ClosedConfig,
	pydantic.Req("server_requests", pydantic.List(pydantic.Str)),
	pydantic.Def("declines_not_shown", pydantic.Bool, false),
	pydantic.Def("canonical_clarify_batch", pydantic.Bool, false),
)

var _ = Method("client.capabilities", ClientCapabilitiesParams, ClientCapabilitiesResult, "What the calling client handles, sent once per connection (after gateway.ready); returns the server→client request methods this backend may send.")
