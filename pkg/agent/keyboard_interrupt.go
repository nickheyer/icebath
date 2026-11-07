package agent

import (
	"context"
	"os"
	"os/signal"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Turn context a SIGINT cancels with the keyboard interrupt cause, stop releasing the signal and the context
func KeyboardInterruptContext(parent context.Context) (context.Context, func()) {
	ctx, cancel := context.WithCancelCause(parent)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	done := make(chan struct{})
	go func() {
		select {
		case <-sig:
			cancel(tools.ErrKeyboardInterrupt)
		case <-done:
		case <-ctx.Done():
		}
	}()
	return ctx, func() {
		signal.Stop(sig)
		close(done)
		cancel(context.Canceled)
	}
}
