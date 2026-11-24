package dingtalk

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"runtime"
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
	streamSDKVersion      = "0.24.3"
	chatbotTopic          = "/v1.0/im/bot/messages/get"
	streamTagDisconnect   = "disconnect"
	ackStatusOK           = 200
	ackStatusNotImplement = 404
	ackStatusSystemError  = 500
	contentTypeJSON       = "application/json"
)

// DingTalk OpenAPI base URL from dingtalk.openapi_endpoint
func openAPIEndpoint(ctx context.Context) string {
	return strings.TrimRight(strings.TrimSpace(settings.DingtalkOpenapiEndpoint.Get(gateway.SettingsContext(ctx))), "/")
}

// Frame headers of a stream message, the names the SDK maps kept apart from the extension names
type streamHeaders struct {
	messageID any
	topic     any
}

// Headers of a frame, the conversion error of a time field that is not a number
func parseStreamHeaders(value any) (streamHeaders, error) {
	var h streamHeaders
	obj, ok := value.(*jsonutil.Object)
	if !ok {
		return h, kindError("frame headers", "an object", value)
	}
	for _, name := range obj.Keys() {
		field := obj.Value(name)
		switch name {
		case "messageId":
			h.messageID = field
		case "topic":
			h.topic = field
		case "time", "eventBornTime":
			if _, err := anyutil.ToInt(field); err != nil {
				return h, err
			}
		}
	}
	return h, nil
}

// One decoded stream frame with its headers and its data payload parsed from the JSON string it carries
type streamMessage struct {
	headers streamHeaders
	data    any
}

// Frame of a decoded JSON message, the data string parsed when it is non empty
func parseStreamMessage(d *jsonutil.Object) (*streamMessage, error) {
	msg := &streamMessage{data: jsonutil.NewObject()}
	var data any = ""
	for _, name := range d.Keys() {
		switch name {
		case "data":
			data = d.Value(name)
		case "headers":
			headers, err := parseStreamHeaders(d.Value(name))
			if err != nil {
				return nil, err
			}
			msg.headers = headers
		}
	}
	if anyutil.IsTrue(data) {
		text, ok := data.(string)
		if !ok {
			return nil, kindError("frame data", "a string", data)
		}
		parsed, err := jsonutil.Decode(text)
		if err != nil {
			return nil, err
		}
		msg.data = parsed
	}
	return msg, nil
}

// Acknowledgement frame answering one stream message
type ackMessage struct {
	code      int
	messageID any
	message   string
	data      any
}

// Wire form of the acknowledgement, the data serialized to a JSON string
func (a *ackMessage) toDict() (*jsonutil.Object, error) {
	headers := jsonutil.ObjectOf("contentType", contentTypeJSON)
	if a.messageID != nil {
		headers.Set("messageId", a.messageID)
	}
	data, err := jsonutil.Encode(a.data)
	if err != nil {
		return nil, err
	}
	return jsonutil.ObjectOf("code", a.code, "headers", headers, "message", a.message, "data", data), nil
}

// Handler of one callback topic, answering a message with an acknowledgement code and response text
type callbackHandler interface {
	preStart()
	process(ctx context.Context, message *streamMessage) (int, string)
}

// Stream Mode client keeping one WebSocket to the DingTalk gateway and routing its frames to handlers
type streamClient struct {
	clientID     any
	clientSecret any

	mu          sync.Mutex
	logger      *slog.Logger
	topics      []string
	handlers    map[string]callbackHandler
	websocket   *websocketutil.Conn
	preStarted  bool
	accessToken *jsonutil.Object
}

// Client for the app credentials logging as the SDK client logger
func newStreamClient(clientID, clientSecret any) *streamClient {
	return &streamClient{clientID: clientID, clientSecret: clientSecret, handlers: map[string]callbackHandler{},
		logger: pkgLog()}
}

// Logger the client reports through
func (c *streamClient) log() *slog.Logger {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.logger
}

// Replaces the logger the client reports through and returns the previous one
func (c *streamClient) swapLogger(next *slog.Logger) *slog.Logger {
	c.mu.Lock()
	defer c.mu.Unlock()
	prev := c.logger
	c.logger = next
	return prev
}

// Routes a callback topic to the handler and subscribes to it on the next connection
func (c *streamClient) registerCallbackHandler(topic string, handler callbackHandler) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, known := c.handlers[topic]; !known {
		c.topics = append(c.topics, topic)
	}
	c.handlers[topic] = handler
}

// Runs every handler's start hook once
func (c *streamClient) preStart() {
	c.mu.Lock()
	if c.preStarted {
		c.mu.Unlock()
		return
	}
	c.preStarted = true
	handlers := make([]callbackHandler, 0, len(c.topics))
	for _, topic := range c.topics {
		handlers = append(handlers, c.handlers[topic])
	}
	c.mu.Unlock()
	for _, handler := range handlers {
		handler.preStart()
	}
}

// Sleeps the given seconds unless the context ends first
func sleepSeconds(ctx context.Context, seconds float64) error {
	timer := time.NewTimer(time.Duration(seconds * float64(time.Second)))
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Opens a gateway connection and serves its frames, reconnecting forever, until the context ends
func (c *streamClient) start(ctx context.Context) error {
	c.preStart()
	for {
		err := c.serveConnection(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var closed *websocketutil.CloseError
		switch {
		case err == nil:
		case errors.Is(err, errOpenConnectionFailed):
			c.log().Error("open connection failed")
			if err := sleepSeconds(ctx, 10); err != nil {
				return err
			}
		case errors.As(err, &closed):
			c.log().Error(fmt.Sprintf("[start] network exception, error=%s", closed.Error()))
			if err := sleepSeconds(ctx, 10); err != nil {
				return err
			}
		default:
			if err := sleepSeconds(ctx, 3); err != nil {
				return err
			}
			c.log().Error("unknown exception "+err.Error(), "error", err)
		}
	}
}

// Marker of an open connection request the gateway refused or that failed in transit
var errOpenConnectionFailed = errors.New("open connection failed")

// One gateway connection from the open request to the close of its WebSocket
func (c *streamClient) serveConnection(ctx context.Context) error {
	connection, err := c.openConnection(ctx)
	if err != nil {
		return err
	}
	if connection == nil {
		return errOpenConnectionFailed
	}
	c.log().Info(fmt.Sprintf("endpoint is %s", anyutil.String(connection)))
	endpoint, ok := connection.Get("endpoint")
	if !ok {
		return missingKey("connection", "endpoint")
	}
	ticket, ok := connection.Get("ticket")
	if !ok {
		return missingKey("connection", "ticket")
	}
	ticketText, ok := ticket.(string)
	if !ok {
		return kindError("connection ticket", "a string", ticket)
	}
	uri := anyutil.String(endpoint) + "?ticket=" + url.QueryEscape(ticketText)
	ws, err := websocketutil.Connect(ctx, uri, websocketutil.Options{OpenTimeout: 10 * time.Second, MaxMessageSize: 1 << 20,
		Compression: true, Proxy: httputil.Proxy})
	if err != nil {
		return err
	}
	defer ws.Close()
	c.mu.Lock()
	c.websocket = ws
	c.mu.Unlock()
	kctx, stopKeepalive := context.WithCancel(ctx)
	defer stopKeepalive()
	go c.keepalive(kctx, ws, 60)
	for {
		msg, err := ws.Receive(ctx)
		if err != nil {
			if websocketutil.IsCleanClose(err) {
				return nil
			}
			return err
		}
		parsed, err := jsonutil.Decode(msg.Data)
		if err != nil {
			return err
		}
		go c.backgroundTask(ctx, parsed, ws)
	}
}

// Pings the socket at the interval until it closes
func (c *streamClient) keepalive(ctx context.Context, ws *websocketutil.Conn, pingInterval float64) {
	for {
		if sleepSeconds(ctx, pingInterval) != nil {
			return
		}
		if ws.Ping() != nil {
			return
		}
	}
}

// Routes one frame and closes the socket when the gateway asked to disconnect, a failure logged
func (c *streamClient) backgroundTask(ctx context.Context, jsonMessage any, ws *websocketutil.Conn) {
	result, err := c.routeMessage(ctx, jsonMessage, ws)
	if err != nil {
		c.log().Error(fmt.Sprintf("error processing message: %s", err.Error()))
		return
	}
	if result == streamTagDisconnect {
		ws.Close()
	}
}

// Dispatches a system, event or callback frame to its handler and sends the acknowledgement back
func (c *streamClient) routeMessage(ctx context.Context, jsonMessage any, ws *websocketutil.Conn) (string, error) {
	frame, ok := jsonMessage.(*jsonutil.Object)
	if !ok {
		return "", kindError("frame", "an object", jsonMessage)
	}
	result := ""
	msgType := frame.ValueOr("type", "")
	var ack *ackMessage
	switch {
	case msgType == "SYSTEM":
		msg, err := parseStreamMessage(frame)
		if err != nil {
			return "", err
		}
		ack = &ackMessage{code: ackStatusOK, messageID: msg.headers.messageID, message: "OK", data: msg.data}
		if msg.headers.topic == streamTagDisconnect {
			result = streamTagDisconnect
			c.log().Info(fmt.Sprintf("received disconnect topic=%s, message=%s", anyutil.String(msg.headers.topic), anyutil.String(frame)))
		} else {
			c.log().Warn(fmt.Sprintf("unknown message topic, topic=%s, message=%s", anyutil.String(msg.headers.topic), anyutil.String(frame)))
		}
	case msgType == "EVENT":
		msg, err := parseStreamMessage(frame)
		if err != nil {
			return "", err
		}
		ack = &ackMessage{code: ackStatusNotImplement, messageID: msg.headers.messageID, message: "not implement", data: msg.data}
	case msgType == "CALLBACK":
		msg, err := parseStreamMessage(frame)
		if err != nil {
			return "", err
		}
		topic, _ := msg.headers.topic.(string)
		c.mu.Lock()
		handler := c.handlers[topic]
		c.mu.Unlock()
		if handler != nil {
			code, response := handler.process(ctx, msg)
			ack = &ackMessage{code: code, messageID: msg.headers.messageID, data: jsonutil.ObjectOf("response", response)}
		} else {
			c.log().Warn(fmt.Sprintf("unknown callback message topic, topic=%s, message=%s", anyutil.String(msg.headers.topic), anyutil.String(frame)))
		}
	default:
		c.log().Warn(fmt.Sprintf("unknown message, content=%s", anyutil.String(frame)))
	}
	if ack != nil {
		wire, err := ack.toDict()
		if err != nil {
			return "", err
		}
		encoded, err := jsonutil.Encode(wire)
		if err != nil {
			return "", err
		}
		if err := ws.SendText(encoded); err != nil {
			return "", err
		}
	}
	return result, nil
}

// Local address the host reaches the internet from, empty when it cannot be found
func hostIP() string {
	conn, err := net.Dial("udp4", "8.8.8.8:80")
	if err != nil {
		return ""
	}
	defer conn.Close()
	if addr, ok := conn.LocalAddr().(*net.UDPAddr); ok {
		return addr.IP.String()
	}
	return ""
}

// One JSON POST without a time limit, the response text kept for the failure log, a 4xx or 5xx status as the error
func sdkPost(ctx context.Context, target string, headers map[string]string, body string) (*httputil.Response, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(body))
	if err != nil {
		return nil, "", err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	client := &http.Client{Transport: httputil.TrustEnvCA(nil), CheckRedirect: httputil.RedirectPolicy(true)}
	resp, err := httputil.Do(client, req, httputil.Timeout{})
	if err != nil {
		return nil, "", err
	}
	text := string(resp.Content)
	if resp.StatusCode >= 400 {
		return resp, text, resp.CheckStatus()
	}
	return resp, text, nil
}

// Asks the gateway for a WebSocket endpoint and ticket for the registered topics, nil when the request fails
func (c *streamClient) openConnection(ctx context.Context) (*jsonutil.Object, error) {
	target := openAPIEndpoint(ctx) + "/v1.0/gateway/connections/open"
	c.log().Info("open connection, url=" + target)
	headers := map[string]string{"Content-Type": contentTypeJSON, "Accept": contentTypeJSON,
		"User-Agent": fmt.Sprintf("DingTalkStream/1.0 SDK/%s Go/%s (+https://github.com/open-dingtalk/dingtalk-stream-sdk-python)",
			streamSDKVersion, runtime.Version())}
	c.mu.Lock()
	topics := make([]any, 0, len(c.topics))
	for _, topic := range c.topics {
		topics = append(topics, jsonutil.ObjectOf("type", "CALLBACK", "topic", topic))
	}
	c.mu.Unlock()
	body, err := jsonutil.Encode(jsonutil.ObjectOf("clientId", c.clientID, "clientSecret", c.clientSecret, "subscriptions", topics,
		"ua", "dingtalk-sdk-python/v"+streamSDKVersion+"-union", "localIp", hostIP()))
	if err != nil {
		return nil, err
	}
	resp, text, err := sdkPost(ctx, target, headers, body)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		c.log().Error(fmt.Sprintf("open connection failed, error=%s, response.text=%s", err.Error(), text))
		return nil, nil
	}
	parsed, err := resp.JSON()
	if err != nil {
		return nil, err
	}
	connection, ok := parsed.(*jsonutil.Object)
	if !ok {
		if !anyutil.IsTrue(parsed) {
			return nil, nil
		}
		return nil, kindError("connection", "an object", parsed)
	}
	if connection.Len() == 0 {
		return nil, nil
	}
	return connection, nil
}

// App access token cached until five minutes before it expires, nil when the token request fails
func (c *streamClient) getAccessToken(ctx context.Context) (any, error) {
	now := time.Now().Unix()
	c.mu.Lock()
	cached := c.accessToken
	c.mu.Unlock()
	if cached != nil && cached.Len() > 0 {
		expire, err := anyutil.ToInt(cached.Value("expireTime"))
		if err != nil {
			return nil, err
		}
		if now < expire {
			return cached.Value("accessToken"), nil
		}
	}
	values, err := jsonutil.Encode(jsonutil.ObjectOf("appKey", c.clientID, "appSecret", c.clientSecret))
	if err != nil {
		return nil, err
	}
	target := openAPIEndpoint(ctx) + "/v1.0/oauth2/accessToken"
	resp, text, err := sdkPost(ctx, target, map[string]string{"Content-Type": contentTypeJSON, "Accept": contentTypeJSON}, values)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		c.log().Error(fmt.Sprintf("get dingtalk access token failed, error=%s, response.text=%s", err.Error(), text))
		return nil, nil
	}
	parsed, err := resp.JSON()
	if err != nil {
		return nil, err
	}
	result, ok := parsed.(*jsonutil.Object)
	if !ok {
		return nil, kindError("access token answer", "an object", parsed)
	}
	expireIn, ok := result.Get("expireIn")
	if !ok {
		return nil, missingKey("access token answer", "expireIn")
	}
	seconds, err := anyutil.ToInt(expireIn)
	if err != nil {
		return nil, err
	}
	result.Set("expireTime", time.Now().Unix()+seconds-5*60)
	token, ok := result.Get("accessToken")
	if !ok {
		return nil, missingKey("access token answer", "accessToken")
	}
	c.mu.Lock()
	c.accessToken = result
	c.mu.Unlock()
	return token, nil
}

// Current gateway socket, nil before the first connection
func (c *streamClient) currentWebsocket() *websocketutil.Conn {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.websocket
}
