package yuanbao

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pywebsockets"
)

// Sign token endpoint answering with the given status and a token for bot_999, counting its requests
func tokenServer(t *testing.T, status int) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(status)
		fmt.Fprint(w, `{"code": 0, "data": {"token": "fresh", "bot_id": "bot_999", "duration": 3600, "source": "bot"}}`)
	}))
	t.Cleanup(server.Close)
	return server, &requests
}

// Makes the reconnect waits instant and every longer sleep last until its task is cancelled
func instantReconnectWaits(t *testing.T) {
	t.Helper()
	swap(t, &sleep, func(ctx context.Context, wait float64) error {
		if wait > 2 {
			<-ctx.Done()
		}
		return ctx.Err()
	})
}

func TestCleanupWSIsBoundedAndSurvivesAFailingClose(t *testing.T) {
	adapter := makeAdapter(t, nil)
	hung := newFakeServer()
	hung.closeDelay = durationOf(wsCloseTimeoutS) + 500*time.Millisecond
	adapter.connection.ws = hung
	started := time.Now()
	adapter.connection.cleanupWS(context.Background())
	if elapsed := time.Since(started); elapsed > durationOf(wsCloseTimeoutS)+300*time.Millisecond || adapter.connection.socket() != nil {
		t.Fatalf("cleanup took %s", elapsed)
	}
	hung.mu.Lock()
	closeCalls := hung.closeCalls
	hung.mu.Unlock()
	if closeCalls != 1 {
		t.Fatalf("close ran %d times", closeCalls)
	}
	failing := newFakeServer()
	failing.closePanic = true
	adapter.connection.ws = failing
	adapter.connection.cleanupWS(context.Background())
	if adapter.connection.socket() != nil {
		t.Fatal("a failing close must still clear the socket")
	}
}

func TestOpenRefusesMissingCredentialsAndAMissingClient(t *testing.T) {
	adapter := makeAdapter(t, map[string]any{"app_secret": "  "})
	if connected, err := adapter.Connect(context.Background(), false); err != nil || connected || adapter.FatalErrorCode() == nil ||
		*adapter.FatalErrorCode() != "yuanbao_missing_credentials" || adapter.FatalErrorRetryable() {
		t.Fatalf("missing secret: %v %v %v", connected, err, adapter.FatalErrorCode())
	}
	adapter = makeAdapter(t, nil)
	swap(t, &websocketsAvailable, false)
	if connected, err := adapter.Connect(context.Background(), false); err != nil || connected || *adapter.FatalErrorCode() != "yuanbao_missing_dependency" ||
		!adapter.FatalErrorRetryable() || checkYuanbaoRequirements() {
		t.Fatalf("missing client: %v %v %v", connected, err, adapter.FatalErrorCode())
	}
}

func TestConnectBindsSendsAndDispatchesInboundPushes(t *testing.T) {
	adapter, server := connectAdapter(t, nil)
	ctx := context.Background()
	if binds := server.sent("auth-bind"); len(binds) != 1 || adapter.connection.boundConnectID() != "conn-1" || GetActive(ctx) != adapter ||
		!adapter.IsConnected() || !adapter.connection.isConnected() {
		t.Fatalf("handshake binds=%d connect id %q", len(binds), adapter.connection.boundConnectID())
	}
	if connected, err := adapter.Connect(ctx, false); err != nil || !connected || len(server.sent("auth-bind")) != 1 {
		t.Fatalf("a connected adapter must not dial again: %v %v", connected, err)
	}
	result, err := adapter.Send(ctx, "direct:alice", "hello", "", nil)
	if err != nil || !result.Success {
		t.Fatalf("send %+v %v", result, err)
	}
	sends := server.sent("send_c2c_message")
	if len(sends) != 1 || firstText(t, sends[0], 5) != "hello" {
		t.Fatalf("sent %d c2c frames", len(sends))
	}
	eventually(t, "the finish heartbeat", func() bool { return len(server.sent("send_private_heartbeat")) == 1 })
	t.Setenv("GATEWAY_ALLOW_ALL_USERS", "true")
	adapter.accessPolicy = makePolicy(t, "open", nil, "open", nil)
	adapter.connection.debounceWindow = 0.03
	events := captureEvents(t)
	server.push(t, "push-1", makeJSONPush(t, "alice", "", "part one", "msg-1"), true)
	server.push(t, "push-2", makeJSONPush(t, "alice", "", "part two", "msg-2"), false)
	eventually(t, "the merged inbound event", func() bool { return len(events()) == 1 })
	if event := events()[0]; event.Text != "part one \n part two" || *event.MessageID != "msg-1" {
		t.Fatalf("event text %q id %v", event.Text, *event.MessageID)
	}
	acks := 0
	for _, frame := range server.sent("inbound_message_push") {
		if head := frame.Vals["head"].(*pyjson.Object); pyjson.Equal(head.Vals["cmd_type"], CmdType["PushAck"]) && head.Vals["msg_id"] == "push-1" {
			acks++
		}
	}
	if acks != 1 {
		t.Fatalf("push acks %d", acks)
	}
	if err := adapter.Disconnect(ctx); err != nil || GetActive(ctx) != nil || adapter.IsConnected() || adapter.connection.socket() != nil || server.Open() {
		t.Fatalf("disconnect left state behind %v", err)
	}
}

func TestBizRequestsFailWhenUnansweredOrDisconnected(t *testing.T) {
	loose := makeAdapter(t, nil)
	if _, err := loose.connection.sendBizRequest(context.Background(), []byte{1}, "req-0", 0.05); err == nil || err.Error() != "Not connected" ||
		pyrepr.ExceptionName(err) != "RuntimeError" {
		t.Fatalf("unconnected request %v", err)
	}
	adapter, server := connectAdapter(t, nil)
	server.mu.Lock()
	server.silent["query_group_info"] = true
	server.mu.Unlock()
	encoded, err := EncodeQueryGroupInfo("grp-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.connection.sendBizRequest(context.Background(), encoded, "req-1", 0.05); !isTimeout(err) || adapter.connection.hasPending("req-1") {
		t.Fatalf("unanswered request %v", err)
	}
	failed := make(chan error, 1)
	go func() {
		_, err := adapter.connection.sendBizRequest(context.Background(), encoded, "req-2", 30)
		failed <- err
	}()
	eventually(t, "the pending request", func() bool { return adapter.connection.hasPending("req-2") })
	if err := adapter.Disconnect(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-failed:
		if err == nil || err.Error() != "YuanbaoAdapter disconnected" {
			t.Fatalf("pending request settled with %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("disconnect left the pending request waiting")
	}
}

func TestGroupQueriesDecodeAndCacheMembers(t *testing.T) {
	adapter, server := connectAdapter(t, nil)
	ctx := context.Background()
	info, member, members := &pbuf{}, &pbuf{}, &pbuf{}
	group := &pbuf{}
	group.s(1, "Team")
	group.s(2, "owner-1")
	info.m(3, group.b)
	member.s(1, "u1")
	member.s(2, "Alice")
	members.m(3, member.b)
	server.mu.Lock()
	server.replies["query_group_info"], server.replies["get_group_member_list"] = info.b, members.b
	server.mu.Unlock()
	got, err := adapter.QueryGroupInfo(ctx, "grp-1")
	if err != nil || got == nil || got.GetOr("group_name", nil) != "Team" || got.GetOr("owner_id", nil) != "owner-1" {
		t.Fatalf("group info %v %v", got, err)
	}
	list, err := adapter.GetGroupMemberList(ctx, "grp-1", 0, defaultMemberListLimit)
	if err != nil || list == nil || len(adapter.cachedMembers("grp-1")) != 1 {
		t.Fatalf("member list %v %v", list, err)
	}
	result, err := adapter.Send(ctx, "group:grp-1", "hi @alice there", "quoted-1", nil)
	if err != nil || !result.Success {
		t.Fatalf("group send %+v %v", result, err)
	}
	elems := bodyElems(t, server.sent("send_group_message")[0], 6)
	if len(elems) != 3 || elems[1].Vals["msg_type"] != "TIMCustomElem" ||
		elems[1].Vals["msg_content"].(*pyjson.Object).GetOr("data", nil) != `{"elem_type": 1002, "text": "@Alice", "user_id": "u1"}` {
		t.Fatalf("mention body %v", elems)
	}
	server.mu.Lock()
	delete(server.replies, "query_group_info")
	server.mu.Unlock()
	if bare, err := adapter.QueryGroupInfo(ctx, "grp-2"); err != nil || bare == nil || bare.GetOr("group_code", nil) != "grp-2" {
		t.Fatalf("bare response %v %v", bare, err)
	}
}

func TestPermanentCloseCodeStopsTheAdapterWithoutAReconnect(t *testing.T) {
	adapter, server := connectAdapter(t, nil)
	server.failures <- &pywebsockets.ConnectionClosed{Rcvd: &pywebsockets.Close{Code: 4012, Reason: "kicked"}}
	eventually(t, "the adapter to stop", func() bool { return !adapter.IsConnected() })
	time.Sleep(20 * time.Millisecond)
	adapter.connection.mu.Lock()
	reconnecting := adapter.connection.reconnecting
	adapter.connection.mu.Unlock()
	if reconnecting || len(server.sent("auth-bind")) != 1 {
		t.Fatal("a permanent close code must not reconnect")
	}
}

func TestReconnectRedialsWithAFreshTokenAndRegistersTheAdapter(t *testing.T) {
	tokens, requests := tokenServer(t, http.StatusOK)
	instantReconnectWaits(t)
	swap(t, &maxReconnectAttempts, 1)
	first, replacement := newFakeServer(), newFakeServer()
	dials := make(chan *fakeServer, 2)
	dials <- first
	dials <- replacement
	swap(t, &wsDial, func(context.Context, string, pywebsockets.ConnectOptions) (wsConn, error) { return <-dials, nil })
	adapter := makeAdapter(t, map[string]any{"api_domain": tokens.URL})
	seedToken(t, adapter.appKey)
	ctx := context.Background()
	if connected, err := adapter.Connect(ctx, false); err != nil || !connected {
		t.Fatalf("connect %v %v", connected, err)
	}
	t.Cleanup(func() { _ = adapter.Disconnect(ctx) })
	SetActive(ctx, nil)
	first.failures <- &pywebsockets.ConnectionClosed{Rcvd: &pywebsockets.Close{Code: 1011, Reason: "restart"}}
	eventually(t, "the reconnect", func() bool { return len(replacement.sent("auth-bind")) == 1 && GetActive(ctx) == adapter })
	if requests.Load() != 1 || adapter.botIDValue() != "bot_999" || !adapter.IsConnected() || adapter.connection.socket() != wsConn(replacement) || first.Open() {
		t.Fatalf("reconnect state token requests %d bot %v", requests.Load(), adapter.botIDValue())
	}
}

func TestReconnectGivesUpAndMarksTheAdapterDisconnected(t *testing.T) {
	tokens, requests := tokenServer(t, http.StatusInternalServerError)
	adapter := makeAdapter(t, map[string]any{"api_domain": tokens.URL})
	ctx := context.Background()
	instantReconnectWaits(t)
	swap(t, &maxReconnectAttempts, 2)
	adapter.SetRunning(true)
	SetActive(ctx, nil)
	if reconnected, err := adapter.connection.reconnectWithBackoff(ctx); err != nil || reconnected || requests.Load() != 2 || adapter.IsConnected() ||
		GetActive(ctx) != nil {
		t.Fatalf("reconnect %v %v after %d token requests", reconnected, err, requests.Load())
	}
}

func TestSenderKeyGroupsFramesBySenderAndGroup(t *testing.T) {
	isolatedHome(t)
	if key := extractSenderKey(makeJSONPush(t, "alice", "grp-1", "hi", "m1")); key != "alice:grp-1" {
		t.Fatalf("json key %q", key)
	}
	push := &pbuf{}
	push.s(2, "bob")
	push.s(6, "grp-2")
	if key := extractSenderKey(push.b); key != "bob:grp-2" {
		t.Fatalf("protobuf key %q", key)
	}
	first, second := extractSenderKey([]byte("{}")), extractSenderKey([]byte("{}"))
	if !strings.HasPrefix(first, "__unknown_") || first == second {
		t.Fatalf("frames without a sender must not share a key %q %q", first, second)
	}
}
