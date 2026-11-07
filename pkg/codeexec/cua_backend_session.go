package codeexec

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/mcp"
	"github.com/nickheyer/icebath/pkg/tools"
)

// bridgeTimeoutError is concurrent.futures.TimeoutError of a bridged call whose deadline passed
type bridgeTimeoutError struct{}

func (bridgeTimeoutError) Error() string { return "" }

// cuaClosedResourceError is the anyio ClosedResourceError of a call on a dead MCP stream
type cuaClosedResourceError struct{}

func (cuaClosedResourceError) Error() string { return "" }

// cuaAsyncBridge runs bridged calls under one loop context that stop cancels
type cuaAsyncBridge struct {
	mu     sync.Mutex
	ctx    context.Context
	cancel context.CancelFunc
}

// Starts the loop context once, a second start of a live bridge is a no-op
func (b *cuaAsyncBridge) start() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.ctx != nil && b.ctx.Err() == nil {
		return nil
	}
	b.ctx, b.cancel = context.WithCancel(context.Background())
	return nil
}

// Loop context of a live bridge, nil when stopped or never started
func (b *cuaAsyncBridge) loop() context.Context {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.ctx == nil || b.ctx.Err() != nil {
		return nil
	}
	return b.ctx
}

// Runs fn on the loop and waits up to timeout seconds, zero or less waits without a deadline
func (b *cuaAsyncBridge) run(fn func(ctx context.Context) (*pyjson.Object, error), timeout float64) (*pyjson.Object, error) {
	ctx := b.loop()
	if ctx == nil {
		return nil, errors.New("cua-driver bridge not started")
	}
	type outcome struct {
		out *pyjson.Object
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		out, err := fn(ctx)
		done <- outcome{out, err}
	}()
	var deadline <-chan time.Time
	if timeout > 0 {
		timer := time.NewTimer(time.Duration(timeout * float64(time.Second)))
		defer timer.Stop()
		deadline = timer.C
	}
	select {
	case o := <-done:
		return o.out, o.err
	case <-deadline:
		return nil, bridgeTimeoutError{}
	}
}

// Cancels the loop context so every pending bridged call ends
func (b *cuaAsyncBridge) stop() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.cancel != nil {
		b.cancel()
	}
	b.ctx, b.cancel = nil, nil
}

// Fail closed messages for calls whose effect on the remote screen is unknown
var unknownOutcomeMessages = map[string]string{
	"transport_outcome_unknown": "cua-driver transport failed during {name}; the action outcome is unknown, so Icebath " +
		"did not replay it. Take fresh state before deciding whether to act again.",
	"timeout_outcome_unknown": "cua-driver MCP call {name} timed out; the action outcome is unknown and may still have " +
		"taken effect on the remote screen. The session has been marked suspect and will be " +
		"recreated before the next computer-use call. Take fresh state before deciding " +
		"whether to act again.",
}

// Fail closed isError envelope for an unknown outcome code
func outcomeUnknown(name string, exc error, code string) *pyjson.Object {
	message := strings.ReplaceAll(unknownOutcomeMessages[code], "{name}", name)
	structured := pyjson.NewObject()
	structured.Set("ok", false)
	structured.Set("code", code)
	structured.Set("message", message)
	structured.Set("operation", name)
	structured.Set("next_step", "fresh_state")
	structured.Set("detail", exc.Error())
	return toolEnvelope(message, []any{}, structured, true, []any{})
}

// Field of a raw MCP object under its wire name, the last of names
func toolField(obj *pyjson.Object, names ...string) any { return cuGet(obj, names[len(names)-1]) }

// Attempts of the CLI fallback transport, backing off from half a second doubling
const cliAttempts = 4

var cliSleep = time.Sleep

// First n code points of s
func firstRunes(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}

// Runs cua-driver call with backoff until it prints JSON, failing fast when the machine wide daemon is down
func cliRunJSON(cmd []string, env map[string]string, name string, timeout float64) (any, error) {
	backoff, lastErr := 0.5, ""
	for attempt := 0; attempt < cliAttempts; attempt++ {
		proc, err := runQuiet(cmd, max(15.0, timeout), runOpts{Env: env, HideWindow: true})
		if err != nil {
			return nil, fmt.Errorf("cua-driver CLI fallback for %s failed to spawn: %v", name, err)
		}
		out, errText := pystr.Strip(proc.Stdout), proc.Stderr
		lastErr = firstRunes(out, 200)
		if lastErr == "" {
			lastErr = firstRunes(errText, 200)
		}
		if strings.Contains(out, "daemon is not running") || strings.Contains(errText, "daemon is not running") {
			return nil, fmt.Errorf("cua-driver CLI fallback for %s unavailable: the "+
				"machine-wide cua-driver daemon is not running (the "+
				"CLI transport requires it; the MCP runtime does not).", name)
		}
		start := -1
		for _, i := range []int{strings.Index(out, "{"), strings.Index(out, "[")} {
			if i != -1 && (start == -1 || i < start) {
				start = i
			}
		}
		if start != -1 {
			if v, err := pyjson.Loads(out[start:]); err == nil {
				return v, nil
			}
		}
		if attempt < cliAttempts-1 {
			slog.Warn(fmt.Sprintf("cua-driver CLI fallback for %s got no JSON (attempt %d/%d); retrying in %.1fs",
				name, attempt+1, cliAttempts, backoff))
			cliSleep(time.Duration(backoff * float64(time.Second)))
			backoff *= 2
		}
	}
	return nil, fmt.Errorf("cua-driver CLI fallback for %s returned no JSON after %d attempts: %s", name, cliAttempts, lastErr)
}

// Remaps a cua-driver call JSON body into the flattened tool result envelope without image mime types
func cliResult(parsed any, shotFile string) *pyjson.Object {
	obj, ok := parsed.(*pyjson.Object)
	if !ok {
		return toolEnvelope(nil, []any{}, nil, false, nil)
	}
	isError := cuGet(obj, "isError") == true || cuGet(obj, "is_error") == true
	shot := cuGet(obj, "screenshot_png_b64")
	fpath := cuGet(obj, "screenshot_file_path")
	if !pyrepr.Truthy(fpath) {
		fpath = shotFile
	}
	if p, isStr := fpath.(string); !pyrepr.Truthy(shot) && isStr && p != "" {
		if _, err := os.Stat(p); err == nil {
			if raw, err := os.ReadFile(p); err == nil {
				shot = base64.StdEncoding.EncodeToString(raw)
			} else {
				slog.Debug(fmt.Sprintf("cua-driver CLI fallback: failed reading %s: %v", p, err))
			}
		}
	}
	data, has := obj.Get("tree_markdown")
	if !has {
		data = nil
	}
	if data != nil && cuGet(obj, "element_count") != nil {
		data = pyrepr.Str(cuGet(obj, "element_count")) + " elements\n" + pyrepr.Str(data)
	}
	images := []any{}
	if pyrepr.Truthy(shot) {
		images = append(images, shot)
	}
	return toolEnvelope(data, images, obj, isError, nil)
}

// Flattens a logical MCP error into text for narrow classification
func logicalErrorText(result *pyjson.Object) string {
	var chunks []string
	for _, value := range []any{cuGet(result, "data"), cuGet(result, "structuredContent")} {
		if value == nil {
			continue
		}
		if s, ok := value.(string); ok {
			chunks = append(chunks, s)
			continue
		}
		if s, err := pyjson.Dumps(value, pyjson.SortKeys(true)); err == nil {
			chunks = append(chunks, s)
		} else {
			chunks = append(chunks, pyrepr.Str(value))
		}
	}
	return strings.Join(chunks, "\n")
}

// Recognises the explicit recoverable ended session rejection of cua-driver
func isEndedSessionResult(result *pyjson.Object) bool {
	if result == nil || cuGet(result, "isError") != true {
		return false
	}
	message := strings.ToLower(logicalErrorText(result))
	return strings.Contains(message, "session") && strings.Contains(message, "start_session") &&
		(strings.Contains(message, "has ended") || strings.Contains(message, "session ended"))
}

// cuaMCPClient is the initialized MCP client session the lifecycle owns
type cuaMCPClient interface {
	Initialize(ctx context.Context) error
	ListTools(ctx context.Context) (*pyjson.Object, error)
	CallTool(ctx context.Context, name string, args *pyjson.Object) (*pyjson.Object, error)
	Close() error
}

// Spawns the stdio MCP server child, swapped by tests for an in process server
var cuaOpenMCP = func(ctx context.Context, command string, args []string, env map[string]string) (cuaMCPClient, error) {
	t, err := mcp.StartStdio(ctx, mcp.StdioParams{Command: command, Args: args, Env: env})
	if err != nil {
		return nil, err
	}
	c := &stdioMCPClient{t: t, pending: map[string]chan *mcp.Message{}, closed: make(chan struct{})}
	go c.pump()
	return c, nil
}

// stdioMCPClient is the mcp ClientSession over a stdio child speaking newline framed JSON-RPC
type stdioMCPClient struct {
	t       *mcp.StdioTransport
	mu      sync.Mutex
	pending map[string]chan *mcp.Message
	closed  chan struct{}
	once    sync.Once
}

// Routes responses to their waiters, answers ping and refuses every other server request
func (c *stdioMCPClient) pump() {
	for msg := range c.t.Receive() {
		switch {
		case msg.IsResponse():
			c.mu.Lock()
			ch := c.pending[mcp.IDKey(msg.ID)]
			delete(c.pending, mcp.IDKey(msg.ID))
			c.mu.Unlock()
			if ch != nil {
				ch <- msg
			}
		case msg.IsRequest():
			var reply *mcp.Message
			if msg.Method == "ping" {
				reply, _ = mcp.NewResponse(msg.ID, map[string]any{})
			} else {
				reply = mcp.NewErrorResponse(msg.ID, -32601, "Method not found")
			}
			_ = c.t.Send(context.Background(), reply)
		}
	}
	c.once.Do(func() { close(c.closed) })
}

// Sends one request and decodes its result, a dead stream fails with the closed resource error
func (c *stdioMCPClient) request(ctx context.Context, method string, params any) (any, error) {
	req, err := mcp.NewRequest(method, params)
	if err != nil {
		return nil, err
	}
	ch := make(chan *mcp.Message, 1)
	key := mcp.IDKey(req.ID)
	c.mu.Lock()
	c.pending[key] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, key)
		c.mu.Unlock()
	}()
	select {
	case <-c.closed:
		return nil, cuaClosedResourceError{}
	default:
	}
	if err := c.t.Send(ctx, req); err != nil {
		if errors.Is(err, syscall.EPIPE) || errors.Is(err, io.ErrClosedPipe) {
			return nil, cuaClosedResourceError{}
		}
		return nil, err
	}
	select {
	case msg := <-ch:
		if msg.Error != nil {
			return nil, errors.New(msg.Error.Message)
		}
		return pyjson.LoadsBytes(msg.Result)
	case <-c.closed:
		return nil, cuaClosedResourceError{}
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Runs the initialize handshake at the latest revision and sends notifications/initialized
func (c *stdioMCPClient) Initialize(ctx context.Context) error {
	params := map[string]any{"protocolVersion": mcp.LatestProtocolVersion, "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "mcp", "version": "0.1.0"}}
	res, err := c.request(ctx, "initialize", params)
	if err != nil {
		return err
	}
	obj, _ := res.(*pyjson.Object)
	version, _ := cuGet(obj, "protocolVersion").(string)
	supported := version == mcp.LatestProtocolVersion
	for _, v := range mcp.SupportedProtocolVersions {
		supported = supported || v == version
	}
	if !supported {
		return fmt.Errorf("Unsupported protocol version from the server: %s", pyrepr.Str(cuGet(obj, "protocolVersion")))
	}
	note, err := mcp.NewNotification("notifications/initialized", nil)
	if err != nil {
		return err
	}
	return c.t.Send(ctx, note)
}

// Raw tools/list result object
func (c *stdioMCPClient) ListTools(ctx context.Context) (*pyjson.Object, error) {
	res, err := c.request(ctx, "tools/list", nil)
	if err != nil {
		return nil, err
	}
	obj, _ := res.(*pyjson.Object)
	return obj, nil
}

// Raw CallToolResult object of one tools/call
func (c *stdioMCPClient) CallTool(ctx context.Context, name string, args *pyjson.Object) (*pyjson.Object, error) {
	params := pyjson.NewObject()
	params.Set("name", name)
	params.Set("arguments", args)
	res, err := c.request(ctx, "tools/call", params)
	if err != nil {
		return nil, err
	}
	obj, _ := res.(*pyjson.Object)
	return obj, nil
}

// Closes the child transport and wakes every waiter
func (c *stdioMCPClient) Close() error {
	err := c.t.Close()
	c.once.Do(func() { close(c.closed) })
	return err
}

// Handshake calls issued by start and stop, exempt from the auto restart guard of callTool
var cuaLifecycleCalls = map[string]bool{"start_session": true, "end_session": true}

// Idempotent reads that are safe to replay after a broken transport
var cuaTransportReplaySafeTools = map[string]bool{"get_cursor_position": true, "get_displays": true, "get_screen_size": true,
	"get_window_state": true, "list_apps": true, "list_windows": true}

// Seconds the lifecycle may take to reach ready, and to shut down
var (
	cuaSessionReadyTimeout    = 30 * time.Second
	cuaSessionShutdownTimeout = 5 * time.Second
)

// cuaDriverSession holds the MCP client, spawned lazily, rebuilt on drop, with a CLI fallback transport
type cuaDriverSession struct {
	bridge         *cuaAsyncBridge
	embeddedDaemon *embeddedCuaDaemon
	env            *tools.Env

	mu      sync.Mutex
	started atomic.Bool

	state             sync.Mutex
	client            cuaMCPClient
	setupErr          error
	startupPhase      string
	capabilities      map[string]map[string]bool
	toolSchemas       map[string]*pyjson.Object
	capabilityVersion string

	ready         chan struct{}
	shutdown      chan struct{}
	shutdownOnce  *sync.Once
	lifecycleDone chan error

	timeoutSuspect      atomic.Bool
	declaredSessionID   *string
	transportGeneration int
	transportReset      func()

	isClosedSessionError   func(err error) bool
	isTransientDaemonError func(err error) bool
	callToolViaCLIFn       func(name string, args *pyjson.Object, timeout float64) (*pyjson.Object, error)
}

// New session over the bridge with an optional private daemon
func newCuaDriverSession(env *tools.Env, bridge *cuaAsyncBridge, daemon *embeddedCuaDaemon) *cuaDriverSession {
	s := &cuaDriverSession{bridge: bridge, embeddedDaemon: daemon, env: env, capabilities: map[string]map[string]bool{},
		toolSchemas: map[string]*pyjson.Object{}, ready: make(chan struct{})}
	s.isClosedSessionError = cuaIsClosedSessionError
	s.isTransientDaemonError = cuaIsTransientDaemonError
	s.callToolViaCLIFn = s.callToolViaCLI
	return s
}

func (s *cuaDriverSession) setPhase(p string) {
	s.state.Lock()
	s.startupPhase = p
	s.state.Unlock()
}

// Owns the stdio MCP client from spawn to shutdown and reports setup failures through setupErr
func (s *cuaDriverSession) lifecycle(ctx context.Context, ready, shutdown chan struct{}) (err error) {
	t0 := time.Now()
	s.setPhase("binary-check")
	signalled := false
	defer func() {
		if err != nil {
			s.state.Lock()
			s.setupErr = err
			s.state.Unlock()
			if !signalled {
				close(ready)
			}
		}
		s.state.Lock()
		s.client = nil
		s.state.Unlock()
		s.started.Store(false)
	}()
	driverCmd, err := resolveCuaDriverCmd(nil)
	if err != nil {
		return err
	}
	if driverCmd == "" {
		sandbox, _, serr := sandboxMCPInvocation(ctx, s.env)
		if serr != nil {
			return serr
		}
		if sandbox == nil {
			return errors.New(cuaDriverInstallHint())
		}
	}
	s.setPhase("manifest-discovery")
	var command string
	var args []string
	var childEnv map[string]string
	if s.embeddedDaemon != nil {
		if command, args, err = s.embeddedDaemon.proxyInvocation(); err != nil {
			return err
		}
		childEnv = s.embeddedDaemon.childEnv()
	} else {
		sandbox, sandboxEnv, serr := sandboxMCPInvocation(ctx, s.env)
		if serr != nil {
			return serr
		}
		if sandbox != nil {
			command, args, childEnv = sandbox.Command, sandbox.Args, sandboxEnv
		} else {
			command, args = resolveMCPInvocation(s.env, driverCmd, 6.0)
			childEnv = cuaDriverChildEnv(s.env, nil)
		}
	}
	tManifest := time.Now()
	sanitized, err := tools.SanitizeSubprocessEnv(s.env, childEnv, nil)
	if err != nil {
		return err
	}
	client, err := cuaOpenMCP(ctx, command, args, sanitized)
	if err != nil {
		return err
	}
	defer client.Close()
	s.setPhase("mcp-initialize")
	if err := client.Initialize(ctx); err != nil {
		return err
	}
	tInit := time.Now()
	s.setPhase("capability-discovery")
	s.populateCapabilities(ctx, client)
	s.state.Lock()
	s.client, s.startupPhase = client, "ready"
	s.state.Unlock()
	signalled = true
	close(ready)
	slog.Info(fmt.Sprintf("cua-driver session ready in %.1fs (manifest=%.1fs, mcp_init=%.1fs)",
		time.Since(t0).Seconds(), tManifest.Sub(t0).Seconds(), tInit.Sub(tManifest).Seconds()))
	select {
	case <-shutdown:
	case <-ctx.Done():
	}
	return nil
}

// Caches per tool capability sets, input schemas and the capability version from tools/list
func (s *cuaDriverSession) populateCapabilities(ctx context.Context, client cuaMCPClient) {
	caps, schemas, version := map[string]map[string]bool{}, map[string]*pyjson.Object{}, ""
	s.state.Lock()
	s.capabilities, s.toolSchemas, s.capabilityVersion = caps, schemas, version
	s.state.Unlock()
	list, err := client.ListTools(ctx)
	if err != nil {
		slog.Debug(fmt.Sprintf("cua-driver tools/list capability discovery failed: %v", err))
		return
	}
	toolsList, _ := toolField(list, "tools").([]any)
	for _, raw := range toolsList {
		tool, ok := raw.(*pyjson.Object)
		if !ok {
			continue
		}
		name, ok := cuGet(tool, "name").(string)
		if !ok {
			continue
		}
		set := map[string]bool{}
		if list, ok := toolField(tool, "capabilities").([]any); ok {
			for _, c := range list {
				if cs, ok := c.(string); ok {
					set[cs] = true
				}
			}
		}
		caps[name] = set
		if schema, ok := toolField(tool, "input_schema", "inputSchema").(*pyjson.Object); ok {
			schemas[name] = schema.Copy()
		} else {
			schemas[name] = pyjson.NewObject()
		}
	}
	if cv, ok := toolField(list, "capability_version").(string); ok {
		version = cv
	}
	s.state.Lock()
	s.capabilities, s.toolSchemas, s.capabilityVersion = caps, schemas, version
	s.state.Unlock()
}

// Starts the bridge and the lifecycle once
func (s *cuaDriverSession) start() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.started.Load() {
		if err := s.bridge.start(); err != nil {
			return err
		}
		if err := s.startLifecycleLocked(); err != nil {
			return err
		}
		s.started.Store(true)
	}
	return nil
}

// Spawns the lifecycle owner and waits for ready, the caller holds mu
func (s *cuaDriverSession) startLifecycleLocked() error {
	ready, shutdown := make(chan struct{}), make(chan struct{})
	s.state.Lock()
	s.ready, s.shutdown, s.shutdownOnce, s.setupErr = ready, shutdown, &sync.Once{}, nil
	s.state.Unlock()
	loop := s.bridge.loop()
	if loop == nil {
		return errors.New("cua-driver bridge not started")
	}
	done := make(chan error, 1)
	s.lifecycleDone = done
	go func() { done <- s.lifecycle(loop, ready, shutdown) }()
	timer := time.NewTimer(cuaSessionReadyTimeout)
	defer timer.Stop()
	select {
	case <-ready:
	case <-timer.C:
		s.signalShutdownLocked()
		s.state.Lock()
		phase := s.startupPhase
		s.state.Unlock()
		if phase == "" {
			phase = "unknown"
		}
		return fmt.Errorf("cua-driver session never reached ready (timeout %ds; stuck in phase: %s). "+
			"Run `icebath computer-use doctor` and check %s/logs/agent.log for the phase timings.",
			int(cuaSessionReadyTimeout.Seconds()), phase, home.Display(home.Dir(context.Background())))
	}
	s.state.Lock()
	setupErr := s.setupErr
	s.state.Unlock()
	if setupErr != nil {
		return fmt.Errorf("cua-driver session setup failed: %w", setupErr)
	}
	s.transportGeneration++
	if s.transportGeneration > 1 {
		s.notifyTransportReset()
	}
	return nil
}

// Stops a started lifecycle
func (s *cuaDriverSession) stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started.Load() {
		s.started.Store(false)
		s.stopLifecycleLocked()
	}
}

// Registers the synchronous cache invalidation hook for transport swaps
func (s *cuaDriverSession) setTransportResetCallback(cb func()) { s.transportReset = cb }

// Runs the transport reset hook, a panic in it is logged and swallowed
func (s *cuaDriverSession) notifyTransportReset() {
	defer func() {
		if r := recover(); r != nil {
			slog.Debug(fmt.Sprintf("cua-driver transport reset callback failed: %v", r))
		}
	}()
	if s.transportReset != nil {
		s.transportReset()
	}
}

// Signals shutdown and waits up to five seconds for the lifecycle to end
func (s *cuaDriverSession) stopLifecycleLocked() {
	s.signalShutdownLocked()
	done := s.lifecycleDone
	s.lifecycleDone = nil
	if done == nil {
		return
	}
	timer := time.NewTimer(cuaSessionShutdownTimeout)
	defer timer.Stop()
	select {
	case err := <-done:
		if err != nil {
			slog.Warn(fmt.Sprintf("cua-driver shutdown error: %v", err))
		}
	case <-timer.C:
		slog.Warn("cua-driver session shutdown timed out (5s)")
	}
}

// Sets the shutdown event of the running lifecycle
func (s *cuaDriverSession) signalShutdownLocked() {
	s.state.Lock()
	shutdown, once := s.shutdown, s.shutdownOnce
	s.state.Unlock()
	if shutdown != nil && once != nil && s.bridge.loop() != nil {
		once.Do(func() { close(shutdown) })
	}
}

// One tools/call on the live client flattened into the envelope
func (s *cuaDriverSession) callToolAsync(name string, args *pyjson.Object) func(ctx context.Context) (*pyjson.Object, error) {
	return func(ctx context.Context) (*pyjson.Object, error) {
		s.state.Lock()
		client := s.client
		s.state.Unlock()
		if client == nil {
			return nil, errors.New("'NoneType' object has no attribute 'call_tool'")
		}
		raw, err := client.CallTool(ctx, name, args)
		if err != nil {
			return nil, err
		}
		return extractToolResult(raw), nil
	}
}

// The driver advertises capability for tool, or for any tool when tool is empty
func (s *cuaDriverSession) supportsCapability(capability, tool string) bool {
	s.state.Lock()
	defer s.state.Unlock()
	if tool != "" {
		return s.capabilities[tool][capability]
	}
	for _, c := range s.capabilities {
		if c[capability] {
			return true
		}
	}
	return false
}

// tools/list advertised the tool, false before discovery
func (s *cuaDriverSession) hasTool(name string) bool {
	s.state.Lock()
	defer s.state.Unlock()
	_, ok := s.capabilities[name]
	return ok
}

// The live input schema of tool accepts the property
func (s *cuaDriverSession) supportsInputProperty(tool, property string) bool {
	s.state.Lock()
	defer s.state.Unlock()
	props, ok := cuGet(s.toolSchemas[tool], "properties").(*pyjson.Object)
	if !ok {
		return false
	}
	_, has := props.Get(property)
	return has
}

// tools/list populated the capability map
func (s *cuaDriverSession) capabilitiesDiscovered() bool {
	s.state.Lock()
	defer s.state.Unlock()
	return len(s.capabilities) > 0
}

// Driver advertised capability vocabulary version, empty on old builds
func (s *cuaDriverSession) capabilityVersionText() string {
	s.state.Lock()
	defer s.state.Unlock()
	return s.capabilityVersion
}

// MCP and stdio failures recoverable by reconnecting
func cuaIsClosedSessionError(err error) bool {
	var closed cuaClosedResourceError
	return errors.As(err, &closed) || errors.Is(err, syscall.EPIPE) || errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.ErrClosedPipe)
}

// Daemon proxy EAGAIN congestion that a retry or the CLI transport survives
func cuaIsTransientDaemonError(err error) bool {
	msg := err.Error()
	for _, needle := range []string{"Resource temporarily unavailable", "os error 35", "daemon transport error", "daemon proxy"} {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}

// Redeclares the stable public label, logging the failure message on rejection
func (s *cuaDriverSession) redeclareSession(timeout float64, failureMsg string) (bool, error) {
	sessionID := s.declaredSessionID
	args := pyjson.NewObject()
	if sessionID != nil {
		args.Set("session", *sessionID)
	} else {
		args.Set("session", nil)
	}
	result, err := s.bridge.run(s.callToolAsync("start_session", args), timeout)
	if err != nil {
		return false, err
	}
	if cuGet(result, "isError") == true {
		slog.Warn(fmt.Sprintf(failureMsg, pyrepr.Str(derefOr(sessionID)), logicalErrorText(result)))
	}
	return cuGet(result, "isError") != true, nil
}

func derefOr(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

// Starts a dead session or rebuilds the lifecycle from scratch, then reattaches the declared label
func (s *cuaDriverSession) recreateSession(name string, timeout float64, logMsg string, restart, clearTimeoutSuspect bool) error {
	slog.Warn(fmt.Sprintf(logMsg, name))
	if !restart {
		if err := s.start(); err != nil {
			return err
		}
	} else {
		err := func() error {
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.started.Load() {
				s.stopLifecycleLocked()
			}
			s.started.Store(false)
			s.state.Lock()
			s.capabilities, s.toolSchemas, s.capabilityVersion = map[string]map[string]bool{}, map[string]*pyjson.Object{}, ""
			s.state.Unlock()
			if err := s.startLifecycleLocked(); err != nil {
				return err
			}
			s.started.Store(true)
			return nil
		}()
		if err != nil {
			return err
		}
		if clearTimeoutSuspect {
			s.timeoutSuspect.Store(false)
		}
	}
	if s.declaredSessionID != nil && *s.declaredSessionID != "" {
		if _, err := s.redeclareSession(timeout, "cua-driver public session label %s could not be restored: %s"); err != nil {
			return err
		}
	}
	return nil
}

// cua-driver call subprocess fallback transport with the screenshot routed through a temp file
func (s *cuaDriverSession) callToolViaCLI(name string, args *pyjson.Object, timeout float64) (*pyjson.Object, error) {
	callArgs, shotFile := args.Copy(), ""
	if _, has := callArgs.Get("screenshot_out_file"); name == "get_window_state" && !has {
		f, err := os.CreateTemp("", "cua_shot_*.png")
		if err != nil {
			return nil, err
		}
		shotFile = f.Name()
		f.Close()
		callArgs.Set("screenshot_out_file", shotFile)
	}
	defer func() {
		if shotFile != "" {
			if _, err := os.Stat(shotFile); err == nil {
				_ = os.Remove(shotFile)
			}
		}
	}()
	driverCommand, err := resolveCuaDriverCmd(nil)
	if err != nil {
		return nil, err
	}
	if driverCommand == "" {
		return nil, errors.New(cuaDriverInstallHint())
	}
	childEnv, socketArgs := cuaDriverChildEnv(s.env, nil), []string{}
	if s.embeddedDaemon != nil {
		command, _, err := s.embeddedDaemon.proxyInvocation()
		if err != nil {
			return nil, err
		}
		driverCommand, childEnv = command, s.embeddedDaemon.childEnv()
		socketArgs = []string{"--socket", s.embeddedDaemon.socketPath}
	}
	encoded, err := pyjson.Dumps(callArgs)
	if err != nil {
		return nil, err
	}
	cmd := append([]string{driverCommand, "call", name, encoded}, socketArgs...)
	sanitized, err := tools.SanitizeSubprocessEnv(s.env, childEnv, nil)
	if err != nil {
		return nil, err
	}
	parsed, err := cliRunJSON(cmd, sanitized, name, timeout)
	if err != nil {
		return nil, err
	}
	return cliResult(parsed, shotFile), nil
}

// One driver tool call with restart of a suspect or dead session, fail closed timeouts and the CLI fallback
func (s *cuaDriverSession) callTool(name string, args *pyjson.Object, timeout float64) (*pyjson.Object, error) {
	if !cuaLifecycleCalls[name] {
		if s.timeoutSuspect.Load() {
			if err := s.recreateSession(name, timeout, "cua-driver session suspect after earlier MCP timeout; recreating before %s", true, true); err != nil {
				return nil, err
			}
		}
		if !s.started.Load() {
			if err := s.recreateSession(name, timeout, "cua-driver session not active on %s; (re)starting before call", false, false); err != nil {
				return nil, err
			}
		}
	}
	if !s.started.Load() {
		return nil, errors.New("cua-driver session not started")
	}
	result, err := s.bridge.run(s.callToolAsync(name, args), timeout)
	if err != nil {
		var timedOut bridgeTimeoutError
		if errors.As(err, &timedOut) {
			s.timeoutSuspect.Store(true)
			slog.Warn(fmt.Sprintf("cua-driver MCP timed out on %s; marking session suspect for recreation before the next call", name))
			return outcomeUnknown(name, err, "timeout_outcome_unknown"), nil
		}
		if s.isTransientDaemonError(err) {
			if !cuaTransportReplaySafeTools[name] {
				s.notifyTransportReset()
				return outcomeUnknown(name, err, "transport_outcome_unknown"), nil
			}
			slog.Warn(fmt.Sprintf("cua-driver MCP transport failed on %s (%v); falling back to CLI transport", name, err))
			return s.callToolViaCLIFn(name, args, timeout)
		}
		if !s.isClosedSessionError(err) {
			return nil, err
		}
		if rerr := s.recreateSession(name, timeout, "cua-driver MCP session closed during %s; reconnecting once", true, false); rerr != nil {
			return nil, rerr
		}
		if !cuaTransportReplaySafeTools[name] {
			return outcomeUnknown(name, err, "transport_outcome_unknown"), nil
		}
		if result, err = s.bridge.run(s.callToolAsync(name, args), timeout); err != nil {
			return nil, err
		}
	}
	declared, _ := cuGet(args, "session").(string)
	ok := cuGet(result, "isError") != true
	if name == "start_session" && ok && declared != "" {
		s.declaredSessionID = &declared
	}
	if isEndedSessionResult(result) {
		if s.declaredSessionID != nil && *s.declaredSessionID != "" && !cuaLifecycleCalls[name] {
			slog.Warn(fmt.Sprintf("cua-driver session %s ended during %s; reviving and retrying once", *s.declaredSessionID, name))
			revived, err := s.redeclareSession(timeout, "cua-driver session %s could not be revived: %s")
			if err != nil {
				return nil, err
			}
			if revived {
				if result, err = s.bridge.run(s.callToolAsync(name, args), timeout); err != nil {
					return nil, err
				}
			}
		}
	} else if declaredRaw := cuGet(args, "session"); name == "end_session" && ok && s.declaredSessionID != nil && declaredRaw == any(*s.declaredSessionID) {
		s.declaredSessionID = nil
	}
	return result, nil
}
