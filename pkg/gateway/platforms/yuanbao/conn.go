package yuanbao

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"github.com/nickheyer/icebath/pkg/gateway"
)

const (
	heartbeatInterval    = 30 * time.Second
	pongTimeout          = 10 * time.Second
	heartbeatMissLimit   = 2
	connectTimeout       = 15 * time.Second
	authTimeout          = 10 * time.Second
	requestTimeout       = 30 * time.Second
	writeTimeout         = 10 * time.Second
	closeTimeout         = time.Second
	maxReconnectAttempts = 100
	debounceWindow       = 1500 * time.Millisecond
)

// Close codes for bans, account removal and same user conflicts that must not be redialed
var noReconnectCloseCodes = map[int]bool{4012: true, 4013: true, 4014: true, 4018: true, 4019: true, 4021: true}

var errRequestTimeout = fmt.Errorf("request timeout after %s", requestTimeout)

// Reply to one outbound request, err set when the connection dropped first
type connReply struct {
	head connHead
	data []byte
	err  error
}

// Frames of one sender buffered until the debounce window passes
type frameBuffer struct {
	frames [][]byte
	timer  *time.Timer
}

// Websocket session with the Yuanbao gateway
type connection struct {
	a *Adapter

	mu         sync.Mutex
	ws         *websocket.Conn
	gen        uint64
	connectID  string
	pending    map[string]chan connReply
	pong       chan struct{}
	loopCancel context.CancelFunc

	writeMu      sync.Mutex
	reconnecting atomic.Bool
	unknownSeq   atomic.Uint64

	bufMu   sync.Mutex
	buffers map[string]*frameBuffer
}

func newConnection(a *Adapter) *connection {
	return &connection{a: a, pending: map[string]chan connReply{}, buffers: map[string]*frameBuffer{}}
}

func (c *connection) connected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ws != nil
}

func (c *connection) currentConnectID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connectID
}

// Sign token, dial, AUTH_BIND and bind, the first connect of Connect
func (c *connection) open(ctx context.Context) error {
	a := c.a
	token, err := getSignToken(ctx, a.appKey, a.appSecret, a.apiDomain, a.routeEnv)
	if err != nil {
		return err
	}
	slog.Info(fmt.Sprintf("[Yuanbao] Connecting to %s", a.wsURL))
	ws, connectID, err := c.dial(ctx, token)
	if err != nil {
		return err
	}
	c.install(ws, connectID)
	a.markBound(ctx)
	return nil
}

// Websocket opened and authenticated with the token, the token bot id adopted first
func (c *connection) dial(ctx context.Context, token signToken) (*websocket.Conn, string, error) {
	a := c.a
	if token.BotID != "" {
		a.setBotID(token.BotID)
	}
	dialer := websocket.Dialer{HandshakeTimeout: connectTimeout,
		Proxy:          gateway.TrustEnvHTTPClient(ctx, 0).Transport.(*http.Transport).Proxy,
		NetDialContext: (&net.Dialer{Timeout: connectTimeout, FallbackDelay: 250 * time.Millisecond}).DialContext}
	dctx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	ws, resp, err := dialer.DialContext(dctx, a.wsURL, nil)
	if resp != nil && resp.Body != nil {
		resp.Body.Close()
	}
	if err != nil {
		return nil, "", fmt.Errorf("websocket dial: %w", err)
	}
	connectID, err := c.authenticate(ws, token)
	if err != nil {
		closeSocket(ws)
		return nil, "", err
	}
	return ws, connectID, nil
}

// Sends AUTH_BIND and reads frames until the bind acknowledgement carrying the connect id
func (c *connection) authenticate(ws *websocket.Conn, token signToken) (string, error) {
	a := c.a
	uid := a.currentBotID()
	if uid == "" {
		uid = token.BotID
	}
	source := token.Source
	if source == "" {
		source = "bot"
	}
	version := baseVersion()
	msgID := uuid.NewString()
	_ = ws.SetWriteDeadline(time.Now().Add(writeTimeout))
	if err := ws.WriteMessage(websocket.BinaryMessage, encodeAuthBind(uid, source, token.Token, msgID, version,
		operatingSystem(), version, a.routeEnv)); err != nil {
		return "", fmt.Errorf("AUTH_BIND send: %w", err)
	}
	slog.Debug(fmt.Sprintf("[Yuanbao] AUTH_BIND sent (msg_id=%s uid=%s)", msgID, uid))
	_ = ws.SetReadDeadline(time.Now().Add(authTimeout))
	defer ws.SetReadDeadline(time.Time{})
	for {
		mt, raw, err := ws.ReadMessage()
		if err != nil {
			return "", fmt.Errorf("AUTH_BIND waiting for BIND_ACK: %w", err)
		}
		if mt != websocket.BinaryMessage {
			continue
		}
		head, data, err := decodeConnMsg(raw)
		if err != nil || head.CmdType != cmdTypeResponse || head.Cmd != cmdAuthBind {
			continue
		}
		status, connectID, err := decodeAuthBindRsp(data)
		if err != nil {
			return "", fmt.Errorf("AuthBindRsp decode: %w", err)
		}
		if status.Code != codeAlreadyAuth && (head.Status != 0 || status.Code != 0) {
			return "", fmt.Errorf("AuthBindRsp error: status=%d code=%d message=%q", head.Status, status.Code, status.Message)
		}
		if connectID == "" {
			return "", errors.New("BIND_ACK missing connectId")
		}
		slog.Info(fmt.Sprintf("[Yuanbao] BIND_ACK received: connectId=%s", connectID))
		return connectID, nil
	}
}

// Makes the socket current, bumping the generation the loops are tied to
func (c *connection) install(ws *websocket.Conn, connectID string) {
	c.mu.Lock()
	c.ws, c.connectID = ws, connectID
	c.gen++
	c.mu.Unlock()
}

// Restarts the heartbeat and receive loops for the current socket
func (c *connection) startLoops() {
	ctx, cancel := context.WithCancel(c.a.backgroundCtx())
	c.mu.Lock()
	if c.loopCancel != nil {
		c.loopCancel()
	}
	c.loopCancel = cancel
	ws, gen := c.ws, c.gen
	c.mu.Unlock()
	go c.heartbeatLoop(ctx, gen)
	go c.receiveLoop(ctx, ws, gen)
}

func (c *connection) isCurrent(gen uint64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.gen == gen && c.ws != nil
}

// Close handshake bounded to one second, then the transport closed
func closeSocket(ws *websocket.Conn) {
	_ = ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
		time.Now().Add(closeTimeout))
	_ = ws.Close()
}

// Drops the current socket and its loops
func (c *connection) dropSocket() {
	c.mu.Lock()
	ws := c.ws
	c.ws = nil
	if c.loopCancel != nil {
		c.loopCancel()
		c.loopCancel = nil
	}
	c.mu.Unlock()
	if ws != nil {
		closeSocket(ws)
	}
}

// Fails pending requests, stops debounce timers and closes the socket
func (c *connection) close() {
	c.dropSocket()
	c.mu.Lock()
	for id, ch := range c.pending {
		select {
		case ch <- connReply{err: errors.New("Yuanbao adapter disconnected")}:
		default:
		}
		delete(c.pending, id)
	}
	c.pong = nil
	c.mu.Unlock()
	c.bufMu.Lock()
	for key, b := range c.buffers {
		b.timer.Stop()
		delete(c.buffers, key)
	}
	c.bufMu.Unlock()
}

// Writes one binary frame on the current socket
func (c *connection) write(frame []byte) error {
	c.mu.Lock()
	ws := c.ws
	c.mu.Unlock()
	if ws == nil {
		return errNotConnected
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_ = ws.SetWriteDeadline(time.Now().Add(writeTimeout))
	return ws.WriteMessage(websocket.BinaryMessage, frame)
}

// Sends a business request and waits for the response matched by request id
func (c *connection) request(ctx context.Context, frame []byte, reqID string) (connReply, error) {
	ch := make(chan connReply, 1)
	c.mu.Lock()
	c.pending[reqID] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, reqID)
		c.mu.Unlock()
	}()
	if err := c.write(frame); err != nil {
		return connReply{}, err
	}
	timer := time.NewTimer(requestTimeout)
	defer timer.Stop()
	select {
	case r := <-ch:
		return r, r.err
	case <-timer.C:
		return connReply{}, errRequestTimeout
	case <-ctx.Done():
		return connReply{}, ctx.Err()
	}
}

// Payload of a group query response, an error for a timeout or a failed head status
func (c *connection) bizQuery(ctx context.Context, label, groupCode string, frame []byte, reqID string) ([]byte, error) {
	if !c.connected() {
		return nil, errNotConnected
	}
	reply, err := c.request(ctx, frame, reqID)
	if errors.Is(err, errRequestTimeout) {
		slog.Warn(fmt.Sprintf("[Yuanbao] %s timeout: group=%s", label, groupCode))
		return nil, fmt.Errorf("%s timed out for group %s", label, groupCode)
	}
	if err != nil {
		slog.Warn(fmt.Sprintf("[Yuanbao] %s failed: %v", label, err))
		return nil, err
	}
	if reply.head.Status != 0 {
		slog.Warn(fmt.Sprintf("[Yuanbao] %s failed: status=%d", label, reply.head.Status))
		return nil, fmt.Errorf("%s failed: status=%d", label, reply.head.Status)
	}
	return reply.data, nil
}

// Sends PING every 30 seconds and reconnects after two missed PONGs in a row
func (c *connection) heartbeatLoop(ctx context.Context, gen uint64) {
	misses := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(heartbeatInterval):
		}
		if !c.isCurrent(gen) {
			return
		}
		pong := make(chan struct{}, 1)
		c.mu.Lock()
		c.pong = pong
		c.mu.Unlock()
		msgID := uuid.NewString()
		if err := c.write(encodePing(msgID)); err != nil {
			slog.Debug(fmt.Sprintf("[Yuanbao] Heartbeat send failed: %v", err))
			continue
		}
		slog.Debug(fmt.Sprintf("[Yuanbao] PING sent (msg_id=%s)", msgID))
		select {
		case <-pong:
			misses = 0
		case <-time.After(pongTimeout):
			misses++
			slog.Warn(fmt.Sprintf("[Yuanbao] PONG timeout (%d/%d)", misses, heartbeatMissLimit))
		case <-ctx.Done():
			return
		}
		c.mu.Lock()
		if c.pong == pong {
			c.pong = nil
		}
		c.mu.Unlock()
		if misses >= heartbeatMissLimit {
			slog.Warn("[Yuanbao] Heartbeat threshold exceeded, triggering reconnect")
			c.scheduleReconnect()
			return
		}
	}
}

// Reads frames until the socket closes, then redials unless the close code is permanent
func (c *connection) receiveLoop(ctx context.Context, ws *websocket.Conn, gen uint64) {
	for {
		mt, raw, err := ws.ReadMessage()
		if err != nil {
			if ctx.Err() != nil || !c.isCurrent(gen) {
				return
			}
			var ce *websocket.CloseError
			if errors.As(err, &ce) {
				slog.Warn(fmt.Sprintf("[Yuanbao] WebSocket connection closed: code=%d reason=%s", ce.Code, ce.Text))
				if noReconnectCloseCodes[ce.Code] {
					slog.Error(fmt.Sprintf("[Yuanbao] Close code %d is non-recoverable, NOT reconnecting", ce.Code))
					bg := c.a.backgroundCtx()
					c.dropSocket()
					c.a.MarkDisconnected(bg)
					return
				}
			} else {
				slog.Warn(fmt.Sprintf("[Yuanbao] receive loop exited: %v", err))
			}
			c.scheduleReconnect()
			return
		}
		if mt == websocket.BinaryMessage {
			c.handleFrame(raw)
		}
	}
}

// Routes one frame to a waiting request, the heartbeat, a push ack or the inbound pipeline
func (c *connection) handleFrame(raw []byte) {
	head, data, err := decodeConnMsg(raw)
	if err != nil {
		slog.Debug(fmt.Sprintf("[Yuanbao] Failed to decode frame: %v", err))
		return
	}
	switch head.CmdType {
	case cmdTypeResponse:
		switch {
		case head.Cmd == cmdPing:
			c.mu.Lock()
			pong := c.pong
			c.mu.Unlock()
			if pong != nil {
				select {
				case pong <- struct{}{}:
				default:
				}
			}
		case head.Cmd == "send_group_heartbeat" || head.Cmd == "send_private_heartbeat":
			slog.Debug(fmt.Sprintf("[Yuanbao] Heartbeat ACK received: cmd=%s msg_id=%s", head.Cmd, head.MsgID))
		default:
			if !c.resolve(head, data) {
				slog.Debug(fmt.Sprintf("[Yuanbao] Unmatched Response: cmd=%s msg_id=%s", head.Cmd, head.MsgID))
			}
		}
	case cmdTypePush:
		slog.Info(fmt.Sprintf("[Yuanbao] Push received: cmd=%s msg_id=%s data_len=%d", head.Cmd, head.MsgID, len(data)))
		if head.NeedAck {
			if err := c.write(encodePushAck(head)); err != nil {
				slog.Debug(fmt.Sprintf("[Yuanbao] Failed to send PushAck: %v", err))
			}
		}
		if head.Cmd == cmdKickout {
			k, err := decodeKickout(data)
			if err != nil {
				slog.Warn(fmt.Sprintf("[Yuanbao] Kicked out, notice undecodable: %v", err))
				return
			}
			slog.Warn(fmt.Sprintf("[Yuanbao] Kicked out: status=%d reason=%s other_device=%s", k.Status, k.Reason, k.OtherDeviceName))
			return
		}
		if c.resolve(head, data) {
			return
		}
		if len(data) > 0 {
			c.bufferInbound(data)
		}
	default:
		slog.Debug(fmt.Sprintf("[Yuanbao] Ignoring frame: cmd_type=%d cmd=%s msg_id=%s", head.CmdType, head.Cmd, head.MsgID))
	}
}

// Hands a frame to the request waiting on its msg id
func (c *connection) resolve(head connHead, data []byte) bool {
	if head.MsgID == "" {
		return false
	}
	c.mu.Lock()
	ch, ok := c.pending[head.MsgID]
	delete(c.pending, head.MsgID)
	c.mu.Unlock()
	if ok {
		ch <- connReply{head: head, data: data}
	}
	return ok
}

// Debounce key of the sender and group of a frame, unique when neither decodes
func (c *connection) senderKey(data []byte) string {
	var raw map[string]any
	if json.Unmarshal(data, &raw) == nil && raw != nil {
		from, group := jsonPushSender(raw)
		if from != "" {
			return from + ":" + group
		}
	} else if p, err := decodeInboundPush(data); err == nil {
		return p.FromAccount + ":" + p.GroupCode
	}
	return "__unknown_" + strconv.FormatUint(c.unknownSeq.Add(1), 10)
}

// Buffers a push so frames one sender sends within the window run through the pipeline together
func (c *connection) bufferInbound(data []byte) {
	key := c.senderKey(data)
	c.bufMu.Lock()
	defer c.bufMu.Unlock()
	b := c.buffers[key]
	if b == nil {
		b = &frameBuffer{}
		c.buffers[key] = b
	} else {
		b.timer.Stop()
	}
	b.frames = append(b.frames, data)
	b.timer = time.AfterFunc(debounceWindow, func() { c.flush(key, b) })
}

func (c *connection) flush(key string, b *frameBuffer) {
	c.bufMu.Lock()
	if c.buffers[key] != b {
		c.bufMu.Unlock()
		return
	}
	delete(c.buffers, key)
	frames := b.frames
	c.bufMu.Unlock()
	slog.Info(fmt.Sprintf("[Yuanbao] Debounce flush: key=%s, aggregated %d frames", key, len(frames)))
	c.a.spawn(func(ctx context.Context) error {
		c.a.runInbound(ctx, frames)
		return nil
	})
}

// Starts the backoff redial unless the adapter is closing or one is already running
func (c *connection) scheduleReconnect() {
	if c.a.isClosing() || !c.reconnecting.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer c.reconnecting.Store(false)
		c.reconnect()
	}()
}

// Redials with backoff of 1, 2, 4 seconds and so on capped at 60, re-signing the token each attempt
func (c *connection) reconnect() {
	a := c.a
	ctx := a.backgroundCtx()
	for attempt := range maxReconnectAttempts {
		wait := 60 * time.Second
		if attempt < 6 {
			wait = time.Duration(1<<attempt) * time.Second
		}
		slog.Info(fmt.Sprintf("[Yuanbao] Reconnect attempt %d/%d in %s", attempt+1, maxReconnectAttempts, wait))
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return
		}
		c.dropSocket()
		token, err := forceRefreshSignToken(ctx, a.appKey, a.appSecret, a.apiDomain, a.routeEnv)
		if err != nil {
			slog.Warn(fmt.Sprintf("[Yuanbao] Reconnect attempt %d failed: %v", attempt+1, err))
			continue
		}
		ws, connectID, err := c.dial(ctx, token)
		if err != nil {
			slog.Warn(fmt.Sprintf("[Yuanbao] Re-auth failed on attempt %d: %v", attempt+1, err))
			continue
		}
		if a.isClosing() {
			closeSocket(ws)
			return
		}
		c.install(ws, connectID)
		a.markBound(ctx)
		slog.Info(fmt.Sprintf("[Yuanbao] Reconnected on attempt %d. connectId=%s", attempt+1, connectID))
		return
	}
	slog.Error(fmt.Sprintf("[Yuanbao] Giving up after %d reconnect attempts", maxReconnectAttempts))
	a.MarkDisconnected(ctx)
}
