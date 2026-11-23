package gatewayrun

const (
	telegramConnectTimeoutSecsDefault   = 180.0
	adapterDisconnectTimeoutSecsDefault = 5.0
)

// Cold start Telegram connect budget before the gateway reaches running
var telegramInitialConnectTimeoutSecsDefault = 45.0
