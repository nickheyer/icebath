package tuigateway

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pywebsockets"
)

// Frames a publisher queues before writes are dropped
const publisherQueueMax = 256

func publisherLog() *slog.Logger {
	return slog.Default().With("logger", "tui_gateway.event_publisher")
}

// Back socket to the dashboard that mirrors every emit, never blocking the agent loop on the sidecar
type WsPublisherTransport struct {
	lock   sync.Mutex
	ws     *websocket.Conn
	dead   atomic.Bool
	queue  chan *string
	worker chan struct{}
}

// Publisher dialed within connectTimeout, dead from the start when the dial fails
func NewWsPublisherTransport(url string, connectTimeout time.Duration) *WsPublisherTransport {
	t := &WsPublisherTransport{queue: make(chan *string, publisherQueueMax)}
	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	defer cancel()
	dialer := websocket.Dialer{HandshakeTimeout: connectTimeout}
	conn, resp, err := dialer.DialContext(ctx, url, nil)
	if err != nil {
		publisherLog().Debug(fmt.Sprintf("event publisher connect failed: %s", pywebsockets.HandshakeError(resp, err)))
		t.dead.Store(true)
		return t
	}
	t.ws = conn
	t.worker = make(chan struct{})
	go t.drain()
	return t
}

// Sends queued frames until the nil stop marker, a failed send latching the publisher dead
func (t *WsPublisherTransport) drain() {
	defer close(t.worker)
	for item := range t.queue {
		if item == nil {
			return
		}
		t.lock.Lock()
		ws := t.ws
		var err error
		if ws != nil {
			err = ws.WriteMessage(websocket.TextMessage, []byte(*item))
		}
		if err != nil {
			publisherLog().Debug(fmt.Sprintf("event publisher write failed: %s", err))
			t.dead.Store(true)
			t.ws = nil
		}
		t.lock.Unlock()
	}
}

// Queues the frame as JSON text, false when the publisher is dead or its queue is full
func (t *WsPublisherTransport) Write(obj *pyjson.Object) bool {
	t.lock.Lock()
	live := t.ws != nil
	t.lock.Unlock()
	if t.dead.Load() || !live || t.worker == nil {
		return false
	}
	text, err := pyjson.Dumps(obj, pyjson.EnsureASCII(false))
	if err != nil {
		panic(err)
	}
	select {
	case t.queue <- &text:
		return true
	default:
		return false
	}
}

// Stops the sender within three seconds and closes the socket
func (t *WsPublisherTransport) Close() {
	t.dead.Store(true)
	if worker := t.worker; worker != nil {
		select {
		case <-worker:
		default:
			select {
			case t.queue <- nil:
			default:
			}
			select {
			case <-worker:
			case <-time.After(3 * time.Second):
			}
		}
	}
	t.lock.Lock()
	defer t.lock.Unlock()
	if t.ws == nil {
		return
	}
	func() {
		defer func() { _ = recover() }()
		deadline := time.Now().Add(10 * time.Second)
		if t.ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), deadline) == nil {
			_ = t.ws.SetReadDeadline(deadline)
			for {
				if _, _, err := t.ws.NextReader(); err != nil {
					break
				}
			}
		}
		_ = t.ws.Close()
	}()
	t.ws = nil
}
