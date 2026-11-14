package dashboard

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Mint target of a gateway socket ticket on the Webapp
const gatewayTicketMint = "/api/webapp/socket-ticket?socket=gateway"

// Session token the Webapp ticket cases install
var gatewayTicketToken = strings.Repeat("a", 43)

// Webapp surface on loopback with no auth gate and the case session token, served at 127.0.0.1
func gatewayTicketWebapp(t *testing.T) (*FastAPI, *testClient) {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	app := dashboardTestApp(t)
	app.State.Set("ui_surface", "webapp")
	app.State.Set("auth_required", false)
	app.State.Set("bound_host", "127.0.0.1")
	previous := SessionToken()
	setSessionToken(gatewayTicketToken)
	t.Cleanup(func() { setSessionToken(previous) })
	return app, newTestClientAt(t, app, "http", "127.0.0.1", 80)
}

// Browser upgrade of /api/ws offering the public protocol and the ticket, enough for the upgrade gate
func gatewayTicketSocket(app *FastAPI, ticket string) *WebSocket {
	scope := testScope("websocket", "GET", "/api/ws?native_dial=browser", []string{"sec-websocket-protocol",
		gateway.CanonicalGatewayProtocol + ", " + gateway.GatewayWSTicketPrefix + ticket})
	scope.App, scope.Client = app, &Address{"127.0.0.1", 50000}
	return NewWebSocket(scope, nil, nil)
}

func TestLoopbackTicketNamesTheSessionTokenOwnerAndIsSpentOnce(t *testing.T) {
	app, client := gatewayTicketWebapp(t)
	if status := client.request("POST", gatewayTicketMint, nil).Status; status != 401 {
		t.Fatalf("mint without a token = %d", status)
	}
	minted := client.request("POST", gatewayTicketMint, nil, "X-Icebath-Session-Token", gatewayTicketToken)
	if minted.Status != 200 {
		t.Fatalf("mint = %d %s", minted.Status, minted.Body)
	}
	if got := minted.Headers.Get("cache-control"); got != "no-store" {
		t.Fatalf("cache-control = %q", got)
	}
	body, _ := pyjson.Plain(minted.JSON(t)).(map[string]any)
	ttl, _ := body["ttl_seconds"].(int64)
	ticket, _ := body["ticket"].(string)
	if len(body) != 2 || ticket == "" || ttl <= 0 {
		t.Fatalf("mint body = %v", body)
	}
	socket := gatewaySocket
	first := gatewayTicketSocket(app, ticket)
	if ok, err := wsAuthOK(context.Background(), first, false, &socket); err != nil || !ok {
		t.Fatalf("first redemption = %v %v", ok, err)
	}
	if identity := wsAuthIdentity(first); !reflect.DeepEqual(identity, map[string]any{"user_id": "legacy-token-owner", "provider": "session-token"}) {
		t.Fatalf("identity = %v", identity)
	}
	if protocol := wsSubprotocol(first); protocol == nil || *protocol != gateway.CanonicalGatewayProtocol {
		t.Fatalf("subprotocol = %v", protocol)
	}
	if home := wsTicketProfileHome(first); home != nil {
		t.Fatalf("a profileless ticket bound the socket to %v", home)
	}
	if ok, err := wsAuthOK(context.Background(), gatewayTicketSocket(app, ticket), false, &socket); err != nil || ok {
		t.Fatalf("second redemption = %v %v", ok, err)
	}
}

func TestAMissingProfileIsRefusedBeforeATicketExists(t *testing.T) {
	_, client := gatewayTicketWebapp(t)
	refused := client.request("POST", gatewayTicketMint+"&profile=missing", nil, "X-Icebath-Session-Token", gatewayTicketToken)
	if refused.Status != 404 {
		t.Fatalf("mint for a missing profile = %d %s", refused.Status, refused.Body)
	}
}

func TestOtherSurfacesDoNotMintWebappSocketTickets(t *testing.T) {
	for _, surface := range []string{"dashboard", "serve"} {
		t.Run(surface, func(t *testing.T) {
			app, client := gatewayTicketWebapp(t)
			app.State.Set("ui_surface", surface)
			refused := client.request("POST", gatewayTicketMint, nil, "X-Icebath-Session-Token", gatewayTicketToken)
			if refused.Status != 404 {
				t.Fatalf("mint on %s = %d %s", surface, refused.Status, refused.Body)
			}
		})
	}
}
