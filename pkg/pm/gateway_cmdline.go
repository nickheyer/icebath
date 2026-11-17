package pm

// Matches a gateway command line, installed by the gateway status port, nil failing closed
var LooksLikeGatewayCommandLine func(cmdline string) bool
