package tuiserver

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tuihost"
)

// Stdio transport over a real OS pipe whose reader decodes every frame
type fanoutPipeClient struct {
	transport *tuihost.StdioTransport
	writer    *os.File
	closed    atomic.Bool
	frames    chan map[string]any
}

func newFanoutPipeClient(t *testing.T) *fanoutPipeClient {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	c := &fanoutPipeClient{writer: writer, frames: make(chan map[string]any, 1024)}
	c.transport = tuihost.NewStdioTransport(func() io.Writer { return c.writer }, &sync.Mutex{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		scanner := bufio.NewScanner(reader)
		scanner.Buffer(make([]byte, 1<<20), 1<<24)
		for scanner.Scan() {
			var frame map[string]any
			if json.Unmarshal(scanner.Bytes(), &frame) == nil {
				c.frames <- frame
			}
		}
	}()
	t.Cleanup(func() {
		writer.Close()
		<-done
		reader.Close()
	})
	return c
}

func (c *fanoutPipeClient) Write(obj *pyjson.Object) bool { return c.transport.Write(obj) }
func (c *fanoutPipeClient) Close()                        { c.closed.Store(true) }
func (c *fanoutPipeClient) Closed() bool                  { return c.closed.Load() }

func (c *fanoutPipeClient) receive(t *testing.T) map[string]any {
	t.Helper()
	select {
	case frame := <-c.frames:
		return frame
	case <-time.After(5 * time.Second):
		t.Fatal("no frame arrived within five seconds")
		return nil
	}
}

// Whether no frame is waiting once in flight writes had a moment to land
func (c *fanoutPipeClient) empty() bool {
	time.Sleep(50 * time.Millisecond)
	return len(c.frames) == 0
}

func TestMembershipPreservesTerminalDeliveryAndRevokesDepartedPeers(t *testing.T) {
	for _, attachment := range []string{"direct", "flattened"} {
		t.Run(attachment, func(t *testing.T) {
			t.Setenv("ICEBATH_HOME", t.TempDir())
			ctx := context.Background()
			a, b, stranger := newFanoutPipeClient(t), newFanoutPipeClient(t), newFanoutPipeClient(t)
			session := tuihost.NewSession(map[string]any{"transport": a, "agent": nil, "session_key": "fanout-invariant", "history": []any{},
				"history_lock": &sync.Mutex{}, "history_version": int64(0), "running": false, "attached_images": []any{}})
			swapSessions(t, map[string]*tuihost.Session{"fanout-invariant": session})
			var newcomer tuihost.Transport = b
			if attachment == "flattened" {
				newcomer = tuihost.NewFanoutTransport(a, b)
			}
			if !tuihost.AttachSessionTransport(session, newcomer) || !tuihost.AttachSessionTransport(session, b) {
				t.Fatal("attach refused")
			}
			for _, kind := range []string{"message.delta", "reasoning.delta", "message.complete"} {
				tuihost.Emit(ctx, kind, "fanout-invariant", map[string]any{"text": "α"})
				first, second := a.receive(t), b.receive(t)
				params, _ := first["params"].(map[string]any)
				if !pyjson.Equal(first, second) || params["type"] != kind {
					t.Fatalf("%s first %v second %v", kind, first, second)
				}
			}
			for _, c := range []struct {
				client  *fanoutPipeClient
				allowed bool
			}{{a, true}, {b, true}, {stranger, false}} {
				authority, _ := currentSessionSteerAuthority(tuihost.BindTransport(ctx, c.client), "fanout-invariant")
				if (authority == tuihost.Transport(c.client)) != c.allowed {
					t.Fatalf("steer authority %v for %p", authority, c.client)
				}
			}
			if !tuihost.WriteJSON(tuihost.BindTransport(ctx, a), pyjson.ObjectOf("jsonrpc", "2.0", "id", "private", "result", "owner only")) {
				t.Fatal("reply write failed")
			}
			if reply := a.receive(t); reply["id"] != "private" || !b.empty() {
				t.Fatalf("reply %v leaked to the subscriber set", reply)
			}
			session.Set("viewers", map[tuihost.Transport]float64{b: 0, stranger: 0})
			if !tuihost.DetachSessionTransport(session, b) {
				t.Fatal("detach refused")
			}
			if viewers, _ := session.Get("viewers").(map[tuihost.Transport]float64); len(viewers) != 0 || tuihost.SessionTransportContains(session, b) {
				t.Fatalf("viewers %v contains %v", viewers, tuihost.SessionTransportContains(session, b))
			}
			tuihost.Emit(ctx, "message.complete", "fanout-invariant", map[string]any{"text": "only A"})
			payload, _ := a.receive(t)["params"].(map[string]any)["payload"].(map[string]any)
			if payload["text"] != "only A" || !b.empty() {
				t.Fatalf("payload %v", payload)
			}
			if !tuihost.AttachSessionTransport(session, b) {
				t.Fatal("reattach refused")
			}
			tuihost.Emit(ctx, "message.complete", "fanout-invariant", map[string]any{"text": "reattached"})
			if first, second := a.receive(t), b.receive(t); !pyjson.Equal(first, second) {
				t.Fatalf("first %v second %v", first, second)
			}
			b.Close()
			if tuihost.AttachSessionTransport(session, b) || tuihost.SessionTransportContains(session, b) {
				t.Fatal("a dead peer regained authority")
			}
			if tuihost.AttachSessionTransport(session, tuihost.ProcessStdioTransport()) {
				t.Fatal("stdio joined a websocket session")
			}
			if reaped, detached := closeSessionsForTransport(ctx, b, "ws_disconnect"); reaped != 0 || detached != 0 {
				t.Fatalf("b counts %d %d", reaped, detached)
			}
			if reaped, detached := closeSessionsForTransport(ctx, a, "ws_disconnect"); reaped != 0 || detached != 1 {
				t.Fatalf("a counts %d %d", reaped, detached)
			}
			if session.Get("transport") != tuihost.DetachedWSTransport() {
				t.Fatalf("transport %v", session.Get("transport"))
			}
		})
	}
}
