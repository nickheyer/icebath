package dashboard

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Close code of the socket within the window, zero when it stays open
func displayCloseCode(t *testing.T, conn *testWebSocket, within time.Duration) int {
	t.Helper()
	select {
	case m := <-conn.fromApp:
		if m.Type == "websocket.close" {
			return m.Code
		}
		t.Fatalf("unexpected frame %+v", m)
	case <-time.After(within):
	}
	return 0
}

// Display route alone on a loopback app whose bridge stands in as a viewer staying until the server closes it
func displayViewerClient(t *testing.T) *testClient {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	resetWSTickets()
	t.Cleanup(resetWSTickets)
	swapVar(t, &displayBridgeRun, func(ctx context.Context, ws *WebSocket, _ *pyjson.Object) error {
		for {
			m, err := ws.Receive(ctx)
			if err != nil || m.Type == "websocket.disconnect" {
				return nil
			}
		}
	})
	previous := SessionToken()
	setSessionToken(strings.Repeat("a", 43))
	t.Cleanup(func() { setSessionToken(previous) })
	app := NewFastAPI("display", "0", nil, nil)
	app.State.Set("auth_required", false)
	app.IncludeRouter(displayRouter(), "")
	return newTestClient(t, app)
}

// Display ticket of the viewer pinned to the bot profile home, with the extra fields
func displayViewerTicket(extra ...any) string {
	fields := pyjson.ObjectOf(append([]any{"hermes_home", "/srv/icebath/bot-a", "viewer_id", "v"}, extra...)...)
	return MintWSTicket("display:v", "bot-desktop", "", fields)
}

func TestABadTicketIsRefusedWithACloseFrameTheRendererCanRead(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	resetWSTickets()
	t.Cleanup(resetWSTickets)
	app := dashboardTestApp(t)
	stateForCase(t, app, "auth_required", false)
	stateForCase(t, app, "bound_host", nil)
	used := displayViewerTicket()
	if _, err := ConsumeWSTicket(used); err != nil {
		t.Fatal(err)
	}
	conn, refused := newTestClient(t, app).websocketConnect("/api/display/ws?display_ticket=" + used)
	if refused != nil {
		t.Fatalf("the handshake was refused before accept: %v", refused)
	}
	if m := conn.next(); m.Type != "websocket.close" || m.Code != 4401 {
		t.Fatalf("frame = %+v", m)
	}
}

func TestSigningOutClosesAScreenViewerABrowserTabOpened(t *testing.T) {
	client := displayViewerClient(t)
	tab := displayViewerTicket("admitted_with", BrowserCredentialGenerationOf(strings.Repeat("a", 43)))
	native := displayViewerTicket()
	fromTab := connectOK(t, client, "/api/display/ws?display_ticket="+tab)
	fromNative := connectOK(t, client, "/api/display/ws?display_ticket="+native)
	if code := displayCloseCode(t, fromTab, 200*time.Millisecond); code != 0 {
		t.Fatalf("the tab viewer closed with %d before sign out", code)
	}
	setSessionToken(strings.Repeat("b", 43))
	closeAllBrowserSockets()
	if code := displayCloseCode(t, fromTab, 5*time.Second); code != 4401 {
		t.Fatalf("the tab viewer close code = %d", code)
	}
	if code := displayCloseCode(t, fromNative, 500*time.Millisecond); code != 0 {
		t.Fatalf("the native viewer closed with %d", code)
	}
	_ = fromNative.close(1000)
}

func TestAScreenViewerTicketFromASignedOutTabOpensNothing(t *testing.T) {
	client := displayViewerClient(t)
	stale := displayViewerTicket("admitted_with", BrowserCredentialGenerationOf(strings.Repeat("a", 43)))
	setSessionToken(strings.Repeat("b", 43))
	conn := connectOK(t, client, "/api/display/ws?display_ticket="+stale)
	if code := displayCloseCode(t, conn, 2*time.Second); code != 4401 {
		t.Fatalf("close code = %d", code)
	}
}
