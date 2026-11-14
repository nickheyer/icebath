package tuiserver

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/mcp"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/provider/anonportaltest"
	"github.com/nickheyer/icebath/pkg/tools"
)

var connectorsPageCatalog = map[string]any{"connectors": []any{map[string]any{"slug": "notion", "name": "Notion",
	"description": "Docs", "category": "Productivity", "logoUrl": nil, "state": "live"}}}

// Fake Portal with no identity, its connector routes and the connector gateway double of one case
type connectorsPage struct {
	portal    *anonportaltest.Portal
	mu        sync.Mutex
	requests  [][2]any
	lists     int
	connected bool
}

// Installs the fake Portal, the connector route recorder when asked and the connector gateway double
func connectorsPageFixture(t *testing.T, connectorRoutes bool) *connectorsPage {
	t.Helper()
	page := &connectorsPage{}
	provider.ResetAnonPortalMemosForTests()
	tools.LiveResetForTests()
	t.Cleanup(func() {
		tools.LiveResetForTests()
		provider.ResetAnonPortalMemosForTests()
	})
	var holder atomic.Pointer[anonportaltest.Portal]
	page.portal = anonportaltest.Serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if connectorRoutes && strings.HasPrefix(r.URL.Path, "/api/v1/connectors/") {
			page.mu.Lock()
			page.requests = append(page.requests, [2]any{r.URL.Path, r.Header.Get("Authorization") != ""})
			page.mu.Unlock()
			body := any(connectorsPageCatalog)
			if strings.HasSuffix(r.URL.Path, "/accounts") {
				body = map[string]any{"accounts": []any{}}
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(body)
			return
		}
		holder.Load().ServeDefault(w, r)
	}))
	holder.Store(page.portal)
	if connectorRoutes {
		t.Setenv("REQUESTS_CA_BUNDLE", page.portal.CAPath)
	}
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var reply any
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/v1/connectors"):
			page.mu.Lock()
			page.lists++
			connected := page.connected
			page.mu.Unlock()
			reply = map[string]any{"items": []any{map[string]any{"connector": "notion", "enabled": true, "connected": connected}}}
		case strings.HasSuffix(r.URL.Path, "/v1/connectors/connections"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			results := []any{}
			names, _ := body["connectors"].([]any)
			for _, name := range names {
				results = append(results, map[string]any{"connector": name, "status": "active", "connectionId": "c1"})
			}
			reply = map[string]any{"results": results}
		default:
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(reply)
	}))
	t.Cleanup(gateway.Close)
	t.Setenv("CONNECTOR_GATEWAY_URL", gateway.URL)
	return page
}

// Recorded Portal connector requests and gateway list calls
func (p *connectorsPage) seen() ([][2]any, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([][2]any(nil), p.requests...), p.lists
}

// Whether the gateway double lists notion as connected
func (p *connectorsPage) setConnected(connected bool) {
	p.mu.Lock()
	p.connected = connected
	p.mu.Unlock()
}

// One request through the real dispatcher
func connectorsPageCall(method string, params map[string]any) map[string]any {
	if params == nil {
		params = map[string]any{}
	}
	return handleRequest(context.Background(), map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
}

// Result of one request, failing the case on an error reply
func connectorsPageResult(t *testing.T, method string, params map[string]any) map[string]any {
	t.Helper()
	response := connectorsPageCall(method, params)
	result, isDict := response["result"].(map[string]any)
	if !isDict {
		t.Fatalf("%s answered %v", method, response)
	}
	return result
}

// Account owner params of the Connectors page with optional extra keys
func connectorsPageAccount(extra map[string]any) map[string]any {
	params := map[string]any{"owner": map[string]any{"type": "account"}}
	for key, value := range extra {
		params[key] = value
	}
	return params
}

// Tool env of the process home with no session bound, the CLI and agent entry
func connectorsPageEnv() *tools.Env {
	return &tools.Env{Home: home.Process(), Values: &sync.Map{}}
}

// Seconds until the connector offer is due, false when none is pending
func connectorsPageOfferDueIn(t *testing.T) (int, bool) {
	t.Helper()
	due, pending, err := provider.OfferDueIn(provider.HomePoolEnv(home.Root()), provider.FreeTierOfferConnector)
	if err != nil {
		t.Fatal(err)
	}
	return due, pending
}

// Agent status read of the connectors tool behind the real availability gate
func connectorsPageAgentStatus(env *tools.Env) map[string]any {
	raw := tools.ManageConnections(tools.WithToolEnv(context.Background(), env), env, map[string]any{"action": "status"},
		tools.ManageOptions{ConnectorsAvailable: func() bool { return tools.ConnectorsAvailable(env) }})
	var result map[string]any
	_ = json.Unmarshal([]byte(raw), &result)
	return result
}

// First row of a connectors listing
func connectorsPageFirstRow(t *testing.T, result map[string]any) map[string]any {
	t.Helper()
	rows, _ := result["connectors"].([]any)
	if len(rows) == 0 {
		t.Fatalf("no connector rows in %v", result)
	}
	row, _ := pyjson.Plain(rows[0]).(map[string]any)
	return row
}

func TestOpeningThePageCreatesNoIdentityAndReadsThePublicCatalog(t *testing.T) {
	page := connectorsPageFixture(t, true)
	if got := connectorsPageResult(t, "connectors.list", connectorsPageAccount(nil)); !pyjson.Equal(pyjson.Plain(got),
		map[string]any{"available": true, "connectors": []any{}}) {
		t.Fatalf("list %v", got)
	}
	var slugs []any
	for _, row := range pyjson.Plain(connectorsPageResult(t, "connectors.catalog", nil)["connectors"]).([]any) {
		slugs = append(slugs, row.(map[string]any)["slug"])
	}
	if !pyjson.Equal(slugs, []any{"notion"}) {
		t.Fatalf("catalog slugs %v", slugs)
	}
	if got := connectorsPageResult(t, "connectors.accounts", nil); !pyjson.Equal(pyjson.Plain(got), map[string]any{"accounts": []any{}}) {
		t.Fatalf("accounts %v", got)
	}
	if code := rpcCode(connectorsPageCall("connectors.policy.get", nil)); code != 4032 {
		t.Fatalf("policy code %d", code)
	}
	requests, lists := page.seen()
	if len(requests) != 1 || requests[0] != [2]any{"/api/v1/connectors/catalog/public", false} {
		t.Fatalf("portal connector requests %v", requests)
	}
	if page.portal.Creates() != 0 || lists != 0 {
		t.Fatalf("creates %d lists %d", page.portal.Creates(), lists)
	}
}

func TestExplicitListsStillCreateOneIdentityAndListFromTheGateway(t *testing.T) {
	for _, entry := range []string{"cli", "agent"} {
		t.Run(entry, func(t *testing.T) {
			page := connectorsPageFixture(t, false)
			env := connectorsPageEnv()
			if entry == "cli" {
				var out, errOut bytes.Buffer
				code, err := mcp.ConnectorsCommand(context.Background(), env, map[string]any{"connectors_action": "list"}, &out, &errOut)
				if err != nil || code != 0 {
					t.Fatalf("cli list %d %v %s %s", code, err, out.String(), errOut.String())
				}
			} else if row := connectorsPageFirstRow(t, connectorsPageAgentStatus(env)); row["connector"] != "notion" {
				t.Fatalf("agent row %v", row)
			}
			if _, lists := page.seen(); page.portal.Creates() != 1 || lists != 1 {
				t.Fatalf("creates %d lists %d calls %v", page.portal.Creates(), lists, page.portal.Calls())
			}
		})
	}
}

func TestFirstConnectCreatesOneIdentityThenThePageReadsSignedIn(t *testing.T) {
	page := connectorsPageFixture(t, true)
	t.Cleanup(provider.PinFreeTierOfferClockForTests(1_000_000.0))
	connectorsPageResult(t, "connectors.connect", connectorsPageAccount(map[string]any{"connectors": []any{"notion"}}))
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, due := connectorsPageResult(t, "free_tier.status", nil)["connector_nudge_due_in"]; due {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the connector offer was never armed")
		}
		time.Sleep(20 * time.Millisecond)
	}
	status := connectorsPageResult(t, "free_tier.status", nil)
	if !pyjson.Equal(status["connector_nudge_due_in"], provider.OfferDelaySeconds) {
		t.Fatalf("status %v", status)
	}
	if _, nudge := status["nudge_due_in"]; nudge || !truthy(status["has_guest"]) || truthy(status["available"]) {
		t.Fatalf("status %v", status)
	}
	if row := connectorsPageFirstRow(t, connectorsPageResult(t, "connectors.list", connectorsPageAccount(nil))); row["connector"] != "notion" {
		t.Fatalf("list row %v", row)
	}
	connectorsPageResult(t, "connectors.catalog", nil)
	requests, _ := page.seen()
	if len(requests) != 1 || requests[0] != [2]any{"/api/v1/connectors/catalog", true} {
		t.Fatalf("portal connector requests %v", requests)
	}
	if page.portal.Creates() != 1 {
		t.Fatalf("creates %d", page.portal.Creates())
	}
}

func TestAConnectByLinkSeenByAStatusReadArmsTheConnectorOffer(t *testing.T) {
	page := connectorsPageFixture(t, false)
	t.Cleanup(provider.PinFreeTierOfferClockForTests(1_000_000.0))
	page.setConnected(true)
	env := connectorsPageEnv()
	for range 2 {
		connectorsPageAgentStatus(env)
	}
	if due, pending := connectorsPageOfferDueIn(t); !pending || due != provider.OfferDelaySeconds {
		t.Fatalf("offer due %d %v", due, pending)
	}
	if page.portal.Creates() != 1 {
		t.Fatalf("creates %d", page.portal.Creates())
	}
}

func TestThePageListArmsTheOfferForAppsAGuestConnectedOnAnOlderHermes(t *testing.T) {
	page := connectorsPageFixture(t, false)
	t.Cleanup(provider.PinFreeTierOfferClockForTests(1_000_000.0))
	if failure := tools.EnsureGuestIdentity(connectorsPageEnv()); failure != "" {
		t.Fatalf("guest identity %q", failure)
	}
	page.setConnected(true)
	if row := connectorsPageFirstRow(t, connectorsPageResult(t, "connectors.list", connectorsPageAccount(nil))); row["connected"] != true {
		t.Fatalf("list row %v", row)
	}
	if due, pending := connectorsPageOfferDueIn(t); !pending || due != provider.OfferDelaySeconds {
		t.Fatalf("offer due %d %v", due, pending)
	}
	if page.portal.Creates() != 1 {
		t.Fatalf("creates %d", page.portal.Creates())
	}
}

func TestAFailedOfferWriteIsRetriedOnTheNextSighting(t *testing.T) {
	page := connectorsPageFixture(t, false)
	t.Cleanup(provider.PinFreeTierOfferClockForTests(1_000_000.0))
	if failure := tools.EnsureGuestIdentity(connectorsPageEnv()); failure != "" {
		t.Fatalf("guest identity %q", failure)
	}
	page.setConnected(true)
	locked := filepath.Join(home.Root(), "free_tier", "connector_offer.lock")
	if err := os.MkdirAll(locked, 0o700); err != nil {
		t.Fatal(err)
	}
	connectorsPageResult(t, "connectors.list", connectorsPageAccount(nil))
	if err := os.Remove(locked); err != nil {
		t.Fatal(err)
	}
	if due, pending := connectorsPageOfferDueIn(t); pending {
		t.Fatalf("offer armed after a failed write %d", due)
	}
	connectorsPageResult(t, "connectors.list", connectorsPageAccount(nil))
	if due, pending := connectorsPageOfferDueIn(t); !pending || due != provider.OfferDelaySeconds {
		t.Fatalf("offer due %d %v", due, pending)
	}
}
