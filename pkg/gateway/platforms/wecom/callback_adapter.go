package wecom

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyhttpx"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const (
	callbackDefaultPort         = 8645
	callbackDefaultPath         = "/wecom/callback"
	callbackMaxBody             = 65_536
	accessTokenTTLSeconds       = 7200
	messageDedupTTLSeconds      = 300
	callbackSendURL             = "https://qyapi.weixin.qq.com/cgi-bin/message/send?access_token="
	callbackTokenURL            = "https://qyapi.weixin.qq.com/cgi-bin/gettoken"
	callbackMaxMessageLength    = 2048
	callbackRequestTimeout      = 20 * time.Second
	callbackDedupDefaultMaxSize = 2000
)

// Wall clock seconds the access token expiry is measured on
var callbackNow = func() float64 { return float64(time.Now().UnixNano()) / 1e9 }

// Length of the text in UTF-8 bytes, the unit message/send caps content in
func utf8Len(text string) int { return len(text) }

// Callback mode needs no optional dependency in this build
func checkWeComCallbackRequirements() bool { return true }

// Dependency installer hook of the registry, satisfied since every dependency is compiled in
func ensureWeComCallbackRequirements(context.Context) (bool, error) {
	return checkWeComCallbackRequirements(), nil
}

// Plain success acknowledgement of an accepted callback
func callbackAck() *gateway.WebResponse {
	return gateway.WebTextResponse(http.StatusOK, "success", "text/plain", nil)
}

// One parsed XML element with the text before its first child and its direct children
type xmlElement struct {
	tag      string
	text     string
	children []*xmlElement
}

// Text of the first direct child with the tag, the default when no child has it
func (e *xmlElement) findtext(tag, def string) string {
	for _, child := range e.children {
		if child.tag == tag {
			return child.text
		}
	}
	return def
}

// Root element of an untrusted XML document, entity declarations refused before any expansion
func parseXML(text string) (*xmlElement, error) {
	decoder := xml.NewDecoder(strings.NewReader(text))
	var root *xmlElement
	var stack []*xmlElement
	sawChild := map[*xmlElement]bool{}
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, &pyErr{"ParseError", err.Error()}
		}
		switch t := token.(type) {
		case xml.Directive:
			if bytes.Contains(bytes.ToUpper(t), []byte("<!ENTITY")) {
				return nil, &pyErr{"EntitiesForbidden", "EntitiesForbidden(name='entity', system_id=None, public_id=None)"}
			}
		case xml.StartElement:
			elem := &xmlElement{tag: t.Name.Local}
			if len(stack) == 0 {
				if root != nil {
					return nil, &pyErr{"ParseError", "junk after document element"}
				}
				root = elem
			} else {
				parent := stack[len(stack)-1]
				parent.children = append(parent.children, elem)
				sawChild[parent] = true
			}
			stack = append(stack, elem)
		case xml.EndElement:
			stack = stack[:len(stack)-1]
		case xml.CharData:
			if len(stack) > 0 && !sawChild[stack[len(stack)-1]] {
				stack[len(stack)-1].text += string(t)
			}
		}
	}
	if root == nil {
		return nil, &pyErr{"ParseError", "no element found"}
	}
	return root, nil
}

// Unbounded first in first out queue of inbound events
type eventQueue struct {
	mu     sync.Mutex
	events []*gateway.MessageEvent
	notify chan struct{}
}

func newEventQueue() *eventQueue { return &eventQueue{notify: make(chan struct{}, 1)} }

// Appends an event without ever blocking the producer
func (q *eventQueue) put(event *gateway.MessageEvent) {
	q.mu.Lock()
	q.events = append(q.events, event)
	q.mu.Unlock()
	select {
	case q.notify <- struct{}{}:
	default:
	}
}

// Next event, waiting until one arrives or the context ends
func (q *eventQueue) get(ctx context.Context) (*gateway.MessageEvent, error) {
	for {
		q.mu.Lock()
		if len(q.events) > 0 {
			event := q.events[0]
			q.events = q.events[1:]
			q.mu.Unlock()
			return event, nil
		}
		q.mu.Unlock()
		select {
		case <-q.notify:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// Cached access token of one app with its expiry in wall clock seconds
type accessToken struct {
	token     any
	expiresAt float64
}

// Class attributes of the WeCom callback adapter
func callbackAdapterTraits() gateway.AdapterTraits {
	t := gateway.DefaultAdapterTraits()
	t.ServesProfilePrefix = true
	t.MaxMessageLength = callbackMaxMessageLength
	t.SplitsLongMessages = true
	return t
}

// WecomCallbackAdapter serves self built apps, decrypting posted XML callbacks and replying through message/send
type WecomCallbackAdapter struct {
	*gateway.BasePlatformAdapter

	host *string
	port int
	path string
	apps []map[string]any

	mu           sync.Mutex
	runner       *gateway.WebAppRunner
	httpClient   *http.Client
	pollTask     *gateway.AdapterTask
	messageQueue *eventQueue
	dedup        *gateway.MessageDeduplicator
	userAppMap   map[string]any
	accessTokens map[string]accessToken
}

// Adapter configured from the platform extra, the int error for a malformed port
func NewWecomCallbackAdapter(_ context.Context, cfg *gateway.PlatformConfig) (*WecomCallbackAdapter, error) {
	if cfg == nil {
		cfg = gateway.NewPlatformConfig()
	}
	a := &WecomCallbackAdapter{messageQueue: newEventQueue(), userAppMap: map[string]any{}, accessTokens: map[string]accessToken{},
		dedup: gateway.NewMessageDeduplicator(callbackDedupDefaultMaxSize, messageDedupTTLSeconds)}
	a.BasePlatformAdapter = gateway.NewBasePlatformAdapter(cfg, gateway.PlatformWeComCallback, a)
	a.Traits = callbackAdapterTraits()
	extra := cfg.Extra
	if extra == nil {
		extra = map[string]any{}
	}
	if rawHost := extra["host"]; pyrepr.Truthy(rawHost) {
		host := pyrepr.Str(rawHost)
		a.host = &host
	}
	var rawPort any = int64(callbackDefaultPort)
	if v := extra["port"]; pyrepr.Truthy(v) {
		rawPort = v
	}
	port, err := pyint.Int64(rawPort)
	if err != nil {
		return nil, err
	}
	a.port = int(port)
	a.path = callbackDefaultPath
	if v := extra["path"]; pyrepr.Truthy(v) {
		a.path = pyrepr.Str(v)
	}
	a.apps = normalizeApps(extra)
	return a, nil
}

// Key of the user to app map, the corp scoped user id or the bare user id without a corp
func userAppKey(corpID, userID string) string {
	if corpID != "" {
		return corpID + ":" + userID
	}
	return userID
}

// Callback apps of the extra, the apps list when given and else the single app of the flat keys
func normalizeApps(extra map[string]any) []map[string]any {
	if apps, ok := extra["apps"].([]any); ok && len(apps) > 0 {
		out := []map[string]any{}
		for _, raw := range apps {
			if app, isDict := pyjson.Dict(raw); isDict {
				copied := make(map[string]any, len(app))
				for k, v := range app {
					copied[k] = v
				}
				out = append(out, copied)
			}
		}
		return out
	}
	if !pyrepr.Truthy(extra["corp_id"]) {
		return []map[string]any{}
	}
	return []map[string]any{{"name": pyrepr.Or(extra["name"], "default"), "corp_id": extraOr(extra, "corp_id", ""),
		"corp_secret": extraOr(extra, "corp_secret", ""), "agent_id": pyrepr.Str(extraOr(extra, "agent_id", "")),
		"token": extraOr(extra, "token", ""), "encoding_aes_key": extraOr(extra, "encoding_aes_key", "")}}
}

// str of an app field with its falsy value read as empty
func appText(app map[string]any, key string) string {
	if v := app[key]; pyrepr.Truthy(v) {
		return pyrepr.Str(v)
	}
	return ""
}

// Name of an app for log lines, default when the key is absent
func appLabel(app map[string]any) string { return pyrepr.Str(extraOr(app, "name", "default")) }

// Binds or publishes the callback listener, starts the poll loop and refreshes every app token
func (a *WecomCallbackAdapter) Connect(ctx context.Context, isReconnect bool) (bool, error) {
	if len(a.apps) == 0 {
		slog.Warn("[WecomCallback] No callback apps configured")
		return false, nil
	}
	if gateway.SharedIngressProfile(a) == "" {
		if conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", a.port), time.Second); err == nil {
			conn.Close()
			slog.Error(fmt.Sprintf("[WecomCallback] Port %d already in use", a.port))
			return false, nil
		}
	}
	if err := a.start(ctx); err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		a.cleanup(ctx)
		slog.Error("[WecomCallback] Failed to start", "error", err)
		return false, nil
	}
	return true, nil
}

// Listener, poll loop and initial token refresh of a connect
func (a *WecomCallbackAdapter) start(ctx context.Context) error {
	client := gateway.LimitedHTTPClient(gateway.PlatformHTTPXLimits(), false)
	a.mu.Lock()
	a.httpClient = client
	a.mu.Unlock()
	app := gateway.NewWebApplication(callbackMaxBody)
	if err := app.AddGet("/health", a.handleHealth); err != nil {
		return err
	}
	if err := app.AddGet(a.path, a.handleVerify); err != nil {
		return err
	}
	if err := app.AddPost(a.path, a.handleCallback); err != nil {
		return err
	}
	runner, err := gateway.BindListener(ctx, a, app, a.host, a.port, a.path, gateway.ListenOptions{})
	if err != nil {
		return err
	}
	poll := gateway.SpawnAdapterTask(ctx, a.pollLoop)
	a.mu.Lock()
	a.runner, a.pollTask = runner, poll
	a.mu.Unlock()
	a.MarkConnected(ctx, "")
	if runner != nil {
		host := "None"
		if a.host != nil {
			host = *a.host
		}
		slog.Info(fmt.Sprintf("[WecomCallback] HTTP server listening on %s:%d%s", host, a.port, a.path))
	}
	for _, app := range a.apps {
		if _, err := a.refreshAccessToken(ctx, app); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			slog.Warn(fmt.Sprintf("[WecomCallback] Initial token refresh failed for app '%s': %s", appLabel(app), errText(err)))
		}
	}
	return nil
}

// Stops the poll loop, the listener and the HTTP client
func (a *WecomCallbackAdapter) Disconnect(ctx context.Context) error {
	a.SetRunning(false)
	a.mu.Lock()
	poll := a.pollTask
	a.pollTask = nil
	a.mu.Unlock()
	gateway.CancelTask(ctx, poll)
	err := a.cleanup(ctx)
	a.MarkDisconnected(ctx)
	slog.Info("[WecomCallback] Disconnected")
	return err
}

// Stops the listener and releases the HTTP client
func (a *WecomCallbackAdapter) cleanup(ctx context.Context) error {
	a.mu.Lock()
	runner, client := a.runner, a.httpClient
	a.runner, a.httpClient = nil, nil
	a.mu.Unlock()
	var err error
	if runner != nil {
		err = runner.Cleanup(ctx)
	}
	if client != nil {
		client.CloseIdleConnections()
	}
	return err
}

// Sends one text message per byte capped chunk through the app that last heard from the chat
func (a *WecomCallbackAdapter) Send(ctx context.Context, chatID, content, replyTo string, metadata map[string]any) (gateway.SendResult, error) {
	app, err := a.resolveAppForChat(chatID)
	if err != nil {
		return gateway.SendResult{}, err
	}
	chunks := gateway.TruncateMessage(content, callbackMaxMessageLength, utf8Len)
	result, err := gateway.SendChunks(ctx, chunks, func(ctx context.Context, chunk string) (*gateway.SendResult, error) {
		sent, err := a.sendText(ctx, app, chatID, chunk)
		return &sent, err
	})
	if err != nil || result == nil {
		return gateway.SendResult{}, err
	}
	return *result, nil
}

// Posts one text message, refreshing a rejected access token once
func (a *WecomCallbackAdapter) sendText(ctx context.Context, app map[string]any, chatID, content string) (gateway.SendResult, error) {
	result, err := a.postText(ctx, app, chatID, content)
	if err != nil {
		if ctx.Err() != nil {
			return gateway.SendResult{}, ctx.Err()
		}
		return gateway.SendResult{Success: false, Error: errText(err)}, nil
	}
	return result, nil
}

// One message/send exchange with the token retry, the error the caller turns into a failed result
func (a *WecomCallbackAdapter) postText(ctx context.Context, app map[string]any, chatID, content string) (gateway.SendResult, error) {
	var agentRaw any = int64(0)
	if v := app["agent_id"]; pyrepr.Truthy(v) {
		agentRaw = v
	}
	agentID, err := pyint.Of(pyrepr.Str(agentRaw))
	if err != nil {
		return gateway.SendResult{}, err
	}
	parts := strings.SplitN(chatID, ":", 2)
	payload := obj("touser", parts[len(parts)-1], "msgtype", "text", "agentid", pyint.Value(agentID), "text", obj("content", content), "safe", int64(0))
	body, err := pyjson.Dumps(payload, pyjson.EnsureASCII(false), pyjson.Separators(",", ":"), pyjson.AllowNaN(false))
	if err != nil {
		return gateway.SendResult{}, err
	}
	for attempt := 0; attempt < 2; attempt++ {
		token, err := a.getAccessToken(ctx, app)
		if err != nil {
			return gateway.SendResult{}, err
		}
		a.mu.Lock()
		client := a.httpClient
		a.mu.Unlock()
		if client == nil {
			return gateway.SendResult{}, &pyErr{"AttributeError", "'NoneType' object has no attribute 'post'"}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, callbackSendURL+pyrepr.Str(token), bytes.NewReader([]byte(body)))
		if err != nil {
			return gateway.SendResult{}, err
		}
		req.Header.Set("Content-Type", "application/json")
		data, err := callbackJSON(client, req)
		if err != nil {
			return gateway.SendResult{}, err
		}
		errcode := data.Vals["errcode"]
		if (pyjson.Equal(errcode, int64(40001)) || pyjson.Equal(errcode, int64(42001))) && attempt == 0 {
			slog.Warn(fmt.Sprintf("[WecomCallback] Token rejected for app '%s' (errcode=%s), refreshing", appLabel(app), pyrepr.Str(errcode)))
			name, err := appName(app)
			if err != nil {
				return gateway.SendResult{}, err
			}
			a.mu.Lock()
			delete(a.accessTokens, name)
			a.mu.Unlock()
			continue
		}
		if errcode != nil && pyjson.Equal(errcode, int64(0)) {
			return gateway.SendResult{Success: true, MessageID: pyrepr.Str(getOr(data, "msgid", "")), RawResponse: data}, nil
		}
		return gateway.SendResult{Success: false, Error: pyrepr.Str(data)}, nil
	}
	return gateway.SendResult{Success: false, Error: "send failed after token refresh"}, nil
}

// Sends a request and decodes its JSON object body, the AttributeError a non object body raises on get
func callbackJSON(client *http.Client, req *http.Request) (*pyjson.Object, error) {
	resp, err := pyhttpx.Do(client, req, callbackRequestTimeout)
	if err != nil {
		if req.Context().Err() != nil {
			return nil, req.Context().Err()
		}
		var te *pyhttpx.TimeoutError
		if errors.As(err, &te) {
			return nil, &pyErr{"ReadTimeout", ""}
		}
		return nil, &pyErr{"ConnectError", pyhttpx.ErrorText(err)}
	}
	decoded, err := resp.JSON()
	if err != nil {
		return nil, err
	}
	data, ok := decoded.(*pyjson.Object)
	if !ok || data == nil {
		return nil, &pyErr{"AttributeError", fmt.Sprintf("'%s' object has no attribute 'get'", pyrepr.TypeName(decoded))}
	}
	return data, nil
}

// Name of an app as its token cache key, the KeyError of an app without one
func appName(app map[string]any) (string, error) {
	name, ok := app["name"]
	if !ok {
		return "", &pyErr{"KeyError", "'name'"}
	}
	return pyrepr.Str(name), nil
}

// App that last heard from the chat, a bare user id matching only a unique corp scoped key, else the first app
func (a *WecomCallbackAdapter) resolveAppForChat(chatID string) (map[string]any, error) {
	a.mu.Lock()
	appName := a.userAppMap[chatID]
	if !pyrepr.Truthy(appName) && !strings.Contains(chatID, ":") {
		var matching []string
		for k := range a.userAppMap {
			if strings.HasSuffix(k, ":"+chatID) {
				matching = append(matching, k)
			}
		}
		if len(matching) == 1 {
			appName = a.userAppMap[matching[0]]
		}
	}
	a.mu.Unlock()
	if app := a.getAppByName(appName); app != nil {
		return app, nil
	}
	if len(a.apps) == 0 {
		return nil, &pyErr{"IndexError", "list index out of range"}
	}
	return a.apps[0], nil
}

// Name and type of a chat, callback chats being direct
func (a *WecomCallbackAdapter) GetChatInfo(ctx context.Context, chatID string) (map[string]any, error) {
	return map[string]any{"name": chatID, "type": "dm"}, nil
}

// Health probe of the callback listener
func (a *WecomCallbackAdapter) handleHealth(context.Context, *gateway.WebRequest) (*gateway.WebResponse, error) {
	return gateway.WebJSONResponse(obj("status", "ok", "platform", "wecom_callback"), http.StatusOK, nil)
}

// Signature, timestamp and nonce query parameters of a callback request
func signatureParams(request *gateway.WebRequest) (string, string, string) {
	query := request.Query()
	return query.Get("msg_signature"), query.Get("timestamp"), query.Get("nonce")
}

// URL verification handshake, the decrypted echostr of the first app whose signature matches
func (a *WecomCallbackAdapter) handleVerify(_ context.Context, request *gateway.WebRequest) (*gateway.WebResponse, error) {
	msgSignature, timestamp, nonce := signatureParams(request)
	echostr := request.Query().Get("echostr")
	for _, app := range a.apps {
		crypt, err := cryptForApp(app)
		if err != nil {
			continue
		}
		plain, err := crypt.VerifyURL(msgSignature, timestamp, nonce, echostr)
		if err != nil {
			continue
		}
		return gateway.WebTextResponse(http.StatusOK, plain, "text/plain", nil), nil
	}
	return gateway.WebTextResponse(http.StatusForbidden, "signature verification failed", "", nil), nil
}

// Decrypts a posted callback with the first app whose crypto accepts it and queues its event
func (a *WecomCallbackAdapter) handleCallback(ctx context.Context, request *gateway.WebRequest) (*gateway.WebResponse, error) {
	msgSignature, timestamp, nonce := signatureParams(request)
	bodyBytes, err := request.Read()
	if err != nil {
		return nil, err
	}
	if len(bodyBytes) > callbackMaxBody {
		slog.Warn(fmt.Sprintf("[WecomCallback] Payload too large (%d bytes) — rejected", len(bodyBytes)))
		return gateway.WebTextResponse(http.StatusRequestEntityTooLarge, "payload too large", "", nil), nil
	}
	body := pystr.DecodeUTF8Replace(bodyBytes)
	for _, app := range a.apps {
		event, err := a.eventOfCallback(ctx, app, body, msgSignature, timestamp, nonce)
		var cryptoErr WeComCryptoError
		if errors.As(err, &cryptoErr) {
			continue
		}
		if err != nil {
			slog.Error("[WecomCallback] Error handling message", "error", err)
			break
		}
		if event == nil {
			return callbackAck(), nil
		}
		if event.MessageID != nil && *event.MessageID != "" && a.dedup.IsDuplicate(*event.MessageID) {
			slog.Debug(fmt.Sprintf("[WecomCallback] Duplicate MsgId %s, skipping", *event.MessageID))
			return callbackAck(), nil
		}
		if event.Source != nil && event.Source.UserID != nil && *event.Source.UserID != "" {
			name, ok := app["name"]
			if !ok {
				slog.Error("[WecomCallback] Error handling message", "error", &pyErr{"KeyError", "'name'"})
				break
			}
			a.mu.Lock()
			a.userAppMap[userAppKey(appText(app, "corp_id"), *event.Source.UserID)] = name
			a.mu.Unlock()
		}
		a.messageQueue.put(event)
		return callbackAck(), nil
	}
	return gateway.WebTextResponse(http.StatusBadRequest, "invalid callback payload", "", nil), nil
}

// Event of one callback body decrypted with the app, nil for lifecycle and non text messages
func (a *WecomCallbackAdapter) eventOfCallback(ctx context.Context, app map[string]any, body, msgSignature, timestamp, nonce string) (*gateway.MessageEvent, error) {
	xmlText, err := decryptRequest(app, body, msgSignature, timestamp, nonce)
	if err != nil {
		return nil, err
	}
	return a.buildEvent(ctx, app, xmlText)
}

// Hands every queued event to the gateway in its own tracked background task
func (a *WecomCallbackAdapter) pollLoop(ctx context.Context) error {
	for {
		event, err := a.messageQueue.get(ctx)
		if err != nil {
			return nil
		}
		a.TrackBackgroundTask(gateway.SpawnAdapterTask(ctx, func(tctx context.Context) error { return a.HandleMessage(tctx, event) }))
	}
}

// Decrypted XML text of a callback body under one app
func decryptRequest(app map[string]any, body, msgSignature, timestamp, nonce string) (string, error) {
	root, err := parseXML(body)
	if err != nil {
		return "", err
	}
	crypt, err := cryptForApp(app)
	if err != nil {
		return "", err
	}
	raw, err := crypt.Decrypt(msgSignature, timestamp, nonce, root.findtext("Encrypt", ""))
	if err != nil {
		return "", err
	}
	return pystr.DecodeUTF8(raw)
}

// Text event of a decrypted callback, a bare event becoming the start command and lifecycle events dropped
func (a *WecomCallbackAdapter) buildEvent(ctx context.Context, app map[string]any, xmlText string) (*gateway.MessageEvent, error) {
	root, err := parseXML(xmlText)
	if err != nil {
		return nil, err
	}
	msgType := pystr.Lower(root.findtext("MsgType", ""))
	if msgType != "text" && msgType != "event" {
		return nil, nil
	}
	if eventName := pystr.Lower(root.findtext("Event", "")); msgType == "event" && (eventName == "enter_agent" || eventName == "subscribe") {
		return nil, nil
	}
	userID := root.findtext("FromUserName", "")
	corpID := root.findtext("ToUserName", pyrepr.Str(extraOr(app, "corp_id", "")))
	content := pystr.Strip(root.findtext("Content", ""))
	if content == "" && msgType == "event" {
		content = "/start"
	}
	msgID := root.findtext("MsgId", "")
	if msgID == "" {
		msgID = userID + ":" + root.findtext("CreateTime", "0")
	}
	source := a.BuildSource(ctx, userAppKey(corpID, userID), gateway.SourceFields{ChatName: &userID, ChatType: "dm", UserID: &userID, UserName: &userID})
	event := gateway.NewMessageEvent(content)
	event.MessageType, event.Source, event.RawMessage, event.MessageID = gateway.MessageTypeText, source, xmlText, &msgID
	return event, nil
}

// Callback crypto of an app from its token, AES key and corp id
func cryptForApp(app map[string]any) (*WXBizMsgCrypt, error) {
	return NewWXBizMsgCrypt(appText(app, "token"), appText(app, "encoding_aes_key"), appText(app, "corp_id"))
}

// App configured under a name, nil for a blank name or no match
func (a *WecomCallbackAdapter) getAppByName(name any) map[string]any {
	if !pyrepr.Truthy(name) {
		return nil
	}
	for _, app := range a.apps {
		if pyrepr.Equal(app["name"], name) {
			return app
		}
	}
	return nil
}

// Cached access token of an app while it has over a minute left, else a refreshed one
func (a *WecomCallbackAdapter) getAccessToken(ctx context.Context, app map[string]any) (any, error) {
	name, err := appName(app)
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	cached, ok := a.accessTokens[name]
	a.mu.Unlock()
	if ok && cached.expiresAt > callbackNow()+60 {
		return cached.token, nil
	}
	return a.refreshAccessToken(ctx, app)
}

// Query text of a parameter value, empty for None
func paramText(v any) string {
	if v == nil {
		return ""
	}
	return pyrepr.Str(v)
}

// Fetches and caches a fresh access token of an app, a RuntimeError when WeCom refuses
func (a *WecomCallbackAdapter) refreshAccessToken(ctx context.Context, app map[string]any) (any, error) {
	a.mu.Lock()
	client := a.httpClient
	a.mu.Unlock()
	if client == nil {
		return nil, &pyErr{"AttributeError", "'NoneType' object has no attribute 'get'"}
	}
	rawURL := pyhttpx.MergeParams(callbackTokenURL, pyhttpx.Params{{"corpid", paramText(app["corp_id"])}, {"corpsecret", paramText(app["corp_secret"])}})
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	data, err := callbackJSON(client, req)
	if err != nil {
		return nil, err
	}
	if errcode := data.Vals["errcode"]; errcode == nil || !pyjson.Equal(errcode, int64(0)) {
		return nil, runtimeError("WeCom token refresh failed: " + pyrepr.Str(data))
	}
	token, ok := data.Vals["access_token"]
	if !ok {
		return nil, &pyErr{"KeyError", "'access_token'"}
	}
	expiresIn, err := pyint.Int64(getOr(data, "expires_in", int64(accessTokenTTLSeconds)))
	if err != nil {
		return nil, err
	}
	name, err := appName(app)
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	a.accessTokens[name] = accessToken{token: token, expiresAt: callbackNow() + float64(expiresIn)}
	a.mu.Unlock()
	slog.Info(fmt.Sprintf("[WecomCallback] Token refreshed for app '%s' (corp=%s), expires in %ds", appLabel(app), pyrepr.Str(extraOr(app, "corp_id", "")), expiresIn))
	return token, nil
}
