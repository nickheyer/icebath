package yuanbao

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/errchain"
)

// The websocket client is compiled into this build
var websocketsAvailable = true

// AttributeError of a field that is not text, naming the field
func notText(v any, field string) error {
	return &pyErr{"AttributeError", fmt.Sprintf("%s must be text, got %s", field, errchain.Kind(v))}
}

// TypeError of using a list or dict where a single value is needed, nil for every other value
func unhashable(v any) error {
	switch v.(type) {
	case []any:
		return &pyErr{"TypeError", "expected a single value, got a list"}
	case *jsonutil.Object, map[string]any:
		return &pyErr{"TypeError", "expected a single value, got a mapping"}
	}
	return nil
}

// Dict behind a value, the AttributeError naming the kind of any other value
func dictOf(v any) (map[string]any, error) {
	d, ok := asDict(v)
	if !ok {
		return nil, &pyErr{"AttributeError", fmt.Sprintf("expected a mapping, got %s", errchain.Kind(v))}
	}
	return d, nil
}

// Text of a value, the AttributeError naming the field for any other value
func strMethod(v any, field string) (string, error) {
	if s, ok := v.(string); ok {
		return s, nil
	}
	return "", notText(v, field)
}

// Items a for loop over a value visits, the TypeError of a value that is not iterable
func iterOf(v any) ([]any, error) {
	switch t := v.(type) {
	case []any:
		return t, nil
	case string:
		out := make([]any, 0, len(t))
		for _, r := range t {
			out = append(out, string(r))
		}
		return out, nil
	case *jsonutil.Object, map[string]any:
		keys := dictKeys(t)
		out := make([]any, len(keys))
		for i, k := range keys {
			out[i] = k
		}
		return out, nil
	}
	return nil, &pyErr{"TypeError", fmt.Sprintf("expected a list, got %s", errchain.Kind(v))}
}

// Parts joined by a separator, the TypeError str join raises for a part that is not text
func joinParts(sep string, parts []any) (string, error) {
	texts := make([]string, len(parts))
	for i, p := range parts {
		s, ok := p.(string)
		if !ok {
			return "", &pyErr{"TypeError", fmt.Sprintf("part %d must be text, got %s", i, errchain.Kind(p))}
		}
		texts[i] = s
	}
	return strings.Join(texts, sep), nil
}

// Decoded JSON of text or bytes, the TypeError json loads raises for any other value
func jsonLoads(v any) (any, error) {
	switch t := v.(type) {
	case string:
		return jsonutil.Decode(t)
	case []byte:
		return jsonutil.Decode(t)
	}
	return nil, &pyErr{"TypeError", fmt.Sprintf("expected JSON text, got %s", errchain.Kind(v))}
}

// Text of a protobuf string field, empty for a falsy value and the AttributeError naming the field for any other value
func protoText(v any, field string) (string, error) {
	if s, ok := v.(string); ok {
		return s, nil
	}
	if !anyutil.IsTrue(v) {
		return "", nil
	}
	return "", notText(v, field)
}

// Text of a protobuf string field that is always written, the AttributeError naming the field for any other value
func encodeText(v any, field string) (string, error) {
	if s, ok := v.(string); ok {
		return s, nil
	}
	return "", notText(v, field)
}

// Whether the path names an existing regular file
func isFile(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Mode().IsRegular()
}

// Duration of a count of seconds
func durationOf(n float64) time.Duration { return time.Duration(n * float64(time.Second)) }

// Runs fn and returns a panic it raised as an error
func recovered(fn func()) (err error) {
	defer func() {
		if r := recover(); r != nil {
			if e, ok := r.(error); ok {
				err = e
				return
			}
			err = fmt.Errorf("%v", r)
		}
	}()
	fn()
	return nil
}

// Pointer to a copy of the text
func strPtr(s string) *string { return &s }

// Result slot one goroutine fills and others await, the asyncio Future of a pending ack
type future struct {
	once   sync.Once
	done   chan struct{}
	result any
	err    error
}

func newFuture() *future { return &future{done: make(chan struct{})} }

// Whether a result, an exception or a cancellation already settled the slot
func (f *future) isDone() bool {
	select {
	case <-f.done:
		return true
	default:
		return false
	}
}

// Settles the slot with a result
func (f *future) setResult(v any) {
	f.once.Do(func() {
		f.result = v
		close(f.done)
	})
}

// Settles the slot with an exception
func (f *future) setException(err error) {
	f.once.Do(func() {
		f.err = err
		close(f.done)
	})
}

// Settles the slot as cancelled, what an elapsed wait does to the future it awaited
func (f *future) cancel() {
	f.once.Do(func() {
		f.err = &pyErr{"CancelledError", ""}
		close(f.done)
	})
}

// Result of the slot within the timeout, a TimeoutError once it elapses and the context error on cancellation
func (f *future) wait(ctx context.Context, timeout float64) (any, error) {
	timer := time.NewTimer(durationOf(timeout))
	defer timer.Stop()
	select {
	case <-f.done:
		return f.result, f.err
	case <-timer.C:
		return nil, &pyErr{"TimeoutError", ""}
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Mutex whose holder state is readable, the asyncio Lock of one chat
type chatLock struct{ held chan struct{} }

func newChatLock() *chatLock { return &chatLock{held: make(chan struct{}, 1)} }

// Takes the lock, the context error when the task is cancelled first
func (l *chatLock) acquire(ctx context.Context) error {
	select {
	case l.held <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (l *chatLock) release() { <-l.held }

// Whether a holder currently has the lock
func (l *chatLock) locked() bool { return len(l.held) == 1 }
