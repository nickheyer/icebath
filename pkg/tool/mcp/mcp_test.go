package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/tool"
)

const fakeEnv = "MCP_FAKE_SERVER"

func TestMain(m *testing.M) {
	if os.Getenv(fakeEnv) == "1" {
		serveStdio()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// JSON-RPC server used by both transports, answering tool calls and probing the client with a ping
type fake struct {
	pong chan struct{}
}

type writer func(msg any)

func (w writer) reply(id json.RawMessage, result any) {
	raw, _ := json.Marshal(result)
	w(rpcMessage{JSONRPC: "2.0", ID: id, Result: raw})
}

func (w writer) fail(id json.RawMessage, code int, msg string) {
	w(rpcMessage{JSONRPC: "2.0", ID: id, Error: &Error{Code: code, Message: msg}})
}

func page(cursor string) listResult {
	ro, no := true, false
	if cursor == "" {
		return listResult{NextCursor: "p2", Tools: []toolDef{
			{Name: "echo", Description: "Echoes", InputSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}}}`), Annotations: &struct {
				ReadOnlyHint    *bool `json:"readOnlyHint"`
				DestructiveHint *bool `json:"destructiveHint"`
				IdempotentHint  *bool `json:"idempotentHint"`
				OpenWorldHint   *bool `json:"openWorldHint"`
			}{ReadOnlyHint: &ro, DestructiveHint: &no, IdempotentHint: &ro, OpenWorldHint: &no}},
			{Name: "image", Title: "Picture"},
		}}
	}
	return listResult{Tools: []toolDef{{Name: "fail"}, {Name: "boom"}, {Name: "all"}, {Name: "weird name!"}, {Name: "structured"}, {Name: "bad"}}}
}

// Handles one client message, returning once every reply has been written
func (f *fake) handle(msg rpcMessage, w writer) {
	switch {
	case msg.Method == "" && len(msg.ID) > 0:
		select {
		case f.pong <- struct{}{}:
		default:
		}
		return
	case msg.Method == "initialize":
		w.reply(msg.ID, map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{"tools": map[string]any{"listChanged": true}}, "serverInfo": Info{Name: "fake", Version: "1"}, "instructions": "be nice"})
	case msg.Method == "ping":
		w.reply(msg.ID, map[string]any{})
	case msg.Method == "tools/list":
		var p struct{ Cursor string }
		json.Unmarshal(msg.Params, &p)
		w.reply(msg.ID, page(p.Cursor))
	case msg.Method == "tools/call":
		f.call(msg, w)
	case msg.Method == "explode":
		w(nil)
	case len(msg.ID) > 0:
		w.fail(msg.ID, -32601, "method not found")
	}
}

func (f *fake) call(msg rpcMessage, w writer) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	json.Unmarshal(msg.Params, &p)
	switch p.Name {
	case "echo":
		w(rpcMessage{JSONRPC: "2.0", ID: json.RawMessage(`"srv-1"`), Method: "ping"})
		select {
		case <-f.pong:
		case <-time.After(2 * time.Second):
			w.fail(msg.ID, -32000, "client never answered the ping")
			return
		}
		w.reply(msg.ID, callResult{Content: []content{{Type: "text", Text: "echo:" + string(p.Arguments)}}})
		w(rpcMessage{JSONRPC: "2.0", Method: "notifications/tools/list_changed"})
		w(rpcMessage{JSONRPC: "2.0", Method: "notifications/message", Params: json.RawMessage(`{"level":"info","logger":"fake","data":"hello"}`)})
	case "image":
		w.reply(msg.ID, callResult{Content: []content{{Type: "image", Data: "AQID", MimeType: "image/png"}}})
	case "fail":
		w.reply(msg.ID, callResult{IsError: true, Content: []content{{Type: "text", Text: "nope"}}})
	case "boom":
		w.fail(msg.ID, -32000, "boom")
	case "structured":
		w.reply(msg.ID, map[string]any{"content": []any{}, "structuredContent": map[string]any{"k": 1}})
	case "bad":
		w.reply(msg.ID, callResult{Content: []content{{Type: "video"}}})
	case "all":
		w.reply(msg.ID, map[string]any{"content": []any{
			map[string]any{"type": "text", "text": "t"},
			map[string]any{"type": "image", "data": "AQ==", "mimeType": "image/gif"},
			map[string]any{"type": "audio", "data": "QUJD", "mimeType": "audio/wav"},
			map[string]any{"type": "resource", "resource": map[string]any{"uri": "file:///a.txt", "mimeType": "text/plain", "text": "body"}},
			map[string]any{"type": "resource", "resource": map[string]any{"uri": "file:///b.png", "mimeType": "image/png", "blob": "AQID"}},
			map[string]any{"type": "resource", "resource": map[string]any{"uri": "file:///c.bin", "mimeType": "application/octet-stream", "blob": "AQID"}},
			map[string]any{"type": "resource_link", "uri": "https://x/y", "name": "Y"},
			map[string]any{"type": "resource_link", "uri": "https://x/z"},
		}})
	default:
		w.fail(msg.ID, -32602, "unknown tool "+p.Name)
	}
}

// Runs the fake over the standard streams of this process
func serveStdio() {
	var wmu sync.Mutex
	out := bufio.NewWriter(os.Stdout)
	f := &fake{pong: make(chan struct{}, 1)}
	w := writer(func(msg any) {
		wmu.Lock()
		defer wmu.Unlock()
		if msg == nil {
			out.WriteString("this is not json\n")
		} else {
			raw, _ := json.Marshal(msg)
			out.Write(append(raw, '\n'))
		}
		out.Flush()
	})
	fmt.Fprintln(os.Stderr, "fake server up")
	br := bufio.NewReader(os.Stdin)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			var msg rpcMessage
			if json.Unmarshal(line, &msg) == nil {
				go f.handle(msg, w)
			}
		}
		if err != nil {
			return
		}
	}
}

// Serves the fake over streamable HTTP, answering echo with an event stream and everything else with JSON
type httpFake struct {
	fake
	versions []string
	deleted  atomic.Bool
	posts    atomic.Int32
}

func newHTTPFake(t *testing.T) (*httpFake, *httptest.Server) {
	t.Helper()
	h := &httpFake{fake: fake{pong: make(chan struct{}, 1)}}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return h, srv
}

func (h *httpFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	case http.MethodDelete:
		h.deleted.Store(true)
		return
	}
	h.posts.Add(1)
	body, _ := io.ReadAll(r.Body)
	var msg rpcMessage
	if err := json.Unmarshal(body, &msg); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if msg.Method != "initialize" && r.Header.Get("Mcp-Session-Id") != "sess-1" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if v := r.Header.Get("MCP-Protocol-Version"); v != "" {
		h.versions = append(h.versions, v)
	}
	switch {
	case msg.Method == "" || len(msg.ID) == 0:
		h.handle(msg, func(any) {})
		w.WriteHeader(http.StatusAccepted)
		return
	case msg.Method == "explode":
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, "kaboom")
		return
	case msg.Method == "initialize":
		w.Header().Set("Mcp-Session-Id", "sess-1")
	}
	var replies []any
	stream := strings.Contains(string(msg.Params), `"echo"`)
	if stream {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		var wmu sync.Mutex
		h.handle(msg, func(m any) {
			wmu.Lock()
			defer wmu.Unlock()
			raw, _ := json.Marshal(m)
			fmt.Fprintf(w, "event: message\ndata: %s\n\n", raw)
			flusher.Flush()
		})
		return
	}
	h.handle(msg, func(m any) { replies = append(replies, m) })
	w.Header().Set("Content-Type", "application/json")
	if len(replies) == 1 {
		json.NewEncoder(w).Encode(replies[0])
		return
	}
	json.NewEncoder(w).Encode(replies)
}

func connectStdio(t *testing.T) *Client {
	t.Helper()
	c, err := Connect(context.Background(), "fake srv", Stdio{Command: os.Args[0], Args: []string{"-test.run=NONE"}, Env: []string{fakeEnv + "=1"}, Stderr: io.Discard}, Info{Name: "icebath", Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestStdioServer(t *testing.T) {
	c := connectStdio(t)
	exercise(t, c)
}

func TestHTTPServer(t *testing.T) {
	h, srv := newHTTPFake(t)
	c, err := Connect(context.Background(), "web", HTTP{URL: srv.URL, Header: http.Header{"Authorization": {"Bearer x"}}}, Info{Name: "icebath", Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	exercise(t, c)
	if _, err := c.Call(context.Background(), "explode", nil); err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("%v", err)
	}
	c.Close()
	if !h.deleted.Load() || len(h.versions) == 0 || h.versions[0] != "2025-03-26" {
		t.Fatalf("deleted %v versions %v", h.deleted.Load(), h.versions)
	}
}

// Runs the same checks against a connected fake, whichever transport carries it
func exercise(t *testing.T, c *Client) {
	t.Helper()
	ctx := context.Background()
	srv := c.Server()
	if srv.Name != "fake" || srv.Version != "1" || srv.ProtocolVersion != "2025-03-26" || srv.Instructions != "be nice" || !srv.Tools || !srv.ToolsChange {
		t.Fatalf("%+v", srv)
	}
	if err := c.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	changed := make(chan struct{}, 1)
	logged := make(chan string, 1)
	c.OnToolsChanged(func() { changed <- struct{}{} })
	c.OnLog(func(level, logger string, data json.RawMessage) { logged <- level + "/" + logger + "/" + string(data) })
	tools, err := c.Tools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var r tool.Registry
	if err := r.Add(tools...); err != nil {
		t.Fatal(err)
	}
	prefix := "mcp__" + strings.ReplaceAll(c.Name(), " ", "_") + "__"
	if names := r.Names(); strings.Join(names, ",") != prefix+"echo,"+prefix+"image,"+prefix+"fail,"+prefix+"boom,"+prefix+"all,"+prefix+"weird_name_,"+prefix+"structured,"+prefix+"bad" {
		t.Fatalf("%v", names)
	}
	echo, _ := r.Lookup(prefix + "echo")
	if d := echo.Definition(); d.Description != "Echoes" || !strings.Contains(string(d.Parameters), `"text"`) || tool.Describe(echo) != (tool.Annotations{ReadOnly: true, Idempotent: true}) || echo.(remote).RemoteName() != "echo" {
		t.Fatalf("%+v %+v", echo.Definition(), tool.Describe(echo))
	}
	image, _ := r.Lookup(prefix + "image")
	if d := image.Definition(); d.Description != "Picture" || string(d.Parameters) != `{"type":"object"}` || tool.Describe(image) != (tool.Annotations{Destructive: true, OpenWorld: true}) {
		t.Fatalf("%+v %+v", d, tool.Describe(image))
	}
	res := r.Run(ctx, protocol.ToolCall{ID: "1", Name: prefix + "echo", Args: json.RawMessage(`{"text":"hi"}`)})
	if res.IsError || res.Content[0].(protocol.Text).Text != `echo:{"text":"hi"}` {
		t.Fatalf("%+v", res)
	}
	select {
	case <-changed:
	case <-time.After(2 * time.Second):
		t.Fatal("tools changed callback never fired")
	}
	select {
	case l := <-logged:
		if l != `info/fake/"hello"` {
			t.Fatal(l)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("log callback never fired")
	}
	res = r.Run(ctx, protocol.ToolCall{ID: "2", Name: prefix + "image"})
	if res.IsError || res.Content[0].(protocol.Image).MediaType != "image/png" || string(res.Content[0].(protocol.Image).Data) != "\x01\x02\x03" {
		t.Fatalf("%+v", res)
	}
	if res = r.Run(ctx, protocol.ToolCall{ID: "3", Name: prefix + "fail"}); !res.IsError || res.Content[0].(protocol.Text).Text != "nope" {
		t.Fatalf("%+v", res)
	}
	if res = r.Run(ctx, protocol.ToolCall{ID: "4", Name: prefix + "boom"}); !res.IsError || !strings.Contains(res.Content[0].(protocol.Text).Text, "boom") {
		t.Fatalf("%+v", res)
	}
	if res = r.Run(ctx, protocol.ToolCall{ID: "5", Name: prefix + "structured"}); res.IsError || res.Content[0].(protocol.Text).Text != `{"k":1}` {
		t.Fatalf("%+v", res)
	}
	if res = r.Run(ctx, protocol.ToolCall{ID: "6", Name: prefix + "bad"}); !res.IsError || !strings.Contains(res.Content[0].(protocol.Text).Text, "unsupported type") {
		t.Fatalf("%+v", res)
	}
	res = r.Run(ctx, protocol.ToolCall{ID: "7", Name: prefix + "all"})
	if res.IsError || len(res.Content) != 8 {
		t.Fatalf("%+v", res)
	}
	texts := []string{}
	for _, p := range res.Content {
		switch v := p.(type) {
		case protocol.Text:
			texts = append(texts, v.Text)
		case protocol.Image:
			texts = append(texts, "image:"+v.MediaType)
		}
	}
	want := []string{"t", "image:image/gif", "[audio audio/wav base64]\nQUJD", "[file:///a.txt]\nbody", "image:image/png", "[file:///c.bin application/octet-stream base64]\nAQID", "[resource link] Y: https://x/y", "[resource link] https://x/z: https://x/z"}
	if strings.Join(texts, "|") != strings.Join(want, "|") {
		t.Fatalf("%q", texts)
	}
	if _, err := c.Call(ctx, "nothing", nil); err == nil || !strings.Contains(err.Error(), "method not found") {
		t.Fatalf("%v", err)
	}
	var rpcErr *Error
	if _, err := c.Call(ctx, "nothing", nil); !errors.As(err, &rpcErr) || rpcErr.Code != -32601 {
		t.Fatalf("%v", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := c.Call(cancelled, "ping", nil); err == nil {
		t.Fatal("cancelled call succeeded")
	}
}

func TestCloseFailsPendingCalls(t *testing.T) {
	c := connectStdio(t)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Call(context.Background(), "ping", nil); err == nil {
		t.Fatal("call after close succeeded")
	}
	if _, err := c.Tools(context.Background()); err == nil {
		t.Fatal("tools after close succeeded")
	}
}

type deadDialer struct{}

func (deadDialer) Dial(context.Context) (Transport, error) {
	return deadTransport{make(chan []byte)}, nil
}

type deadTransport struct{ ch chan []byte }

func (d deadTransport) Send(context.Context, []byte) error { close(d.ch); return nil }
func (d deadTransport) Receive() <-chan []byte             { return d.ch }
func (deadTransport) Close() error                         { return nil }

func TestDialAndHandshakeFailures(t *testing.T) {
	ctx := context.Background()
	if _, err := Connect(ctx, "x", Stdio{}, Info{}); err == nil {
		t.Fatal("empty command accepted")
	}
	if _, err := Connect(ctx, "x", Stdio{Command: "/no/such/server"}, Info{}); err == nil {
		t.Fatal("missing command accepted")
	}
	if _, err := Connect(ctx, "x", HTTP{}, Info{}); err == nil {
		t.Fatal("empty url accepted")
	}
	if _, err := Connect(ctx, "x", deadDialer{}, Info{}); err == nil || !strings.Contains(err.Error(), "transport closed") {
		t.Fatalf("%v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, "hello")
	}))
	defer srv.Close()
	if _, err := Connect(ctx, "x", HTTP{URL: srv.URL}, Info{}); err == nil || !strings.Contains(err.Error(), "content type") {
		t.Fatalf("%v", err)
	}
	notools := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var msg rpcMessage
		json.Unmarshal(body, &msg)
		if msg.Method != "initialize" {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2025-06-18","capabilities":{},"serverInfo":{"name":"n","version":"v"}}}`, msg.ID)
	}))
	defer notools.Close()
	c, err := Connect(ctx, "x", HTTP{URL: notools.URL}, Info{})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Tools(ctx); err == nil || !strings.Contains(err.Error(), "offers no tools") {
		t.Fatalf("%v", err)
	}
}

func TestCloseDuringAnInFlightCall(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var msg rpcMessage
		json.Unmarshal(body, &msg)
		w.Header().Set("Content-Type", "application/json")
		if msg.Method == "initialize" {
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2025-06-18","capabilities":{},"serverInfo":{"name":"n","version":"v"}}}`, msg.ID)
			return
		}
		if len(msg.ID) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		<-release
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{}}`, msg.ID)
	}))
	defer srv.Close()
	c, err := Connect(context.Background(), "slow", HTTP{URL: srv.URL}, Info{})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := c.Call(context.Background(), "ping", nil)
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	c.Close()
	close(release)
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("call survived close")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("call hung after close")
	}
}

func TestToolNameAndSplit(t *testing.T) {
	if ToolName("my server", "do/thing") != "mcp__my_server__do_thing" {
		t.Fatal(ToolName("my server", "do/thing"))
	}
	if msgs := split([]byte(`[{"jsonrpc":"2.0","id":1,"result":{}},{"jsonrpc":"2.0","method":"x"}]`)); len(msgs) != 2 || msgs[1].Method != "x" {
		t.Fatalf("%+v", msgs)
	}
	if msgs := split([]byte(`junk`)); msgs != nil {
		t.Fatalf("%+v", msgs)
	}
	if _, err := decodeContent([]content{{Type: "image", Data: "!!"}}); err == nil {
		t.Fatal("bad base64 accepted")
	}
	if _, err := decodeContent([]content{{Type: "resource"}}); err == nil {
		t.Fatal("empty resource accepted")
	}
}
