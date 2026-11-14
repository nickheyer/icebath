package wecom

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/dlclark/regexp2/v2"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyaiohttpws"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyre"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const (
	defaultWSURL = "wss://openws.work.weixin.qq.com"

	appCmdSubscribe      = "aibot_subscribe"
	appCmdCallback       = "aibot_msg_callback"
	appCmdLegacyCallback = "aibot_callback"
	appCmdEventCallback  = "aibot_event_callback"
	appCmdPing           = "ping"

	maxMessageLength         = 4000
	connectTimeoutSeconds    = 20.0
	requestTimeoutSeconds    = 15.0
	heartbeatIntervalSeconds = 30.0
	dedupMaxSize             = 1000
	wecomSplitThreshold      = 3900
)

var (
	callbackCommands    = map[string]bool{appCmdCallback: true, appCmdLegacyCallback: true}
	nonResponseCommands = map[string]bool{appCmdCallback: true, appCmdLegacyCallback: true, appCmdEventCallback: true}
	reconnectBackoff    = []float64{2, 5, 10, 30, 60}

	wecomPrefixRE  = mustIgnoreCase(`^wecom:`)
	entryKindRE    = mustIgnoreCase(`^(user|group):`)
	groupMentionRE = pyre.MustCompile(`^@\S+\s*`)
)

// Case insensitive Python pattern, panicking on a pattern that does not compile
func mustIgnoreCase(pattern string) *regexp2.Regexp {
	re, err := pyre.CompileIgnoreCase(pattern)
	if err != nil {
		panic(err)
	}
	return re
}

// Every match of re in s replaced by repl, s unchanged when matching fails
func reSub(re *regexp2.Regexp, s, repl string) string {
	out, err := re.Replace(s, repl, -1, -1)
	if err != nil {
		return s
	}
	return out
}

// Python exception carried as a Go error with its class name
type pyErr struct{ kind, msg string }

func (e *pyErr) Error() string      { return e.msg }
func (e *pyErr) PyTypeName() string { return e.kind }

func runtimeError(msg string) error { return &pyErr{"RuntimeError", msg} }

// Whether the error is the TimeoutError of an awaited ack
func isTimeout(err error) bool {
	var pe *pyErr
	return errors.As(err, &pe) && pe.kind == "TimeoutError"
}

// Whether the error is a RuntimeError or one of its subclasses
func isRuntimeError(err error) bool {
	var pe *pyErr
	if errors.As(err, &pe) {
		return pe.kind == "RuntimeError"
	}
	var expired *WeComStreamExpiredError
	return errors.As(err, &expired)
}

// str of an exception as a percent s log argument renders it
func errText(err error) string { return pyrepr.ExceptionMessage(err) }

// Thirty two lowercase hex digits of a random UUID
func uuidHex() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return hex.EncodeToString(b)
}

// WeCom needs no optional dependency in this build
func checkWeComRequirements() bool { return true }

// Config value of none, comma text, a sequence or a scalar as a trimmed list without empty items
func coerceList(value any) []string {
	var items []any
	switch v := value.(type) {
	case string:
		for _, part := range strings.Split(v, ",") {
			items = append(items, part)
		}
	case []any:
		items = v
	case []string:
		for _, s := range v {
			items = append(items, s)
		}
	case nil:
	default:
		items = []any{v}
	}
	out := []string{}
	for _, item := range items {
		if s := pystr.Strip(pyrepr.Str(item)); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// Allowlist entry without its wecom and user or group prefixes
func normalizeEntry(raw string) string {
	value := reSub(wecomPrefixRE, pystr.Strip(raw), "")
	return pystr.Strip(reSub(entryKindRE, value, ""))
}

// Case insensitive allowlist match where a star entry matches anyone
func entriesMatch(entries []string, target string) bool {
	normalizedTarget := pystr.Lower(pystr.Strip(target))
	for _, e := range entries {
		if n := pystr.Lower(normalizeEntry(e)); n == "*" || n == normalizedTarget {
			return true
		}
	}
	return false
}

// Object under key, an empty object when the value is not one
func dictOrEmpty(container *pyjson.Object, key string) *pyjson.Object {
	if container != nil {
		if o, ok := container.Vals[key].(*pyjson.Object); ok && o != nil {
			return o
		}
	}
	return pyjson.NewObject()
}

// Stripped content text of the object under key
func contentOf(container *pyjson.Object, key string) string {
	return textOf(dictOrEmpty(container, key).Vals["content"])
}

// Lowercased str of a falsy defaulted value
func lowerOf(v any) string {
	if !pyrepr.Truthy(v) {
		return ""
	}
	return pystr.Lower(pyrepr.Str(v))
}

// Class attributes of the WeCom adapter
func adapterTraits() gateway.AdapterTraits {
	t := gateway.DefaultAdapterTraits()
	t.MaxMessageLength = maxMessageLength
	t.SupportsMessageEditing = false
	t.SupportsNativeStreaming = true
	t.SplitsLongMessages = true
	t.SerialReceiveLoop = true
	return t
}

// WeComAdapter is the WeCom AI Bot adapter over one persistent openws websocket
type WeComAdapter struct {
	*gateway.BasePlatformAdapter
	*gateway.OwnAccessPolicyMixin
	stream *Streamer

	botID, secret, wsURL            string
	allowFrom, groupAllowFrom       []string
	groups                          map[string]any
	attachmentTextMergeDelaySeconds float64
	deviceID                        string

	mu               sync.Mutex
	queue            *ChatSendQueue
	ws               *pyaiohttpws.Conn
	httpClient       *http.Client
	listenTask       *gateway.AdapterTask
	heartbeatTask    *gateway.AdapterTask
	pendingResponses map[string]*replyFuture
	dedup            *gateway.MessageDeduplicator
	replyReqIDs      *pydict.Dict[string, string]
	lastChatReqIDs   *pydict.Dict[string, string]
	groupChatIDs     map[string]bool
}

// Adapter configured from the platform extra and the profile scoped WECOM env
func NewWeComAdapter(ctx context.Context, cfg *gateway.PlatformConfig) (*WeComAdapter, error) {
	if cfg == nil {
		cfg = gateway.NewPlatformConfig()
	}
	a := &WeComAdapter{pendingResponses: map[string]*replyFuture{}, replyReqIDs: pydict.New[string, string](),
		lastChatReqIDs: pydict.New[string, string](), groupChatIDs: map[string]bool{}, deviceID: uuidHex()}
	a.BasePlatformAdapter = gateway.NewBasePlatformAdapter(cfg, gateway.PlatformWeCom, a)
	a.Traits = adapterTraits()
	policy, err := gateway.NewOwnAccessPolicyMixin("WeComAdapter", "WECOM")
	if err != nil {
		return nil, err
	}
	a.OwnAccessPolicyMixin = policy
	extra := cfg.Extra
	if extra == nil {
		extra = map[string]any{}
	}
	extraFloat := func(key string, def float64) float64 {
		raw, ok := extra[key]
		if !ok {
			return def
		}
		f, err := pyint.Float(raw)
		if err != nil {
			return def
		}
		return f
	}
	setting := func(env, def string, keys ...string) (string, error) {
		for _, k := range keys {
			if v := extra[k]; pyrepr.Truthy(v) {
				return pystr.Strip(pyrepr.Str(v)), nil
			}
		}
		if env == "" {
			return "", nil
		}
		secret, err := gateway.GetScopedSecret(ctx, env, def, false)
		return pystr.Strip(secret), err
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
	a.DMPolicy = pystr.Lower(dmPolicy)
	allowRaw := pyrepr.Or(extra["allow_from"], extra["allowFrom"])
	if !pyrepr.Truthy(allowRaw) {
		secret, err := gateway.GetScopedSecret(ctx, "WECOM_ALLOWED_USERS", "", false)
		if err != nil {
			return nil, err
		}
		allowRaw = secret
	}
	a.allowFrom = coerceList(allowRaw)
	groupPolicy, err := setting("WECOM_GROUP_POLICY", "pairing", "group_policy")
	if err != nil {
		return nil, err
	}
	a.GroupPolicy = pystr.Lower(groupPolicy)
	a.groupAllowFrom = coerceList(pyrepr.Or(extra["group_allow_from"], extra["groupAllowFrom"]))
	for _, entry := range a.allowFrom {
		a.AllowFrom[entry] = true
	}
	for _, entry := range a.groupAllowFrom {
		a.GroupAllowFrom[entry] = true
	}
	a.OwnAccessPolicyMixin.EntryMatches = func(_ context.Context, entries map[string]bool, target string) (bool, error) {
		list := make([]string, 0, len(entries))
		for entry, present := range entries {
			if present {
				list = append(list, entry)
			}
		}
		return entriesMatch(list, target), nil
	}
	a.groups, _ = extra["groups"].(map[string]any)
	if a.groups == nil {
		a.groups = map[string]any{}
	}
	a.dedup = gateway.NewMessageDeduplicator(dedupMaxSize, 300)
	a.SetTextBatchDelays(config.EnvFloat("ICEBATH_WECOM_TEXT_BATCH_DELAY_SECONDS", 0.6), config.EnvFloat("ICEBATH_WECOM_TEXT_BATCH_SPLIT_DELAY_SECONDS", 2.0))
	a.attachmentTextMergeDelaySeconds = extraFloat("attachment_text_merge_delay_seconds", 0.8)
	a.stream = &Streamer{Host: a,
		SafeDurationSeconds:      extraFloat("stream_safe_duration_seconds", StreamSafeDurationSeconds),
		KeepaliveEnabled:         pyrepr.Truthy(extraOr(extra, "stream_keepalive_enabled", StreamKeepaliveEnabledDefault)),
		KeepaliveIntervalSeconds: extraFloat("stream_keepalive_interval_seconds", StreamKeepaliveIntervalSeconds)}
	a.queue = NewChatSendQueue(a.Name())
	return a, nil
}

// Value of an extra key, the default when the key is absent
func extraOr(extra map[string]any, key string, def any) any {
	if v, ok := extra[key]; ok {
		return v
	}
	return def
}

// The adapter gates DM and group access itself
func (a *WeComAdapter) EnforcesOwnAccessPolicy() bool {
	return a.OwnAccessPolicyMixin.EnforcesOwnAccessPolicy()
}

// Records a retryable fatal startup error, logs it and reports the failed connect
func (a *WeComAdapter) startupFailure(ctx context.Context, code, message string) bool {
	a.SetFatalError(ctx, code, message, true)
	slog.Warn(fmt.Sprintf("[%s] %s", a.Name(), message))
	return false
}

// Opens the websocket, subscribes the bot and starts the listen and heartbeat loops
func (a *WeComAdapter) Connect(ctx context.Context, isReconnect bool) (bool, error) {
	if a.botID == "" || a.secret == "" {
		return a.startupFailure(ctx, "wecom_missing_credentials", "WeCom startup failed: WECOM_BOT_ID and WECOM_SECRET are required"), nil
	}
	client := gateway.PlatformHTTPClient(ctx, gateway.PlatformHTTPClientOptions{})
	a.mu.Lock()
	a.httpClient = client
	a.mu.Unlock()
	if err := a.openConnection(ctx); err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		a.SetFatalError(ctx, "wecom_connect_error", "WeCom startup failed: "+errText(err), true)
		slog.Error(fmt.Sprintf("[%s] Failed to connect: %s", a.Name(), errText(err)), "error", err)
		a.teardown(ctx)
		return false, nil
	}
	a.MarkConnected(ctx, "")
	listen := gateway.SpawnAdapterTask(ctx, a.listenLoop)
	heartbeat := gateway.SpawnAdapterTask(ctx, a.heartbeatLoop)
	a.mu.Lock()
	a.listenTask, a.heartbeatTask = listen, heartbeat
	a.mu.Unlock()
	slog.Info(fmt.Sprintf("[%s] Connected to %s", a.Name(), a.wsURL))
	a.WirePluginHandlers(ctx, nil)
	return true, nil
}

// Stops the send workers and loops, fails every waiter, closes the transport and clears the dedup cache
func (a *WeComAdapter) Disconnect(ctx context.Context) error {
	a.SetRunning(false)
	a.MarkDisconnected(ctx)
	a.mu.Lock()
	queue := a.queue
	a.queue = NewChatSendQueue(a.Name())
	listen, heartbeat := a.listenTask, a.heartbeatTask
	a.listenTask, a.heartbeatTask = nil, nil
	a.mu.Unlock()
	queue.Shutdown()
	for _, task := range []*gateway.AdapterTask{listen, heartbeat} {
		gateway.CancelTask(ctx, task)
	}
	a.failAll(runtimeError("WeCom adapter disconnected"))
	a.teardown(ctx)
	a.dedup.Clear()
	slog.Info(fmt.Sprintf("[%s] Disconnected", a.Name()))
	return nil
}

// Fails every pending response and reply ack with the error
func (a *WeComAdapter) failAll(err error) {
	a.failPendingResponses(err)
	a.stream.FailReplyQueues(err)
}

// Closes the live websocket when one is open and forgets it
func (a *WeComAdapter) cleanupWS(ctx context.Context) {
	a.mu.Lock()
	ws := a.ws
	a.ws = nil
	a.mu.Unlock()
	if ws != nil && !ws.Closed() {
		ws.Close(ctx)
	}
}

// Closes the websocket and then the HTTP client
func (a *WeComAdapter) teardown(ctx context.Context) {
	a.cleanupWS(ctx)
	a.mu.Lock()
	client := a.httpClient
	a.httpClient = nil
	a.mu.Unlock()
	if client != nil {
		client.CloseIdleConnections()
	}
}

// Live websocket, nil while disconnected
func (a *WeComAdapter) socket() *pyaiohttpws.Conn {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.ws
}

// Dials the websocket and completes the subscribe handshake, an error for a refused authentication
func (a *WeComAdapter) openConnection(ctx context.Context) error {
	a.cleanupWS(ctx)
	ws, err := pyaiohttpws.Connect(ctx, a.wsURL, pyaiohttpws.Options{Heartbeat: time.Duration(heartbeatIntervalSeconds * 2 * float64(time.Second)),
		CloseTimeout: time.Duration(connectTimeoutSeconds * float64(time.Second)), TrustEnv: gateway.GatewayTrustEnv(ctx)})
	if err != nil {
		return err
	}
	a.mu.Lock()
	a.ws = ws
	a.mu.Unlock()
	reqID := newReqID("subscribe")
	if err := a.SendJSON(ctx, obj("cmd", appCmdSubscribe, "headers", obj("req_id", reqID),
		"body", obj("bot_id", a.botID, "secret", a.secret, "device_id", a.deviceID))); err != nil {
		return err
	}
	authPayload, err := a.waitForHandshake(ctx, reqID)
	if err != nil {
		return err
	}
	errcode := getOr(authPayload, "errcode", int64(0))
	if errcode != nil && !pyjson.Equal(errcode, int64(0)) {
		return runtimeError(fmt.Sprintf("%s (errcode=%s)", pyrepr.Str(getOr(authPayload, "errmsg", "authentication failed")), pyrepr.Str(errcode)))
	}
	return nil
}

// Waits for the subscribe acknowledgement, skipping pings and unrelated frames until the connect timeout
func (a *WeComAdapter) waitForHandshake(ctx context.Context, reqID string) (*pyjson.Object, error) {
	ws := a.socket()
	if ws == nil {
		return nil, runtimeError("WebSocket not initialized")
	}
	deadline := time.Now().Add(time.Duration(connectTimeoutSeconds * float64(time.Second)))
	for time.Until(deadline) > 0 {
		rctx, cancel := context.WithDeadline(ctx, deadline)
		msg, err := ws.Receive(rctx)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, &pyErr{"TimeoutError", ""}
		}
		switch msg.Type {
		case pyaiohttpws.Text:
			payload := parseJSON(msg.Data)
			if payload == nil || len(payload.Keys) == 0 || pyrepr.Equal(payload.Vals["cmd"], appCmdPing) {
				continue
			}
			if payloadReqID(payload) == reqID {
				return payload, nil
			}
			slog.Debug(fmt.Sprintf("[%s] Ignoring pre-auth payload: %s", a.Name(), pyrepr.Str(payload.Vals["cmd"])))
		case pyaiohttpws.Closed, pyaiohttpws.Close, pyaiohttpws.Error:
			return nil, runtimeError("WeCom websocket closed during authentication")
		}
	}
	return nil, &pyErr{"TimeoutError", "Timed out waiting for WeCom subscribe acknowledgement"}
}

// Reads events until the adapter stops, reconnecting with the backoff ladder after a websocket error
func (a *WeComAdapter) listenLoop(ctx context.Context) error {
	backoffIdx := 0
	for a.IsConnected() {
		err := a.readEvents(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if err == nil {
			backoffIdx = 0
			continue
		}
		if !a.IsConnected() {
			return nil
		}
		slog.Warn(fmt.Sprintf("[%s] WebSocket error: %s", a.Name(), errText(err)))
		a.failAll(runtimeError("WeCom connection interrupted"))
		if sendSleep(ctx, reconnectBackoff[min(backoffIdx, len(reconnectBackoff)-1)]) != nil {
			return nil
		}
		backoffIdx++
		if err := a.openConnection(ctx); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			slog.Warn(fmt.Sprintf("[%s] Reconnect failed: %s", a.Name(), errText(err)))
			continue
		}
		backoffIdx = 0
		a.MarkConnected(ctx, "")
		slog.Info(fmt.Sprintf("[%s] Reconnected", a.Name()))
	}
	return nil
}

// Dispatches each text or binary frame until the websocket closes or the adapter stops
func (a *WeComAdapter) readEvents(ctx context.Context) error {
	if a.socket() == nil {
		return runtimeError("WebSocket not connected")
	}
	for a.IsConnected() {
		ws := a.socket()
		if ws == nil || ws.Closed() {
			break
		}
		msg, err := ws.Receive(ctx)
		if err != nil {
			return err
		}
		switch msg.Type {
		case pyaiohttpws.Text, pyaiohttpws.Binary:
			if err := a.handleFrame(ctx, msg.Data, msg.Type == pyaiohttpws.Binary); err != nil {
				return err
			}
		case pyaiohttpws.Close, pyaiohttpws.Closed, pyaiohttpws.Error, pyaiohttpws.Closing:
			return runtimeError("WeCom websocket closed")
		default:
			slog.Info(fmt.Sprintf("[%s] Inbound frame ignored: WSMsgType=%s", a.Name(), msg.Type))
		}
	}
	return nil
}

// Length of a frame in characters for text and bytes for binary
func frameLen(data any) int {
	switch d := data.(type) {
	case string:
		return pystr.Len(d)
	case []byte:
		return len(d)
	}
	return -1
}

// Parses one frame and dispatches it, logging every drop
func (a *WeComAdapter) handleFrame(ctx context.Context, data any, isBinary bool) error {
	dataLen := frameLen(data)
	if isBinary {
		decoded := "<undecodable>"
		if b, ok := data.([]byte); ok {
			decoded = pystr.DecodeUTF8Replace(b)
		}
		slog.Info(fmt.Sprintf("[%s] Inbound BINARY frame received (len=%d) head=%s — attempting JSON parse", a.Name(), dataLen, pyrepr.Repr(pystr.Slice(decoded, 0, 200))))
	}
	payload := parseJSON(data)
	switch {
	case payload != nil && len(payload.Keys) > 0:
		return a.dispatchPayload(ctx, payload)
	case isBinary:
		slog.Info(fmt.Sprintf("[%s] BINARY frame not parseable as JSON — dropped", a.Name()))
	default:
		slog.Info(fmt.Sprintf("[%s] Inbound TEXT frame dropped (unparseable/non-dict) len=%d", a.Name(), dataLen))
	}
	return nil
}

// Sends an application ping every heartbeat interval while the websocket is open
func (a *WeComAdapter) heartbeatLoop(ctx context.Context) error {
	for a.IsConnected() {
		if sendSleep(ctx, heartbeatIntervalSeconds) != nil {
			return nil
		}
		if ws := a.socket(); ws != nil && !ws.Closed() {
			if err := a.SendJSON(ctx, obj("cmd", appCmdPing, "headers", obj("req_id", newReqID("ping")), "body", pyjson.NewObject())); err != nil {
				slog.Debug(fmt.Sprintf("[%s] Heartbeat send failed: %s", a.Name(), errText(err)))
			}
		}
	}
	return nil
}

// Python list repr of strings
func listRepr(items []string) string {
	vals := make([]any, len(items))
	for i, s := range items {
		vals[i] = s
	}
	return pyrepr.Repr(vals)
}

// Routes one payload to a reply ack, a pending response, the message handler or the kick handling
func (a *WeComAdapter) dispatchPayload(ctx context.Context, payload *pyjson.Object) error {
	reqID := payloadReqID(payload)
	cmd := ""
	if v := payload.Vals["cmd"]; pyrepr.Truthy(v) {
		cmd = pyrepr.Str(v)
	}
	bodyDict, _ := payload.Vals["body"].(*pyjson.Object)
	orLabel := func(s, label string) string {
		if s == "" {
			return label
		}
		return s
	}
	if queues := a.stream.replyQueueKeys(); len(queues) > 0 && cmd != appCmdPing {
		slog.Debug(fmt.Sprintf("[%s] _dispatch_payload[ALL]: req_id=%s cmd=%s active_queues=%s", a.Name(), orLabel(reqID, "(none)"),
			pyrepr.Repr(orLabel(cmd, "(empty)")), listRepr(queues)))
	}
	if exists, pendingAck := a.stream.replyQueueState(reqID); reqID != "" && exists {
		errcode := "N/A"
		if bodyDict != nil {
			errcode = pyrepr.Str(getOr(bodyDict, "errcode", "N/A"))
		}
		slog.Debug(fmt.Sprintf("[%s] _dispatch_payload: req_id=%s cmd=%s has_pending_ack=%s errcode=%s in_NON_RESPONSE=%s payload_keys=%s", a.Name(), reqID,
			pyrepr.Repr(cmd), pyrepr.Str(pendingAck), errcode, pyrepr.Str(nonResponseCommands[cmd]), listRepr(payload.Keys)))
	}
	if reqID != "" && !nonResponseCommands[cmd] {
		if a.stream.ResolveReplyAck(reqID, payload) {
			return nil
		}
		a.mu.Lock()
		future, pending := a.pendingResponses[reqID]
		a.mu.Unlock()
		if pending {
			if future != nil && !future.isDone() {
				future.finish(payload, nil, false)
			}
			return nil
		}
	}
	switch {
	case callbackCommands[cmd]:
		return a.onMessage(ctx, payload)
	case cmd == appCmdEventCallback:
		body := payload.Vals["body"]
		if !pyrepr.Truthy(body) {
			body = pyjson.NewObject()
		}
		eventType, err := dictGet(body, "event_type")
		if err != nil {
			return err
		}
		if pyrepr.Truthy(eventType) && pyrepr.Str(eventType) == "disconnected_event" {
			slog.Warn(fmt.Sprintf("[%s] Kicked by server (another WS connection established). Suppressing reconnect to avoid mutual kicking. Check for duplicate gateway instances.", a.Name()))
			a.SetRunning(false)
		}
	case cmd != appCmdPing:
		bodyKeys := "None"
		if bodyDict != nil {
			bodyKeys = listRepr(bodyDict.Keys)
		}
		slog.Info(fmt.Sprintf("[%s] Unrouted websocket payload dropped: cmd=%s req_id=%s body_keys=%s", a.Name(), pyrepr.Repr(orLabel(cmd, "(empty)")),
			orLabel(reqID, "(none)"), bodyKeys))
	}
	return nil
}

// Fails and forgets every pending response
func (a *WeComAdapter) failPendingResponses(err error) {
	a.mu.Lock()
	pending := a.pendingResponses
	a.pendingResponses = map[string]*replyFuture{}
	a.mu.Unlock()
	for _, future := range pending {
		if !future.isDone() {
			future.finish(nil, err, false)
		}
	}
}

// RuntimeError when the websocket is missing or closed
func (a *WeComAdapter) RequireWS() error {
	if ws := a.socket(); ws == nil || ws.Closed() {
		return runtimeError("WeCom websocket is not connected")
	}
	return nil
}

// Sends one JSON frame over the live websocket
func (a *WeComAdapter) SendJSON(ctx context.Context, payload *pyjson.Object) error {
	if err := a.RequireWS(); err != nil {
		return err
	}
	text, err := pyjson.Dumps(payload)
	if err != nil {
		return err
	}
	ws := a.socket()
	if ws == nil {
		return runtimeError("WeCom websocket is not connected")
	}
	return ws.SendStr(text)
}

// Sends a command frame and waits for the response correlated by its req id
func (a *WeComAdapter) request(ctx context.Context, cmd, reqID string, body *pyjson.Object, timeout float64) (*pyjson.Object, error) {
	future := newReplyFuture()
	a.mu.Lock()
	a.pendingResponses[reqID] = future
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		delete(a.pendingResponses, reqID)
		a.mu.Unlock()
	}()
	if err := a.SendJSON(ctx, obj("cmd", cmd, "headers", obj("req_id", reqID), "body", body)); err != nil {
		return nil, err
	}
	timer := time.NewTimer(time.Duration(timeout * float64(time.Second)))
	defer timer.Stop()
	select {
	case <-future.done:
		if future.err != nil {
			return nil, future.err
		}
		if future.cancelled {
			return nil, context.Canceled
		}
		return future.payload, nil
	case <-timer.C:
		return nil, &pyErr{"TimeoutError", ""}
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Sends a proactive command under a fresh req id
func (a *WeComAdapter) sendRequest(ctx context.Context, cmd string, body *pyjson.Object, timeout float64) (*pyjson.Object, error) {
	if err := a.RequireWS(); err != nil {
		return nil, err
	}
	return a.request(ctx, cmd, newReqID(cmd), body, timeout)
}

// Sends a reply frame correlated to an inbound callback req id
func (a *WeComAdapter) sendReplyRequest(ctx context.Context, replyReqID string, body *pyjson.Object) (*pyjson.Object, error) {
	if err := a.RequireWS(); err != nil {
		return nil, err
	}
	normalized, err := a.RequireReplyReqID(replyReqID)
	if err != nil {
		return nil, err
	}
	return a.request(ctx, AppCmdResponse, normalized, body, requestTimeoutSeconds)
}

// Stripped reply req id, a ValueError when it is blank
func (a *WeComAdapter) RequireReplyReqID(replyReqID string) (string, error) {
	normalized := pystr.Strip(replyReqID)
	if normalized == "" {
		return "", &pyErr{"ValueError", "reply_req_id is required"}
	}
	return normalized, nil
}

// Request id of a prefix and a random UUID
func newReqID(prefix string) string { return prefix + "-" + uuidHex() }

// Request id carried in the headers of a payload, empty when absent
func payloadReqID(payload *pyjson.Object) string {
	if payload == nil {
		return ""
	}
	headers, ok := payload.Vals["headers"].(*pyjson.Object)
	if !ok || headers == nil || !pyrepr.Truthy(headers.Vals["req_id"]) {
		return ""
	}
	return pyrepr.Str(headers.Vals["req_id"])
}

// Decoded object of a frame, retrying without strict control character checks, nil for anything else
func parseJSON(raw any) *pyjson.Object {
	rawLen := frameLen(raw)
	var text string
	var decodeErr error
	switch r := raw.(type) {
	case string:
		text = r
	case []byte:
		text, decodeErr = pyjson.DecodeBytes(r)
	default:
		slog.Warn(fmt.Sprintf("Failed to parse WeCom payload: error=the JSON object must be str, bytes or bytearray, not %s len=%d", pyrepr.TypeName(raw), rawLen))
		return nil
	}
	var payload any
	err := decodeErr
	if err == nil {
		payload, err = pyjson.Loads(text)
	}
	var jsonErr *pyjson.DecodeError
	switch {
	case err == nil:
	case errors.As(err, &jsonErr):
		lenientText := text
		if b, ok := raw.([]byte); ok {
			lenientText = pystr.DecodeUTF8Replace(b)
		}
		var err2 error
		if payload, err2 = pyjson.LoadsLenient(lenientText); err2 != nil {
			tail := pyrepr.Repr(raw)
			switch r := raw.(type) {
			case string:
				if pystr.Len(r) > 100 {
					tail = pyrepr.Repr(pystr.Slice(r, -100, pystr.Len(r)))
				}
			case []byte:
				if len(r) > 100 {
					tail = pyrepr.BytesRepr(r[len(r)-100:])
				}
			}
			slog.Warn(fmt.Sprintf("Failed to parse WeCom payload (strict=False also failed): error=%s len=%d tail=%s", errText(err2), rawLen, tail))
			return nil
		}
		slog.Info(fmt.Sprintf("WeCom payload required strict=False fallback (len=%d)", rawLen))
	default:
		slog.Warn(fmt.Sprintf("Failed to parse WeCom payload: error=%s len=%d", errText(err), rawLen))
		return nil
	}
	o, _ := payload.(*pyjson.Object)
	return o
}
