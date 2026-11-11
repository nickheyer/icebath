package dashboard

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net/netip"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Lifetimes and cap of the one use window and launch handoff tickets
const (
	webappWindowTicketTTLSeconds = 30
	webappLaunchTicketTTLSeconds = 60
	webappMaxWindowTickets       = 128
)

var webappTicketMu sync.Mutex

// Identity of a socket opened with the loopback session token
var webappSessionTokenOwner = [3]string{"legacy-token-owner", "session-token", ""}

// Pending handoff ticket, its expiry, the browser credential generation it spends and whether a launcher carried it
type webappWindowTicket struct {
	expires    float64
	generation string
	launch     bool
}

// 404 unless the app serves the private Webapp launch link
func requirePrivateWebapp(req *Request) error {
	if !PrivateLaunch(req.App().State) {
		return NewHTTPException(404, "Private Webapp window handoff is unavailable")
	}
	return nil
}

// Pending handoff tickets kept on the app state
func webappWindowTickets(state *State) map[string]webappWindowTicket {
	tickets, _ := state.Get("webapp_window_tickets").(map[string]webappWindowTicket)
	if tickets == nil {
		tickets = map[string]webappWindowTicket{}
	}
	return tickets
}

// One use ticket the window session route exchanges for the current session token, never storing the credential
func mintHandoffTicket(state *State, ttl float64, launch bool) (string, error) {
	now := monotonic()
	webappTicketMu.Lock()
	defer webappTicketMu.Unlock()
	live := map[string]webappWindowTicket{}
	for key, value := range webappWindowTickets(state) {
		gen := value.generation
		if value.expires > now && BrowserCredentialIsCurrent(&gen) {
			live[key] = value
		}
	}
	state.Set("webapp_window_tickets", live)
	if len(live) >= webappMaxWindowTickets {
		return "", NewHTTPException(429, "Too many pending Webapp windows; try again shortly")
	}
	ticket := tokenURLSafe(32)
	live[ticket] = webappWindowTicket{expires: now + ttl, generation: BrowserCredentialGeneration(), launch: launch}
	return ticket, nil
}

// Ticket for the browser the server opens itself, carried in the launcher argv instead of the token
func mintLaunchTicket(state *State) (string, error) {
	return mintHandoffTicket(state, webappLaunchTicketTTLSeconds, true)
}

// Ticket for a launch link the user opens by hand, spent without the connection owner check
func mintPrintedLaunchTicket(state *State) (string, error) {
	return mintHandoffTicket(state, webappLaunchTicketTTLSeconds, false)
}

// Address of a host, IPv4 mapped addresses unwrapped, with its port
type peerAddress struct {
	addr netip.Addr
	port int
}

// Normalised address of a textual host and port, false for an invalid address
func normalPeer(host string, port int) (peerAddress, bool) {
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return peerAddress{}, false
	}
	return peerAddress{addr.Unmap(), port}, true
}

// Address of a proc net tcp column, each 32 bit word printed in host byte order
func procRowAddress(field string) (peerAddress, bool) {
	hostHex, portHex, ok := strings.Cut(field, ":")
	if !ok || strings.Contains(portHex, ":") {
		return peerAddress{}, false
	}
	raw, err := hex.DecodeString(hostHex)
	if err != nil {
		return peerAddress{}, false
	}
	packed := make([]byte, 0, len(raw))
	for i := 0; i < len(raw); i += 4 {
		word := make([]byte, 4)
		chunk := raw[i:min(i+4, len(raw))]
		var n uint32
		for _, b := range chunk {
			n = n<<8 | uint32(b)
		}
		binary.NativeEndian.PutUint32(word, n)
		packed = append(packed, word...)
	}
	addr, ok := netip.AddrFromSlice(packed)
	if !ok {
		return peerAddress{}, false
	}
	port, err := strconv.ParseInt(portHex, 16, 64)
	if err != nil {
		return peerAddress{}, false
	}
	return peerAddress{addr.Unmap(), int(port)}, true
}

// Owner uid of the client end of a local TCP connection from the Linux proc tables, false when absent
func loopbackPeerUID(clientHost string, clientPort int, serverHost string, serverPort int) (int, bool) {
	client, ok := normalPeer(clientHost, clientPort)
	if !ok {
		return 0, false
	}
	server, ok := normalPeer(serverHost, serverPort)
	if !ok {
		return 0, false
	}
	for _, table := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		data, err := os.ReadFile(table)
		if err != nil {
			continue
		}
		lines := strings.Split(strings.ReplaceAll(strings.ReplaceAll(string(data), "\r\n", "\n"), "\r", "\n"), "\n")
		if len(lines) > 0 && lines[len(lines)-1] == "" {
			lines = lines[:len(lines)-1]
		}
		if len(lines) > 0 {
			lines = lines[1:]
		}
		for _, row := range lines {
			fields := strings.Fields(row)
			if len(fields) < 8 {
				continue
			}
			local, ok1 := procRowAddress(fields[1])
			remote, ok2 := procRowAddress(fields[2])
			if !ok1 || !ok2 {
				continue
			}
			if local == client && remote == server {
				uid, err := strconv.Atoi(fields[7])
				if err != nil {
					continue
				}
				return uid, true
			}
		}
	}
	return 0, false
}

// Python sys.platform of this host
func webappSysPlatform() string {
	switch runtime.GOOS {
	case "windows":
		return "win32"
	case "linux", "android":
		return "linux"
	}
	return runtime.GOOS
}

// Whether a launcher carried ticket may be spent over this connection, the server user check on Linux
func peerIsServerUser(req *Request, platform string) bool {
	if platform == "win32" {
		return true
	}
	if platform != "linux" {
		return false
	}
	client, server := req.Client(), req.Scope.Server
	if client == nil || server == nil {
		return false
	}
	uid, ok := loopbackPeerUID(client.Host, client.Port, server.Host, server.Port)
	return ok && uid == os.Getuid()
}

// Identity of the caller and the session token it presented, its verified login or the loopback token owner
func ticketPrincipal(req *Request) (*pyjson.Object, *string, error) {
	if v, ok := req.State().Lookup("session"); ok {
		if session, ok := v.(*gateway.DashboardSession); ok && session != nil {
			return pyjson.ObjectOf("user_id", session.UserID, "provider", session.Provider, "issuer", session.Issuer), nil, nil
		}
	}
	if !req.App().State.Bool("auth_required") {
		presented := req.Headers().Get(sessionHeaderName)
		if presented == "" {
			presented = strings.TrimPrefix(req.Headers().Get("authorization"), "Bearer ")
		}
		if hasValidSessionToken(req) {
			return pyjson.ObjectOf("user_id", webappSessionTokenOwner[0], "provider", webappSessionTokenOwner[1],
				"issuer", webappSessionTokenOwner[2]), &presented, nil
		}
	}
	return nil, nil, NewHTTPException(401, "Unauthorized")
}

// One use ticket that opens only socket as identity, bound to the credential generation it was minted under
func mintSocketTicket(identity *pyjson.Object, socket string, sessionToken, signIn, profileHome *string) string {
	extra := pyjson.ObjectOf("socket", socket)
	if sessionToken != nil {
		extra.Set("session_token", BrowserCredentialGenerationOf(*sessionToken))
	}
	if signIn != nil {
		extra.Set("sign_in", *signIn)
	}
	if profileHome != nil {
		extra.Set("profile_home", *profileHome)
	}
	userID, _ := identity.GetOr("user_id", "").(string)
	provider, _ := identity.GetOr("provider", "").(string)
	issuer, _ := identity.GetOr("issuer", "").(string)
	return MintWSTicket(userID, provider, issuer, extra)
}

// Uncached JSON body of a socket ticket and its lifetime
func socketTicketResponse(req *Request, identity *pyjson.Object, socket string, sessionToken, profileHome *string) (*Response, error) {
	ticket := mintSocketTicket(identity, socket, sessionToken, webappSignInOf(&req.HTTPConnection), profileHome)
	return JSONResponse(pyjson.ObjectOf("ticket", ticket, "ttl_seconds", int64(WSTicketTTLSeconds)), 200, "Cache-Control", "no-store")
}

// Surface policy field that lets a surface mint each socket ticket and whether the mint checks the profile
var webappSocketTickets = map[string]struct {
	surface       string
	checksProfile bool
}{
	gatewaySocket:          {"gateway_ticket", true},
	hostTerminalSocket:     {"host_terminal", false},
	speakStreamSocket:      {"gateway_ticket", false},
	transcribeStreamSocket: {"gateway_ticket", false},
}

// Credential free uncached page whose only script is script with the base path bound
func handoffPage(req *Request, title, status, script string) (*Response, error) {
	prefix := gateway.NormalisePrefix(req.Headers().Get("x-forwarded-prefix"))
	nonce := tokenURLSafe(16)
	encoded, err := pyjson.Dumps(prefix)
	if err != nil {
		return nil, err
	}
	script = strings.ReplaceAll(script, "__BASE_PATH__", strings.ReplaceAll(encoded, "</", `<\/`))
	return HTMLResponse(`<!doctype html><meta charset="utf-8"><meta name="referrer" content="no-referrer">`+
		"<title>"+title+`</title><p id="status">`+status+"</p>"+
		`<script nonce="`+nonce+`">`+script+"</script>", 200,
		"Cache-Control", "no-store",
		"Referrer-Policy", "no-referrer",
		"Content-Security-Policy", fmt.Sprintf("default-src 'none'; script-src 'nonce-%s'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'", nonce),
	), nil
}

// Router of the Webapp host terminal socket, socket tickets and private window handoff pages
func webappRouter() *APIRouter {
	r := NewAPIRouter("")
	r.WebSocket("/api/host-terminal", &WSEndpoint{
		Name: "host_terminal_ws",
		Handle: func(ctx context.Context, c *Call) error {
			ws := c.WebSocket
			peer, mode, cred, ok, err := wsGate(ctx, ws, hostTerminalSocket)
			if err != nil || !ok {
				return err
			}
			if !hostTerminalRequestAllowed(ws.App()) {
				return ws.Close(ctx, 4403, "host terminal requires authenticated Webapp")
			}
			if err := ws.Accept(ctx, nil, nil); err != nil {
				return err
			}
			webServerLog().Info(fmt.Sprintf("host terminal accepted peer=%s mode=%s cred=%s", peer, mode, cred))
			if !tools.TerminalBridgeAvailable() {
				return ptyFail(ctx, ws, &tools.PtyUnavailableError{Msg: "Pseudo-terminal support is not installed on this host."}, "Terminal")
			}
			return hostTerminal(ctx, ws)
		},
	})
	r.Post("/api/webapp/window-ticket", &Endpoint{
		Name: "issue_window_ticket",
		Handle: func(ctx context.Context, c *Call) (any, error) {
			if err := requirePrivateWebapp(c.Request); err != nil {
				return nil, err
			}
			if err := requireToken(c.Request); err != nil {
				return nil, err
			}
			ticket, err := mintHandoffTicket(c.Request.App().State, webappWindowTicketTTLSeconds, false)
			if err != nil {
				return nil, err
			}
			return JSONResponse(pyjson.ObjectOf("ticket", ticket), 200, "Cache-Control", "no-store")
		},
	})
	r.Post("/api/webapp/socket-ticket", &Endpoint{
		Name:   "issue_socket_ticket",
		Params: []Param{QueryParam("socket", Str, ""), QueryParam("profile", optStr, nil)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			socket, profile := c.Str("socket"), c.Str("profile")
			spec, known := webappSocketTickets[socket]
			if !known {
				return nil, NewHTTPException(400, "Unknown socket")
			}
			if !PolicyOf(c.Request.App().State).field(spec.surface) {
				return nil, NewHTTPException(404, fmt.Sprintf("Webapp %s tickets are unavailable on this surface", socket))
			}
			identity, sessionToken, err := ticketPrincipal(c.Request)
			if err != nil {
				return nil, err
			}
			var profileHome *string
			if spec.checksProfile {
				_, dir, err := cronProfileHome(ctx, profile)
				if err != nil {
					return nil, err
				}
				if socket == gatewaySocket && profile != "" {
					profileHome = &dir
				}
			}
			return socketTicketResponse(c.Request, identity, socket, sessionToken, profileHome)
		},
	})
	r.Post("/webapp/window-session", &Endpoint{
		Name: "redeem_window_ticket",
		Handle: func(ctx context.Context, c *Call) (any, error) {
			req := c.Request
			if err := requirePrivateWebapp(req); err != nil {
				return nil, err
			}
			origins := req.Headers().GetList("origin")
			var origin httpOrigin
			haveOrigin := false
			if len(origins) == 1 {
				origin, haveOrigin = parseHTTPOrigin(origins[0])
			}
			target, haveTarget := parseHTTPOrigin(req.URL().Scheme() + "://" + req.URL().Netloc())
			if !haveOrigin || !haveTarget || origin != target {
				return nil, NewHTTPException(403, "Webapp window handoff requires a same-origin request")
			}
			ticket := req.Headers().Get("X-Hermes-Window-Ticket")
			webappTicketMu.Lock()
			tickets := webappWindowTickets(req.App().State)
			entry, found := tickets[ticket]
			if found && entry.launch && !peerIsServerUser(req, webappSysPlatform()) {
				found = false
			} else if found {
				delete(tickets, ticket)
			}
			webappTicketMu.Unlock()
			token, ok := "", false
			if found && entry.expires > monotonic() {
				gen := entry.generation
				token, ok = BrowserCredentialCurrentIf(&gen)
			}
			if !ok {
				return nil, NewHTTPException(403, "Webapp window link expired or was already used; open a new window")
			}
			return JSONResponse(pyjson.ObjectOf("token", token), 200, "Cache-Control", "no-store")
		},
	})
	r.Get("/webapp/launch", &Endpoint{
		Name: "launch_page",
		Handle: func(ctx context.Context, c *Call) (any, error) {
			if err := requirePrivateWebapp(c.Request); err != nil {
				return nil, err
			}
			return handoffPage(c.Request, "Opening Hermes", "Opening Hermes Webapp…", webappLaunchScript)
		},
	})
	r.Get("/webapp/window", &Endpoint{
		Name: "child_window_page",
		Handle: func(ctx context.Context, c *Call) (any, error) {
			if err := requirePrivateWebapp(c.Request); err != nil {
				return nil, err
			}
			return handoffPage(c.Request, "Opening Hermes", "Authorizing this Hermes window…", webappWindowScript)
		},
	})
	return r
}
