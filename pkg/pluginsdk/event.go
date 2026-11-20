package pluginsdk

import (
	"context"
	"time"
)

// Flag living in icebath that a plugin reads, waits on and sets, the form a stop or cancel signal reaches a plugin in
type Event struct {
	peer *peer
	id   int64
}

// One operation on the flag in icebath
func (e *Event) op(ctx context.Context, name string, timeout any) (any, error) {
	return e.peer.call(ctx, "parent_event", map[string]any{"id": e.id, "op": name, "timeout": timeout})
}

// Whether the flag is set
func (e *Event) IsSet(ctx context.Context) (bool, error) {
	value, err := e.op(ctx, "is_set", nil)
	set, _ := value.(bool)
	return set, err
}

// Waits until the flag is set or the timeout passes and answers whether it is set, a negative timeout waiting without limit
func (e *Event) Wait(ctx context.Context, timeout time.Duration) (bool, error) {
	var seconds any
	if timeout >= 0 {
		seconds = timeout.Seconds()
	}
	value, err := e.op(ctx, "wait", seconds)
	set, _ := value.(bool)
	return set, err
}

// Sets the flag, waking everything in icebath that waits on it
func (e *Event) Set(ctx context.Context) error {
	_, err := e.op(ctx, "set", nil)
	return err
}

// Clears the flag so later waits block again
func (e *Event) Clear(ctx context.Context) error {
	_, err := e.op(ctx, "clear", nil)
	return err
}
