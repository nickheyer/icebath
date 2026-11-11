package dashboard

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Bridge whose reads replay a script and whose close is recorded
type scriptedBridge struct {
	mu     sync.Mutex
	reads  []func() ([]byte, error)
	closed int
}

func (b *scriptedBridge) Pid() int      { return 0 }
func (b *scriptedBridge) IsAlive() bool { return true }
func (b *scriptedBridge) Read(time.Duration) ([]byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.reads) == 0 {
		return nil, errors.New("StopIteration")
	}
	next := b.reads[0]
	b.reads = b.reads[1:]
	return next()
}
func (b *scriptedBridge) Write(context.Context, []byte, time.Duration, tools.PtyWriteFence) (bool, error) {
	return true, nil
}
func (b *scriptedBridge) Resize(int, int) {}
func (b *scriptedBridge) Close() error {
	b.mu.Lock()
	b.closed++
	b.mu.Unlock()
	return nil
}

func TestLegacyPumpClosesNormallyAndReapsTheChildAsOnMain(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	for _, failure := range []string{"", "send", "read"} {
		t.Run("failure="+failure, func(t *testing.T) {
			ctx := context.Background()
			incoming := make(chan Message, 8)
			incoming <- Message{Type: "websocket.connect"}
			var mu sync.Mutex
			var closes []int
			receive := func(ctx context.Context) (Message, error) {
				select {
				case m := <-incoming:
					return m, nil
				case <-ctx.Done():
					return Message{}, ctx.Err()
				}
			}
			send := func(ctx context.Context, m Message) error {
				if m.Type == "websocket.send" && failure == "send" {
					return &RuntimeError{Msg: "transport failed"}
				}
				if m.Type == "websocket.close" {
					mu.Lock()
					closes = append(closes, m.Code)
					mu.Unlock()
					incoming <- Message{Type: "websocket.disconnect", Code: m.Code}
				}
				return nil
			}
			ws := NewWebSocket(&Scope{Type: "websocket"}, receive, send)
			if err := ws.Accept(ctx, nil, nil); err != nil {
				t.Fatal(err)
			}
			bridge := &scriptedBridge{}
			if failure == "read" {
				bridge.reads = []func() ([]byte, error){func() ([]byte, error) { return nil, &RuntimeError{Msg: "read failed"} }}
			} else {
				bridge.reads = []func() ([]byte, error){
					func() ([]byte, error) { return []byte("output"), nil },
					func() ([]byte, error) { return nil, nil },
				}
			}
			waitCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			if err := legacyPump(waitCtx, ws, bridge); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(closes) != 1 || closes[0] != 1000 {
				t.Fatalf("closes %v", closes)
			}
			if bridge.closed == 0 {
				t.Fatal("bridge never closed")
			}
		})
	}
}
