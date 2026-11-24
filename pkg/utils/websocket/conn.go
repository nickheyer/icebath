package websocketutil

import (
	"compress/flate"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/gorilla/websocket"
)

// Messages the reader holds for Receive before it stops reading
const messageBuffer = 64

// Longest close reason that fits a control frame beside its two byte code
const maxCloseReason = 123

// Wait for the close frame a failing connection sends before it drops
const failWait = time.Second

// Client connection with a background reader, keepalive pings and writes safe for concurrent use
type Conn struct {
	ws  *websocket.Conn
	lim limits
	// Time of the last bytes from the peer, the keepalive's measure of silence
	act *activity
	// Whether the reader waits for Receive to take a message, time the keepalive does not count as silence
	stalled atomic.Bool

	messages    chan Message
	closing     chan struct{}
	done        chan struct{}
	closingOnce sync.Once
	doneOnce    sync.Once

	// One data message writer at a time, which gorilla/websocket requires; it serializes control frames itself
	writeMu sync.Mutex

	mu         sync.Mutex
	received   bool
	rcvdCode   int
	rcvdReason string
	sentCode   int
	sentReason string
	sentFirst  bool
	end        *CloseError
	local      bool
	seen       bool
}

func newConn(ws *websocket.Conn, lim limits, act *activity) *Conn {
	c := &Conn{ws: ws, lim: lim, act: act, messages: make(chan Message, messageBuffer),
		closing: make(chan struct{}), done: make(chan struct{})}
	ws.SetPingHandler(c.onPing)
	ws.SetCloseHandler(c.onClose)
	go c.readLoop()
	if lim.pingInterval > 0 {
		go c.keepalive()
	}
	return c
}

// Next text or binary message; ctx's error when ctx ends the wait, the connection staying usable, and once the connection has ended its *CloseError, the messages that arrived before it delivered first
func (c *Conn) Receive(ctx context.Context) (Message, error) {
	select {
	case m, ok := <-c.messages:
		return c.take(m, ok)
	default:
	}
	select {
	case m, ok := <-c.messages:
		return c.take(m, ok)
	case <-ctx.Done():
		return Message{}, ctx.Err()
	}
}

// The message, or the end of the connection once the reader has closed the channel
func (c *Conn) take(m Message, ok bool) (Message, error) {
	if ok {
		return m, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seen = true
	if c.end == nil {
		return Message{}, &CloseError{Code: CloseAbnormal}
	}
	return Message{}, c.end
}

// Sends one text message, the *CloseError of the connection once it is closing or closed
func (c *Conn) SendText(text string) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	w, err := c.beginWrite(websocket.TextMessage)
	if err != nil {
		return err
	}
	_, err = io.WriteString(w, text)
	return c.endWrite(w, err)
}

// Sends one binary message, the *CloseError of the connection once it is closing or closed
func (c *Conn) SendBinary(data []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	w, err := c.beginWrite(websocket.BinaryMessage)
	if err != nil {
		return err
	}
	_, err = w.Write(data)
	return c.endWrite(w, err)
}

// Writer of one message under the write lock, its write limit set
func (c *Conn) beginWrite(kind int) (io.WriteCloser, error) {
	if err := c.sendable(); err != nil {
		return nil, err
	}
	var deadline time.Time
	if c.lim.writeTimeout > 0 {
		deadline = time.Now().Add(c.lim.writeTimeout)
	}
	_ = c.ws.SetWriteDeadline(deadline)
	w, err := c.ws.NextWriter(kind)
	if err != nil {
		return nil, c.writeFailed(err)
	}
	return w, nil
}

// Flushes the message, a failed write failing the connection
func (c *Conn) endWrite(w io.WriteCloser, err error) error {
	if closeErr := w.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return c.writeFailed(err)
	}
	return nil
}

// Sends one ping frame without waiting for its pong, the *CloseError of the connection once it is closing or closed; a ping that cannot be written within the control frame wait fails the connection
func (c *Conn) Ping() error {
	if err := c.sendable(); err != nil {
		return err
	}
	if err := c.ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(c.lim.controlWait())); err != nil {
		return c.writeFailed(err)
	}
	return nil
}

// Runs the closing handshake with CloseNormal, waiting for the peer's close frame up to the close timeout; nil once the handshake completed, else the *CloseError of how the connection ended
func (c *Conn) Close() error { return c.CloseWith(context.Background(), CloseNormal, "") }

// Runs the closing handshake with the code and a reason cut to fit a control frame, waiting for the peer's close frame until the close timeout counted from the call runs out or ctx ends, then drops the connection; nil once the handshake completed, else the *CloseError of how the connection ended
func (c *Conn) CloseWith(ctx context.Context, code int, reason string) error {
	if !sendableCloseCode(code) {
		return fmt.Errorf("websocket close code %d cannot be sent", code)
	}
	reason = fitCloseReason(reason)
	c.mu.Lock()
	c.local = true
	send := c.claimLocked(code, reason)
	c.mu.Unlock()
	c.closingOnce.Do(func() { close(c.closing) })
	wait := c.lim.closeTimeout
	if wait < 0 {
		if send {
			c.writeClose(code, reason, time.Now().Add(c.lim.controlWait()))
		}
		c.finish(nil)
		return c.closeResult()
	}
	limit := time.Now().Add(wait)
	if send && ctx.Err() != nil {
		// ctx ended already: one short try so the peer still learns of the close before the connection drops
		c.writeClose(code, reason, time.Now().Add(min(failWait, wait)))
		send = false
	}
	stop := context.AfterFunc(ctx, func() { c.finish(ctx.Err()) })
	defer stop()
	if send {
		c.writeClose(code, reason, limit)
	}
	timer := time.NewTimer(time.Until(limit))
	defer timer.Stop()
	select {
	case <-c.done:
	case <-timer.C:
		c.finish(ErrCloseTimeout)
	}
	return c.closeResult()
}

// Writes this side's close frame, a write that fails ending the connection since the peer cannot answer a frame it never got
func (c *Conn) writeClose(code int, reason string, deadline time.Time) {
	err := c.ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, reason), deadline)
	if err != nil && !errors.Is(err, websocket.ErrCloseSent) {
		c.finish(err)
	}
}

// Whether this side has closed the connection or Receive has reported its end
func (c *Conn) Closed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.local || c.seen
}

// Channel closed once the connection has ended and its network connection is gone
func (c *Conn) Done() <-chan struct{} { return c.done }

// The subprotocol the server chose, empty when it chose none
func (c *Conn) Subprotocol() string { return c.ws.Subprotocol() }

// Nil when the closing handshake completed, else the end of the connection
func (c *Conn) closeResult() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.end == nil || c.end.Received() && c.end.SentCode != 0 {
		return nil
	}
	return c.end
}

// Reason as valid UTF-8 cut at a character boundary to fit a close frame
func fitCloseReason(reason string) string {
	reason = strings.ToValidUTF8(reason, "\uFFFD")
	if len(reason) <= maxCloseReason {
		return reason
	}
	cut := maxCloseReason
	for cut > 0 && !utf8.RuneStart(reason[cut]) {
		cut--
	}
	return reason[:cut]
}

// Error a write gets once the connection is closing or closed, nil while it is open
func (c *Conn) sendable() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case c.end != nil:
		return c.end
	case c.sentCode != 0:
		return c.closeErrorLocked(nil)
	}
	return nil
}

// Error of a failed write: the closing state when a close frame went out meanwhile, else the end of the connection the failure caused
func (c *Conn) writeFailed(err error) error {
	if !errors.Is(err, websocket.ErrCloseSent) {
		c.finish(err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.end != nil {
		return c.end
	}
	return c.closeErrorLocked(nil)
}

// End of the connection as it stands, the cause kept only while no close frame has arrived
func (c *Conn) closeErrorLocked(cause error) *CloseError {
	e := &CloseError{Code: CloseAbnormal, SentCode: c.sentCode, SentReason: c.sentReason, SentFirst: c.sentFirst, Err: cause}
	if c.received {
		e.Code, e.Reason, e.Err = c.rcvdCode, c.rcvdReason, nil
	}
	return e
}

// Records this side's close frame as going out, false when one went out already or the connection ended; c.mu held
func (c *Conn) claimLocked(code int, reason string) bool {
	if c.end != nil || c.sentCode != 0 {
		return false
	}
	c.sentCode, c.sentReason, c.sentFirst = code, reason, !c.received
	return true
}

// Records the end once, drops the network connection and releases everything waiting on the end
func (c *Conn) finish(cause error) {
	c.mu.Lock()
	if c.end == nil {
		c.end = c.closeErrorLocked(cause)
	}
	c.mu.Unlock()
	_ = c.ws.Close()
	c.doneOnce.Do(func() { close(c.done) })
}

// Sends a close frame with the code unless one went out already, then ends the connection with the cause
func (c *Conn) fail(code int, reason string, cause error) {
	c.mu.Lock()
	send := c.claimLocked(code, reason)
	c.mu.Unlock()
	if send {
		_ = c.ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, reason), time.Now().Add(failWait))
	}
	c.finish(cause)
}

// Reads messages into the channel until the connection ends, then closes it
func (c *Conn) readLoop() {
	defer close(c.messages)
	for {
		kind, data, fits, err := c.readMessage()
		switch {
		case err != nil:
			c.readFailed(err)
			return
		case !fits:
			c.fail(CloseMessageTooBig, "", fmt.Errorf("%w of %d bytes", ErrMessageTooBig, c.lim.maxMessageSize))
			return
		case kind == websocket.TextMessage && !utf8.Valid(data):
			c.fail(CloseInvalidPayload, "invalid UTF-8 in a text message", ErrInvalidUTF8)
			return
		}
		if !c.deliver(Message{Type: MessageType(kind), Data: data}) {
			return
		}
	}
}

// Next data message, read decompressed up to one byte past the size limit so a bigger one is caught without being held whole; false when it runs past the limit
func (c *Conn) readMessage() (int, []byte, bool, error) {
	kind, r, err := c.ws.NextReader()
	if err != nil {
		return 0, nil, false, err
	}
	limit := c.lim.maxMessageSize
	if limit > 0 && limit < math.MaxInt64 {
		r = io.LimitReader(r, limit+1)
	}
	data, err := io.ReadAll(r)
	return kind, data, limit <= 0 || int64(len(data)) <= limit, err
}

// Queues one message, waiting while the buffer is full unless a close is under way, when the message is dropped; false once the connection has ended
func (c *Conn) deliver(m Message) bool {
	select {
	case c.messages <- m:
		return true
	default:
	}
	c.stalled.Store(true)
	defer c.unstall()
	select {
	case c.messages <- m:
		return true
	case <-c.closing:
		select {
		case c.messages <- m:
		default:
		}
		return true
	case <-c.done:
		return false
	}
}

// Ends a wait for Receive, the silence the keepalive measures starting over from here
func (c *Conn) unstall() {
	c.act.mark()
	c.stalled.Store(false)
}

// Ends the connection on the reader's error, the cause kept only when no close frame arrived, with the close frame a violation by the peer calls for
func (c *Conn) readFailed(err error) {
	c.mu.Lock()
	received := c.received
	c.mu.Unlock()
	var closeErr *websocket.CloseError
	var netErr net.Error
	var corrupt flate.CorruptInputError
	switch {
	case received:
		c.finish(nil)
	case errors.As(err, &closeErr) && closeErr.Code == websocket.CloseAbnormalClosure:
		c.finish(io.ErrUnexpectedEOF)
	case errors.As(err, &closeErr), errors.As(err, &netErr):
		c.finish(err)
	case errors.Is(err, websocket.ErrReadLimit):
		c.fail(CloseMessageTooBig, "", fmt.Errorf("%w: its length does not fit in 63 bits", ErrMessageTooBig))
	case errors.As(err, &corrupt):
		c.fail(CloseProtocolError, "corrupt compressed message", fmt.Errorf("websocket compressed message does not decompress: %w", err))
	case protocolViolation(err):
		c.mu.Lock()
		c.claimLocked(CloseProtocolError, fitCloseReason(strings.TrimPrefix(err.Error(), "websocket: ")))
		c.mu.Unlock()
		c.finish(err)
	default:
		c.finish(err)
	}
}

// Whether a read error is a protocol violation of the peer, which gorilla/websocket answers with a CloseProtocolError frame of its own carrying the error text; it builds those errors with errors.New behind a "websocket: " prefix and exposes no value or type for them, so the prefix is their only mark, its internal error built the same way left out; readFailed tells every typed error apart before this test
func protocolViolation(err error) bool {
	text := err.Error()
	return strings.HasPrefix(text, "websocket: ") && !strings.HasPrefix(text, "websocket: internal error")
}

func (c *Conn) onPing(data string) error {
	_ = c.ws.WriteControl(websocket.PongMessage, []byte(data), time.Now().Add(c.lim.controlWait()))
	return nil
}

// Records the peer's close frame and answers it with the same code unless this side sent its own already
func (c *Conn) onClose(code int, text string) error {
	c.mu.Lock()
	c.received, c.rcvdCode, c.rcvdReason = true, code, text
	echo := c.claimLocked(code, "")
	c.mu.Unlock()
	if echo {
		_ = c.ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, ""), time.Now().Add(c.lim.controlWait()))
	}
	return nil
}
