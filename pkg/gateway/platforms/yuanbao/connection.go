package yuanbao

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pywebsockets"
)

const (
	pongTimeoutS     = 10.0
	wsCloseHandshake = 5 * time.Second
	happyEyeballs    = 250 * time.Millisecond
	reconnectMaxWait = 60
)

// Socket surface the connection manager drives
type wsConn interface {
	Open() bool
	Recv(ctx context.Context) (any, error)
	SendBytes(message []byte) error
	Close()
}

// Opens the gateway socket, a package var the way hermes tests patch websockets connect
var wsDial = func(ctx context.Context, rawURL string, opts pywebsockets.ConnectOptions) (wsConn, error) {
	conn, err := pywebsockets.Connect(ctx, rawURL, opts)
	if err != nil {
		return nil, err
	}
	return conn, nil
}

// Running count behind the debounce key of a frame that names no sender
var unknownSenderSeq atomic.Int64

// Debounce timer of one sender key, compared by identity when it fires
type debounceTimer struct{ timer *time.Timer }

// connectionManager owns the websocket lifecycle, the auth bind, the ping heartbeat, the receive loop and the reconnects
type connectionManager struct {
	adapter        *YuanbaoAdapter
	debounceWindow float64

	mu                    sync.Mutex
	ws                    wsConn
	connectID             string
	heartbeatTask         *gateway.AdapterTask
	recvTask              *gateway.AdapterTask
	pendingAcks           map[string]*future
	pendingPong           *future
	consecutiveHBTimeouts int
	reconnecting          bool
	inboundBuffer         map[string][][]byte
	inboundTimers         map[string]*debounceTimer
}

func newConnectionManager(adapter *YuanbaoAdapter) *connectionManager {
	return &connectionManager{adapter: adapter, debounceWindow: debounceWindow, pendingAcks: map[string]*future{},
		inboundBuffer: map[string][][]byte{}, inboundTimers: map[string]*debounceTimer{}}
}

// Current socket, nil while disconnected
func (c *connectionManager) socket() wsConn {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ws
}

// Connection id the last auth bind returned
func (c *connectionManager) boundConnectID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connectID
}

// Whether a socket is held and still open
func (c *connectionManager) isConnected() bool {
	ws := c.socket()
	return ws != nil && ws.Open()
}

// Signs a token, dials the socket, binds the bot and starts the loops, false when any step fails
func (c *connectionManager) open(ctx context.Context) (bool, error) {
	adapter := c.adapter
	name := adapter.Name()
	if !websocketsAvailable {
		msg := "Yuanbao startup failed: 'websockets' package not installed"
		adapter.SetFatalError(ctx, "yuanbao_missing_dependency", msg, true)
		slog.Warn(fmt.Sprintf("[%s] %s. Run: icebath pm repair", name, msg))
		return false, nil
	}
	if adapter.appKey == "" || adapter.appSecret == "" {
		msg := "Yuanbao startup failed: YUANBAO_APP_ID and YUANBAO_APP_SECRET are required"
		adapter.SetFatalError(ctx, "yuanbao_missing_credentials", msg, false)
		slog.Error(fmt.Sprintf("[%s] %s", name, msg))
		return false, nil
	}
	if c.isConnected() {
		slog.Debug(fmt.Sprintf("[%s] Already connected, skipping connect()", name))
		return true, nil
	}
	acquired, err := adapter.AcquirePlatformLock(ctx, "yuanbao-app-key", adapter.appKey, "Yuanbao app key")
	if err != nil || !acquired {
		return false, err
	}
	connected, err := func() (bool, error) {
		slog.Info(fmt.Sprintf("[%s] Fetching sign token from %s", name, adapter.apiDomain))
		tokenData, err := adapter.getCachedToken(ctx)
		if err != nil {
			return false, err
		}
		slog.Info(fmt.Sprintf("[%s] Connecting to %s", name, adapter.wsURL))
		dialed, err := c.dial(ctx, tokenData)
		if err != nil || !dialed {
			return false, err
		}
		c.connected(ctx, false)
		slog.Info(fmt.Sprintf("[%s] Connected. connectId=%s botId=%s", name, c.boundConnectID(), pyrepr.Str(adapter.botIDValue())))
		return true, nil
	}()
	switch {
	case err == nil && !connected:
		return false, nil
	case err == nil:
		return true, nil
	case ctx.Err() != nil:
		return false, ctx.Err()
	case isTimeout(err):
		slog.Error(fmt.Sprintf("[%s] Connection timed out", name))
	default:
		slog.Error(fmt.Sprintf("[%s] connect() failed: %s", name, errText(err)), "error", err)
	}
	c.cleanupWS(ctx)
	adapter.ReleasePlatformLock()
	return false, nil
}

// Adopts the bot id of the sign token, opens the socket without keepalive pings and runs the auth bind
func (c *connectionManager) dial(ctx context.Context, tokenData map[string]any) (bool, error) {
	if botID := tokenData["bot_id"]; pyrepr.Truthy(botID) {
		c.adapter.setBotID(pyrepr.Str(botID))
	}
	dctx, cancel := context.WithTimeout(ctx, durationOf(connectTimeoutSeconds))
	ws, err := wsDial(dctx, c.adapter.wsURL, pywebsockets.ConnectOptions{NoKeepalive: true, CloseTimeout: wsCloseHandshake,
		HappyEyeballsDelay: happyEyeballs})
	cancel()
	if err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		return false, err
	}
	c.mu.Lock()
	c.ws = ws
	c.mu.Unlock()
	authenticated, err := c.authenticate(ctx, tokenData)
	if err != nil {
		return false, err
	}
	if !authenticated {
		c.cleanupWS(ctx)
		return false, nil
	}
	return true, nil
}

// Bookkeeping after a successful auth bind, the adapter marked connected, the loops started and the adapter made active
func (c *connectionManager) connected(ctx context.Context, cancelExisting bool) {
	c.adapter.MarkConnected(ctx, "")
	c.startLoops(ctx, cancelExisting)
	SetActive(ctx, c.adapter)
}

// Starts the heartbeat and receive loops of the current socket, cancelling the previous ones on request
func (c *connectionManager) startLoops(ctx context.Context, cancelExisting bool) {
	c.mu.Lock()
	previous := []*gateway.AdapterTask{c.heartbeatTask, c.recvTask}
	ws := c.ws
	c.mu.Unlock()
	if cancelExisting {
		for _, task := range previous {
			if task != nil && !task.Done() {
				task.Cancel()
			}
		}
	}
	heartbeat := gateway.SpawnAdapterTask(ctx, c.heartbeatLoop)
	recv := gateway.SpawnAdapterTask(ctx, func(tctx context.Context) error { return c.receiveLoop(tctx, ws) })
	c.mu.Lock()
	c.heartbeatTask, c.recvTask = heartbeat, recv
	c.mu.Unlock()
}

// Cancels the loops, fails the pending requests and closes the socket
func (c *connectionManager) close(ctx context.Context) {
	c.mu.Lock()
	heartbeat, recv := c.heartbeatTask, c.recvTask
	c.mu.Unlock()
	gateway.CancelTask(ctx, heartbeat)
	gateway.CancelTask(ctx, recv)
	c.mu.Lock()
	c.heartbeatTask, c.recvTask = nil, nil
	pending := c.pendingAcks
	c.pendingAcks = map[string]*future{}
	c.mu.Unlock()
	disconnected := runtimeError("YuanbaoAdapter disconnected")
	for _, fut := range pending {
		fut.setException(disconnected)
	}
	signClearLocks()
	c.cleanupWS(ctx)
}

// Sends the auth bind and reads frames until its ack names a connection id, false on refusal or timeout
func (c *connectionManager) authenticate(ctx context.Context, tokenData map[string]any) (bool, error) {
	adapter := c.adapter
	name := adapter.Name()
	ws := c.socket()
	if ws == nil {
		return false, nil
	}
	uidValue := pyrepr.Or(adapter.botIDValue(), dictGetOr(tokenData, "bot_id", ""))
	msgID := uuid.NewString()
	version, err := baseVersion()
	if err != nil {
		return false, err
	}
	uid, err := encodeText(uidValue)
	if err != nil {
		return false, err
	}
	source, err := encodeText(pyrepr.Or(tokenData["source"], "bot"))
	if err != nil {
		return false, err
	}
	token, err := encodeText(dictGetOr(tokenData, "token", ""))
	if err != nil {
		return false, err
	}
	routeEnv, err := protoText(pyrepr.Or(adapter.routeEnv, dictGetOr(tokenData, "route_env", "")))
	if err != nil {
		return false, err
	}
	frame, err := EncodeAuthBind("ybBot", uid, source, token, msgID, version, operationSystem(), version, routeEnv)
	if err != nil {
		return false, err
	}
	if err := ws.SendBytes(frame); err != nil {
		return false, err
	}
	slog.Debug(fmt.Sprintf("[%s] AUTH_BIND sent (msg_id=%s uid=%s)", name, msgID, uid))
	deadline := monotonic() + authTimeoutSeconds
	for {
		remaining := deadline - monotonic()
		if remaining <= 0 {
			slog.Error(fmt.Sprintf("[%s] AUTH_BIND timeout waiting for BIND_ACK", name))
			return false, nil
		}
		rctx, cancel := context.WithTimeout(ctx, durationOf(remaining))
		raw, err := ws.Recv(rctx)
		timedOut := rctx.Err() != nil
		cancel()
		switch {
		case err != nil && ctx.Err() != nil:
			return false, ctx.Err()
		case err != nil && timedOut:
			slog.Error(fmt.Sprintf("[%s] AUTH_BIND timeout", name))
			return false, nil
		case err != nil:
			slog.Error(fmt.Sprintf("[%s] AUTH_BIND error: %s", name, errText(err)), "error", err)
			return false, nil
		}
		data, isBytes := raw.([]byte)
		if !isBytes {
			continue
		}
		msg, err := DecodeConnMsg(data)
		if err != nil {
			continue
		}
		head, _ := msg.GetOr("head", nil).(*pyjson.Object)
		if head == nil || !pyjson.Equal(head.GetOr("cmd_type", int64(-1)), CmdType["Response"]) || head.GetOr("cmd", "") != Cmd["AuthBind"] {
			continue
		}
		connectID := c.extractConnectID(msg)
		c.mu.Lock()
		c.connectID = connectID
		c.mu.Unlock()
		if connectID == "" {
			slog.Error(fmt.Sprintf("[%s] BIND_ACK missing connectId", name))
			return false, nil
		}
		slog.Info(fmt.Sprintf("[%s] BIND_ACK received: connectId=%s", name, connectID))
		return true, nil
	}
}

// Pending future registered under the message id and not yet settled, removed from the table
func (c *connectionManager) popPending(msgID string) *future {
	if msgID == "" {
		return nil
	}
	c.mu.Lock()
	fut := c.pendingAcks[msgID]
	delete(c.pendingAcks, msgID)
	c.mu.Unlock()
	if fut == nil || fut.isDone() {
		return nil
	}
	return fut
}

// Whether a request still waits under the message id
func (c *connectionManager) hasPending(msgID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, waiting := c.pendingAcks[msgID]
	return waiting
}

// Connection id of a decoded auth bind ack, empty when the ack carries an error code or no id
func (c *connectionManager) extractConnectID(decoded *pyjson.Object) string {
	data, _ := decoded.GetOr("data", []byte{}).([]byte)
	if len(data) == 0 {
		return ""
	}
	name := c.adapter.Name()
	fields, err := parseDict(data)
	if err != nil {
		slog.Warn(fmt.Sprintf("[%s] Failed to extract connectId: %s", name, errText(err)))
		return ""
	}
	if code := fields.GetVarint(1); !equalsInt(code, 0) {
		slog.Error(fmt.Sprintf("[%s] AuthBindRsp error: code=%s message=%s", name, pyrepr.Str(code), pyrepr.StrRepr(fields.GetString(2))))
		return ""
	}
	return fields.GetString(3)
}

// Pings every heartbeat interval while the adapter runs and schedules a reconnect after too many missed pongs
func (c *connectionManager) heartbeatLoop(ctx context.Context) error {
	adapter := c.adapter
	name := adapter.Name()
	for adapter.IsConnected() {
		if err := sleep(ctx, heartbeatIntervalSeconds); err != nil {
			return nil
		}
		ws := c.socket()
		if ws == nil {
			continue
		}
		msgID := uuid.NewString()
		pong := newFuture()
		c.mu.Lock()
		c.pendingPong = pong
		c.pendingAcks[msgID] = pong
		c.mu.Unlock()
		frame, err := EncodePing(msgID)
		if err == nil {
			err = ws.SendBytes(frame)
		}
		if err != nil {
			c.mu.Lock()
			delete(c.pendingAcks, msgID)
			c.pendingPong = nil
			c.mu.Unlock()
			slog.Debug(fmt.Sprintf("[%s] Heartbeat send failed: %s", name, errText(err)))
			continue
		}
		slog.Debug(fmt.Sprintf("[%s] PING sent (msg_id=%s)", name, msgID))
		_, err = pong.wait(ctx, pongTimeoutS)
		c.mu.Lock()
		delete(c.pendingAcks, msgID)
		c.pendingPong = nil
		switch {
		case err == nil:
			c.consecutiveHBTimeouts = 0
		case isTimeout(err):
			c.consecutiveHBTimeouts++
		}
		missed := c.consecutiveHBTimeouts
		c.mu.Unlock()
		switch {
		case err == nil:
		case ctx.Err() != nil:
			return nil
		case isTimeout(err):
			pong.cancel()
			slog.Warn(fmt.Sprintf("[%s] PONG timeout (%d/%d)", name, missed, heartbeatTimeoutThreshold))
			if missed >= heartbeatTimeoutThreshold {
				slog.Warn(fmt.Sprintf("[%s] Heartbeat threshold exceeded, triggering reconnect", name))
				c.scheduleReconnect(ctx)
				return nil
			}
		default:
			slog.Debug(fmt.Sprintf("[%s] Heartbeat send failed: %s", name, errText(err)))
		}
	}
	return nil
}

// Reads the frames of one socket and dispatches them, scheduling a reconnect unless the close code is permanent
func (c *connectionManager) receiveLoop(ctx context.Context, ws wsConn) error {
	adapter := c.adapter
	name := adapter.Name()
	if ws == nil {
		slog.Warn(fmt.Sprintf("[%s] receive_loop exited: 'async for' requires an object with __aiter__ method, got NoneType", name))
		c.scheduleReconnect(ctx)
		return nil
	}
	for {
		raw, err := ws.Recv(ctx)
		if err == nil {
			if data, isBytes := raw.([]byte); isBytes {
				c.handleFrame(ctx, data)
			}
			continue
		}
		var closed *pywebsockets.ConnectionClosed
		switch {
		case ctx.Err() != nil, pywebsockets.IsClosedOK(err):
		case errors.As(err, &closed):
			code, reason := 1006, ""
			if closed.Rcvd != nil {
				code, reason = closed.Rcvd.Code, closed.Rcvd.Reason
			}
			slog.Warn(fmt.Sprintf("[%s] WebSocket connection closed: code=%d reason=%s", name, code, reason))
			if noReconnectCloseCodes[code] {
				slog.Error(fmt.Sprintf("[%s] Close code %d is non-recoverable, NOT reconnecting", name, code))
				adapter.MarkDisconnected(ctx)
			} else {
				c.scheduleReconnect(ctx)
			}
		default:
			slog.Warn(fmt.Sprintf("[%s] receive_loop exited: %s", name, errText(err)))
			c.scheduleReconnect(ctx)
		}
		return nil
	}
}

// Routes one frame, a response to its waiting request and a push to its waiter or the inbound debounce
func (c *connectionManager) handleFrame(ctx context.Context, raw []byte) {
	name := c.adapter.Name()
	msg, err := DecodeConnMsg(raw)
	if err != nil {
		slog.Debug(fmt.Sprintf("[%s] Failed to decode frame: %s", name, errText(err)))
		return
	}
	head, _ := msg.GetOr("head", nil).(*pyjson.Object)
	if head == nil {
		head = pyjson.NewObject()
	}
	cmdType := head.GetOr("cmd_type", int64(-1))
	cmd := pyrepr.Str(head.GetOr("cmd", ""))
	msgID := pyrepr.Str(head.GetOr("msg_id", ""))
	data, _ := msg.GetOr("data", []byte{}).([]byte)
	switch {
	case pyjson.Equal(cmdType, CmdType["Response"]):
		c.handleResponse(name, cmd, msgID, head, data)
	case pyjson.Equal(cmdType, CmdType["Push"]):
		c.handlePush(ctx, name, cmd, msgID, head, data)
	default:
		slog.Debug(fmt.Sprintf("[%s] Ignoring frame: cmd_type=%s cmd=%s msg_id=%s", name, pyrepr.Str(cmdType), cmd, msgID))
	}
}

// Settles the pong of a ping ack and the waiting request of any other response
func (c *connectionManager) handleResponse(name, cmd, msgID string, head *pyjson.Object, data []byte) {
	switch {
	case cmd == Cmd["Ping"]:
		slog.Debug(fmt.Sprintf("[%s] HEARTBEAT_ACK received (msg_id=%s)", name, msgID))
		c.mu.Lock()
		pong := c.pendingPong
		c.mu.Unlock()
		if pong == nil || pong.isDone() {
			pong = c.popPending(msgID)
		}
		if pong != nil {
			pong.setResult(true)
		}
	case cmd == "send_group_heartbeat" || cmd == "send_private_heartbeat":
		slog.Debug(fmt.Sprintf("[%s] Heartbeat ACK received: cmd=%s msg_id=%s", name, cmd, msgID))
	case msgID != "" && c.hasPending(msgID):
		if fut := c.popPending(msgID); fut != nil {
			result := pyjson.ObjectOf("head", head)
			if len(data) > 0 {
				result.Set("data", data)
			}
			fut.setResult(result)
		}
	default:
		slog.Debug(fmt.Sprintf("[%s] Unmatched Response: cmd=%s msg_id=%s", name, cmd, msgID))
	}
}

// Acks a push that asks for it, then settles its waiting request or buffers it as an inbound message
func (c *connectionManager) handlePush(ctx context.Context, name, cmd, msgID string, head *pyjson.Object, data []byte) {
	slog.Info(fmt.Sprintf("[%s] Push received: cmd=%s msg_id=%s data_len=%d", name, cmd, msgID, len(data)))
	if ws := c.socket(); pyrepr.Truthy(head.GetOr("need_ack", false)) && ws != nil {
		frame, err := EncodePushAck(head)
		if err == nil {
			err = ws.SendBytes(frame)
		}
		if err != nil {
			slog.Debug(fmt.Sprintf("[%s] Failed to send PushAck: %s", name, errText(err)))
		}
	}
	if msgID != "" && c.hasPending(msgID) {
		if fut := c.popPending(msgID); fut != nil {
			var result any
			if len(data) == 0 {
				result = pyjson.ObjectOf("head", head)
			} else if push := DecodeInboundPush(data); push != nil {
				result = push
			}
			fut.setResult(result)
		}
		return
	}
	if len(data) > 0 {
		slog.Info(fmt.Sprintf("[%s] WS received inbound push, decoding and dispatching: cmd=%s, data_len=%d", name, cmd, len(data)))
		c.pushToInbound(ctx, data)
	}
}

// Debounce key of a frame, its sender and group from JSON or protobuf, a fresh key when it names neither
func extractSenderKey(raw []byte) string {
	if text, err := pystr.DecodeUTF8(raw); err == nil {
		if parsed, err := pyjson.Loads(text); err == nil {
			if rawJSON, isDict := asDict(parsed); isDict {
				if fromAccount, groupCode := jsonSenderFields(rawJSON); pyrepr.Truthy(fromAccount) {
					return pyrepr.Str(fromAccount) + ":" + pyrepr.Str(groupCode)
				}
			}
		}
	}
	if push := DecodeInboundPush(raw); push != nil && push.Len() > 0 {
		return pyrepr.Str(push.GetOr("from_account", "")) + ":" + pyrepr.Str(push.GetOr("group_code", ""))
	}
	return fmt.Sprintf("__unknown_%d", unknownSenderSeq.Add(1))
}

// Buffers a frame under its sender key and restarts the debounce timer so companion frames run as one pipeline pass
func (c *connectionManager) pushToInbound(ctx context.Context, raw []byte) {
	key := extractSenderKey(raw)
	entry := &debounceTimer{}
	c.mu.Lock()
	if existing := c.inboundTimers[key]; existing != nil {
		existing.timer.Stop()
	}
	c.inboundBuffer[key] = append(c.inboundBuffer[key], raw)
	count := len(c.inboundBuffer[key])
	entry.timer = time.AfterFunc(durationOf(c.debounceWindow), func() { c.flushInboundBuffer(ctx, key, entry) })
	c.inboundTimers[key] = entry
	c.mu.Unlock()
	slog.Debug(fmt.Sprintf("[%s] Debounce: buffered frame for key=%s, count=%d", c.adapter.Name(), key, count))
}

// Runs the inbound pipeline over the frames buffered for a key once its latest debounce timer fires
func (c *connectionManager) flushInboundBuffer(ctx context.Context, key string, fired *debounceTimer) {
	c.mu.Lock()
	if c.inboundTimers[key] != fired {
		c.mu.Unlock()
		return
	}
	delete(c.inboundTimers, key)
	frames := c.inboundBuffer[key]
	delete(c.inboundBuffer, key)
	c.mu.Unlock()
	if len(frames) == 0 {
		return
	}
	adapter := c.adapter
	slog.Info(fmt.Sprintf("[%s] Debounce flush: key=%s, aggregated %d frames", adapter.Name(), key, len(frames)))
	adapter.trackTask(gateway.SpawnAdapterTask(ctx, func(tctx context.Context) error {
		return adapter.inboundPipeline.Execute(tctx, NewInboundContext(adapter, frames))
	}))
}

// Sends a business request and waits for the response registered under its request id
func (c *connectionManager) sendBizRequest(ctx context.Context, encoded []byte, reqID string, timeout float64) (any, error) {
	ws := c.socket()
	if ws == nil {
		return nil, runtimeError("Not connected")
	}
	fut := newFuture()
	c.mu.Lock()
	c.pendingAcks[reqID] = fut
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pendingAcks, reqID)
		c.mu.Unlock()
	}()
	if err := ws.SendBytes(encoded); err != nil {
		return nil, err
	}
	return fut.wait(ctx, timeout)
}

// Starts a reconnect while the adapter runs and none is under way
func (c *connectionManager) scheduleReconnect(ctx context.Context) {
	c.mu.Lock()
	reconnecting := c.reconnecting
	c.mu.Unlock()
	if c.adapter.IsConnected() && !reconnecting {
		gateway.SpawnAdapterTask(ctx, func(tctx context.Context) error {
			_, err := c.reconnectWithBackoff(tctx)
			return err
		})
	}
}

// Runs the reconnect loop unless one is already in progress
func (c *connectionManager) reconnectWithBackoff(ctx context.Context) (bool, error) {
	c.mu.Lock()
	if c.reconnecting {
		c.mu.Unlock()
		slog.Debug(fmt.Sprintf("[%s] Reconnect already in progress, skipping", c.adapter.Name()))
		return false, nil
	}
	c.reconnecting = true
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.reconnecting = false
		c.mu.Unlock()
	}()
	return c.doReconnect(ctx)
}

// Redials with a fresh token after a doubling wait capped at a minute, marking the adapter disconnected on giving up
func (c *connectionManager) doReconnect(ctx context.Context) (bool, error) {
	adapter := c.adapter
	name := adapter.Name()
	for attempt := 0; attempt < maxReconnectAttempts; attempt++ {
		wait := reconnectMaxWait
		if attempt < 6 {
			wait = 1 << attempt
		}
		slog.Info(fmt.Sprintf("[%s] Reconnect attempt %d/%d in %ds", name, attempt+1, maxReconnectAttempts, wait))
		if err := sleep(ctx, float64(wait)); err != nil {
			return false, err
		}
		c.cleanupWS(ctx)
		dialed, err := func() (bool, error) {
			tokenData, err := signForceRefresh(ctx, adapter.appKey, adapter.appSecret, adapter.apiDomain, adapter.routeEnv)
			if err != nil {
				return false, err
			}
			return c.dial(ctx, tokenData)
		}()
		switch {
		case err != nil && ctx.Err() != nil:
			return false, ctx.Err()
		case err != nil && isTimeout(err):
			slog.Warn(fmt.Sprintf("[%s] Reconnect attempt %d timed out", name, attempt+1))
		case err != nil:
			slog.Warn(fmt.Sprintf("[%s] Reconnect attempt %d failed: %s", name, attempt+1, errText(err)))
		case !dialed:
			slog.Warn(fmt.Sprintf("[%s] Re-auth failed on attempt %d", name, attempt+1))
		default:
			c.mu.Lock()
			c.consecutiveHBTimeouts = 0
			c.mu.Unlock()
			c.connected(ctx, true)
			slog.Info(fmt.Sprintf("[%s] Reconnected on attempt %d. connectId=%s", name, attempt+1, c.boundConnectID()))
			return true, nil
		}
	}
	slog.Error(fmt.Sprintf("[%s] Giving up after %d reconnect attempts", name, maxReconnectAttempts))
	adapter.MarkDisconnected(ctx)
	return false, nil
}

// Closes and forgets the socket, waiting no longer than the close bound for the closing handshake
func (c *connectionManager) cleanupWS(ctx context.Context) {
	c.mu.Lock()
	ws := c.ws
	c.ws = nil
	c.mu.Unlock()
	if ws == nil {
		return
	}
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		_ = recovered(ws.Close)
	}()
	bound := time.NewTimer(durationOf(wsCloseTimeoutS))
	defer bound.Stop()
	select {
	case <-closed:
	case <-bound.C:
		slog.Debug(fmt.Sprintf("[%s] WS close handshake exceeded %.1fs — dropping connection", c.adapter.Name(), wsCloseTimeoutS))
	case <-ctx.Done():
	}
}
