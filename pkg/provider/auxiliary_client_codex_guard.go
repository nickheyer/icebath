package provider

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
	"weak"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Python InterruptedError a user interrupt raises inside a provider stream
type InterruptedError struct {
	Message string
}

func (e *InterruptedError) Error() string { return e.Message }
func (*InterruptedError) auxInterrupted() {}

// Implemented by a stream whose unreleased attempt connection a stranger goroutine may shut down
type auxAttemptConnHolder interface {
	auxAttemptConn() net.Conn
}

// Raw connection under an SDK event stream, nil when the stream has none or already released it
func auxAttemptStreamSocket(stream any) net.Conn {
	if _, ok := stream.(auxAttemptConnHolder); ok {
		return socketFromStream(stream)
	}
	return nil
}

// Raw socket behind one network stream, the attempt connection a stream still holds or the connection itself
func socketFromStream(stream any) net.Conn {
	switch s := stream.(type) {
	case auxAttemptConnHolder:
		return s.auxAttemptConn()
	case net.Conn:
		return s
	}
	return nil
}

// Raw socket behind one wrapper of a nested connection, its inner stream first and the wrapper itself after
func socketFromCandidate(candidate any) net.Conn {
	if wrapper, ok := candidate.(interface{ NetConn() net.Conn }); ok {
		if inner := wrapper.NetConn(); inner != nil {
			if sock := socketFromStream(inner); sock != nil {
				return sock
			}
		}
	}
	return socketFromStream(candidate)
}

// Raw socket behind a response through the stream that carries it, nil when it holds none
func socketFromResponse(response any) net.Conn {
	if response == nil {
		return nil
	}
	return socketFromCandidate(response)
}

// Closes the target when it can be closed, a failure logged at debug under the note and silent without one
func auxCloseQuietly(target any, failureNote string) {
	closer, ok := target.(interface{ Close() error })
	if !ok {
		return
	}
	if err := closer.Close(); err != nil && failureNote != "" {
		slog.Debug("Codex auxiliary: "+failureNote, "error", err)
	}
}

// Text the context compressor reads as a retry ladder timeout
const CodexStreamStallMarker = "stream stalled"

// Progress aware deadline and connection safe timeout watchdog of one Codex auxiliary stream attempt
type auxCodexStreamGuard struct {
	ctx                   context.Context
	client                any
	totalTimeout          float64
	hasTotal              bool
	start                 time.Time
	noProgressTimeout     float64
	hardDeadline          time.Time
	deadlineMu            sync.Mutex
	progressDeadline      time.Time
	sawContent            atomic.Bool
	timedOut              atomic.Bool
	timeoutReleasePending atomic.Bool
	streamFinished        atomic.Bool
	timerMu               sync.Mutex
	timer                 *time.Timer
	protectedCancelCheck  AuxCancelCheck
	attemptMu             sync.Mutex
	attemptStream         any
}

// Guard with a task or default no progress window clamped by the total and host deadline
func newAuxCodexStreamGuard(ctx context.Context, client any, totalTimeout float64, hasTotal bool, noProgressTimeout float64) *auxCodexStreamGuard {
	g := &auxCodexStreamGuard{ctx: ctx, client: client, totalTimeout: totalTimeout, hasTotal: hasTotal, start: time.Now()}
	g.noProgressTimeout = auxStreamNoProgressTimeoutSeconds
	if noProgressTimeout > 0 {
		g.noProgressTimeout = noProgressTimeout
	}
	ceilingInput := 0.0
	if hasTotal {
		g.noProgressTimeout = min(g.noProgressTimeout, totalTimeout)
		ceilingInput = totalTimeout
	}
	g.hardDeadline = g.start.Add(auxSeconds(auxStreamTotalCeilingFn(ceilingInput)))
	if host, ok := auxCurrentAuxStreamDeadline(ctx); ok && host.Before(g.hardDeadline) {
		g.hardDeadline = host
	}
	g.progressDeadline = g.start.Add(auxSeconds(g.noProgressTimeout))
	if auxInterruptProtected(ctx) {
		g.protectedCancelCheck = auxCaptureAuxCancelCheck(ctx)
	}
	return g
}

func auxSeconds(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }

// Earlier of the hard ceiling and the current no progress deadline
func (g *auxCodexStreamGuard) effectiveDeadline() time.Time {
	g.deadlineMu.Lock()
	defer g.deadlineMu.Unlock()
	if g.progressDeadline.Before(g.hardDeadline) {
		return g.progressDeadline
	}
	return g.hardDeadline
}

// True when the frozen hard cancel source reports the owner already cancelled
func (g *auxCodexStreamGuard) cancelRequested() bool {
	return g.protectedCancelCheck != nil && auxCapturedAuxCancelRequested(g.protectedCancelCheck)
}

func (g *auxCodexStreamGuard) adoptStream(stream any) {
	g.attemptMu.Lock()
	g.attemptStream = stream
	g.attemptMu.Unlock()
}

// Closes the attempt stream silently on the owner side and forgets it
func (g *auxCodexStreamGuard) releaseStream(stream any) {
	auxCloseQuietly(stream, "")
	g.attemptMu.Lock()
	g.attemptStream = nil
	g.attemptMu.Unlock()
}

// Wakes only this attempt stream, a stranger goroutine shutting its connection down instead of closing it
func (g *auxCodexStreamGuard) closeAttemptStream(failureNote string, owner bool) {
	g.attemptMu.Lock()
	stream := g.attemptStream
	g.attemptMu.Unlock()
	if stream == nil {
		return
	}
	if !owner {
		if conn := auxAttemptStreamSocket(stream); conn != nil {
			auxShutdownConn(conn)
			return
		}
	}
	auxCloseQuietly(stream, failureNote)
}

// Substantive payload re-arms the no progress window while the hard ceiling never moves
func (g *auxCodexStreamGuard) recordProgress() {
	g.deadlineMu.Lock()
	g.progressDeadline = time.Now().Add(auxSeconds(g.noProgressTimeout))
	g.deadlineMu.Unlock()
}

// Hard ceiling, first token or mid stream stall wording of the timeout
func (g *auxCodexStreamGuard) timeoutMessage() string {
	elapsed := time.Since(g.start).Seconds()
	if !time.Now().Before(g.hardDeadline) {
		return fmt.Sprintf("Codex auxiliary Responses stream exceeded %.1fs hard ceiling", g.hardDeadline.Sub(g.start).Seconds())
	}
	if !g.sawContent.Load() {
		return fmt.Sprintf("Codex auxiliary Responses stream produced no output within %.1fs (no-progress timeout, %.1fs elapsed)",
			g.noProgressTimeout, elapsed)
	}
	return fmt.Sprintf("Codex auxiliary Responses %s: no new output for %.1fs (%.1fs elapsed)", CodexStreamStallMarker, g.noProgressTimeout, elapsed)
}

// Closes or shuts down the client once the timeout wins, waking only the attempt stream after an owner cancel
func (g *auxCodexStreamGuard) closeClientOnTimeout(owner bool) {
	var timeoutWon bool
	if d, ok := g.protectedCancelCheck.(interface{ BeginTimeoutCleanup() bool }); ok {
		timeoutWon = d.BeginTimeoutCleanup()
	} else {
		timeoutWon = !g.cancelRequested()
	}
	g.timedOut.Store(true)
	if !timeoutWon {
		g.closeAttemptStream("cancelled attempt stream close during timeout failed", owner)
		return
	}
	g.timeoutReleasePending.Store(true)
	if owner {
		auxCloseQuietly(g.client, "client close during timeout failed")
	} else {
		if err := auxRecoverRaise(func() {
			count := forceCloseTCPSockets(g.client)
			slog.Info(fmt.Sprintf("Codex auxiliary client aborted (timeout, tcp_force_closed=%d, deferred_close=stranger_thread)", count))
		}); err != nil {
			slog.Debug("Codex auxiliary: client abort during timeout failed", "error", err)
		}
		g.closeAttemptStream("attempt stream close during stranger-thread timeout failed", false)
	}
	if err := auxRecoverRaise(func() { auxEvictCachedClientInstance(g.client) }); err != nil {
		slog.Debug("Codex auxiliary: cache eviction on timeout failed", "error", err)
	}
}

// Timeout once the effective deadline passed, then explicit host cancel, then an unprotected user interrupt
func (g *auxCodexStreamGuard) checkCancelled() error {
	if g.hasTotal && !time.Now().Before(g.effectiveDeadline()) {
		if !g.timedOut.Load() {
			g.closeClientOnTimeout(true)
		}
		return &AuxTimeoutError{Message: g.timeoutMessage()}
	}
	if auxInterruptCancelRequested(g.ctx) {
		return &AuxiliaryExplicitCancellation{}
	}
	if tools.IsInterrupted(g.ctx) && !auxInterruptProtected(g.ctx) {
		return &InterruptedError{Message: "Codex auxiliary Responses stream interrupted"}
	}
	return nil
}

// Reschedules while progress moved the deadline forward, otherwise times the attempt out from the timer goroutine
func (g *auxCodexStreamGuard) watchdogFire() {
	if remaining := time.Until(g.effectiveDeadline()); remaining > 0 {
		if !(g.timedOut.Load() || g.streamFinished.Load()) {
			g.armTimer(remaining)
		}
		return
	}
	g.closeClientOnTimeout(false)
}

// Watchdog timer constructor the fd ownership tests swap for one that never fires
var auxArmTimerFn = time.AfterFunc

func (g *auxCodexStreamGuard) armTimer(delay time.Duration) {
	g.timerMu.Lock()
	g.timer = auxArmTimerFn(delay, g.watchdogFire)
	g.timerMu.Unlock()
}

// Arms the watchdog when a total timeout exists and runs the first cancel check
func (g *auxCodexStreamGuard) begin() error {
	if g.hasTotal && g.totalTimeout != 0 {
		g.armTimer(max(time.Until(g.effectiveDeadline()), 0))
	}
	return g.checkCancelled()
}

// Substantive events re-arm the window and tick progress, other frames only count toward first response timing
func (g *auxCodexStreamGuard) onEvent(event any) error {
	if m, ok := event.(map[string]any); ok && codexEventHasContent(m) {
		g.recordProgress()
		g.sawContent.Store(true)
		auxNotifyAuxProviderResponse(g.ctx)
	} else {
		auxNotifyAuxTimingResponse(g.ctx)
	}
	return g.checkCancelled()
}

// Stops the watchdog and performs the client close a stranger goroutine timeout deferred to the owner
func (g *auxCodexStreamGuard) finish() {
	g.streamFinished.Store(true)
	g.timerMu.Lock()
	if g.timer != nil {
		g.timer.Stop()
	}
	g.timerMu.Unlock()
	if g.timeoutReleasePending.Load() {
		auxCloseQuietly(g.client, "owner-thread close after timeout failed")
	}
}

// Live connections of one pool, each leaving the set when it closes
type auxConnSet struct {
	mu    sync.Mutex
	conns map[net.Conn]struct{}
}

func (s *auxConnSet) add(c net.Conn) {
	s.mu.Lock()
	if s.conns == nil {
		s.conns = map[net.Conn]struct{}{}
	}
	s.conns[c] = struct{}{}
	s.mu.Unlock()
}

func (s *auxConnSet) remove(c net.Conn) {
	s.mu.Lock()
	delete(s.conns, c)
	s.mu.Unlock()
}

func (s *auxConnSet) snapshot() []net.Conn {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]net.Conn, 0, len(s.conns))
	for c := range s.conns {
		out = append(out, c)
	}
	return out
}

// Pooled connection registered in its transport set until closed
type auxPooledConn struct {
	net.Conn
	set  *auxConnSet
	once sync.Once
	cut  atomic.Bool
}

func (c *auxPooledConn) NetConn() net.Conn { return c.Conn }

// Marks the connection cut so every later read fails and the transport never pools it again
func (c *auxPooledConn) cutAttempt() { c.cut.Store(true) }

// Delivers bytes until the attempt is cut, failing every read afterwards
func (c *auxPooledConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if c.cut.Load() {
		return 0, auxErrAttemptCut
	}
	return n, err
}

func (c *auxPooledConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { c.set.remove(c) })
	return err
}

// Connection sets of live tracked transports keyed weakly so a dropped transport leaves the map
var auxPoolConnSets auxConnSetRegistry

type auxConnSetRegistry struct{ m sync.Map }

func (r *auxConnSetRegistry) Store(t *http.Transport, set *auxConnSet) {
	key := weak.Make(t)
	r.m.Store(key, set)
	runtime.AddCleanup(t, func(k weak.Pointer[http.Transport]) { r.m.Delete(k) }, key)
}

func (r *auxConnSetRegistry) Load(t *http.Transport) (*auxConnSet, bool) {
	v, ok := r.m.Load(weak.Make(t))
	if !ok {
		return nil, false
	}
	return v.(*auxConnSet), true
}

// Records every connection the transport dials so a timeout abort can reach its whole pool
func auxTrackPoolConns(t *http.Transport) *http.Transport {
	set := &auxConnSet{}
	dial := t.DialContext
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}
	t.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := dial(ctx, network, address)
		if err != nil {
			return nil, err
		}
		pc := &auxPooledConn{Conn: conn, set: set}
		set.add(pc)
		return pc, nil
	}
	if dialTLS := t.DialTLSContext; dialTLS != nil {
		t.DialTLSContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			conn, err := dialTLS(ctx, network, address)
			if err != nil {
				return nil, err
			}
			pc := &auxPooledConn{Conn: conn, set: set}
			set.add(pc)
			return pc, nil
		}
	}
	auxPoolConnSets.Store(t, set)
	return t
}

// TrackPoolConns tracks the transport's pool and seeds it with conns for tests that sweep a known socket
func TrackPoolConns(t *http.Transport, conns ...net.Conn) *http.Transport {
	tracked := auxTrackPoolConns(t)
	set, _ := auxPoolConnSets.Load(tracked)
	for _, c := range conns {
		set.add(c)
	}
	return tracked
}

// Innermost connection under TLS and pool wrappers, the identity of one socket
func auxRawConn(conn net.Conn) net.Conn {
	for {
		switch c := conn.(type) {
		case *tls.Conn:
			conn = c.NetConn()
		case interface{ NetConn() net.Conn }:
			conn = c.NetConn()
		default:
			return conn
		}
	}
}

// Sockets of the client pools, only the own in flight requests on a process shared pool
func auxPoolSockets(client any) []net.Conn {
	var rts []http.RoundTripper
	switch c := client.(type) {
	case nil:
		return nil
	case *http.Client:
		rts = append(rts, auxClientTransport(c))
	case interface{ poolHTTPClient() *http.Client }:
		if hc := c.poolHTTPClient(); hc != nil {
			rts = append(rts, auxClientTransport(hc))
		}
	case interface{ poolTransports() []http.RoundTripper }:
		rts = append(rts, c.poolTransports()...)
	case http.RoundTripper:
		rts = append(rts, c)
	}
	var pools []http.RoundTripper
	seenPools := map[http.RoundTripper]bool{}
	addPool := func(rt http.RoundTripper) {
		if rt != nil && !seenPools[rt] {
			seenPools[rt] = true
			pools = append(pools, rt)
		}
	}
	for _, rt := range rts {
		if ct, ok := rt.(*keepaliveClientTransport); ok {
			for _, scheme := range []string{"http", "https"} {
				addPool(ct.mounts[scheme])
			}
			continue
		}
		addPool(rt)
	}
	seen := map[net.Conn]bool{}
	var out []net.Conn
	for _, pool := range pools {
		var conns []net.Conn
		switch p := pool.(type) {
		case *sharedTransportView:
			conns = p.inflightConns()
		case *http.Transport:
			if set, ok := auxPoolConnSets.Load(p); ok {
				conns = set.snapshot()
			}
		}
		for _, conn := range conns {
			raw := socketFromCandidate(auxRawConn(conn))
			if raw != nil && !seen[raw] {
				seen[raw] = true
				out = append(out, raw)
			}
		}
	}
	return out
}

// PoolSockets returns the raw sockets of the client pools, only the own in flight requests on a shared pool
func PoolSockets(client any) []net.Conn { return auxPoolSockets(client) }

// Transport of an HTTP client, the default transport when none is set
func auxClientTransport(c *http.Client) http.RoundTripper {
	if c.Transport == nil {
		return http.DefaultTransport
	}
	return c.Transport
}

// Shuts down every pool socket of the client without closing a descriptor and returns the count
func forceCloseTCPSockets(client any) int {
	count := 0
	for _, conn := range auxPoolSockets(client) {
		auxShutdownConn(conn)
		count++
	}
	return count
}

// ForceCloseTCPSockets aborts in flight TCP reads and writes on every pool socket of the client
func ForceCloseTCPSockets(client any) int { return forceCloseTCPSockets(client) }

func (c *auxOpenAIClient) poolHTTPClient() *http.Client { return c.httpClient }

// Route transport and every per connect timeout transport of the client
func (c *chatCompletionsClient) poolTransports() []http.RoundTripper {
	c.transportsMu.Lock()
	defer c.transportsMu.Unlock()
	return auxRouteAndTransports(c.route.Transport, c.transports)
}

// Route transport and every per connect timeout transport of the client
func (c *codexResponsesClient) poolTransports() []http.RoundTripper {
	c.transportsMu.Lock()
	defer c.transportsMu.Unlock()
	return auxRouteAndTransports(c.route.Transport, c.transports)
}

// Route transport and every per connect timeout transport of the client
func (c *anthropicClient) poolTransports() []http.RoundTripper {
	c.transportsMu.Lock()
	defer c.transportsMu.Unlock()
	return auxRouteAndTransports(c.route.Transport, c.transports)
}

// Every per timeout transport of the Gemini native client
func (c *auxGeminiNativeClient) poolTransports() []http.RoundTripper {
	c.mu.Lock()
	defer c.mu.Unlock()
	return auxRouteAndTransports(nil, c.transports)
}

// Route supplied transport first, then the client built transports
func auxRouteAndTransports[K comparable](route http.RoundTripper, transports map[K]*http.Transport) []http.RoundTripper {
	var out []http.RoundTripper
	if route != nil {
		out = append(out, route)
	}
	for _, t := range transports {
		out = append(out, t)
	}
	return out
}

// Guard start seam the stream guard tests swap
var auxCodexGuardBeginFn = (*auxCodexStreamGuard).begin

// Attempt stream whose end or failure after the watchdog fired reads as the guard's timeout
type auxGuardedCodexStream struct {
	AuxChatStream
	guard *auxCodexStreamGuard
}

func (g *auxCodexStreamGuard) wrapStream(stream AuxChatStream) AuxChatStream {
	return &auxGuardedCodexStream{AuxChatStream: stream, guard: g}
}

func (s *auxGuardedCodexStream) Next(ctx context.Context) (map[string]any, error) {
	event, err := s.AuxChatStream.Next(ctx)
	if err != nil && s.guard.timedOut.Load() {
		cause := err
		if errors.Is(err, io.EOF) {
			cause = nil
		}
		return nil, &AuxTimeoutError{Message: s.guard.timeoutMessage(), Err: cause}
	}
	return event, err
}
