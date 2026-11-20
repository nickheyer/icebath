package selfupdate

import "github.com/nickheyer/icebath/pkg/gateway"

// Seconds the detached restart watcher waits for the old gateway pid to exit before giving up
const gatewayRestartWatcherTimeoutS = gateway.GatewayRestartWatcherTimeoutS
