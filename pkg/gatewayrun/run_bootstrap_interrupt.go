package gatewayrun

import (
	"context"
	"os"
	"os/signal"
	"sync/atomic"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Set while the gateway loop routes interrupt and terminate itself, an interrupt then draining instead of cancelling
var gatewaySignalsRouted atomic.Bool

// Error of a start an interrupt ended before the gateway routed its own signals, the KeyboardInterrupt of asyncio.run
var ErrKeyboardInterrupt error = &gateway.PyError{Kind: "KeyboardInterrupt"}

// Runs StartGateway as asyncio.run does, a first unrouted interrupt cancelling the start and a second ending the wait
func RunGateway(ctx context.Context, o StartGatewayOptions) (bool, error) {
	interrupts := make(chan os.Signal, 2)
	if !signal.Ignored(os.Interrupt) {
		signal.Notify(interrupts, os.Interrupt)
		defer signal.Stop(interrupts)
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	type outcome struct {
		success bool
		err     error
	}
	done := make(chan outcome, 1)
	go func() {
		success, err := StartGateway(runCtx, o)
		done <- outcome{success, err}
	}()
	interrupted := 0
	for {
		select {
		case result := <-done:
			if interrupted > 0 {
				return false, ErrKeyboardInterrupt
			}
			return result.success, result.err
		case <-interrupts:
			if gatewaySignalsRouted.Load() {
				continue
			}
			interrupted++
			if interrupted > 1 {
				return false, ErrKeyboardInterrupt
			}
			cancel()
		}
	}
}

// Registers a function the process exit path runs, the atexit registration of the gateway command
func RegisterAtExit(fn func()) { registerBootstrapAtExit(fn) }
