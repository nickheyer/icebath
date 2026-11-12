package dashboard

import (
	"context"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func TestDisplayTicketIsRefusedAsAGatewayLogin(t *testing.T) {
	resetWSTickets()
	t.Cleanup(resetWSTickets)
	app := dashboardTestApp(t)
	stateForCase(t, app, "auth_required", true)
	ticket := MintWSTicket("display:v", "bot-desktop", "", pyjson.ObjectOf("hermes_home", "/srv/icebath/bot-a", "viewer_id", "v"))
	scope := testScope("websocket", "GET", "/api/ws?ticket="+ticket, nil)
	scope.App, scope.Client = app, &Address{"203.0.113.9", 50000}
	ws := NewWebSocket(scope, nil, nil)
	reason, _, err := wsAuthReason(context.Background(), ws, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if reason != "ticket_invalid" {
		t.Fatalf("reason = %q", reason)
	}
	if identity := wsAuthIdentity(ws); identity != nil {
		t.Fatalf("a display ticket stamped a login identity: %v", identity)
	}
}
