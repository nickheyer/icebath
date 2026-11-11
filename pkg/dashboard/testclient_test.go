package dashboard

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
)

// In process client of an app, the Starlette TestClient on its testserver base URL
type testClient struct {
	t      *testing.T
	app    *FastAPI
	scheme string
	server *Address
}

// Client on a base URL such as https://agent.example.test, its scheme, host header and server port
func newTestClientAt(t *testing.T, app *FastAPI, scheme, host string, port int) *testClient {
	return &testClient{t: t, app: app, scheme: scheme, server: &Address{host, port}}
}

// Response a test request collected
type testResponse struct {
	Status  int
	Raw     []Header
	Body    []byte
	AppErr  error
	Headers Headers
}

func newTestClient(t *testing.T, app *FastAPI) *testClient { return &testClient{t: t, app: app} }

// Scope the TestClient builds for a request target and extra headers
func testScope(typ, method, target string, headers []string) *Scope {
	path, query, _ := strings.Cut(target, "?")
	raw := []Header{{"host", "testserver"}, {"accept", "*/*"}, {"accept-encoding", "gzip, deflate"},
		{"connection", "keep-alive"}, {"user-agent", "testclient"}}
	for i := 0; i+1 < len(headers); i += 2 {
		name := strings.ToLower(headers[i])
		replaced := false
		for j := range raw {
			if raw[j][0] == name {
				raw[j][1], replaced = headers[i+1], true
			}
		}
		if !replaced {
			raw = append(raw, Header{name, headers[i+1]})
		}
	}
	return &Scope{Type: typ, HTTPVersion: "1.1", Method: method, Scheme: "http", Path: pyurl.Unquote(path), RawPath: path,
		QueryString: query, Headers: raw, Client: &Address{"testclient", 50000}, Server: &Address{"testserver", 80},
		Extensions: map[string]any{}, State: map[string]any{}}
}

// Sends one request with a body and header pairs, the app error kept on the response
func (c *testClient) request(method, target string, body []byte, headers ...string) *testResponse {
	c.t.Helper()
	scope := testScope("http", method, target, headers)
	if c.server != nil {
		scope.Scheme, scope.Server = c.scheme, c.server
		for j := range scope.Headers {
			if scope.Headers[j][0] == "host" && scope.Headers[j][1] == "testserver" {
				scope.Headers[j][1] = c.server.Host
			}
		}
	}
	sent := false
	var mu sync.Mutex
	resp := &testResponse{}
	done := make(chan struct{})
	receive := func(ctx context.Context) (Message, error) {
		mu.Lock()
		if !sent {
			sent = true
			mu.Unlock()
			return Message{Type: "http.request", Body: body}, nil
		}
		mu.Unlock()
		select {
		case <-done:
			return Message{Type: "http.disconnect"}, nil
		case <-ctx.Done():
			return Message{}, ctx.Err()
		}
	}
	send := func(_ context.Context, m Message) error {
		mu.Lock()
		defer mu.Unlock()
		switch m.Type {
		case "http.response.start":
			resp.Status, resp.Raw = m.Status, append([]Header(nil), m.Headers...)
		case "http.response.body":
			resp.Body = append(resp.Body, m.Body...)
		}
		return nil
	}
	resp.AppErr = c.app.ASGI(context.Background(), scope, receive, send)
	close(done)
	resp.Headers = headersOfRaw(&resp.Raw)
	return resp
}

func (c *testClient) get(target string, headers ...string) *testResponse {
	return c.request("GET", target, nil, headers...)
}

// Decoded JSON body
func (r *testResponse) JSON(t *testing.T) any {
	t.Helper()
	v, err := pyjson.LoadsBytes(r.Body)
	if err != nil {
		t.Fatalf("body %q: %v", r.Body, err)
	}
	return v
}

// Asserts status and exact body bytes
func (r *testResponse) expect(t *testing.T, status int, body string) {
	t.Helper()
	if r.Status != status || !bytes.Equal(r.Body, []byte(body)) {
		t.Fatalf("got %d %q (err %v), want %d %q", r.Status, r.Body, r.AppErr, status, body)
	}
}

// In process websocket session of the TestClient, the app running in its own goroutine
type testWebSocket struct {
	t        *testing.T
	toApp    chan Message
	fromApp  chan Message
	done     chan error
	appErr   error
	finished bool
}

// Opens a websocket to target, the close code when the app refuses before accepting
func (c *testClient) websocketConnect(target string, headers ...string) (*testWebSocket, *WebSocketDisconnect) {
	c.t.Helper()
	scope := testScope("websocket", "", target, headers)
	scope.Scheme = "ws"
	scope.Extensions = map[string]any{"websocket.http.response": map[string]any{}}
	if p := headersOfScope(scope).Get("sec-websocket-protocol"); p != "" {
		for _, v := range strings.Split(p, ",") {
			scope.Subprotocols = append(scope.Subprotocols, strings.TrimSpace(v))
		}
	}
	ws := &testWebSocket{t: c.t, toApp: make(chan Message, 64), fromApp: make(chan Message, 64), done: make(chan error, 1)}
	ws.toApp <- Message{Type: "websocket.connect"}
	go func() {
		err := c.app.ASGI(context.Background(), scope, func(ctx context.Context) (Message, error) {
			select {
			case m := <-ws.toApp:
				return m, nil
			case <-ctx.Done():
				return Message{}, ctx.Err()
			}
		}, func(_ context.Context, m Message) error {
			ws.fromApp <- m
			return nil
		})
		ws.done <- err
	}()
	first := ws.next()
	switch first.Type {
	case "websocket.accept":
		return ws, nil
	case "websocket.close":
		ws.finish()
		return nil, &WebSocketDisconnect{Code: first.Code, Reason: first.Reason}
	}
	c.t.Fatalf("unexpected first websocket message %q", first.Type)
	return nil, nil
}

// Next message the app sent, failing the test when the app ends without one
func (w *testWebSocket) next() Message {
	w.t.Helper()
	select {
	case m := <-w.fromApp:
		return m
	case err := <-w.done:
		w.finished, w.appErr = true, err
		select {
		case m := <-w.fromApp:
			return m
		default:
		}
		w.t.Fatalf("app ended without a message: %v", err)
	case <-time.After(5 * time.Second):
		w.t.Fatal("timed out waiting for the app")
	}
	return Message{}
}

// Waits for the app to end and keeps its error
func (w *testWebSocket) finish() error {
	if !w.finished {
		w.finished = true
		select {
		case w.appErr = <-w.done:
		case <-time.After(5 * time.Second):
			w.t.Fatal("app did not end")
		}
	}
	return w.appErr
}

func (w *testWebSocket) sendText(s string) { w.toApp <- Message{Type: "websocket.receive", Text: &s} }

// Next text frame, or the disconnect error when the app closes instead
func (w *testWebSocket) receiveText() (string, *WebSocketDisconnect) {
	w.t.Helper()
	m := w.next()
	if m.Type == "websocket.close" {
		return "", &WebSocketDisconnect{Code: m.Code, Reason: m.Reason}
	}
	if m.Text == nil {
		w.t.Fatalf("expected text, got %+v", m)
	}
	return *m.Text, nil
}

// Disconnects with a code and waits for the app to end, returning its error
func (w *testWebSocket) close(code int) error {
	w.toApp <- Message{Type: "websocket.disconnect", Code: code}
	return w.finish()
}
