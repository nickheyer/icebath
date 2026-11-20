package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pyhttpx"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Per phase timeouts, pool bounds and routing of one Bot API request path, the HTTPXRequest kwargs
type requestSettings struct {
	poolTimeout       float64
	connectTimeout    float64
	readTimeout       float64
	writeTimeout      float64
	mediaWriteTimeout float64
	pollTimeout       float64
	transport         TransportOptions
	fallbackIPs       []string
}

// Live HTTP client of a request path with the transport it owns
type ptbClient struct {
	transport InnerTransport
	http      *http.Client
	mu        sync.Mutex
	closed    bool
}

func (c *ptbClient) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

// Closes the client's transport once, the httpx AsyncClient aclose
func (c *ptbClient) aclose(context.Context) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.mu.Unlock()
	return c.transport.Close()
}

// Bot API request path with PTB error semantics, the HTTPXRequest one Bot routes calls through
type ptbRequest struct {
	settings requestSettings
	mu       sync.Mutex
	client   *ptbClient
	observer func(ctx context.Context, generation *int, statusCode int, payload []byte)
}

// Request path with a freshly built client
func newPTBRequest(ctx context.Context, settings requestSettings) (*ptbRequest, error) {
	r := &ptbRequest{settings: settings}
	client, err := r.buildClient(ctx)
	if err != nil {
		return nil, err
	}
	r.client = client
	return r, nil
}

// Fresh client over the fallback IP transport, or a proxied or direct transport
func (r *ptbRequest) buildClient(ctx context.Context) (*ptbClient, error) {
	var transport InnerTransport
	var err error
	if len(r.settings.fallbackIPs) > 0 && r.settings.transport.Proxy == nil {
		transport, err = NewTelegramFallbackTransport(ctx, r.settings.fallbackIPs, r.settings.transport)
	} else {
		transport, err = NewHTTPTransport(r.settings.transport)
	}
	if err != nil {
		return nil, err
	}
	return &ptbClient{transport: transport, http: &http.Client{Transport: transport}}, nil
}

func (r *ptbRequest) currentClient() *ptbClient {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.client
}

// Closes the connection pool, the HTTPXRequest shutdown
func (r *ptbRequest) Shutdown(ctx context.Context) error {
	client := r.currentClient()
	if client == nil || client.isClosed() {
		return nil
	}
	return client.aclose(ctx)
}

// Rebuilds the client when the current one is closed, the HTTPXRequest initialize
func (r *ptbRequest) Initialize(ctx context.Context) error {
	client := r.currentClient()
	if client != nil && !client.isClosed() {
		return nil
	}
	fresh, err := r.buildClient(ctx)
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.client = fresh
	r.mu.Unlock()
	return nil
}

// Swaps an open client for a fresh one, returning the old client's close for a detached cleanup
func (r *ptbRequest) RebuildClient() (func(ctx context.Context) error, bool, error) {
	old := r.currentClient()
	if old == nil || old.isClosed() {
		return nil, false, nil
	}
	fresh, err := r.buildClient(context.Background())
	if err != nil {
		return nil, false, err
	}
	r.mu.Lock()
	r.client = fresh
	r.mu.Unlock()
	return old.aclose, true, nil
}

type requestTimeoutsKey struct{}

type rawRetrieveKey struct{}

// Context whose Bot API request returns the raw response body, the PTB retrieve of a file download
func withRawRetrieve(ctx context.Context) context.Context {
	return context.WithValue(ctx, rawRetrieveKey{}, true)
}

// Per call read and write timeout overrides of one Bot API request
type requestTimeouts struct {
	read  *float64
	media bool
}

// Context whose Bot API request uses the given read timeout and the media write timeout when media is set
func withRequestTimeouts(ctx context.Context, read *float64, media bool) context.Context {
	return context.WithValue(ctx, requestTimeoutsKey{}, requestTimeouts{read: read, media: media})
}

// Bot API method name of a request URL, the last path segment
func apiMethod(req *http.Request) string {
	path := req.URL.Path
	return path[strings.LastIndex(path, "/")+1:]
}

// Sends one Bot API request, mapping transport failures and error responses to python-telegram-bot exceptions
func (r *ptbRequest) Do(req *http.Request) (*http.Response, error) {
	client := r.currentClient()
	if client == nil || client.isClosed() {
		inner := &runtimeError{msg: "Cannot send a request, as the client has been closed."}
		return nil, &TelegramError{Class: "NetworkError", Message: "Unknown error in HTTP implementation: " +
			pyrepr.ExceptionRepr(inner), Err: inner}
	}
	ctx := req.Context()
	timeout := pyhttpx.Timeout{Connect: seconds(r.settings.connectTimeout), Read: seconds(r.settings.readTimeout),
		Write: seconds(r.settings.writeTimeout), Pool: seconds(r.settings.poolTimeout)}
	if overrides, ok := ctx.Value(requestTimeoutsKey{}).(requestTimeouts); ok {
		if overrides.read != nil {
			timeout.Read = seconds(*overrides.read)
		}
		if overrides.media {
			timeout.Write = seconds(r.settings.mediaWriteTimeout)
		}
	}
	if apiMethod(req) == "getUpdates" {
		timeout.Read += seconds(r.settings.pollTimeout)
	}
	resp, err := pyhttpx.DoTimeout(client.http, req, timeout)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, ptbTransportError(err)
	}
	if r.observer != nil {
		r.observer(ctx, pollingGenerationFrom(ctx), resp.StatusCode, resp.Content)
	}
	if resp.StatusCode >= 200 && resp.StatusCode <= 299 {
		if _, raw := ctx.Value(rawRetrieveKey{}).(bool); !raw && !json.Valid(bytes.ToValidUTF8(resp.Content, []byte("\uFFFD"))) {
			return nil, newTelegramError("TelegramError", "Invalid server response", nil)
		}
		return &http.Response{StatusCode: resp.StatusCode, Header: resp.Header, Body: io.NopCloser(bytes.NewReader(resp.Content)),
			Request: req}, nil
	}
	return nil, ptbResponseError(resp.StatusCode, resp.Content)
}

// httpx exception class standing for a timed out phase
var timeoutPhaseClass = map[string]string{"connect": "ConnectTimeout", "read": "ReadTimeout", "write": "WriteTimeout", "pool": "PoolTimeout"}

// httpx timeout carrying its phase class name
type httpxTimeout struct{ class string }

func (e *httpxTimeout) Error() string      { return "" }
func (e *httpxTimeout) PyTypeName() string { return e.class }

const poolTimeoutMessage = "Pool timeout: All connections in the connection pool are occupied. Request was *not* sent to Telegram. Consider adjusting the connection pool size or the pool timeout."

// Transport failure as TimedOut for a timed out phase and NetworkError for anything else
func ptbTransportError(err error) error {
	var timeout *pyhttpx.TimeoutError
	if errors.As(err, &timeout) {
		cause := &httpxTimeout{class: timeoutPhaseClass[timeout.Phase]}
		if timeout.Phase == "pool" {
			return &TelegramError{Class: "TimedOut", Message: poolTimeoutMessage, Err: cause}
		}
		return &TelegramError{Class: "TimedOut", Message: "Timed out", Err: cause}
	}
	return &TelegramError{Class: "NetworkError", Message: fmt.Sprintf("httpx.%s: %s", className(err), err.Error()), Err: err}
}

// Error response mapped to the python-telegram-bot exception its status code and parameters select
func ptbResponseError(code int, payload []byte) error {
	data, err := pyjson.LoadsBytes(bytes.ToValidUTF8(payload, []byte("�")))
	obj, isObj := data.(*pyjson.Object)
	if err != nil {
		return newTelegramError("TelegramError", "Invalid server response", err)
	}
	var description any
	var parameters any
	if isObj {
		description, _ = obj.Get("description")
		parameters, _ = obj.Get("parameters")
	}
	message := "Unknown HTTPError"
	if pyTruthy(description) {
		message = pyrepr.Str(description)
	}
	if params, ok := parameters.(*pyjson.Object); ok && params.Len() > 0 {
		if migrate, _ := params.Get("migrate_to_chat_id"); pyTruthy(migrate) {
			chatID, _ := pyint.Of(migrate)
			te := newTelegramError("ChatMigrated", fmt.Sprintf("Group migrated to supergroup. New chat id: %s", pyrepr.Str(migrate)), nil)
			if chatID != nil {
				te.NewChatID = chatID.Int64()
			}
			return te
		}
		if retry, _ := params.Get("retry_after"); pyTruthy(retry) {
			wait, _ := pyint.Float(retry)
			te := newTelegramError("RetryAfter", fmt.Sprintf("Flood control exceeded. Retry in %s seconds", pyrepr.Str(retry)), nil)
			te.RetryAfter = wait
			return te
		}
		message += "\nThe server response contained unknown parameters: " + pyrepr.Repr(params)
	}
	switch code {
	case http.StatusForbidden:
		return newTelegramError("Forbidden", message, nil)
	case http.StatusNotFound, http.StatusUnauthorized:
		return newTelegramError("InvalidToken", message, nil)
	case http.StatusBadRequest:
		return newTelegramError("BadRequest", message, nil)
	case http.StatusConflict:
		return newTelegramError("Conflict", message, nil)
	case http.StatusBadGateway:
		text := "Bad Gateway"
		if pyTruthy(description) {
			text = pyrepr.Str(description)
		}
		return newTelegramError("NetworkError", text, nil)
	}
	return newTelegramError("NetworkError", fmt.Sprintf("%s (%d)", message, code), nil)
}

// The python-telegram-bot exception inside an SDK error, the error itself when none is wrapped
func ptbError(err error) error {
	if err == nil {
		return nil
	}
	var te *TelegramError
	if errors.As(err, &te) {
		return te
	}
	return err
}

// Bot client transport routing getUpdates through the polling request and every other call through the general one
type botHTTPClient struct {
	general *ptbRequest
	polling *ptbRequest
	apiBase string
}

func (c *botHTTPClient) Do(req *http.Request) (*http.Response, error) {
	if c.apiBase != "" {
		target, err := url.Parse(c.apiBase + "/" + apiMethod(req))
		if err != nil {
			return nil, err
		}
		req.URL, req.Host = target, target.Host
	}
	if apiMethod(req) == "getUpdates" {
		return c.polling.Do(req)
	}
	return c.general.Do(req)
}
