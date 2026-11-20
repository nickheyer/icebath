package gatewayrun

// Whether a name is a builtin or registered plugin gateway command
var IsGatewayKnownCommand func(name string) bool

// Whether a name is one of the builtin gateway commands
var GatewayKnownCommand func(name string) bool
