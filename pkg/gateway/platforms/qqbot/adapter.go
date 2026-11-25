package qqbot

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/settings"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	httputil "github.com/nickheyer/icebath/pkg/utils/http"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	websocketutil "github.com/nickheyer/icebath/pkg/utils/websocket"
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

func runtimeError(msg string) error { return &RuntimeError{Msg: msg} }

// Websocket close carrying the close code and reason for the reconnect loop
type QQCloseError struct {
	Code   *int
	Reason string
}

// Close error of a received close frame, a zero code read as none
func newQQCloseError(code int, reason string) *QQCloseError {
	e := &QQCloseError{Reason: reason}
	if code != 0 {
		e.Code = &code
	}
	return e
}

func (e *QQCloseError) Error() string {
	code := "none"
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
	ws                  *websocketutil.Conn
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

// Text of a value with surrounding white space removed, empty for nil, false, zero and empty values
func textOf(v any) string {
	if !anyutil.IsTrue(v) {
		return ""
	}
	return strings.TrimSpace(anyutil.String(v))
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
		if v := extra[key]; anyutil.IsTrue(v) {
			return strings.TrimSpace(anyutil.String(v)), nil
		}
		secret, err := gateway.GetScopedSecret(ctx, env, "", false)
		return strings.TrimSpace(secret), err
	}
	if a.appID, err = credential("app_id", "QQ_APP_ID"); err != nil {
		return nil, err
	}
	if a.clientSecret, err = credential("client_secret", "QQ_CLIENT_SECRET"); err != nil {
		return nil, err
	}
	sctx := gateway.SettingsContext(ctx)
	a.markdownSupport = settings.QqbotMarkdownSupport.Get(sctx)
	a.DMPolicy = strings.ToLower(strings.TrimSpace(settings.QqbotDmPolicy.Get(sctx)))
	a.GroupPolicy = strings.ToLower(strings.TrimSpace(settings.QqbotGroupPolicy.Get(sctx)))
	for _, entry := range gateway.SettingList(ctx, settings.QqbotAllowFrom) {
		a.AllowFrom[entry] = true
	}
	for _, entry := range gateway.SettingList(ctx, settings.QqbotGroupAllowFrom) {
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
	normalizedTarget := strings.ToLower(strings.TrimSpace(target))
	for entry, present := range entries {
		if n := strings.ToLower(strings.TrimSpace(entry)); present && (n == "*" || n == normalizedTarget) {
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
func (a *QQAdapter) socket() *websocketutil.Conn {
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
		pkgLog().Warn(fmt.Sprintf("[%s] %s", a.logTag(), message))
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
		shown := describeConnectProxyError(err, wsProxy())
		message := "QQ startup failed: " + cmp.Or(shown, err.Error())
		a.SetFatalError(ctx, "qq_connect_error", message, true)
		if shown != "" {
			pkgLog().Error(fmt.Sprintf("[%s] %s", a.logTag(), message))
		} else {
			pkgLog().Error(fmt.Sprintf("[%s] %s", a.logTag(), message), "error", err)
		}
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
	pkgLog().Info(fmt.Sprintf("[%s] Connected", a.logTag()))
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
	pkgLog().Info(fmt.Sprintf("[%s] Disconnected", a.logTag()))
	return nil
}

// Closes the websocket and forgets it, the HTTP client staying alive
func (a *QQAdapter) closeWS(ctx context.Context) {
	a.mu.Lock()
	ws := a.ws
	a.ws = nil
	a.mu.Unlock()
	if ws != nil && !ws.Closed() {
		ws.CloseWith(ctx, websocketutil.CloseNormal, "")
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

// One JSON or raw request through a client with every phase held to the timeout, a failure as the *url.Error of the exchange
func httpRequest(ctx context.Context, client *http.Client, method, rawURL string, headers map[string]string, body []byte, timeout float64) (*httputil.Response, error) {
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
	resp, err := httputil.Do(client, req, httputil.EachPhase(time.Duration(timeout*float64(time.Second))))
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}
	return resp, nil
}

// Compact UTF-8 JSON body of a payload
func jsonBody(payload any) ([]byte, error) {
	return jsonutil.Append(nil, payload)
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
			return nil, errors.New("HTTP client not initialized")
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
		if err := resp.CheckStatus(); err != nil {
			return nil, err
		}
		decoded, err := resp.JSON()
		if err != nil {
			return nil, err
		}
		dict, ok := jsonutil.AsMap(decoded)
		if !ok {
			return nil, notDictErr(decoded)
		}
		return dict, nil
	}()
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, runtimeError(fmt.Sprintf("Failed to get QQ Bot %s: %v", what, err))
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
	if !anyutil.IsTrue(token) {
		return "", runtimeError("QQ Bot token response missing access_token: " + anyutil.String(data))
	}
	expiresIn, err := anyutil.ToInt(getOr(data, "expires_in", int64(7200)))
	if err != nil {
		return "", err
	}
	a.mu.Lock()
	a.accessToken, a.tokenExpiresAt = anyutil.String(token), wallClock()+float64(expiresIn)
	access := a.accessToken
	a.mu.Unlock()
	pkgLog().Info(fmt.Sprintf("[%s] Access token refreshed, expires in %ds", a.logTag(), expiresIn))
	return access, nil
}

// Websocket gateway URL of the bot
func (a *QQAdapter) getGatewayURL(ctx context.Context) (string, error) {
	token, err := a.ensureToken(ctx)
	if err != nil {
		return "", err
	}
	data, err := a.fetchJSON(ctx, "gateway URL", http.MethodGet, apiBase(ctx)+GatewayURLPath,
		map[string]string{"Authorization": "QQBot " + token, "User-Agent": BuildUserAgent()}, nil)
	if err != nil {
		return "", err
	}
	gatewayURL := data["url"]
	if !anyutil.IsTrue(gatewayURL) {
		return "", runtimeError("QQ Bot gateway response missing url: " + anyutil.String(data))
	}
	return anyutil.String(gatewayURL), nil
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
		pkgLog().Info(fmt.Sprintf("[%s] Gateway URL: %s", a.logTag(), gatewayURL))
	}
	return a.openWS(ctx, gatewayURL)
}

// Proxy the websocket dials, the first proxy env var that is set
func wsProxy() string {
	for _, name := range []string{"WSS_PROXY", "wss_proxy", "HTTPS_PROXY", "https_proxy", "ALL_PROXY", "all_proxy"} {
		if v := os.Getenv(name); v != "" {
			return v
		}
	}
	return ""
}

// Opens the websocket through the first proxy env var that is set, else through the system proxy settings when the gateway trusts the environment
func (a *QQAdapter) openWS(ctx context.Context, gatewayURL string) error {
	a.closeWS(ctx)
	opts := websocketutil.Options{PingInterval: -1, MaxMessageSize: -1, Header: http.Header{"User-Agent": []string{BuildUserAgent()}},
		CloseTimeout: time.Duration(ConnectTimeoutSeconds * float64(time.Second))}
	if gateway.GatewayTrustEnv(ctx) {
		opts.Proxy = httputil.Proxy
	}
	if raw := wsProxy(); raw != "" {
		proxyURL, err := url.Parse(raw)
		if err != nil {
			return err
		}
		opts.Proxy = http.ProxyURL(proxyURL)
	}
	ws, err := websocketutil.Connect(ctx, gatewayURL, opts)
	if err != nil {
		return err
	}
	a.mu.Lock()
	a.ws = ws
	a.mu.Unlock()
	pkgLog().Info(fmt.Sprintf("[%s] WebSocket connected to %s", a.logTag(), gatewayURL))
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
			pkgLog().Warn(fmt.Sprintf("[%s] WebSocket error: %v", a.logTag(), err))
			a.markTransportDisconnected(ctx)
			a.failPending("Connection interrupted")
			if backoffIdx >= MaxReconnectAttempts {
				pkgLog().Error(fmt.Sprintf("[%s] Max reconnect attempts reached", a.logTag()))
				a.MarkDisconnected(ctx)
				return nil
			}
			if reconnect() != nil {
				return nil
			}
			continue
		}
		codeText, code := "none", 0
		if closeErr.Code != nil {
			code, codeText = *closeErr.Code, strconv.Itoa(*closeErr.Code)
		}
		pkgLog().Warn(fmt.Sprintf("[%s] WebSocket closed: code=%s reason=%s", a.logTag(), codeText, closeErr.Reason))
		if duration := monotonic() - connectTime; duration < QuickDisconnectThreshold && connectTime > 0 {
			quickDisconnectCount++
			pkgLog().Info(fmt.Sprintf("[%s] Quick disconnect (%.1fs), count: %d", a.logTag(), duration, quickDisconnectCount))
			if quickDisconnectCount >= MaxQuickDisconnectCount {
				pkgLog().Error(fmt.Sprintf("[%s] Too many quick disconnects. Check: 1) AppID/Secret correct 2) Bot permissions on QQ Open Platform", a.logTag()))
				a.SetFatalError(ctx, "qq_quick_disconnect", "Too many quick disconnects — check bot permissions", true)
				return nil
			}
		} else {
			quickDisconnectCount = 0
		}
		a.markTransportDisconnected(ctx)
		a.failPending("Connection closed")
		if desc, fatal := fatalCloseCodes[code]; fatal && closeErr.Code != nil {
			pkgLog().Error(fmt.Sprintf("[%s] Bot is %s. Check QQ Open Platform.", a.logTag(), desc))
			a.SetFatalError(ctx, "qq_"+desc, "Bot is "+desc, false)
			return nil
		}
		if closeErr.Code != nil && code == 4008 {
			pkgLog().Info(fmt.Sprintf("[%s] Rate limited (4008), waiting %ds", a.logTag(), RateLimitDelay))
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
			pkgLog().Info(fmt.Sprintf("[%s] Invalid token (4004), will refresh and reconnect", a.logTag()))
			a.mu.Lock()
			a.accessToken, a.tokenExpiresAt = "", 0.0
			a.mu.Unlock()
		}
		if closeErr.Code != nil && sessionInvalidCloseCode(code) {
			pkgLog().Info(fmt.Sprintf("[%s] Session error (%d), clearing session for re-identify", a.logTag(), code))
			a.mu.Lock()
			a.sessionID, a.lastSeq = nil, nil
			a.mu.Unlock()
		}
		if reconnect() != nil {
			return nil
		}
		if backoffIdx >= MaxReconnectAttempts {
			pkgLog().Error(fmt.Sprintf("[%s] Max reconnect attempts reached (QQCloseError)", a.logTag()))
			a.MarkDisconnected(ctx)
			return nil
		}
	}
	return nil
}

// Waits the backoff delay and reopens the gateway websocket, false when the reopen fails
func (a *QQAdapter) reconnect(ctx context.Context, backoffIdx int) (bool, error) {
	delay := ReconnectBackoff[min(backoffIdx, len(ReconnectBackoff)-1)]
	pkgLog().Info(fmt.Sprintf("[%s] Reconnecting in %ds (attempt %d)...", a.logTag(), delay, backoffIdx+1))
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
		pkgLog().Warn(fmt.Sprintf("[%s] Reconnect failed: %s", a.logTag(), cmp.Or(describeConnectProxyError(err, wsProxy()), err.Error())))
		return false, nil
	}
	a.MarkConnected(ctx, "")
	pkgLog().Info(fmt.Sprintf("[%s] Reconnected", a.logTag()))
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
			var closed *websocketutil.CloseError
			if !errors.As(err, &closed) {
				return err
			}
			if closed.Received() && !closed.SentFirst {
				return newQQCloseError(closed.Code, closed.Reason)
			}
			return runtimeError("WebSocket closed")
		}
		if msg.Type == websocketutil.Text {
			if payload := parseJSON(msg.Data); len(payload) > 0 {
				if err := a.dispatchPayload(ctx, payload); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// Sends one payload over the websocket as a compact JSON text frame
func sendJSON(ws *websocketutil.Conn, payload any) error {
	text, err := jsonutil.Encode(payload)
	if err != nil {
		return err
	}
	return ws.SendText(text)
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
			pkgLog().Debug(fmt.Sprintf("[%s] Heartbeat failed: %v", a.logTag(), err))
		}
	}
	return nil
}

// Sends an Identify or Resume payload, false only when the send itself failed
func (a *QQAdapter) sendWSAuth(name string, payload *jsonutil.Object, sentMsg string) bool {
	ws := a.socket()
	if ws == nil || ws.Closed() {
		pkgLog().Warn(fmt.Sprintf("[%s] Cannot send %s: WebSocket not connected", a.logTag(), name))
		return true
	}
	if err := sendJSON(ws, payload); err != nil {
		pkgLog().Error(fmt.Sprintf("[%s] Failed to send %s: %v", a.logTag(), name, err))
		return false
	}
	pkgLog().Info(fmt.Sprintf("[%s] %s", a.logTag(), sentMsg))
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
	seqText := "null"
	if a.lastSeq != nil {
		seq = *a.lastSeq
		seqText = strconv.FormatInt(*a.lastSeq, 10)
	}
	a.mu.Unlock()
	if !a.sendWSAuth("Resume", obj("op", int64(6), "d", obj("token", "QQBot "+token, "session_id", sessionID, "seq", seq)),
		fmt.Sprintf("Resume sent (session_id=%s, seq=%s)", anyutil.String(sessionID), seqText)) {
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
			ws.CloseWith(tctx, websocketutil.CloseNormal, "")
			return nil
		})
	}
}

// Whether a decoded value is a number equal to n, integer and float forms alike
func equalsInt(v any, n int64) bool { return v != nil && anyutil.Equal(v, n) }

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
		if dict, ok := jsonutil.AsMap(d); ok {
			intervalRaw = getOr(dict, "heartbeat_interval", int64(30000))
		}
		intervalMS, err := jsonFloat(intervalRaw)
		if err != nil {
			return err
		}
		a.mu.Lock()
		a.heartbeatInterval = intervalMS / 1000.0 * 0.8
		interval := a.heartbeatInterval
		resume := anyutil.IsTrue(a.sessionID) && a.lastSeq != nil
		a.mu.Unlock()
		pkgLog().Debug(fmt.Sprintf("[%s] Hello received, heartbeat_interval=%dms (sending every %.1fs)", a.logTag(), int64(intervalMS), interval))
		if resume {
			spawn(a.sendResume)
		} else {
			spawn(a.sendIdentify)
		}
	case equalsInt(op, 0) && anyutil.IsTrue(t):
		event := anyutil.String(t)
		switch {
		case event == "READY":
			if dict, ok := jsonutil.AsMap(d); ok {
				a.mu.Lock()
				a.sessionID = dict["session_id"]
				a.mu.Unlock()
				pkgLog().Info(fmt.Sprintf("[%s] Ready, session_id=%s", a.logTag(), anyutil.String(dict["session_id"])))
			}
		case event == "RESUMED":
			pkgLog().Info(fmt.Sprintf("[%s] Session resumed", a.logTag()))
		case inboundHandlers[event] != nil:
			spawn(func(tctx context.Context) error { return a.onMessage(tctx, event, d) })
		case event == "INTERACTION_CREATE":
			spawn(func(tctx context.Context) error {
				a.onInteraction(tctx, d)
				return nil
			})
		default:
			pkgLog().Debug(fmt.Sprintf("[%s] Unhandled dispatch: %s", a.logTag(), event))
		}
	case equalsInt(op, 11):
	case equalsInt(op, 7):
		pkgLog().Info(fmt.Sprintf("[%s] Server requested reconnect (op 7)", a.logTag()))
		a.closeWSSoon(ctx)
	case equalsInt(op, 9):
		if d != nil && anyutil.IsTrue(d) {
			pkgLog().Info(fmt.Sprintf("[%s] Invalid session (op 9, resumable)", a.logTag()))
		} else {
			pkgLog().Info(fmt.Sprintf("[%s] Invalid session (op 9, not resumable), clearing session", a.logTag()))
			a.mu.Lock()
			a.sessionID, a.lastSeq = nil, nil
			a.mu.Unlock()
		}
		a.closeWSSoon(ctx)
	default:
		pkgLog().Debug(fmt.Sprintf("[%s] Unknown op: %s", a.logTag(), anyutil.String(op)))
	}
	return nil
}

// Float of a decoded JSON number or boolean, an error for any other value
func jsonFloat(v any) (float64, error) {
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
	return 0, errors.New("expected a number, got " + anyutil.Kind(v))
}

// Decoded object of a text frame, nil for a frame that does not parse or is not an object
func parseJSON(raw []byte) map[string]any {
	payload, err := jsonutil.Decode(raw)
	if err != nil {
		pkgLog().Warn(fmt.Sprintf("[QQBot] Failed to parse JSON: %s", strconv.Quote(string(raw))))
		return nil
	}
	dict, _ := jsonutil.AsMap(payload)
	return dict
}

// Message sequence number in the range the API accepts, from the clock and a random value
func nextMsgSeq() int64 {
	timePart := int64(wallClock()) % 100000000
	var raw [2]byte
	_, _ = rand.Read(raw[:])
	return (timePart ^ int64(raw[0])<<8 ^ int64(raw[1])) % 65536
}
