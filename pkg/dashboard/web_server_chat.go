package dashboard

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"math"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Resize escape the terminal client sends, consumed locally and never written to the PTY
var ptyResizeRE = regexp.MustCompile(`^\x1b\[RESIZE:(\d+);(\d+)\]`)

const (
	ptyReadChunkTimeout = 200 * time.Millisecond
	ptyIdleBackoff      = 50 * time.Millisecond
)

// Keep alive PTY sessions of the dashboard terminal sockets
var ptyRegistry = tools.NewPtySessionRegistry(tools.PtySessionRegistryOptions{
	TTL: 30 * time.Minute, MaxSessions: 16, BufferCap: 1 * 1024 * 1024, ReadTimeout: ptyReadChunkTimeout})

// Subscriber sockets of each event channel the chat sidecar publishes to
type eventChannels map[string]map[*WebSocket]struct{}

// Empty channel table the lifespan binds to app state
func newEventChannels() eventChannels { return eventChannels{} }

// Closes only the terminal socket when its child stops accepting input
func closeStalledPtyInput(ctx context.Context, ws *WebSocket, path string) {
	webServerLog().Warn("pty input stalled path=" + path + "; recycling terminal session")
	_ = ws.Close(context.WithoutCancel(ctx), 1013, "PTY input stalled")
}

// Decimal digits as an int, saturating past the int range the way the bridge clamp treats a huge value
func resizeDimension(digits []byte) int {
	n, ok := new(big.Int).SetString(string(digits), 10)
	if !ok || !n.IsInt64() || n.Int64() > math.MaxInt {
		return math.MaxInt
	}
	return int(n.Int64())
}

// Whether a receive error is the RuntimeError of a socket already disconnected
func wsReceiveRuntimeError(err error) bool {
	var rt *RuntimeError
	var gone *WebSocketDisconnected
	return errors.As(err, &rt) || errors.As(err, &gone)
}

// Streams PTY output to the socket until end of file or a failed send, then closes bridge and socket
func pumpPtyToWS(ctx context.Context, ws *WebSocket, bridge tools.TerminalBridge) {
	defer func() {
		_ = bridge.Close()
		_ = ws.Close(context.WithoutCancel(ctx), 1000, "")
	}()
	for {
		if ctx.Err() != nil {
			return
		}
		chunk, err := bridge.Read(ptyReadChunkTimeout)
		if err != nil || chunk == nil {
			return
		}
		if len(chunk) == 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(ptyIdleBackoff):
			}
			continue
		}
		if err := ws.SendBytes(ctx, chunk); err != nil {
			return
		}
	}
}

// One to one socket and PTY pump used without an attach token, closing the bridge when the socket ends
func legacyPump(ctx context.Context, ws *WebSocket, bridge tools.TerminalBridge) error {
	readerCtx, cancelReader := context.WithCancel(ctx)
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		pumpPtyToWS(readerCtx, ws, bridge)
	}()
	err := func() error {
		for {
			msg, err := ws.Receive(ctx)
			if err != nil {
				var disconnect *WebSocketDisconnect
				if wsReceiveRuntimeError(err) || errors.As(err, &disconnect) {
					return nil
				}
				return err
			}
			if msg.Type == "websocket.disconnect" {
				return nil
			}
			raw := msg.Bytes
			if raw == nil {
				raw = []byte{}
				if msg.Text != nil {
					raw = []byte(*msg.Text)
				}
			}
			if len(raw) == 0 {
				continue
			}
			if m := ptyResizeRE.FindSubmatch(raw); m != nil && len(m[0]) == len(raw) {
				bridge.Resize(resizeDimension(m[1]), resizeDimension(m[2]))
				continue
			}
			ok, err := bridge.Write(ctx, raw, tools.PtyWriteTimeout, nil)
			if err != nil {
				return err
			}
			if !ok {
				closeStalledPtyInput(ctx, ws, "legacy")
				return nil
			}
		}
	}()
	cancelReader()
	<-readerDone
	_ = bridge.Close()
	return err
}

// Peer hosts the dashboard boundary treats as loopback
var wsLoopbackHosts = map[string]bool{"127.0.0.1": true, "::1": true, "localhost": true, "testclient": true}

const (
	wsAuthIdentityKey      = "_hermes_auth_identity"
	wsNativeGrantKey       = "_hermes_native_grant"
	wsSubprotocolKey       = "_hermes_ws_subprotocol"
	wsTicketProfileHomeKey = "_hermes_ticket_profile_home"
)

// Server minted identity the upgrade gate stamped on the socket, nil when none
func wsAuthIdentity(ws *WebSocket) map[string]any {
	identity, _ := ws.Scope.Get(wsAuthIdentityKey).(map[string]any)
	return identity
}

// Native grant the audio upgrade redeemed, nil when none
func wsNativeGrant(ws *WebSocket) map[string]any {
	grant, _ := ws.Scope.Get(wsNativeGrantKey).(map[string]any)
	return grant
}

// Public subprotocol the accept selects, nil when the gate chose none
func wsSubprotocol(ws *WebSocket) *string {
	if protocol, ok := ws.Scope.Get(wsSubprotocolKey).(string); ok {
		return &protocol
	}
	return nil
}

// Profile home a redeemed ticket carried, nil when none
func wsTicketProfileHome(ws *WebSocket) any { return ws.Scope.Get(wsTicketProfileHomeKey) }

// Rejection token for the peer address under the dashboard state, empty when allowed
func wsClientReason(ws *WebSocket) string {
	return gateway.WSClientReason(&ws.HTTPConnection, listenerStateOf(ws.App().State))
}

// Whether the peer address passes the client gate
func wsClientIsAllowed(ws *WebSocket) bool { return wsClientReason(ws) == "" }

// Host or origin mismatch token of the upgrade, empty when allowed
func wsHostOriginReason(ws *WebSocket) string {
	return gateway.WSHostOriginReason(&ws.HTTPConnection, listenerStateOf(ws.App().State))
}

// Whether the upgrade passes the dashboard Host and Origin guard
func wsHostOriginIsAllowed(ws *WebSocket) bool { return wsHostOriginReason(ws) == "" }

// Whether the WebSocket upgrade matches the dashboard boundaries
func wsRequestIsAllowed(ws *WebSocket) bool {
	return wsHostOriginIsAllowed(ws) && wsClientIsAllowed(ws)
}

// Request stand in over a socket carrying the app, its headers, peer host and path for the auth helpers
func wsRequestView(ws *WebSocket, app *FastAPI) (*HTTPConnection, error) {
	var headers []Header
	for _, item := range ws.Headers().Items() {
		name, err := latin1Encode(strings.ToLower(item[0]))
		if err != nil {
			return nil, err
		}
		value, err := latin1Encode(item[1])
		if err != nil {
			return nil, err
		}
		headers = append(headers, Header{name, value})
	}
	var client *Address
	if c := ws.Client(); c != nil {
		client = &Address{Host: c.Host, Port: 0}
	}
	scope := &Scope{Type: "http", App: app, Headers: headers, Client: client, Scheme: "ws", Method: "GET",
		Path: ws.URL().Path(), QueryString: "", RootPath: ""}
	return &HTTPConnection{Scope: scope}, nil
}

// Stamps the identity fields of a credential record on the socket
func stampWSIdentity(ws *WebSocket, info *pyjson.Object) {
	userID, _ := info.Get("user_id")
	provider, _ := info.Get("provider")
	identity := map[string]any{"user_id": userID, "provider": provider}
	if issuer, _ := info.Get("issuer"); pyTruthy(issuer) {
		identity["issuer"] = issuer
	}
	ws.Scope.Set(wsAuthIdentityKey, identity)
}

// Upgrade auth verdict as a rejection token, empty when accepted, and the credential kind presented
func wsAuthReason(ctx context.Context, ws *WebSocket, allowInternal bool, socket *string) (string, string, error) {
	app := ws.App()
	authRequired := pyTruthy(app.State.Get("auth_required"))
	webapp := app.State.Get("ui_surface") == "webapp"
	if socket != nil && ownTicketSockets[*socket] {
		reject := ignoreRejection
		if authRequired {
			reject = auditWSRejection(ctx, ws)
		}
		if reason, cred, judged := consumeWSTicket(ctx, ws, reject, socket, authRequired); judged {
			return reason, cred, nil
		}
		return "no_credential", "none", nil
	}
	if ws.Headers().Contains(nativeHTTPHeader) {
		grant := authenticateNativeWS(ws)
		if grant == nil {
			return "native_invalid", "native", nil
		}
		ws.Scope.Set(wsAuthIdentityKey, map[string]any{"user_id": grant["subject"], "provider": "local"})
		return "", "native", nil
	}
	grant, err := redeemAudioWS(ws)
	if err != nil {
		return "", "", err
	}
	if grant != nil {
		ws.Scope.Set(wsAuthIdentityKey, map[string]any{"user_id": grant["subject"], "provider": "local"})
		ws.Scope.Set(wsNativeGrantKey, grant)
		ws.Scope.Set(wsSubprotocolKey, gateway.CanonicalGatewayProtocol)
		return "", "native", nil
	}
	if authRequired {
		reject := auditWSRejection(ctx, ws)
		if internal := ws.QueryParams().Get("internal"); internal != "" {
			if webapp && !allowInternal {
				reject("internal: endpoint not allowed")
				return "internal_not_allowed", "internal", nil
			}
			info, err := ConsumeInternalWSCredential(internal)
			var invalid *TicketInvalid
			if errors.As(err, &invalid) {
				reject("internal: " + invalid.Error())
				return "internal_invalid", "internal", nil
			}
			if err != nil {
				return "", "", err
			}
			stampWSIdentity(ws, info)
			return "", "internal", nil
		}
		if reason, cred, judged := consumeWSTicket(ctx, ws, reject, socket, true); judged {
			return reason, cred, nil
		}
		if token := ws.QueryParams().Get("token"); token != "" {
			view, err := wsRequestView(ws, app)
			if err != nil {
				return "", "", err
			}
			var session *gateway.DashboardSession
			if webappTokenWorksAlone(view, token) {
				session, err = verifyAccessToken(ctx, view, token, "", false)
			}
			var unavailable *gateway.ProviderError
			if errors.As(err, &unavailable) {
				reject("session token verify unavailable: " + unavailable.Error())
				return "token_unavailable", "token", nil
			}
			if err != nil {
				return "", "", err
			}
			ip := ""
			if c := ws.Client(); c != nil {
				ip = c.Host
			}
			if session != nil {
				ws.Scope.Set(wsAuthIdentityKey, map[string]any{"user_id": session.UserID, "provider": session.Provider})
				auditLog(ctx, AuditTokenAuthSuccess, "provider", session.Provider, "user_id", session.UserID, "ip", ip,
					"path", ws.URL().Path())
				return "", "token", nil
			}
			auditLog(ctx, AuditTokenAuthFailure, "reason", "session_token_invalid", "ip", ip, "path", ws.URL().Path())
			return "token_invalid", "token", nil
		}
		return "no_credential", "none", nil
	}
	if webapp {
		if reason, cred, judged := consumeWSTicket(ctx, ws, ignoreRejection, socket, false); judged {
			return reason, cred, nil
		}
	}
	token := ws.QueryParams().Get("token")
	if token == "" {
		return "no_credential", "none", nil
	}
	if subtle.ConstantTimeCompare([]byte(token), []byte(SessionToken())) == 1 {
		trackBrowserSocket(ctx, ws, BrowserCredentialGenerationOf(token))
		return "", "token", nil
	}
	return "token_mismatch", "token", nil
}

// The loopback gate keeps no audit record of a refused credential
func ignoreRejection(string) {}

// Audit record of a refused upgrade credential on the gated listener
func auditWSRejection(ctx context.Context, ws *WebSocket) func(string) {
	return func(reason string) {
		ip := ""
		if c := ws.Client(); c != nil {
			ip = c.Host
		}
		auditLog(ctx, AuditWSTicketRejected, "reason", reason, "ip", ip, "path", ws.URL().Path())
	}
}

// Verdict on a one use ticket from the subprotocol or query, false when the upgrade presents none
func consumeWSTicket(ctx context.Context, ws *WebSocket, reject func(string), socket *string, authRequired bool) (string, string, bool) {
	protocolTicket, protocolReason := gateway.GatewayWSTicketFromSubprotocol(&ws.HTTPConnection)
	if protocolReason == "invalid" {
		return "ticket_invalid", "ticket-subprotocol", true
	}
	ticket := protocolTicket
	if ticket == "" {
		ticket = ws.QueryParams().Get("ticket")
	}
	if ticket == "" {
		return "", "", false
	}
	info, err := ConsumeWSTicket(ticket)
	if err != nil {
		reject(err.Error())
		return "ticket_invalid", "ticket", true
	}
	if refusal, refused := ticketRefusal(info, socket, authRequired); refused {
		reject(refusal)
		return "ticket_invalid", "ticket", true
	}
	if provider, _ := info.Get("provider"); provider == "bot-desktop" {
		reject("display ticket presented as a gateway login")
		return "ticket_invalid", "ticket", true
	}
	stampWSIdentity(ws, info)
	if profileHome, _ := info.Get("profile_home"); pyTruthy(profileHome) {
		ws.Scope.Set(wsTicketProfileHomeKey, profileHome)
	}
	if admitted, ok := admittedWith(info); ok {
		trackBrowserSocket(ctx, ws, admitted)
	}
	if protocolTicket == "" {
		return "", "ticket", true
	}
	ws.Scope.Set(wsSubprotocolKey, gateway.CanonicalGatewayProtocol)
	return "", "ticket-subprotocol", true
}

// Whether the upgrade credential is accepted
func wsAuthOK(ctx context.Context, ws *WebSocket, allowInternal bool, socket *string) (bool, error) {
	reason, _, err := wsAuthReason(ctx, ws, allowInternal, socket)
	return reason == "" && err == nil, err
}

// Label of the active socket auth mode, gated, insecure or loopback
func wsAuthMode(app *FastAPI) string {
	if pyTruthy(app.State.Get("auth_required")) {
		return "gated"
	}
	boundHost := strings.ToLower(pystr.Strip(app.State.String("bound_host")))
	if boundHost != "" && !wsLoopbackHosts[boundHost] {
		return "insecure"
	}
	return "loopback"
}

// Close reason clamped to the 123 byte limit of RFC 6455, truncated with an ellipsis past it
func wsCloseReason(text string) string {
	encoded := strings.ToValidUTF8(text, "?")
	if len(encoded) <= 123 {
		return text
	}
	return strings.ToValidUTF8(encoded[:120], "") + "..."
}

// Pre accept gates of the console, PTY and host terminal sockets, closing with the failed gate code
func wsGate(ctx context.Context, ws *WebSocket, kind string) (string, string, string, bool, error) {
	peer := "?"
	if c := ws.Client(); c != nil {
		peer = c.Host
	}
	if !dashboardEmbeddedChatEnabled {
		webServerLog().Info(fmt.Sprintf("%s refused: embedded chat disabled peer=%s", kind, peer))
		return "", "", "", false, ws.Close(ctx, 4404, "embedded chat disabled")
	}
	authReason, cred, err := wsAuthReason(ctx, ws, false, &kind)
	if err != nil {
		return "", "", "", false, err
	}
	mode := wsAuthMode(ws.App())
	if authReason != "" {
		webServerLog().Warn(fmt.Sprintf("%s auth rejected reason=%s mode=%s cred=%s peer=%s", kind, authReason, mode, cred, peer))
		return "", "", "", false, ws.Close(ctx, 4401, wsCloseReason("auth: "+authReason))
	}
	if reason := wsHostOriginReason(ws); reason != "" {
		webServerLog().Warn(fmt.Sprintf("%s refused: %s peer=%s", kind, reason, peer))
		return "", "", "", false, ws.Close(ctx, 4403, wsCloseReason(reason))
	}
	if reason := wsClientReason(ws); reason != "" {
		webServerLog().Warn(fmt.Sprintf("%s refused: %s", kind, reason))
		return "", "", "", false, ws.Close(ctx, 4408, wsCloseReason(reason))
	}
	return peer, mode, cred, true, nil
}

// Tells the user why chat could not start, then closes 1011 so the SPA offers a new session
func ptyFail(ctx context.Context, ws *WebSocket, exc error, surface string) error {
	webServerLog().Warn(fmt.Sprintf("pty start failed: %s: %s", pyTypeNameOf(exc), exc.Error()))
	if err := ws.SendText(ctx, "\r\n\x1b[31m"+chatStartFailureMessage(exc, surface)+"\x1b[0m\r\n"); err != nil {
		return err
	}
	return ws.Close(ctx, 1011, "")
}

// TUI launcher of pkg/app the chat PTY runs, assigned at startup
var TUILaunch struct {
	MakeArgv       func(ctx context.Context, tuiDir string, dev bool) ([]string, string, error)
	ApplyPythonEnv func(env map[string]string) error
}

// Sets a key of a child env only when it is absent
func envSetDefault(env map[string]string, key, value string) {
	if _, ok := env[key]; !ok {
		env[key] = value
	}
}

// Argv, cwd and env of the chat PTY, the TUI scoped to a profile, workspace, resumed leaf and attach URLs
func resolveChatArgv(ctx context.Context, app *FastAPI, resume, sidecarURL, profile, activeSessionFile, workspaceCwd string) ([]string, string, map[string]string, error) {
	profileDir := ""
	requested := pystr.Strip(profile)
	if requested != "" && strings.ToLower(requested) != "current" {
		dir, err := resolveProfileDir(requested)
		if err != nil {
			return nil, "", nil, err
		}
		profileDir = dir
	}
	argv, cwd, err := TUILaunch.MakeArgv(ctx, filepath.Join(ProjectRoot(), "ui-tui"), false)
	if err != nil {
		return nil, "", nil, err
	}
	env, err := tools.BuildSubprocessEnv(ctx, nil, tools.SubprocessEnvOptions{ScrubSecrets: false, InheritProfileHome: true})
	if err != nil {
		return nil, "", nil, err
	}
	if profileDir != "" {
		env["ICEBATH_HOME"] = profileDir
	}
	if err := applyChatTerminalConfig(ctx, env, profileDir, requested); err != nil {
		webServerLog().Warn("Failed to apply terminal config bridge for dashboard chat", "exc_info", err)
	}
	if workspaceCwd != "" {
		env["ICEBATH_CWD"] = workspaceCwd
		env["ICEBATH_TUI_CWD"] = workspaceCwd
	}
	if err := TUILaunch.ApplyPythonEnv(env); err != nil {
		return nil, "", nil, err
	}
	envSetDefault(env, "NODE_ENV", "production")
	envSetDefault(env, "ICEBATH_TUI_DISABLE_MOUSE", "1")
	envSetDefault(env, "ICEBATH_TUI_INLINE", "1")
	envSetDefault(env, "COLORTERM", "truecolor")
	env["ICEBATH_TUI_DASHBOARD"] = "1"
	if resume != "" {
		var scoped *string
		if profileDir != "" {
			scoped = &requested
		}
		db, err := openSessionDBForProfile(ctx, scoped, true)
		if err != nil {
			return nil, "", nil, err
		}
		latest, _, err := sessionLatestDescendant(ctx, resume, db)
		db.Close()
		if err != nil {
			return nil, "", nil, err
		}
		if latest != "" {
			resume = latest
		}
		env["ICEBATH_TUI_RESUME"] = resume
	}
	if sidecarURL != "" {
		env["ICEBATH_TUI_SIDECAR_URL"] = sidecarURL
	}
	if activeSessionFile != "" {
		env["ICEBATH_TUI_ACTIVE_SESSION_FILE"] = activeSessionFile
	}
	if profileDir == "" {
		if gatewayWSURL, ok := buildGatewayWSURL(app); ok {
			env["ICEBATH_TUI_GATEWAY_URL"] = gatewayWSURL
		}
	}
	return append([]string(nil), argv...), cwd, env, nil
}

// Bridges terminal config into the chat env, a selected profile's keys replacing those the launch profile owns
func applyChatTerminalConfig(ctx context.Context, env map[string]string, profileDir, requested string) error {
	if profileDir == "" {
		return config.ApplyTerminalConfigToEnv(ctx, home.Dir(ctx), config.TerminalEnvOptions{Env: env})
	}
	raw, err := config.ReadRawConfig(ctx, home.Dir(ctx))
	if err != nil {
		return err
	}
	for envVar := range config.TerminalConfigOwnedEnvVars(raw["terminal"]) {
		delete(env, envVar)
	}
	scoped, _, err := configProfileScope(ctx, requested)
	if err != nil {
		return err
	}
	return config.ApplyTerminalConfigToEnv(scoped, home.Dir(scoped), config.TerminalEnvOptions{Env: env})
}

// Wildcard bind hosts an in container client must not dial
var wildcardHosts = map[string]bool{"0.0.0.0": true, "::": true}

// Host the in container socket client dials, the explicit override first and a wildcard bind as loopback
func resolveClientWSHost(app *FastAPI) (string, bool) {
	if explicit := pystr.Strip(os.Getenv("ICEBATH_DASHBOARD_WS_HOST")); explicit != "" {
		return explicit, true
	}
	host := app.State.Get("bound_host")
	if !pyTruthy(host) {
		return "", false
	}
	text := pyrepr.Str(host)
	if wildcardHosts[text] {
		return "127.0.0.1", true
	}
	return text, true
}

// Socket URL for server spawned clients carrying the internal credential or session token, false when unbound
func serverInternalWSURL(app *FastAPI, path string, extra ...[2]string) (string, bool) {
	host, ok := resolveClientWSHost(app)
	port := app.State.Get("bound_port")
	if !ok || host == "" || !pyTruthy(port) {
		return "", false
	}
	netloc := fmt.Sprintf("%s:%s", host, pyrepr.Str(port))
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		netloc = fmt.Sprintf("[%s]:%s", host, pyrepr.Str(port))
	}
	auth := [2]string{"token", SessionToken()}
	if pyTruthy(app.State.Get("auth_required")) {
		auth = [2]string{"internal", InternalWSCredential()}
	}
	pairs := append([][2]string{auth}, extra...)
	parts := make([]string, len(pairs))
	for i, pair := range pairs {
		parts[i] = url.QueryEscape(pair[0]) + "=" + url.QueryEscape(pair[1])
	}
	return "ws://" + netloc + path + "?" + strings.Join(parts, "&"), true
}

// Socket URL the PTY child attaches to for gateway JSON-RPC traffic
func buildGatewayWSURL(app *FastAPI) (string, bool) { return serverInternalWSURL(app, "/api/ws") }

// Socket URL the PTY child publishes events to on its channel
func buildSidecarURL(app *FastAPI, channel string) (string, bool) {
	return serverInternalWSURL(app, "/api/pub", [2]string{"channel", channel})
}

// Chat argv resolved one build at a time under the app's chat argv lock
func resolveChatArgvAsync(ctx context.Context, app *FastAPI, resume, sidecarURL, profile, activeSessionFile, workspaceCwd string) ([]string, string, map[string]string, error) {
	lock := getChatArgvLock(app)
	lock.Lock()
	defer lock.Unlock()
	return resolveChatArgv(ctx, app, resume, sidecarURL, profile, activeSessionFile, workspaceCwd)
}

// Per channel file a dashboard TUI writes its active session id to, created empty on first use
func activeSessionFileForChannel(app *FastAPI, channel string) (string, error) {
	files := getPtyActiveSessionFiles(app)
	if path, ok := files.Get(channel); ok && path != "" {
		return path, nil
	}
	f, err := os.CreateTemp("", "icebath-pty-active-*.json")
	if err != nil {
		return "", err
	}
	path := f.Name()
	if err := f.Close(); err != nil {
		return "", err
	}
	files.Set(channel, path)
	path, _ = files.Get(channel)
	return path, nil
}

const consoleExecutorMaxWorkers = 4

// Pending console command whose worker may still be queued, running or finished
type consoleFuture struct {
	mu        sync.Mutex
	fn        func() (any, error)
	state     int
	result    any
	err       error
	done      chan struct{}
	cancelled bool
}

const (
	consolePending = iota
	consoleRunning
	consoleFinished
)

// Cancels a command still queued, false once a worker picked it up
func (f *consoleFuture) Cancel() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cancelled {
		return true
	}
	if f.state != consolePending {
		return false
	}
	f.cancelled = true
	f.state = consoleFinished
	close(f.done)
	return true
}

// Closed once the command finished or was cancelled
func (f *consoleFuture) Done() <-chan struct{} { return f.done }

// Outcome of a finished command, a cancellation error for one cancelled while queued
func (f *consoleFuture) Result() (any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cancelled {
		return nil, context.Canceled
	}
	return f.result, f.err
}

// Runs the command unless it was cancelled while queued
func (f *consoleFuture) run() {
	f.mu.Lock()
	if f.cancelled {
		f.mu.Unlock()
		return
	}
	f.state = consoleRunning
	f.mu.Unlock()
	result, err := f.fn()
	f.mu.Lock()
	f.result, f.err, f.state = result, err, consoleFinished
	f.mu.Unlock()
	close(f.done)
}

// Bounded worker pool console commands run on, queued in submission order
type consoleExecutor struct {
	mu      sync.Mutex
	queue   []*consoleFuture
	workers int
}

// Queues a command, starting a worker while fewer than the bound run
func (e *consoleExecutor) Submit(fn func() (any, error)) *consoleFuture {
	f := &consoleFuture{fn: fn, done: make(chan struct{})}
	e.mu.Lock()
	e.queue = append(e.queue, f)
	if e.workers < consoleExecutorMaxWorkers {
		e.workers++
		go e.work()
	}
	e.mu.Unlock()
	return f
}

// Drains queued commands until none is left
func (e *consoleExecutor) work() {
	for {
		e.mu.Lock()
		if len(e.queue) == 0 {
			e.workers--
			e.mu.Unlock()
			return
		}
		f := e.queue[0]
		e.queue = e.queue[1:]
		e.mu.Unlock()
		f.run()
	}
}

var (
	consoleExecutorOnce sync.Once
	consoleExecutorPool *consoleExecutor
)

// Bounded console worker pool, created once per process
func getConsoleExecutor() *consoleExecutor {
	consoleExecutorOnce.Do(func() { consoleExecutorPool = &consoleExecutor{} })
	return consoleExecutorPool
}
