package vsc

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"testing"
	"time"
)

// Joins two peers over an in-memory pipe and closes both at the end
func pipePeers(t *testing.T) (*Peer, *Peer) {
	t.Helper()
	c1, c2 := net.Pipe()
	a, b := NewPeer(c1), NewPeer(c2)
	t.Cleanup(func() {
		a.Close()
		b.Close()
	})
	return a, b
}

func TestPeerRequestResponse(t *testing.T) {
	a, b := pipePeers(t)
	b.Handle("add", func(ctx context.Context, params json.RawMessage) (any, error) {
		var in struct{ X, Y int }
		if err := json.Unmarshal(params, &in); err != nil {
			return nil, &RPCError{Code: codeBadParams, Message: err.Error()}
		}
		return map[string]int{"sum": in.X + in.Y}, nil
	})
	var out struct{ Sum int }
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := a.Call(ctx, "add", map[string]int{"X": 2, "Y": 3}, &out); err != nil {
		t.Fatal(err)
	}
	if out.Sum != 5 {
		t.Fatalf("sum %d", out.Sum)
	}
}

func TestPeerReverseDirection(t *testing.T) {
	a, b := pipePeers(t)
	a.Handle("ping", func(ctx context.Context, params json.RawMessage) (any, error) {
		return "pong", nil
	})
	var out string
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := b.Call(ctx, "ping", nil, &out); err != nil {
		t.Fatal(err)
	}
	if out != "pong" {
		t.Fatalf("got %q", out)
	}
}

func TestPeerNotification(t *testing.T) {
	a, b := pipePeers(t)
	got := make(chan string, 1)
	b.HandleNotify("hello", func(params json.RawMessage) {
		var s string
		json.Unmarshal(params, &s)
		got <- s
	})
	if err := a.Notify("hello", "world"); err != nil {
		t.Fatal(err)
	}
	select {
	case s := <-got:
		if s != "world" {
			t.Fatalf("got %q", s)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no notification")
	}
}

func TestPeerHandlerError(t *testing.T) {
	a, b := pipePeers(t)
	b.Handle("boom", func(ctx context.Context, params json.RawMessage) (any, error) {
		return nil, &RPCError{Code: 123, Message: "kaboom"}
	})
	b.Handle("plain", func(ctx context.Context, params json.RawMessage) (any, error) {
		return nil, errors.New("plain failure")
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var out any
	err := a.Call(ctx, "boom", nil, &out)
	var re *RPCError
	if !errors.As(err, &re) || re.Code != 123 || re.Message != "kaboom" {
		t.Fatalf("got %v", err)
	}
	err = a.Call(ctx, "plain", nil, &out)
	if !errors.As(err, &re) || re.Code != codeInternal {
		t.Fatalf("plain error %v", err)
	}
	err = a.Call(ctx, "missing", nil, &out)
	if !errors.As(err, &re) || re.Code != codeNoMethod {
		t.Fatalf("missing method %v", err)
	}
}

func TestPeerCallCancelled(t *testing.T) {
	a, b := pipePeers(t)
	release := make(chan struct{})
	b.Handle("slow", func(ctx context.Context, params json.RawMessage) (any, error) {
		<-release
		return nil, nil
	})
	defer close(release)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	var out any
	err := a.Call(ctx, "slow", nil, &out)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}

func TestPeerCloseEndsDone(t *testing.T) {
	a, b := pipePeers(t)
	a.Close()
	select {
	case <-a.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("done not closed on a")
	}
	select {
	case <-b.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("done not closed on b after peer close")
	}
}

func TestPeerTrace(t *testing.T) {
	a, b := pipePeers(t)
	lines := make(chan string, 8)
	a.SetTrace(func(dir string, line []byte) { lines <- dir + " " + string(line) })
	b.Handle("echo", func(ctx context.Context, params json.RawMessage) (any, error) { return "ok", nil })
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var out string
	if err := a.Call(ctx, "echo", nil, &out); err != nil {
		t.Fatal(err)
	}
	select {
	case <-lines:
	case <-time.After(2 * time.Second):
		t.Fatal("no trace line")
	}
}

func TestHandlersBeforeStart(t *testing.T) {
	c1, c2 := net.Pipe()
	client := NewPeer(c1)
	defer client.Close()
	server := newPeer(c2)
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	type reply struct {
		result string
		err    error
	}
	got := make(chan reply, 1)
	go func() {
		var out string
		err := client.Call(ctx, "early", nil, &out)
		got <- reply{out, err}
	}()
	time.Sleep(50 * time.Millisecond)
	server.Handle("early", func(context.Context, json.RawMessage) (any, error) { return "served", nil })
	server.Start()
	select {
	case r := <-got:
		if r.err != nil || r.result != "served" {
			t.Fatalf("request sent before start got %q %v", r.result, r.err)
		}
	case <-ctx.Done():
		t.Fatal("no reply to a request sent before the server started")
	}
}
