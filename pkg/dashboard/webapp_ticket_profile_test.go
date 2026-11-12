package dashboard

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

// Session token the ticket profile cases install
var ticketProfileToken = strings.Repeat("b", 43)

// Gateway runner holding the multiplexed registry and its launch authority, never draining
type ticketProfileRunner struct {
	mu        sync.Mutex
	registry  *gateway.SessionAuthorities
	authority *gateway.SessionAuthority
}

func (r *ticketProfileRunner) SessionRuntimeDescriptor() map[string]any { return nil }
func (r *ticketProfileRunner) Draining() bool                           { return false }
func (r *ticketProfileRunner) Running() bool                            { return true }
func (r *ticketProfileRunner) SessionStore() *gateway.SessionStore      { return nil }
func (r *ticketProfileRunner) AdapterForSource(context.Context, *gateway.SessionSource) (gateway.PlatformAdapter, error) {
	return nil, nil
}
func (r *ticketProfileRunner) MultiplexProfiles() bool                         { return false }
func (r *ticketProfileRunner) SessionAuthorities() *gateway.SessionAuthorities { return r.registry }
func (r *ticketProfileRunner) SessionAuthority() *gateway.SessionAuthority {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.authority
}

// Webapp over a launch profile and a served beta profile, each with its own authority in a multiplexed registry
func twoProfiles(t *testing.T) (*testClient, map[string]string) {
	t.Helper()
	tmp := t.TempDir()
	launch := filepath.Join(tmp, ".icebath")
	beta := filepath.Join(launch, "profiles", "beta")
	if err := os.MkdirAll(beta, 0o755); err != nil {
		t.Fatal(err)
	}
	homes := map[string]string{"default": launch, "beta": beta}
	for _, h := range homes {
		if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	app := dashboardTestApp(t)
	t.Cleanup(home.PatchUserHome(tmp))
	t.Setenv("ICEBATH_HOME", launch)
	ctx := context.Background()
	runner := &ticketProfileRunner{registry: gateway.NewSessionAuthorities(launch, true)}
	authorities := map[string]*gateway.SessionAuthority{}
	for _, name := range []string{"default", "beta"} {
		db, err := state.NewSessionDB(ctx, filepath.Join(homes[name], "state.db"), false)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		epoch, err := db.BeginRuntimeEpoch(ctx, "ticket-profile")
		if err != nil {
			t.Fatal(err)
		}
		authorities[name] = gateway.NewSessionAuthority(runner, homes[name], "ticket-profile", db, epoch)
		profile := name
		if err := runner.registry.Add(homes[name], authorities[name], &profile); err != nil {
			t.Fatal(err)
		}
	}
	runner.mu.Lock()
	runner.authority = authorities["default"]
	runner.mu.Unlock()
	stateForCase(t, app, "session_authority", authorities["default"])
	stateForCase(t, app, "ui_surface", "webapp")
	stateForCase(t, app, "auth_required", false)
	stateForCase(t, app, "bound_host", "127.0.0.1")
	previous := SessionToken()
	setSessionToken(ticketProfileToken)
	t.Cleanup(func() { setSessionToken(previous) })
	return newTestClientAt(t, app, "http", "127.0.0.1", 80), homes
}

// Reply to method on a tab socket whose gateway ticket was minted for the profile, empty for the launch profile
func askOnTab(t *testing.T, client *testClient, method string, params map[string]any, profile string) map[string]any {
	t.Helper()
	query := ""
	if profile != "" {
		query = "&profile=" + url.QueryEscape(profile)
	}
	minted := client.request("POST", "/api/webapp/socket-ticket?socket=gateway"+query, nil, "X-Icebath-Session-Token", ticketProfileToken)
	body, _ := pyjson.Plain(minted.JSON(t)).(map[string]any)
	ticket, _ := body["ticket"].(string)
	if minted.Status != 200 || ticket == "" {
		t.Fatalf("mint = %d %s", minted.Status, minted.Body)
	}
	conn, refused := client.websocketConnect("/api/ws?native_dial=browser&ticket="+url.QueryEscape(ticket)+query, "host", "127.0.0.1")
	if refused != nil {
		t.Fatalf("the tab socket was refused: %v", refused)
	}
	defer conn.close(1000)
	request, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": "ask", "method": method, "params": params})
	if err != nil {
		t.Fatal(err)
	}
	conn.sendText(string(request))
	for {
		if frame := receiveJSON(t, conn); frame["id"] == "ask" {
			return frame
		}
	}
}

// Profile home the tab socket answers for
func answeringHome(t *testing.T, client *testClient, profile string) string {
	t.Helper()
	reply := askOnTab(t, client, "runtime.describe", map[string]any{}, profile)
	result, ok := reply["result"].(map[string]any)
	if !ok {
		t.Fatalf("runtime.describe = %v", reply)
	}
	profileID, _ := result["profile_id"].(string)
	return filepath.Clean(profileID)
}

func TestATabOpenedOnAProfileIsAnsweredForItAndKeepsItsHostScope(t *testing.T) {
	client, homes := twoProfiles(t)
	if got := answeringHome(t, client, "beta"); got != homes["beta"] {
		t.Fatalf("answered for %s, want %s", got, homes["beta"])
	}
	reply := askOnTab(t, client, "session.active_list", map[string]any{"profile": "default"}, "beta")
	if !reflect.DeepEqual(reply["result"], map[string]any{"sessions": []any{}}) {
		t.Fatalf("session.active_list = %v", reply)
	}
}

func TestATabOpenedWithoutAProfileIsAnsweredForTheLaunchProfile(t *testing.T) {
	client, homes := twoProfiles(t)
	if got := answeringHome(t, client, ""); got != homes["default"] {
		t.Fatalf("answered for %s, want %s", got, homes["default"])
	}
}
