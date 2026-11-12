package gateway

import (
	"context"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

func TestDiscoveryAuthorizesBeforeScanning(t *testing.T) {
	savedCatalog, savedCompletions := CommandCatalog, SlashCompletions
	t.Cleanup(func() { CommandCatalog, SlashCompletions = savedCatalog, savedCompletions })
	CommandCatalog = func(context.Context, func(context.Context) (context.Context, error)) (map[string]any, error) {
		t.Fatal("unauthorized request reached discovery")
		return nil, nil
	}
	SlashCompletions = func(context.Context, string, func(context.Context) (context.Context, error)) (map[string]any, error) {
		t.Fatal("unauthorized request reached discovery")
		return nil, nil
	}
	authority := &SessionAuthority{InstanceID: "owned", ProfileID: t.TempDir(), Events: map[string]tuigateway.Transport{}}
	ctx := context.Background()
	for _, identity := range []map[string]any{{}, {"user_id": "reader", "capabilities": []any{}},
		{"user_id": "reader", "profile_id": "foreign"}, {"user_id": "reader", "instance_id": "stale"}} {
		connection, err := NewAuthorityConnection(authority, struct{ tuigateway.Transport }{}, identity, false)
		if err != nil {
			t.Fatal(err)
		}
		for _, method := range []string{"commands.catalog", "complete.slash"} {
			params := pyjson.NewObject()
			if method == "complete.slash" {
				params.Set("text", "/probe")
			}
			reply, err := connection.Dispatch(ctx, pyjson.ObjectOf("id", int64(1), "method", method, "params", params))
			if err != nil {
				t.Fatalf("%v %s: %v", identity, method, err)
			}
			refusal, _ := reply.GetOr("error", nil).(*pyjson.Object)
			if refusal == nil || refusal.GetOr("message", nil) != "permission_denied" {
				text, _ := pyjson.Dumps(reply)
				t.Fatalf("%v %s = %s", identity, method, text)
			}
		}
		if err := connection.Close(ctx); err != nil {
			t.Fatal(err)
		}
	}
}
