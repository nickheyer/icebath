package gatewayrun

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/nickheyer/icebath/pkg/gateway"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	errorutil "github.com/nickheyer/icebath/pkg/utils/error"
)

// Path of the native session socket on the gateway listener
const sessionSocketPath = "/api/ws"

// Time open session sockets get to end once the listener stops
const listenerGrace = 5 * time.Second

// Loopback listener of the gateway with the native session socket as its only route
type sessionListener struct {
	runner   *GatewayRunner
	state    *gateway.ListenerState
	server   *http.Server
	serveCtx context.Context
	cancel   context.CancelFunc
	exit     chan struct{}
	exitOnce sync.Once
	mu       sync.Mutex
	stopping bool
	sockets  map[*sessionSocket]struct{}
	handlers sync.WaitGroup
}

// Binds the loopback listener on a free port and serves the native session socket on it
func startGatewayAPI(ctx context.Context, r *GatewayRunner) (*gatewayAPIHandle, error) {
	if gateway.ActiveAPIRunner() != nil {
		return nil, errors.New("gateway API already started")
	}
	socket, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	port := socket.Addr().(*net.TCPAddr).Port
	serveCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	l := &sessionListener{runner: r, serveCtx: serveCtx, cancel: cancel, exit: make(chan struct{}), sockets: map[*sessionSocket]struct{}{},
		state: &gateway.ListenerState{BoundHost: "127.0.0.1"}}
	l.server = &http.Server{Handler: l}
	gateway.SetActiveAPIRunner(r)
	handle := &gatewayAPIHandle{apiOrigin: fmt.Sprintf("http://127.0.0.1:%d", port), listener: l, task: make(chan struct{}), socket: socket}
	go func() {
		defer close(handle.task)
		handle.serveErr = l.serve(socket)
		gateway.SetActiveAPIRunner(nil)
	}()
	return handle, nil
}

// Asks the serve loop to stop accepting and end its sockets
func (l *sessionListener) requestExit() {
	l.exitOnce.Do(func() { close(l.exit) })
}

// Serves until an exit is asked or the server fails, then ends the open sockets within the grace
func (l *sessionListener) serve(socket net.Listener) error {
	failed := make(chan error, 1)
	go func() { failed <- l.server.Serve(socket) }()
	var failure error
	select {
	case <-l.exit:
	case failure = <-failed:
	}
	socket.Close()
	l.mu.Lock()
	l.stopping = true
	open := make([]*sessionSocket, 0, len(l.sockets))
	for s := range l.sockets {
		open = append(open, s)
	}
	l.mu.Unlock()
	for _, s := range open {
		s.goingAway()
	}
	ended := make(chan struct{})
	go func() {
		l.handlers.Wait()
		close(ended)
	}()
	select {
	case <-ended:
	case <-time.After(listenerGrace):
		pkgLog().Error(fmt.Sprintf("Cancel %d running session socket(s), timeout graceful shutdown exceeded", len(open)))
		l.cancel()
		for _, s := range open {
			s.drop()
		}
	}
	l.cancel()
	l.server.Close()
	if errors.Is(failure, http.ErrServerClosed) {
		return nil
	}
	return failure
}

// Counts a session handler in unless the listener is stopping
func (l *sessionListener) enter() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.stopping {
		return false
	}
	l.handlers.Add(1)
	return true
}

// Records an accepted socket for the stop, false once the listener is stopping
func (l *sessionListener) track(s *sessionSocket) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.stopping {
		return false
	}
	l.sockets[s] = struct{}{}
	return true
}

func (l *sessionListener) untrack(s *sessionSocket) {
	l.mu.Lock()
	delete(l.sockets, s)
	l.mu.Unlock()
}

// Answers the session socket upgrade and 404 for every other request
func (l *sessionListener) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.URL.Path != sessionSocketPath || !websocket.IsWebSocketUpgrade(req) {
		http.NotFound(w, req)
		return
	}
	sock := &sessionSocket{listener: l, w: w, req: req, frames: make(chan socketFrame, 16), done: make(chan struct{})}
	if !l.enter() {
		sock.refuse()
		return
	}
	defer l.handlers.Done()
	err := l.admitSession(sock)
	if err != nil && !errors.Is(err, context.Canceled) && !sock.wasAccepted() {
		how := "with HTTP 500"
		if sock.wasAnswered() {
			how = "after its handler had refused it"
		}
		detail := err.Error()
		var kinded interface{ ErrorKind() string }
		if errors.As(err, &kinded) && kinded.ErrorKind() != "" {
			detail = kinded.ErrorKind() + ": " + detail
		}
		pkgLog().Error(fmt.Sprintf("Gateway refused a session WebSocket upgrade from %s %s: %s",
			sock.peerText(), how, detail), "exc_info", err)
	}
	sock.finish(err)
}

// Whether err is the PermissionError a ticket redemption raises
func ticketRefused(err error) bool {
	var raised *errorutil.Error
	return errors.As(err, &raised) && raised.Kind == "PermissionError"
}

// Admits a ticketed loopback socket against the runtime state and runs its session, refusing every other upgrade
func (l *sessionListener) admitSession(sock *sessionSocket) error {
	runner := l.runner
	descriptor := runner.SessionRuntimeDescriptor()
	if descriptor == nil {
		sock.refuse()
		return nil
	}
	var stateMu sync.Mutex
	lastState := descriptor["state"]
	notReady := func() bool {
		stateMu.Lock()
		if live := runner.SessionRuntimeDescriptor(); live != nil {
			lastState = live["state"]
		}
		state := lastState
		stateMu.Unlock()
		return state != "ready" || runner.Draining()
	}
	state := descriptor["state"]
	draining := (state == "ready" || state == "draining") && (runner.Draining() || state == "draining")
	if (state != "ready" || runner.Draining()) && !draining {
		sock.refuse()
		return nil
	}
	ticket, reason := gateway.GatewayWSTicketFromSubprotocol(sock)
	peer, _ := sock.BoundaryClient()
	if reason != "ok" || sock.req.Header.Get("Origin") != "" || gateway.WSHostReason(sock, l.state) != "" ||
		gateway.WSClientReason(sock, l.state) != "" || (peer != "127.0.0.1" && peer != "::1") {
		sock.refuse()
		return nil
	}
	operator := true
	store := runner.SessionTicketStore()
	grant, err := store.Redeem(ticket, nil, "interactive")
	if ticketRefused(err) {
		operator = false
		if grant, err = store.Redeem(ticket, nil, "worker-adoption"); ticketRefused(err) {
			sock.refuse()
			return nil
		}
	}
	if err != nil {
		return err
	}
	allowDraining := !operator
	admittedInWait := gateway.RestartWaitOpen(runner) && !anyutil.IsTrue(grant["refuse_restart_wait"])
	if notReady() && !allowDraining && !admittedInWait {
		sock.refuse()
		return nil
	}
	sock.shut = func() bool {
		return notReady() && !allowDraining && !(admittedInWait && gateway.RestartWaitOpen(runner))
	}
	profileID, _ := grant["profile_id"].(string)
	authority, err := gateway.AuthorityForProfileID(runner, profileID)
	if err != nil {
		return err
	}
	if authority == nil {
		sock.refuse()
		return nil
	}
	sock.authority = authority
	profileScope, scoped := grant["scope"]
	if !scoped {
		profileScope = "profile"
	}
	owned := gateway.OwnerScope(l.serveCtx, authority, false)
	release := runner.AttachClient()
	defer release()
	protocol := gateway.CanonicalGatewayProtocol
	return ServeSessionSocket(owned, sock, map[string]any{"user_id": grant["subject"], "provider": "local",
		"profile_id": grant["profile_id"], "instance_id": grant["instance_id"], "capabilities": grant["capabilities"],
		"native_bootstrap": true, "profile_scope": profileScope}, &protocol, operator)
}
