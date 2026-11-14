package yuanbao

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/hermetic"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pywebsockets"
	"github.com/nickheyer/icebath/pkg/provider"
)

func TestMain(m *testing.M) {
	hermetic.BlockExternalHTTP()
	hermetic.BlockExternalDNS()
	os.Exit(m.Run())
}

// Swaps a package seam for one test
func swap[T any](t *testing.T, p *T, v T) {
	t.Helper()
	old := *p
	*p = v
	t.Cleanup(func() { *p = old })
}

// Private home and lock dir with every Yuanbao and allow all env var cleared
func isolatedHome(t *testing.T) string {
	t.Helper()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv("ICEBATH_GATEWAY_LOCK_DIR", t.TempDir())
	keys := []string{"YUANBAO_HOME_CHANNEL", "YUANBAO_ALLOW_ALL_USERS", "GATEWAY_ALLOW_ALL_USERS"}
	for _, kv := range os.Environ() {
		if key, _, _ := strings.Cut(kv, "="); strings.HasPrefix(key, "YUANBAO_") {
			keys = append(keys, key)
		}
	}
	for _, key := range keys {
		t.Setenv(key, "")
		os.Unsetenv(key)
	}
	t.Cleanup(func() { gateway.FlushRuntimeStatus(5 * time.Second) })
	return h
}

// Adapter over the test credentials with the bot id bot_123, extra entries laid over the defaults
func makeAdapter(t *testing.T, extra map[string]any) *YuanbaoAdapter {
	t.Helper()
	isolatedHome(t)
	config := gateway.NewPlatformConfig()
	config.Extra = map[string]any{"app_id": "test_key", "app_secret": "test_secret", "ws_url": "wss://test.example.com/ws",
		"api_domain": "https://test.example.com"}
	for key, value := range extra {
		config.Extra[key] = value
	}
	adapter, err := NewYuanbaoAdapter(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	adapter.setBotID("bot_123")
	return adapter
}

// Context of a secondary profile secret scope while the process multiplexes profiles
func withMultiplexScope(t *testing.T, secrets map[string]string) context.Context {
	t.Helper()
	provider.SetMultiplexActive(true)
	t.Cleanup(func() { provider.SetMultiplexActive(false) })
	return provider.WithSecretScope(context.Background(), secrets, "")
}

// Policy over the given DM and group policies, failing the test on a construction error
func makePolicy(t *testing.T, dmPolicy string, dmAllowFrom []string, groupPolicy string, groupAllowFrom []string) *AccessPolicy {
	t.Helper()
	policy, err := NewAccessPolicy(dmPolicy, dmAllowFrom, groupPolicy, groupAllowFrom)
	if err != nil {
		t.Fatal(err)
	}
	return policy
}

// JSON callback push of one text message, a group push when a group code is given
func makeJSONPush(t *testing.T, fromAccount, groupCode, text, msgID string) []byte {
	t.Helper()
	push := pyjson.ObjectOf("CallbackCommand", "C2C.CallbackAfterSendMsg", "From_Account", fromAccount, "To_Account", "bot_123",
		"MsgBody", []any{pyjson.ObjectOf("MsgType", "TIMTextElem", "MsgContent", pyjson.ObjectOf("text", text))}, "MsgKey", msgID)
	if groupCode != "" {
		push.Set("CallbackCommand", "Group.CallbackAfterSendMsg")
		push.Set("GroupId", groupCode)
	}
	encoded, err := pyjson.Dumps(push)
	if err != nil {
		t.Fatal(err)
	}
	return []byte(encoded)
}

// Counts how often the pipeline continuation ran
type nextCounter struct{ calls int }

func (n *nextCounter) next() error {
	n.calls++
	return nil
}

// Runs one middleware over the context and returns how often it continued the chain
func runMiddleware(t *testing.T, mw InboundMiddleware, ic *InboundContext) int {
	t.Helper()
	counter := &nextCounter{}
	if err := mw.Handle(context.Background(), ic, counter.next); err != nil {
		t.Fatal(err)
	}
	return counter.calls
}

// Waits until cond holds, failing the test after two seconds
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Gateway socket double that answers auth binds, pings and business requests the way the Yuanbao server does
type fakeServer struct {
	mu         sync.Mutex
	inbox      chan any
	failures   chan error
	done       chan struct{}
	closeOnce  sync.Once
	frames     []*pyjson.Object
	replies    map[string][]byte
	silent     map[string]bool
	closeDelay time.Duration
	closePanic bool
	closeCalls int
}

func newFakeServer() *fakeServer {
	return &fakeServer{inbox: make(chan any, 64), failures: make(chan error, 1), done: make(chan struct{}), replies: map[string][]byte{},
		silent: map[string]bool{}}
}

func (s *fakeServer) Open() bool {
	select {
	case <-s.done:
		return false
	default:
		return true
	}
}

func (s *fakeServer) Recv(ctx context.Context) (any, error) {
	select {
	case frame := <-s.inbox:
		return frame, nil
	case err := <-s.failures:
		return nil, err
	case <-s.done:
		return nil, &pywebsockets.ConnectionClosed{}
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Records a client frame and queues the response a request gets, none for a cmd marked silent
func (s *fakeServer) SendBytes(frame []byte) error {
	select {
	case <-s.done:
		return &pywebsockets.ConnectionClosed{}
	default:
	}
	msg, err := DecodeConnMsg(frame)
	if err != nil {
		return err
	}
	head := msg.Vals["head"].(*pyjson.Object)
	cmd, msgID := head.Vals["cmd"].(string), head.Vals["msg_id"].(string)
	s.mu.Lock()
	s.frames = append(s.frames, msg)
	silent, reply := s.silent[cmd], s.replies[cmd]
	s.mu.Unlock()
	if silent || !pyjson.Equal(head.Vals["cmd_type"], CmdType["Request"]) {
		return nil
	}
	if cmd == Cmd["AuthBind"] {
		bound := &pbuf{}
		bound.s(3, "conn-1")
		reply = bound.b
	}
	response, err := EncodeConnMsgFull(CmdType["Response"], cmd, 0, msgID, head.Vals["module"].(string), reply, false)
	if err != nil {
		return err
	}
	s.inbox <- response
	return nil
}

func (s *fakeServer) Close() {
	s.mu.Lock()
	s.closeCalls++
	delay, panics := s.closeDelay, s.closePanic
	s.mu.Unlock()
	if panics {
		panic("connection already reset")
	}
	time.Sleep(delay)
	s.closeOnce.Do(func() { close(s.done) })
}

// Decoded client frames carrying the cmd, oldest first
func (s *fakeServer) sent(cmd string) []*pyjson.Object {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*pyjson.Object
	for _, frame := range s.frames {
		if frame.Vals["head"].(*pyjson.Object).Vals["cmd"] == cmd {
			out = append(out, frame)
		}
	}
	return out
}

// Delivers a server push carrying the payload under the message id
func (s *fakeServer) push(t *testing.T, msgID string, payload []byte, needAck bool) {
	t.Helper()
	frame, err := EncodeConnMsgFull(CmdType["Push"], "inbound_message_push", 0, msgID, bizPkg, payload, needAck)
	if err != nil {
		t.Fatal(err)
	}
	s.inbox <- frame
}

// Message body elements a sent message frame carries in the given request field
func bodyElems(t *testing.T, frame *pyjson.Object, field int64) []*pyjson.Object {
	t.Helper()
	fields, err := parseDict(frame.Vals["data"].([]byte))
	if err != nil {
		t.Fatal(err)
	}
	var elems []*pyjson.Object
	for _, raw := range fields.GetRepeatedBytes(field) {
		elem, err := DecodeMsgBodyElement(raw)
		if err != nil {
			t.Fatal(err)
		}
		elems = append(elems, elem)
	}
	return elems
}

// Text of the first element of a sent message frame
func firstText(t *testing.T, frame *pyjson.Object, field int64) string {
	t.Helper()
	elems := bodyElems(t, frame, field)
	if len(elems) == 0 {
		t.Fatal("frame carries no message body")
	}
	text, _ := elems[0].Vals["msg_content"].(*pyjson.Object).GetOr("text", "").(string)
	return text
}

// Seeds the sign token cache so a connect needs no token request
func seedToken(t *testing.T, appKey string) {
	t.Helper()
	signManager.Lock()
	signManager.cache[appKey] = map[string]any{"token": "tok", "bot_id": "bot_123", "duration": int64(3600), "product": "yb", "source": "bot",
		"expire_ts": wallClock() + 3600}
	signManager.Unlock()
	t.Cleanup(func() {
		signManager.Lock()
		delete(signManager.cache, appKey)
		signManager.Unlock()
	})
}

// Adapter connected to a fake server through the real open path, disconnected when the test ends
func connectAdapter(t *testing.T, extra map[string]any) (*YuanbaoAdapter, *fakeServer) {
	t.Helper()
	adapter := makeAdapter(t, extra)
	server := newFakeServer()
	seedToken(t, adapter.appKey)
	swap(t, &wsDial, func(context.Context, string, pywebsockets.ConnectOptions) (wsConn, error) { return server, nil })
	connected, err := adapter.Connect(context.Background(), false)
	if err != nil || !connected {
		t.Fatalf("connect %v %v", connected, err)
	}
	t.Cleanup(func() { _ = adapter.Disconnect(context.Background()) })
	return adapter, server
}

// Events the gateway dispatch received, the dispatch seam replaced for the test
func captureEvents(t *testing.T) func() []*gateway.MessageEvent {
	t.Helper()
	var mu sync.Mutex
	var events []*gateway.MessageEvent
	swap(t, &handleMessageFn, func(_ context.Context, _ *YuanbaoAdapter, event *gateway.MessageEvent) error {
		mu.Lock()
		events = append(events, event)
		mu.Unlock()
		return nil
	})
	return func() []*gateway.MessageEvent {
		mu.Lock()
		defer mu.Unlock()
		return append([]*gateway.MessageEvent{}, events...)
	}
}
