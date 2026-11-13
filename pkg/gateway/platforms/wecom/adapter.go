package wecom

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
)

// Callback commands carrying user messages and the commands whose req_id is never a reply
var (
	callbackCmds    = map[string]bool{cmdCallback: true, cmdLegacyCallback: true}
	nonResponseCmds = map[string]bool{cmdCallback: true, cmdLegacyCallback: true, cmdEventCallback: true}
)

// Unicode whitespace class of the Python patterns the mention and trim rules came from
const unicodeSpace = `\s\v\x{1c}-\x{1f}\x{85}\p{Z}`

var (
	mentionPrefixRE  = regexp.MustCompile(`^@[^` + unicodeSpace + `]+[` + unicodeSpace + `]*`)
	platformPrefixRE = regexp.MustCompile(`(?i)^wecom:`)
	kindPrefixRE     = regexp.MustCompile(`(?i)^(user|group):`)
)

// One websocket connection with its serialized writer and protocol keepalive
type wsConn struct {
	ws      *websocket.Conn
	writeMu sync.Mutex
	closed  atomic.Bool
	done    chan struct{}
}

func newWSConn(ws *websocket.Conn) *wsConn { return &wsConn{ws: ws, done: make(chan struct{})} }

// Writes one text frame under the write deadline
func (c *wsConn) write(data []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_ = c.ws.SetWriteDeadline(time.Now().Add(writeTimeout))
	return c.ws.WriteMessage(websocket.TextMessage, data)
}

// Closes the socket once, stopping its keepalive
func (c *wsConn) close() {
	if c.closed.CompareAndSwap(false, true) {
		close(c.done)
		_ = c.ws.Close()
	}
}

// Websocket pings at the protocol cadence so an idle peer is detected, ending with the connection
func (c *wsConn) keepalive() {
	ticker := time.NewTicker(wsPingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-c.done:
			return
		case <-ticker.C:
			if err := c.ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(writeTimeout)); err != nil {
				return
			}
		}
	}
}

// WeCom AI bot adapter over the openws websocket gateway with native stream replies and a per chat send queue
type WeComAdapter struct {
	*gateway.BasePlatformAdapter
	*gateway.OwnAccessPolicyMixin
	*Streamer

	botID                string
	secret               string
	wsURL                string
	groups               map[string]any
	attachmentMergeDelay float64
	deviceID             string
	dedup                *gateway.MessageDeduplicator

	mu             sync.Mutex
	conn           *wsConn
	pending        map[string]*ackFuture
	httpClient     *http.Client
	queue          *ChatSendQueue
	replyReqIDs    *boundedMap
	lastChatReqIDs *boundedMap
	groupChats     map[string]bool
	loopCancel     context.CancelFunc
	loopsDone      chan struct{}
}

// Class attributes of the AI bot adapter
func adapterTraits() gateway.AdapterTraits {
	t := gateway.DefaultAdapterTraits()
	t.MaxMessageLength = maxMessageLength
	t.SupportsMessageEditing = false
	t.SupportsNativeStreaming = true
	t.SplitsLongMessages = true
	t.SerialReceiveLoop = true
	return t
}

// Trimmed non empty strings of a comma list, a list or a scalar
func coerceList(v any) []string {
	var raw []string
	switch x := v.(type) {
	case nil:
	case string:
		raw = strings.Split(x, ",")
	case []string:
		raw = x
	case []any:
		for _, item := range x {
			raw = append(raw, textOf(item))
		}
	default:
		raw = []string{textOf(x)}
	}
	var out []string
	for _, item := range raw {
		if s := strings.TrimSpace(item); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// First set value among the keys of a config map
func firstSet(m map[string]any, keys ...string) any {
	for _, k := range keys {
		if isSet(m[k]) {
			return m[k]
		}
	}
	return nil
}

// Allowlist entry without its wecom and user or group prefixes
func normalizeEntry(raw string) string {
	value := platformPrefixRE.ReplaceAllString(strings.TrimSpace(raw), "")
	return strings.TrimSpace(kindPrefixRE.ReplaceAllString(value, ""))
}

// Case insensitive allowlist match where a star admits everyone
func entryMatches(entries []string, target string) bool {
	normalized := strings.ToLower(strings.TrimSpace(target))
	for _, e := range entries {
		if n := strings.ToLower(normalizeEntry(e)); n == "*" || n == normalized {
			return true
		}
	}
	return false
}

// Adapter configured from extra and the scoped WECOM env
func NewWeComAdapter(ctx context.Context, cfg *gateway.PlatformConfig) (*WeComAdapter, error) {
	if cfg == nil {
		cfg = gateway.NewPlatformConfig()
	}
	a := &WeComAdapter{deviceID: randHex(16), dedup: gateway.NewMessageDeduplicator(dedupMaxSize, gateway.DefaultDedupTTLSeconds),
		pending: map[string]*ackFuture{}, replyReqIDs: newBoundedMap(dedupMaxSize), lastChatReqIDs: newBoundedMap(dedupMaxSize),
		groupChats: map[string]bool{}}
	a.BasePlatformAdapter = gateway.NewBasePlatformAdapter(cfg, gateway.PlatformWeCom, a)
	a.Traits = adapterTraits()
	policy, err := gateway.NewOwnAccessPolicyMixin("WeComAdapter", "WECOM")
	if err != nil {
		return nil, err
	}
	a.OwnAccessPolicyMixin = policy
	policy.EntryMatches = func(_ context.Context, entries map[string]bool, target string) (bool, error) {
		list := make([]string, 0, len(entries))
		for e, present := range entries {
			if present {
				list = append(list, e)
			}
		}
		return entryMatches(list, target), nil
	}
	extra := cfg.Extra
	if extra == nil {
		extra = map[string]any{}
	}
	setting := func(env, def string, keys ...string) (string, error) {
		if v := firstSet(extra, keys...); v != nil {
			return strings.TrimSpace(textOf(v)), nil
		}
		v, err := gateway.GetScopedSecret(ctx, env, def, false)
		return strings.TrimSpace(v), err
	}
	if a.botID, err = setting("WECOM_BOT_ID", "", "bot_id"); err != nil {
		return nil, err
	}
	if a.secret, err = setting("WECOM_SECRET", "", "secret"); err != nil {
		return nil, err
	}
	if a.wsURL, err = setting("WECOM_WEBSOCKET_URL", defaultWSURL, "websocket_url", "websocketUrl"); err != nil {
		return nil, err
	}
	if a.wsURL == "" {
		a.wsURL = defaultWSURL
	}
	dmPolicy, err := setting("WECOM_DM_POLICY", "pairing", "dm_policy")
	if err != nil {
		return nil, err
	}
	groupPolicy, err := setting("WECOM_GROUP_POLICY", "pairing", "group_policy")
	if err != nil {
		return nil, err
	}
	policy.DMPolicy, policy.GroupPolicy = strings.ToLower(dmPolicy), strings.ToLower(groupPolicy)
	allowRaw := firstSet(extra, "allow_from", "allowFrom")
	if allowRaw == nil {
		env, err := gateway.GetScopedSecret(ctx, "WECOM_ALLOWED_USERS", "", false)
		if err != nil {
			return nil, err
		}
		allowRaw = env
	}
	for _, e := range coerceList(allowRaw) {
		policy.AllowFrom[e] = true
	}
	for _, e := range coerceList(firstSet(extra, "group_allow_from", "groupAllowFrom")) {
		policy.GroupAllowFrom[e] = true
	}
	if groups, ok := extra["groups"].(map[string]any); ok {
		a.groups = groups
	}
	a.attachmentMergeDelay = floatOf(extra["attachment_text_merge_delay_seconds"], 0.8)
	a.Streamer = newStreamer(a, truthy(extra["stream_keepalive_enabled"]),
		floatOf(extra["stream_keepalive_interval_seconds"], defaultKeepaliveInterval),
		floatOf(extra["stream_safe_duration_seconds"], defaultStreamSafeDuration))
	a.SetTextBatchDelays(config.EnvFloat("ICEBATH_WECOM_TEXT_BATCH_DELAY_SECONDS", 0.6),
		config.EnvFloat("ICEBATH_WECOM_TEXT_BATCH_SPLIT_DELAY_SECONDS", 2.0))
	return a, nil
}

// The adapter gates DMs and groups itself
func (a *WeComAdapter) EnforcesOwnAccessPolicy() bool { return true }

// Opens the websocket, subscribes with the bot credentials and starts the receive, heartbeat and intake loops
func (a *WeComAdapter) Connect(ctx context.Context, isReconnect bool) (bool, error) {
	if a.botID == "" || a.secret == "" {
		msg := "WeCom startup failed: WECOM_BOT_ID and WECOM_SECRET are required"
		a.SetFatalError(ctx, "wecom_missing_credentials", msg, true)
		slog.Warn(fmt.Sprintf("[%s] %s", a.Name(), msg))
		return false, nil
	}
	a.stopLoops()
	a.teardown()
	a.mu.Lock()
	a.httpClient = newMediaHTTPClient(ctx)
	a.mu.Unlock()
	if err := a.openConnection(ctx); err != nil {
		a.teardown()
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		a.SetFatalError(ctx, "wecom_connect_error", "WeCom startup failed: "+err.Error(), true)
		slog.Error(fmt.Sprintf("[%s] Failed to connect: %v", a.Name(), err))
		return false, nil
	}
	a.MarkConnected(ctx, "")
	a.startLoops(ctx)
	slog.Info(fmt.Sprintf("[%s] Connected to %s", a.Name(), a.wsURL))
	a.WirePluginHandlers(ctx, nil)
	return true, nil
}

// Stops the loops and send workers, fails every waiting reply and closes the connection
func (a *WeComAdapter) Disconnect(ctx context.Context) error {
	a.MarkDisconnected(ctx)
	a.mu.Lock()
	queue := a.queue
	a.queue = nil
	a.mu.Unlock()
	if queue != nil {
		queue.Shutdown()
	}
	a.stopLoops()
	stopped := errors.New("WeCom adapter disconnected")
	a.failPending(stopped)
	a.Streamer.reset(stopped)
	a.teardown()
	a.dedup.Clear()
	slog.Info(fmt.Sprintf("[%s] Disconnected", a.Name()))
	return nil
}

// Send queue of the adapter, created on first use after a disconnect
func (a *WeComAdapter) sendQueue() *ChatSendQueue {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.queue == nil {
		a.queue = NewChatSendQueue(a.Name())
	}
	return a.queue
}

// Starts the receive, heartbeat and intake loops detached from the caller's cancellation
func (a *WeComAdapter) startLoops(ctx context.Context) {
	loopCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	inbound := make(chan map[string]any, 1024)
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); a.listenLoop(loopCtx, inbound) }()
	go func() { defer wg.Done(); a.heartbeatLoop(loopCtx) }()
	go func() { defer wg.Done(); a.intakeLoop(loopCtx, inbound) }()
	go func() { wg.Wait(); close(done) }()
	a.mu.Lock()
	a.loopCancel, a.loopsDone = cancel, done
	a.mu.Unlock()
}

// Cancels the loops, unblocks the reader and waits for them to end
func (a *WeComAdapter) stopLoops() {
	a.mu.Lock()
	cancel, done := a.loopCancel, a.loopsDone
	a.loopCancel, a.loopsDone = nil, nil
	a.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	a.closeConn()
	<-done
}

// Live connection, nil between connections
func (a *WeComAdapter) currentConn() *wsConn {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.conn
}

// Closes and forgets the live connection
func (a *WeComAdapter) closeConn() {
	a.mu.Lock()
	conn := a.conn
	a.conn = nil
	a.mu.Unlock()
	if conn != nil {
		conn.close()
	}
}

// Closes the connection and the media HTTP client
func (a *WeComAdapter) teardown() {
	a.closeConn()
	a.mu.Lock()
	client := a.httpClient
	a.httpClient = nil
	a.mu.Unlock()
	if client != nil {
		client.CloseIdleConnections()
	}
}

// Dials the gateway, sends the subscribe handshake and installs the connection once WeCom accepts it
func (a *WeComAdapter) openConnection(ctx context.Context) error {
	a.closeConn()
	dialer := websocket.Dialer{HandshakeTimeout: connectTimeout}
	if gateway.GatewayTrustEnv(ctx) {
		dialer.Proxy = http.ProxyFromEnvironment
	}
	dctx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	ws, _, err := dialer.DialContext(dctx, a.wsURL, nil)
	if err != nil {
		return err
	}
	conn := newWSConn(ws)
	reqID := newReqID("subscribe")
	data, err := encodeJSON(map[string]any{"cmd": cmdSubscribe, "headers": map[string]any{"req_id": reqID},
		"body": map[string]any{"bot_id": a.botID, "secret": a.secret, "device_id": a.deviceID}})
	if err == nil {
		err = conn.write(data)
	}
	if err != nil {
		conn.close()
		return err
	}
	auth, err := a.waitForHandshake(conn, reqID)
	if err != nil {
		conn.close()
		return err
	}
	if _, failed := errcodeOf(auth); failed {
		conn.close()
		msg := textOf(auth["errmsg"])
		if msg == "" {
			msg = "authentication failed"
		}
		return fmt.Errorf("%s (errcode=%s)", msg, textOf(auth["errcode"]))
	}
	_ = ws.SetReadDeadline(time.Now().Add(readIdleTimeout))
	ws.SetPongHandler(func(string) error { return ws.SetReadDeadline(time.Now().Add(readIdleTimeout)) })
	a.mu.Lock()
	a.conn = conn
	a.mu.Unlock()
	go conn.keepalive()
	return nil
}

// Subscribe acknowledgement of the req_id, skipping pings and other pre auth frames
func (a *WeComAdapter) waitForHandshake(conn *wsConn, reqID string) (map[string]any, error) {
	_ = conn.ws.SetReadDeadline(time.Now().Add(connectTimeout))
	for {
		mt, data, err := conn.ws.ReadMessage()
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				return nil, errors.New("timed out waiting for WeCom subscribe acknowledgement")
			}
			return nil, fmt.Errorf("WeCom websocket closed during authentication: %w", err)
		}
		if mt != websocket.TextMessage {
			continue
		}
		payload, _, err := decodeFrame(data)
		if err != nil || textOf(payload["cmd"]) == cmdPing {
			continue
		}
		if payloadReqID(payload) == reqID {
			return payload, nil
		}
		slog.Debug(fmt.Sprintf("[%s] Ignoring pre-auth payload: %s", a.Name(), textOf(payload["cmd"])))
	}
}

// Sleeps for d, false when the context ends first
func sleepCtx(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// Reads frames until the adapter stops, reconnecting with backoff after every connection failure
func (a *WeComAdapter) listenLoop(ctx context.Context, inbound chan<- map[string]any) {
	attempt := 0
	for ctx.Err() == nil && a.IsConnected() {
		err := a.readEvents(ctx, inbound)
		if ctx.Err() != nil || !a.IsConnected() || err == nil {
			return
		}
		slog.Warn(fmt.Sprintf("[%s] WebSocket error: %v", a.Name(), err))
		interrupted := errors.New("WeCom connection interrupted")
		a.failPending(interrupted)
		a.Streamer.failReplyQueues(interrupted)
		a.closeConn()
		if !sleepCtx(ctx, reconnectBackoff[min(attempt, len(reconnectBackoff)-1)]) {
			return
		}
		attempt++
		if err := a.openConnection(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Warn(fmt.Sprintf("[%s] Reconnect failed: %v", a.Name(), err))
			continue
		}
		attempt = 0
		a.MarkConnected(ctx, "")
		slog.Info(fmt.Sprintf("[%s] Reconnected", a.Name()))
	}
}

// Reads and dispatches frames of the live connection, nil once the adapter stops
func (a *WeComAdapter) readEvents(ctx context.Context, inbound chan<- map[string]any) error {
	conn := a.currentConn()
	if conn == nil {
		return errors.New("WebSocket not connected")
	}
	for ctx.Err() == nil && a.IsConnected() {
		mt, data, err := conn.ws.ReadMessage()
		if err != nil {
			return fmt.Errorf("WeCom websocket closed: %w", err)
		}
		_ = conn.ws.SetReadDeadline(time.Now().Add(readIdleTimeout))
		a.handleFrame(ctx, data, mt == websocket.BinaryMessage, inbound)
	}
	return nil
}

// Parses one text or binary frame and dispatches it, every drop logged at info
func (a *WeComAdapter) handleFrame(ctx context.Context, data []byte, binary bool, inbound chan<- map[string]any) {
	name := a.Name()
	if binary {
		head := []rune(strings.ToValidUTF8(string(data), "�"))
		slog.Info(fmt.Sprintf("[%s] Inbound BINARY frame received (len=%d) head=%q — attempting JSON parse", name, len(data), string(head[:min(len(head), 200)])))
	}
	payload, lenient, err := decodeFrame(data)
	if err != nil {
		if binary {
			slog.Info(fmt.Sprintf("[%s] BINARY frame not parseable as JSON — dropped", name))
			return
		}
		tail := data[max(0, len(data)-100):]
		slog.Warn(fmt.Sprintf("Failed to parse WeCom payload: error=%v len=%d tail=%q", err, len(data), tail))
		slog.Info(fmt.Sprintf("[%s] Inbound TEXT frame dropped (unparseable/non-dict) len=%d", name, len(data)))
		return
	}
	if lenient {
		slog.Info(fmt.Sprintf("WeCom payload required lenient control character parsing (len=%d)", len(data)))
	}
	a.dispatchPayload(ctx, payload, inbound)
}

// Routes a frame to its waiting reply, the message intake or the kicked connection handling
func (a *WeComAdapter) dispatchPayload(ctx context.Context, payload map[string]any, inbound chan<- map[string]any) {
	reqID := payloadReqID(payload)
	cmd := textOf(payload["cmd"])
	if reqID != "" && !nonResponseCmds[cmd] {
		if a.Streamer.resolveReplyAck(reqID, payload) || a.resolvePending(reqID, payload) {
			return
		}
	}
	switch {
	case callbackCmds[cmd]:
		select {
		case inbound <- payload:
		case <-ctx.Done():
		}
	case cmd == cmdEventCallback:
		if textOf(objAt(payload, "body")["event_type"]) == "disconnected_event" {
			slog.Warn(fmt.Sprintf("[%s] Kicked by server (another WS connection established). Suppressing reconnect to avoid mutual kicking. Check for duplicate gateway instances.", a.Name()))
			a.SetRunning(false)
		}
	case cmd == cmdPing, strings.HasPrefix(reqID, "ping-"):
	default:
		keys := []string{}
		for k := range objAt(payload, "body") {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if cmd == "" {
			cmd = "(empty)"
		}
		if reqID == "" {
			reqID = "(none)"
		}
		slog.Info(fmt.Sprintf("[%s] Unrouted websocket payload dropped: cmd=%q req_id=%s body_keys=%v", a.Name(), cmd, reqID, keys))
	}
}

// Sends an application ping every heartbeat interval while the adapter runs
func (a *WeComAdapter) heartbeatLoop(ctx context.Context) {
	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if !a.IsConnected() {
			return
		}
		if a.currentConn() == nil {
			continue
		}
		if err := a.sendJSON(map[string]any{"cmd": cmdPing, "headers": map[string]any{"req_id": newReqID("ping")}, "body": map[string]any{}}); err != nil {
			slog.Debug(fmt.Sprintf("[%s] Heartbeat send failed: %v", a.Name(), err))
		}
	}
}

// Handles inbound message callbacks one at a time in arrival order, the reader staying free for acks
func (a *WeComAdapter) intakeLoop(ctx context.Context, inbound <-chan map[string]any) {
	for {
		select {
		case <-ctx.Done():
			return
		case payload := <-inbound:
			a.onMessage(ctx, payload)
		}
	}
}

// Completes the waiting request of a req_id, true when one was waiting
func (a *WeComAdapter) resolvePending(reqID string, payload map[string]any) bool {
	a.mu.Lock()
	f, ok := a.pending[reqID]
	a.mu.Unlock()
	if ok {
		f.resolve(payload, nil)
	}
	return ok
}

// Fails every waiting request with the error
func (a *WeComAdapter) failPending(err error) {
	a.mu.Lock()
	pending := a.pending
	a.pending = map[string]*ackFuture{}
	a.mu.Unlock()
	for _, f := range pending {
		f.resolve(nil, err)
	}
}

// Error unless a connection is live
func (a *WeComAdapter) requireWS() error {
	if conn := a.currentConn(); conn == nil || conn.closed.Load() {
		return errors.New("WeCom websocket is not connected")
	}
	return nil
}

// Writes one JSON frame on the live connection
func (a *WeComAdapter) sendJSON(payload map[string]any) error {
	conn := a.currentConn()
	if conn == nil || conn.closed.Load() {
		return errors.New("WeCom websocket is not connected")
	}
	data, err := encodeJSON(payload)
	if err != nil {
		return err
	}
	return conn.write(data)
}

// Sends a command frame under the req_id and waits for the reply carrying it back
func (a *WeComAdapter) request(ctx context.Context, cmd, reqID string, body map[string]any, timeout time.Duration) (map[string]any, error) {
	f := newAckFuture()
	a.mu.Lock()
	a.pending[reqID] = f
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		if a.pending[reqID] == f {
			delete(a.pending, reqID)
		}
		a.mu.Unlock()
	}()
	if err := a.sendJSON(map[string]any{"cmd": cmd, "headers": map[string]any{"req_id": reqID}, "body": body}); err != nil {
		return nil, err
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-f.done:
		return f.payload, f.err
	case <-timer.C:
		return nil, errRequestTimeout
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Proactive command under a fresh req_id
func (a *WeComAdapter) sendRequest(ctx context.Context, cmd string, body map[string]any) (map[string]any, error) {
	if err := a.requireWS(); err != nil {
		return nil, err
	}
	return a.request(ctx, cmd, newReqID(cmd), body, requestTimeout)
}

// Passive reply correlated to an inbound callback req_id
func (a *WeComAdapter) sendReplyRequest(ctx context.Context, replyReqID string, body map[string]any) (map[string]any, error) {
	if err := a.requireWS(); err != nil {
		return nil, err
	}
	normalized := strings.TrimSpace(replyReqID)
	if normalized == "" {
		return nil, errors.New("reply_req_id is required")
	}
	return a.request(ctx, cmdRespond, normalized, body, requestTimeout)
}

// Proactive command whose WeCom errcode is turned into an error
func (a *WeComAdapter) checkedRequest(ctx context.Context, cmd string, body map[string]any, operation string) (map[string]any, error) {
	response, err := a.sendRequest(ctx, cmd, body)
	if err != nil {
		return nil, err
	}
	if err := raiseForWeComError(response, operation); err != nil {
		return nil, err
	}
	return response, nil
}

// Content of the container object under key, trimmed
func contentOf(m map[string]any, key string) string { return strAt(objAt(m, key), "content") }

// Message text and the quoted text of a callback body
func extractText(body map[string]any) (string, string) {
	msgtype := strings.ToLower(textOf(body["msgtype"]))
	var parts []string
	if msgtype == "mixed" {
		items, _ := objAt(body, "mixed")["msg_item"].([]any)
		for _, item := range items {
			if m, ok := item.(map[string]any); ok && strings.ToLower(textOf(m["msgtype"])) == "text" {
				parts = append(parts, contentOf(m, "text"))
			}
		}
	} else {
		parts = append(parts, contentOf(body, "text"))
		if msgtype == "voice" {
			parts = append(parts, contentOf(body, "voice"))
		}
		if msgtype == "appmsg" {
			parts = append(parts, strAt(objAt(body, "appmsg"), "title"))
		}
	}
	quote := objAt(body, "quote")
	reply := ""
	if qt := strings.ToLower(textOf(quote["msgtype"])); qt == "text" || qt == "voice" {
		reply = contentOf(quote, qt)
	}
	var kept []string
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	return strings.TrimSpace(strings.Join(kept, "\n")), reply
}

// Message type of a callback from its attachments and msgtype
func deriveMessageType(body map[string]any, text string, mediaTypes []string) gateway.MessageType {
	for _, t := range mediaTypes {
		if strings.HasPrefix(t, "application/") || strings.HasPrefix(t, "text/") {
			return gateway.MessageTypeDocument
		}
	}
	for _, t := range mediaTypes {
		if strings.HasPrefix(t, "image/") {
			if text != "" {
				return gateway.MessageTypeText
			}
			return gateway.MessageTypePhoto
		}
	}
	if strings.ToLower(textOf(body["msgtype"])) == "voice" {
		return gateway.MessageTypeVoice
	}
	return gateway.MessageTypeText
}

// Optional string, nil for empty
func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// Turns one message callback into a gateway event after dedup and the DM and group policy gates
func (a *WeComAdapter) onMessage(ctx context.Context, payload map[string]any) {
	body, ok := payload["body"].(map[string]any)
	if !ok {
		return
	}
	name := a.Name()
	reqID := payloadReqID(payload)
	msgID := textOf(body["msgid"])
	if msgID == "" {
		msgID = reqID
	}
	if msgID == "" {
		msgID = randHex(16)
	}
	senderID := strAt(objAt(body, "from"), "userid")
	if a.dedup.IsDuplicate(msgID) {
		slog.Info(fmt.Sprintf("[%s] Duplicate message %s ignored (dedup drop) req_id=%s sender=%q chattype=%q", name, msgID, reqID, senderID, textOf(body["chattype"])))
		return
	}
	if reqID != "" {
		a.mu.Lock()
		a.replyReqIDs.put(msgID, reqID)
		a.mu.Unlock()
	}
	chatID := strAt(body, "chatid")
	if chatID == "" {
		chatID = senderID
	}
	slog.Info(fmt.Sprintf("[%s] Inbound callback: chattype=%q chatid=%q sender=%q msgtype=%q has_chatid=%t", name,
		textOf(body["chattype"]), textOf(body["chatid"]), senderID, textOf(body["msgtype"]), isSet(body["chatid"])))
	if chatID == "" {
		slog.Info(fmt.Sprintf("[%s] Missing chat id, skipping message", name))
		return
	}
	isGroup := strings.ToLower(textOf(body["chattype"])) == "group"
	if !a.admitInbound(ctx, isGroup, chatID, senderID) {
		return
	}
	a.rememberChatReqID(chatID, reqID)
	text, replyText := extractText(body)
	if isGroup && text != "" {
		text = strings.TrimSpace(mentionPrefixRE.ReplaceAllString(text, ""))
	}
	mediaURLs, mediaTypes := a.extractMedia(ctx, body)
	messageType := deriveMessageType(body, text, mediaTypes)
	hasReplyContext := replyText != "" && (text != "" || len(mediaURLs) > 0)
	if replyText != "" && !hasReplyContext {
		text = replyText
	}
	if text == "" && len(mediaURLs) == 0 {
		slog.Info(fmt.Sprintf("[%s] Empty WeCom message skipped: is_group=%t chat=%s msgtype=%q", name, isGroup, chatID, textOf(body["msgtype"])))
		return
	}
	chatType := "dm"
	if isGroup {
		chatType = "group"
	}
	source := a.BuildSource(ctx, chatID, gateway.SourceFields{ChatType: chatType, UserID: optional(senderID), UserName: optional(senderID), MessageID: &msgID})
	event := gateway.NewMessageEvent(text)
	event.MessageType, event.Source, event.RawMessage, event.MessageID = messageType, source, payload, &msgID
	event.MediaURLs, event.MediaTypes, event.Timestamp = mediaURLs, mediaTypes, time.Now().UTC()
	if hasReplyContext {
		quoteID := "quote:" + msgID
		event.ReplyToMessageID, event.ReplyToText = &quoteID, &replyText
	}
	key, err := a.TextBatchKey(ctx, event)
	if err != nil {
		slog.Warn(fmt.Sprintf("[%s] Could not key inbound message %s: %v", name, msgID, err))
		return
	}
	delay, _ := a.TextBatchDelays()
	attachmentOnly := len(mediaURLs) > 0 && strings.TrimSpace(text) == ""
	batch := messageType == gateway.MessageTypeText && (delay > 0 || a.PendingTextBatch(key) != nil)
	if batch || (attachmentOnly && a.attachmentMergeDelay > 0) {
		err = a.EnqueueTextEvent(ctx, event)
	} else {
		err = a.HandleMessage(ctx, event)
	}
	if err != nil {
		slog.Warn(fmt.Sprintf("[%s] Inbound message %s failed: %v", name, msgID, err))
	}
}

// DM and group intake policy, every drop logged
func (a *WeComAdapter) admitInbound(ctx context.Context, isGroup bool, chatID, senderID string) bool {
	if !isGroup {
		allowed, err := a.IsDMIntakeAllowed(ctx, senderID)
		if err != nil {
			slog.Warn(fmt.Sprintf("[%s] DM policy check failed for %s: %v", a.Name(), senderID, err))
			return false
		}
		if !allowed {
			slog.Info(fmt.Sprintf("[%s] DM sender %s blocked by policy", a.Name(), senderID))
		}
		return allowed
	}
	a.mu.Lock()
	a.groupChats[chatID] = true
	a.mu.Unlock()
	allowed, err := a.IsGroupAllowed(ctx, chatID, senderID)
	if err != nil {
		slog.Warn(fmt.Sprintf("[%s] Group policy check failed for %s: %v", a.Name(), chatID, err))
		return false
	}
	if !allowed {
		slog.Info(fmt.Sprintf("[%s] Group message DROPPED by policy: chat=%s sender=%s group_policy=%q (set group_policy to 'open' or add to group_allow_from to receive)",
			a.Name(), chatID, senderID, a.GroupPolicy))
	}
	return allowed
}

// Chat level group policy, then the per group allow_from restricting senders
func (a *WeComAdapter) IsGroupAllowed(ctx context.Context, chatID, senderID string) (bool, error) {
	allowed, err := a.OwnAccessPolicyMixin.IsGroupAllowed(ctx, chatID, senderID)
	if err != nil || !allowed {
		return false, err
	}
	group := a.resolveGroupConfig(chatID)
	senders := coerceList(firstSet(group, "allow_from", "allowFrom"))
	if len(senders) == 0 {
		return true, nil
	}
	return entryMatches(senders, senderID), nil
}

// Per group config by exact key, then case insensitive key, then the star key
func (a *WeComAdapter) resolveGroupConfig(chatID string) map[string]any {
	if exact, ok := a.groups[chatID].(map[string]any); ok {
		return exact
	}
	lowered := strings.ToLower(chatID)
	keys := make([]string, 0, len(a.groups))
	for k := range a.groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if v, ok := a.groups[k].(map[string]any); ok && strings.ToLower(k) == lowered {
			return v
		}
	}
	if star, ok := a.groups["*"].(map[string]any); ok {
		return star
	}
	return map[string]any{}
}

// Caches the chat's latest inbound req_id, a fresh one also reopening its expired stream
func (a *WeComAdapter) rememberChatReqID(chatID, reqID string) {
	chatID, reqID = strings.TrimSpace(chatID), strings.TrimSpace(reqID)
	if chatID == "" || reqID == "" {
		return
	}
	a.mu.Lock()
	a.lastChatReqIDs.put(chatID, reqID)
	a.mu.Unlock()
	a.Streamer.clearExpired(chatID)
}

// Req_id of the explicit reply target, else the chat's last inbound req_id
func (a *WeComAdapter) cachedReplyReqID(chatID, replyTo string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if target := strings.TrimSpace(replyTo); target != "" && !strings.HasPrefix(target, "quote:") {
		if id := a.replyReqIDs.get(target); id != "" {
			return id
		}
	}
	return a.lastChatReqIDs.get(strings.TrimSpace(chatID))
}

// Whether the chat is a group, groups accepting only passive replies
func (a *WeComAdapter) isGroupChat(chatID string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.groupChats[chatID]
}

// Forgets the req_ids of a lost subscription without closing the socket, a second connection kicking the first
func (a *WeComAdapter) purgeStaleReqIDs() {
	slog.Warn(fmt.Sprintf("[%s] Got errcode %d (subscription lost) — clearing stale state", a.Name(), errcodeNotSubscribed))
	a.mu.Lock()
	a.lastChatReqIDs.clear()
	a.replyReqIDs.clear()
	a.mu.Unlock()
}

// Markdown message body capped at the message length
func markdownBody(content string) map[string]any {
	runes := []rune(content)
	return map[string]any{"msgtype": "markdown", "markdown": map[string]any{"content": string(runes[:min(len(runes), maxMessageLength)])}}
}

// Passive markdown reply to an inbound req_id, failing on a WeCom errcode
func (a *WeComAdapter) sendReplyMarkdown(ctx context.Context, replyReqID, content string) (map[string]any, error) {
	response, err := a.sendReplyRequest(ctx, replyReqID, markdownBody(content))
	if err != nil {
		return nil, err
	}
	if err := raiseForWeComError(response, "send reply markdown"); err != nil {
		return nil, err
	}
	return response, nil
}

// Proactive markdown message to a chat
func (a *WeComAdapter) sendProactiveMarkdown(ctx context.Context, chatID, content string) (map[string]any, error) {
	body := markdownBody(content)
	body["chatid"] = chatID
	return a.sendRequest(ctx, cmdSend, body)
}

// Markdown split at 4000 characters, each chunk queued per chat under the 30 a minute limit, approvals on the control lane
func (a *WeComAdapter) Send(ctx context.Context, chatID, content, replyTo string, metadata map[string]any) (gateway.SendResult, error) {
	if chatID == "" {
		return gateway.SendResult{Success: false, Error: "chat_id is required"}, nil
	}
	isControl := truthy(metadata["is_approval_prompt"])
	forceProactive := truthy(metadata["force_proactive_send"])
	delete(metadata, "is_approval_prompt")
	delete(metadata, "force_proactive_send")
	chunks := gateway.TruncateMessage(content, maxMessageLength, nil)
	result, err := gateway.SendChunks(ctx, chunks, func(ctx context.Context, chunk string) (*gateway.SendResult, error) {
		value, err := a.sendQueue().EnqueueChatSend(ctx, chatID, func(qctx context.Context) (any, error) {
			return a.sendInner(qctx, chatID, chunk, replyTo, forceProactive), nil
		}, isControl)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return &gateway.SendResult{Success: false, Error: err.Error()}, nil
		}
		r := value.(gateway.SendResult)
		return &r, nil
	})
	if err != nil || result == nil {
		return gateway.SendResult{}, err
	}
	return *result, nil
}

// One markdown chunk as a passive reply when a req_id is cached, proactive otherwise, groups requiring the passive path
func (a *WeComAdapter) sendInner(ctx context.Context, chatID, content, replyTo string, forceProactive bool) gateway.SendResult {
	isGroup := a.isGroupChat(chatID)
	reqID := ""
	if !forceProactive || isGroup {
		reqID = a.cachedReplyReqID(chatID, replyTo)
	}
	var response map[string]any
	var err error
	switch {
	case reqID != "":
		if response, err = a.sendReplyMarkdown(ctx, reqID, content); err != nil && ctx.Err() == nil {
			slog.Warn(fmt.Sprintf("[%s] Passive reply failed (%v), falling back to proactive send", a.Name(), err))
			response, err = a.sendProactiveMarkdown(ctx, chatID, content)
		}
	case isGroup:
		slog.Warn(fmt.Sprintf("[%s] No cached req_id for group chat %s — cannot send (groups require passive reply via req_id)", a.Name(), chatID))
		return gateway.SendResult{Success: false, Error: "No req_id available for group chat (passive reply required)"}
	default:
		response, err = a.sendProactiveMarkdown(ctx, chatID, content)
	}
	if err != nil {
		if errors.Is(err, errRequestTimeout) {
			return gateway.SendResult{Success: false, Error: "Timeout sending message to WeCom"}
		}
		slog.Error(fmt.Sprintf("[%s] Send failed: %v", a.Name(), err))
		return a.sendFailure(err.Error(), strings.Contains(err.Error(), fmt.Sprint(errcodeNotSubscribed)))
	}
	if e := responseError(response); e != "" {
		code, _ := errcodeOf(response)
		return a.sendFailure(e, code == errcodeNotSubscribed)
	}
	id := payloadReqID(response)
	if id == "" {
		id = randHex(6)
	}
	return gateway.SendResult{Success: true, MessageID: id, RawResponse: response}
}

// Failed send result, a lost subscription purging the stale req_ids so later sends recover
func (a *WeComAdapter) sendFailure(errText string, subscriptionLost bool) gateway.SendResult {
	if subscriptionLost {
		a.purgeStaleReqIDs()
	}
	return gateway.SendResult{Success: false, Error: errText}
}

// Chat info of a chat, group ids reading as groups
func (a *WeComAdapter) GetChatInfo(ctx context.Context, chatID string) (map[string]any, error) {
	chatType := "dm"
	if strings.HasPrefix(strings.ToLower(chatID), "group") {
		chatType = "group"
	}
	return map[string]any{"name": chatID, "type": chatType}, nil
}

// Buffers a text or attachment event, real text joining a held attachment promoting the batch to text with its quote
func (a *WeComAdapter) EnqueueTextEvent(ctx context.Context, event *gateway.MessageEvent) error {
	return a.RunLoopTurn(ctx, func(ctx context.Context) error {
		key, err := a.TextBatchKey(ctx, event)
		if err != nil {
			return err
		}
		existing := a.PendingTextBatch(key)
		if err := a.BasePlatformAdapter.EnqueueTextEvent(ctx, event); err != nil {
			return err
		}
		if existing != nil && existing != event && strings.TrimSpace(event.Text) != "" {
			existing.MessageType = gateway.MessageTypeText
			if event.ReplyToText != nil && *event.ReplyToText != "" && (existing.ReplyToText == nil || *existing.ReplyToText == "") {
				existing.ReplyToText, existing.ReplyToMessageID = event.ReplyToText, event.ReplyToMessageID
			}
		}
		return nil
	})
}

// Quiet period of a batch, an attachment only batch waiting for its trailing text frame
func (a *WeComAdapter) TextBatchDelayFor(pending *gateway.MessageEvent) float64 {
	if pending != nil && len(pending.MediaURLs) > 0 && strings.TrimSpace(pending.Text) == "" {
		return a.attachmentMergeDelay
	}
	delay, split := a.TextBatchDelays()
	if pending != nil && pending.LastChunkLen() >= splitThreshold {
		return split
	}
	return delay
}
