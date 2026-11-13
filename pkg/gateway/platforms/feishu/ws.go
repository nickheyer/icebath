package feishu

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"reflect"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/gorilla/websocket"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"
)

// Keepalive ping cadence and pong wait the Python websockets client applies when the profile sets none
const (
	defaultKeepalivePingInterval = 20 * time.Second
	defaultKeepalivePingTimeout  = 20 * time.Second
	sdkOverrideRefresh           = time.Second
)

// Completion of one websocket run on the executor
type wsFuture struct {
	done chan struct{}
	err  error
}

// One lark websocket client with the adapter's reconnect, ping and keepalive overrides applied
type wsLink struct {
	adapter          *FeishuAdapter
	client           *larkws.Client
	ctx              context.Context
	cancel           context.CancelFunc
	reconnectOff     atomic.Bool
	lastInbound      atomic.Int64
	reconnectNonce   int
	reconnectSeconds int
	pingInterval     *int
	pingTimeout      *int
}

// Websocket client for the adapter's credentials and domain with the channel UA tag Feishu needs for group mentions
func newWSLink(a *FeishuAdapter, domain string, handler *dispatcher.EventDispatcher) *wsLink {
	loop := a.currentLoop()
	parent := context.Background()
	if loop != nil {
		parent = loop.ctx
	}
	ctx, cancel := context.WithCancel(parent)
	link := &wsLink{adapter: a, ctx: ctx, cancel: cancel, reconnectNonce: a.settings.WSReconnectNonce,
		reconnectSeconds: a.settings.WSReconnectInterval, pingInterval: a.settings.WSPingInterval, pingTimeout: a.settings.WSPingTimeout}
	dialer := *websocket.DefaultDialer
	dialer.NetDialContext = func(dctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := (&net.Dialer{}).DialContext(dctx, network, addr)
		if err != nil {
			return nil, err
		}
		return &activityConn{Conn: conn, link: link}, nil
	}
	link.client = larkws.NewClient(a.settings.AppID, a.settings.AppSecret,
		larkws.WithEventHandler(handler), larkws.WithDomain(domain), larkws.WithLogLevel(larkcore.LogLevelInfo),
		larkws.WithLogger(slogLarkLogger{}), larkws.WithSource("channel"), larkws.WithWebSocketDialer(&dialer),
		larkws.WithHttpClient(&endpointClient{inner: &http.Client{Timeout: 10 * time.Second}, link: link}),
		larkws.WithOnReady(link.linkUp), larkws.WithOnReconnected(link.linkUp), larkws.WithOnReconnecting(link.reconnecting))
	link.applyRuntimeOverrides()
	return link
}

// Net conn recording when inbound bytes last arrived, the liveness evidence the keepalive waits for
type activityConn struct {
	net.Conn
	link *wsLink
}

func (c *activityConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.link.lastInbound.Store(time.Now().UnixNano())
	}
	return n, err
}

// HTTP client for the endpoint bootstrap whose client config carries the adapter's overrides
type endpointClient struct {
	inner *http.Client
	link  *wsLink
}

func (c *endpointClient) Do(req *http.Request) (*http.Response, error) {
	resp, err := c.inner.Do(req)
	if err != nil || resp == nil || req.URL == nil || req.URL.Path != larkws.GenEndpointUri {
		return resp, err
	}
	body, rerr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if rerr != nil {
		return nil, rerr
	}
	var envelope map[string]json.RawMessage
	if json.Unmarshal(body, &envelope) == nil {
		var data map[string]json.RawMessage
		if raw, ok := envelope["data"]; ok && json.Unmarshal(raw, &data) == nil {
			conf := &larkws.ClientConfig{}
			rawConf, present := data["ClientConfig"]
			if present && json.Unmarshal(rawConf, conf) == nil {
				c.link.overrideConfig(conf)
			}
			if encoded, merr := json.Marshal(conf); present && merr == nil {
				data["ClientConfig"] = encoded
				if encodedData, derr := json.Marshal(data); derr == nil {
					envelope["data"] = encodedData
					if out, eerr := json.Marshal(envelope); eerr == nil {
						body = out
					}
				}
			}
		}
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	return resp, nil
}

// Server client config with the reconnect nonce, interval and a configured ping interval taking precedence
func (l *wsLink) overrideConfig(conf *larkws.ClientConfig) {
	conf.ReconnectNonce = l.reconnectNonce
	conf.ReconnectInterval = l.reconnectSeconds
	if l.pingInterval != nil {
		conf.PingInterval = *l.pingInterval
	}
}

// Unexported SDK client field, the Go reach of the private attributes hermes sets on the Python client
func sdkField(target reflect.Value, name string) reflect.Value {
	f := target.FieldByName(name)
	return reflect.NewAt(f.Type(), unsafe.Pointer(f.UnsafeAddr())).Elem()
}

// Writes the reconnect nonce, reconnect interval and a configured ping interval into the live SDK client
func (l *wsLink) applyRuntimeOverrides() {
	defer func() {
		if r := recover(); r != nil {
			slog.Debug("[Feishu] Failed to apply websocket runtime overrides")
		}
	}()
	c := reflect.ValueOf(l.client).Elem()
	mu := sdkField(c, "stateMu").Addr().Interface().(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()
	sdkField(c, "reconnectNonce").SetInt(int64(l.reconnectNonce))
	sdkField(c, "reconnectInterval").SetInt(int64(time.Duration(l.reconnectSeconds) * time.Second))
	if l.pingInterval != nil {
		sdkField(c, "pingInterval").SetInt(int64(time.Duration(*l.pingInterval) * time.Second))
	}
}

// Socket of the SDK's active connection, nil between connections
func (l *wsLink) activeSocket() *websocket.Conn {
	defer func() { _ = recover() }()
	c := reflect.ValueOf(l.client).Elem()
	mu := sdkField(c, "stateMu").Addr().Interface().(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()
	run := sdkField(c, "run")
	if run.IsNil() {
		return nil
	}
	conn := sdkField(run.Elem(), "conn")
	if conn.IsNil() {
		return nil
	}
	socket, _ := sdkField(conn.Elem(), "socket").Interface().(*websocket.Conn)
	return socket
}

// Hops the link up report onto the adapter so a rebuilt link re-stamps connected
func (l *wsLink) linkUp() {
	l.applyRuntimeOverrides()
	l.lastInbound.Store(time.Now().UnixNano())
	if loop := l.adapter.currentLoop(); loop != nil && !loop.closed() {
		go l.adapter.wsLinkUp(loop.ctx, l)
	}
}

// Stops the ladder when auto reconnect was turned off, otherwise publishes the retrying state
func (l *wsLink) reconnecting() {
	if l.reconnectOff.Load() {
		l.client.Close()
		return
	}
	l.applyRuntimeOverrides()
	if loop := l.adapter.currentLoop(); loop != nil && !loop.closed() {
		go l.adapter.wsLinkRetrying(loop.ctx, l)
	}
}

func (l *wsLink) disableAutoReconnect() { l.reconnectOff.Store(true) }

func (l *wsLink) alive() bool { return l.ctx.Err() == nil }

// Stops the SDK run, which sends the websocket close frame to Feishu
func (l *wsLink) sendClose(ctx context.Context) error { return l.client.CloseAndWait(ctx) }

// Cancels every goroutine of the link's run
func (l *wsLink) stop() { l.cancel() }

// Websocket protocol pings at the keepalive cadence, closing the socket when nothing answers in time
func (l *wsLink) keepalive() {
	interval, timeout := defaultKeepalivePingInterval, defaultKeepalivePingTimeout
	if l.pingInterval != nil {
		interval = time.Duration(*l.pingInterval) * time.Second
	}
	if l.pingTimeout != nil {
		timeout = time.Duration(*l.pingTimeout) * time.Second
	}
	for sleepCtx(l.ctx, interval) {
		socket := l.activeSocket()
		if socket == nil {
			continue
		}
		sent := time.Now().UnixNano()
		if err := socket.WriteControl(websocket.PingMessage, nil, time.Now().Add(timeout)); err != nil {
			continue
		}
		if !sleepCtx(l.ctx, timeout) {
			return
		}
		if l.lastInbound.Load() < sent && l.activeSocket() == socket {
			slog.Warn("[Feishu] websocket keepalive ping timeout; closing the link so the SDK reconnects")
			_ = socket.Close()
		}
	}
}

// Reapplies the overrides after any config the server pushes in a pong frame
func (l *wsLink) refreshOverrides() {
	for sleepCtx(l.ctx, sdkOverrideRefresh) {
		l.applyRuntimeOverrides()
	}
}

// Starts the link on an executor slot, its future completing when the SDK run ends
func (a *FeishuAdapter) submitWSRun(ctx context.Context, link *wsLink) (*wsFuture, error) {
	ex, err := a.getSDKExecutor()
	if err != nil {
		return nil, err
	}
	fut := &wsFuture{done: make(chan struct{})}
	go func() {
		defer close(fut.done)
		select {
		case ex.slots <- struct{}{}:
		case <-ex.ctx.Done():
			fut.err = context.Canceled
			return
		case <-ctx.Done():
			fut.err = ctx.Err()
			return
		}
		defer func() { <-ex.slots }()
		a.runOfficialFeishuWSClient(link)
	}()
	return fut, nil
}

// Runs the SDK client until its run ends, logging an unrecoverable exit while the adapter is up
func (a *FeishuAdapter) runOfficialFeishuWSClient(link *wsLink) {
	defer link.cancel()
	go link.keepalive()
	go link.refreshOverrides()
	err := link.client.Start(link.ctx)
	if err == nil || link.ctx.Err() != nil && !a.IsConnected() {
		if err != nil {
			slog.Debug("[Feishu] lark WS receive loop ended during disconnect: " + err.Error())
		}
		return
	}
	if a.IsConnected() {
		slog.Error("[Feishu] lark WS receive loop died; stopping the worker loop so the supervisor can rebuild: " + err.Error())
	} else {
		slog.Debug("[Feishu] lark WS receive loop ended during disconnect: " + err.Error())
	}
}
