package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/provider"
)

// Answers a server's sampling/createMessage requests
type Sampler interface {
	Capabilities() map[string]any
	CreateMessage(ctx context.Context, params *CreateMessageParams) (any, *RPCError)
}

// Answers a server's elicitation/create requests
type Elicitor interface {
	Capabilities() map[string]any
	Elicit(ctx context.Context, params *ElicitParams) (*ElicitResult, *RPCError)
}

// Elicitor that follows the browser steps of URL mode elicitations it accepted until the server reports them finished
type URLTracker interface {
	// The server reported the step finished
	Complete(id string)
	// Closed once the step is reported finished, nil when the id is not waited on
	Done(id string) <-chan struct{}
	// Longest the user gets to finish a step
	Wait() time.Duration
}

type SessionOptions struct {
	// Server the session talks to, named in messages
	Name     string
	Sampler  Sampler
	Elicitor Elicitor
	// Called when the server says its tools changed
	OnToolsChanged func()
}

// One JSON-RPC client session over a transport
type Session struct {
	t        Transport
	opts     SessionOptions
	ctx      context.Context
	cancel   context.CancelFunc
	done     chan struct{}
	mu       sync.Mutex
	pending  map[string]chan *Message
	inflight map[string]context.CancelFunc
	progress map[string]func(ProgressParams)
	result   *InitializeResult
	version  string
	err      error
}

// Session reading the transport's messages from now on
func NewSession(t Transport, opts SessionOptions) *Session {
	ctx, cancel := context.WithCancel(context.Background())
	s := &Session{t: t, opts: opts, ctx: ctx, cancel: cancel, done: make(chan struct{}), pending: map[string]chan *Message{},
		inflight: map[string]context.CancelFunc{}, progress: map[string]func(ProgressParams){}}
	go s.read()
	return s
}

func (s *Session) Name() string { return s.opts.Name }

// Transport the session runs on
func (s *Session) Transport() Transport { return s.t }

// Handshake or discovery answer adopted, nil before negotiation
func (s *Session) InitializeResult() *InitializeResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.result
}

// Revision later requests are shaped for
func (s *Session) ProtocolVersion() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.version
}

// Closed once the transport ended and every pending request failed
func (s *Session) Done() <-chan struct{} { return s.done }

// Why the session ended, nil while it runs
func (s *Session) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

func (s *Session) read() {
	for msg := range s.t.Receive() {
		switch {
		case msg.IsResponse():
			s.mu.Lock()
			ch := s.pending[IDKey(msg.ID)]
			delete(s.pending, IDKey(msg.ID))
			s.mu.Unlock()
			if ch != nil {
				ch <- msg
			}
		case msg.IsRequest():
			ctx, cancel := context.WithCancel(s.ctx)
			s.mu.Lock()
			s.inflight[IDKey(msg.ID)] = cancel
			s.mu.Unlock()
			go s.answer(ctx, msg)
		case msg.IsNotification():
			s.notified(msg)
		}
	}
	closed := &RPCError{Code: CodeConnectionClosed, Message: "Connection closed"}
	s.mu.Lock()
	if err := s.t.Err(); err != nil {
		s.err = fmt.Errorf("%w: %v", closed, err)
	} else {
		s.err = closed
	}
	pending := s.pending
	s.pending = map[string]chan *Message{}
	s.mu.Unlock()
	for _, ch := range pending {
		close(ch)
	}
	s.cancel()
	close(s.done)
}

// Answers one request of the server on its own goroutine
func (s *Session) answer(ctx context.Context, msg *Message) {
	defer func() {
		s.mu.Lock()
		if cancel := s.inflight[IDKey(msg.ID)]; cancel != nil {
			cancel()
		}
		delete(s.inflight, IDKey(msg.ID))
		s.mu.Unlock()
	}()
	var reply *Message
	switch msg.Method {
	case MethodPing:
		reply, _ = NewResponse(msg.ID, map[string]any{})
	case MethodCreateMessage:
		reply = s.sample(ctx, msg)
	case MethodElicit:
		reply = s.elicit(ctx, msg)
	default:
		reply = NewErrorResponse(msg.ID, CodeMethodNotFound, "Method not found")
	}
	if ctx.Err() != nil {
		return
	}
	if err := s.t.Send(s.ctx, reply); err != nil {
		slog.Debug("MCP answer to the server failed", "server", s.opts.Name, "method", msg.Method, "error", err)
	}
}

func (s *Session) sample(ctx context.Context, msg *Message) *Message {
	if s.opts.Sampler == nil {
		return NewErrorResponse(msg.ID, CodeInvalidRequest, "Sampling not supported")
	}
	var params CreateMessageParams
	if json.Unmarshal(msg.Params, &params) != nil {
		return NewErrorResponse(msg.ID, CodeInvalidParams, "Invalid request parameters")
	}
	result, failure := s.opts.Sampler.CreateMessage(ctx, &params)
	return reply(msg.ID, result, failure)
}

func (s *Session) elicit(ctx context.Context, msg *Message) *Message {
	if s.opts.Elicitor == nil {
		return NewErrorResponse(msg.ID, CodeInvalidRequest, "Elicitation not supported")
	}
	var params ElicitParams
	if json.Unmarshal(msg.Params, &params) != nil {
		return NewErrorResponse(msg.ID, CodeInvalidParams, "Invalid request parameters")
	}
	result, failure := s.opts.Elicitor.Elicit(ctx, &params)
	return reply(msg.ID, result, failure)
}

func reply(id json.RawMessage, result any, failure *RPCError) *Message {
	if failure != nil {
		return &Message{JSONRPC: "2.0", ID: id, Error: failure}
	}
	answer, err := NewResponse(id, result)
	if err != nil {
		return NewErrorResponse(id, CodeInternalError, err.Error())
	}
	return answer
}

// Characters of one server log line kept
const logCap = 2000

func (s *Session) notified(msg *Message) {
	switch msg.Method {
	case MethodToolsListChanged:
		slog.Info("MCP server's tools changed", "server", s.opts.Name)
		if s.opts.OnToolsChanged != nil {
			s.opts.OnToolsChanged()
		}
	case MethodElicitationComplete:
		var p struct {
			ID string `json:"elicitationId"`
		}
		if tracker, ok := s.opts.Elicitor.(URLTracker); ok && json.Unmarshal(msg.Params, &p) == nil && p.ID != "" {
			tracker.Complete(p.ID)
		}
	case MethodLoggingMessage:
		s.logged(msg.Params)
	case MethodProgress:
		var p ProgressParams
		if json.Unmarshal(msg.Params, &p) != nil {
			return
		}
		s.mu.Lock()
		sink := s.progress[IDKey(p.ProgressToken)]
		s.mu.Unlock()
		if sink != nil {
			sink(p)
		}
	case MethodCancelled:
		var p struct {
			ID json.RawMessage `json:"requestId"`
		}
		if json.Unmarshal(msg.Params, &p) == nil && len(p.ID) > 0 {
			s.mu.Lock()
			if cancel := s.inflight[IDKey(p.ID)]; cancel != nil {
				cancel()
			}
			s.mu.Unlock()
		}
	}
}

// Writes a server's log notification to the log at its level, tagged with the server and its logger
func (s *Session) logged(raw json.RawMessage) {
	var p struct {
		Level  string          `json:"level"`
		Logger string          `json:"logger"`
		Data   json.RawMessage `json:"data"`
	}
	if json.Unmarshal(raw, &p) != nil {
		return
	}
	level := slog.LevelInfo
	switch strings.ToLower(p.Level) {
	case "debug":
		level = slog.LevelDebug
	case "warning":
		level = slog.LevelWarn
	case "error", "critical", "alert", "emergency":
		level = slog.LevelError
	}
	var data string
	if json.Unmarshal(p.Data, &data) != nil {
		data = string(p.Data)
	}
	if runes := []rune(data); len(runes) > logCap {
		data = string(runes[:logCap]) + "... [truncated]"
	}
	origin := s.opts.Name
	if p.Logger != "" {
		origin += "/" + p.Logger
	}
	slog.Log(context.Background(), level, "MCP server log ["+origin+"]: "+data)
}

// Sends one request and waits for its answer, telling the server when the caller gives up
func (s *Session) request(ctx context.Context, msg *Message, out any) error {
	if version, modern := s.modern(); modern {
		if err := s.stamp(msg, version); err != nil {
			return err
		}
	}
	ch := make(chan *Message, 1)
	key := IDKey(msg.ID)
	s.mu.Lock()
	if s.err != nil {
		err := s.err
		s.mu.Unlock()
		return err
	}
	s.pending[key] = ch
	s.mu.Unlock()
	if err := s.t.Send(ctx, msg); err != nil {
		s.mu.Lock()
		delete(s.pending, key)
		s.mu.Unlock()
		return err
	}
	select {
	case resp, ok := <-ch:
		if !ok {
			return s.Err()
		}
		if resp.Error != nil {
			return resp.Error
		}
		if out == nil || len(resp.Result) == 0 {
			return nil
		}
		if err := json.Unmarshal(resp.Result, out); err != nil {
			return fmt.Errorf("The answer of the server to %s cannot be read: %w", msg.Method, err)
		}
		return nil
	case <-ctx.Done():
		s.mu.Lock()
		delete(s.pending, key)
		s.mu.Unlock()
		if note, err := NewNotification(MethodCancelled, map[string]any{"requestId": msg.ID, "reason": ctx.Err().Error()}); err == nil {
			sendCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			s.t.Send(sendCtx, note)
			cancel()
		}
		return ctx.Err()
	}
}

func (s *Session) call(ctx context.Context, method string, params, out any) error {
	msg, err := NewRequest(method, params)
	if err != nil {
		return err
	}
	return s.request(ctx, msg, out)
}

func (s *Session) notify(ctx context.Context, method string, params any) error {
	msg, err := NewNotification(method, params)
	if err != nil {
		return err
	}
	return s.t.Send(ctx, msg)
}

// Capabilities this client advertises: sampling and elicitation only when it answers them
func (s *Session) capabilities() ClientCapabilities {
	var caps ClientCapabilities
	if s.opts.Sampler != nil {
		caps.Sampling = s.opts.Sampler.Capabilities()
		if caps.Sampling == nil {
			caps.Sampling = map[string]any{}
		}
	}
	if s.opts.Elicitor != nil {
		caps.Elicitation = s.opts.Elicitor.Capabilities()
		if caps.Elicitation == nil {
			caps.Elicitation = map[string]any{}
		}
	}
	return caps
}

func (s *Session) initializeParams() InitializeParams {
	return InitializeParams{ProtocolVersion: HandshakeVersion, Capabilities: s.capabilities(), ClientInfo: Implementation{Name: ClientName, Version: provider.Version()}}
}

// Initialize answered with a revision the handshake does not speak
type UnsupportedVersionError struct {
	Version string
}

func (e *UnsupportedVersionError) Error() string {
	return "Unsupported protocol version from the server: " + e.Version
}

// Adopts a negotiated answer and pins later requests to the revision
func (s *Session) adopt(result *InitializeResult, version string) {
	s.mu.Lock()
	s.result, s.version = result, version
	s.mu.Unlock()
	if pinned, ok := s.t.(interface{ SetProtocolVersion(string) }); ok {
		pinned.SetProtocolVersion(version)
	}
}

// Handshake, refusing a revision it does not speak, then notifications/initialized
func (s *Session) Initialize(ctx context.Context) (*InitializeResult, error) {
	var result InitializeResult
	if err := s.call(ctx, MethodInitialize, s.initializeParams(), &result); err != nil {
		return nil, err
	}
	if !handshakeVersion(result.ProtocolVersion) {
		return nil, &UnsupportedVersionError{Version: result.ProtocolVersion}
	}
	s.adopt(&result, result.ProtocolVersion)
	if err := s.notify(ctx, MethodInitialized, nil); err != nil {
		return nil, err
	}
	return &result, nil
}

// Stateless server/discover probe, adopting the capabilities and revision it names
func (s *Session) Discover(ctx context.Context) (*InitializeResult, error) {
	msg, err := NewRequest(MethodDiscover, nil)
	if err != nil {
		return nil, err
	}
	if err := s.stamp(msg, LatestVersion); err != nil {
		return nil, err
	}
	var result InitializeResult
	if err := s.request(ctx, msg, &result); err != nil {
		return nil, err
	}
	version := result.ProtocolVersion
	if version == "" {
		version = LatestVersion
	}
	s.adopt(&result, version)
	return &result, nil
}

// Handshake again, adopting its answer pinned to the revision this client offered
func (s *Session) handshakeAtOffered(ctx context.Context) (*InitializeResult, error) {
	var result InitializeResult
	if err := s.call(ctx, MethodInitialize, s.initializeParams(), &result); err != nil {
		return nil, err
	}
	pinned := result
	pinned.ProtocolVersion = HandshakeVersion
	s.adopt(&pinned, HandshakeVersion)
	if err := s.notify(ctx, MethodInitialized, nil); err != nil {
		return nil, err
	}
	return &result, nil
}

// Whether the chain carries one of the codes, or its text one of the markers
func rpcMatches(err error, codes []int, markers []string) bool {
	if code := RPCCode(err); code != 0 {
		for _, c := range codes {
			if code == c {
				return true
			}
		}
	}
	text := strings.ToLower(err.Error())
	for _, m := range markers {
		if strings.Contains(text, m) {
			return true
		}
	}
	return false
}

// Whether the server answered method not found, structurally or in a common wording
func MethodNotFound(err error) bool {
	return err != nil && rpcMatches(err, []int{CodeMethodNotFound}, []string{strconv.Itoa(CodeMethodNotFound), "method not found", "unknown method", "not found: ping"})
}

// Whether a failed handshake says the server speaks only the stateless revision
func rejectedAsModern(err error) bool {
	return rpcMatches(err, []int{CodeUnsupportedProtocolVersion}, []string{"unsupported protocol version", strconv.Itoa(CodeUnsupportedProtocolVersion)}) ||
		MethodNotFound(err)
}

// Whether an error is a deadline, which negotiation never falls back from
func Timeout(err error) bool {
	var timeout interface{ Timeout() bool }
	return errors.Is(err, context.DeadlineExceeded) || errors.As(err, &timeout) && timeout.Timeout()
}

// Protocol modes a server's settings may pin
const (
	ModeAuto      = "auto"
	ModeStateless = "stateless"
	ModeLegacy    = "legacy"
)

// Negotiates the protocol era under the connect timeout: auto tries the handshake first and discover only when the server says it is
// stateless; stateless the other way round; legacy only the handshake
func (s *Session) Negotiate(ctx context.Context, mode string, connectTimeout time.Duration) (*InitializeResult, error) {
	call := func(step func(context.Context) (*InitializeResult, error)) (*InitializeResult, error) {
		c, cancel := context.WithTimeout(ctx, connectTimeout)
		defer cancel()
		return step(c)
	}
	attempt := func(primary, fallback func(context.Context) (*InitializeResult, error), handshakeFirst bool, fall func(error) bool, note string) (*InitializeResult, error) {
		result, err := call(primary)
		if err == nil {
			return result, nil
		}
		if Timeout(err) || !fall(err) {
			return nil, err
		}
		slog.Info(note, "server", s.opts.Name, "error", err)
		result, fallbackErr := call(fallback)
		if fallbackErr == nil {
			return result, nil
		}
		if _, unsupported := errors.AsType[*UnsupportedVersionError](err); Timeout(fallbackErr) || !handshakeFirst || !unsupported {
			return nil, fallbackErr
		}
		slog.Info("MCP server/discover also failed; completing the handshake at the version this client offered", "server", s.opts.Name,
			"error", fallbackErr, "version", HandshakeVersion)
		return call(s.handshakeAtOffered)
	}
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case ModeStateless, "modern", LatestVersion:
		return attempt(s.Discover, s.Initialize, false, func(error) bool { return true },
			"MCP server/discover rejected despite protocol: stateless; falling back to the legacy handshake")
	case ModeLegacy, "handshake":
		return call(s.Initialize)
	case ModeAuto, "":
	default:
		slog.Warn("MCP protocol setting unknown, treating it as auto (valid: auto, stateless, legacy)", "server", s.opts.Name, "protocol", mode)
	}
	return attempt(s.Initialize, s.Discover, true, func(err error) bool {
		_, unsupported := errors.AsType[*UnsupportedVersionError](err)
		return rejectedAsModern(err) || unsupported
	}, "MCP legacy handshake rejected; retrying through server/discover (stateless server)")
}

func (s *Session) Ping(ctx context.Context) error {
	return s.call(ctx, MethodPing, nil, nil)
}

// Pages a list may follow, so a cursor that never ends cannot spin
const listPages = 50

// Hints of how long the first page of a list may be kept
type CacheHints struct {
	TTLMs      *float64
	CacheScope string
}

func paginate[T any](ctx context.Context, s *Session, method string, items func(*listPage) []T, hints *CacheHints) ([]T, error) {
	var out []T
	cursor := ""
	for page := 0; ; page++ {
		if page == listPages {
			slog.Warn("MCP list pagination passed its page limit, truncating", "server", s.opts.Name, "method", method, "items", len(out))
			return out, nil
		}
		var params any
		if cursor != "" {
			params = map[string]any{"cursor": cursor}
		}
		var result listPage
		if err := s.call(ctx, method, params, &result); err != nil {
			return nil, err
		}
		if hints != nil && page == 0 {
			hints.TTLMs, hints.CacheScope = result.TTLMs, result.CacheScope
		}
		out = append(out, items(&result)...)
		if cursor = result.NextCursor; cursor == "" {
			return out, nil
		}
	}
}

// Every tool the server lists, the first page's cache hints in hints when given
func (s *Session) ListTools(ctx context.Context, hints *CacheHints) ([]*Tool, error) {
	return paginate(ctx, s, MethodToolsList, func(p *listPage) []*Tool { return p.Tools }, hints)
}

func (s *Session) ListResources(ctx context.Context) ([]*Resource, error) {
	return paginate(ctx, s, MethodResourcesList, func(p *listPage) []*Resource { return p.Resources }, nil)
}

func (s *Session) ListPrompts(ctx context.Context) ([]*Prompt, error) {
	return paginate(ctx, s, MethodPromptsList, func(p *listPage) []*Prompt { return p.Prompts }, nil)
}

func (s *Session) ReadResource(ctx context.Context, uri string) (*ReadResourceResult, error) {
	var out ReadResourceResult
	if err := s.callWithInputs(ctx, MethodResourcesRead, map[string]any{"uri": uri}, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (s *Session) GetPrompt(ctx context.Context, name string, args map[string]string) (*GetPromptResult, error) {
	params := map[string]any{"name": name}
	if args != nil {
		params["arguments"] = args
	}
	var out GetPromptResult
	if err := s.callWithInputs(ctx, MethodPromptsGet, params, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Receives the progress of a running call
type ProgressFunc func(progress float64, total *float64, message string)

// Calls a tool, asking for progress when given a sink; a browser step the user refused or left unfinished comes back as a tool error
func (s *Session) CallTool(ctx context.Context, name string, args map[string]any, progress ProgressFunc) (*CallToolResult, error) {
	if args == nil {
		args = map[string]any{}
	}
	var out CallToolResult
	err := s.callWithInputs(ctx, MethodToolsCall, map[string]any{"name": name, "arguments": args}, progress, &out)
	if step, ok := errors.AsType[*URLElicitationError](err); ok {
		return &CallToolResult{Content: []*ContentBlock{{Type: "text", Text: step.Error()}}, IsError: true}, nil
	}
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// Opens the optional stream a server sends its own requests on, for a transport that has one
func (s *Session) OpenStream() {
	if opener, ok := s.t.(interface{ OpenStream(context.Context) }); ok {
		opener.OpenStream(s.ctx)
	}
}

// Closes the transport and waits until every pending request failed
func (s *Session) Close() error {
	err := s.t.Close()
	<-s.done
	return err
}
