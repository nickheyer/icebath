package tuigateway

import (
	"bytes"
	"encoding/binary"
	"io"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/internal/pywebsockets"
)

// Keepalive timings, close timeout and receive high water mark of the websockets sync client defaults
const (
	publisherPingInterval = 20.0
	publisherPingTimeout  = 20 * time.Second
	publisherCloseTimeout = 10 * time.Second
	publisherMaxQueue     = 16
)

// Ping awaiting its pong with the monotonic time it went out
type publisherPing struct {
	data   []byte
	sentAt float64
	pong   chan struct{}
}

// Client socket reading frames, answering pings and sending keepalive pings like the websockets sync client
type publisherConn struct {
	conn         *websocket.Conn
	mu           sync.Mutex
	open         bool
	failed       bool
	rcvd, sent   *pywebsockets.Close
	rcvdThenSent bool
	pings        []*publisherPing
	latency      float64
	readerDone   chan struct{}
	release      chan struct{}
	releaseOnce  sync.Once
}

// Connection over a dialed socket with its frame reader and keepalive pinger running
func newPublisherConn(conn *websocket.Conn) *publisherConn {
	c := &publisherConn{conn: conn, open: true, readerDone: make(chan struct{}), release: make(chan struct{})}
	conn.SetPongHandler(func(data string) error {
		c.acknowledgePings([]byte(data))
		return nil
	})
	conn.SetCloseHandler(c.closeReceived)
	go c.recvEvents()
	go c.keepalive()
	return c
}

// Reads frames until the socket ends, stopping for good once more than the high water mark sit unread
func (c *publisherConn) recvEvents() {
	queued := 0
	for {
		if queued > publisherMaxQueue {
			<-c.release
			break
		}
		_, r, err := c.conn.NextReader()
		if err != nil {
			break
		}
		if _, err := io.Copy(io.Discard, r); err != nil {
			break
		}
		queued++
	}
	c.mu.Lock()
	c.open = false
	for _, p := range c.pings {
		close(p.pong)
	}
	c.pings = nil
	c.mu.Unlock()
	_ = c.conn.Close()
	close(c.readerDone)
}

// Records the peer close frame and echoes it when the connection was still open
func (c *publisherConn) closeReceived(code int, text string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failed {
		return nil
	}
	c.rcvd = &pywebsockets.Close{Code: code, Reason: text}
	if !c.open {
		c.rcvdThenSent = false
		return nil
	}
	_ = c.conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, text), time.Now().Add(publisherCloseTimeout))
	c.sent = c.rcvd
	c.rcvdThenSent = true
	c.open = false
	return nil
}

// Pings every interval less the measured latency and fails the connection when a pong misses the timeout
func (c *publisherConn) keepalive() {
	for {
		c.mu.Lock()
		timer := time.NewTimer(time.Duration((publisherPingInterval - c.latency) * float64(time.Second)))
		c.mu.Unlock()
		select {
		case <-c.readerDone:
			timer.Stop()
			return
		case <-timer.C:
		}
		select {
		case <-c.readerDone:
			return
		default:
		}
		pong, ok := c.ping()
		if !ok {
			return
		}
		select {
		case <-pong:
		case <-time.After(publisherPingTimeout):
			c.fail(websocket.CloseInternalServerErr, "keepalive ping timeout")
			return
		}
	}
}

// Sends a ping with a fresh random payload, false once the connection left the open state
func (c *publisherConn) ping() (chan struct{}, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.open {
		return nil, false
	}
	data := make([]byte, 4)
	for taken := true; taken; {
		binary.BigEndian.PutUint32(data, rand.Uint32())
		taken = false
		for _, p := range c.pings {
			taken = taken || bytes.Equal(p.data, data)
		}
	}
	if err := c.conn.WriteControl(websocket.PingMessage, data, time.Now().Add(publisherCloseTimeout)); err != nil {
		_ = c.conn.Close()
		return nil, false
	}
	p := &publisherPing{data: data, sentAt: pythread.Monotonic(), pong: make(chan struct{})}
	c.pings = append(c.pings, p)
	return p.pong, true
}

// Settles every ping up to the one a pong answers and measures latency from it
func (c *publisherConn) acknowledgePings(data []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failed {
		return
	}
	now := pythread.Monotonic()
	for i, p := range c.pings {
		if !bytes.Equal(p.data, data) {
			continue
		}
		for _, settled := range c.pings[:i+1] {
			close(settled.pong)
		}
		c.latency = now - p.sentAt
		c.pings = append([]*publisherPing(nil), c.pings[i+1:]...)
		return
	}
}

// Sends the failure close frame, ignores later frames and waits for the server to drop the socket
func (c *publisherConn) fail(code int, reason string) {
	c.mu.Lock()
	if c.open {
		c.sent = &pywebsockets.Close{Code: code, Reason: reason}
		_ = c.conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, reason), time.Now().Add(publisherCloseTimeout))
		c.open = false
	}
	c.failed = true
	c.mu.Unlock()
	c.awaitClosed()
}

// Starts the closing handshake with a normal closure and waits for it within the close timeout
func (c *publisherConn) close() {
	c.mu.Lock()
	if c.open {
		c.sent = &pywebsockets.Close{Code: websocket.CloseNormalClosure}
		_ = c.conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(publisherCloseTimeout))
		c.open = false
	}
	c.mu.Unlock()
	c.awaitClosed()
}

// Waits for the frame reader to end, dropping the socket and a paused reader after the close timeout
func (c *publisherConn) awaitClosed() {
	select {
	case <-c.readerDone:
	case <-time.After(publisherCloseTimeout):
		_ = c.conn.Close()
		c.releaseOnce.Do(func() { close(c.release) })
		<-c.readerDone
	}
}

// Sends one text frame, the ConnectionClosed error once the connection left the open state
func (c *publisherConn) send(text string) error {
	c.mu.Lock()
	open := c.open
	c.mu.Unlock()
	if open {
		if err := c.conn.WriteMessage(websocket.TextMessage, []byte(text)); err == nil {
			return nil
		}
		_ = c.conn.Close()
	}
	c.awaitClosed()
	c.mu.Lock()
	defer c.mu.Unlock()
	return &pywebsockets.ConnectionClosed{Rcvd: c.rcvd, Sent: c.sent, RcvdThenSent: c.rcvdThenSent}
}
