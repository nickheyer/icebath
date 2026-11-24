package provider

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/tools"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	executil "github.com/nickheyer/icebath/pkg/utils/exec"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	pathutil "github.com/nickheyer/icebath/pkg/utils/path"
	textutil "github.com/nickheyer/icebath/pkg/utils/text"
)

// CodexMinVersion is the oldest codex CLI the app server runtime accepts
var CodexMinVersion = [3]int{0, 125, 0}

const codexTransportLostCode = -32000

// CodexAppServerError is a JSON-RPC error the app server reported
type CodexAppServerError struct {
	Code    any
	Message string
	Data    any
}

func (e *CodexAppServerError) Error() string {
	return fmt.Sprintf("codex app-server error %s: %s", anyutil.String(e.Code), e.Message)
}

// CodexAppServerTransportError reports the JSON-RPC transport gone, a failed write or a drained request
type CodexAppServerTransportError struct{ CodexAppServerError }

func (e *CodexAppServerTransportError) Error() string { return e.Message }

// Matches a CodexAppServerError target, a lost transport being an app server error too
func (e *CodexAppServerTransportError) As(target any) bool {
	if t, ok := target.(**CodexAppServerError); ok {
		*t = &e.CodexAppServerError
		return true
	}
	return false
}

func codexTransportLost(msg string) *CodexAppServerTransportError {
	return &CodexAppServerTransportError{CodexAppServerError{Code: codexTransportLostCode, Message: msg}}
}

// CodexRequestTimeoutError reports a request whose reply did not land in time
type CodexRequestTimeoutError struct {
	Method  string
	Timeout time.Duration
}

func (e *CodexRequestTimeoutError) Error() string {
	return fmt.Sprintf("codex app-server method %q timed out after %ss", e.Method, strconv.FormatFloat(e.Timeout.Seconds(), 'f', -1, 64))
}
func (e *CodexRequestTimeoutError) ErrorKind() string { return "TimeoutError" }

// Failure of the app server protocol carrying the kind of mistake it reports
type codexKindError struct{ kind, msg string }

func (e *codexKindError) Error() string     { return e.msg }
func (e *codexKindError) ErrorKind() string { return e.kind }

// Unbounded FIFO with a timed take, the queue.Queue the reader threads feed
type codexQueue struct {
	mu    sync.Mutex
	items []any
	ready chan struct{}
}

func newCodexQueue() *codexQueue { return &codexQueue{ready: make(chan struct{}, 1)} }

func (q *codexQueue) put(v any) {
	q.mu.Lock()
	q.items = append(q.items, v)
	q.mu.Unlock()
	select {
	case q.ready <- struct{}{}:
	default:
	}
}

func (q *codexQueue) pop() (any, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.items) == 0 {
		return nil, false
	}
	v := q.items[0]
	q.items = q.items[1:]
	if len(q.items) > 0 {
		select {
		case q.ready <- struct{}{}:
		default:
		}
	}
	return v, true
}

// Next item, waiting up to timeout, a non positive timeout never blocks
func (q *codexQueue) take(timeout time.Duration) any {
	if v, ok := q.pop(); ok || timeout <= 0 {
		return v
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case <-q.ready:
			if v, ok := q.pop(); ok {
				return v
			}
		case <-timer.C:
			v, _ := q.pop()
			return v
		}
	}
}

// CodexAppServerOptions configures the codex app-server child process
type CodexAppServerOptions struct {
	CodexBin  string
	CodexHome string
	ExtraArgs []string
	Env       map[string]string
	ToolEnv   *tools.Env
}

// CodexAppServerClient is a synchronous JSON-RPC 2.0 client of codex app-server over stdio
type CodexAppServerClient struct {
	codexBin       string
	cmd            *exec.Cmd
	stdin          *os.File
	sendMu         sync.Mutex
	idMu           sync.Mutex
	nextID         int
	pendingMu      sync.Mutex
	pending        map[int]chan any
	notifications  *codexQueue
	serverRequests *codexQueue
	stderrMu       sync.Mutex
	stderrLines    []string
	closed         atomic.Bool
	initialized    bool
	exited         chan struct{}
	stderrDone     chan struct{}
}

// Argv and environment of the app-server child, the kanban worker scope and sandbox overrides included
func codexAppServerSpawn(opts CodexAppServerOptions) ([]string, map[string]string, error) {
	spawnEnv, err := codexSpawnEnvFn(opts.ToolEnv, true, nil)
	if err != nil {
		return nil, nil, err
	}
	for k, v := range opts.Env {
		spawnEnv[k] = v
	}
	if opts.CodexHome != "" {
		spawnEnv["CODEX_HOME"] = opts.CodexHome
	}
	argv := append([]string{opts.CodexBin, "app-server"}, opts.ExtraArgs...)
	ownedTask := os.Getenv("ICEBATH_KANBAN_TASK") != "" && tools.IsDispatcherOwnedWorkerContext(opts.ToolEnv)
	if ownedTask {
		for _, key := range append(tools.KanbanEnvKeys(), "ICEBATH_KANBAN_DB", "ICEBATH_KANBAN_BOARD") {
			if val, ok := os.LookupEnv(key); ok {
				quoted, _ := jsonutil.Encode(val)
				argv = append(argv, "-c", fmt.Sprintf("mcp_servers.%s.env.%s=%s", CodexToolsMCPServerName, key, quoted))
			}
		}
		argv = append(argv, "-c", fmt.Sprintf(`mcp_servers.%s.env.%s=""`, CodexToolsMCPServerName, tools.DelegatedChildEnvMarker))
	}
	spawnEnv = tools.DelegatedChildSubprocessEnv(opts.ToolEnv, spawnEnv)
	if ownedTask {
		homeDir, ok := spawnEnv["ICEBATH_HOME"]
		if !ok {
			homeDir = home.PlatformDefault()
		}
		kanbanRoot, rootSet := spawnEnv["ICEBATH_KANBAN_ROOT"]
		if !rootSet {
			kanbanRoot = pathutil.Join(homeDir, "kanban")
		}
		if db := spawnEnv["ICEBATH_KANBAN_DB"]; db != "" {
			kanbanRoot = filepath.Dir(db)
		}
		argv = append(argv,
			"-c", `sandbox_mode="workspace-write"`,
			"-c", fmt.Sprintf(`sandbox_workspace_write.writable_roots=["%s"]`, kanbanRoot),
			"-c", "sandbox_workspace_write.network_access=false",
		)
	}
	if _, ok := spawnEnv["RUST_LOG"]; !ok {
		spawnEnv["RUST_LOG"] = "warn"
	}
	return argv, spawnEnv, nil
}

// Sanitized app-server child environment builder, replaced in tests to raise the damaged record error
var codexSpawnEnvFn = tools.IcebathSubprocessEnvChecked

// Starts codex app-server and its stdout and stderr readers
func NewCodexAppServerClient(opts CodexAppServerOptions) (*CodexAppServerClient, error) {
	if opts.CodexBin == "" {
		opts.CodexBin = "codex"
	}
	argv, spawnEnv, err := codexAppServerSpawn(opts)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = make([]string, 0, len(spawnEnv))
	for k, v := range spawnEnv {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	hideChildWindow(cmd)
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		stdinR.Close()
		stdinW.Close()
		return nil, err
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		for _, f := range []*os.File{stdinR, stdinW, stdoutR, stdoutW} {
			f.Close()
		}
		return nil, err
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdinR, stdoutW, stderrW
	startErr := cmd.Start()
	stdinR.Close()
	stdoutW.Close()
	stderrW.Close()
	if startErr != nil {
		stdinW.Close()
		stdoutR.Close()
		stderrR.Close()
		return nil, startErr
	}
	c := &CodexAppServerClient{
		codexBin: opts.CodexBin, cmd: cmd, stdin: stdinW, nextID: 1, pending: map[int]chan any{},
		notifications: newCodexQueue(), serverRequests: newCodexQueue(),
		exited: make(chan struct{}), stderrDone: make(chan struct{}),
	}
	go func() {
		_ = cmd.Wait()
		close(c.exited)
	}()
	go c.readStdout(stdoutR)
	go c.readStderr(stderrR)
	return c, nil
}

// Pid of the app-server child
func (c *CodexAppServerClient) PID() int { return c.cmd.Process.Pid }

// Sends initialize then initialized and returns the server's InitializeResponse
func (c *CodexAppServerClient) Initialize(clientName, clientTitle, clientVersion string, capabilities any, timeout time.Duration) (any, error) {
	if c.initialized {
		return nil, &codexKindError{"RuntimeError", "already initialized"}
	}
	info := jsonutil.NewObject()
	info.Set("name", clientName)
	info.Set("title", clientTitle)
	info.Set("version", clientVersion)
	params := jsonutil.NewObject()
	params.Set("clientInfo", info)
	params.Set("capabilities", cxOr(capabilities, jsonutil.NewObject()))
	result, err := c.Request("initialize", params, timeout)
	if err != nil {
		return nil, err
	}
	if err := c.Notify("initialized", nil); err != nil {
		return nil, err
	}
	c.initialized = true
	return result, nil
}

// Closes stdin and waits for the process tree to exit, sweeping the descendants snapshotted before the root retired
func (c *CodexAppServerClient) Close(timeout time.Duration) {
	if c.closed.Swap(true) {
		return
	}
	c.failPendingRequests("codex app-server client is closing")
	descendants := codexSnapshotDescendants(c.cmd.Process.Pid)
	defer codexReapSnapshotted(descendants)
	c.sendMu.Lock()
	_ = c.stdin.Close()
	c.sendMu.Unlock()
	_ = codexTerminate(c.cmd.Process)
	select {
	case <-c.exited:
	case <-time.After(timeout):
		KillProcessTree(c.cmd.Process.Pid, 0)
		select {
		case <-c.exited:
		case <-time.After(time.Second):
		}
	}
}

// Unblocks every waiting request with a synthetic transport lost reply
func (c *CodexAppServerClient) failPendingRequests(reason string) {
	c.pendingMu.Lock()
	items := c.pending
	c.pending = map[int]chan any{}
	c.pendingMu.Unlock()
	if len(items) == 0 {
		return
	}
	errObj := jsonutil.NewObject()
	errObj.Set("code", codexTransportLostCode)
	errObj.Set("message", reason)
	synthetic := jsonutil.NewObject()
	synthetic.Set("error", errObj)
	synthetic.Set("transportLost", true)
	for _, ch := range items {
		select {
		case ch <- synthetic:
		default:
		}
	}
}

// Sends a request and blocks for its result, a reported error becomes CodexAppServerError
func (c *CodexAppServerClient) Request(method string, params any, timeout time.Duration) (any, error) {
	c.idMu.Lock()
	rid := c.nextID
	c.nextID++
	c.idMu.Unlock()
	ch := make(chan any, 1)
	c.pendingMu.Lock()
	c.pending[rid] = ch
	c.pendingMu.Unlock()
	msg := jsonutil.NewObject()
	msg.Set("id", rid)
	msg.Set("method", method)
	msg.Set("params", cxOr(params, jsonutil.NewObject()))
	if err := c.send(msg); err != nil {
		c.dropPending(rid)
		return nil, err
	}
	var reply any
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case reply = <-ch:
	case <-timer.C:
		c.dropPending(rid)
		return nil, &CodexRequestTimeoutError{Method: method, Timeout: timeout}
	}
	if cxHas(reply, "error") {
		errVal := cxGet(reply, "error")
		if !cxIsObject(errVal) {
			return nil, &codexKindError{"AttributeError", fmt.Sprintf("the Codex app server error is %s, expected a mapping", errchain.Kind(errVal))}
		}
		code := any(-1)
		if cxHas(errVal, "code") {
			code = cxGet(errVal, "code")
		}
		message := ""
		if cxHas(errVal, "message") {
			message = anyutil.String(cxGet(errVal, "message"))
		}
		base := CodexAppServerError{Code: code, Message: message, Data: cxGet(errVal, "data")}
		if cxTruthy(cxGet(reply, "transportLost")) {
			return nil, &CodexAppServerTransportError{base}
		}
		return nil, &base
	}
	if cxHas(reply, "result") {
		return cxGet(reply, "result"), nil
	}
	return jsonutil.NewObject(), nil
}

func (c *CodexAppServerClient) dropPending(rid int) {
	c.pendingMu.Lock()
	delete(c.pending, rid)
	c.pendingMu.Unlock()
}

// Sends a JSON-RPC notification, no id and no reply
func (c *CodexAppServerClient) Notify(method string, params any) error {
	msg := jsonutil.NewObject()
	msg.Set("method", method)
	msg.Set("params", cxOr(params, jsonutil.NewObject()))
	return c.send(msg)
}

// Replies to a server initiated request such as an approval prompt
func (c *CodexAppServerClient) Respond(requestID any, result any) error {
	msg := jsonutil.NewObject()
	msg.Set("id", requestID)
	msg.Set("result", result)
	return c.send(msg)
}

// Replies to a server initiated request with an error, data only when given
func (c *CodexAppServerClient) RespondError(requestID any, code int, message string, data any) error {
	errObj := jsonutil.NewObject()
	errObj.Set("code", code)
	errObj.Set("message", message)
	if data != nil {
		errObj.Set("data", data)
	}
	msg := jsonutil.NewObject()
	msg.Set("id", requestID)
	msg.Set("error", errObj)
	return c.send(msg)
}

// Next streaming notification or nil on timeout, zero never blocks
func (c *CodexAppServerClient) TakeNotification(timeout time.Duration) any {
	return c.notifications.take(timeout)
}

// Next server initiated request such as an exec or applyPatch approval
func (c *CodexAppServerClient) TakeServerRequest(timeout time.Duration) any {
	return c.serverRequests.take(timeout)
}

// Last n lines of codex stderr, every line for zero and all but the first -n for a negative n
func (c *CodexAppServerClient) StderrTail(n int) []string {
	if !c.IsAlive() {
		select {
		case <-c.stderrDone:
		case <-time.After(time.Second):
		}
	}
	c.stderrMu.Lock()
	defer c.stderrMu.Unlock()
	start := 0
	switch {
	case n > 0:
		start = max(len(c.stderrLines)-n, 0)
	case n < 0:
		start = min(-n, len(c.stderrLines))
	}
	return append([]string(nil), c.stderrLines[start:]...)
}

// True while the app-server process runs
func (c *CodexAppServerClient) IsAlive() bool {
	select {
	case <-c.exited:
		return false
	default:
		return true
	}
}

func (c *CodexAppServerClient) send(obj any) error {
	if c.closed.Load() {
		return codexTransportLost("codex app-server client is closed")
	}
	if c.stdin == nil {
		return codexTransportLost("codex app-server stdin not available")
	}
	text, err := jsonutil.Encode(obj)
	if err != nil {
		return err
	}
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	if _, err := c.stdin.Write([]byte(text + "\n")); err != nil {
		return codexTransportLost(fmt.Sprintf("codex app-server stdin closed unexpectedly: %s", codexOSErrorText(err)))
	}
	return nil
}

// Text of a failed read or write on the app server pipes
func codexOSErrorText(err error) string {
	if errors.Is(err, os.ErrClosed) {
		return "write to closed file"
	}
	return errchain.OSText(err)
}

func (c *CodexAppServerClient) appendStderr(line string) {
	c.stderrMu.Lock()
	defer c.stderrMu.Unlock()
	c.stderrLines = append(c.stderrLines, line)
	if len(c.stderrLines) > 500 {
		c.stderrLines = append([]string(nil), c.stderrLines[len(c.stderrLines)-500:]...)
	}
}

// Routes every stdout line to its pending reply or queue, failing pending requests when stdout ends
func (c *CodexAppServerClient) readStdout(r io.ReadCloser) {
	defer r.Close()
	defer c.failPendingRequests("codex app-server stdout closed")
	reader := bufio.NewReader(r)
	for {
		raw, readErr := reader.ReadBytes('\n')
		if line := bytes.TrimSpace(raw); len(line) > 0 {
			msg, err := jsonutil.Decode(line)
			var decodeErr *jsonutil.SyntaxError
			switch {
			case errors.As(err, &decodeErr):
				cut := line
				if len(cut) > 200 {
					cut = cut[:200]
				}
				c.appendStderr("<non-json on stdout> " + strconv.Quote(string(cut)))
				continue
			case err != nil:
				c.appendStderr("<stdout reader error> " + err.Error())
				return
			}
			if dispatchErr := c.dispatch(msg); dispatchErr != nil {
				c.appendStderr("<stdout reader error> " + dispatchErr.Error())
				return
			}
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) && !errors.Is(readErr, fs.ErrClosed) {
				c.appendStderr("<stdout reader error> " + codexOSErrorText(readErr))
			}
			return
		}
	}
}

// Whether a decoded message holds key, a type error for a message that is no container
func codexMsgHas(msg any, key string) (bool, error) {
	switch m := msg.(type) {
	case *jsonutil.Object, map[string]any:
		return cxHas(m, key), nil
	case []any:
		for _, v := range m {
			if s, ok := v.(string); ok && s == key {
				return true, nil
			}
		}
		return false, nil
	case string:
		return strings.Contains(m, key), nil
	}
	return false, &codexKindError{"TypeError", fmt.Sprintf("a Codex app server message is %s, expected a mapping", errchain.Kind(msg))}
}

// Pending reply key of a JSON id, integral numbers and bools keyed by their integer value
func codexReplyKey(id any) (int, bool) {
	switch v := id.(type) {
	case bool:
		if v {
			return 1, true
		}
		return 0, true
	case float64:
		if v == float64(int(v)) {
			return int(v), true
		}
		return 0, false
	}
	if n, ok := anyutil.Int(id); ok {
		return int(n), true
	}
	return 0, false
}

func (c *CodexAppServerClient) dispatch(msg any) error {
	hasID, err := codexMsgHas(msg, "id")
	if err != nil {
		return err
	}
	hasResult, _ := codexMsgHas(msg, "result")
	hasError, _ := codexMsgHas(msg, "error")
	hasMethod, _ := codexMsgHas(msg, "method")
	if hasID && (hasResult || hasError) {
		if !cxIsObject(msg) {
			return &codexKindError{"TypeError", fmt.Sprintf("a Codex app server message is %s, expected a mapping", errchain.Kind(msg))}
		}
		key, ok := codexReplyKey(cxGet(msg, "id"))
		if !ok {
			return nil
		}
		c.pendingMu.Lock()
		ch := c.pending[key]
		delete(c.pending, key)
		c.pendingMu.Unlock()
		if ch != nil {
			select {
			case ch <- msg:
			default:
			}
		}
	} else if hasMethod {
		if hasID {
			c.serverRequests.put(msg)
		} else {
			c.notifications.put(msg)
		}
	}
	return nil
}

func (c *CodexAppServerClient) readStderr(r io.ReadCloser) {
	defer close(c.stderrDone)
	defer r.Close()
	reader := bufio.NewReader(r)
	for {
		raw, err := reader.ReadBytes('\n')
		if len(raw) > 0 {
			c.appendStderr(strings.TrimRightFunc(strings.ToValidUTF8(string(raw), "\uFFFD"), unicode.IsSpace))
		}
		if err != nil {
			return
		}
	}
}

var codexVersionRe = regexp.MustCompile(`(\d+)\.(\d+)\.(\d+)`)

// Major, minor and patch of codex --version output, false when no version is present
func ParseCodexVersion(output string) ([3]int, bool) {
	m := codexVersionRe.FindStringSubmatch(output)
	if m == nil {
		return [3]int{}, false
	}
	var v [3]int
	for i := range v {
		n, err := strconv.Atoi(m[i+1])
		if err != nil {
			return [3]int{}, false
		}
		v[i] = n
	}
	return v, true
}

func codexVersionText(v [3]int) string { return fmt.Sprintf("%d.%d.%d", v[0], v[1], v[2]) }

func codexVersionLess(a, b [3]int) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

// Verifies the codex CLI is installed at the minimum version, the version text or the failure message
func CheckCodexBinary(codexBin string, minVersion [3]int) (bool, string, error) {
	if codexBin == "" {
		codexBin = "codex"
	}
	res, err := executil.Run(context.Background(), []string{codexBin, "--version"}, executil.Options{Timeout: 10 * time.Second, RequireSuccess: true})
	stdout, stderr := res.Stdout, res.Stderr
	var notFound *exec.Error
	var timeout *executil.TimeoutError
	var called *executil.StatusError
	switch {
	case errors.As(err, &notFound) && errors.Is(notFound.Err, exec.ErrNotFound), errors.Is(err, fs.ErrNotExist):
		return false, fmt.Sprintf("codex CLI not found at %q. Install with: npm i -g @openai/codex", codexBin), nil
	case errors.As(err, &timeout):
		return false, "codex --version timed out", nil
	case errors.As(err, &called):
		return false, fmt.Sprintf("codex --version exited %d: %s", called.ExitCode, strings.TrimSpace(textutil.NormalizeNewlines(strings.ToValidUTF8(string(stderr), "\uFFFD")))), nil
	case err != nil:
		return false, "", err
	}
	out := textutil.NormalizeNewlines(strings.ToValidUTF8(string(stdout), "\uFFFD"))
	version, ok := ParseCodexVersion(out)
	if !ok {
		return false, fmt.Sprintf("could not parse codex version from: %q", out), nil
	}
	have := codexVersionText(version)
	if codexVersionLess(version, minVersion) {
		return false, fmt.Sprintf("codex %s is older than required %s. Run: npm i -g @openai/codex", have, codexVersionText(minVersion)), nil
	}
	return true, have, nil
}
