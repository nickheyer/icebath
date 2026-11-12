//go:build unix

package gatewayrun

import (
	"os"
	"os/signal"
	"syscall"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Signal that asks the gateway for a drain aware service restart
var restartSignal os.Signal = syscall.SIGUSR1

// Routes interrupt and terminate to the drain and the user signal to the restart on the loop until stop is called
func startGatewayInstallSignalHandlers(loop gateway.EventLoop, shutdown func(sig *int), restart func()) (stop func()) {
	ch := make(chan os.Signal, 4)
	gateway.SupersedeHostSignalCleanup()
	gatewaySignalsRouted.Store(true)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM, restartSignal)
	quit := make(chan struct{})
	go func() {
		for {
			select {
			case <-quit:
				return
			case received := <-ch:
				if received == restartSignal {
					loop.CallSoonThreadsafe(restart)
					continue
				}
				num := int(received.(syscall.Signal))
				loop.CallSoonThreadsafe(func() { shutdown(&num) })
			}
		}
	}()
	return func() {
		signal.Stop(ch)
		close(quit)
		gatewaySignalsRouted.Store(false)
	}
}
