package fakellm

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Model id every scripted completion reports
const ModelID = "fake-model"

// Text is a plain assistant answer, optionally with reasoning, streamed in chunks
type Text struct {
	Text             string
	Reasoning        string
	ChunkChars       int
	DelayPerChunk    time.Duration
	PromptTokens     *int
	CompletionTokens *int
	CachedTokens     int
	FinishReason     string
}

// Call is one tool call of an assistant turn, Raw sent verbatim as the arguments string when set
type Call struct {
	Name string
	Args map[string]any
	Raw  *string
}

// ToolCall is one assistant turn issuing one or more tool calls
type ToolCall struct {
	Name     string
	Args     map[string]any
	Raw      *string
	Parallel []Call
	Text     *string
}

// Error is an HTTP error response
type Error struct {
	Status     int
	Message    string
	RetryAfter *float64
}

// Hang accepts the request and never answers, the connection dropped after Seconds
type Hang struct{ Seconds float64 }

// DropMidStream streams the first AfterChars of Text then closes the socket without a finish chunk
type DropMidStream struct {
	Text       string
	AfterChars int
}

// StallMidStream opens the stream, sends the first AfterChars of Text, then stays silent for Seconds
type StallMidStream struct {
	Text       string
	AfterChars int
	Seconds    float64
}

// Raw sends an arbitrary body verbatim
type Raw struct {
	Body        string
	Status      int
	ContentType string
}

// Request is one recorded request with what the server answered
type Request struct {
	Path     string
	Kind     string
	Auth     string
	Headers  map[string]string
	Body     map[string]any
	Time     time.Time
	Response string
	Usage    map[string]any
}

// Responder picks the response of one request from its record
type Responder func(*Request) any

// Options tune a Server beyond its script
type Options struct {
	DefaultText    string
	Aux            Responder
	APIKeys        []string
	RecordGet      bool
	PromptTokensFn func(map[string]any) int
}

// Server is a scripted, recording loopback provider speaking the OpenAI Chat Completions wire format
type Server struct {
	mu        sync.Mutex
	script    []any
	responder Responder
	opts      Options
	requests  []*Request
	stop      chan struct{}
	stopOnce  sync.Once
	listener  net.Listener
	http      *http.Server
	toolSeq   int
}

// Starts a server answering main turns from script in order, then with the default text
func New(script []any, opts Options) (*Server, error) {
	return start(append([]any(nil), script...), nil, opts)
}

// Starts a server whose every main turn is answered by responder
func NewResponder(responder Responder, opts Options) (*Server, error) {
	return start(nil, responder, opts)
}

func start(script []any, responder Responder, opts Options) (*Server, error) {
	if opts.DefaultText == "" {
		opts.DefaultText = "ok"
	}
	if opts.Aux == nil {
		opts.Aux = func(*Request) any { return Text{Text: "Fake summary of the earlier conversation."} }
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	s := &Server{script: script, responder: responder, opts: opts, stop: make(chan struct{}), listener: listener}
	s.http = &http.Server{Handler: http.HandlerFunc(s.serve), ReadHeaderTimeout: 30 * time.Second}
	go s.http.Serve(listener)
	return s, nil
}

// Releases every hanging response and closes the listener and its connections
func (s *Server) Stop() {
	s.stopOnce.Do(func() {
		close(s.stop)
		s.http.Close()
	})
}

// Port the server listens on
func (s *Server) Port() int { return s.listener.Addr().(*net.TCPAddr).Port }

// Base URL a provider config points at
func (s *Server) BaseURL() string { return "http://127.0.0.1:" + strconv.Itoa(s.Port()) + "/v1" }

// Appends scripted responses
func (s *Server) Push(responses ...any) {
	s.mu.Lock()
	s.script = append(s.script, responses...)
	s.mu.Unlock()
}

// Discards scripted responses no request consumed and returns how many
func (s *Server) DropUnconsumed() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	dropped := len(s.script)
	s.script = nil
	return dropped
}

func (s *Server) nextMain(record *Request) any {
	if s.responder != nil {
		return s.responder(record)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.script) > 0 {
		next := s.script[0]
		s.script = s.script[1:]
		return next
	}
	return Text{Text: s.opts.DefaultText}
}

// Snapshot of every recorded request in arrival order
func (s *Server) Requests() []*Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*Request(nil), s.requests...)
}

func (s *Server) bodies(kind string) []map[string]any {
	out := []map[string]any{}
	for _, r := range s.Requests() {
		if r.Kind == kind {
			out = append(out, r.Body)
		}
	}
	return out
}

// Bodies of the requests that carried tools
func (s *Server) MainRequests() []map[string]any { return s.bodies("main") }

// Bodies of the requests without tools
func (s *Server) AuxRequests() []map[string]any { return s.bodies("aux") }

// Waits until n requests of a kind arrived, an error naming the shortfall after the timeout
func (s *Server) WaitForRequests(n int, timeout time.Duration, kind string) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if len(s.bodies(kind)) >= n {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("expected %d %s requests, saw %d total", n, kind, len(s.Requests()))
}

func (s *Server) nextToolCallID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.toolSeq++
	return "call_fake_" + strconv.Itoa(s.toolSeq)
}

func sendJSON(w http.ResponseWriter, status int, payload any, headers map[string]string) {
	body, _ := json.Marshal(payload)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	for k, v := range headers {
		w.Header().Set(k, v)
	}
	w.WriteHeader(status)
	w.Write(body)
}

func lowerHeaders(h http.Header) map[string]string {
	out := map[string]string{}
	for k, v := range h {
		out[strings.ToLower(k)] = strings.Join(v, ", ")
	}
	return out
}

func (s *Server) record(r *Request) {
	s.mu.Lock()
	s.requests = append(s.requests, r)
	s.mu.Unlock()
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	auth := r.Header.Get("Authorization")
	if r.Method == http.MethodGet {
		if s.opts.RecordGet {
			s.record(&Request{Path: r.URL.RequestURI(), Kind: "get", Auth: auth, Headers: lowerHeaders(r.Header), Time: time.Now()})
		}
		if strings.HasSuffix(strings.TrimRight(r.URL.RequestURI(), "/"), "/models") {
			sendJSON(w, 200, map[string]any{"object": "list", "data": []any{
				map[string]any{"id": ModelID, "object": "model", "context_length": 128000}}}, nil)
			return
		}
		sendJSON(w, 404, map[string]any{"error": map[string]any{"message": "not found"}}, nil)
		return
	}
	raw, _ := io.ReadAll(r.Body)
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		sendJSON(w, 400, map[string]any{"error": map[string]any{"message": "invalid json"}}, nil)
		return
	}
	kind := "aux"
	if tools, ok := body["tools"].([]any); ok && len(tools) > 0 {
		kind = "main"
	}
	record := &Request{Path: r.URL.RequestURI(), Kind: kind, Auth: auth, Headers: lowerHeaders(r.Header), Body: body, Time: time.Now()}
	s.record(record)
	if s.opts.APIKeys != nil {
		accepted := false
		for _, key := range s.opts.APIKeys {
			accepted = accepted || auth == "Bearer "+key
		}
		if !accepted {
			sendJSON(w, 401, map[string]any{"error": map[string]any{"message": "invalid api key", "type": "authentication_error"}}, nil)
			return
		}
	}
	if !strings.HasSuffix(strings.TrimRight(r.URL.RequestURI(), "/"), "/chat/completions") {
		sendJSON(w, 404, map[string]any{"error": map[string]any{"message": "unsupported path " + r.URL.RequestURI()}}, nil)
		return
	}
	var resp any
	if kind == "main" {
		resp = s.nextMain(record)
	} else {
		resp = s.opts.Aux(record)
	}
	s.mu.Lock()
	record.Response = strings.TrimPrefix(fmt.Sprintf("%T", resp), "fakellm.")
	s.mu.Unlock()
	var promptTokens *int
	if s.opts.PromptTokensFn != nil {
		n := s.opts.PromptTokensFn(body)
		promptTokens = &n
	}
	stream, _ := body["stream"].(bool)
	s.respond(w, resp, stream, promptTokens, record)
}

func chunk(delta map[string]any, finish any) map[string]any {
	return map[string]any{"id": "chatcmpl-fake", "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": ModelID,
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}}
}

func pieces(text string, size int) []string {
	size = max(1, size)
	runes := []rune(text)
	out := []string{}
	for i := 0; i < len(runes); i += size {
		out = append(out, string(runes[i:min(len(runes), i+size)]))
	}
	if len(out) == 0 {
		out = []string{""}
	}
	return out
}

func prefix(text string, n int) string {
	runes := []rune(text)
	return string(runes[:min(max(n, 0), len(runes))])
}

func startSSE(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "close")
	w.WriteHeader(200)
}

func sse(w http.ResponseWriter, payload any) {
	raw, _ := json.Marshal(payload)
	w.Write([]byte("data: " + string(raw) + "\n\n"))
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *Server) wait(seconds float64) {
	select {
	case <-s.stop:
	case <-time.After(time.Duration(seconds * float64(time.Second))):
	}
}

func dropConnection(w http.ResponseWriter) {
	if hj, ok := w.(http.Hijacker); ok {
		if conn, _, err := hj.Hijack(); err == nil {
			conn.Close()
		}
	}
}

func intOr(p *int, def int) int {
	if p != nil {
		return *p
	}
	return def
}

func (s *Server) messageFor(resp any, promptTokens *int) (map[string]any, string, map[string]any) {
	if text, ok := resp.(Text); ok {
		message := map[string]any{"role": "assistant", "content": text.Text}
		if text.Reasoning != "" {
			message["reasoning_content"] = text.Reasoning
		}
		pt := intOr(promptTokens, intOr(text.PromptTokens, 100))
		completion := intOr(text.CompletionTokens, 20)
		finish := text.FinishReason
		if finish == "" {
			finish = "stop"
		}
		return message, finish, map[string]any{"prompt_tokens": pt, "completion_tokens": completion, "total_tokens": pt + completion,
			"prompt_tokens_details": map[string]any{"cached_tokens": text.CachedTokens}}
	}
	call := resp.(ToolCall)
	calls := append([]Call{{Name: call.Name, Args: call.Args, Raw: call.Raw}}, call.Parallel...)
	toolCalls := []any{}
	for _, c := range calls {
		arguments := ""
		if c.Raw != nil {
			arguments = *c.Raw
		} else {
			args := c.Args
			if args == nil {
				args = map[string]any{}
			}
			raw, _ := json.Marshal(args)
			arguments = string(raw)
		}
		toolCalls = append(toolCalls, map[string]any{"id": s.nextToolCallID(), "type": "function",
			"function": map[string]any{"name": c.Name, "arguments": arguments}})
	}
	var content any
	if call.Text != nil {
		content = *call.Text
	}
	pt := intOr(promptTokens, 100)
	return map[string]any{"role": "assistant", "content": content, "tool_calls": toolCalls}, "tool_calls",
		map[string]any{"prompt_tokens": pt, "completion_tokens": 10, "total_tokens": pt + 10}
}

func (s *Server) respond(w http.ResponseWriter, resp any, stream bool, promptTokens *int, record *Request) {
	switch v := resp.(type) {
	case Error:
		headers := map[string]string{}
		if v.RetryAfter != nil {
			headers["Retry-After"] = strconv.FormatFloat(*v.RetryAfter, 'f', -1, 64)
		}
		status, message := v.Status, v.Message
		if status == 0 {
			status = 500
		}
		if message == "" {
			message = "scripted failure"
		}
		sendJSON(w, status, map[string]any{"error": map[string]any{"message": message, "type": "server_error"}}, headers)
		return
	case Hang:
		s.wait(v.Seconds)
		dropConnection(w)
		return
	case Raw:
		status, contentType := v.Status, v.ContentType
		if status == 0 {
			status = 200
		}
		if contentType == "" {
			contentType = "application/json"
		}
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Content-Length", strconv.Itoa(len(v.Body)))
		w.WriteHeader(status)
		w.Write([]byte(v.Body))
		return
	case StallMidStream:
		startSSE(w)
		sse(w, chunk(map[string]any{"role": "assistant", "content": ""}, nil))
		sse(w, chunk(map[string]any{"content": prefix(v.Text, v.AfterChars)}, nil))
		s.wait(v.Seconds)
		dropConnection(w)
		return
	case DropMidStream:
		startSSE(w)
		sse(w, chunk(map[string]any{"role": "assistant", "content": ""}, nil))
		sse(w, chunk(map[string]any{"content": prefix(v.Text, v.AfterChars)}, nil))
		dropConnection(w)
		return
	}
	message, finish, usage := s.messageFor(resp, promptTokens)
	s.mu.Lock()
	record.Usage = usage
	s.mu.Unlock()
	if !stream {
		sendJSON(w, 200, map[string]any{"id": "chatcmpl-fake", "object": "chat.completion", "created": time.Now().Unix(), "model": ModelID,
			"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}}, "usage": usage}, nil)
		return
	}
	startSSE(w)
	sse(w, chunk(map[string]any{"role": "assistant", "content": ""}, nil))
	if text, ok := resp.(Text); ok {
		size := text.ChunkChars
		if size == 0 {
			size = 8
		}
		if text.Reasoning != "" {
			for _, piece := range pieces(text.Reasoning, size) {
				sse(w, chunk(map[string]any{"reasoning_content": piece}, nil))
			}
		}
		for _, piece := range pieces(text.Text, size) {
			if text.DelayPerChunk > 0 {
				time.Sleep(text.DelayPerChunk)
			}
			sse(w, chunk(map[string]any{"content": piece}, nil))
		}
	} else {
		if content, _ := message["content"].(string); content != "" {
			sse(w, chunk(map[string]any{"content": content}, nil))
		}
		for i, raw := range message["tool_calls"].([]any) {
			tc := raw.(map[string]any)
			fn := tc["function"].(map[string]any)
			sse(w, chunk(map[string]any{"tool_calls": []any{map[string]any{"index": i, "id": tc["id"], "type": "function",
				"function": map[string]any{"name": fn["name"], "arguments": ""}}}}, nil))
			sse(w, chunk(map[string]any{"tool_calls": []any{map[string]any{"index": i,
				"function": map[string]any{"arguments": fn["arguments"]}}}}, nil))
		}
	}
	last := chunk(map[string]any{}, finish)
	last["usage"] = usage
	sse(w, last)
	w.Write([]byte("data: [DONE]\n\n"))
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// Writes a minimal config.yaml and .env routing the main model of a home to baseURL
func WriteHome(home, baseURL, apiKey, extraConfig string, offerVersion int) error {
	if apiKey == "" {
		apiKey = "sk-fake-e2e"
	}
	if err := os.MkdirAll(home, 0o755); err != nil {
		return err
	}
	config := "model:\n" +
		"  provider: custom\n" +
		"  base_url: " + baseURL + "\n" +
		"  default: " + ModelID + "\n" +
		"  context_length: 128000\n" +
		"agent:\n" +
		"  api_max_retries: 1\n" +
		"telemetry:\n" +
		"  shared_metrics:\n" +
		"    enabled: false\n" +
		"    send: false\n" +
		"    offer_version: " + strconv.Itoa(offerVersion) + "\n" +
		extraConfig
	if err := os.WriteFile(filepath.Join(home, "config.yaml"), []byte(config), 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(home, ".env"), []byte("OPENAI_API_KEY="+apiKey+"\n"), 0o644)
}
