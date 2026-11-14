package qqbot

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyaiohttpws"
	"github.com/nickheyer/icebath/pkg/internal/pyhttpx"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const (
	typingInputSeconds      = 60
	typingDebounceSeconds   = 50
	reconnectWaitSeconds    = 15.0
	reconnectPollInterval   = 0.5
	approvalTimeoutSeconds  = 300
	defaultHeartbeatSeconds = 30.0
)

// Close codes that stop the reconnect loop for good, with what each means
var fatalCloseCodes = map[int]string{4001: "invalid opcode", 4002: "invalid payload", 4010: "invalid shard",
	4011: "sharding required", 4012: "invalid API version", 4013: "invalid intent",
	4014: "intent not authorized", 4914: "offline/sandbox-only", 4915: "banned"}

// Whether a close code invalidates the session so the next Hello must identify again
func sessionInvalidCloseCode(code int) bool {
	return code == 4006 || code == 4007 || (code >= 4900 && code < 4914)
}

// Wall clock, monotonic clock and sleep of the adapter, swappable like module globals
var (
	processStart = time.Now()
	wallClock    = func() float64 { return float64(time.Now().UnixNano()) / 1e9 }
	monotonic    = func() float64 { return time.Since(processStart).Seconds() }
	sleep        = func(ctx context.Context, seconds float64) error {
		timer := time.NewTimer(time.Duration(seconds * float64(time.Second)))
		defer timer.Stop()
		select {
		case <-timer.C:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
)

// Python exception carried as a Go error with its class name
type pyErr struct{ kind, msg string }

func (e *pyErr) Error() string      { return e.msg }
func (e *pyErr) PyTypeName() string { return e.kind }

func runtimeError(msg string) error { return &RuntimeError{Msg: msg} }

// str of an exception as a percent s log argument renders it
func errText(err error) string { return pyrepr.ExceptionMessage(err) }

// str of an exception, its class name when the message is empty
func errTextOrName(err error) string {
	if msg := errText(err); msg != "" {
		return msg
	}
	return pyrepr.ExceptionName(err)
}

// Websocket close carrying the close code and reason for the reconnect loop
type QQCloseError struct {
	Code   *int
	Reason string
}

// Close error of a received close frame, a falsy code or reason read as none
func newQQCloseError(code any, reason string) *QQCloseError {
	e := &QQCloseError{Reason: reason}
	if n, ok := code.(int); ok && n != 0 {
		e.Code = &n
	}
	return e
}

func (e *QQCloseError) Error() string {
	code := "None"
	if e.Code != nil {
		code = strconv.Itoa(*e.Code)
	}
	return fmt.Sprintf("WebSocket closed (code=%s, reason=%s)", code, e.Reason)
}

// Thirty two lowercase hex digits of a random UUID
func uuidHex() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return hex.EncodeToString(b)
}

// QQ Bot needs no optional dependency in this build
func checkQQRequirements() bool { return true }

// Class attributes of the QQ Bot adapter
func adapterTraits() gateway.AdapterTraits {
	t := gateway.DefaultAdapterTraits()
	t.SupportsMessageEditing = false
	t.MaxMessageLength = MaxMessageLength
	return t
}

// QQAdapter is the official QQ Bot adapter, the websocket gateway inbound and the REST API outbound
type QQAdapter struct {
	*gateway.BasePlatformAdapter
	*gateway.OwnAccessPolicyMixin

	appID, clientSecret string
	markdownSupport     bool

	tokenLock sync.Mutex

	mu                  sync.Mutex
	ws                  *pyaiohttpws.Conn
	httpClient          *http.Client
	listenTask          *gateway.AdapterTask
	heartbeatTask       *gateway.AdapterTask
	heartbeatInterval   float64
	sessionID           any
	lastSeq             *int64
	chatTypeMap         map[string]string
	pendingResponses    map[string]chan error
	dedup               *gateway.MessageDeduplicator
	lastMsgID           map[string]string
	typingSentAt        map[string]float64
	accessToken         string
	tokenExpiresAt      float64
	interactionCallback func(ctx context.Context, event InteractionEvent) error
}

// Stripped str of a falsy defaulted value, like str(value or "").strip()
func textOf(v any) string {
	if !pyrepr.Truthy(v) {
		return ""
	}
	return pystr.Strip(pyrepr.Str(v))
}

// Adapter configured from the platform extra and the profile scoped QQ env
func NewQQAdapter(ctx context.Context, cfg *gateway.PlatformConfig) (*QQAdapter, error) {
	if cfg == nil {
		cfg = gateway.NewPlatformConfig()
	}
	a := &QQAdapter{heartbeatInterval: defaultHeartbeatSeconds, chatTypeMap: map[string]string{}, pendingResponses: map[string]chan error{},
		lastMsgID: map[string]string{}, typingSentAt: map[string]float64{},
		dedup: gateway.NewMessageDeduplicator(DedupMaxSize, DedupWindowSeconds)}
	a.BasePlatformAdapter = gateway.NewBasePlatformAdapter(cfg, gateway.PlatformQQBot, a)
	a.Traits = adapterTraits()
	policy, err := gateway.NewOwnAccessPolicyMixin("QQAdapter", "QQ")
	if err != nil {
		return nil, err
	}
	a.OwnAccessPolicyMixin = policy
	extra := cfg.Extra
	if extra == nil {
		extra = map[string]any{}
	}
	credential := func(key, env string) (string, error) {
		if v := extra[key]; pyrepr.Truthy(v) {
			return pystr.Strip(pyrepr.Str(v)), nil
		}
		secret, err := gateway.GetScopedSecret(ctx, env, "", false)
		return pystr.Strip(secret), err
	}
	if a.appID, err = credential("app_id", "QQ_APP_ID"); err != nil {
		return nil, err
	}
	if a.clientSecret, err = credential("client_secret", "QQ_CLIENT_SECRET"); err != nil {
		return nil, err
	}
	a.markdownSupport = pyrepr.Truthy(getOr(extra, "markdown_support", true))
	a.DMPolicy = pystr.Lower(pystr.Strip(pyrepr.Str(getOr(extra, "dm_policy", "pairing"))))
	a.GroupPolicy = pystr.Lower(pystr.Strip(pyrepr.Str(getOr(extra, "group_policy", "pairing"))))
	for _, entry := range CoerceList(pyrepr.Or(extra["allow_from"], extra["allowFrom"])) {
		a.AllowFrom[entry] = true
	}
	for _, entry := range CoerceList(pyrepr.Or(extra["group_allow_from"], extra["groupAllowFrom"])) {
		a.GroupAllowFrom[entry] = true
	}
	a.OwnAccessPolicyMixin.EntryMatches = func(_ context.Context, entries map[string]bool, target string) (bool, error) {
		return entryMatches(entries, target), nil
	}
	a.interactionCallback = a.defaultInteractionDispatch
	return a, nil
}

// Case insensitive allowlist match where a star entry matches anyone
func entryMatches(entries map[string]bool, target string) bool {
	normalizedTarget := pystr.Lower(pystr.Strip(target))
	for entry, present := range entries {
		if n := pystr.Lower(pystr.Strip(entry)); present && (n == "*" || n == normalizedTarget) {
			return true
		}
	}
	return false
}

// The adapter gates DM and group access itself
func (a *QQAdapter) EnforcesOwnAccessPolicy() bool {
	return a.OwnAccessPolicyMixin.EnforcesOwnAccessPolicy()
}

// Log prefix carrying the app id so several instances can be told apart
func (a *QQAdapter) logTag() string {
	if a.appID != "" {
		return "QQBot:" + a.appID
	}
	return "QQBot"
}

// Fails every pending response with a RuntimeError carrying the reason
func (a *QQAdapter) failPending(reason string) {
	a.mu.Lock()
	pending := a.pendingResponses
	a.pendingResponses = map[string]chan error{}
	a.mu.Unlock()
	for _, future := range pending {
		select {
		case future <- runtimeError(reason):
		default:
		}
	}
}

// Publishes the transport as down without stopping the reconnect loop, a fatal error left standing
func (a *QQAdapter) markTransportDisconnected(ctx context.Context) {
	if a.HasFatalError() {
		return
	}
	a.WriteRuntimeStatusSafe(ctx, "disconnected", map[string]any{"platform_state": "disconnected", "error_code": nil, "error_message": nil})
}

// Whether the adapter lifecycle is running, whatever the transport state
func (a *QQAdapter) running() bool { return a.BasePlatformAdapter.IsConnected() }

// Live websocket, nil while none is open
func (a *QQAdapter) socket() *pyaiohttpws.Conn {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.ws
}

// Whether the websocket transport is usable right now
func (a *QQAdapter) IsConnected() bool {
	ws := a.socket()
	return a.running() && ws != nil && !ws.Closed()
}

// Display name of the platform
func (a *QQAdapter) Name() string { return "QQBot" }

// Authenticates, resolves the gateway URL and opens the websocket under the app id lock
func (a *QQAdapter) Connect(ctx context.Context, isReconnect bool) (bool, error) {
	if a.appID == "" || a.clientSecret == "" {
		message := "QQ startup failed: QQ_APP_ID and QQ_CLIENT_SECRET are required"
		a.SetFatalError(ctx, "qq_missing_credentials", message, true)
		slog.Warn(fmt.Sprintf("[%s] %s", a.logTag(), message))
		return false, nil
	}
	acquired, err := a.AcquirePlatformLock(ctx, "qqbot-appid", a.appID, "QQBot app ID")
	if err != nil || !acquired {
		return false, err
	}
	client := gateway.PlatformHTTPClient(ctx, gateway.PlatformHTTPClientOptions{})
	a.mu.Lock()
	a.httpClient = client
	a.mu.Unlock()
	if err := a.openGatewayWS(ctx, true); err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		message := "QQ startup failed: " + errText(err)
		a.SetFatalError(ctx, "qq_connect_error", message, true)
		slog.Error(fmt.Sprintf("[%s] %s", a.logTag(), message), "error", err)
		a.cleanup(ctx)
		a.ReleasePlatformLock()
		return false, nil
	}
	a.MarkConnected(ctx, "")
	listen := gateway.SpawnAdapterTask(ctx, a.listenLoop)
	heartbeat := gateway.SpawnAdapterTask(ctx, a.heartbeatLoop)
	a.mu.Lock()
	a.listenTask, a.heartbeatTask = listen, heartbeat
	a.mu.Unlock()
	slog.Info(fmt.Sprintf("[%s] Connected", a.logTag()))
	a.WirePluginHandlers(ctx, nil)
	return true, nil
}

// Stops the loops, closes the transport and releases the app id lock
func (a *QQAdapter) Disconnect(ctx context.Context) error {
	a.SetRunning(false)
	a.MarkDisconnected(ctx)
	a.mu.Lock()
	listen, heartbeat := a.listenTask, a.heartbeatTask
	a.listenTask, a.heartbeatTask = nil, nil
	a.mu.Unlock()
	gateway.CancelTask(ctx, listen)
	gateway.CancelTask(ctx, heartbeat)
	a.cleanup(ctx)
	a.ReleasePlatformLock()
	slog.Info(fmt.Sprintf("[%s] Disconnected", a.logTag()))
	return nil
}

// Closes the websocket and forgets it, the HTTP client staying alive
func (a *QQAdapter) closeWS(ctx context.Context) {
	a.mu.Lock()
	ws := a.ws
	a.ws = nil
	a.mu.Unlock()
	if ws != nil && !ws.Closed() {
		ws.Close(ctx)
	}
}

// Closes the websocket and the HTTP client and fails the pending responses
func (a *QQAdapter) cleanup(ctx context.Context) {
	a.closeWS(ctx)
	a.mu.Lock()
	client := a.httpClient
	a.httpClient = nil
	a.mu.Unlock()
	if client != nil {
		client.CloseIdleConnections()
	}
	a.failPending("Disconnected")
}

// One JSON or raw request through a client with the httpx timeout, failures carrying the httpx error class
func httpRequest(ctx context.Context, client *http.Client, method, rawURL string, headers map[string]string, body []byte, timeout float64) (*pyhttpx.Response, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, reader)
	if err != nil {
		return nil, err
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	resp, err := pyhttpx.Do(client, req, time.Duration(timeout*float64(time.Second)))
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		var te *pyhttpx.TimeoutError
		if errors.As(err, &te) {
			return nil, &pyErr{"ReadTimeout", ""}
		}
		return nil, &pyErr{"ConnectError", pyhttpx.ErrorText(err)}
	}
	return resp, nil
}

// Compact JSON body the way httpx encodes a json argument
func jsonBody(payload any) ([]byte, error) {
	text, err := pyjson.Dumps(payload, pyjson.EnsureASCII(false), pyjson.Separators(",", ":"), pyjson.AllowNaN(false))
	return []byte(text), err
}

// HTTPStatusError of httpx raise_for_status for a response outside the success range
func raiseForStatus(resp *pyhttpx.Response, rawURL string) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	return &pyErr{"HTTPStatusError", resp.StatusErrorText(rawURL)}
}

// HTTP client in use, a RuntimeError while disconnected
func (a *QQAdapter) requireHTTPClient() (*http.Client, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.httpClient == nil {
		return nil, runtimeError("HTTP client not initialized — not connected?")
	}
	return a.httpClient, nil
}

// JSON object of a request that must succeed, any failure wrapped as a RuntimeError naming what was fetched
func (a *QQAdapter) fetchJSON(ctx context.Context, what, method, rawURL string, headers map[string]string, payload any) (map[string]any, error) {
	data, err := func() (map[string]any, error) {
		a.mu.Lock()
		client := a.httpClient
		a.mu.Unlock()
		if client == nil {
			return nil, &pyErr{"AttributeError", "'NoneType' object has no attribute '" + pystr.Lower(method) + "'"}
		}
		var body []byte
		if payload != nil {
			var err error
			if body, err = jsonBody(payload); err != nil {
				return nil, err
			}
			if headers == nil {
				headers = map[string]string{}
			}
			headers["Content-Type"] = "application/json"
		}
		resp, err := httpRequest(ctx, client, method, rawURL, headers, body, DefaultAPITimeout)
		if err != nil {
			return nil, err
		}
		if err := raiseForStatus(resp, rawURL); err != nil {
			return nil, err
		}
		decoded, err := resp.JSON()
		if err != nil {
			return nil, err
		}
		dict, ok := asDict(decoded)
		if !ok {
			return nil, notDictErr(decoded)
		}
		return dict, nil
	}()
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, runtimeError(fmt.Sprintf("Failed to get QQ Bot %s: %s", what, errText(err)))
	}
	return data, nil
}

// Whether the cached access token has over a minute left
func (a *QQAdapter) tokenFresh() (string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.accessToken, a.accessToken != "" && wallClock() < a.tokenExpiresAt-60
}

// Valid access token, refreshed under the token lock when it is missing or about to expire
func (a *QQAdapter) ensureToken(ctx context.Context) (string, error) {
	if token, fresh := a.tokenFresh(); fresh {
		return token, nil
	}
	a.tokenLock.Lock()
	defer a.tokenLock.Unlock()
	if token, fresh := a.tokenFresh(); fresh {
		return token, nil
	}
	data, err := a.fetchJSON(ctx, "access token", http.MethodPost, TokenURL, nil, obj("appId", a.appID, "clientSecret", a.clientSecret))
	if err != nil {
		return "", err
	}
	token := data["access_token"]
	if !pyrepr.Truthy(token) {
		return "", runtimeError("QQ Bot token response missing access_token: " + pyrepr.Str(data))
	}
	expiresIn, err := pyInt64(getOr(data, "expires_in", int64(7200)))
	if err != nil {
		return "", err
	}
	a.mu.Lock()
	a.accessToken, a.tokenExpiresAt = pyrepr.Str(token), wallClock()+float64(expiresIn)
	access := a.accessToken
	a.mu.Unlock()
	slog.Info(fmt.Sprintf("[%s] Access token refreshed, expires in %ds", a.logTag(), expiresIn))
	return access, nil
}

// Websocket gateway URL of the bot
func (a *QQAdapter) getGatewayURL(ctx context.Context) (string, error) {
	token, err := a.ensureToken(ctx)
	if err != nil {
		return "", err
	}
	data, err := a.fetchJSON(ctx, "gateway URL", http.MethodGet, APIBase+GatewayURLPath,
		map[string]string{"Authorization": "QQBot " + token, "User-Agent": BuildUserAgent()}, nil)
	if err != nil {
		return "", err
	}
	url := data["url"]
	if !pyrepr.Truthy(url) {
		return "", runtimeError("QQ Bot gateway response missing url: " + pyrepr.Str(data))
	}
	return pyrepr.Str(url), nil
}

// Token, gateway URL and websocket in turn, shared by the first connect and every reconnect
func (a *QQAdapter) openGatewayWS(ctx context.Context, logURL bool) error {
	if _, err := a.ensureToken(ctx); err != nil {
		return err
	}
	gatewayURL, err := a.getGatewayURL(ctx)
	if err != nil {
		return err
	}
	if logURL {
		slog.Info(fmt.Sprintf("[%s] Gateway URL: %s", a.logTag(), gatewayURL))
	}
	return a.openWS(ctx, gatewayURL)
}

// Opens the websocket through the first proxy env var that is set
func (a *QQAdapter) openWS(ctx context.Context, gatewayURL string) error {
	a.closeWS(ctx)
	wsProxy := ""
	for _, name := range []string{"WSS_PROXY", "wss_proxy", "HTTPS_PROXY", "https_proxy", "ALL_PROXY", "all_proxy"} {
		if v := os.Getenv(name); v != "" {
			wsProxy = v
			break
		}
	}
	ws, err := pyaiohttpws.Connect(ctx, gatewayURL, pyaiohttpws.Options{TrustEnv: gateway.GatewayTrustEnv(ctx), Proxy: wsProxy,
		Header: http.Header{"User-Agent": []string{BuildUserAgent()}}, CloseTimeout: time.Duration(ConnectTimeoutSeconds * float64(time.Second))})
	if err != nil {
		return err
	}
	a.mu.Lock()
	a.ws = ws
	a.mu.Unlock()
	slog.Info(fmt.Sprintf("[%s] WebSocket connected to %s", a.logTag(), gatewayURL))
	return nil
}

// Reads events and reconnects on errors, close codes choosing token refresh, re identify, rate limit wait or stop
func (a *QQAdapter) listenLoop(ctx context.Context) error {
	backoffIdx, quickDisconnectCount := 0, 0
	connectTime := 0.0
	reconnect := func() error {
		ok, err := a.reconnect(ctx, backoffIdx)
		if err != nil {
			return err
		}
		if ok {
			backoffIdx, quickDisconnectCount = 0, 0
		} else {
			backoffIdx++
		}
		return nil
	}
	for a.running() {
		connectTime = monotonic()
		err := a.readEvents(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if err == nil {
			backoffIdx, quickDisconnectCount = 0, 0
			continue
		}
		if !a.running() {
			return nil
		}
		var closeErr *QQCloseError
		if !errors.As(err, &closeErr) {
			slog.Warn(fmt.Sprintf("[%s] WebSocket error: %s", a.logTag(), errText(err)))
			a.markTransportDisconnected(ctx)
			a.failPending("Connection interrupted")
			if backoffIdx >= MaxReconnectAttempts {
				slog.Error(fmt.Sprintf("[%s] Max reconnect attempts reached", a.logTag()))
				a.MarkDisconnected(ctx)
				return nil
			}
			if reconnect() != nil {
				return nil
			}
			continue
		}
		codeText, code := "None", 0
		if closeErr.Code != nil {
			code, codeText = *closeErr.Code, strconv.Itoa(*closeErr.Code)
		}
		slog.Warn(fmt.Sprintf("[%s] WebSocket closed: code=%s reason=%s", a.logTag(), codeText, closeErr.Reason))
		if duration := monotonic() - connectTime; duration < QuickDisconnectThreshold && connectTime > 0 {
			quickDisconnectCount++
			slog.Info(fmt.Sprintf("[%s] Quick disconnect (%.1fs), count: %d", a.logTag(), duration, quickDisconnectCount))
			if quickDisconnectCount >= MaxQuickDisconnectCount {
				slog.Error(fmt.Sprintf("[%s] Too many quick disconnects. Check: 1) AppID/Secret correct 2) Bot permissions on QQ Open Platform", a.logTag()))
				a.SetFatalError(ctx, "qq_quick_disconnect", "Too many quick disconnects — check bot permissions", true)
				return nil
			}
		} else {
			quickDisconnectCount = 0
		}
		a.markTransportDisconnected(ctx)
		a.failPending("Connection closed")
		if desc, fatal := fatalCloseCodes[code]; fatal && closeErr.Code != nil {
			slog.Error(fmt.Sprintf("[%s] Bot is %s. Check QQ Open Platform.", a.logTag(), desc))
			a.SetFatalError(ctx, "qq_"+desc, "Bot is "+desc, false)
			return nil
		}
		if closeErr.Code != nil && code == 4008 {
			slog.Info(fmt.Sprintf("[%s] Rate limited (4008), waiting %ds", a.logTag(), RateLimitDelay))
			if backoffIdx >= MaxReconnectAttempts {
				a.MarkDisconnected(ctx)
				return nil
			}
			if sleep(ctx, RateLimitDelay) != nil || reconnect() != nil {
				return nil
			}
			continue
		}
		if closeErr.Code != nil && code == 4004 {
			slog.Info(fmt.Sprintf("[%s] Invalid token (4004), will refresh and reconnect", a.logTag()))
			a.mu.Lock()
			a.accessToken, a.tokenExpiresAt = "", 0.0
			a.mu.Unlock()
		}
		if closeErr.Code != nil && sessionInvalidCloseCode(code) {
			slog.Info(fmt.Sprintf("[%s] Session error (%d), clearing session for re-identify", a.logTag(), code))
			a.mu.Lock()
			a.sessionID, a.lastSeq = nil, nil
			a.mu.Unlock()
		}
		if reconnect() != nil {
			return nil
		}
		if backoffIdx >= MaxReconnectAttempts {
			slog.Error(fmt.Sprintf("[%s] Max reconnect attempts reached (QQCloseError)", a.logTag()))
			a.MarkDisconnected(ctx)
			return nil
		}
	}
	return nil
}

// Waits the backoff delay and reopens the gateway websocket, false when the reopen fails
func (a *QQAdapter) reconnect(ctx context.Context, backoffIdx int) (bool, error) {
	delay := ReconnectBackoff[min(backoffIdx, len(ReconnectBackoff)-1)]
	slog.Info(fmt.Sprintf("[%s] Reconnecting in %ds (attempt %d)...", a.logTag(), delay, backoffIdx+1))
	if err := sleep(ctx, float64(delay)); err != nil {
		return false, err
	}
	a.mu.Lock()
	a.heartbeatInterval = defaultHeartbeatSeconds
	a.mu.Unlock()
	if err := a.openGatewayWS(ctx, false); err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		slog.Warn(fmt.Sprintf("[%s] Reconnect failed: %s", a.logTag(), errText(err)))
		return false, nil
	}
	a.MarkConnected(ctx, "")
	slog.Info(fmt.Sprintf("[%s] Reconnected", a.logTag()))
	return true, nil
}

// Dispatches text frames until the websocket closes, a close frame raising its code and reason
func (a *QQAdapter) readEvents(ctx context.Context) error {
	ws := a.socket()
	if ws == nil {
		return runtimeError("WebSocket not connected")
	}
	if ws.Closed() {
		return runtimeError("WebSocket closed")
	}
	for a.running() {
		ws := a.socket()
		if ws == nil || ws.Closed() {
			break
		}
		msg, err := ws.Receive(ctx)
		if err != nil {
			return err
		}
		switch msg.Type {
		case pyaiohttpws.Text:
			if payload := parseJSON(msg.Data); len(payload) > 0 {
				if err := a.dispatchPayload(ctx, payload); err != nil {
					return err
				}
			}
		case pyaiohttpws.Close:
			return newQQCloseError(msg.Data, msg.Extra)
		case pyaiohttpws.Closed, pyaiohttpws.Error:
			return runtimeError("WebSocket closed")
		}
	}
	return nil
}

// Sends one JSON frame over the websocket with the default json.dumps separators
func sendJSON(ws *pyaiohttpws.Conn, payload any) error {
	text, err := pyjson.Dumps(payload)
	if err != nil {
		return err
	}
	return ws.SendStr(text)
}

// Sends op 1 heartbeats with the latest sequence at the interval the Hello set
func (a *QQAdapter) heartbeatLoop(ctx context.Context) error {
	for a.running() {
		a.mu.Lock()
		interval := a.heartbeatInterval
		a.mu.Unlock()
		if sleep(ctx, interval) != nil {
			return nil
		}
		ws := a.socket()
		if ws == nil || ws.Closed() {
			continue
		}
		a.mu.Lock()
		var seq any
		if a.lastSeq != nil {
			seq = *a.lastSeq
		}
		a.mu.Unlock()
		if err := sendJSON(ws, obj("op", int64(1), "d", seq)); err != nil {
			slog.Debug(fmt.Sprintf("[%s] Heartbeat failed: %s", a.logTag(), errText(err)))
		}
	}
	return nil
}

// Sends an Identify or Resume payload, false only when the send itself failed
func (a *QQAdapter) sendWSAuth(name string, payload *pyjson.Object, sentMsg string) bool {
	ws := a.socket()
	if ws == nil || ws.Closed() {
		slog.Warn(fmt.Sprintf("[%s] Cannot send %s: WebSocket not connected", a.logTag(), name))
		return true
	}
	if err := sendJSON(ws, payload); err != nil {
		slog.Error(fmt.Sprintf("[%s] Failed to send %s: %s", a.logTag(), name, errText(err)))
		return false
	}
	slog.Info(fmt.Sprintf("[%s] %s", a.logTag(), sentMsg))
	return true
}

// Sends op 2 Identify with the group, guild, direct message and interaction intents
func (a *QQAdapter) sendIdentify(ctx context.Context) error {
	token, err := a.ensureToken(ctx)
	if err != nil {
		return err
	}
	a.sendWSAuth("Identify", obj("op", int64(2), "d", obj("token", "QQBot "+token,
		"intents", int64((1<<25)|(1<<30)|(1<<12)|(1<<26)), "shard", []any{int64(0), int64(1)},
		"properties", obj("$os", "macOS", "$browser", "icebath-agent", "$device", "icebath-agent"))), "Identify sent")
	return nil
}

// Sends op 6 Resume, a failed send clearing the session so the next Hello identifies
func (a *QQAdapter) sendResume(ctx context.Context) error {
	token, err := a.ensureToken(ctx)
	if err != nil {
		return err
	}
	a.mu.Lock()
	sessionID := a.sessionID
	var seq any
	if a.lastSeq != nil {
		seq = *a.lastSeq
	}
	a.mu.Unlock()
	if !a.sendWSAuth("Resume", obj("op", int64(6), "d", obj("token", "QQBot "+token, "session_id", sessionID, "seq", seq)),
		fmt.Sprintf("Resume sent (session_id=%s, seq=%s)", pyrepr.Str(sessionID), pyrepr.Str(seq))) {
		a.mu.Lock()
		a.sessionID, a.lastSeq = nil, nil
		a.mu.Unlock()
	}
	return nil
}

// Closes the websocket in the background so the read loop fails and the listener reconnects
func (a *QQAdapter) closeWSSoon(ctx context.Context) {
	if ws := a.socket(); ws != nil && !ws.Closed() {
		gateway.SpawnAdapterTask(ctx, func(tctx context.Context) error {
			ws.Close(tctx)
			return nil
		})
	}
}

// Whether a decoded value equals an int the way Python compares numbers
func equalsInt(v any, n int64) bool { return v != nil && pyjson.Equal(v, n) }

// Routes one websocket payload by its opcode, spawning the async handlers
func (a *QQAdapter) dispatchPayload(ctx context.Context, payload map[string]any) error {
	op, t, s, d := payload["op"], payload["t"], payload["s"], payload["d"]
	if seq, ok := s.(int64); ok {
		a.mu.Lock()
		if a.lastSeq == nil || seq > *a.lastSeq {
			a.lastSeq = &seq
		}
		a.mu.Unlock()
	}
	spawn := func(fn func(tctx context.Context) error) { gateway.SpawnAdapterTask(ctx, fn) }
	switch {
	case equalsInt(op, 10):
		var intervalRaw any = int64(30000)
		if dict, ok := asDict(d); ok {
			intervalRaw = getOr(dict, "heartbeat_interval", int64(30000))
		}
		intervalMS, err := pyjsonFloat(intervalRaw)
		if err != nil {
			return err
		}
		a.mu.Lock()
		a.heartbeatInterval = intervalMS / 1000.0 * 0.8
		interval := a.heartbeatInterval
		resume := pyrepr.Truthy(a.sessionID) && a.lastSeq != nil
		a.mu.Unlock()
		slog.Debug(fmt.Sprintf("[%s] Hello received, heartbeat_interval=%dms (sending every %.1fs)", a.logTag(), int64(intervalMS), interval))
		if resume {
			spawn(a.sendResume)
		} else {
			spawn(a.sendIdentify)
		}
	case equalsInt(op, 0) && pyrepr.Truthy(t):
		event := pyrepr.Str(t)
		switch {
		case event == "READY":
			if dict, ok := asDict(d); ok {
				a.mu.Lock()
				a.sessionID = dict["session_id"]
				a.mu.Unlock()
				slog.Info(fmt.Sprintf("[%s] Ready, session_id=%s", a.logTag(), pyrepr.Str(dict["session_id"])))
			}
		case event == "RESUMED":
			slog.Info(fmt.Sprintf("[%s] Session resumed", a.logTag()))
		case inboundHandlers[event] != nil:
			spawn(func(tctx context.Context) error { return a.onMessage(tctx, event, d) })
		case event == "INTERACTION_CREATE":
			spawn(func(tctx context.Context) error {
				a.onInteraction(tctx, d)
				return nil
			})
		default:
			slog.Debug(fmt.Sprintf("[%s] Unhandled dispatch: %s", a.logTag(), event))
		}
	case equalsInt(op, 11):
	case equalsInt(op, 7):
		slog.Info(fmt.Sprintf("[%s] Server requested reconnect (op 7)", a.logTag()))
		a.closeWSSoon(ctx)
	case equalsInt(op, 9):
		if d != nil && pyrepr.Truthy(d) {
			slog.Info(fmt.Sprintf("[%s] Invalid session (op 9, resumable)", a.logTag()))
		} else {
			slog.Info(fmt.Sprintf("[%s] Invalid session (op 9, not resumable), clearing session", a.logTag()))
			a.mu.Lock()
			a.sessionID, a.lastSeq = nil, nil
			a.mu.Unlock()
		}
		a.closeWSSoon(ctx)
	default:
		slog.Debug(fmt.Sprintf("[%s] Unknown op: %s", a.logTag(), pyrepr.Str(op)))
	}
	return nil
}

// Float of a decoded JSON number, the TypeError dividing any other value raises
func pyjsonFloat(v any) (float64, error) {
	switch n := v.(type) {
	case float64:
		return n, nil
	case int64:
		return float64(n), nil
	case bool:
		if n {
			return 1, nil
		}
		return 0, nil
	}
	return 0, &pyErr{"TypeError", fmt.Sprintf("unsupported operand type(s) for /: '%s' and 'float'", pyrepr.TypeName(v))}
}

// Decoded object of a text frame, nil for a frame that does not parse or is not an object
func parseJSON(raw any) map[string]any {
	text, _ := raw.(string)
	payload, err := pyjson.Loads(text)
	if err != nil {
		slog.Warn(fmt.Sprintf("[QQBot] Failed to parse JSON: %s", pyrepr.Repr(raw)))
		return nil
	}
	dict, _ := asDict(payload)
	return dict
}

// Message sequence number in the range the API accepts, from the clock and a random value
func nextMsgSeq() int64 {
	timePart := int64(wallClock()) % 100000000
	var raw [2]byte
	_, _ = rand.Read(raw[:])
	return (timePart ^ int64(raw[0])<<8 ^ int64(raw[1])) % 65536
}
