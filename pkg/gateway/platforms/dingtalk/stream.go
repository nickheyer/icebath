package dingtalk

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyrequests"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
	"github.com/nickheyer/icebath/pkg/internal/pywebsockets"
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

// DingTalk OpenAPI base URL, the public endpoint unless DINGTALK_OPENAPI_ENDPOINT overrides it
var openAPIEndpoint = sync.OnceValue(func() string {
	if v, ok := os.LookupEnv("DINGTALK_OPENAPI_ENDPOINT"); ok {
		return v
	}
	return "https://api.dingtalk.com"
})

// Frame headers of a stream message, the names the SDK maps kept apart from the extension names
type streamHeaders struct {
	messageID any
	topic     any
}

// Headers of a frame, the ValueError int raises for a malformed time field
func parseStreamHeaders(value any) (streamHeaders, error) {
	var h streamHeaders
	obj, ok := value.(*pyjson.Object)
	if !ok {
		return h, attributeError(value, "items")
	}
	for _, name := range obj.Keys {
		field := obj.Vals[name]
		switch name {
		case "messageId":
			h.messageID = field
		case "topic":
			h.topic = field
		case "time", "eventBornTime":
			if _, err := pyint.Of(field); err != nil {
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
func parseStreamMessage(d *pyjson.Object) (*streamMessage, error) {
	msg := &streamMessage{data: pyjson.NewObject()}
	var data any = ""
	for _, name := range d.Keys {
		switch name {
		case "data":
			data = d.Vals[name]
		case "headers":
			headers, err := parseStreamHeaders(d.Vals[name])
			if err != nil {
				return nil, err
			}
			msg.headers = headers
		}
	}
	if pyrepr.Truthy(data) {
		text, ok := data.(string)
		if !ok {
			return nil, &gateway.PyError{Kind: "TypeError", Msg: fmt.Sprintf("the JSON object must be str, bytes or bytearray, not %s",
				pyrepr.TypeName(data))}
		}
		parsed, err := pyjson.Loads(text)
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
func (a *ackMessage) toDict() (*pyjson.Object, error) {
	headers := pyjson.ObjectOf("contentType", contentTypeJSON)
	if a.messageID != nil {
		headers.Set("messageId", a.messageID)
	}
	data, err := pyjson.Dumps(a.data)
	if err != nil {
		return nil, err
	}
	return pyjson.ObjectOf("code", a.code, "headers", headers, "message", a.message, "data", data), nil
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
	websocket   *pywebsockets.Conn
	preStarted  bool
	accessToken *pyjson.Object
}

// Client for the app credentials logging as the SDK client logger
func newStreamClient(clientID, clientSecret any) *streamClient {
	return &streamClient{clientID: clientID, clientSecret: clientSecret, handlers: map[string]callbackHandler{},
		logger: slog.Default().With("logger", "dingtalk_stream.client")}
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
		var closed *pywebsockets.ConnectionClosed
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
			c.log().Error("unknown exception "+pyrepr.ExceptionMessage(err), "error", err)
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
	c.log().Info(fmt.Sprintf("endpoint is %s", pyrepr.Str(connection)))
	endpoint, ok := connection.Get("endpoint")
	if !ok {
		return &gateway.PyError{Kind: "KeyError", Msg: "'endpoint'"}
	}
	ticket, ok := connection.Get("ticket")
	if !ok {
		return &gateway.PyError{Kind: "KeyError", Msg: "'ticket'"}
	}
	ticketText, ok := ticket.(string)
	if !ok {
		return &gateway.PyError{Kind: "TypeError", Msg: "quote_from_bytes() expected bytes"}
	}
	uri := pyrepr.Str(endpoint) + "?ticket=" + pyurl.QuotePlus(ticketText, "")
	ws, err := pywebsockets.Connect(ctx, uri, pywebsockets.ConnectOptions{})
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
		raw, err := ws.Recv(ctx)
		if err != nil {
			if pywebsockets.IsClosedOK(err) {
				return nil
			}
			return err
		}
		var parsed any
		switch frame := raw.(type) {
		case string:
			parsed, err = pyjson.Loads(frame)
		case []byte:
			parsed, err = pyjson.LoadsBytes(frame)
		}
		if err != nil {
			return err
		}
		go c.backgroundTask(ctx, parsed, ws)
	}
}

// Pings the socket at the interval until it closes
func (c *streamClient) keepalive(ctx context.Context, ws *pywebsockets.Conn, pingInterval float64) {
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
func (c *streamClient) backgroundTask(ctx context.Context, jsonMessage any, ws *pywebsockets.Conn) {
	result, err := c.routeMessage(ctx, jsonMessage, ws)
	if err != nil {
		c.log().Error(fmt.Sprintf("error processing message: %s", pyrepr.ExceptionMessage(err)))
		return
	}
	if result == streamTagDisconnect {
		ws.Close()
	}
}

// Dispatches a system, event or callback frame to its handler and sends the acknowledgement back
func (c *streamClient) routeMessage(ctx context.Context, jsonMessage any, ws *pywebsockets.Conn) (string, error) {
	frame, ok := jsonMessage.(*pyjson.Object)
	if !ok {
		return "", attributeError(jsonMessage, "get")
	}
	result := ""
	msgType := frame.GetOr("type", "")
	var ack *ackMessage
	switch {
	case pyrepr.Equal(msgType, "SYSTEM"):
		msg, err := parseStreamMessage(frame)
		if err != nil {
			return "", err
		}
		ack = &ackMessage{code: ackStatusOK, messageID: msg.headers.messageID, message: "OK", data: msg.data}
		if pyrepr.Equal(msg.headers.topic, streamTagDisconnect) {
			result = streamTagDisconnect
			c.log().Info(fmt.Sprintf("received disconnect topic=%s, message=%s", pyrepr.Str(msg.headers.topic), pyrepr.Str(frame)))
		} else {
			c.log().Warn(fmt.Sprintf("unknown message topic, topic=%s, message=%s", pyrepr.Str(msg.headers.topic), pyrepr.Str(frame)))
		}
	case pyrepr.Equal(msgType, "EVENT"):
		msg, err := parseStreamMessage(frame)
		if err != nil {
			return "", err
		}
		ack = &ackMessage{code: ackStatusNotImplement, messageID: msg.headers.messageID, message: "not implement", data: msg.data}
	case pyrepr.Equal(msgType, "CALLBACK"):
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
			ack = &ackMessage{code: code, messageID: msg.headers.messageID, data: pyjson.ObjectOf("response", response)}
		} else {
			c.log().Warn(fmt.Sprintf("unknown callback message topic, topic=%s, message=%s", pyrepr.Str(msg.headers.topic), pyrepr.Str(frame)))
		}
	default:
		c.log().Warn(fmt.Sprintf("unknown message, content=%s", pyrepr.Str(frame)))
	}
	if ack != nil {
		wire, err := ack.toDict()
		if err != nil {
			return "", err
		}
		encoded, err := pyjson.Dumps(wire)
		if err != nil {
			return "", err
		}
		if err := ws.Send(encoded); err != nil {
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

// One JSON POST the way the SDK sends it through requests, the response text kept for the failure log
func sdkPost(ctx context.Context, target string, headers map[string]string, body string) (*pyrequests.Response, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader([]byte(body)))
	if err != nil {
		return nil, "", err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := pyrequests.Send(ctx, http.DefaultTransport.(*http.Transport), pyrequests.Timeout{}, req)
	if err != nil {
		return nil, "", err
	}
	text := string(resp.Body)
	return resp, text, resp.RaiseForStatus(target)
}

// Asks the gateway for a WebSocket endpoint and ticket for the registered topics, nil when the request fails
func (c *streamClient) openConnection(ctx context.Context) (*pyjson.Object, error) {
	target := openAPIEndpoint() + "/v1.0/gateway/connections/open"
	c.log().Info("open connection, url=" + target)
	headers := map[string]string{"Content-Type": contentTypeJSON, "Accept": contentTypeJSON,
		"User-Agent": fmt.Sprintf("DingTalkStream/1.0 SDK/%s Go/%s (+https://github.com/open-dingtalk/dingtalk-stream-sdk-python)",
			streamSDKVersion, runtime.Version())}
	c.mu.Lock()
	topics := make([]any, 0, len(c.topics))
	for _, topic := range c.topics {
		topics = append(topics, pyjson.ObjectOf("type", "CALLBACK", "topic", topic))
	}
	c.mu.Unlock()
	body, err := pyjson.Dumps(pyjson.ObjectOf("clientId", c.clientID, "clientSecret", c.clientSecret, "subscriptions", topics,
		"ua", "dingtalk-sdk-python/v"+streamSDKVersion+"-union", "localIp", hostIP()))
	if err != nil {
		return nil, err
	}
	resp, text, err := sdkPost(ctx, target, headers, body)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		c.log().Error(fmt.Sprintf("open connection failed, error=%s, response.text=%s", pyrepr.ExceptionMessage(err), text))
		return nil, nil
	}
	parsed, err := pyjson.LoadsBytes(resp.Body)
	if err != nil {
		return nil, err
	}
	connection, ok := parsed.(*pyjson.Object)
	if !ok {
		if !pyrepr.Truthy(parsed) {
			return nil, nil
		}
		return nil, &gateway.PyError{Kind: "TypeError", Msg: fmt.Sprintf("'%s' object is not subscriptable", pyrepr.TypeName(parsed))}
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
		expire, err := pyint.Int64(cached.Vals["expireTime"])
		if err != nil {
			return nil, err
		}
		if now < expire {
			return cached.Vals["accessToken"], nil
		}
	}
	values, err := pyjson.Dumps(pyjson.ObjectOf("appKey", c.clientID, "appSecret", c.clientSecret))
	if err != nil {
		return nil, err
	}
	target := openAPIEndpoint() + "/v1.0/oauth2/accessToken"
	resp, text, err := sdkPost(ctx, target, map[string]string{"Content-Type": contentTypeJSON, "Accept": contentTypeJSON}, values)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		c.log().Error(fmt.Sprintf("get dingtalk access token failed, error=%s, response.text=%s", pyrepr.ExceptionMessage(err), text))
		return nil, nil
	}
	parsed, err := pyjson.LoadsBytes(resp.Body)
	if err != nil {
		return nil, err
	}
	result, ok := parsed.(*pyjson.Object)
	if !ok {
		return nil, &gateway.PyError{Kind: "TypeError", Msg: fmt.Sprintf("'%s' object does not support item assignment", pyrepr.TypeName(parsed))}
	}
	expireIn, ok := result.Get("expireIn")
	if !ok {
		return nil, &gateway.PyError{Kind: "KeyError", Msg: "'expireIn'"}
	}
	seconds, err := pyint.Int64(expireIn)
	if err != nil {
		return nil, err
	}
	result.Set("expireTime", time.Now().Unix()+seconds-5*60)
	token, ok := result.Get("accessToken")
	if !ok {
		return nil, &gateway.PyError{Kind: "KeyError", Msg: "'accessToken'"}
	}
	c.mu.Lock()
	c.accessToken = result
	c.mu.Unlock()
	return token, nil
}

// Current gateway socket, nil before the first connection
func (c *streamClient) currentWebsocket() *pywebsockets.Conn {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.websocket
}
