package tuigateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/state"
)

// Waits of a worker for its flush, of one socket send, and of buffered tokens before a flush
var (
	wsWriteTimeout  = 10 * time.Second
	wsSendDeadline  = 30 * time.Second
	wsTokenCoalesce = 33 * time.Millisecond
)

// High frequency display only events buffered and flushed as one batch
var wsStreamingEventTypes = map[string]bool{"message.delta": true, "reasoning.delta": true, "thinking.delta": true}

func wsLog() *slog.Logger {
	return slog.Default().With("logger", "tui_gateway.ws")
}

// Text a WebSocket frame can carry as UTF-8, each lone surrogate replaced by U+FFFD
func sanitizeWSText(text string) string {
	if text == "" {
		return text
	}
	return state.SanitizeSurrogates(text)
}

// Socket of one WebSocket peer, SendText returning once the frame is handed to the connection
type WSConn interface {
	SendText(ctx context.Context, text string) error
	Close(code int) error
}

// Lock granting its waiters in arrival order, a reserved place keeping its turn until it is taken
type fifoLock struct {
	mu      sync.Mutex
	held    bool
	waiters []chan struct{}
}

// Takes the next place in line, the returned channel closing when that place owns the lock
func (l *fifoLock) reserve() chan struct{} {
	l.mu.Lock()
	defer l.mu.Unlock()
	turn := make(chan struct{})
	if !l.held {
		l.held = true
		close(turn)
		return turn
	}
	l.waiters = append(l.waiters, turn)
	return turn
}

// Blocks until the caller owns the lock
func (l *fifoLock) lock() { <-l.reserve() }

// Hands the lock to the longest waiting place, freeing it when nobody waits
func (l *fifoLock) unlock() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.waiters) == 0 {
		l.held = false
		return
	}
	turn := l.waiters[0]
	l.waiters = l.waiters[1:]
	close(turn)
}

// Number of places queued behind the current owner
func (l *fifoLock) waiting() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.waiters)
}

// Whether some place owns the lock
func (l *fifoLock) locked() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.held
}

// Per connection WebSocket transport, safe to write from any goroutine with batches kept in wire order
type WSTransport struct {
	ws   WSConn
	peer string
	// Identity the upgrade credential verified, nil for the legacy token and stdio
	authIdentity map[string]any
	DesktopLocal bool
	// Served profile home whose skin this peer follows, nil for the launch home
	SkinHome *string

	closed atomic.Bool

	tokenMu         sync.Mutex
	pendingTokens   []string
	tokenFlushTimer *time.Timer
	tokenFlushArmed bool
	abortRequested  bool

	sendLock fifoLock
}

// Transport over a socket, peer naming it in logs and authIdentity carrying the verified login
func NewWSTransport(ws WSConn, peer string, authIdentity map[string]any) *WSTransport {
	if peer == "" {
		peer = "unknown"
	}
	return &WSTransport{ws: ws, peer: peer, authIdentity: authIdentity}
}

// Server verified identity of the upgrade credential, the sole authority for who this peer acts as
func (t *WSTransport) AuthIdentity() map[string]any { return t.authIdentity }

// Buffers a streamed token or sends the frame behind every buffered token, waiting a bounded time for the wire
func (t *WSTransport) Write(obj *pyjson.Object) bool {
	if t.closed.Load() {
		return false
	}
	line := SerializeFrame(obj, t.peer, wsLog())
	if params, ok := obj.GetOr("params", nil).(*pyjson.Object); ok {
		if kind, _ := params.GetOr("type", nil).(string); wsStreamingEventTypes[kind] {
			t.tokenMu.Lock()
			t.pendingTokens = append(t.pendingTokens, line)
			if !t.tokenFlushArmed {
				t.tokenFlushArmed = true
				t.armTokenFlush()
			}
			t.tokenMu.Unlock()
			return !t.closed.Load()
		}
	}
	t.tokenMu.Lock()
	batch := append(t.pendingTokens, line)
	t.pendingTokens = nil
	turn := t.sendLock.reserve()
	t.tokenMu.Unlock()
	done := make(chan any, 1)
	go func() { done <- t.guardedSendMany(turn, batch, nil, 0) }()
	timer := time.NewTimer(wsWriteTimeout)
	defer timer.Stop()
	select {
	case failure := <-done:
		if failure != nil {
			t.closed.Store(true)
			wsLog().Warn(fmt.Sprintf("ws write failed peer=%s error_type=%s error=%v", t.peer, panicTypeName(failure), failure))
			return false
		}
	case <-timer.C:
		wsLog().Warn(fmt.Sprintf("ws write slow (loop stalled >%ss) peer=%s — frame left in flight",
			pyrepr.Float(wsWriteTimeout.Seconds()), t.peer))
	}
	return !t.closed.Load()
}

// Class name of a value a send panicked with
func panicTypeName(failure any) string {
	if err, ok := failure.(error); ok {
		return plugins.ErrorTypeName(err)
	}
	return pyrepr.TypeName(failure)
}

// Starts the coalescing timer of the token buffer unless the transport closed, the token lock held
func (t *WSTransport) armTokenFlush() {
	if !t.closed.Load() {
		t.tokenFlushTimer = time.AfterFunc(wsTokenCoalesce, t.flushTokens)
	}
}

// Sends the buffered tokens as one batch whose place in line is taken under the token lock
func (t *WSTransport) flushTokens() {
	t.tokenMu.Lock()
	defer t.tokenMu.Unlock()
	t.tokenFlushTimer = nil
	t.tokenFlushArmed = false
	batch := t.pendingTokens
	t.pendingTokens = nil
	if len(batch) > 0 && !t.closed.Load() {
		turn := t.sendLock.reserve()
		go func() {
			if failure := t.guardedSendMany(turn, batch, nil, 0); failure != nil {
				slog.Default().With("logger", "asyncio").Error(fmt.Sprintf("Task exception was never retrieved peer=%s %s: %v", t.peer, panicTypeName(failure), failure))
			}
		}()
	}
}

// Whether the transport latched closed
func (t *WSTransport) Closed() bool { return t.closed.Load() }

// Socket this transport writes to, what sign out tracks a browser tab by
func (t *WSTransport) WS() WSConn { return t.ws }

// Sends buffered tokens and the frame as one batch and returns once it is on the wire
func (t *WSTransport) WriteAsync(obj *pyjson.Object) bool {
	if t.closed.Load() {
		return false
	}
	t.tokenMu.Lock()
	batch := append(t.pendingTokens, SerializeFrame(obj, t.peer, wsLog()))
	t.pendingTokens = nil
	turn := t.sendLock.reserve()
	t.tokenMu.Unlock()
	t.safeSendMany(turn, batch, nil, 0)
	return !t.closed.Load()
}

// Sends buffered tokens then the frames in order as one batch and returns once it is on the wire
func (t *WSTransport) WriteManyAsync(frames []*pyjson.Object) bool {
	return t.WriteManyIfCurrentAsync(frames, nil)
}

// Sends a batch whose own frames stop once isCurrent turns false, tokens of other producers going out regardless
func (t *WSTransport) WriteManyIfCurrentAsync(frames []*pyjson.Object, isCurrent func() bool) bool {
	if t.closed.Load() {
		return false
	}
	t.tokenMu.Lock()
	batch := t.pendingTokens
	t.pendingTokens = nil
	unguardedPrefix := len(batch)
	for _, frame := range frames {
		batch = append(batch, SerializeFrame(frame, t.peer, wsLog()))
	}
	turn := t.sendLock.reserve()
	t.tokenMu.Unlock()
	t.safeSendMany(turn, batch, isCurrent, unguardedPrefix)
	return !t.closed.Load()
}

// Runs one batch send and returns the value it panicked with, nil when it returned
func (t *WSTransport) guardedSendMany(turn chan struct{}, lines []string, isCurrent func() bool, unguardedPrefix int) (failure any) {
	defer func() { failure = recover() }()
	t.safeSendMany(turn, lines, isCurrent, unguardedPrefix)
	return nil
}

// Whether a send error is the timeout class, a deadline of the context or of the socket
func isSendTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// Hands one frame to the socket, timedOut set when the send outlived the deadline
func (t *WSTransport) sendText(payload string) (err error, timedOut bool) {
	ctx, cancel := context.WithTimeout(context.Background(), wsSendDeadline)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				if asErr, ok := r.(error); ok {
					done <- asErr
					return
				}
				done <- fmt.Errorf("%v", r)
			}
		}()
		done <- t.ws.SendText(ctx, payload)
	}()
	select {
	case err = <-done:
		return err, err != nil && isSendTimeout(err)
	case <-ctx.Done():
		return nil, true
	}
}

// Sends one indivisible batch of serialized frames in wire order once its turn at the socket comes
func (t *WSTransport) safeSendMany(turn chan struct{}, lines []string, isCurrent func() bool, unguardedPrefix int) {
	<-turn
	defer t.sendLock.unlock()
	if t.closed.Load() {
		return
	}
	for index, line := range lines {
		if t.closed.Load() {
			return
		}
		if isCurrent != nil && index >= unguardedPrefix && !isCurrent() {
			return
		}
		err, timedOut := t.sendText(sanitizeWSText(line))
		if timedOut {
			t.closed.Store(true)
			wsLog().Warn(fmt.Sprintf("ws send deadline exceeded (socket stalled, loop responsive) peer=%s deadline=%ss — closing",
				t.peer, pyrepr.Float(wsSendDeadline.Seconds())))
			go t.closeSocket(1011, "send deadline")
			return
		}
		if err == nil {
			continue
		}
		var encode *pystr.UnicodeEncodeError
		if errors.As(err, &encode) {
			wsLog().Warn(fmt.Sprintf("ws send skipped invalid utf-8 frame peer=%s error=%s", t.peer, err))
			continue
		}
		t.closed.Store(true)
		wsLog().Warn(fmt.Sprintf("ws send failed peer=%s error_type=%s error=%s", t.peer, plugins.ErrorTypeName(err), err))
		return
	}
}

// Latches closed and cancels the pending token flush
func (t *WSTransport) Close() {
	t.closed.Store(true)
	t.tokenMu.Lock()
	defer t.tokenMu.Unlock()
	if t.tokenFlushTimer != nil {
		t.tokenFlushTimer.Stop()
		t.tokenFlushTimer = nil
	}
}

// Closes from any goroutine and drops the socket with 1011 once so the client reconnects and replays
func (t *WSTransport) Abort() {
	t.closed.Store(true)
	t.tokenMu.Lock()
	if t.abortRequested {
		t.tokenMu.Unlock()
		return
	}
	t.abortRequested = true
	t.tokenMu.Unlock()
	t.Close()
	go t.closeSocket(1011, "fanout overflow")
}

// Closes the peer socket so the read loop of its connection ends and the disconnect teardown runs
func (t *WSTransport) closeSocket(code int, reason string) {
	defer func() {
		if r := recover(); r != nil {
			wsLog().Debug(fmt.Sprintf("ws close after %s failed peer=%s error=%v", reason, t.peer, r))
		}
	}()
	if err := t.ws.Close(code); err != nil {
		wsLog().Debug(fmt.Sprintf("ws close after %s failed peer=%s error=%s", reason, t.peer, err))
	}
}
