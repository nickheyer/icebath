package tool

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	"github.com/nickheyer/icebath/internal/config"
)

const (
	ReasonUserInterrupt   = "user interrupt"
	ReasonNewMessage      = "user sent a new message"
	ReasonHardStop        = "explicit stop requested"
	ReasonGatewayShutdown = "gateway shutdown"
	ReasonHostCancelled   = "host cancelled the command"
)

type Interrupted struct {
	Reason string
	User   bool
}

func (e *Interrupted) Error() string {
	if e.Reason == "" {
		return "interrupted"
	}
	return e.Reason
}

func NewInterrupted(reason string) *Interrupted {
	switch reason {
	case ReasonUserInterrupt, ReasonNewMessage, ReasonHardStop:
		return &Interrupted{Reason: reason, User: true}
	}
	return &Interrupted{Reason: reason}
}

func InterruptReason(ctx context.Context) (string, bool) {
	var in *Interrupted
	if !errors.As(context.Cause(ctx), &in) || in.Reason == "" {
		return "", false
	}
	return in.Reason, true
}

type Yield struct {
	mu      sync.Mutex
	ch      chan struct{}
	pending bool
}

func NewYield() *Yield {
	return &Yield{ch: make(chan struct{})}
}

func (y *Yield) Request() {
	if y == nil {
		return
	}
	y.mu.Lock()
	defer y.mu.Unlock()
	if y.pending {
		return
	}
	y.pending = true
	close(y.ch)
}

func (y *Yield) Requested() <-chan struct{} {
	if y == nil {
		return nil
	}
	y.mu.Lock()
	defer y.mu.Unlock()
	return y.ch
}

func (y *Yield) Pending() bool {
	if y == nil {
		return false
	}
	y.mu.Lock()
	defer y.mu.Unlock()
	return y.pending
}

func (y *Yield) Consume() bool {
	if y == nil {
		return false
	}
	y.mu.Lock()
	defer y.mu.Unlock()
	if !y.pending {
		return false
	}
	y.pending = false
	y.ch = make(chan struct{})
	return true
}

func (y *Yield) Clear() {
	y.Consume()
}

type Interrupt struct {
	cancel context.CancelCauseFunc
	yield  *Yield
	debug  bool
}

func NewInterrupt(parent context.Context) (context.Context, *Interrupt) {
	ctx, cancel := context.WithCancelCause(parent)
	return ctx, &Interrupt{cancel: cancel, yield: NewYield(), debug: config.EnvEnabled("ICEBATH_DEBUG_INTERRUPT", "")}
}

func (i *Interrupt) Stop(reason string) {
	if i.debug {
		slog.Info("[interrupt-debug] interrupt requested", "reason", reason)
	}
	i.cancel(NewInterrupted(reason))
}

func (i *Interrupt) Yield() *Yield {
	return i.yield
}
