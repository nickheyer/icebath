package tuigateway

import (
	"github.com/nickheyer/icebath/pkg/internal/pydantic"
)

var PingParams = pydantic.NewModel("PingParams", ParamsConfig)

var PingResult = pydantic.NewModel("PingResult", ClosedConfig,
	pydantic.Req("pong", pydantic.Bool),
)

var _ = Method("ping", PingParams, PingResult, "Cheapest liveness probe; answered on the WS reader thread even while every agent is mid-turn.")

var GatewayCapabilitiesResult = pydantic.NewModel("GatewayCapabilitiesResult", ClosedConfig,
	pydantic.Req("per_session_exclusive_submit", pydantic.Bool),
)

var _ = Method("gateway.capabilities", PingParams, GatewayCapabilitiesResult, "What THIS build enforces (a client withholds a feature unless advertised).")

var ClientCapabilitiesParams = pydantic.NewModel("ClientCapabilitiesParams", ParamsConfig,
	pydantic.Def("server_requests", pydantic.Bool, false),
	pydantic.Def("desktop_local", pydantic.Bool, false),
)

var ClientCapabilitiesResult = pydantic.NewModel("ClientCapabilitiesResult", ClosedConfig,
	pydantic.Req("server_requests", pydantic.List(pydantic.Str)),
	pydantic.Def("declines_not_shown", pydantic.Bool, false),
	pydantic.Def("canonical_clarify_batch", pydantic.Bool, false),
)

var _ = Method("client.capabilities", ClientCapabilitiesParams, ClientCapabilitiesResult, "What the calling client handles, sent once per connection (after gateway.ready); returns the server→client request methods this backend may send.")
