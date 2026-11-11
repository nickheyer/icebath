package dashboard

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"weak"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

const (
	gatewaySocket          = "gateway"
	hostTerminalSocket     = "host-terminal"
	speakStreamSocket      = "speak-stream"
	transcribeStreamSocket = "transcribe-stream"
	signedOutCode          = 4401
)

// Voice streams beside the gateway socket, each named for its audio route
var audioSockets = map[string]bool{speakStreamSocket: true, transcribeStreamSocket: true}

// Sockets that open only with a ticket minted for them, never with a standing credential
var ownTicketSockets = map[string]bool{hostTerminalSocket: true}

// Live socket the browser credential admitted, the server context it closes on and its credential generation
type liveBrowserSocket struct {
	ref      weak.Pointer[WebSocket]
	ctx      context.Context
	admitted string
}

var (
	liveBrowserSocketsMu sync.Mutex
	liveBrowserSockets   = map[weak.Pointer[WebSocket]]liveBrowserSocket{}
)

func init() {
	BrowserCredentialOnRotate("sockets", closeAllBrowserSockets)
}

func webSessionSocketsLog() *slog.Logger {
	return slog.Default().With("logger", "hermes_cli.web_session_sockets")
}

// Reason a redeemed ticket may not open the socket, false when it may open it
func ticketRefusal(info *pyjson.Object, socket *string, authRequired bool) (string, bool) {
	named, _ := info.Get("socket")
	namedStr, isStr := named.(string)
	same := (named == nil && socket == nil) || (isStr && socket != nil && namedStr == *socket)
	if !same && (named != nil || (socket != nil && ownTicketSockets[*socket])) {
		namedText := "any socket"
		if pyTruthy(named) {
			namedText = pyrepr.Str(named)
		}
		socketText := "an unnamed socket"
		if socket != nil && *socket != "" {
			socketText = *socket
		}
		return fmt.Sprintf("ticket for %s presented to %s", namedText, socketText), true
	}
	if granted, ok := admittedWith(info); ok && !BrowserCredentialIsCurrent(&granted) {
		return "ticket minted under a browser credential that has since been replaced", true
	}
	if authRequired {
		if v, _ := info.Get("session_token"); v != nil {
			return "ticket minted with the session token, which opens nothing behind the sign-in gate", true
		}
	}
	return "", false
}

// Browser credential generation a redeemed ticket was minted under, false when minted with another credential
func admittedWith(info *pyjson.Object) (string, bool) {
	for _, key := range []string{"session_token", "sign_in"} {
		if v, _ := info.Get(key); pyTruthy(v) {
			s, ok := v.(string)
			return s, ok
		}
	}
	return "", false
}

// Keeps a socket admitted under a browser credential generation until it ends, closing it at once when already replaced
func trackBrowserSocket(ctx context.Context, ws *WebSocket, admitted string) {
	liveBrowserSocketsMu.Lock()
	for key, entry := range liveBrowserSockets {
		if entry.ref.Value() == nil {
			delete(liveBrowserSockets, key)
		}
	}
	ref := weak.Make(ws)
	liveBrowserSockets[ref] = liveBrowserSocket{ref: ref, ctx: ctx, admitted: admitted}
	liveBrowserSocketsMu.Unlock()
	if !BrowserCredentialIsCurrent(&admitted) {
		closeBrowserSocketOn(ws, ctx)
	}
}

// Browser credential generation a live socket was tracked under, false when signing out does not close it
func admissionOf(ws *WebSocket) (string, bool) {
	liveBrowserSocketsMu.Lock()
	entry, ok := liveBrowserSockets[weak.Make(ws)]
	liveBrowserSocketsMu.Unlock()
	if !ok || entry.ref.Value() != ws {
		return "", false
	}
	return entry.admitted, true
}

// Closes a signed out socket, ignoring a socket already closing or gone
func closeSignedOutSocket(ctx context.Context, ws *WebSocket) {
	_ = ws.Close(context.WithoutCancel(ctx), signedOutCode, "Signed out")
}

// Schedules the close of a socket while its server still runs, false once that server ended
func closeBrowserSocketOn(ws *WebSocket, ctx context.Context) bool {
	if ctx.Err() != nil {
		return false
	}
	go closeSignedOutSocket(ctx, ws)
	return true
}

// Closes every live socket the browser credential admitted, returning how many were asked to close
func closeAllBrowserSockets() int {
	liveBrowserSocketsMu.Lock()
	live := make([]liveBrowserSocket, 0, len(liveBrowserSockets))
	for _, entry := range liveBrowserSockets {
		live = append(live, entry)
	}
	clear(liveBrowserSockets)
	liveBrowserSocketsMu.Unlock()
	closing := 0
	for _, entry := range live {
		if ws := entry.ref.Value(); ws != nil && closeBrowserSocketOn(ws, entry.ctx) {
			closing++
		}
	}
	webSessionSocketsLog().Info(fmt.Sprintf("Closing %d browser sockets opened with the replaced browser credential", closing))
	return closing
}
