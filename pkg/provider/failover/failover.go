// Tries models in order, moving to the next when one fails before producing anything
package failover

import (
	"context"
	"errors"
	"fmt"
	"iter"

	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Anything that completes and streams turns, such as a provider
type Model interface {
	Complete(ctx context.Context, req protocol.Request) (protocol.Response, error)
	Stream(ctx context.Context, req protocol.Request) iter.Seq2[protocol.Event, error]
}

// Ordered models with optional observers of each failure that moved on and of the model that answered
type Chain struct {
	Models []Model
	// Called with the index of the model that failed and why before the next one is tried
	OnFail func(i int, err error)
	// Called with the index of the model that produced the response or the first streamed event
	OnServe func(i int)
}

func (c Chain) fail(i int, err error) {
	if c.OnFail != nil {
		c.OnFail(i, err)
	}
}

func (c Chain) serve(i int) {
	if c.OnServe != nil {
		c.OnServe(i)
	}
}

// Completes with the first model that succeeds, giving up on a failure that is not worth retrying
func (c Chain) Complete(ctx context.Context, req protocol.Request) (protocol.Response, error) {
	if len(c.Models) == 0 {
		return protocol.Response{}, errors.New("failover: no models")
	}
	var errs []error
	for i, m := range c.Models {
		resp, err := m.Complete(ctx, req)
		if err == nil {
			c.serve(i)
			return resp, nil
		}
		errs = append(errs, fmt.Errorf("model %d: %w", i, err))
		if !provider.Retryable(err) {
			break
		}
		c.fail(i, err)
	}
	return protocol.Response{}, errors.Join(errs...)
}

// Streams from the first model that yields an event, failing over only before anything arrived
func (c Chain) Stream(ctx context.Context, req protocol.Request) iter.Seq2[protocol.Event, error] {
	return func(yield func(protocol.Event, error) bool) {
		if len(c.Models) == 0 {
			yield(protocol.Event{}, errors.New("failover: no models"))
			return
		}
		var errs []error
		for i, m := range c.Models {
			started := false
			var failed error
			for ev, err := range m.Stream(ctx, req) {
				if err != nil && !started {
					failed = err
					break
				}
				if !started {
					c.serve(i)
				}
				started = true
				if !yield(ev, err) || err != nil {
					return
				}
			}
			if started {
				return
			}
			if failed == nil {
				failed = errors.New("stream ended without events")
			}
			errs = append(errs, fmt.Errorf("model %d: %w", i, failed))
			if !provider.Retryable(failed) {
				break
			}
			c.fail(i, failed)
		}
		yield(protocol.Event{}, errors.Join(errs...))
	}
}
