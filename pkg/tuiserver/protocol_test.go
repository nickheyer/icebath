package tuiserver

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestUnknownMethod(t *testing.T) {
	resp := handleRequest(context.Background(), map[string]any{"id": "1", "method": "bogus"})
	if rpcCode(resp) != -32601 {
		t.Fatalf("response %v", resp)
	}
}

func TestDispatchRunsShortHandlersInline(t *testing.T) {
	swapMethod(t, "fast.ping", func(_ context.Context, rid any, _ map[string]any) map[string]any {
		return ok(rid, map[string]any{"pong": true})
	})
	resp := Dispatch(context.Background(), map[string]any{"id": "r1", "method": "fast.ping", "params": map[string]any{}}, nil)
	want := map[string]any{"jsonrpc": "2.0", "id": "r1", "result": map[string]any{"pong": true}}
	if !reflect.DeepEqual(resp, want) {
		t.Fatalf("response %v", resp)
	}
}

func TestSlowHandlersRunOffTheReaderThread(t *testing.T) {
	for _, slowMethod := range []string{"browser.manage", "plugins.manage", "voice.toggle", "voice.record", "wake.start", "wake.status"} {
		t.Run(slowMethod, func(t *testing.T) {
			release := make(chan struct{})
			ran := make(chan struct{}, 1)
			swapMethod(t, slowMethod, func(_ context.Context, rid any, _ map[string]any) map[string]any {
				ran <- struct{}{}
				select {
				case <-release:
				case <-time.After(10 * time.Second):
				}
				return ok(rid, map[string]any{})
			})
			transport := &recordingTransport{}
			started := time.Now()
			resp := Dispatch(context.Background(), map[string]any{"id": "slow", "method": slowMethod, "params": map[string]any{}}, transport)
			elapsed := time.Since(started)
			close(release)
			if resp != nil {
				t.Fatalf("%s ran inline on the reader thread: %v", slowMethod, resp)
			}
			if elapsed >= 5*time.Second {
				t.Fatalf("dispatch took %s", elapsed)
			}
			deadline := time.Now().Add(10 * time.Second)
			var frames []map[string]any
			for time.Now().Before(deadline) {
				transport.mu.Lock()
				frames = append([]map[string]any(nil), transport.frames...)
				transport.mu.Unlock()
				if len(frames) > 0 {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if len(frames) == 0 || frames[0]["id"] != "slow" {
				t.Fatalf("frames %v", frames)
			}
			select {
			case <-ran:
			default:
				t.Fatal("handler never ran")
			}
		})
	}
}

// Line buffer standing in for the real stdout of the capture fixture
type stdoutCapture struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *stdoutCapture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}

func (c *stdoutCapture) text() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

// Every JSON frame written so far, one per line
func (c *stdoutCapture) frames(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(c.text()), "\n") {
		if line == "" {
			continue
		}
		var frame map[string]any
		if err := json.Unmarshal([]byte(line), &frame); err != nil {
			t.Fatalf("frame %q: %v", line, err)
		}
		out = append(out, frame)
	}
	return out
}

// Stop doubles of the three agent shapes the explicit stop compatibility case drives
type legacyStopAgent struct{ calls *[]string }

func (a legacyStopAgent) Interrupt(string) { *a.calls = append(*a.calls, "legacy") }

type hardOnlyStopAgent struct{ calls *[]string }

func (a hardOnlyStopAgent) HardInterrupt(string) { *a.calls = append(*a.calls, "hard") }
