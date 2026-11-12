package provider

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Socket timeouts of one Gemini facade call in the httpx connect, read and write sense
type auxGeminiTimeouts struct {
	connect, read, write time.Duration
}

// httpx default of the native Gemini facade, a 20 minute read so long decode hops can think
var auxGeminiDefaultTimeouts = auxGeminiTimeouts{connect: 15 * time.Second, read: 1200 * time.Second, write: 30 * time.Second}

// OpenAI chat completions facade over the native Gemini REST API for auxiliary callers
type auxGeminiNativeClient struct {
	auxEffectiveStamp
	route      Route
	apiKey     string
	baseURL    string
	mu         sync.Mutex
	transports map[auxGeminiTimeouts]*http.Transport
	closed     atomic.Bool
}

// Facade for the key and base URL with extra client headers, an error when the key is blank
func newAuxGeminiNativeClient(apiKey, baseURL string, defaultHeaders map[string]string) (*auxGeminiNativeClient, error) {
	route := Route{Provider: "gemini", Mode: ModeGeminiNative, BaseURL: baseURL, APIKey: apiKey, Headers: copyStringMap(defaultHeaders)}
	native, err := newGeminiNativeClient(route)
	if err != nil {
		return nil, err
	}
	return &auxGeminiNativeClient{route: route, apiKey: apiKey, baseURL: native.(*geminiNativeClient).base,
		transports: map[auxGeminiTimeouts]*http.Transport{}}, nil
}

func (c *auxGeminiNativeClient) BaseURL() string { return c.baseURL }

func (c *auxGeminiNativeClient) APIKey() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.apiKey
}

// Replaces the key the next generateContent call sends as x-goog-api-key
func (c *auxGeminiNativeClient) SetAPIKey(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.apiKey, c.route.APIKey = key, key
}

var _ interface{ SetAPIKey(string) } = (*auxGeminiNativeClient)(nil)

// A complete client the auxiliary transport wrap never redispatches through a wire adapter
func (c *auxGeminiNativeClient) AuxDeclares(flag string) bool {
	return flag == "ICEBATH_SKIP_TRANSPORT_WRAP"
}

// Whether the facade was closed, read by the cached client close path
func (c *auxGeminiNativeClient) IsClosed() bool { return c.closed.Load() }

// Marks the facade closed and drops every pooled connection
func (c *auxGeminiNativeClient) Close() error {
	c.closed.Store(true)
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, tr := range c.transports {
		tr.CloseIdleConnections()
	}
	return nil
}

// Timeouts of the request timeout kwarg, every phase set to a number and the client default for None
func auxGeminiCallTimeouts(kwargs map[string]any) (auxGeminiTimeouts, error) {
	var seconds float64
	switch v := kwargs["timeout"].(type) {
	case nil:
		return auxGeminiDefaultTimeouts, nil
	case float64:
		seconds = v
	case float32:
		seconds = float64(v)
	case int:
		seconds = float64(v)
	case int64:
		seconds = float64(v)
	case time.Duration:
		seconds = v.Seconds()
	default:
		return auxGeminiTimeouts{}, fmt.Errorf("Timeout value must be a float, a Timeout or None, got %s", pyrepr.Repr(v))
	}
	d := time.Duration(seconds * float64(time.Second))
	return auxGeminiTimeouts{connect: d, read: d, write: d}, nil
}

// Socket whose every read and write carries its own httpx style deadline
type auxDeadlineConn struct {
	net.Conn
	read, write time.Duration
}

func (c *auxDeadlineConn) Read(p []byte) (int, error) {
	if c.read > 0 {
		_ = c.Conn.SetReadDeadline(time.Now().Add(c.read))
	}
	return c.Conn.Read(p)
}

func (c *auxDeadlineConn) Write(p []byte) (int, error) {
	if c.write > 0 {
		_ = c.Conn.SetWriteDeadline(time.Now().Add(c.write))
	}
	return c.Conn.Write(p)
}

// Pooled transport for one timeout set, reused across calls until the facade closes
func (c *auxGeminiNativeClient) transport(t auxGeminiTimeouts) *http.Transport {
	c.mu.Lock()
	defer c.mu.Unlock()
	if tr, ok := c.transports[t]; ok {
		return tr
	}
	tr := auxDeadlineTransport(t.connect, t.read, t.write)
	c.transports[t] = tr
	return tr
}

// Pooled transport whose dial, TLS handshake, reads and writes carry the httpx connect, read and write timeouts
func auxDeadlineTransport(connect, read, write time.Duration) *http.Transport {
	dialer := &net.Dialer{Timeout: connect, KeepAlive: 30 * time.Second}
	return auxTrackPoolConns(&http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			conn, err := dialer.DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			return &auxDeadlineConn{Conn: conn, read: read, write: write}, nil
		},
		TLSClientConfig: PlatformTLSConfig(), TLSHandshakeTimeout: connect, ForceAttemptHTTP2: true,
		MaxIdleConns: 100, IdleConnTimeout: 90 * time.Second, ExpectContinueTimeout: time.Second,
	})
}

// Messages kwarg as the list the Gemini request builder reads
func auxGeminiMessages(v any) []any {
	switch m := v.(type) {
	case []any:
		return m
	case []map[string]any:
		out := make([]any, len(m))
		for i, msg := range m {
			out[i] = msg
		}
		return out
	}
	return []any{}
}

// Native generateContent call for OpenAI kwargs, long decode hops joined, or a chunk stream when stream is set
func (c *auxGeminiNativeClient) CreateChatCompletion(ctx context.Context, kwargs map[string]any) (any, error) {
	timeouts, err := auxGeminiCallTimeouts(kwargs)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	route := c.route
	c.mu.Unlock()
	route.Transport = c.transport(timeouts)
	built, err := newGeminiNativeClient(route)
	if err != nil {
		return nil, err
	}
	native := built.(*geminiNativeClient)
	extra, _ := kwargs["extra_body"].(map[string]any)
	if extra == nil {
		extra = map[string]any{}
	}
	maxTokens := kwargs["max_tokens"]
	if budget, ok := geminiIntValue(extra["max_output_tokens"]); ok {
		current, _ := geminiPyNumber(maxTokens)
		if budget > current {
			maxTokens = budget
		}
	}
	thinking := extra["thinking_config"]
	if !pyTruthy(thinking) {
		thinking = extra["thinkingConfig"]
	}
	responseFormat := kwargs["response_format"]
	if !pyTruthy(responseFormat) {
		responseFormat = extra["response_format"]
	}
	model := "gemini-3.7-flash"
	if m, ok := kwargs["model"].(string); ok {
		model = m
	}
	request := buildGeminiRequest(geminiRequestInput{Messages: auxGeminiMessages(kwargs["messages"]), Tools: kwargs["tools"],
		ToolChoice: kwargs["tool_choice"], Temperature: kwargs["temperature"], MaxTokens: maxTokens, TopP: kwargs["top_p"], Stop: kwargs["stop"],
		ThinkingConfig: thinking, ResponseFormat: responseFormat, Model: model, ToolsAsJSONSchema: geminiAcceptsParametersJSONSchema(c.baseURL)})
	baseURL, auth, err := native.credentials(ctx)
	if err != nil {
		return nil, err
	}
	bare := bareGeminiModelID(model)
	p := &geminiPrepared{request: request, model: bare, url: baseURL + "/models/" + bare + ":", extra: extra, auth: auth, baseURL: baseURL}
	if stream, _ := kwargs["stream"].(bool); stream {
		return newAuxGeminiStream(ctx, native, p, timeouts.connect), nil
	}
	first, _, _, err := native.generate(ctx, p, request, timeouts.connect)
	if err != nil {
		return nil, err
	}
	hops := []map[string]any{first}
	maxHops := geminiMaxHops(request)
	for token := continuationToken(hops[len(hops)-1]); token != "" && len(hops) < maxHops; token = continuationToken(hops[len(hops)-1]) {
		next, _, _, err := native.generate(ctx, p, geminiWithToken(request, token), timeouts.connect)
		if err != nil {
			return nil, err
		}
		hops = append(hops, next)
	}
	if len(hops) > 1 {
		return translateGeminiResponse(mergeHopResponses(hops), bare), nil
	}
	return translateGeminiResponse(hops[0], bare), nil
}

// OpenAI chunk stream fed by the native streamGenerateContent producer
type auxGeminiStream struct {
	msgs    chan geminiStreamMsg
	cancel  context.CancelFunc
	pending []map[string]any
	done    bool
	err     error
}

// Starts the producer under its own cancel so closing the stream stops every hop
func newAuxGeminiStream(ctx context.Context, native *geminiNativeClient, p *geminiPrepared, connect time.Duration) *auxGeminiStream {
	sctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	s := &auxGeminiStream{msgs: make(chan geminiStreamMsg), cancel: cancel}
	go func() {
		defer close(s.msgs)
		native.produceStream(sctx, p, connect, s.msgs)
	}()
	return s
}

// Next OpenAI chunk, io.EOF once the turn ends, the producer error when a hop fails
func (s *auxGeminiStream) Next(ctx context.Context) (map[string]any, error) {
	for {
		if len(s.pending) > 0 {
			chunk := s.pending[0]
			s.pending = s.pending[1:]
			return chunk, nil
		}
		if s.err != nil {
			return nil, s.err
		}
		if s.done {
			return nil, io.EOF
		}
		select {
		case <-ctx.Done():
			s.cancel()
			return nil, ctx.Err()
		case msg, ok := <-s.msgs:
			switch {
			case !ok || msg.done:
				s.done = true
			case msg.err != nil:
				s.err = msg.err
			default:
				s.pending = append(s.pending, msg.chunks...)
			}
		}
	}
}

// Stops the producer and its open response
func (s *auxGeminiStream) Close() error {
	s.cancel()
	return nil
}

// Async wrapper over a native Gemini client whose leaf client is the sync client itself
type auxAsyncGeminiNativeClient struct {
	auxEffectiveStamp
	sync *auxGeminiNativeClient
}

// Wraps a sync native Gemini client for async callers
func newAuxAsyncGeminiNativeClient(sync *auxGeminiNativeClient) *auxAsyncGeminiNativeClient {
	return &auxAsyncGeminiNativeClient{sync: sync}
}

func (c *auxAsyncGeminiNativeClient) BaseURL() string { return c.sync.BaseURL() }
func (c *auxAsyncGeminiNativeClient) APIKey() string  { return c.sync.APIKey() }

// Runs the sync client body
func (c *auxAsyncGeminiNativeClient) CreateChatCompletion(ctx context.Context, kwargs map[string]any) (any, error) {
	return c.sync.CreateChatCompletion(ctx, kwargs)
}

// Closes the sync client
func (c *auxAsyncGeminiNativeClient) Close() error { return c.sync.Close() }

func (c *auxAsyncGeminiNativeClient) auxRealClient() AuxClient { return c.sync }
