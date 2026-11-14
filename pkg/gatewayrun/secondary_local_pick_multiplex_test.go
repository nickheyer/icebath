package gatewayrun

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/profiles"
)

// Root home and one named profile home with the loopback custom route as every profile's gateway config
func reserveMultiplexHomes(t *testing.T, config string) []profiles.ServedProfile {
	t.Helper()
	tmp := t.TempDir()
	root := filepath.Join(tmp, ".icebath")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", root)
	homes := []profiles.ServedProfile{{Name: "default", Home: root}}
	beta := filepath.Join(root, "profiles", "beta")
	if err := os.MkdirAll(beta, 0o700); err != nil {
		t.Fatal(err)
	}
	homes = append(homes, profiles.ServedProfile{Name: "beta", Home: beta})
	for _, h := range homes {
		if err := os.WriteFile(filepath.Join(h.Home, "config.yaml"), []byte(config), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return homes
}

// test_secondary_local_pick_multiplex.py test_secondary_pick_reaches_next_turn_after_primary_local_session
func TestSecondaryPickReachesNextTurnAfterPrimaryLocalSession(t *testing.T) {
	installContractPrefillLoaders(t)
	homes := reserveMultiplexHomes(t, `{"platform_toolsets": {"cli": [], "tui": []}, `+
		`"model": {"provider": "custom", "base_url": "http://127.0.0.1:9/v1", "api_key": "loopback-only"}}`)
	ctx := context.Background()
	config := gateway.DefaultGatewayConfig(ctx)
	multiplex := true
	config.MultiplexProfiles = &multiplex
	config.SetRuntimeProfileHomes(homes)
	runner, err := NewGatewayRunner(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	reserved := []string{homes[0].Home, homes[1].Home}
	if err := gateway.ProcessOwnership.Reserve(ctx, reserved, ""); err != nil {
		t.Fatal(err)
	}
	var connections []*gateway.AuthorityConnection
	t.Cleanup(func() {
		for _, connection := range connections {
			_ = connection.Close(ctx)
		}
		for _, authority := range gateway.AllAuthorities(runner) {
			authority.DB.Close()
		}
		for _, h := range reserved {
			gateway.ProcessOwnership.Release(h)
		}
	})
	if err := runner.initializeGatewayRuntime(ctx); err != nil {
		t.Fatal(err)
	}
	registry := runner.sessionAuthorities
	primary, secondary := registry.ForHome(homes[0].Home), registry.ForHome(homes[1].Home)
	connect := func(authority *gateway.SessionAuthority) *gateway.AuthorityConnection {
		connection, err := gateway.NewAuthorityConnection(authority, picksTransport{}, map[string]any{"user_id": "human"}, false)
		if err != nil {
			t.Fatal(err)
		}
		connections = append(connections, connection)
		return connection
	}

	if _, err := gateway.CreateLocalSession(gateway.OwnerScope(ctx, primary, false), primary, connect(primary).Actor(), map[string]any{
		"request_id": "primary", "source": "cli", "cwd": t.TempDir(), "model": "primary-model", "toolsets": []any{}}); err != nil {
		t.Fatal(err)
	}
	if adapter, _ := runner.adapters.Get(gateway.PlatformLocal); func() bool { _, ok := adapter.(*gateway.LocalSessionAdapter); return !ok }() {
		t.Fatalf("primary local adapter %T", adapter)
	}

	owner := connect(secondary)
	ref, err := gateway.CreateLocalSession(gateway.OwnerScope(ctx, secondary, false), secondary, owner.Actor(), map[string]any{
		"request_id": "desktop", "source": "tui", "cwd": t.TempDir(), "model": "default-a", "toolsets": []any{}})
	if err != nil {
		t.Fatal(err)
	}
	row, err := secondary.DB.GetSession(ctx, ref.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	revision, _ := row.Get("runtime_revision")
	generation, _ := row.Get("runtime_generation")
	response, err := owner.Dispatch(ctx, pyjson.ObjectOf("id", int64(1), "method", "session.mutate", "params", pyjson.ObjectOf(
		"session_id", ref.SessionID, "request_id", "pick", "operation", "model", "payload", pyjson.ObjectOf("model", "picked-b"),
		"expected_revision", revision, "expected_generation", generation)))
	if err != nil {
		t.Fatal(err)
	}
	if model, _ := picksField(response, "result").Get("model"); model != "picked-b" {
		t.Fatalf("pick %s", picksDump(response))
	}

	live, ok := secondary.Live(ref.SessionID)
	if !ok {
		t.Fatalf("secondary session %s not live", ref.SessionID)
	}
	source := live.Source
	runner.canonicalize(ctx, source, "", "")
	if policy, err := gateway.SessionPolicyForSource(ctx, runner, source); err != nil || policy == nil {
		t.Fatalf("the pick was read through the primary adapter %v %v", policy, err)
	}
	model, _, err := runner.ResolveSessionAgentRuntime(gateway.OwnerScope(ctx, secondary, false), source, "", nil, true)
	if err != nil || model != "picked-b" {
		t.Fatalf("next turn model %q %v", model, err)
	}
}
