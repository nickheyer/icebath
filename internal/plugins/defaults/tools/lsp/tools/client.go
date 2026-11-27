package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/packages"
	lspconfig "github.com/nickheyer/icebath/internal/plugins/defaults/tools/lsp/config"
	"github.com/nickheyer/icebath/internal/terminal"
	"github.com/nickheyer/icebath/pkg/proc"
	"github.com/nickheyer/icebath/pkg/term"
)

// Budgets and retries of one client
const (
	initializeTimeout   = 45 * time.Second
	documentWait        = 5 * time.Second
	fullWait            = 10 * time.Second
	diagnosticTimeout   = 3 * time.Second
	pushDebounce        = 150 * time.Millisecond
	modifiedRetries     = 3
	retryDelay          = 500 * time.Millisecond
	stderrTail          = 30
	shutdownGrace       = time.Second
	writeQueue          = 64
	stderrLineRunes     = 1000
	pushWaitSlice       = 500 * time.Millisecond
	startFailureWait    = time.Second
	startFailureStderr  = 500 * time.Millisecond
	shutdownRequestWait = 2 * time.Second
)

// Lifecycle states of a client
const (
	stopped  = "stopped"
	starting = "starting"
	running  = "running"
	failed   = "error"
)

var capabilities = map[string]any{
	"window": map[string]any{"workDoneProgress": true},
	"workspace": map[string]any{"configuration": true, "workspaceFolders": true,
		"didChangeWatchedFiles": map[string]any{"dynamicRegistration": true}, "diagnostics": map[string]any{"refreshSupport": false}},
	"textDocument": map[string]any{
		"synchronization": map[string]any{"dynamicRegistration": false, "didOpen": true, "didChange": true, "didSave": true, "willSave": false,
			"willSaveWaitUntil": false},
		"diagnostic": map[string]any{"dynamicRegistration": true, "relatedDocumentSupport": true},
		"publishDiagnostics": map[string]any{"relatedInformation": true, "tagSupport": map[string]any{"valueSet": []int{1, 2}}, "versionSupport": true,
			"codeDescriptionSupport": true, "dataSupport": false},
		"hover":          map[string]any{"contentFormat": []string{"markdown", "plaintext"}},
		"definition":     map[string]any{"linkSupport": true},
		"references":     map[string]any{},
		"documentSymbol": map[string]any{"hierarchicalDocumentSymbolSupport": true},
	},
	"general": map[string]any{"positionEncodings": []string{"utf-16"}},
}

// What a client is started with
type launch struct {
	server  string
	root    string
	command []string
	// Variables set over the child environment, and whether the package manager's Node goes on its PATH
	env  map[string]string
	node bool
	cwd  string
	init map[string]any
	// The server's first push per document is a seed sent before it looked at the text
	seeds bool
}

// One language server process serving one workspace root, more for a server that takes several
type client struct {
	launch
	m   *Manager
	cfg *config.Config

	mu       sync.Mutex
	state    string
	folders  []string
	proc     *process
	pending  map[int64]chan reply
	next     int64
	docs     *documents
	syncKind int
	// The server answers pulled diagnostics; it turned the synchronous tsserver request down
	pullOK  bool
	syncOff bool
	// Shutdown asked for, so a closed connection is no failure
	stopping bool
	// Closed and replaced at each push
	pushed chan struct{}
	// Last lines of stderr, and the exit once the process ended
	tail []string
	exit *int
	// One cleanup of the process at a time
	cleaning sync.Mutex
}

type reply struct {
	result json.RawMessage
	err    error
}

// A started server and the pipes the client holds
type process struct {
	cmd     *exec.Cmd
	pid     int
	stdin   *os.File
	stdout  *os.File
	stderr  *os.File
	exited  chan struct{}
	code    int
	closing atomic.Bool
	// Messages the writer sends in order, closed lost once a write failed, quit once stdin is closed, written once the writer is done
	out     chan []byte
	lost    chan struct{}
	lose    sync.Once
	quit    chan struct{}
	written chan struct{}
	unwatch func()
	// Readers of stdout and stderr ended
	read    chan struct{}
	drained chan struct{}
}

func (p *process) broken() {
	p.closing.Store(true)
	p.lose.Do(func() { close(p.lost) })
}

// Exit status once the process ended, nil while it runs
func (p *process) status() *int {
	select {
	case <-p.exited:
		code := p.code
		return &code
	default:
		return nil
	}
}

func newClient(m *Manager, cfg *config.Config, l launch) *client {
	if l.cwd == "" {
		l.cwd = l.root
	}
	if l.init == nil {
		l.init = map[string]any{}
	}
	return &client{launch: l, m: m, cfg: cfg, state: stopped, folders: []string{l.root}, pending: map[int64]chan reply{}, docs: newDocuments(),
		syncKind: 1, pullOK: true, pushed: make(chan struct{})}
}

func (c *client) logf(level slog.Level, format string, args ...any) {
	c.m.logf(level, "["+c.server+"] "+format, args...)
}

func live(state string) bool { return state == starting || state == running }

// Whether the connection is open, with the lock held
func (c *client) openLocked() bool {
	p := c.proc
	if !live(c.state) || p == nil || p.status() != nil || p.closing.Load() {
		return false
	}
	select {
	case <-p.read:
		return false
	default:
		return true
	}
}

func (c *client) open() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.openLocked()
}

// Whether the client runs over an open connection
func (c *client) running() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state == running && c.openLocked()
}

func (c *client) snapshot() (state string, folders []string, isRunning bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state, slices.Clone(c.folders), c.state == running && c.openLocked()
}

// Starts the server and runs the handshake; on failure the process is killed and its exit and last stderr lines join the error
func (c *client) start(ctx context.Context) error {
	c.mu.Lock()
	if live(c.state) {
		c.mu.Unlock()
		return nil
	}
	c.state = starting
	c.mu.Unlock()
	err := c.spawn()
	if err == nil {
		err = c.initialize(ctx)
	}
	if err == nil && !c.open() {
		err = protocolErrorf("server connection closed during initialization")
	}
	c.mu.Lock()
	if err == nil {
		c.state = running
		c.mu.Unlock()
		return nil
	}
	c.state = failed
	p := c.proc
	c.mu.Unlock()
	if p != nil {
		wait(p.exited, startFailureWait)
		c.record(p)
		wait(p.drained, startFailureStderr)
	}
	c.cleanup(false)
	if details := c.failure(); details != "" && !cancelled(err) {
		err = fmt.Errorf("%w (%s)", err, details)
	}
	return err
}

// Waits for the channel to close within the timeout, whether it did
func wait(ch <-chan struct{}, timeout time.Duration) bool {
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case <-ch:
		return true
	case <-t.C:
		return false
	}
}

func cancelled(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) && !lspFailure(err)
}

// Executable of argv0 as the child's own PATH finds it, an empty entry standing for its folder
func lookPath(name string, env map[string]string, cwd string) (string, error) {
	if runtime.GOOS == "windows" {
		key, _ := terminal.PathKey(env)
		if found := packages.Which(name, env[key]); found != "" {
			return found, nil
		}
		return "", fmt.Errorf("%s: %s", syscall.ENOENT, name)
	}
	if strings.Contains(name, "/") {
		return name, nil
	}
	path, ok := env["PATH"]
	if !ok {
		path = ":/bin:/usr/bin"
	}
	for _, dir := range strings.Split(path, ":") {
		candidate := filepath.Join(dir, name)
		if dir == "" {
			candidate = filepath.Join(cwd, name)
		}
		if st, err := os.Stat(candidate); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("%s: %s", syscall.ENOENT, name)
}

// Starts the server in a session of its own with the child environment, its readers and its writer
func (c *client) spawn() error {
	argv := slices.Clone(c.command)
	if lower := strings.ToLower(argv[0]); runtime.GOOS == "windows" && (strings.HasSuffix(lower, ".cmd") || strings.HasSuffix(lower, ".bat")) {
		argv = append([]string{"cmd.exe", "/c"}, argv...)
	}
	env := c.m.terminal.ChildEnv(c.cfg, nil)
	if c.node {
		env = c.m.packages.EnvFor([]string{packages.Node}, env)
	}
	maps.Copy(env, c.env)
	path, err := lookPath(argv[0], env, c.cwd)
	if err != nil {
		return protocolErrorf("LSP server binary not found: %s (%s)", argv[0], err)
	}
	childIn, ourIn, err := stdinPipe()
	if err != nil {
		return err
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		childIn.Close()
		ourIn.Close()
		return err
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		childIn.Close()
		ourIn.Close()
		outR.Close()
		outW.Close()
		return err
	}
	vars := make([]string, 0, len(env))
	for _, k := range slices.Sorted(maps.Keys(env)) {
		vars = append(vars, k+"="+env[k])
	}
	cmd := &exec.Cmd{Path: path, Args: argv, Env: vars, Dir: c.cwd, Stdin: childIn, Stdout: outW, Stderr: errW}
	terminal.NewSession(cmd)
	started := cmd.Start()
	childIn.Close()
	outW.Close()
	errW.Close()
	if started != nil {
		ourIn.Close()
		outR.Close()
		errR.Close()
		if errors.Is(started, fs.ErrNotExist) || errors.Is(started, exec.ErrNotFound) {
			return protocolErrorf("LSP server binary not found: %s (%s)", argv[0], started)
		}
		return started
	}
	p := &process{cmd: cmd, pid: cmd.Process.Pid, stdin: ourIn, stdout: outR, stderr: errR, exited: make(chan struct{}), out: make(chan []byte, writeQueue),
		lost: make(chan struct{}), quit: make(chan struct{}), written: make(chan struct{}), read: make(chan struct{}), drained: make(chan struct{})}
	go func() {
		cmd.Wait()
		p.code = proc.ExitCode(cmd.ProcessState)
		close(p.exited)
	}()
	go func() {
		defer close(p.written)
		for {
			select {
			case data := <-p.out:
				if _, err := p.stdin.Write(data); err != nil {
					p.broken()
					return
				}
			case <-p.quit:
				return
			}
		}
	}()
	p.unwatch = watchClose(ourIn, p.broken)
	c.m.terminal.Supervisor.Own(p.pid)
	terminal.RegisterChild(c.cfg, p.pid, "lsp-server")
	c.mu.Lock()
	c.proc = p
	c.mu.Unlock()
	go c.drain(p)
	go c.reader(p)
	return nil
}

// Keeps reading the server's stderr so its pipe never fills, its last lines kept for a failure report
func (c *client) drain(p *process) {
	defer close(p.drained)
	r := bufio.NewReaderSize(p.stderr, 64<<10)
	for {
		var line []byte
		var err error
		overrun := false
		for {
			chunk, readErr := r.ReadSlice('\n')
			line = append(line, chunk...)
			if len(line) > streamLimit {
				overrun, line = true, nil
			}
			if !errors.Is(readErr, bufio.ErrBufferFull) {
				err = readErr
				break
			}
		}
		if overrun {
			c.logf(slog.LevelWarn, "stderr: line exceeded stream limit, discarding")
		} else if text := strings.TrimRightFunc(strings.ToValidUTF8(string(line), "\uFFFD"), unicode.IsSpace); text != "" {
			if runes := []rune(text); len(runes) > stderrLineRunes {
				text = string(runes[:stderrLineRunes])
			}
			c.logf(slog.LevelDebug, "stderr: %s", text)
			c.mu.Lock()
			if c.tail = append(c.tail, text); len(c.tail) > stderrTail {
				c.tail = c.tail[len(c.tail)-stderrTail:]
			}
			c.mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

// Exit of the process, kept once it ended
func (c *client) record(p *process) {
	if code := p.status(); code != nil {
		c.mu.Lock()
		if c.exit == nil {
			c.exit = code
		}
		c.mu.Unlock()
	}
}

// How the server ended and the last lines it wrote to stderr
func (c *client) failure() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var parts []string
	if code := c.exit; code != nil {
		if *code < 0 {
			parts = append(parts, fmt.Sprintf("server exited on signal %d", -*code))
		} else {
			parts = append(parts, fmt.Sprintf("server exited with code %d", *code))
		}
	}
	if len(c.tail) > 0 {
		parts = append(parts, "last stderr lines: "+strings.Join(c.tail, " | "))
	}
	return strings.Join(parts, "; ")
}

// Hands each message of the server on until its stdout ends or breaks, then retires a client that was not asked to stop
func (c *client) reader(p *process) {
	r := bufio.NewReaderSize(p.stdout, 64<<10)
	for {
		e, ok, err := readMessage(r)
		if err != nil {
			if _, protocol := errors.AsType[*protocolError](err); protocol {
				c.logf(slog.LevelWarn, "protocol error in reader loop: %s", err)
			}
			break
		}
		if !ok {
			c.logf(slog.LevelDebug, "server closed stdout cleanly")
			break
		}
		c.dispatch(p, e)
	}
	c.mu.Lock()
	unexpected := !c.stopping && live(c.state)
	if unexpected {
		c.state = failed
	}
	pending := c.pending
	c.pending = map[int64]chan reply{}
	close(p.read)
	c.mu.Unlock()
	for _, ch := range pending {
		ch <- reply{err: protocolErrorf("server connection closed")}
	}
	if unexpected {
		c.cleanup(true)
	}
}

func (c *client) dispatch(p *process, e *envelope) {
	kind := invalid
	if e != nil {
		kind = e.kind()
	}
	switch kind {
	case response:
		id, ok := e.id()
		if !ok {
			return
		}
		c.mu.Lock()
		ch := c.pending[id]
		delete(c.pending, id)
		c.mu.Unlock()
		if ch == nil {
			return
		}
		if e.Error == nil {
			ch <- reply{result: e.Result}
			return
		}
		code, text := -32000, "unknown"
		if e.Error.Code != nil {
			if n, err := e.Error.Code.Int64(); err == nil {
				code = int(n)
			}
		}
		if e.Error.Message != nil {
			text = *e.Error.Message
		}
		ch <- reply{err: &requestError{code: code, message: text}}
	case request:
		go c.answer(p, e)
	case notification:
		if *e.Method == "textDocument/publishDiagnostics" {
			c.published(e.Params)
		}
	default:
		c.logf(slog.LevelWarn, "dropping invalid message")
	}
}

// Answers a request of the server, those it has no handler for with method not found
func (c *client) answer(p *process, e *envelope) {
	method := *e.Method
	var message map[string]any
	switch method {
	case "window/workDoneProgress/create", "client/registerCapability", "client/unregisterCapability", "workspace/diagnostic/refresh":
		message = resultMessage(e.ID, nil)
	case "workspace/workspaceFolders":
		c.mu.Lock()
		folders, err := folderList(c.folders)
		c.mu.Unlock()
		if err != nil {
			c.logf(slog.LevelWarn, "request handler %s failed: %s", method, err)
			message = errorMessage(e.ID, -32000, "handler failed: "+err.Error())
		} else {
			message = resultMessage(e.ID, folders)
		}
	case "workspace/configuration":
		message = resultMessage(e.ID, c.configuration(e.Params))
	default:
		message = errorMessage(e.ID, methodNotFound, "method not found: "+method)
	}
	data, err := encode(message)
	if err != nil || p.closing.Load() {
		return
	}
	select {
	case p.out <- data:
	case <-p.lost:
	case <-p.quit:
	}
}

// Sections the server asks for, walked by their dotted path through the initialization options, null when missing
func (c *client) configuration(params json.RawMessage) []any {
	var asked struct {
		Items []json.RawMessage `json:"items"`
	}
	if json.Unmarshal(params, &asked) != nil {
		return []any{nil}
	}
	out := make([]any, 0, len(asked.Items))
	for _, raw := range asked.Items {
		var item struct {
			Section *string `json:"section"`
		}
		if json.Unmarshal(raw, &item) != nil || !bytes.HasPrefix(bytes.TrimSpace(raw), []byte("{")) {
			out = append(out, nil)
			continue
		}
		section := ""
		if item.Section != nil {
			section = *item.Section
		}
		out = append(out, c.section(section))
	}
	return out
}

func (c *client) section(section string) any {
	if len(c.init) == 0 {
		return nil
	}
	if section == "" {
		return c.init
	}
	var current any = c.init
	for part := range strings.SplitSeq(section, ".") {
		m, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		if current, ok = m[part]; !ok {
			return nil
		}
	}
	return current
}

// Workspace folder of a root
func folder(root string) (map[string]any, error) {
	name := filepath.Base(strings.TrimRight(root, string(filepath.Separator)))
	if name == "" || name == "." || name == string(filepath.Separator) {
		name = root
	}
	uri, err := fileURI(root)
	if err != nil {
		return nil, err
	}
	return map[string]any{"name": name, "uri": uri}, nil
}

func folderList(roots []string) ([]any, error) {
	out := make([]any, len(roots))
	for i, root := range roots {
		f, err := folder(root)
		if err != nil {
			return nil, err
		}
		out[i] = f
	}
	return out, nil
}

// Attaches another root to a running server that takes several, kept before it is announced
func (c *client) addFolder(ctx context.Context, root string) error {
	c.mu.Lock()
	if slices.Contains(c.folders, root) {
		c.mu.Unlock()
		return nil
	}
	c.folders = append(c.folders, root)
	c.mu.Unlock()
	f, err := folder(root)
	if err != nil {
		return err
	}
	return c.notify(ctx, "workspace/didChangeWorkspaceFolders", map[string]any{"event": map[string]any{"added": []any{f}, "removed": []any{}}})
}

// Detaches a root from a server that keeps serving the others
func (c *client) removeFolder(ctx context.Context, root string) error {
	c.mu.Lock()
	i := slices.Index(c.folders, root)
	if i < 0 {
		c.mu.Unlock()
		return nil
	}
	c.folders = slices.Delete(slices.Clone(c.folders), i, i+1)
	c.mu.Unlock()
	f, err := folder(root)
	if err != nil {
		return err
	}
	return c.notify(ctx, "workspace/didChangeWorkspaceFolders", map[string]any{"event": map[string]any{"added": []any{}, "removed": []any{f}}})
}

// The handshake: initialize, the change kind the server syncs by, initialized and the settings it was given
func (c *client) initialize(ctx context.Context) error {
	uri, err := fileURI(c.root)
	if err != nil {
		return err
	}
	c.mu.Lock()
	folders, err := folderList(c.folders)
	c.mu.Unlock()
	if err != nil {
		return err
	}
	params := map[string]any{"rootUri": uri, "rootPath": c.root, "processId": os.Getpid(), "workspaceFolders": folders,
		"initializationOptions": c.init, "capabilities": capabilities}
	result, err := within(ctx, initializeTimeout, func(ctx context.Context) (json.RawMessage, error) { return c.request(ctx, "initialize", params) })
	if err != nil {
		return err
	}
	var answer struct {
		Capabilities struct {
			Sync json.RawMessage `json:"textDocumentSync"`
		} `json:"capabilities"`
	}
	if !bytes.HasPrefix(bytes.TrimSpace(result), []byte("{")) || json.Unmarshal(result, &answer) != nil {
		return protocolErrorf("initialize answered %s, not an object", result)
	}
	kind := 1
	var options struct {
		Change *int `json:"change"`
	}
	if json.Unmarshal(answer.Capabilities.Sync, &kind) != nil {
		kind = 1
		if json.Unmarshal(answer.Capabilities.Sync, &options) == nil && options.Change != nil {
			kind = *options.Change
		}
	}
	c.mu.Lock()
	c.syncKind = kind
	c.mu.Unlock()
	if err := c.notify(ctx, "initialized", map[string]any{}); err != nil {
		return err
	}
	if len(c.init) > 0 {
		return c.notify(ctx, "workspace/didChangeConfiguration", map[string]any{"settings": c.init})
	}
	return nil
}

// Runs fn under a deadline, the deadline passing reading as a timeout and the caller giving up as a cancellation
func within[T any](ctx context.Context, d time.Duration, fn func(ctx context.Context) (T, error)) (T, error) {
	inner, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	v, err := fn(inner)
	if err != nil && ctx.Err() == nil && inner.Err() != nil && errors.Is(err, inner.Err()) {
		return v, &timeoutError{text: fmt.Sprintf("no answer within %s", d)}
	}
	return v, err
}

// Shutdown and exit, a grace for the server to leave, then the kill of its tree
func (c *client) shutdown(ctx context.Context) error {
	c.mu.Lock()
	if c.stopping {
		c.mu.Unlock()
		return nil
	}
	c.stopping = true
	isRunning := c.state == running && c.openLocked()
	p := c.proc
	c.mu.Unlock()
	var err error
	if isRunning {
		if _, rerr := within(ctx, shutdownRequestWait, func(ctx context.Context) (json.RawMessage, error) { return c.request(ctx, "shutdown", nil) }); rerr != nil &&
			!lspFailure(rerr) {
			err = rerr
		}
		if err == nil {
			if nerr := c.notify(ctx, "exit", nil); nerr != nil && cancelled(nerr) {
				err = nerr
			}
		}
		if err == nil && p != nil && p.status() == nil {
			select {
			case <-p.exited:
			case <-time.After(shutdownGrace):
			case <-ctx.Done():
				err = ctx.Err()
			}
		}
	}
	c.mu.Lock()
	c.state = stopped
	c.mu.Unlock()
	c.cleanup(false)
	return err
}

// Ends the pipe readers, reaps or kills the process tree and hands its group back; from the reader itself its own end is not waited for
func (c *client) cleanup(fromReader bool) {
	c.cleaning.Lock()
	defer c.cleaning.Unlock()
	c.mu.Lock()
	p := c.proc
	c.proc = nil
	c.mu.Unlock()
	if p == nil {
		return
	}
	p.stdout.Close()
	p.stderr.Close()
	if !fromReader {
		<-p.read
	}
	<-p.drained
	defer c.closeStdin(p)
	if p.status() == nil {
		descendants := term.Descendants(p.pid)
		killGroup(p.pid)
		p.cmd.Process.Kill()
		for _, pid := range descendants {
			if proc, err := os.FindProcess(pid); err == nil {
				proc.Kill()
			}
		}
		wait(p.exited, shutdownGrace)
	}
	c.record(p)
	c.m.terminal.Supervisor.Release(p.pid)
}

// Stops the watcher and the writer, then closes the stdin pipe
func (c *client) closeStdin(p *process) {
	p.broken()
	close(p.quit)
	p.unwatch()
	p.stdin.Close()
	<-p.written
}

// Queues a message behind every earlier one, waiting only while the queue is full
func (c *client) write(ctx context.Context, message map[string]any) error {
	data, err := encode(message)
	if err != nil {
		return err
	}
	c.mu.Lock()
	p := c.proc
	c.mu.Unlock()
	if p == nil || p.closing.Load() {
		return errors.New("Connection lost")
	}
	select {
	case p.out <- data:
		return nil
	case <-p.lost:
		return errors.New("Connection lost")
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *client) requireOpen(method string) error {
	if !c.open() {
		return protocolErrorf("cannot send %q: server connection closed", method)
	}
	return nil
}

// Sends a request and waits for its answer
func (c *client) request(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if err := c.requireOpen(method); err != nil {
		return nil, err
	}
	ch := make(chan reply, 1)
	c.mu.Lock()
	id := c.next
	c.next++
	c.pending[id] = ch
	c.mu.Unlock()
	forget := func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}
	if err := c.write(ctx, requestMessage(id, method, params)); err != nil {
		forget()
		if cancelled(err) {
			return nil, err
		}
		return nil, protocolErrorf("send failed for %q: %s", method, err)
	}
	select {
	case r := <-ch:
		return r.result, r.err
	case <-ctx.Done():
		forget()
		return nil, ctx.Err()
	}
}

// Sends a request, a ContentModified answer retried with a backoff that doubles
func (c *client) retried(ctx context.Context, method string, params any, timeout time.Duration) (json.RawMessage, error) {
	for attempt := 0; ; attempt++ {
		result, err := within(ctx, timeout, func(ctx context.Context) (json.RawMessage, error) { return c.request(ctx, method, params) })
		if err == nil || errorCode(err) != contentModified || attempt >= modifiedRetries {
			return result, err
		}
		select {
		case <-time.After(retryDelay << attempt):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// Sends a notification, a failed write logged and let go
func (c *client) notify(ctx context.Context, method string, params any) error {
	if err := c.requireOpen(method); err != nil {
		return err
	}
	if err := c.write(ctx, notificationMessage(method, params)); err != nil {
		if cancelled(err) {
			return err
		}
		c.logf(slog.LevelDebug, "notify %s failed: %s", method, err)
	}
	return nil
}

// Keeps a push tagged with the version it echoes or the document's current one; the seed push of a seeding server untagged
func (c *client) published(params json.RawMessage) {
	var push struct {
		URI         *string           `json:"uri"`
		Version     *int              `json:"version"`
		Diagnostics []json.RawMessage `json:"diagnostics"`
	}
	if json.Unmarshal(params, &push) != nil || push.URI == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	doc := c.docs.ensure(uriPath(*push.URI))
	seed := c.seeds && !doc.seeded
	doc.seeded = true
	doc.push = diagnostics(push.Diagnostics)
	if seed {
		return
	}
	doc.pushed = doc.version
	if push.Version != nil {
		doc.pushed = *push.Version
	}
	close(c.pushed)
	c.pushed = make(chan struct{})
}

// Sends didOpen the first time and didChange after, the text read from disk when none is given; the version it is synced at
func (c *client) openFile(ctx context.Context, path, language string, content *string) (int, error) {
	if !c.running() {
		return 0, protocolErrorf("client not running")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return 0, err
	}
	var synced string
	if content == nil {
		raw, err := os.ReadFile(abs)
		if err != nil {
			reason := err
			if pe, ok := errors.AsType[*fs.PathError](err); ok {
				reason = pe.Err
			}
			return 0, protocolErrorf("cannot read %s: %s", abs, reason)
		}
		synced = asDiskRead(string(raw))
	} else {
		synced = asDiskRead(*content)
	}
	uri, err := fileURI(abs)
	if err != nil {
		return 0, err
	}
	c.mu.Lock()
	doc := c.docs.get(abs)
	if doc != nil && doc.version < 0 {
		doc = nil
	}
	c.mu.Unlock()
	change := 2
	if doc == nil {
		change = 1
	}
	if err := c.notify(ctx, "workspace/didChangeWatchedFiles", map[string]any{"changes": []any{map[string]any{"uri": uri, "type": change}}}); err != nil {
		return 0, err
	}
	if doc == nil {
		c.mu.Lock()
		c.docs.touch(abs, newDocument(0, synced))
		c.mu.Unlock()
		if err := c.notify(ctx, "textDocument/didOpen", map[string]any{"textDocument": map[string]any{"uri": uri, "languageId": language, "version": 0,
			"text": synced}}); err != nil {
			return 0, err
		}
		return 0, c.evict(ctx)
	}
	c.mu.Lock()
	c.docs.touch(abs, doc)
	contentChange := map[string]any{"text": synced}
	if c.syncKind == 2 {
		contentChange["range"] = span{End: endOf(doc.text)}
	}
	doc.version++
	doc.text = synced
	version := doc.version
	c.mu.Unlock()
	return version, c.notify(ctx, "textDocument/didChange", map[string]any{"textDocument": map[string]any{"uri": uri, "version": version},
		"contentChanges": []any{contentChange}})
}

// Drops the least recently touched documents past the cap, closing those the server has open
func (c *client) evict(ctx context.Context) error {
	for {
		c.mu.Lock()
		if c.docs.len() <= maxDocuments {
			c.mu.Unlock()
			return nil
		}
		path, doc := c.docs.oldest()
		c.mu.Unlock()
		if doc.version < 0 {
			continue
		}
		uri, err := fileURI(path)
		if err != nil {
			return err
		}
		if err := c.notify(ctx, "textDocument/didClose", map[string]any{"textDocument": map[string]any{"uri": uri}}); err != nil {
			return err
		}
	}
}

// Sends didSave, which some linters wait for before they look again
func (c *client) saveFile(ctx context.Context, path string) error {
	if !c.running() {
		return nil
	}
	uri, err := fileURI(path)
	if err != nil {
		return err
	}
	return c.notify(ctx, "textDocument/didSave", map[string]any{"textDocument": map[string]any{"uri": uri}})
}

// Pulls the document's diagnostics into its pull store, tagged with the version it had when asked
func (c *client) pull(ctx context.Context, abs string) error {
	c.mu.Lock()
	sent := -1
	if doc := c.docs.get(abs); doc != nil {
		sent = doc.version
	}
	c.mu.Unlock()
	uri, err := fileURI(abs)
	if err != nil {
		return err
	}
	result, err := c.retried(ctx, "textDocument/diagnostic", map[string]any{"textDocument": map[string]any{"uri": uri}}, diagnosticTimeout)
	if err != nil {
		if !lspFailure(err) {
			return err
		}
		if errorCode(err) == methodNotFound {
			c.mu.Lock()
			c.pullOK = false
			c.mu.Unlock()
		}
		c.logf(slog.LevelDebug, "document diagnostic pull failed: %s", err)
		return nil
	}
	type pulled struct {
		Items *[]json.RawMessage `json:"items"`
	}
	var report struct {
		pulled
		Related map[string]pulled `json:"relatedDocuments"`
	}
	if json.Unmarshal(result, &report) != nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if report.Items != nil {
		doc := c.docs.ensure(abs)
		doc.pull, doc.pulled = diagnostics(*report.Items), sent
	}
	for related, r := range report.Related {
		if r.Items != nil {
			doc := c.docs.ensure(uriPath(related))
			doc.pull, doc.pulled = diagnostics(*r.Items), doc.version
		}
	}
	return nil
}

// Waits for fresh diagnostics at the version, pulled and pushed at once; false means no data rather than no errors
func (c *client) awaitDiagnostics(ctx context.Context, path string, version int, mode string, timeout time.Duration) (bool, error) {
	if timeout <= 0 {
		timeout = documentWait
		if mode == lspconfig.WaitFull {
			timeout = fullWait
		}
	}
	deadline := time.Now().Add(timeout)
	abs, err := filepath.Abs(path)
	if err != nil {
		return false, err
	}
	inner, cancel := context.WithCancel(ctx)
	c.mu.Lock()
	pullOK := c.pullOK
	c.mu.Unlock()
	// The pull and the push wait run together; each channel is set to nil once its wait ended, which a select then never picks
	var pulled, pushed chan struct{}
	if pullOK {
		pulled = make(chan struct{})
		go func() {
			defer close(pulled)
			c.pull(inner, abs)
		}()
	}
	pushed = make(chan struct{})
	go func() {
		defer close(pushed)
		c.freshPush(inner, abs, version, timeout)
	}()
	pulling, pushing := pulled, pushed
	defer func() {
		cancel()
		if pulled != nil {
			<-pulled
		}
		<-pushed
	}()
	for {
		c.mu.Lock()
		open := c.openLocked()
		doc := c.docs.get(abs)
		fresh := doc != nil && doc.fresh(version)
		c.mu.Unlock()
		if !open {
			return false, protocolErrorf("server connection closed while waiting for diagnostics")
		}
		if fresh {
			return true, nil
		}
		remaining := time.Until(deadline)
		if remaining <= 0 || pulling == nil && pushing == nil {
			return false, nil
		}
		t := time.NewTimer(remaining)
		select {
		case <-pulling:
			pulling = nil
		case <-pushing:
			pushing = nil
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			return false, ctx.Err()
		}
		t.Stop()
	}
}

// Waits for the next push or the timeout, whether a push came
func (c *client) awaitPush(ctx context.Context, since chan struct{}, timeout time.Duration) (bool, error) {
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case <-since:
		return true, nil
	case <-t.C:
		return false, nil
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

// Waits for a fresh push at the version, then a moment more for the second push some servers send right after
func (c *client) freshPush(ctx context.Context, abs string, version int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		c.mu.Lock()
		doc := c.docs.get(abs)
		fresh := doc != nil && doc.freshPush(version)
		next := c.pushed
		c.mu.Unlock()
		if fresh {
			_, err := c.awaitPush(ctx, next, pushDebounce)
			return err
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil
		}
		if _, err := c.awaitPush(ctx, next, min(remaining, pushWaitSlice)); err != nil {
			return err
		}
	}
}

// Waits for the seed push of a document on a seeding server, and what it carried
func (c *client) awaitSeed(ctx context.Context, abs string, timeout time.Duration) ([]diagnostic, bool, error) {
	deadline := time.Now().Add(timeout)
	for {
		c.mu.Lock()
		doc := c.docs.get(abs)
		if doc != nil && doc.seeded {
			seed := slices.Clone(doc.push)
			c.mu.Unlock()
			return seed, true, nil
		}
		open := c.openLocked()
		next := c.pushed
		c.mu.Unlock()
		remaining := time.Until(deadline)
		if remaining <= 0 || !open {
			return nil, false, nil
		}
		if _, err := c.awaitPush(ctx, next, min(remaining, pushWaitSlice)); err != nil {
			return nil, false, err
		}
	}
}

// Text last synced for a path, false when it was never opened
func (c *client) synced(abs string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if doc := c.docs.get(abs); doc != nil && doc.version >= 0 {
		return doc.text, true
	}
	return "", false
}

// Pushed and pulled diagnostics together, each store counted only while fresh at the document's version
func (c *client) diagnosticsFor(abs string) []diagnostic {
	c.mu.Lock()
	defer c.mu.Unlock()
	doc := c.docs.get(abs)
	if doc == nil {
		return []diagnostic{}
	}
	var push, pull []diagnostic
	if doc.freshPush(doc.version) {
		push = doc.push
	}
	if doc.freshPull(doc.version) {
		pull = doc.pull
	}
	return dedupe(push, pull)
}
