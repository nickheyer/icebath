package gateway

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// One scripted owner answering each request with the frame its reply builds, hooks replacing send and receive
type scriptedOwner struct {
	reply   func(request map[string]any) any
	inbox   chan string
	closing bool
	mu      sync.Mutex
	sent    []string
	send    func(raw string) error
	recv    func() (string, error)
	shut    chan struct{}
	once    sync.Once
}

func newScriptedOwner(reply func(request map[string]any) any) *scriptedOwner {
	return &scriptedOwner{reply: reply, inbox: make(chan string, 16), shut: make(chan struct{})}
}

func (s *scriptedOwner) Send(raw string) error {
	if s.send != nil {
		return s.send(raw)
	}
	decoded, err := pyjson.Loads(raw)
	if err != nil {
		return err
	}
	if frame := s.reply(pyjson.Plain(decoded).(map[string]any)); frame != nil {
		text, err := pyjson.Dumps(frame)
		if err != nil {
			return err
		}
		s.inbox <- text
	}
	return nil
}

func (s *scriptedOwner) Recv() (string, error) {
	if s.recv != nil {
		return s.recv()
	}
	select {
	case raw := <-s.inbox:
		return raw, nil
	case <-s.shut:
		return "", errors.New("connection closed")
	}
}

func (s *scriptedOwner) Open() bool          { return !s.closing }
func (s *scriptedOwner) Subprotocol() string { return "" }
func (s *scriptedOwner) Close() error {
	s.once.Do(func() { close(s.shut) })
	return nil
}

// Whether the reader of the client has ended
func scriptedReaderDone(client *GatewayClient) bool {
	select {
	case <-client.readDone:
		return true
	default:
		return false
	}
}

func TestRPCErrorKeepsNumericCodeAndBoundedData(t *testing.T) {
	errorsByMethod := map[string]any{
		"session.mutate": map[string]any{"code": 4001, "message": "invalid_params", "data": map[string]any{
			"reason": "invalid_params", "fields": []any{"payload.title"},
			"trace": map[string]any{"nested": "remote diagnostics"}, "blob": strings.Repeat("x", 5000)}},
		"nope": map[string]any{"code": -32601, "message": "unknown method: nope", "data": map[string]any{"reason": "unknown_method"}},
	}
	owner := newScriptedOwner(func(request map[string]any) any {
		return map[string]any{"jsonrpc": "2.0", "id": request["id"], "error": errorsByMethod[request["method"].(string)]}
	})
	client := NewGatewayClient(owner).Start()
	defer client.Close()
	var refused *GatewayRPCError
	_, err := client.RPC(context.Background(), "session.mutate", map[string]any{"session_id": "s"})
	if !errors.As(err, &refused) || refused.Error() != "invalid_params" || refused.Code == nil || *refused.Code != 4001 {
		t.Fatalf("refusal %#v", err)
	}
	if !pyjson.Equal(pyjson.Plain(refused.Data), map[string]any{"reason": "invalid_params", "fields": []any{"payload.title"}}) {
		t.Fatalf("data %v", refused.Data)
	}
	_, err = client.RPC(context.Background(), "nope", nil)
	if !errors.As(err, &refused) || refused.Error() != "unknown_method" || refused.Code == nil || *refused.Code != -32601 {
		t.Fatalf("unknown method refusal %#v", err)
	}
}

func TestSendOnADeadSocketIsAClientErrorNotARawWebsocketsOne(t *testing.T) {
	owner := newScriptedOwner(func(map[string]any) any { return nil })
	owner.send = func(string) error { return errors.New("received 1006 (connection closed abnormally)") }
	client := NewGatewayClient(owner).Start()
	defer client.Close()
	_, err := client.RPC(context.Background(), "subagent.list", map[string]any{"session_id": "s"})
	var failure *GatewayClientError
	if !errors.As(err, &failure) || !strings.Contains(failure.Error(), "turn outcome is unknown") {
		t.Fatalf("dead socket send answered %#v", err)
	}
	if n := client.PendingCount(); n != 0 {
		t.Fatalf("pending %d", n)
	}
}

func TestARequestOnAClosingSocketIsNotSent(t *testing.T) {
	owner := newScriptedOwner(func(map[string]any) any { return nil })
	owner.closing = true
	owner.send = func(raw string) error {
		owner.mu.Lock()
		owner.sent = append(owner.sent, raw)
		owner.mu.Unlock()
		return errors.New("connection closed")
	}
	client := NewGatewayClient(owner).Start()
	defer client.Close()
	if scriptedReaderDone(client) {
		t.Fatal("the reader ended on a closing socket")
	}
	_, err := client.RPC(context.Background(), "prompt.submit", map[string]any{"session_id": "s"})
	var failure *GatewayClientError
	if !errors.As(err, &failure) || failure.Error() != GatewayNotSent {
		t.Fatalf("closing socket answered %#v", err)
	}
	if len(owner.sent) != 0 || client.PendingCount() != 0 {
		t.Fatalf("sent %v pending %d", owner.sent, client.PendingCount())
	}
}

func TestARequestAfterTheReaderEndedIsNotSent(t *testing.T) {
	owner := newScriptedOwner(func(map[string]any) any { return nil })
	owner.send = func(raw string) error {
		owner.mu.Lock()
		owner.sent = append(owner.sent, raw)
		owner.mu.Unlock()
		return errors.New("connection closed")
	}
	owner.recv = func() (string, error) { return "", errors.New("iteration ended") }
	client := NewGatewayClient(owner).Start()
	defer client.Close()
	select {
	case <-client.readDone:
	case <-time.After(time.Second):
	}
	if !scriptedReaderDone(client) {
		t.Fatal("the reader did not see the close")
	}
	_, err := client.RPC(context.Background(), "prompt.submit", map[string]any{"session_id": "s"})
	var failure *GatewayClientError
	if !errors.As(err, &failure) || failure.Error() != GatewayNotSent {
		t.Fatalf("ended reader answered %#v", err)
	}
	if len(owner.sent) != 0 || client.PendingCount() != 0 {
		t.Fatalf("sent %v pending %d", owner.sent, client.PendingCount())
	}
}

func TestASendThatFailsAfterTheReaderFailedItsRequestLogsNothing(t *testing.T) {
	owner := newScriptedOwner(func(map[string]any) any { return nil })
	lost := make(chan struct{})
	var client *GatewayClient
	owner.send = func(string) error {
		close(lost)
		select {
		case <-client.readDone:
		case <-time.After(time.Second):
		}
		return errors.New("connection closed")
	}
	owner.recv = func() (string, error) {
		<-lost
		return "", errors.New("connection closed")
	}
	client = NewGatewayClient(owner).Start()
	_, err := client.RPC(context.Background(), "subagent.list", map[string]any{"session_id": "s"})
	var failure *GatewayClientError
	if !errors.As(err, &failure) {
		t.Fatalf("send after the lost reader answered %#v", err)
	}
	if !scriptedReaderDone(client) {
		t.Fatal("the reader had not failed the request before the send raised")
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	if n := client.PendingCount(); n != 0 {
		t.Fatalf("pending %d", n)
	}
}
