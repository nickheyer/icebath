package tools

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/mcp"
	"github.com/nickheyer/icebath/internal/tools"
)

const (
	// Reconnects a session that never proved healthy may take, and attempts of the very first connection
	maxReconnects     = 5
	maxInitialRetries = 3
	maxBackoff        = 60 * time.Second
	// How often a parked server tries to come back by itself
	parkedProbe = 300 * time.Second
	// Longest a keepalive probe may take
	probeTimeout = 30 * time.Second
	// Grace a server gets to stop before its task is cancelled
	shutdownWait = 10 * time.Second
	// How a server stands
	stateConnecting = "connecting"
	stateConnected  = "connected"
	stateParked     = "failed"
	stateRecycled   = "recycled"
	stateLazy       = "lazy"
	stateStopped    = "stopped"
)

// One server of a profile: a long lived goroutine that connects, serves, reconnects, parks after repeated failures and recycles an idle
// stdio child
type server struct {
	pool *pool
	name string
	// Connected once to see what it offers, never registered
	probe bool

	mu      sync.Mutex
	spec    *spec
	session *mcp.Session
	result  *mcp.InitializeResult
	tools   []*mcp.Tool
	// Tools and utility tools of the schema cache, offered until the server connects
	cached        []*mcp.Tool
	cachedUtility []string
	hints         mcp.CacheHints
	state         string
	// Why it is parked, nil while it serves
	err error
	// It had a session once, and that session proved healthy
	ever, proven bool
	// Why it is parked or recycled, empty when it is neither
	parked, recycled string
	// Credentials of an OAuth sign in it parked on, so a self probe waits for new ones
	authStamp *mcp.Stamp
	lastCall  time.Time
	started   time.Time
	pingless  bool
	// The server refused Streamable HTTP once and works over SSE
	sseFallback bool
	rejection   mcp.HTTPRejection
	stdio       *mcp.StdioTransport
	// Environment and context of the call running, where its sampling and elicitation requests go
	pending    *tools.Env
	pendingCtx context.Context
	inflight   map[*inflight]bool
	// A deliberate teardown is failing the calls in flight
	tearing  bool
	breaker  breaker
	sampler  *sampler
	elicitor *elicitor
	provider *mcp.Provider
	// Closed and replaced whenever the state changes, for those waiting on it
	changed chan struct{}

	rpc     chan struct{}
	wake    chan struct{}
	stop    chan struct{}
	done    chan struct{}
	running bool
	cancel  context.CancelFunc
	refresh sync.WaitGroup
}

type inflight struct{ cancel context.CancelFunc }

// Consecutive failures of calls, which pause the server's tools once they reach three
type breaker struct {
	failures int
	opened   time.Time
	// Every failure was the server answering with an error rather than not answering
	application bool
}

func newServer(p *pool, s *spec) *server {
	now := time.Now()
	return &server{pool: p, name: s.Name, spec: s, state: stateConnecting, inflight: map[*inflight]bool{}, changed: make(chan struct{}),
		rpc: make(chan struct{}, 1), wake: make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{}), lastCall: now, started: now}
}

// Sets the state, telling those waiting on it
func (s *server) set(update func()) {
	s.mu.Lock()
	update()
	close(s.changed)
	s.changed = make(chan struct{})
	s.mu.Unlock()
	s.pool.touch()
}

func (s *server) snapshot() (state string, session *mcp.Session, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state, s.session, s.err
}

func (s *server) currentSpec() *spec {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.spec
}

// Tools it offers now: those it listed, or the cached ones until it connects
func (s *server) offered() []*mcp.Tool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.session != nil || s.state == stateRecycled {
		return s.tools
	}
	if s.state == stateLazy {
		return s.cached
	}
	return nil
}

// Whether its tools may be offered: connected, a dormant stdio child woken on use, or lazy
func (s *server) available() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.session != nil || s.state == stateRecycled || s.state == stateLazy
}

func (s *server) lock(ctx context.Context) error {
	select {
	case s.rpc <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *server) unlock() { <-s.rpc }

func (s *server) busy() bool { return len(s.rpc) == 1 }

// Asks the run goroutine to rebuild its connection
func (s *server) signal() bool {
	s.mu.Lock()
	running := s.running
	s.mu.Unlock()
	if !running {
		return false
	}
	select {
	case s.wake <- struct{}{}:
	default:
	}
	return true
}

func (s *server) stopping() bool {
	select {
	case <-s.stop:
		return true
	default:
		return false
	}
}

// Starts the run goroutine and waits until the first connection is up or failed, or ctx ends
func (s *server) start(ctx context.Context) error {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return s.wait(ctx)
	}
	s.running = true
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	s.cancel = cancel
	s.state = stateConnecting
	s.mu.Unlock()
	go s.run(runCtx, mcp.SignerOf(ctx))
	return s.wait(ctx)
}

// Waits until the server has a session, or stands parked, or ctx ends
func (s *server) wait(ctx context.Context) error {
	for {
		s.mu.Lock()
		state, session, err, changed := s.state, s.session, s.err, s.changed
		s.mu.Unlock()
		switch {
		case session != nil:
			return nil
		case state == stateParked && err != nil:
			return err
		case state == stateStopped:
			return errors.New("MCP server '" + s.name + "' stopped")
		}
		select {
		case <-changed:
		case <-s.done:
			s.mu.Lock()
			err := s.err
			s.mu.Unlock()
			return cmp.Or(err, errors.New("MCP server '"+s.name+"' task exited before becoming ready"))
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Connecting, serving, reconnecting and parking until stopped; a sign in a connect needs goes to the signer it started with
func (s *server) run(ctx context.Context, signer mcp.Signer) {
	defer func() {
		s.set(func() { s.state, s.session, s.running = stateStopped, nil, false })
		close(s.done)
	}()
	retries, initial, backoff := 0, 0, time.Second
	grace := false
	for {
		connectCtx := ctx
		if signer != nil {
			connectCtx = mcp.WithSigner(ctx, signer)
		}
		reason, err := s.serve(connectCtx)
		// A signer serves the first connection alone, later ones run unattended
		signer = nil
		s.set(func() { s.session, s.stdio = nil, nil })
		if s.stopping() || ctx.Err() != nil {
			return
		}
		if err == nil {
			s.mu.Lock()
			proven, tearing := s.proven, s.tearing
			s.tearing = false
			s.mu.Unlock()
			switch {
			case reason == "recycle":
				slog.Info("MCP server stdio child recycled; it starts again on the next call", "server", s.name, "reason", s.recycledReason())
				if !s.idle(ctx) {
					return
				}
				continue
			case proven || tearing:
				retries, backoff = 0, time.Second
			default:
				if retries++; retries > maxReconnects {
					if !s.park(ctx, fmt.Errorf("%d consecutive reconnects without a healthy session", maxReconnects), "rapid-drop budget exhausted") {
						return
					}
					retries, backoff = maxReconnects, time.Second
				}
			}
			continue
		}
		s.mu.Lock()
		ever, proven := s.ever, s.proven
		s.mu.Unlock()
		permanent := classify(err) == "permanent"
		switch {
		case !ever && permanent:
			if !s.park(ctx, err, "after a permanent initial failure") {
				return
			}
			retries, initial, backoff = 0, 0, time.Second
			continue
		case !ever:
			if initial++; initial > maxInitialRetries {
				if !s.park(ctx, err, "after initial connection failures") {
					return
				}
				retries, initial, backoff = 0, 0, time.Second
				continue
			}
		case permanent && mcp.AuthFailure(err) && proven && !grace:
			grace = true
			slog.Warn("MCP auth error on a previously healthy session; reconnecting once before parking", "server", s.name, "error", err)
			retries, backoff = 0, time.Second
		case permanent:
			if !s.park(ctx, err, "after a permanent error") {
				return
			}
			retries, backoff = maxReconnects, time.Second
			continue
		default:
			if retries++; retries > maxReconnects {
				if !s.park(ctx, err, fmt.Sprintf("after %d reconnection attempts", maxReconnects)) {
					return
				}
				retries, backoff = maxReconnects, time.Second
				continue
			}
		}
		slog.Debug("MCP server connection failed, retrying", "server", s.name, "error", err, "in", backoff)
		if !s.sleep(ctx, jitter(backoff)) {
			return
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

// Seconds with twenty percent jitter either way, so servers that lost one backend don't all retry at once
func jitter(d time.Duration) time.Duration {
	return time.Duration(float64(d) * (0.8 + rand.Float64()*0.4))
}

func (s *server) sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-s.wake:
		return true
	case <-s.stop:
		return false
	case <-ctx.Done():
		return false
	}
}

func (s *server) recycledReason() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.recycled
}

// Waits dormant after a recycle until a call wakes it, false when stopped
func (s *server) idle(ctx context.Context) bool {
	select {
	case <-s.wake:
		s.set(func() { s.state, s.recycled = stateConnecting, "" })
		return true
	case <-s.stop:
		return false
	case <-ctx.Done():
		return false
	}
}

// Drops its tools and waits for a reconnect request, probing by itself every five minutes while it is still set up and enabled; false
// when stopped
func (s *server) park(ctx context.Context, err error, why string) bool {
	line := fmt.Sprintf("MCP server '%s' parked %s; it probes again every %s or on /reload-mcp", s.name, why, parkedProbe)
	if mcp.AuthFailure(err) {
		line = fmt.Sprintf("MCP server '%s' needs a sign in; run `icebath mcp login %s` or sign in from /mcp", s.name, s.name)
	}
	s.mu.Lock()
	repeat := s.parked == line
	var stamp *mcp.Stamp
	if s.provider != nil && mcp.AuthFailure(err) {
		current := s.provider.Store().Stamp()
		stamp = &current
	}
	s.mu.Unlock()
	if repeat {
		slog.Debug(line, "error", err)
	} else {
		slog.Warn(line, "error", err)
	}
	s.set(func() { s.state, s.err, s.parked, s.session, s.authStamp = stateParked, err, line, nil, stamp })
	for {
		t := time.NewTimer(parkedProbe)
		select {
		case <-s.wake:
			t.Stop()
			s.set(func() { s.state, s.err, s.parked, s.authStamp = stateConnecting, nil, "", nil })
			return true
		case <-s.stop:
			t.Stop()
			return false
		case <-ctx.Done():
			t.Stop()
			return false
		case <-t.C:
		}
		spec, ok := s.pool.spec(s.name)
		if !ok || !spec.Enabled {
			slog.Debug("MCP server parked and disabled or gone from the settings; not probing", "server", s.name)
			continue
		}
		s.mu.Lock()
		waiting := s.authStamp != nil && s.provider != nil && s.provider.Store().Stamp() == *s.authStamp
		s.mu.Unlock()
		if waiting {
			continue
		}
		s.set(func() { s.spec, s.state, s.parked = spec, stateConnecting, "" })
		return true
	}
}

// Stops the run goroutine and waits for it, cancelling it after the grace period
func (s *server) shutdown() {
	s.mu.Lock()
	running := s.running
	select {
	case <-s.stop:
	default:
		close(s.stop)
	}
	s.mu.Unlock()
	s.failInflight("shutdown")
	if running {
		select {
		case <-s.done:
		case <-time.After(shutdownWait):
			slog.Warn("MCP server shutdown timed out, cancelling its task", "server", s.name)
			s.mu.Lock()
			cancel := s.cancel
			s.mu.Unlock()
			cancel()
			<-s.done
		}
	}
	s.refresh.Wait()
}

// Cancels every call running before the transport goes, flagging the teardown so a reconnect does not count against it
func (s *server) failInflight(reason string) {
	s.mu.Lock()
	victims := make([]*inflight, 0, len(s.inflight))
	for call := range s.inflight {
		victims = append(victims, call)
	}
	if len(victims) > 0 {
		s.tearing = true
		slog.Warn("MCP server teardown cancelled calls in flight", "server", s.name, "reason", reason, "calls", len(victims))
	}
	s.mu.Unlock()
	for _, call := range victims {
		call.cancel()
	}
}

func (s *server) track(cancel context.CancelFunc) func() {
	call := &inflight{cancel: cancel}
	s.mu.Lock()
	s.inflight[call] = true
	s.mu.Unlock()
	return func() {
		s.mu.Lock()
		delete(s.inflight, call)
		s.mu.Unlock()
	}
}

func (s *server) reconnecting() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tearing
}

// One life of a transport: opened, negotiated, its tools listed, served until a lifecycle event
func (s *server) serve(ctx context.Context) (string, error) {
	spec := s.currentSpec()
	s.setHandlers(spec)
	if spec.remote() {
		return s.serveHTTP(ctx, spec)
	}
	return s.serveStdio(ctx, spec)
}

// Sampling and elicitation handlers of the settings, those turned off left out so the client never declares them
func (s *server) setHandlers(spec *spec) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sampler, s.elicitor = nil, nil
	if spec.Sampling.Enabled {
		s.sampler = &sampler{server: s, config: spec.Sampling, metrics: map[string]int{}}
	}
	if spec.Elicitation.Enabled {
		s.elicitor = &elicitor{server: s, config: spec.Elicitation, pending: map[string]chan struct{}{}, metrics: map[string]int{}}
	}
}

func (s *server) options() mcp.SessionOptions {
	s.mu.Lock()
	defer s.mu.Unlock()
	opts := mcp.SessionOptions{Name: s.name, OnToolsChanged: s.scheduleRefresh}
	if s.sampler != nil {
		opts.Sampler = s.sampler
	}
	if s.elicitor != nil {
		opts.Elicitor = s.elicitor
	}
	return opts
}

// Negotiates, lists its tools, publishes them and serves until shutdown, a reconnect, a recycle or the session dropping
func (s *server) attend(ctx context.Context, session *mcp.Session, spec *spec, stdio *mcp.StdioTransport) (string, error) {
	mode := cmp.Or(spec.Protocol, mcp.ModeAuto)
	result, err := session.Negotiate(ctx, mode, spec.ConnectTimeout)
	if err != nil {
		return "", err
	}
	session.OpenStream()
	var listed []*mcp.Tool
	var hints mcp.CacheHints
	if result.Capabilities == nil || result.Capabilities.Tools != nil {
		if err := s.lock(ctx); err != nil {
			return "", err
		}
		listCtx, cancel := context.WithTimeout(ctx, spec.ConnectTimeout)
		listed, err = session.ListTools(listCtx, &hints)
		cancel()
		s.unlock()
		if err != nil {
			return "", err
		}
	} else {
		slog.Info("MCP server does not advertise tools; skipping tools/list (prompts and resources stay available)", "server", s.name)
	}
	now := time.Now()
	s.set(func() {
		s.session, s.result, s.tools, s.hints, s.stdio = session, result, listed, hints, stdio
		s.state, s.err, s.parked, s.ever, s.proven, s.pingless = stateConnected, nil, "", true, false, false
		s.started, s.lastCall = now, now
	})
	if !s.probe {
		s.pool.connected(s)
	}
	return s.lifecycle(ctx, session, spec)
}

// Serves until a lifecycle event: probing a remote or configured session, recycling an idle or old stdio child, and taking a quiet
// stdio child as proven after the keepalive interval
func (s *server) lifecycle(ctx context.Context, session *mcp.Session, spec *spec) (string, error) {
	proofAt := time.Now().Add(defaultKeepalive)
	for {
		if reason := s.recycleDue(spec); reason != "" {
			return "recycle", nil
		}
		wait := time.Duration(-1)
		if spec.Keepalive > 0 {
			wait = spec.Keepalive
		} else if !s.isProven() {
			wait = max(0, time.Until(proofAt))
		}
		if deadline, ok := s.nextRecycle(spec); ok && (wait < 0 || time.Until(deadline) < wait) {
			wait = max(0, time.Until(deadline))
		}
		reason, event, err := s.await(ctx, session, spec, wait)
		if event {
			return reason, err
		}
		if reason := s.recycleDue(spec); reason != "" {
			return "recycle", nil
		}
		if spec.Keepalive == 0 {
			if !s.isProven() && !time.Now().Before(proofAt) {
				s.prove()
			}
			continue
		}
		if s.busy() || s.inflightCount() > 0 {
			continue
		}
		if err := s.lock(ctx); err != nil {
			return "", err
		}
		err = s.keepalive(ctx, session)
		s.unlock()
		if err != nil {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			slog.Warn("MCP server keepalive failed; reconnecting", "server", s.name, "error", err)
			s.failInflight("reconnect")
			return "reconnect", nil
		}
		s.prove()
	}
}

// Waits for a lifecycle event or the time given, forever for a negative one; false for event when the time passed first
func (s *server) await(ctx context.Context, session *mcp.Session, spec *spec, wait time.Duration) (string, bool, error) {
	var timer <-chan time.Time
	if wait >= 0 {
		t := time.NewTimer(wait)
		defer t.Stop()
		timer = t.C
	}
	select {
	case <-ctx.Done():
		return "", true, ctx.Err()
	case <-s.stop:
		s.failInflight("shutdown")
		return "shutdown", true, nil
	case <-s.wake:
		s.failInflight("reconnect")
		slog.Info("MCP server reconnect requested; tearing down its session", "server", s.name)
		return "reconnect", true, nil
	case <-session.Done():
		if s.stopping() {
			return "shutdown", true, nil
		}
		drop := cmp.Or(session.Err(), errors.New("MCP session closed"))
		if spec.remote() && s.isEver() {
			slog.Debug("MCP server session dropped; reconnecting at once", "server", s.name, "error", drop)
			return "reconnect", true, nil
		}
		return "", true, drop
	case <-timer:
		return "", false, nil
	}
}

func (s *server) isProven() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.proven
}

func (s *server) isEver() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ever
}

func (s *server) prove() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.proven = true
}

func (s *server) inflightCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.inflight)
}

// Ping, or tools/list for a server that does not answer ping
func (s *server) keepalive(ctx context.Context, session *mcp.Session) error {
	list := func() error {
		c, cancel := context.WithTimeout(ctx, probeTimeout)
		defer cancel()
		_, err := session.ListTools(c, nil)
		return err
	}
	s.mu.Lock()
	pingless, advertises := s.pingless, s.result == nil || s.result.Capabilities == nil || s.result.Capabilities.Tools != nil
	s.mu.Unlock()
	if pingless {
		return list()
	}
	c, cancel := context.WithTimeout(ctx, probeTimeout)
	err := session.Ping(c)
	cancel()
	switch {
	case err == nil:
		return nil
	case mcp.MethodNotFound(err) && advertises:
		slog.Info("MCP server does not implement ping; using tools/list for keepalive", "server", s.name)
	case mcp.Timeout(err) && advertises:
		if list() != nil {
			return err
		}
		slog.Info("MCP server ping timed out but tools/list answered; using tools/list for keepalive", "server", s.name)
	default:
		return err
	}
	s.mu.Lock()
	s.pingless = true
	s.mu.Unlock()
	return list()
}

// Limit of an idle or long lived stdio child that has passed, empty when none; never while a call runs
func (s *server) recycleDue(spec *spec) string {
	if spec.remote() || s.busy() {
		return ""
	}
	s.mu.Lock()
	started, last := s.started, s.lastCall
	s.mu.Unlock()
	reason := ""
	switch {
	case spec.MaxLifetime > 0 && time.Since(started) >= spec.MaxLifetime:
		reason = "max_lifetime_seconds"
	case spec.IdleTimeout > 0 && time.Since(last) >= spec.IdleTimeout:
		reason = "idle_timeout_seconds"
	}
	if reason != "" {
		s.set(func() { s.state, s.recycled, s.session = stateRecycled, reason, nil })
	}
	return reason
}

func (s *server) nextRecycle(spec *spec) (time.Time, bool) {
	if spec.remote() {
		return time.Time{}, false
	}
	s.mu.Lock()
	started, last := s.started, s.lastCall
	s.mu.Unlock()
	var next time.Time
	if spec.MaxLifetime > 0 {
		next = started.Add(spec.MaxLifetime)
	}
	if spec.IdleTimeout > 0 && (next.IsZero() || last.Add(spec.IdleTimeout).Before(next)) {
		next = last.Add(spec.IdleTimeout)
	}
	return next, !next.IsZero()
}

func (s *server) called() {
	s.mu.Lock()
	s.lastCall = time.Now()
	s.mu.Unlock()
}

// Lists the tools again after the server said they changed, and publishes them
func (s *server) scheduleRefresh() {
	s.refresh.Add(1)
	go func() {
		defer s.refresh.Done()
		ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
		defer cancel()
		if err := s.lock(ctx); err != nil {
			return
		}
		_, session, _ := s.snapshot()
		if session == nil {
			s.unlock()
			return
		}
		fresh, err := session.ListTools(ctx, nil)
		s.unlock()
		if err != nil {
			slog.Error("MCP server tool refresh failed", "server", s.name, "error", err)
			return
		}
		old := names(s.offered())
		s.set(func() { s.tools = fresh })
		added, removed := difference(names(fresh), old), difference(old, names(fresh))
		if len(added)+len(removed) > 0 {
			slog.Warn("MCP server tools changed; verify these changes are expected", "server", s.name, "added", strings.Join(added, ", "),
				"removed", strings.Join(removed, ", "))
		}
		if !s.probe {
			s.pool.connected(s)
		}
	}()
}

func names(list []*mcp.Tool) []string {
	out := make([]string, 0, len(list))
	for _, t := range list {
		if t != nil {
			out = append(out, t.Name)
		}
	}
	return out
}

func difference(a, b []string) []string {
	var out []string
	for _, x := range a {
		if !slices.Contains(b, x) {
			out = append(out, x)
		}
	}
	return out
}

// One stdio child: its launch preflighted, its stderr teed to the log, its session attended
func (s *server) serveStdio(ctx context.Context, spec *spec) (string, error) {
	if spec.Command == "" {
		return "", fmt.Errorf("MCP server '%s' has no 'command' in config", s.name)
	}
	if spec.Identity[0] != "" {
		slog.Warn("MCP identity_header works on HTTP and SSE servers only; ignored for this stdio server", "server", s.name)
	}
	env, command, err := s.pool.launchEnv(ctx, spec)
	if err != nil {
		return "", err
	}
	command, args, err := s.pool.preflight(ctx, s.name, command, spec.Args)
	if err != nil {
		return "", err
	}
	log := s.pool.stderrLog()
	fmt.Fprintf(log, "\n%s ===== starting MCP server '%s' =====\n", stamp(), s.name)
	tee := newTee(log)
	transport, err := mcp.StartStdio(mcp.StdioParams{Command: command, Args: args, Env: env, Cwd: cmp.Or(spec.Cwd, s.pool.workspaceDir()), Stderr: tee,
		Owner: s.pool.m.owner})
	if err != nil {
		return "", s.pool.abiOr(s.name, tee.tail(), err)
	}
	session := mcp.NewSession(transport, s.options())
	reason, err := s.attend(ctx, session, spec, transport)
	session.Close()
	if err != nil {
		return "", s.pool.abiOr(s.name, tee.tail(), err)
	}
	return reason, nil
}

// Remote server: Streamable HTTP, or SSE when pinned, latched or after Streamable HTTP refused the first connect
func (s *server) serveHTTP(ctx context.Context, spec *spec) (string, error) {
	url, err := mcp.ValidURL(s.name, spec.URL)
	if err != nil {
		return "", err
	}
	if refs := spec.unsetRemote(); len(refs) > 0 {
		return "", &unsetError{server: s.name, refs: refs}
	}
	headers := spec.headers()
	configured := map[string]bool{}
	for k := range spec.Headers {
		configured[strings.ToLower(k)] = true
	}
	opts := mcp.HTTPOptions{URL: url, Headers: headers, Configured: configured, Strict: spec.Strict, SSLVerify: spec.SSLVerify, ClientCert: spec.ClientCert,
		ClientKey: spec.ClientKey, ConnectTimeout: spec.ConnectTimeout, ReadTimeout: 300 * time.Second, Server: s.name}
	if spec.Auth == "oauth" {
		provider, err := s.pool.provider(spec, opts)
		if err != nil {
			slog.Warn("MCP OAuth setup failed", "server", s.name, "error", err)
			return "", err
		}
		s.mu.Lock()
		s.provider = provider
		s.mu.Unlock()
		opts.Auth = provider
	}
	if spec.Transport != "sse" && !spec.SkipPreflight && spec.Auth != "oauth" && !s.isEver() {
		if err := s.preflight(ctx, opts); err != nil {
			return "", err
		}
	}
	sse := func() (mcp.Transport, error) {
		if spec.Strict {
			return nil, fmt.Errorf("MCP server '%s': strict_redirect_headers is not supported on the SSE transport.", s.name)
		}
		return mcp.StartSSE(ctx, opts, spec.ConnectTimeout)
	}
	s.mu.Lock()
	latched := s.sseFallback
	s.mu.Unlock()
	if spec.Transport == "sse" || latched {
		return s.serveRemote(ctx, spec, sse)
	}
	reason, err := s.serveRemote(ctx, spec, func() (mcp.Transport, error) { return mcp.StartStreamableHTTP(opts) })
	if err == nil {
		return reason, nil
	}
	s.mu.Lock()
	rejection, ever := s.rejection, s.ever
	s.mu.Unlock()
	detail := err.Error()
	if rpc, ok := errors.AsType[*mcp.RPCError](err); ok && rpc.Code == mcp.CodeInternalError && rejection.Status != 0 {
		detail = fmt.Sprintf("%s (%s)", detail, rejection.Describe())
	}
	if ever || spec.Strict || !mcp.StreamableRejected(err) {
		if detail != err.Error() {
			return "", fmt.Errorf("MCP server '%s': Streamable HTTP connect failed (%s): %w", s.name, detail, err)
		}
		return "", err
	}
	slog.Warn("MCP server: Streamable HTTP rejected the first connect; retrying over SSE. If this connects, set transport: sse for it to skip the failed "+
		"attempt", "server", s.name, "error", detail)
	s.mu.Lock()
	s.sseFallback = true
	s.mu.Unlock()
	reason, sseErr := s.serveRemote(ctx, spec, sse)
	if sseErr == nil {
		return reason, nil
	}
	if !s.isEver() {
		s.mu.Lock()
		s.sseFallback = false
		s.mu.Unlock()
		return "", fmt.Errorf("MCP server '%s': both Streamable HTTP and SSE transports failed (Streamable HTTP: %s; SSE: %s). Check the URL points at an MCP "+
			"endpoint, or pin transport: sse if the server is SSE-only: %w", s.name, detail, sseErr, sseErr)
	}
	return "", sseErr
}

func (s *server) serveRemote(ctx context.Context, spec *spec, open func() (mcp.Transport, error)) (string, error) {
	transport, err := open()
	if err != nil {
		return "", err
	}
	session := mcp.NewSession(transport, s.options())
	reason, err := s.attend(ctx, session, spec, nil)
	session.Close()
	if rejecting, ok := transport.(interface{ Rejection() mcp.HTTPRejection }); ok {
		s.mu.Lock()
		s.rejection = rejecting.Rejection()
		s.mu.Unlock()
	}
	return reason, err
}

// initialize a content type probe posts
var probeBody = `{"jsonrpc":"2.0","id":"_probe","method":"initialize","params":{"protocolVersion":"` + mcp.ProbeVersion +
	`","capabilities":{},"clientInfo":{"name":"icebath-probe","version":"0.1"}}}`

// Fails fast when a remote address answers 2xx with a type no MCP endpoint answers with, such as a web page
func (s *server) preflight(ctx context.Context, opts mcp.HTTPOptions) error {
	opts.ConnectTimeout, opts.ReadTimeout = 5*time.Second, 5*time.Second
	client, err := mcp.NewClient(opts)
	if err != nil {
		return err
	}
	client.Timeout = 5 * time.Second
	do := func(method, body string, extra map[string]string) (*http.Response, error) {
		var reader io.Reader
		if body != "" {
			reader = strings.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, opts.URL, reader)
		if err != nil {
			return nil, err
		}
		for k, v := range opts.Headers {
			req.Header.Set(k, v)
		}
		for k, v := range extra {
			req.Header.Set(k, v)
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		io.Copy(io.Discard, io.LimitReader(resp.Body, mcp.MaxBodyBytes))
		resp.Body.Close()
		return resp, nil
	}
	foreign := func(resp *http.Response) bool {
		kind := mcp.ContentType(resp)
		return resp.StatusCode >= 200 && resp.StatusCode < 300 && kind != "" && kind != "application/json" && kind != "text/event-stream"
	}
	resp, err := do(http.MethodHead, "", nil)
	if err == nil && (resp.StatusCode == 405 || resp.StatusCode == 501) {
		resp, err = do(http.MethodGet, "", nil)
	}
	if err == nil && foreign(resp) {
		if post, err := do(http.MethodPost, probeBody, map[string]string{"Content-Type": "application/json", "Accept": "application/json, text/event-stream"}); err == nil &&
			!foreign(post) && post.StatusCode >= 200 && post.StatusCode < 300 {
			resp = post
		}
	}
	if err != nil {
		return ctx.Err()
	}
	if !foreign(resp) {
		return nil
	}
	return &mcp.NonMCPError{Msg: fmt.Sprintf("MCP server '%s' at %s returned Content-Type '%s', not an MCP response (expected one of: application/json, "+
		"text/event-stream). The URL most likely points at a web page rather than an MCP endpoint — check it resolves to a Streamable HTTP / SSE endpoint "+
		"(e.g. https://host/mcp, not https://host/).", s.name, opts.URL, mcp.ContentType(resp))}
}

// Local time the log lines carry
func stamp() string { return time.Now().Format("2006-01-02 15:04:05,000") }

var stamped = regexp.MustCompile(`^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2},\d{3}(?:\s|$)`)

// Copies a child's stderr into the log a stamped line at a time, keeping the last bytes for an error that needs them
type tee struct {
	log     io.Writer
	mu      sync.Mutex
	last    []byte
	partial []byte
}

func newTee(log io.Writer) *tee { return &tee{log: log} }

func (t *tee) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.last = append(t.last, p...)
	if len(t.last) > 16<<10 {
		t.last = t.last[len(t.last)-16<<10:]
	}
	t.partial = append(t.partial, p...)
	if i := strings.LastIndexByte(string(t.partial), '\n'); i >= 0 {
		var b strings.Builder
		for _, line := range strings.Split(strings.ToValidUTF8(string(t.partial[:i]), "�"), "\n") {
			line = strings.TrimRight(line, "\r")
			if !stamped.MatchString(line) {
				line = stamp() + " " + line
			}
			b.WriteString(line + "\n")
		}
		io.WriteString(t.log, b.String())
		t.partial = append([]byte(nil), t.partial[i+1:]...)
	}
	return len(p), nil
}

func (t *tee) tail() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.ToValidUTF8(string(t.last), "�")
}

// Last lines a server wrote to the shared stderr log since it last started, empty when none
func tailLog(path, server string, maxBytes, maxLines int) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return ""
	}
	start := max(0, info.Size()-int64(maxBytes))
	data := make([]byte, info.Size()-start)
	if _, err := f.ReadAt(data, start); err != nil && !errors.Is(err, io.EOF) {
		return ""
	}
	chunk := strings.ToValidUTF8(string(data), "�")
	at := strings.LastIndex(chunk, fmt.Sprintf("starting MCP server '%s'", server))
	if at < 0 {
		return ""
	}
	var lines []string
	for _, line := range strings.Split(chunk[at:], "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, strings.TrimRight(line, "\r"))
		}
	}
	return strings.Join(lines[max(0, len(lines)-maxLines):], "\n")
}

func logPath(dir string) string { return filepath.Join(dir, "logs", "mcp-stderr.log") }
