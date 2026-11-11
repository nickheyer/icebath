//go:build !unix

package gatewayrun

import "github.com/nickheyer/icebath/pkg/gateway"

// Routes nothing where the loop cannot take signals, so an interrupt stays with the caller that runs the start
func startGatewayInstallSignalHandlers(loop gateway.EventLoop, shutdown func(sig *int), restart func()) (stop func()) {
	return func() {}
}
