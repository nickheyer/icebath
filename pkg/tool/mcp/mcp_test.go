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

const (
	fakeEnv     = "MCP_FAKE_SERVER"
	fakeVersion = "MCP_FAKE_VERSION"
)

func TestMain(m *testing.M) {
	if os.Getenv(fakeEnv) == "1" {
		serveStdio()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// JSON-RPC server used by both transports, answering tool calls and probing the client with requests
type fake struct {
	pong      chan struct{}
	replies   chan rpcMessage
	version   string
	mu        sync.Mutex
	caps      json.RawMessage
	cancelled int
	exit      func()
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
	return listResult{Tools: []toolDef{{Name: "fail"}, {Name: "boom"}, {Name: "all"}, {Name: "weird name!"}, {Name: "structured"}, {Name: "bad"}, {Name: "slow"}, {Name: "sample"}, {Name: "roots"}, {Name: "elicit"}, {Name: "caps"}, {Name: "cancels"}, {Name: "hang"}, {Name: "die"}, {Name: strings.Repeat("verylongtoolname", 6)}}}
}

// Handles one client message, returning once every reply has been written
func (f *fake) handle(msg rpcMessage, w writer) {
	switch {
	case msg.Method == "" && len(msg.ID) > 0:
		select {
		case f.replies <- msg:
		default:
		}
		return
	case msg.Method == "initialize":
		var p initializeParams
		json.Unmarshal(msg.Params, &p)
		raw, _ := json.Marshal(p.Capabilities)
		f.mu.Lock()
		f.caps = raw
		f.mu.Unlock()
		version := f.version
		if version == "" {
			version = "2025-03-26"
		}
		w.reply(msg.ID, map[string]any{"protocolVersion": version, "capabilities": map[string]any{
			"tools":     map[string]any{"listChanged": true},
			"resources": map[string]any{"listChanged": true, "subscribe": true},
			"prompts":   map[string]any{"listChanged": false},
			"logging":   map[string]any{},
		}, "serverInfo": Info{Name: "fake", Version: "1"}, "instructions": "be nice"})
	case msg.Method == "ping":
		w.reply(msg.ID, map[string]any{})
	case msg.Method == "notifications/cancelled":
		f.mu.Lock()
		f.cancelled++
		f.mu.Unlock()
	case msg.Method == "tools/list":
		var p struct{ Cursor string }
		json.Unmarshal(msg.Params, &p)
		w.reply(msg.ID, page(p.Cursor))
	case msg.Method == "tools/call":
		f.call(msg, w)
	case msg.Method == "resources/list":
		w.reply(msg.ID, map[string]any{"resources": []Resource{{URI: "file:///a.txt", Name: "a", MimeType: "text/plain"}, {URI: "file:///b.png", Name: "b"}}})
	case msg.Method == "resources/templates/list":
		w.reply(msg.ID, map[string]any{"resourceTemplates": []ResourceTemplate{{URITemplate: "file:///{path}", Name: "files"}}})
	case msg.Method == "resources/read":
		var p struct{ URI string }
		json.Unmarshal(msg.Params, &p)
		w.reply(msg.ID, map[string]any{"contents": []map[string]any{{"uri": p.URI, "mimeType": "text/plain", "text": "body of " + p.URI}}})
	case msg.Method == "resources/subscribe":
		w.reply(msg.ID, map[string]any{})
		w(rpcMessage{JSONRPC: "2.0", Method: "notifications/resources/updated", Params: json.RawMessage(`{"uri":"file:///a.txt"}`)})
	case msg.Method == "resources/unsubscribe", msg.Method == "logging/setLevel":
		w.reply(msg.ID, map[string]any{})
	case msg.Method == "prompts/list":
		w.reply(msg.ID, map[string]any{"prompts": []Prompt{{Name: "review", Description: "Reviews code", Arguments: []PromptArgument{{Name: "file", Required: true}}}}})
	case msg.Method == "prompts/get":
		var p struct {
			Name string            `json:"name"`
			Args map[string]string `json:"arguments"`
		}
		json.Unmarshal(msg.Params, &p)
		w.reply(msg.ID, map[string]any{"description": "d", "messages": []map[string]any{
			{"role": "user", "content": map[string]any{"type": "text", "text": "review " + p.Args["file"]}},
			{"role": "assistant", "content": map[string]any{"type": "text", "text": "ok"}},
		}})
	case msg.Method == "explode":
		w(nil)
	case len(msg.ID) > 0:
		w.fail(msg.ID, -32601, "method not found")
	}
}

// Sends a request to the client and waits for its reply
func (f *fake) ask(w writer, id, method string, params any) (rpcMessage, bool) {
	raw, _ := json.Marshal(params)
	w(rpcMessage{JSONRPC: "2.0", ID: json.RawMessage(`"` + id + `"`), Method: method, Params: raw})
	select {
	case r := <-f.replies:
		return r, true
	case <-time.After(2 * time.Second):
		return rpcMessage{}, false
	}
}

func (f *fake) call(msg rpcMessage, w writer) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
		Meta      struct {
			Token json.RawMessage `json:"progressToken"`
		} `json:"_meta"`
	}
	json.Unmarshal(msg.Params, &p)
	switch p.Name {
	case "echo":
		if _, ok := f.ask(w, "srv-1", "ping", nil); !ok {
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
	case "slow":
		for i := 1; i <= 2; i++ {
			raw, _ := json.Marshal(map[string]any{"progressToken": p.Meta.Token, "progress": i, "total": 2, "message": fmt.Sprintf("step %d", i)})
			w(rpcMessage{JSONRPC: "2.0", Method: "notifications/progress", Params: raw})
		}
		w.reply(msg.ID, callResult{Content: []content{{Type: "text", Text: "slow done"}}})
	case "sample":
		r, ok := f.ask(w, "srv-2", "sampling/createMessage", map[string]any{"messages": []any{}})
		if !ok {
			w.fail(msg.ID, -32000, "no sampling reply")
			return
		}
		if r.Error != nil {
			w.reply(msg.ID, callResult{Content: []content{{Type: "text", Text: "sampling refused: " + r.Error.Message}}})
			return
		}
		w.reply(msg.ID, callResult{Content: []content{{Type: "text", Text: "sampled:" + string(r.Result)}}})
	case "elicit":
		r, ok := f.ask(w, "srv-3", "elicitation/create", map[string]any{"message": "name?"})
		if !ok || r.Error != nil {
			w.fail(msg.ID, -32000, "no elicitation reply")
			return
		}
		w.reply(msg.ID, callResult{Content: []content{{Type: "text", Text: "elicited:" + string(r.Result)}}})
	case "roots":
		r, ok := f.ask(w, "srv-4", "roots/list", nil)
		if !ok {
			w.fail(msg.ID, -32000, "no roots reply")
			return
		}
		if r.Error != nil {
			w.reply(msg.ID, callResult{Content: []content{{Type: "text", Text: "roots refused"}}})
			return
		}
		w.reply(msg.ID, callResult{Content: []content{{Type: "text", Text: "roots:" + string(r.Result)}}})
	case "caps":
		f.mu.Lock()
		caps := f.caps
		f.mu.Unlock()
		w.reply(msg.ID, callResult{Content: []content{{Type: "text", Text: string(caps)}}})
	case "cancels":
		f.mu.Lock()
		n := f.cancelled
		f.mu.Unlock()
		w.reply(msg.ID, callResult{Content: []content{{Type: "text", Text: fmt.Sprint(n)}}})
	case "hang":
		time.Sleep(500 * time.Millisecond)
		w.reply(msg.ID, callResult{Content: []content{{Type: "text", Text: "hung"}}})
	case "die":
		if f.exit != nil {
			f.exit()
		}
		w.reply(msg.ID, callResult{Content: []content{{Type: "text", Text: "dying"}}})
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
	f := &fake{pong: make(chan struct{}, 1), replies: make(chan rpcMessage, 4), version: os.Getenv(fakeVersion), exit: func() { os.Exit(3) }}
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
	mu       sync.Mutex
	versions []string
	deleted  atomic.Bool
	posts    atomic.Int32
	session  atomic.Int32
	expired  atomic.Bool
	getOK    bool
	events   chan string
}

func newHTTPFake(t *testing.T) (*httpFake, *httptest.Server) {
	t.Helper()
	h := &httpFake{fake: fake{pong: make(chan struct{}, 1), replies: make(chan rpcMessage, 4)}, events: make(chan string, 4)}
	h.fake.exit = func() { h.expired.Store(true) }
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return h, srv
}

func (h *httpFake) sessionID() string { return fmt.Sprintf("sess-%d", h.session.Load()) }

func (h *httpFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		if !h.getOK {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		flusher.Flush()
		for {
			select {
			case ev := <-h.events:
				fmt.Fprintf(w, "id: 7\nevent: message\ndata: %s\n\n", ev)
				flusher.Flush()
			case <-r.Context().Done():
				return
			}
		}
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
	if msg.Method != "initialize" {
		if h.expired.Load() {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Header.Get("Mcp-Session-Id") != h.sessionID() {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
	}
	if v := r.Header.Get("MCP-Protocol-Version"); v != "" {
		h.mu.Lock()
		h.versions = append(h.versions, v)
		h.mu.Unlock()
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
		h.expired.Store(false)
		h.session.Add(1)
		w.Header().Set("Mcp-Session-Id", h.sessionID())
	}
	var replies []any
	stream := strings.Contains(string(msg.Params), `"echo"`) || strings.Contains(string(msg.Params), `"sample"`) || strings.Contains(string(msg.Params), `"roots"`) || strings.Contains(string(msg.Params), `"elicit"`) || strings.Contains(string(msg.Params), `"slow"`)
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

func stdioDialer(env ...string) Stdio {
	return Stdio{Command: os.Args[0], Args: []string{"-test.run=NONE"}, Env: append([]string{fakeEnv + "=1"}, env...), Stderr: io.Discard}
}

func testOptions() Options {
	return Options{
		Info:  Info{Name: "icebath", Version: "test"},
		Roots: []Root{{URI: "file:///work", Name: "work"}},
		Sampler: func(ctx context.Context, params json.RawMessage) (json.RawMessage, error) {
			return json.RawMessage(`{"role":"assistant","content":{"type":"text","text":"hi"},"model":"m"}`), nil
		},
		Elicitor: func(ctx context.Context, params json.RawMessage) (json.RawMessage, error) {
			return json.RawMessage(`{"action":"accept","content":{"name":"nick"}}`), nil
		},
	}
}

func connectStdio(t *testing.T) *Client {
	t.Helper()
	c, err := Connect(context.Background(), "fake srv", stdioDialer(), testOptions())
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
	c, err := Connect(context.Background(), "web", HTTP{URL: srv.URL, Header: http.Header{"Authorization": {"Bearer x"}}}, testOptions())
	if err != nil {
		t.Fatal(err)
	}
	exercise(t, c)
	if _, err := c.Call(context.Background(), "explode", nil); err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("%v", err)
	}
	c.Close()
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.deleted.Load() || len(h.versions) == 0 || h.versions[0] != "2025-03-26" {
		t.Fatalf("deleted %v versions %v", h.deleted.Load(), h.versions)
	}
}

// Runs the same checks against a connected fake, whichever transport carries it
func exercise(t *testing.T, c *Client) {
	t.Helper()
	ctx := context.Background()
	srv := c.Server()
	if srv.Name != "fake" || srv.Version != "1" || srv.ProtocolVersion != "2025-03-26" || srv.Instructions != "be nice" || !srv.Tools || !srv.ToolsChange || !srv.Resources || !srv.ResourcesSubscribe || !srv.Prompts || srv.PromptsChange || !srv.Logging {
		t.Fatalf("%+v", srv)
	}
	if err := c.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.SetLogLevel(ctx, "debug"); err != nil {
		t.Fatal(err)
	}
	changed := make(chan int, 1)
	logged := make(chan string, 1)
	c.OnToolsChanged(func() {
		tools, err := c.Tools(ctx)
		if err != nil {
			t.Error(err)
		}
		changed <- len(tools)
	})
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
	names := r.Names()
	if names[0] != prefix+"echo" || names[1] != prefix+"image" || names[5] != prefix+"weird_name_" || len(names) != 17 || len(names[16]) != 64 {
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
	if res.IsError || res.Content[0].(protocol.Text).Text != `echo:{"text":"hi"}` || res.Data.(CallData).Tool != "echo" {
		t.Fatalf("%+v", res)
	}
	select {
	case n := <-changed:
		if n != 17 {
			t.Fatalf("tools listed from inside the callback: %d", n)
		}
	case <-time.After(3 * time.Second):
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
	if res = r.Run(ctx, protocol.ToolCall{ID: "5", Name: prefix + "structured"}); res.IsError || res.Content[0].(protocol.Text).Text != `{"k":1}` || string(res.Data.(CallData).Structured) != `{"k":1}` {
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
	var mu sync.Mutex
	var progress []tool.Progress
	var notified []Progress
	c.OnProgress(func(p Progress) {
		mu.Lock()
		notified = append(notified, p)
		mu.Unlock()
	})
	pctx := tool.WithReporter(ctx, func(p tool.Progress) {
		mu.Lock()
		progress = append(progress, p)
		mu.Unlock()
	})
	if res = r.Run(pctx, protocol.ToolCall{ID: "8", Name: prefix + "slow"}); res.IsError || res.Content[0].(protocol.Text).Text != "slow done" {
		t.Fatalf("%+v", res)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n, m := len(progress), len(notified)
		mu.Unlock()
		if n == 2 && m == 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	if len(progress) != 2 || progress[1].Done != 2 || progress[1].Total != 2 || progress[1].Text != "step 2" || progress[1].CallID != "8" || len(notified) != 2 || notified[0].Message != "step 1" {
		t.Fatalf("progress %+v notified %+v", progress, notified)
	}
	mu.Unlock()
	if res = r.Run(ctx, protocol.ToolCall{ID: "9", Name: prefix + "sample"}); res.IsError || !strings.Contains(res.Content[0].(protocol.Text).Text, `sampled:{"role":"assistant"`) {
		t.Fatalf("%+v", res)
	}
	if res = r.Run(ctx, protocol.ToolCall{ID: "10", Name: prefix + "elicit"}); res.IsError || !strings.Contains(res.Content[0].(protocol.Text).Text, `elicited:{"action":"accept"`) {
		t.Fatalf("%+v", res)
	}
	if res = r.Run(ctx, protocol.ToolCall{ID: "11", Name: prefix + "roots"}); res.IsError || !strings.Contains(res.Content[0].(protocol.Text).Text, `file:///work`) {
		t.Fatalf("%+v", res)
	}
	if res = r.Run(ctx, protocol.ToolCall{ID: "12", Name: prefix + "caps"}); res.IsError || !strings.Contains(res.Content[0].(protocol.Text).Text, `"roots"`) || !strings.Contains(res.Content[0].(protocol.Text).Text, `"sampling"`) || !strings.Contains(res.Content[0].(protocol.Text).Text, `"elicitation"`) {
		t.Fatalf("declared capabilities %+v", res)
	}
	resources, err := c.ListResources(ctx)
	if err != nil || len(resources) != 2 || resources[0].URI != "file:///a.txt" || resources[0].MimeType != "text/plain" {
		t.Fatalf("%v %+v", err, resources)
	}
	templates, err := c.ListResourceTemplates(ctx)
	if err != nil || len(templates) != 1 || templates[0].URITemplate != "file:///{path}" {
		t.Fatalf("%v %+v", err, templates)
	}
	contents, err := c.ReadResource(ctx, "file:///a.txt")
	if err != nil || len(contents) != 1 || contents[0].Text != "body of file:///a.txt" {
		t.Fatalf("%v %+v", err, contents)
	}
	updated := make(chan string, 1)
	c.OnResourceUpdated(func(uri string) { updated <- uri })
	if err := c.Subscribe(ctx, "file:///a.txt"); err != nil {
		t.Fatal(err)
	}
	select {
	case uri := <-updated:
		if uri != "file:///a.txt" {
			t.Fatal(uri)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("resource update never arrived")
	}
	if err := c.Unsubscribe(ctx, "file:///a.txt"); err != nil {
		t.Fatal(err)
	}
	prompts, err := c.ListPrompts(ctx)
	if err != nil || len(prompts) != 1 || prompts[0].Name != "review" || !prompts[0].Arguments[0].Required {
		t.Fatalf("%v %+v", err, prompts)
	}
	prompt, err := c.GetPrompt(ctx, "review", map[string]string{"file": "x.go"})
	if err != nil || prompt.Description != "d" || len(prompt.Messages) != 2 || prompt.Messages[0].Content[0].(protocol.Text).Text != "review x.go" || prompt.Messages[1].Role != protocol.RoleAssistant {
		t.Fatalf("%v %+v", err, prompt)
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
	short, cancelShort := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancelShort()
	if res = r.Run(short, protocol.ToolCall{ID: "14", Name: prefix + "hang"}); !res.IsError || !strings.Contains(res.Content[0].(protocol.Text).Text, "deadline") {
		t.Fatalf("in flight call must fail on cancel: %+v", res)
	}
	deadline = time.Now().Add(2 * time.Second)
	for {
		res = r.Run(ctx, protocol.ToolCall{ID: "13", Name: prefix + "cancels"})
		if res.Content[0].(protocol.Text).Text == "1" || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if res.Content[0].(protocol.Text).Text != "1" {
		t.Fatalf("server never saw the cancellation notice: %+v", res)
	}
	select {
	case <-c.Done():
		t.Fatal("client must still be alive")
	default:
	}
	if c.Err() != nil {
		t.Fatal(c.Err())
	}
}

func TestVersionCheck(t *testing.T) {
	if _, err := Connect(context.Background(), "old", stdioDialer(fakeVersion+"=1999-01-01"), testOptions()); err == nil || !strings.Contains(err.Error(), "speaks protocol") {
		t.Fatalf("%v", err)
	}
	c, err := Connect(context.Background(), "new", stdioDialer(fakeVersion+"=2025-06-18"), testOptions())
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
}

func TestRequestsWithoutHandlersAreRefused(t *testing.T) {
	c, err := Connect(context.Background(), "bare", stdioDialer(), Options{Info: Info{Name: "x"}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	tools, _ := c.Tools(context.Background())
	var r tool.Registry
	r.Add(tools...)
	ctx := context.Background()
	if res := r.Run(ctx, protocol.ToolCall{ID: "1", Name: "mcp__bare__sample"}); !strings.Contains(res.Content[0].(protocol.Text).Text, "sampling refused") {
		t.Fatalf("%+v", res)
	}
	if res := r.Run(ctx, protocol.ToolCall{ID: "2", Name: "mcp__bare__roots"}); res.Content[0].(protocol.Text).Text != "roots refused" {
		t.Fatalf("%+v", res)
	}
	if res := r.Run(ctx, protocol.ToolCall{ID: "3", Name: "mcp__bare__caps"}); res.Content[0].(protocol.Text).Text != "{}" {
		t.Fatalf("%+v", res)
	}
	failing := Options{Info: Info{Name: "x"}, Sampler: func(ctx context.Context, params json.RawMessage) (json.RawMessage, error) {
		return nil, &Error{Code: -1, Message: "declined"}
	}}
	c2, err := Connect(context.Background(), "decl", stdioDialer(), failing)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	tools, _ = c2.Tools(ctx)
	var r2 tool.Registry
	r2.Add(tools...)
	if res := r2.Run(ctx, protocol.ToolCall{ID: "1", Name: "mcp__decl__sample"}); res.Content[0].(protocol.Text).Text != "sampling refused: declined" {
		t.Fatalf("%+v", res)
	}
}

func TestServerReconnectsOverStdio(t *testing.T) {
	var mu sync.Mutex
	var states []State
	var toolSets int
	s := &Server{Name: "sup", Dialer: stdioDialer(), Options: testOptions(), Backoff: 20 * time.Millisecond, MaxBackoff: 100 * time.Millisecond}
	s.OnState(func(st State, err error) {
		mu.Lock()
		states = append(states, st)
		mu.Unlock()
	})
	s.OnTools(func([]tool.Tool) {
		mu.Lock()
		toolSets++
		mu.Unlock()
	})
	if err := s.Open(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Open(context.Background()); err == nil {
		t.Fatal("double open accepted")
	}
	if st, err := s.State(); st != StateConnected || err != nil || s.Info().Name != "fake" || s.Client() == nil {
		t.Fatalf("%v %v", st, err)
	}
	tools := s.Tools()
	if len(tools) != 17 {
		t.Fatalf("%d tools", len(tools))
	}
	var r tool.Registry
	if err := r.Add(tools...); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if res := r.Run(ctx, protocol.ToolCall{ID: "1", Name: "mcp__sup__echo", Args: json.RawMessage(`{}`)}); res.IsError {
		t.Fatalf("%+v", res)
	}
	first := s.Client()
	r.Run(ctx, protocol.ToolCall{ID: "2", Name: "mcp__sup__die"})
	deadline := time.Now().Add(5 * time.Second)
	for {
		if c := s.Client(); c != nil && c != first {
			break
		}
		if time.Now().After(deadline) {
			st, err := s.State()
			t.Fatalf("server never reconnected: %v %v", st, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := first.Err(); err == nil || !strings.Contains(err.Error(), "exit status 3") {
		t.Fatalf("dead client must report the exit: %v", err)
	}
	if res := r.Run(ctx, protocol.ToolCall{ID: "3", Name: "mcp__sup__echo", Args: json.RawMessage(`{"after":"reconnect"}`)}); res.IsError || !strings.Contains(res.Content[0].(protocol.Text).Text, "reconnect") {
		t.Fatalf("tools must follow the new client: %+v", res)
	}
	mu.Lock()
	seen := strings.Join(statesOf(states), ",")
	sets := toolSets
	mu.Unlock()
	if !strings.Contains(seen, "connected,disconnected,connecting,connected") || sets != 1 {
		t.Fatalf("states %s tool sets %d", seen, sets)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.State(); st != StateClosed || s.Client() != nil {
		t.Fatalf("%v", st)
	}
	if _, err := s.Call(ctx, "ping", nil); err == nil {
		t.Fatal("call after close succeeded")
	}
	bad := &Server{Name: "bad", Dialer: Stdio{Command: "/no/such/server"}, Options: testOptions()}
	if err := bad.Open(ctx); err == nil {
		t.Fatal("open of a missing server succeeded")
	}
	if st, err := bad.State(); st != StateDisconnected || err == nil {
		t.Fatalf("%v %v", st, err)
	}
	if err := (&Server{Name: "nil"}).Open(ctx); err == nil {
		t.Fatal("open without dialer")
	}
}

func statesOf(states []State) []string {
	out := make([]string, len(states))
	for i, s := range states {
		out[i] = string(s)
	}
	return out
}

func TestServerReconnectsAfterHTTPSessionExpiry(t *testing.T) {
	h, srv := newHTTPFake(t)
	h.getOK = true
	s := &Server{Name: "web", Dialer: HTTP{URL: srv.URL}, Options: testOptions(), Backoff: 20 * time.Millisecond}
	if err := s.Open(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	logged := make(chan string, 1)
	s.OnLog(func(level, logger string, data json.RawMessage) { logged <- level })
	h.events <- `{"jsonrpc":"2.0","method":"notifications/message","params":{"level":"warning","logger":"srv","data":"x"}}`
	select {
	case l := <-logged:
		if l != "warning" {
			t.Fatal(l)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("server to client stream never delivered")
	}
	first := s.Client()
	var r tool.Registry
	r.Add(s.Tools()...)
	ctx := context.Background()
	r.Run(ctx, protocol.ToolCall{ID: "1", Name: "mcp__web__die"})
	if res := r.Run(ctx, protocol.ToolCall{ID: "2", Name: "mcp__web__echo"}); !res.IsError {
		t.Fatalf("call after expiry must fail: %+v", res)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if c := s.Client(); c != nil && c != first {
			break
		}
		if time.Now().After(deadline) {
			st, err := s.State()
			t.Fatalf("server never reconnected: %v %v", st, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !errors.Is(first.Err(), ErrSessionExpired) {
		t.Fatalf("%v", first.Err())
	}
	if res := r.Run(ctx, protocol.ToolCall{ID: "3", Name: "mcp__web__echo", Args: json.RawMessage(`{}`)}); res.IsError {
		t.Fatalf("%+v", res)
	}
	if h.session.Load() != 2 {
		t.Fatalf("a fresh session must be negotiated, got %d", h.session.Load())
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
	select {
	case <-c.Done():
	default:
		t.Fatal("done must be closed")
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
	if _, err := Connect(ctx, "x", Stdio{}, Options{}); err == nil {
		t.Fatal("empty command accepted")
	}
	if _, err := Connect(ctx, "x", Stdio{Command: "/no/such/server"}, Options{}); err == nil {
		t.Fatal("missing command accepted")
	}
	if _, err := Connect(ctx, "x", HTTP{}, Options{}); err == nil {
		t.Fatal("empty url accepted")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := Connect(cancelled, "x", stdioDialer(), Options{}); err == nil {
		t.Fatal("cancelled dial accepted")
	}
	if _, err := Connect(cancelled, "x", HTTP{URL: "http://127.0.0.1:1"}, Options{}); err == nil {
		t.Fatal("cancelled http dial accepted")
	}
	if _, err := Connect(ctx, "x", deadDialer{}, Options{}); err == nil || !strings.Contains(err.Error(), "transport closed") {
		t.Fatalf("%v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, "hello")
	}))
	defer srv.Close()
	if _, err := Connect(ctx, "x", HTTP{URL: srv.URL}, Options{}); err == nil || !strings.Contains(err.Error(), "content type") {
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
	c, err := Connect(ctx, "x", HTTP{URL: notools.URL}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for _, probe := range []func() error{
		func() error { _, err := c.Tools(ctx); return err },
		func() error { _, err := c.ListResources(ctx); return err },
		func() error { _, err := c.ListResourceTemplates(ctx); return err },
		func() error { _, err := c.ListPrompts(ctx); return err },
		func() error { return c.Subscribe(ctx, "x") },
	} {
		if err := probe(); err == nil || !strings.Contains(err.Error(), "offers no") && !strings.Contains(err.Error(), "does not support") {
			t.Fatalf("%v", err)
		}
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
	c, err := Connect(context.Background(), "slow", HTTP{URL: srv.URL}, Options{})
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
	long := ToolName("server", strings.Repeat("x", 100))
	if len(long) != 64 || !tool.ValidName(long) || long == ToolName("server", strings.Repeat("x", 101)) {
		t.Fatalf("%q", long)
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
	if _, err := decodeContent([]content{{Type: "resource", Resource: &struct {
		URI      string `json:"uri"`
		MimeType string `json:"mimeType"`
		Text     string `json:"text"`
		Blob     string `json:"blob"`
	}{URI: "x", MimeType: "image/png", Blob: "!!"}}}); err == nil {
		t.Fatal("bad resource blob accepted")
	}
}
