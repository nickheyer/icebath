package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pydict"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

const secretScopeConfig = "model: {default: m1, provider: custom, base_url: \"http://127.0.0.1:9/v1\", api_key: route-token}\n" +
	"agent: {reasoning_effort: high}\nplatform_toolsets: {cli: [terminal]}\nterminal: {backend: local}\n" +
	"mcp_servers: {docs: {command: docs-mcp, env: {DOCS_TOKEN: mcp-token}}}\n"

// Policy fields an older gateway's reference did not hash
var secretScopeLaterFields = []string{"kanban_json", "safe_mode", "ignore_user_config", "skills_prompt", "editor_mcp_json",
	"coding_posture", "client_env_json", "client_env_remote", "client_env_rules_json", "edit_approval_policy"}

// Runner of a resumed owner, no cached agent and a session runtime on m1
type secretScopeRunner struct {
	*recoverTestRunner
}

func (secretScopeRunner) CachedAgentFor(string) any                          { return nil }
func (secretScopeRunner) SetSessionReasoningOverride(string, map[string]any) {}
func (secretScopeRunner) ResolveSessionAgentRuntime(context.Context, *SessionSource, string, map[string]any, bool) (string, map[string]any, error) {
	return "m1", map[string]any{}, nil
}

// Home holding the fixture config.yaml, the active profile home of the case
func secretScopeHome(t *testing.T) string {
	t.Helper()
	home := filepath.Join(t.TempDir(), "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", home)
	quietHandleHealth(t)
	installPrefillLoaders(t)
	envRefsWrite(t, home, "config.yaml", secretScopeConfig)
	return home
}

// Owner authority over the home's own sessions store and state database
func secretScopeOwner(t *testing.T, ctx context.Context, home, instance string) (*state.SessionDB, *SessionAuthority) {
	t.Helper()
	db, err := state.NewSessionDB(ctx, filepath.Join(home, "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store := NewSessionStore(ctx, filepath.Join(home, "sessions"), DefaultGatewayConfig(ctx), nil)
	store.PinDB(db)
	base := &recoverTestRunner{store: store, adapters: pydict.New[Platform, PlatformAdapter](), profiles: pydict.New[string, *pydict.Dict[Platform, PlatformAdapter]]()}
	a, err := InitializeSessionAuthority(ctx, secretScopeRunner{base}, home, instance, db, false)
	if err != nil {
		t.Fatal(err)
	}
	base.launch = a
	return db, a
}

// Result of a verb dispatched on the connection, failing the case on an error reply
func secretScopeCall(t *testing.T, ctx context.Context, connection *AuthorityConnection, method string, params map[string]any) map[string]any {
	t.Helper()
	reply := secretScopeDispatch(t, ctx, connection, method, params)
	result, ok := reply["result"].(map[string]any)
	if !ok {
		t.Fatalf("%s reply %v", method, reply)
	}
	return result
}

func secretScopeDispatch(t *testing.T, ctx context.Context, connection *AuthorityConnection, method string, params map[string]any) map[string]any {
	t.Helper()
	request, _ := pyjson.Ordered(params).(*pyjson.Object)
	reply, err := connection.Dispatch(ctx, pyjson.ObjectOf("id", int64(1), "method", method, "params", request))
	if err != nil {
		t.Fatal(err)
	}
	return wireReply(t, reply)
}

func secretScopeConnect(t *testing.T, a *SessionAuthority) *AuthorityConnection {
	t.Helper()
	connection, err := NewAuthorityConnection(a, wireContractTransport{}, map[string]any{"user_id": "human"}, false)
	if err != nil {
		t.Fatal(err)
	}
	return connection
}

func secretScopeStored(t *testing.T, ctx context.Context, db *state.SessionDB, sid string) *LocalSessionPolicy {
	t.Helper()
	raw, _ := recoverReceipt(t, ctx, db, sid).Get("policy")
	policy, err := restorePolicy(raw)
	if err != nil {
		t.Fatal(err)
	}
	return policy
}

// Reference the first release of frozen config secrets wrote, a whole policy hash and no bound endpoints
func olderGatewayReference(t *testing.T, policy *LocalSessionPolicy) string {
	t.Helper()
	parsed, err := pyjson.Loads(strings.TrimPrefix(*policy.ConfigSecretRef, configSecretPrefix))
	if err != nil {
		t.Fatal(err)
	}
	source := pyjson.Plain(parsed).(map[string]any)
	data := policy.asDict()
	for _, name := range append([]string{"credential_ref", "config_secret_ref"}, secretScopeLaterFields...) {
		delete(data, name)
	}
	text, err := pyjson.Dumps(data, pyjson.SortKeys(true))
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(text))
	source["policy"] = hex.EncodeToString(digest[:])
	delete(source, "bound")
	ref, err := pyjson.Dumps(source, pyjson.SortKeys(true))
	if err != nil {
		t.Fatal(err)
	}
	return configSecretPrefix + ref
}

// Session an older gateway stamped, adopted by a restarted current owner
func olderSession(t *testing.T, ctx context.Context, cwd string) (string, *state.SessionDB, *SessionAuthority, string) {
	t.Helper()
	home := secretScopeHome(t)
	firstDB, first := secretScopeOwner(t, ctx, home, "first")
	owner := secretScopeConnect(t, first)
	sid, _ := secretScopeCall(t, ctx, owner, "session.create", map[string]any{"request_id": "s", "source": "cli", "cwd": cwd})["session_id"].(string)
	_ = owner.Close(ctx)
	recoverStoreReceipt(t, ctx, firstDB, sid, recoverWithField(t, olderGatewayReference(t, secretScopeStored(t, ctx, firstDB, sid)), "policy", "config_secret_ref"))
	_ = firstDB.Close()
	db, cold := secretScopeOwner(t, ctx, home, "cold")
	return home, db, cold, sid
}

// Replaces the model switch with one landing on m2 at the fixture endpoint
func switchToM2(t *testing.T) {
	t.Helper()
	real := provider.SwitchModel
	t.Cleanup(func() { provider.SwitchModel = real })
	provider.SwitchModel = func(context.Context, provider.SwitchModelOptions) (provider.ModelSwitchResult, error) {
		return provider.ModelSwitchResult{Success: true, NewModel: "m2", TargetProvider: "custom", BaseURL: "http://127.0.0.1:9/v1"}, nil
	}
}

func secretScopeAPIKey(t *testing.T, ctx context.Context, policy *LocalSessionPolicy, owner launchSecretOwner) any {
	t.Helper()
	cfg, err := policy.config(ctx, owner, true)
	if err != nil {
		t.Fatal(err)
	}
	return cfg["model"].(map[string]any)["api_key"]
}

func TestAReferenceStampedByAnOlderGatewayStillRunsItsSession(t *testing.T) {
	ctx := context.Background()
	home, db, cold, sid := olderSession(t, ctx, t.TempDir())
	cfg, err := secretScopeStored(t, ctx, db, sid).config(ctx, cold, true)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg["model"].(map[string]any)["api_key"]; got != "route-token" {
		t.Fatalf("api key %v", got)
	}
	docs := cfg["mcp_servers"].(map[string]any)["docs"].(map[string]any)
	if !reflect.DeepEqual(docs["env"], map[string]any{"DOCS_TOKEN": "mcp-token"}) {
		t.Fatalf("docs env %v", docs["env"])
	}
	envRefsWrite(t, home, "config.yaml", strings.ReplaceAll(secretScopeConfig, "route-token", "rotated-token"))
	fresh := &secretOwnerDouble{dbPath: db.Path(), profile: cold.ProfileID}
	_, err = secretScopeStored(t, ctx, db, sid).config(ctx, fresh, true)
	if field, _ := refusedField(t, err); field != "model.api_key" {
		t.Fatalf("refused field %q", field)
	}
}

func TestModelReStampsAReferenceAnOlderGatewayWrote(t *testing.T) {
	ctx := context.Background()
	switchToM2(t)
	_, db, cold, sid := olderSession(t, ctx, t.TempDir())
	viewer := secretScopeConnect(t, cold)
	t.Cleanup(func() { _ = viewer.Close(ctx) })
	snap, err := db.GetSession(ctx, sid)
	if err != nil || snap == nil {
		t.Fatalf("session row %v %v", snap, err)
	}
	revision, _ := snap.Get("runtime_revision")
	generation, _ := snap.Get("runtime_generation")
	reply := secretScopeDispatch(t, ctx, viewer, "session.mutate", map[string]any{"session_id": sid, "request_id": "switch",
		"operation": "model", "payload": map[string]any{"model": "m2"}, "expected_revision": revision, "expected_generation": generation})
	if _, ok := reply["result"]; !ok {
		t.Fatalf("mutate reply %v", reply)
	}
	switched := secretScopeStored(t, ctx, db, sid)
	if switched.Model == nil || *switched.Model != "m2" {
		t.Fatalf("model %v", switched.Model)
	}
	parsed, err := pyjson.Loads(strings.TrimPrefix(*switched.ConfigSecretRef, configSecretPrefix))
	if err != nil {
		t.Fatal(err)
	}
	source := pyjson.Plain(parsed).(map[string]any)
	if _, bound := source["bound"]; source["policy"] != policyIdentity(switched, nil) || !bound {
		t.Fatalf("reference %v", source)
	}
	restarted := &secretOwnerDouble{dbPath: db.Path(), profile: cold.ProfileID}
	if got := secretScopeAPIKey(t, ctx, switched, restarted); got != "route-token" {
		t.Fatalf("api key after restart %v", got)
	}
}

func TestANonSecretProfileEditKeepsTheSessionAndItsModelSwitch(t *testing.T) {
	edits := map[string]string{
		"base_url":  strings.ReplaceAll(secretScopeConfig, "127.0.0.1:9", "127.0.0.1:10"),
		"reasoning": strings.ReplaceAll(secretScopeConfig, "reasoning_effort: high", "reasoning_effort: low"),
		"mcp_list":  strings.ReplaceAll(secretScopeConfig, "}}}\n", "}}, extra: {command: extra-mcp}}\n"),
	}
	for _, edit := range []string{"base_url", "reasoning", "mcp_list"} {
		t.Run(edit, func(t *testing.T) {
			ctx := context.Background()
			switchToM2(t)
			home := secretScopeHome(t)
			cwd := t.TempDir()
			firstDB, first := secretScopeOwner(t, ctx, home, "first")
			owner := secretScopeConnect(t, first)
			sid, _ := secretScopeCall(t, ctx, owner, "session.create", map[string]any{"request_id": "s", "source": "cli", "cwd": cwd})["session_id"].(string)
			_ = owner.Close(ctx)
			_ = firstDB.Close()
			envRefsWrite(t, home, "config.yaml", edits[edit])
			db, cold := secretScopeOwner(t, ctx, home, "cold")
			viewer := secretScopeConnect(t, cold)
			t.Cleanup(func() { _ = viewer.Close(ctx) })
			resumed := secretScopeCall(t, ctx, viewer, "session.resume", map[string]any{"session_id": sid})
			if got := secretScopeAPIKey(t, ctx, secretScopeStored(t, ctx, db, sid), cold); got != "route-token" {
				t.Fatalf("api key after resume %v", got)
			}
			reply := secretScopeDispatch(t, ctx, viewer, "session.mutate", map[string]any{"session_id": sid, "request_id": "switch",
				"operation": "model", "payload": map[string]any{"model": "m2"},
				"expected_revision": resumed["revision"], "expected_generation": resumed["execution_generation"]})
			if _, ok := reply["result"]; !ok {
				t.Fatalf("mutate reply %v", reply)
			}
			if got := secretScopeAPIKey(t, ctx, secretScopeStored(t, ctx, db, sid), cold); got != "route-token" {
				t.Fatalf("api key after switch %v", got)
			}
		})
	}
}
