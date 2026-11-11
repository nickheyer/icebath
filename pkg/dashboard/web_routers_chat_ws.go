package dashboard

import (
	"context"
	"errors"
	"fmt"
	"net"
	"regexp"
	"sync"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/mcp"
	"github.com/nickheyer/icebath/pkg/tuiserver"
)

// Console engine of the CLI layer above this package, assigned by the command wiring
var ConsoleCLI struct {
	NewEngine func(ctx context.Context, outputLimit int) func(ctx context.Context, line string, confirmed bool) (status, output, command, confirmation string, err error)
}

// Subscriber table and its lock from app state, created on first use when no lifespan ran
func getEventState(app *FastAPI) (eventChannels, *sync.Mutex) {
	channels := appStateDefault(app, "event_channels", func() any { return newEventChannels() }).(eventChannels)
	lock := appStateDefault(app, "event_lock", func() any { return &sync.Mutex{} }).(*sync.Mutex)
	return channels, lock
}

var validChannelRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}\n?$`)

// Removes an active session marker file, a missing file and any OS refusal ignored
func unlinkActiveSessionFile(path string) {
	if path != "" {
		_ = pypath.Unlink(path)
	}
}

// Removes a marker file and forgets it for its channel when the channel still maps to it
func discardActiveSessionFile(app *FastAPI, channel, path string) {
	unlinkActiveSessionFile(path)
	if channel == "" || path == "" {
		return
	}
	files := getPtyActiveSessionFiles(app)
	if current, ok := files.Get(channel); ok && current == path {
		files.Pop(channel)
	}
}

// Sends one publisher frame to every subscriber of a channel, a failed send logged and skipped
func broadcastEvent(ctx context.Context, app *FastAPI, channel, payload string) {
	channels, lock := getEventState(app)
	lock.Lock()
	subs := make([]eventSubscriber, 0, len(channels[channel]))
	for sub := range channels[channel] {
		subs = append(subs, sub)
	}
	lock.Unlock()
	for _, sub := range subs {
		if err := sub.SendText(ctx, payload); err != nil {
			routersLog().Warn(fmt.Sprintf("broadcast send failed for subscriber on %s", channel), "exc_info", err)
		}
	}
}

// Channel id of the query string, empty when it is absent or malformed
func channelOf(ws *WebSocket) string {
	channel := ws.QueryParams().Get("channel")
	if validChannelRE.MatchString(channel) {
		return channel
	}
	return ""
}

// Pre accept gates of the sidecar sockets, closing 4403 for a disabled chat or refused request, 4401 for bad auth
func closeUnlessSidecarAllowed(ctx context.Context, ws *WebSocket, allowInternal bool, socket *string) (bool, error) {
	if !dashboardEmbeddedChatEnabled {
		return false, ws.Close(ctx, 4403, "")
	}
	ok, err := wsAuthOK(ctx, ws, allowInternal, socket)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, ws.Close(ctx, 4401, "")
	}
	if !wsRequestIsAllowed(ws) {
		return false, ws.Close(ctx, 4403, "")
	}
	return true, nil
}

// Authority serving a ticket home, the given one for its own home and the served sibling otherwise
func authorityForTicketHome(authority *gateway.SessionAuthority, h string) (*gateway.SessionAuthority, error) {
	own, err := pypath.Resolve(authority.ProfileID)
	if err != nil {
		return nil, err
	}
	wanted, err := pypath.Resolve(h)
	if err != nil {
		return nil, err
	}
	if own == wanted {
		return authority, nil
	}
	return gateway.AuthorityForHome(authority.Runner, h)
}

// Dashboard socket seen through the surface the JSON RPC session handler serves
type servedSocket struct {
	ctx context.Context
	ws  *WebSocket
}

func (s *servedSocket) SendText(ctx context.Context, text string) error {
	return s.ws.SendText(ctx, text)
}

func (s *servedSocket) Close(code int) error {
	return s.ws.Close(context.WithoutCancel(s.ctx), code, "")
}

func (s *servedSocket) Accept(ctx context.Context, subprotocol *string) error {
	if subprotocol != nil && *subprotocol == "" {
		subprotocol = nil
	}
	return s.ws.Accept(ctx, subprotocol, nil)
}

// Next text frame, the peer close reported as the disconnect the session handler ends on
func (s *servedSocket) ReceiveText(ctx context.Context) (string, error) {
	text, err := s.ws.ReceiveText(ctx)
	var disconnect *WebSocketDisconnect
	if errors.As(err, &disconnect) {
		return "", &tuiserver.SocketDisconnect{Code: int64(disconnect.Code), Reason: disconnect.Reason}
	}
	return text, err
}

func (s *servedSocket) Client() *tuiserver.SocketPeer {
	client := s.ws.Client()
	if client == nil {
		return nil
	}
	port := client.Port
	return &tuiserver.SocketPeer{Host: client.Host, Port: &port}
}

// Stream socket a server published as the transport extension of the scope, nil when it published none
func (s *servedSocket) RawConn() net.Conn {
	conn, _ := s.ws.Scope.Extensions["transport"].(net.Conn)
	return conn
}

func (s *servedSocket) SessionAuthority() *gateway.SessionAuthority {
	if scoped, ok := s.ws.Scope.Get("hermes.session_authority").(*gateway.SessionAuthority); ok && scoped != nil {
		return scoped
	}
	authority, _ := s.ws.App().State.Get("session_authority").(*gateway.SessionAuthority)
	return authority
}

// Copy of an identity with extra claims set
func identityWith(identity map[string]any, kv ...any) map[string]any {
	out := make(map[string]any, len(identity)+len(kv)/2)
	for k, v := range identity {
		out[k] = v
	}
	for i := 0; i+1 < len(kv); i += 2 {
		out[kv[i].(string)] = kv[i+1]
	}
	return out
}

// JSON RPC sidecar of the Chat tab, served in the scope of the profile its ticket or the backend names
func gatewayWS(ctx context.Context, ws *WebSocket) error {
	socket := gatewaySocket
	if ok, err := closeUnlessSidecarAllowed(ctx, ws, true, &socket); err != nil || !ok {
		return err
	}
	mcp.StartDeferredDiscoveryNow()
	identity := wsAuthIdentity(ws)
	authority, _ := ws.App().State.Get("session_authority").(*gateway.SessionAuthority)
	if authority != nil && identity == nil {
		identity = map[string]any{"user_id": "legacy-token-owner", "provider": "session-token",
			"profile_id": authority.ProfileID, "instance_id": authority.InstanceID,
			"capabilities": []any{"session:create", "session:read", "session:submit", "session:control", "session:approve", "session:respond"}}
	}
	if identity != nil {
		identity = identityWith(identity, "profile_scope", "host")
	}
	ticketHome := wsTicketProfileHome(ws)
	served := authority
	if pyrepr.Truthy(ticketHome) && authority == nil {
		return ws.Close(ctx, 1013, "")
	}
	if pyrepr.Truthy(ticketHome) && identity != nil {
		h, ok := ticketHome.(string)
		if !ok {
			return fmt.Errorf("TypeError: expected str, bytes or os.PathLike object, not %s", pyrepr.TypeName(ticketHome))
		}
		var err error
		if served, err = authorityForTicketHome(authority, h); err != nil {
			return err
		}
		if served == nil {
			return ws.Close(ctx, 4403, "")
		}
		ws.Scope.Set("hermes.session_authority", served)
		identity = identityWith(identity, "profile_id", served.ProfileID, "instance_id", served.InstanceID)
	}
	if served != nil {
		ctx = gateway.OwnerScope(ctx, served, false)
	}
	return tuiserver.HandleWS(ctx, &servedSocket{ctx: ctx, ws: ws}, identity, wsSubprotocol(ws), true)
}

// Gates and accepts a channel socket, closing 4400 for a missing or malformed channel
func acceptChannelWS(ctx context.Context, ws *WebSocket, allowInternal bool) (string, error) {
	if ok, err := closeUnlessSidecarAllowed(ctx, ws, allowInternal, nil); err != nil || !ok {
		return "", err
	}
	channel := channelOf(ws)
	if channel == "" {
		return "", ws.Close(ctx, 4400, "")
	}
	return channel, ws.Accept(ctx, nil, nil)
}

// Publisher socket of a PTY child, every frame fanned out to the subscribers of its channel
func pubWS(ctx context.Context, ws *WebSocket) error {
	channel, err := acceptChannelWS(ctx, ws, true)
	if err != nil || channel == "" {
		return err
	}
	for {
		text, err := ws.ReceiveText(ctx)
		if err != nil {
			var disconnect *WebSocketDisconnect
			if errors.As(err, &disconnect) {
				return nil
			}
			return err
		}
		broadcastEvent(ctx, ws.App(), channel, text)
	}
}

// Subscriber socket of a channel, registered until its peer leaves
func eventsWS(ctx context.Context, ws *WebSocket) error {
	channel, err := acceptChannelWS(ctx, ws, false)
	if err != nil || channel == "" {
		return err
	}
	channels, lock := getEventState(ws.App())
	lock.Lock()
	if channels[channel] == nil {
		channels[channel] = map[eventSubscriber]struct{}{}
	}
	channels[channel][ws] = struct{}{}
	lock.Unlock()
	defer func() {
		lock.Lock()
		defer lock.Unlock()
		if subs, ok := channels[channel]; ok {
			delete(subs, ws)
			if len(subs) == 0 {
				delete(channels, channel)
			}
		}
	}()
	for {
		if _, err := ws.ReceiveText(ctx); err != nil {
			var disconnect *WebSocketDisconnect
			if errors.As(err, &disconnect) {
				return nil
			}
			return err
		}
	}
}

// Router of the console, chat PTY, JSON RPC sidecar and event channel sockets
func chatWsRouter() *APIRouter {
	r := NewAPIRouter("")
	r.WebSocket("/api/console", &WSEndpoint{Name: "console_ws", Handle: func(ctx context.Context, c *Call) error { return consoleWS(ctx, c.WebSocket) }})
	r.WebSocket("/api/pty", &WSEndpoint{Name: "pty_ws", Handle: func(ctx context.Context, c *Call) error { return ptyWS(ctx, c.WebSocket) }})
	r.WebSocket("/api/ws", &WSEndpoint{Name: "gateway_ws", Handle: func(ctx context.Context, c *Call) error { return gatewayWS(ctx, c.WebSocket) }})
	r.WebSocket("/api/pub", &WSEndpoint{Name: "pub_ws", Handle: func(ctx context.Context, c *Call) error { return pubWS(ctx, c.WebSocket) }})
	r.WebSocket("/api/events", &WSEndpoint{Name: "events_ws", Handle: func(ctx context.Context, c *Call) error { return eventsWS(ctx, c.WebSocket) }})
	return r
}
