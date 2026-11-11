package tuiserver

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
)

func TestLegacyFallbackKeepsTheTicketProfileForSessionlessScopedHandlers(t *testing.T) {
	secondary := filepath.Join(t.TempDir(), "profiles", "l106742sec")
	if err := os.MkdirAll(secondary, 0o755); err != nil {
		t.Fatal(err)
	}
	wsTestMethod(t, "probe.scoped", profileScoped(func(ctx context.Context, rid any, _ map[string]any) map[string]any {
		return map[string]any{"jsonrpc": "2.0", "id": rid, "result": map[string]any{"home": home.Dir(ctx)}}
	}))
	reply := wsTestDriveAuthorityFallback(t, wsTestInteractive, secondary, "probe.scoped")
	result, _ := reply["result"].(map[string]any)
	if result["home"] != secondary {
		t.Fatalf("the scoped handler ran in %v, want %s (reply %v)", result["home"], secondary, reply)
	}
}

// Runner of the authority owned session case, a gateway that is running and not draining
type wsTestAuthorityRunner struct{}

func (wsTestAuthorityRunner) SessionRuntimeDescriptor() map[string]any { return map[string]any{} }
func (wsTestAuthorityRunner) Draining() bool                           { return false }
func (wsTestAuthorityRunner) Running() bool                            { return true }
func (wsTestAuthorityRunner) SessionStore() *gateway.SessionStore      { return nil }
func (wsTestAuthorityRunner) MultiplexProfiles() bool                  { return false }
func (wsTestAuthorityRunner) AdapterForSource(context.Context, *gateway.SessionSource) (gateway.PlatformAdapter, error) {
	return nil, nil
}

func TestLegacyFallbackNeverWritesAnAuthorityOwnedSession(t *testing.T) {
	for _, row := range []struct {
		method string
		params *pyjson.Object
	}{
		{"session.delete", pyjson.ObjectOf("session_id", "owned")},
		{"session.archive", pyjson.ObjectOf("session_id", "owned", "archived", true)},
	} {
		t.Run(row.method, func(t *testing.T) {
			wsTestNoOrphanGrace(t)
			tmp := t.TempDir()
			t.Setenv("ICEBATH_HOME", tmp)
			ctx := context.Background()
			db, err := state.NewSessionDB(ctx, filepath.Join(tmp, "state.db"), false)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			if err := db.CreateSession(state.NewSession{ID: "owned", Source: "tui"}); err != nil {
				t.Fatal(err)
			}
			before, err := db.GetSession(ctx, "owned")
			if err != nil || before == nil {
				t.Fatalf("seeded row %v, %v", before, err)
			}
			revision := before.GetOr("runtime_revision", nil)
			epoch, err := db.BeginRuntimeEpoch(ctx, "current")
			if err != nil {
				t.Fatal(err)
			}
			authority := gateway.NewSessionAuthority(wsTestAuthorityRunner{}, tmp, "current", db, epoch)
			authority.Sessions["owned"] = gateway.NewLiveSession(nil, "route")
			sock := newWSTestSocket()
			sock.authority = authority
			sock.inbound <- pyjson.ObjectOf("jsonrpc", "2.0", "id", 1, "method", row.method, "params", row.params)
			sock.inbound <- nil
			if err := HandleWS(ctx, sock, map[string]any{"user_id": "owner", "instance_id": "current"}, nil, false); err != nil {
				t.Fatal(err)
			}
			var reply map[string]any
			for _, frame := range sock.frames() {
				if pyrepr.Equal(frame["id"], 1) {
					reply = frame
				}
			}
			failure, _ := reply["error"].(map[string]any)
			if !pyrepr.Equal(failure["code"], -32601) {
				t.Fatalf("reply %v", reply)
			}
			after, err := db.GetSession(ctx, "owned")
			if err != nil || after == nil {
				t.Fatalf("the authority owned row is gone: %v, %v", after, err)
			}
			if pyrepr.Truthy(after.GetOr("archived", nil)) || !pyrepr.Equal(after.GetOr("runtime_revision", nil), revision) {
				t.Fatalf("the row changed behind the authority: %v", pyrepr.Repr(after))
			}
			if _, live := authority.Sessions["owned"]; !live {
				t.Fatal("the authority lost its live session")
			}
		})
	}
}
