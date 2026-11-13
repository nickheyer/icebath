package callback

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/gateway"
)

const (
	defaultPort        = 8645
	defaultPath        = "/wecom/callback"
	maxBodyBytes       = 65_536
	accessTokenTTL     = 7200
	dedupTTLSeconds    = 300
	maxMessageBytes    = 2048
	sendTimeout        = 20 * time.Second
	tokenRefreshMargin = 60 * time.Second
	sendURL            = "https://qyapi.weixin.qq.com/cgi-bin/message/send?access_token="
	tokenURL           = "https://qyapi.weixin.qq.com/cgi-bin/gettoken"
)

// Error codes of a rejected or expired access token
var tokenRejectedCodes = map[int64]bool{40001: true, 42001: true}

// One self-built app the callback endpoint serves
type app struct {
	name, corpID, corpSecret, agentID, token, encodingAESKey string
}

// Cached access token of one app
type accessToken struct {
	value     string
	expiresAt time.Time
}

// Adapter of WeCom self-built apps, decrypting POSTed XML callbacks and replying through message/send
type Adapter struct {
	*gateway.BasePlatformAdapter

	host *string
	port int
	path string
	apps []app

	mu       sync.Mutex
	runner   *gateway.WebAppRunner
	client   *http.Client
	dedup    *gateway.MessageDeduplicator
	userApps map[string]string
	tokens   map[string]accessToken
}

// Class attributes of the callback adapter
func adapterTraits() gateway.AdapterTraits {
	t := gateway.DefaultAdapterTraits()
	t.ServesProfilePrefix = true
	t.MaxMessageLength = maxMessageBytes
	t.SplitsLongMessages = true
	return t
}

// Text of a config or JSON scalar, empty for null
func textOf(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case json.Number:
		return x.String()
	case bool:
		return strconv.FormatBool(x)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	}
	return fmt.Sprint(v)
}

// Whether a config value is set to something other than its zero
func isSet(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case string:
		return x != ""
	case bool:
		return x
	case int:
		return x != 0
	case int64:
		return x != 0
	case float64:
		return x != 0
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	return true
}

// Numeric errcode of a WeCom API answer and whether it is present
func errcodeOf(m map[string]any) (int64, bool) {
	switch v := m["errcode"].(type) {
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return n, true
		}
		if f, err := v.Float64(); err == nil {
			return int64(f), true
		}
	case float64:
		return int64(v), true
	}
	return 0, false
}

// Session key scoping a user to the corp of the app it wrote to
func userAppKey(corpID, userID string) string {
	if corpID != "" {
		return corpID + ":" + userID
	}
	return userID
}

// Apps of the apps list, or the single app of the top level corp keys
func normalizeApps(extra map[string]any) []app {
	if list, ok := extra["apps"].([]any); ok && len(list) > 0 {
		var out []app
		for _, item := range list {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			a := app{name: textOf(m["name"]), corpID: textOf(m["corp_id"]), corpSecret: textOf(m["corp_secret"]),
				agentID: textOf(m["agent_id"]), token: textOf(m["token"]), encodingAESKey: textOf(m["encoding_aes_key"])}
			if a.name == "" {
				a.name = a.corpID + ":" + a.agentID
			}
			out = append(out, a)
		}
		return out
	}
	if !isSet(extra["corp_id"]) {
		return nil
	}
	name := textOf(extra["name"])
	if name == "" {
		name = "default"
	}
	return []app{{name: name, corpID: textOf(extra["corp_id"]), corpSecret: textOf(extra["corp_secret"]),
		agentID: textOf(extra["agent_id"]), token: textOf(extra["token"]), encodingAESKey: textOf(extra["encoding_aes_key"])}}
}

// Adapter configured from extra, a malformed port refused
func NewAdapter(ctx context.Context, config *gateway.PlatformConfig) (*Adapter, error) {
	if config == nil {
		config = gateway.NewPlatformConfig()
	}
	a := &Adapter{dedup: gateway.NewMessageDeduplicator(gateway.DefaultDedupMaxSize, dedupTTLSeconds),
		userApps: map[string]string{}, tokens: map[string]accessToken{}}
	a.BasePlatformAdapter = gateway.NewBasePlatformAdapter(config, gateway.PlatformWeComCallback, a)
	a.Traits = adapterTraits()
	extra := config.Extra
	if extra == nil {
		extra = map[string]any{}
	}
	if isSet(extra["host"]) {
		host := textOf(extra["host"])
		a.host = &host
	}
	a.port = defaultPort
	if isSet(extra["port"]) {
		port, err := strconv.Atoi(strings.TrimSpace(textOf(extra["port"])))
		if err != nil {
			return nil, fmt.Errorf("wecom_callback port %q is not an integer", textOf(extra["port"]))
		}
		a.port = port
	}
	a.path = defaultPath
	if isSet(extra["path"]) {
		a.path = textOf(extra["path"])
	}
	a.apps = normalizeApps(extra)
	return a, nil
}

// Builds the callback adapter as a platform adapter, nil on a config error
func newPlatformAdapter(ctx context.Context, cfg *gateway.PlatformConfig) (gateway.PlatformAdapter, error) {
	a, err := NewAdapter(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return a, nil
}

// Factory the platform registry builds the callback adapter with
func Factory(ctx context.Context, cfg *gateway.PlatformConfig) (any, error) {
	return newPlatformAdapter(ctx, cfg)
}

// Starts the callback HTTP server, or publishes it on the shared listener, then primes every app token
func (a *Adapter) Connect(ctx context.Context, isReconnect bool) (bool, error) {
	if len(a.apps) == 0 {
		slog.Warn("[WecomCallback] No callback apps configured")
		return false, nil
	}
	if gateway.SharedIngressProfile(a) == "" {
		if conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(a.port)), time.Second); err == nil {
			_ = conn.Close()
			slog.Error(fmt.Sprintf("[WecomCallback] Port %d already in use", a.port))
			return false, nil
		}
	}
	web := gateway.NewWebApplication(maxBodyBytes)
	for _, route := range []struct {
		get     bool
		path    string
		handler gateway.WebHandler
	}{{true, "/health", a.handleHealth}, {true, a.path, a.handleVerify}, {false, a.path, a.handleCallback}} {
		add := web.AddPost
		if route.get {
			add = web.AddGet
		}
		if err := add(route.path, route.handler); err != nil {
			slog.Error(fmt.Sprintf("[WecomCallback] Failed to start: %v", err))
			return false, nil
		}
	}
	client := gateway.TrustEnvHTTPClient(ctx, sendTimeout)
	runner, err := gateway.BindListener(ctx, a, web, a.host, a.port, a.path, gateway.ListenOptions{})
	if err != nil {
		client.CloseIdleConnections()
		slog.Error(fmt.Sprintf("[WecomCallback] Failed to start: %v", err))
		return false, nil
	}
	a.mu.Lock()
	a.runner, a.client = runner, client
	a.mu.Unlock()
	a.MarkConnected(ctx, "")
	if runner != nil {
		host := ""
		if a.host != nil {
			host = *a.host
		}
		slog.Info(fmt.Sprintf("[WecomCallback] HTTP server listening on %s:%d%s", host, a.port, a.path))
	}
	for i := range a.apps {
		if _, err := a.refreshAccessToken(ctx, &a.apps[i]); err != nil {
			slog.Warn(fmt.Sprintf("[WecomCallback] Initial token refresh failed for app '%s': %v", a.apps[i].name, err))
		}
	}
	return true, nil
}

// Stops the HTTP server, drops the client and marks the adapter disconnected
func (a *Adapter) Disconnect(ctx context.Context) error {
	a.mu.Lock()
	runner, client := a.runner, a.client
	a.runner, a.client = nil, nil
	a.mu.Unlock()
	if client != nil {
		client.CloseIdleConnections()
	}
	var err error
	if runner != nil {
		err = runner.Cleanup(ctx)
	}
	a.MarkDisconnected(ctx)
	slog.Info("[WecomCallback] Disconnected")
	return err
}

// UTF-8 byte length, the unit message/send truncates text.content by
func utf8Len(s string) int { return len(s) }

// One text message per 2048 byte chunk through the app that last heard from the chat
func (a *Adapter) Send(ctx context.Context, chatID, content, replyTo string, metadata map[string]any) (gateway.SendResult, error) {
	target, ok := a.resolveAppForChat(chatID)
	if !ok {
		return gateway.SendResult{Success: false, Error: "No callback apps configured"}, nil
	}
	chunks := gateway.TruncateMessage(content, maxMessageBytes, utf8Len)
	result, err := gateway.SendChunks(ctx, chunks, func(ctx context.Context, chunk string) (*gateway.SendResult, error) {
		r, err := a.sendText(ctx, target, chatID, chunk)
		return &r, err
	})
	if err != nil || result == nil {
		return gateway.SendResult{}, err
	}
	return *result, nil
}

// HTTP client of the live connection, a fresh one when disconnected
func (a *Adapter) httpClient(ctx context.Context) *http.Client {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.client == nil {
		return gateway.TrustEnvHTTPClient(ctx, sendTimeout)
	}
	return a.client
}

// Decoded JSON answer of a WeCom API request
func (a *Adapter) apiCall(ctx context.Context, method, rawURL string, payload any) (map[string]any, error) {
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return nil, err
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.httpClient(ctx).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var data map[string]any
	if err := dec.Decode(&data); err != nil {
		return nil, fmt.Errorf("WeCom answered HTTP %d with a body that is not a JSON object", resp.StatusCode)
	}
	return data, nil
}

// One text message to the chat's user, refreshing a rejected token once
func (a *Adapter) sendText(ctx context.Context, target *app, chatID, content string) (gateway.SendResult, error) {
	agentID := strings.TrimSpace(target.agentID)
	if agentID == "" {
		agentID = "0"
	}
	agent, err := strconv.ParseInt(agentID, 10, 64)
	if err != nil {
		return gateway.SendResult{Success: false, Error: fmt.Sprintf("agent_id %q of app '%s' is not an integer", target.agentID, target.name)}, nil
	}
	touser := chatID
	if _, user, found := strings.Cut(chatID, ":"); found {
		touser = user
	}
	payload := map[string]any{"touser": touser, "msgtype": "text", "agentid": agent, "text": map[string]any{"content": content}, "safe": 0}
	for attempt := 0; attempt < 2; attempt++ {
		token, err := a.getAccessToken(ctx, target)
		if err == nil {
			var data map[string]any
			if data, err = a.apiCall(ctx, http.MethodPost, sendURL+url.QueryEscape(token), payload); err == nil {
				code, present := errcodeOf(data)
				if tokenRejectedCodes[code] && attempt == 0 {
					slog.Warn(fmt.Sprintf("[WecomCallback] Token rejected for app '%s' (errcode=%d), refreshing", target.name, code))
					a.mu.Lock()
					delete(a.tokens, target.name)
					a.mu.Unlock()
					continue
				}
				if present && code == 0 {
					return gateway.SendResult{Success: true, MessageID: textOf(data["msgid"]), RawResponse: data}, nil
				}
				encoded, _ := json.Marshal(data)
				return gateway.SendResult{Success: false, Error: string(encoded), RawResponse: data}, nil
			}
		}
		if ctx.Err() != nil {
			return gateway.SendResult{}, ctx.Err()
		}
		return gateway.SendResult{Success: false, Error: err.Error()}, nil
	}
	return gateway.SendResult{Success: false, Error: "send failed after token refresh"}, nil
}

// App that last heard from the chat, a unique legacy bare user match, else the first app
func (a *Adapter) resolveAppForChat(chatID string) (*app, bool) {
	if len(a.apps) == 0 {
		return nil, false
	}
	a.mu.Lock()
	name := a.userApps[chatID]
	if name == "" && !strings.Contains(chatID, ":") {
		var matching []string
		for key := range a.userApps {
			if strings.HasSuffix(key, ":"+chatID) {
				matching = append(matching, key)
			}
		}
		if len(matching) == 1 {
			name = a.userApps[matching[0]]
		}
	}
	a.mu.Unlock()
	for i := range a.apps {
		if name != "" && a.apps[i].name == name {
			return &a.apps[i], true
		}
	}
	return &a.apps[0], true
}

// Chat info of a user chat
func (a *Adapter) GetChatInfo(ctx context.Context, chatID string) (map[string]any, error) {
	return map[string]any{"name": chatID, "type": "dm"}, nil
}

// Liveness probe of the callback server
func (a *Adapter) handleHealth(ctx context.Context, request *gateway.WebRequest) (*gateway.WebResponse, error) {
	return gateway.WebJSONResponse(map[string]any{"status": "ok", "platform": "wecom_callback"}, http.StatusOK, nil)
}

// Signature, timestamp and nonce of a callback request
func signatureParams(request *gateway.WebRequest) (string, string, string) {
	q := request.Query()
	return q.Get("msg_signature"), q.Get("timestamp"), q.Get("nonce")
}

// URL verification handshake answering the decrypted echostr of the first app it verifies under
func (a *Adapter) handleVerify(ctx context.Context, request *gateway.WebRequest) (*gateway.WebResponse, error) {
	msgSignature, timestamp, nonce := signatureParams(request)
	echostr := request.QueryGet("echostr")
	for i := range a.apps {
		crypt, err := a.cryptFor(&a.apps[i])
		if err != nil {
			continue
		}
		if plain, err := crypt.verifyURL(msgSignature, timestamp, nonce, echostr); err == nil {
			return gateway.WebTextResponse(http.StatusOK, plain, "text/plain", nil), nil
		}
	}
	return gateway.WebTextResponse(http.StatusForbidden, "signature verification failed", "", nil), nil
}

// Outer callback envelope carrying the ciphertext
type envelope struct {
	Encrypt string `xml:"Encrypt"`
}

// Decrypted callback message fields, nil pointers for absent elements
type callbackMessage struct {
	MsgType      *string `xml:"MsgType"`
	Event        *string `xml:"Event"`
	FromUserName *string `xml:"FromUserName"`
	ToUserName   *string `xml:"ToUserName"`
	Content      *string `xml:"Content"`
	MsgID        *string `xml:"MsgId"`
	CreateTime   *string `xml:"CreateTime"`
}

// Text of an optional element
func elem(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// Plain ack WeCom expects before its retry timer fires
func ack() *gateway.WebResponse {
	return gateway.WebTextResponse(http.StatusOK, "success", "text/plain", nil)
}

// Decrypts a callback under the first app it verifies with, acks it at once and runs the turn in the background
func (a *Adapter) handleCallback(ctx context.Context, request *gateway.WebRequest) (*gateway.WebResponse, error) {
	msgSignature, timestamp, nonce := signatureParams(request)
	body, err := request.Read()
	if err != nil {
		return nil, err
	}
	if len(body) > maxBodyBytes {
		slog.Warn(fmt.Sprintf("[WecomCallback] Payload too large (%d bytes) — rejected", len(body)))
		return gateway.WebTextResponse(http.StatusRequestEntityTooLarge, "payload too large", "", nil), nil
	}
	invalid := gateway.WebTextResponse(http.StatusBadRequest, "invalid callback payload", "", nil)
	var outer envelope
	if err := xml.Unmarshal(body, &outer); err != nil {
		slog.Error(fmt.Sprintf("[WecomCallback] Error handling message: %v", err))
		return invalid, nil
	}
	for i := range a.apps {
		target := &a.apps[i]
		crypt, err := a.cryptFor(target)
		if err != nil {
			continue
		}
		plain, err := crypt.decrypt(msgSignature, timestamp, nonce, outer.Encrypt)
		if err != nil {
			continue
		}
		event, err := a.buildEvent(ctx, target, plain)
		if err != nil {
			slog.Error(fmt.Sprintf("[WecomCallback] Error handling message: %v", err))
			return invalid, nil
		}
		if event == nil {
			return ack(), nil
		}
		if id := *event.MessageID; id != "" && a.dedup.IsDuplicate(id) {
			slog.Debug(fmt.Sprintf("[WecomCallback] Duplicate MsgId %s, skipping", id))
			return ack(), nil
		}
		if event.Source != nil && event.Source.UserID != nil && *event.Source.UserID != "" {
			a.mu.Lock()
			a.userApps[userAppKey(target.corpID, *event.Source.UserID)] = target.name
			a.mu.Unlock()
		}
		task := gateway.SpawnAdapterTask(context.WithoutCancel(ctx), func(tctx context.Context) error { return a.HandleMessage(tctx, event) })
		a.TrackBackgroundTask(task)
		return ack(), nil
	}
	return invalid, nil
}

// Message event of a decrypted text or event callback, nil for lifecycle events and other message types
func (a *Adapter) buildEvent(ctx context.Context, target *app, plain []byte) (*gateway.MessageEvent, error) {
	if !utf8.Valid(plain) {
		return nil, errors.New("decrypted callback is not UTF-8")
	}
	var msg callbackMessage
	if err := xml.Unmarshal(plain, &msg); err != nil {
		return nil, err
	}
	msgType := strings.ToLower(elem(msg.MsgType))
	if msgType != "text" && msgType != "event" {
		return nil, nil
	}
	if msgType == "event" {
		switch strings.ToLower(elem(msg.Event)) {
		case "enter_agent", "subscribe":
			return nil, nil
		}
	}
	userID := elem(msg.FromUserName)
	corpID := target.corpID
	if msg.ToUserName != nil {
		corpID = *msg.ToUserName
	}
	content := strings.TrimSpace(elem(msg.Content))
	if content == "" && msgType == "event" {
		content = "/start"
	}
	msgID := elem(msg.MsgID)
	if msgID == "" {
		created := "0"
		if msg.CreateTime != nil {
			created = *msg.CreateTime
		}
		msgID = userID + ":" + created
	}
	user := userID
	source := a.BuildSource(ctx, userAppKey(corpID, userID), gateway.SourceFields{ChatName: &user, ChatType: "dm", UserID: &user, UserName: &user})
	event := gateway.NewMessageEvent(content)
	event.MessageType, event.Source, event.RawMessage, event.MessageID = gateway.MessageTypeText, source, string(plain), &msgID
	return event, nil
}

// Message crypto of one app
func (a *Adapter) cryptFor(target *app) (*msgCrypt, error) {
	return newMsgCrypt(target.token, target.encodingAESKey, target.corpID)
}

// Cached access token of the app, refreshed when it expires within a minute
func (a *Adapter) getAccessToken(ctx context.Context, target *app) (string, error) {
	a.mu.Lock()
	cached, ok := a.tokens[target.name]
	a.mu.Unlock()
	if ok && cached.expiresAt.After(time.Now().Add(tokenRefreshMargin)) {
		return cached.value, nil
	}
	return a.refreshAccessToken(ctx, target)
}

// Fresh access token of the app from gettoken, cached for its lifetime
func (a *Adapter) refreshAccessToken(ctx context.Context, target *app) (string, error) {
	query := url.Values{"corpid": {target.corpID}, "corpsecret": {target.corpSecret}}
	data, err := a.apiCall(ctx, http.MethodGet, tokenURL+"?"+query.Encode(), nil)
	if err != nil {
		return "", err
	}
	if code, present := errcodeOf(data); !present || code != 0 {
		encoded, _ := json.Marshal(data)
		return "", errors.New("WeCom token refresh failed: " + string(encoded))
	}
	token := textOf(data["access_token"])
	if token == "" {
		return "", errors.New("WeCom token refresh returned no access_token")
	}
	expiresIn := int64(accessTokenTTL)
	if raw, ok := data["expires_in"].(json.Number); ok {
		if n, err := raw.Int64(); err == nil {
			expiresIn = n
		}
	}
	a.mu.Lock()
	a.tokens[target.name] = accessToken{value: token, expiresAt: time.Now().Add(time.Duration(expiresIn) * time.Second)}
	a.mu.Unlock()
	slog.Info(fmt.Sprintf("[WecomCallback] Token refreshed for app '%s' (corp=%s), expires in %ds", target.name, target.corpID, expiresIn))
	return token, nil
}

// Adapter class the gateway migration code reads the profile prefix trait from
var adapterClass = &gateway.AdapterClass{Name: "WecomCallbackAdapter", Traits: adapterTraits(), New: newPlatformAdapter}

func init() {
	gateway.RegisterAdapterModule(adapterClass)
}
