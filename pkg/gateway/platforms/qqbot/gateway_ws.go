package qqbot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Close codes that end the session for good with their meaning
var fatalCloseCodes = map[int]string{
	4001: "invalid opcode", 4002: "invalid payload", 4010: "invalid shard", 4011: "sharding required",
	4012: "invalid API version", 4013: "invalid intent", 4014: "intent not authorized", 4914: "offline/sandbox-only", 4915: "banned",
}

// Whether a close code invalidates the session so the next Hello identifies afresh
func sessionInvalidCode(code int) bool {
	return code == 4006 || code == 4007 || (code >= 4900 && code <= 4913)
}

// Dispatch types of inbound messages and the QQ surface each comes from
var inboundKinds = map[string]string{
	"C2C_MESSAGE_CREATE": "c2c", "GROUP_AT_MESSAGE_CREATE": "group", "GUILD_MESSAGE_CREATE": "guild",
	"GUILD_AT_MESSAGE_CREATE": "guild", "DIRECT_MESSAGE_CREATE": "dm",
}

// C2C and group at messages, public guild messages, guild direct messages and button interactions
const identifyIntents = 1<<25 | 1<<30 | 1<<12 | 1<<26

// Error of a websocket the adapter closed itself after a server reconnect or invalid session
var errLocalClose = errors.New("websocket closed for a fresh session")

// One gateway websocket with serialized writes and a close signal
type wsConn struct {
	c             *websocket.Conn
	writeMu       sync.Mutex
	done          chan struct{}
	closeOnce     sync.Once
	heartbeatOnce sync.Once
}

func newWSConn(c *websocket.Conn) *wsConn { return &wsConn{c: c, done: make(chan struct{})} }

// Writes one JSON payload under the write lock
func (w *wsConn) writeJSON(v any) error {
	w.writeMu.Lock()
	defer w.writeMu.Unlock()
	if err := w.c.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return err
	}
	return w.c.WriteJSON(v)
}

// Whether the websocket was closed
func (w *wsConn) isClosed() bool {
	select {
	case <-w.done:
		return true
	default:
		return false
	}
}

// Sends a normal close frame and drops the socket, once
func (w *wsConn) close() {
	w.closeOnce.Do(func() {
		close(w.done)
		_ = w.c.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
		_ = w.c.Close()
	})
}

// Proxy of the first set WSS, HTTPS or ALL proxy variable, nil when none is set
func wsProxy() func(*http.Request) (*url.URL, error) {
	for _, name := range []string{"WSS_PROXY", "wss_proxy", "HTTPS_PROXY", "https_proxy", "ALL_PROXY", "all_proxy"} {
		if v := os.Getenv(name); v != "" {
			u, err := url.Parse(v)
			if err != nil {
				slog.Warn(fmt.Sprintf("[QQBot] Ignoring unparsable %s: %v", name, err))
				return nil
			}
			return http.ProxyURL(u)
		}
	}
	return nil
}

// Token, then gateway URL, then the websocket dial
func (a *QQAdapter) openGateway(ctx context.Context, logURL bool) (*wsConn, error) {
	rest, err := a.client()
	if err != nil {
		return nil, err
	}
	if _, err := rest.ensureToken(ctx); err != nil {
		return nil, err
	}
	gatewayURL, err := rest.gatewayURL(ctx)
	if err != nil {
		return nil, err
	}
	if logURL {
		slog.Info(fmt.Sprintf("[%s] Gateway URL: %s", a.logTag(), gatewayURL))
	}
	dialer := websocket.Dialer{Proxy: wsProxy(), HandshakeTimeout: connectTimeout}
	dctx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	c, resp, err := dialer.DialContext(dctx, gatewayURL, http.Header{"User-Agent": {userAgent()}})
	if resp != nil && resp.Body != nil {
		resp.Body.Close()
	}
	if err != nil {
		return nil, fmt.Errorf("websocket connect to %s: %w", gatewayURL, err)
	}
	slog.Info(fmt.Sprintf("[%s] WebSocket connected to %s", a.logTag(), gatewayURL))
	return newWSConn(c), nil
}

// Live websocket, nil between connections
func (a *QQAdapter) currentConn() *wsConn {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.conn
}

// Reads gateway events and reconnects after drops until the context ends or a close code is fatal
func (a *QQAdapter) listen(ctx context.Context) {
	tag := a.logTag()
	backoffIdx, quick := 0, 0
	for ctx.Err() == nil {
		conn := a.currentConn()
		started := time.Now()
		err := a.readEvents(ctx, conn)
		if ctx.Err() != nil {
			return
		}
		if conn != nil {
			conn.close()
		}
		a.markTransportDisconnected(ctx)
		var wait time.Duration
		var ce *websocket.CloseError
		switch {
		case errors.As(err, &ce):
			code := ce.Code
			slog.Warn(fmt.Sprintf("[%s] WebSocket closed: code=%d reason=%s", tag, code, ce.Text))
			if lived := time.Since(started); lived < quickDisconnectWindow {
				quick++
				slog.Info(fmt.Sprintf("[%s] Quick disconnect (%.1fs), count: %d", tag, lived.Seconds(), quick))
				if quick >= maxQuickDisconnects {
					slog.Error(fmt.Sprintf("[%s] Too many quick disconnects. Check: 1) AppID/Secret correct 2) Bot permissions on QQ Open Platform", tag))
					a.fail(ctx, "qq_quick_disconnect", "Too many quick disconnects — check bot permissions", true)
					return
				}
			} else {
				quick = 0
			}
			if desc, fatal := fatalCloseCodes[code]; fatal {
				slog.Error(fmt.Sprintf("[%s] Bot is %s. Check QQ Open Platform.", tag, desc))
				a.fail(ctx, "qq_"+strings.NewReplacer(" ", "_", "/", "_").Replace(desc), "Bot is "+desc, false)
				return
			}
			switch code {
			case 4008:
				slog.Info(fmt.Sprintf("[%s] Rate limited (4008), waiting %ds", tag, int(rateLimitDelay.Seconds())))
				wait = rateLimitDelay
			case 4004:
				slog.Info(fmt.Sprintf("[%s] Invalid token (4004), will refresh and reconnect", tag))
				if rest, rerr := a.client(); rerr == nil {
					rest.invalidateToken()
				}
			}
			if sessionInvalidCode(code) {
				slog.Info(fmt.Sprintf("[%s] Session error (%d), clearing session for re-identify", tag, code))
				a.clearSession()
			}
		case errors.Is(err, errLocalClose):
			slog.Info(fmt.Sprintf("[%s] Reconnecting for a fresh gateway session", tag))
		default:
			slog.Warn(fmt.Sprintf("[%s] WebSocket error: %v", tag, err))
		}
		if wait > 0 && sleepCtx(ctx, wait) != nil {
			return
		}
		for {
			if backoffIdx >= maxReconnectAttempts {
				slog.Error(fmt.Sprintf("[%s] Max reconnect attempts reached", tag))
				a.fail(ctx, "qq_reconnect_exhausted", fmt.Sprintf("QQ gateway stayed down through %d reconnect attempts", maxReconnectAttempts), true)
				return
			}
			if a.reconnect(ctx, backoffIdx) {
				backoffIdx = 0
				break
			}
			if ctx.Err() != nil {
				return
			}
			backoffIdx++
		}
	}
}

// Waits the backoff delay of the attempt and opens a fresh websocket, true when it is live
func (a *QQAdapter) reconnect(ctx context.Context, attempt int) bool {
	tag := a.logTag()
	delay := reconnectBackoff[min(attempt, len(reconnectBackoff)-1)]
	slog.Info(fmt.Sprintf("[%s] Reconnecting in %ds (attempt %d)...", tag, int(delay.Seconds()), attempt+1))
	if sleepCtx(ctx, delay) != nil {
		return false
	}
	a.mu.Lock()
	a.heartbeatEvery = 30 * time.Second
	a.mu.Unlock()
	conn, err := a.openGateway(ctx, false)
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn(fmt.Sprintf("[%s] Reconnect failed: %v", tag, err))
		}
		return false
	}
	a.mu.Lock()
	if ctx.Err() != nil {
		a.mu.Unlock()
		conn.close()
		return false
	}
	a.conn = conn
	a.mu.Unlock()
	a.MarkConnected(ctx, "")
	slog.Info(fmt.Sprintf("[%s] Reconnected", tag))
	return true
}

// Reads frames of one websocket until it fails, dispatching each JSON payload
func (a *QQAdapter) readEvents(ctx context.Context, conn *wsConn) error {
	if conn == nil {
		return errors.New("websocket not connected")
	}
	for {
		kind, data, err := conn.c.ReadMessage()
		if err != nil {
			if conn.isClosed() {
				return errLocalClose
			}
			return err
		}
		if kind != websocket.TextMessage {
			continue
		}
		decoded, err := decodeJSON(data)
		payload := jmap(decoded)
		if err != nil || payload == nil {
			slog.Warn(fmt.Sprintf("[%s] Failed to parse JSON: %q", a.logTag(), cutRunes(string(data), 200)))
			continue
		}
		a.dispatchPayload(ctx, conn, payload)
	}
}

// Forgets the session so the next Hello identifies instead of resuming
func (a *QQAdapter) clearSession() {
	a.mu.Lock()
	a.sessionID, a.lastSeq, a.hasSeq = "", 0, false
	a.mu.Unlock()
}

// Routes one gateway payload, spawning message and interaction handlers off the read loop
func (a *QQAdapter) dispatchPayload(ctx context.Context, conn *wsConn, payload map[string]any) {
	tag := a.logTag()
	if n, ok := payload["s"].(json.Number); ok {
		if s, err := n.Int64(); err == nil {
			a.mu.Lock()
			if !a.hasSeq || s > a.lastSeq {
				a.lastSeq, a.hasSeq = s, true
			}
			a.mu.Unlock()
		}
	}
	op, hasOp := jint(payload["op"])
	t := jstr(payload["t"])
	d := payload["d"]
	switch {
	case hasOp && op == 10:
		intervalMS, ok := jint(jmap(d)["heartbeat_interval"])
		if !ok {
			intervalMS = 30000
		}
		every := time.Duration(float64(intervalMS)*0.8) * time.Millisecond
		a.mu.Lock()
		a.heartbeatEvery = every
		resume := a.sessionID != "" && a.hasSeq
		a.mu.Unlock()
		slog.Debug(fmt.Sprintf("[%s] Hello received, heartbeat_interval=%dms (sending every %.1fs)", tag, intervalMS, every.Seconds()))
		conn.heartbeatOnce.Do(func() { go a.heartbeat(conn) })
		if resume {
			a.sendResume(ctx, conn)
		} else {
			a.sendIdentify(ctx, conn)
		}
	case hasOp && op == 0 && t != "":
		switch {
		case t == "READY":
			if m := jmap(d); m != nil {
				a.mu.Lock()
				a.sessionID = jstr(m["session_id"])
				a.mu.Unlock()
				slog.Info(fmt.Sprintf("[%s] Ready, session_id=%s", tag, jstr(m["session_id"])))
			}
		case t == "RESUMED":
			slog.Info(fmt.Sprintf("[%s] Session resumed", tag))
		case inboundKinds[t] != "":
			data := jmap(d)
			a.spawn(ctx, func(tctx context.Context) error { return a.onMessage(tctx, t, data) })
		case t == "INTERACTION_CREATE":
			data := jmap(d)
			a.spawn(ctx, func(tctx context.Context) error { return a.onInteraction(tctx, data) })
		default:
			slog.Debug(fmt.Sprintf("[%s] Unhandled dispatch: %s", tag, t))
		}
	case hasOp && op == 11:
	case hasOp && op == 7:
		slog.Info(fmt.Sprintf("[%s] Server requested reconnect (op 7)", tag))
		conn.close()
	case hasOp && op == 9:
		if d != nil && jbool(d) {
			slog.Info(fmt.Sprintf("[%s] Invalid session (op 9, resumable)", tag))
		} else {
			slog.Info(fmt.Sprintf("[%s] Invalid session (op 9, not resumable), clearing session", tag))
			a.clearSession()
		}
		conn.close()
	default:
		slog.Debug(fmt.Sprintf("[%s] Unknown op: %v", tag, payload["op"]))
	}
}

// Runs a handler as a tracked adapter task detached from the read loop
func (a *QQAdapter) spawn(ctx context.Context, fn func(ctx context.Context) error) {
	task := gateway.SpawnAdapterTask(ctx, func(tctx context.Context) error {
		err := fn(tctx)
		if err != nil && tctx.Err() == nil {
			slog.Error(fmt.Sprintf("[%s] Event handler failed: %v", a.logTag(), err))
		}
		return err
	})
	a.TrackBackgroundTask(task)
}

// Sends op 1 heartbeats with the latest sequence at the interval Hello set until the websocket closes
func (a *QQAdapter) heartbeat(conn *wsConn) {
	for {
		a.mu.Lock()
		every := a.heartbeatEvery
		a.mu.Unlock()
		t := time.NewTimer(every)
		select {
		case <-conn.done:
			t.Stop()
			return
		case <-t.C:
		}
		a.mu.Lock()
		var seq any
		if a.hasSeq {
			seq = a.lastSeq
		}
		a.mu.Unlock()
		if err := conn.writeJSON(map[string]any{"op": 1, "d": seq}); err != nil {
			slog.Debug(fmt.Sprintf("[%s] Heartbeat failed: %v", a.logTag(), err))
		}
	}
}

// Sends op 2 Identify for the message, direct message and interaction intents
func (a *QQAdapter) sendIdentify(ctx context.Context, conn *wsConn) {
	rest, err := a.client()
	var token string
	if err == nil {
		token, err = rest.ensureToken(ctx)
	}
	if err != nil {
		slog.Error(fmt.Sprintf("[%s] Failed to send Identify: %v", a.logTag(), err))
		conn.close()
		return
	}
	payload := map[string]any{"op": 2, "d": map[string]any{
		"token": "QQBot " + token, "intents": identifyIntents, "shard": []int{0, 1},
		"properties": map[string]any{"$os": runtime.GOOS, "$browser": "icebath", "$device": "icebath"}}}
	if err := conn.writeJSON(payload); err != nil {
		slog.Error(fmt.Sprintf("[%s] Failed to send Identify: %v", a.logTag(), err))
		return
	}
	slog.Info(fmt.Sprintf("[%s] Identify sent", a.logTag()))
}

// Sends op 6 Resume for the stored session, clearing the session when the send fails
func (a *QQAdapter) sendResume(ctx context.Context, conn *wsConn) {
	rest, err := a.client()
	var token string
	if err == nil {
		token, err = rest.ensureToken(ctx)
	}
	a.mu.Lock()
	sessionID, seq := a.sessionID, a.lastSeq
	a.mu.Unlock()
	if err == nil {
		err = conn.writeJSON(map[string]any{"op": 6, "d": map[string]any{"token": "QQBot " + token, "session_id": sessionID, "seq": seq}})
	}
	if err != nil {
		slog.Error(fmt.Sprintf("[%s] Failed to send Resume: %v", a.logTag(), err))
		a.clearSession()
		conn.close()
		return
	}
	slog.Info(fmt.Sprintf("[%s] Resume sent (session_id=%s, seq=%d)", a.logTag(), sessionID, seq))
}
