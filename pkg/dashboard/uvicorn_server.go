package dashboard

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Settings of the ASGI server, the uvicorn Config keyword arguments the dashboard passes
type UvicornConfig struct {
	App                     ASGIApp
	Host                    string
	Port                    int
	ProxyHeaders            bool
	ForwardedAllowIPs       []string
	WSPingInterval          *time.Duration
	WSPingTimeout           *time.Duration
	WSPerMessageDeflate     bool
	WSMaxSize               int64
	TimeoutGracefulShutdown time.Duration
	TimeoutKeepAlive        time.Duration
	Lifespan                LifespanMode
}

// Signals the server turns into an exit request
var captureSignalSet = []os.Signal{os.Interrupt, syscall.SIGTERM}

func uvicornLog() *slog.Logger { return slog.Default().With("logger", "uvicorn.error") }

// ASGI server over a TCP listener, the uvicorn Server lifecycle
type UvicornServer struct {
	cfg         *UvicornConfig
	app         ASGIApp
	ln          net.Listener
	ShouldExit  atomic.Bool
	ForceExit   atomic.Bool
	Started     bool
	date        atomic.Value
	mu          sync.Mutex
	conns       map[*httpConn]bool
	tasks       sync.WaitGroup
	lifespan    *lifespanOn
	captured    []os.Signal
	serveCtx    context.Context
	cancelServe context.CancelFunc
}

// Server of a config, the app wrapped with the proxy headers middleware when asked
func NewUvicornServer(cfg *UvicornConfig) *UvicornServer {
	if cfg.TimeoutKeepAlive == 0 {
		cfg.TimeoutKeepAlive = 5 * time.Second
	}
	app := cfg.App
	if cfg.ProxyHeaders {
		allow := cfg.ForwardedAllowIPs
		if len(allow) == 0 {
			env, ok := os.LookupEnv("FORWARDED_ALLOW_IPS")
			if !ok {
				env = "127.0.0.1"
			}
			allow = strings.Split(env, ",")
		}
		app = proxyHeadersMiddleware(app, newTrustedHosts(allow))
	}
	s := &UvicornServer{cfg: cfg, app: app, conns: map[*httpConn]bool{}}
	s.updateDate()
	s.serveCtx, s.cancelServe = context.WithCancel(context.Background())
	return s
}

// Refreshes the date header the responses carry
func (s *UvicornServer) updateDate() {
	s.date.Store(time.Now().UTC().Format(http.TimeFormat))
}

// Default headers every response starts with, the date and the server name
func (s *UvicornServer) defaultHeaders() []Header {
	return []Header{{"date", s.date.Load().(string)}, {"server", "uvicorn"}}
}

// Listener address the server bound, nil before startup
func (s *UvicornServer) Addr() net.Addr {
	if s.ln == nil {
		return nil
	}
	return s.ln.Addr()
}

// Runs the lifespan startup then binds, the uvicorn startup with its exit on a bind failure
func (s *UvicornServer) Startup(ctx context.Context) error {
	if s.cfg.Lifespan != LifespanOff {
		s.lifespan = newLifespanOn(s.cfg.App)
		s.lifespan.startup(ctx)
		if s.lifespan.shouldExit {
			s.ShouldExit.Store(true)
			return nil
		}
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(s.cfg.Host, strconv.Itoa(s.cfg.Port)))
	if err != nil {
		uvicornLog().Error(bindErrorText(s.cfg.Host, s.cfg.Port, err))
		if s.lifespan != nil {
			s.lifespan.shutdown(ctx)
		}
		return &gateway.DashboardExit{Code: 1}
	}
	s.ln = ln
	s.Started = true
	go s.acceptLoop()
	return nil
}

// Text of the OSError asyncio raises for a failed bind
func bindErrorText(host string, port int, err error) string {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return fmt.Sprintf("[Errno %d] error while attempting to bind on address (%s, %d): %s", int(errno),
			pyrepr.Repr(host), port, lowerASCII(errno.Error()))
	}
	return err.Error()
}

func lowerASCII(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return string(b)
}

func (s *UvicornServer) acceptLoop() {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			uvicornLog().Error(err.Error())
			continue
		}
		hc := newHTTPConn(s, c)
		s.mu.Lock()
		s.conns[hc] = true
		s.mu.Unlock()
		s.tasks.Add(1)
		go func() {
			defer s.tasks.Done()
			hc.serve()
			s.mu.Lock()
			delete(s.conns, hc)
			s.mu.Unlock()
		}()
	}
}

// Ticks every tenth of a second refreshing the date each second until an exit is requested
func (s *UvicornServer) MainLoop(ctx context.Context) {
	counter := 0
	for {
		if counter%10 == 0 {
			s.updateDate()
		}
		if s.ShouldExit.Load() {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(100 * time.Millisecond):
		}
		counter = (counter + 1) % 864000
	}
}

// Stops accepting, asks connections to finish, waits up to the graceful timeout, then runs the lifespan shutdown
func (s *UvicornServer) Shutdown(ctx context.Context) {
	if s.ln != nil {
		s.ln.Close()
	}
	s.mu.Lock()
	for c := range s.conns {
		c.shutdown()
	}
	s.mu.Unlock()
	time.Sleep(100 * time.Millisecond)
	done := make(chan struct{})
	go func() {
		s.tasks.Wait()
		close(done)
	}()
	var timeout <-chan time.Time
	if s.cfg.TimeoutGracefulShutdown > 0 {
		timeout = time.After(s.cfg.TimeoutGracefulShutdown)
	}
	select {
	case <-done:
	case <-timeout:
		s.mu.Lock()
		n := len(s.conns)
		s.mu.Unlock()
		uvicornLog().Error(fmt.Sprintf("Cancel %d running task(s), timeout graceful shutdown exceeded", n))
		s.cancelServe()
		s.mu.Lock()
		for c := range s.conns {
			c.conn.Close()
		}
		s.mu.Unlock()
	}
	if !s.ForceExit.Load() && s.lifespan != nil {
		s.lifespan.shutdown(ctx)
	}
}

// Runs body with SIGINT and SIGTERM requesting an exit, a second SIGINT forcing it, and returns the signals caught
func (s *UvicornServer) CaptureSignals(body func()) []os.Signal {
	ch := make(chan os.Signal, 4)
	notifySignals(ch, captureSignalSet...)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case sig := <-ch:
				s.mu.Lock()
				s.captured = append(s.captured, sig)
				s.mu.Unlock()
				if s.ShouldExit.Load() && sig == os.Interrupt {
					s.ForceExit.Store(true)
				} else {
					s.ShouldExit.Store(true)
				}
			case <-stop:
				return
			}
		}
	}()
	body()
	stopSignals(ch)
	close(stop)
	wg.Wait()
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]os.Signal(nil), s.captured...)
}

// Lifespan protocol driver, startup and shutdown events to the app run in its own task
type lifespanOn struct {
	app          ASGIApp
	receiveQ     chan Message
	startupDone  chan struct{}
	shutdownDone chan struct{}
	failed       atomic.Bool
	errored      atomic.Bool
	shouldExit   bool
	finished     chan struct{}
	state        map[string]any
}

func newLifespanOn(app ASGIApp) *lifespanOn {
	return &lifespanOn{app: app, receiveQ: make(chan Message, 2), startupDone: make(chan struct{}),
		shutdownDone: make(chan struct{}), finished: make(chan struct{}), state: map[string]any{}}
}

// Sends the startup event and waits for the app to complete or fail it
func (l *lifespanOn) startup(ctx context.Context) {
	go l.main(ctx)
	l.receiveQ <- Message{Type: "lifespan.startup"}
	select {
	case <-l.startupDone:
	case <-l.finished:
	}
	if l.failed.Load() {
		uvicornLog().Error("Application startup failed. Exiting.")
		l.shouldExit = true
	}
}

// Sends the shutdown event and waits for the app to finish it
func (l *lifespanOn) shutdown(ctx context.Context) {
	if l.errored.Load() {
		return
	}
	l.receiveQ <- Message{Type: "lifespan.shutdown"}
	select {
	case <-l.shutdownDone:
	case <-l.finished:
	}
	if l.failed.Load() {
		uvicornLog().Error("Application shutdown failed. Exiting.")
		l.shouldExit = true
	}
}

func (l *lifespanOn) main(ctx context.Context) {
	defer close(l.finished)
	var startupOnce, shutdownOnce sync.Once
	scope := &Scope{Type: "lifespan", State: l.state}
	receive := func(ctx context.Context) (Message, error) {
		select {
		case m := <-l.receiveQ:
			return m, nil
		case <-ctx.Done():
			return Message{}, ctx.Err()
		}
	}
	send := func(_ context.Context, m Message) error {
		switch m.Type {
		case "lifespan.startup.complete":
			startupOnce.Do(func() { close(l.startupDone) })
		case "lifespan.startup.failed":
			l.failed.Store(true)
			if m.Reason != "" {
				uvicornLog().Error(m.Reason)
			}
			startupOnce.Do(func() { close(l.startupDone) })
		case "lifespan.shutdown.complete":
			shutdownOnce.Do(func() { close(l.shutdownDone) })
		case "lifespan.shutdown.failed":
			l.failed.Store(true)
			if m.Reason != "" {
				uvicornLog().Error(m.Reason)
			}
			shutdownOnce.Do(func() { close(l.shutdownDone) })
		}
		return nil
	}
	if err := l.app(ctx, scope, receive, send); err != nil {
		l.errored.Store(true)
		if l.failed.Load() {
			return
		}
		uvicornLog().Info("ASGI 'lifespan' protocol appears unsupported.")
	}
}
