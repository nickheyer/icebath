package gateway

import (
	"context"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

// Refuses at the snapshot check, before any FIFO write, until the returned switch is cleared
func refuseAutomationSnapshot(t *testing.T, reason string) *atomic.Bool {
	t.Helper()
	refusing := &atomic.Bool{}
	refusing.Store(true)
	real := snapshotAutomation
	snapshotAutomation = func(ctx context.Context, a *SessionAuthority, adapter PlatformAdapter, event *MessageEvent, identity string) (*pyjson.Object, *SessionEntry, error) {
		if refusing.Load() {
			return nil, nil, &state.RuntimeStoreError{Reason: reason}
		}
		return real(ctx, a, adapter, event, identity)
	}
	t.Cleanup(func() { snapshotAutomation = real })
	return refusing
}

func TestARefusedDeliveryAnswersThroughTheDeliverContract(t *testing.T) {
	f := newBotRetryFixture(t)
	connection, err := NewAuthorityConnection(f.a, wireContractTransport{}, map[string]any{"user_id": "uid:1000", "provider": "local",
		"profile_id": f.a.ProfileID, "instance_id": f.a.InstanceID,
		"capabilities": []any{"session:read", "session:submit", "session:control", "session:create"}}, false)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close(f.ctx)
	realSibling := connectionSiblingFor
	connectionSiblingFor = func(c *AuthorityConnection, ctx context.Context, profile string) (*AuthorityConnection, error) {
		if c == connection {
			return nil, nil
		}
		return realSibling(c, ctx, profile)
	}
	t.Cleanup(func() { connectionSiblingFor = realSibling })
	key := strings.Repeat("e", 32)
	request := func() *pyjson.Object {
		return pyjson.ObjectOf("id", int64(1), "method", "bot_relay.deliver", "params", pyjson.ObjectOf("id", key, "profile", "default", "message", "ping"))
	}
	refusing := refuseAutomationSnapshot(t, "permission_denied")
	reply, err := connection.Dispatch(f.ctx, request())
	if err != nil {
		t.Fatal(err)
	}
	first := wireReply(t, reply)
	failure, _ := first["error"].(map[string]any)
	data, _ := failure["data"].(map[string]any)
	if data["reason"] != "permission_denied" {
		t.Fatalf("first answered %v", first)
	}
	refusing.Store(false)
	reply, err = connection.Dispatch(f.ctx, request())
	if err != nil {
		t.Fatal(err)
	}
	again := wireReply(t, reply)
	result, ok := again["result"].(map[string]any)
	if !ok {
		t.Fatalf("again answered %v", again)
	}
	if got := []any{result["status"], result["refused"], result["reason"]}; !reflect.DeepEqual(got, []any{"failed", true, "agent_blocked"}) {
		t.Fatalf("result %v", result)
	}
	if _, admitted := result["admission_id"]; result["delivery_id"] != key || admitted {
		t.Fatalf("result %v", result)
	}
	if rows := f.admissions(t); len(rows) != 0 {
		t.Fatalf("admissions %v", rows)
	}
}
