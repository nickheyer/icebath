package lsp

import (
	"bufio"
	"container/list"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"

	"github.com/nickheyer/icebath/pkg/internal/proc"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
)

// Client wait budgets and retry policy
const (
	InitializeTimeout         = 45 * time.Second
	DiagnosticsDocumentWait   = 5 * time.Second
	DiagnosticsFullWait       = 10 * time.Second
	diagnosticsRequestTimeout = 3 * time.Second
	pushDebounce              = 150 * time.Millisecond
	maxContentModifiedRetries = 3
	retryBaseDelay            = 500 * time.Millisecond
	stderrTailLines           = 30
)

// Cap on tracked documents, evicted ones being closed and reopened on their next touch
var maxTrackedFiles = 64

// Wait after exit before the hard kill, and after the hard kill for the reap
var shutdownGrace = time.Second

// Hard kills a server and every descendant while its ancestry is still observable
var killServerTree = func(p *serverProc) {
	proc.KillTree(p.pid)
	proc.Kill(p.cmd)
}

const clientLogger = "agent.lsp.client"

var clientCapabilities = map[string]any{
	"window": map[string]any{"workDoneProgress": true},
	"workspace": map[string]any{"configuration": true, "workspaceFolders": true,
		"didChangeWatchedFiles": map[string]any{"dynamicRegistration": true}, "diagnostics": map[string]any{"refreshSupport": false}},
	"textDocument": map[string]any{
		"synchronization": map[string]any{"dynamicRegistration": false, "didOpen": true, "didChange": true,
			"didSave": true, "willSave": false, "willSaveWaitUntil": false},
		"diagnostic": map[string]any{"dynamicRegistration": true, "relatedDocumentSupport": true},
		"publishDiagnostics": map[string]any{"relatedInformation": true, "tagSupport": map[string]any{"valueSet": []any{1, 2}},
			"versionSupport": true, "codeDescriptionSupport": true, "dataSupport": false},
		"hover":          map[string]any{"contentFormat": []any{"markdown", "plaintext"}},
		"definition":     map[string]any{"linkSupport": true},
		"references":     map[string]any{},
		"documentSymbol": map[string]any{"hierarchicalDocumentSymbolSupport": true},
	},
	"general": map[string]any{"positionEncodings": []any{"utf-16"}},
}

// FileURI returns the file URI of a path, spaces, unicode and drive letters included
func FileURI(path string) (string, error) {
	abs, err := pypath.Abspath(path)
	if err != nil {
		return "", err
	}
	if runtime.GOOS == "windows" {
		abs = strings.ReplaceAll(abs, `\`, "/")
		if !strings.HasPrefix(abs, "/") {
			abs = "/" + abs
		}
	}
	return "file://" + pyQuote(abs, "/:"), nil
}

// LSP WorkspaceFolder of a root
func folder(root string) (map[string]any, error) {
	name := pypath.Basename(strings.TrimRight(root, string(filepath.Separator)))
	if name == "" {
		name = root
	}
	uri, err := FileURI(root)
	if err != nil {
		return nil, err
	}
	return map[string]any{"name": name, "uri": uri}, nil
}

// URIToPath inverts FileURI
func URIToPath(uri string) string {
	if !strings.HasPrefix(uri, "file://") {
		return uri
	}
	raw := uri[len("file://"):]
	if runtime.GOOS == "windows" && strings.HasPrefix(raw, "/") && len(raw) > 2 && raw[2] == ':' {
		raw = raw[1:]
	}
	return pypath.Normpath(pyUnquote(raw))
}

// UTF-16 code units of a string
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		n++
		if r > 0xFFFF {
			n++
		}
	}
	return n
}

// LSP position at the end of a text for a whole document replace range
func endPosition(text string) map[string]any {
	if text == "" {
		return map[string]any{"line": 0, "character": 0}
	}
	lines := pystr.Splitlines(text, false)
	if strings.HasSuffix(text, "\n") || strings.HasSuffix(text, "\r") {
		return map[string]any{"line": len(lines), "character": 0}
	}
	return map[string]any{"line": len(lines) - 1, "character": utf16Len(lines[len(lines)-1])}
}

// AsDiskRead returns a text as a utf-8-sig replace read with universal newlines would return its file
func AsDiskRead(text string) string {
	text = decodeUTF8Replace([]byte(text))
	text = strings.TrimPrefix(text, "\uFEFF")
	return strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
}

// Per document sync state, versions doubling as freshness tags with -1 meaning no data
type docState struct {
	version     int
	text        string
	push        []map[string]any
	pull        []map[string]any
	pushVersion int
	pullVersion int
	seedSeen    bool
}

func newDocState(version int, text string) *docState {
	return &docState{version: version, text: text, pushVersion: -1, pullVersion: -1}
}

func (d *docState) freshPushAt(version int) bool { return d.pushVersion >= version }
func (d *docState) freshPullAt(version int) bool { return d.pullVersion >= version }
func (d *docState) freshAt(version int) bool     { return d.freshPushAt(version) || d.freshPullAt(version) }

// Insertion ordered document map with Python dict semantics
type docMap struct {
	index map[string]*list.Element
	order *list.List
}

type docEntry struct {
	path string
	doc  *docState
}

func newDocMap() *docMap { return &docMap{index: map[string]*list.Element{}, order: list.New()} }

func (m *docMap) get(path string) *docState {
	if e, ok := m.index[path]; ok {
		return e.Value.(*docEntry).doc
	}
	return nil
}

func (m *docMap) set(path string, d *docState) {
	if e, ok := m.index[path]; ok {
		e.Value.(*docEntry).doc = d
		return
	}
	m.index[path] = m.order.PushBack(&docEntry{path, d})
}

func (m *docMap) setdefault(path string, mk func() *docState) *docState {
	if d := m.get(path); d != nil {
		return d
	}
	d := mk()
	m.set(path, d)
	return d
}

func (m *docMap) pop(path string) *docState {
	e, ok := m.index[path]
	if !ok {
		return nil
	}
	delete(m.index, path)
	m.order.Remove(e)
	return e.Value.(*docEntry).doc
}

func (m *docMap) len() int { return len(m.index) }

func (m *docMap) oldest() (string, *docState) {
	e := m.order.Front().Value.(*docEntry)
	return e.path, e.doc
}

// asyncio transport flow control marks
const (
	writeHighWater = 64 * 1024
	writeLowWater  = writeHighWater / 4
)

// Ordered write buffer drained to the server stdin by one goroutine, as an asyncio pipe transport buffer is
type outbox struct {
	mu      sync.Mutex
	cond    *sync.Cond
	items   [][]byte
	pending int
	err     error
	closed  bool
	resumed chan struct{}
}

func newOutbox() *outbox {
	o := &outbox{resumed: make(chan struct{})}
	o.cond = sync.NewCond(&o.mu)
	return o
}

// Appends a message behind every earlier one, reporting whether the buffer passed the high water mark
func (o *outbox) put(data []byte) (paused bool, resumed <-chan struct{}) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed || o.err != nil {
		return false, nil
	}
	o.items = append(o.items, data)
	o.pending += len(data)
	o.cond.Signal()
	return o.pending > writeHighWater, o.resumed
}

// Whether the buffer is still above the low water mark with the pipe healthy
func (o *outbox) stillPaused() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.err == nil && !o.closed && o.pending > writeLowWater
}

func (o *outbox) wakeLocked() {
	close(o.resumed)
	o.resumed = make(chan struct{})
}

func (o *outbox) close() {
	o.mu.Lock()
	o.closed = true
	o.cond.Broadcast()
	o.wakeLocked()
	o.mu.Unlock()
}

// Writes buffered messages in order until closed, dropping the rest after the first write error
func (o *outbox) drain(w *os.File, onBroken func()) {
	for {
		o.mu.Lock()
		for len(o.items) == 0 && !o.closed {
			o.cond.Wait()
		}
		if len(o.items) == 0 || o.err != nil {
			o.mu.Unlock()
			return
		}
		data := o.items[0]
		o.items = o.items[1:]
		o.mu.Unlock()
		_, err := w.Write(data)
		o.mu.Lock()
		o.pending -= len(data)
		if err != nil {
			o.err = err
			o.items = nil
			o.pending = 0
		}
		if err != nil || o.pending <= writeLowWater {
			o.wakeLocked()
		}
		o.mu.Unlock()
		if err != nil {
			onBroken()
			return
		}
	}
}

// A spawned server and the pipes the client holds
type serverProc struct {
	cmd     *exec.Cmd
	pid     int
	stdin   *os.File
	stdout  *os.File
	stderr  *os.File
	reader  *bufio.Reader
	exited  chan struct{}
	code    int
	closing atomic.Bool
	out     *outbox
	writer  chan struct{}
	unwatch func()
}

// Exit status once reaped, nil while the process runs
func (p *serverProc) returncode() *int {
	select {
	case <-p.exited:
		c := p.code
		return &c
	default:
		return nil
	}
}

// Python returncode of a finished process, the negated signal for a signalled one
func exitCodeOf(state *os.ProcessState) int {
	if ws, ok := state.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return -int(ws.Signal())
	}
	return state.ExitCode()
}

// ClientOptions describe one server process and the workspace root it serves
type ClientOptions struct {
	ServerID                   string
	WorkspaceRoot              string
	Command                    []string
	Env                        map[string]string
	Cwd                        string
	InitializationOptions      map[string]any
	SeedDiagnosticsOnFirstPush bool
}

// Client is one language server process serving one workspace root
type Client struct {
	ServerID         string
	WorkspaceRoot    string
	workspaceFolders []string

	command       []string
	env           map[string]string
	cwd           string
	initOptions   map[string]any
	seedFirstPush bool
	rt            *Runtime
	l             *loop

	proc            *serverProc
	stderrTask      *task
	readerTask      *task
	stderrTail      []string
	exitCode        *int
	cleanupSem      chan struct{}
	nextID          int64
	pending         map[int64]*future[any]
	docs            *docMap
	state           string
	syncKind        int
	pullOK          bool
	stopping        bool
	pushEvent       *event
	pushCounter     int
	syncUnsupported bool
}

// NewClient prepares a client on a loop without spawning anything
func newClient(l *loop, rt *Runtime, o ClientOptions) *Client {
	cwd := o.Cwd
	if cwd == "" {
		cwd = o.WorkspaceRoot
	}
	init := o.InitializationOptions
	if init == nil {
		init = map[string]any{}
	}
	return &Client{
		ServerID: o.ServerID, WorkspaceRoot: o.WorkspaceRoot, workspaceFolders: []string{o.WorkspaceRoot},
		command: append([]string{}, o.Command...), env: o.Env, cwd: cwd, initOptions: init,
		seedFirstPush: o.SeedDiagnosticsOnFirstPush, rt: rt, l: l,
		cleanupSem: make(chan struct{}, 1), pending: map[int64]*future[any]{}, docs: newDocMap(),
		state: "stopped", syncKind: 1, pullOK: true, pushEvent: newEvent(),
	}
}

func (c *Client) logf(level slog.Level, format string, args ...any) {
	c.rt.logf(level, clientLogger, format, args...)
}

// State of the client lifecycle
func (c *Client) State() string { return c.state }

func liveState(s string) bool { return s == "starting" || s == "running" }

// Whether the client is running over an open connection
func (c *Client) isRunning() bool { return c.state == "running" && c.connectionIsOpen() }

func (c *Client) connectionIsOpen() bool {
	p, reader := c.proc, c.readerTask
	return liveState(c.state) && p != nil && p.returncode() == nil && p.stdin != nil && !p.closing.Load() &&
		reader != nil && !reader.finished()
}

// Spawns the server and runs the initialize handshake, killing the process and recording details on failure
func (c *Client) start(ctx context.Context) (err error) {
	if liveState(c.state) {
		return nil
	}
	c.state = "starting"
	err = c.spawn(ctx)
	if err == nil {
		err = c.initialize(ctx)
	}
	if err == nil && !c.connectionIsOpen() {
		err = protocolErrorf("server connection closed during initialization")
	}
	if err == nil {
		c.state = "running"
		return nil
	}
	c.state = "error"
	shielded := context.WithoutCancel(ctx)
	if p := c.proc; p != nil {
		c.l.awaitAny(shielded, time.Second, p.exited)
		if rc := p.returncode(); rc != nil {
			c.exitCode = rc
		}
		if t := c.stderrTask; t != nil {
			c.l.awaitAny(shielded, 500*time.Millisecond, t.done)
		}
	}
	c.cleanupProcess(shielded, nil)
	if details := c.failureDetails(); details != "" && !isCancellation(err) {
		attachDetails(err, details)
	}
	return err
}

// Executable for argv0, POSIX searching the child's own PATH the way subprocess does
func lookPathIn(name string, env map[string]string, cwd string) (string, error) {
	if runtime.GOOS == "windows" {
		p, err := exec.LookPath(name)
		if err != nil && !errors.Is(err, exec.ErrDot) {
			return "", &os.PathError{Op: "exec", Path: name, Err: syscall.ENOENT}
		}
		return p, nil
	}
	if strings.Contains(name, "/") {
		return name, nil
	}
	pathValue, ok := env["PATH"]
	if !ok {
		pathValue = ":/bin:/usr/bin"
	}
	for _, dir := range strings.Split(pathValue, ":") {
		p := filepath.Join(dir, name)
		if dir == "" {
			p = filepath.Join(cwd, name)
		}
		if st, err := os.Stat(p); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return "", &os.PathError{Op: "exec", Path: name, Err: syscall.ENOENT}
}

// Starts the server process in its own session with the scrubbed env plus its own, then the pipe tasks
func (c *Client) spawn(ctx context.Context) error {
	cmdline := c.command
	if runtime.GOOS == "windows" {
		if lower := strings.ToLower(cmdline[0]); strings.HasSuffix(lower, ".cmd") || strings.HasSuffix(lower, ".bat") {
			cmdline = append([]string{"cmd.exe", "/c"}, cmdline...)
		}
	}
	base, err := c.rt.subprocessEnv()
	if err != nil {
		return err
	}
	maps.Copy(base, c.env)
	if c.rt.DelegatedChildEnv == nil {
		return missingHook("DelegatedChildEnv")
	}
	childEnv, err := guard(func() (map[string]string, error) { return c.rt.DelegatedChildEnv(base), nil })
	if err != nil {
		return err
	}
	var spawnErr error
	var p *serverProc
	c.l.unlocked(func() { p, spawnErr = startServerProc(cmdline, childEnv, c.cwd) })
	if spawnErr != nil {
		var oe *pyExc
		if errors.As(spawnErr, &oe) && oe.typ == "FileNotFoundError" {
			return protocolErrorf("LSP server binary not found: %s (%s)", cmdline[0], oe.text)
		}
		return spawnErr
	}
	c.proc = p
	if c.rt.OwnProcessGroups == nil || c.rt.RegisterChild == nil {
		return missingHook("process ledger")
	}
	if _, err := guard(func() (struct{}, error) { c.rt.OwnProcessGroups([]int{p.pid}); return struct{}{}, nil }); err != nil {
		return err
	}
	var ledgerErr error
	c.l.unlocked(func() {
		_, ledgerErr = guard(func() (bool, error) { return c.rt.RegisterChild(ctx, p.pid, "lsp-server"), nil })
	})
	if ledgerErr != nil {
		return ledgerErr
	}
	c.stderrTask = c.l.spawn(c.l.ctx, func(ctx context.Context) { c.drainStderr(ctx, p) })
	c.readerTask = c.l.spawn(c.l.ctx, func(ctx context.Context) { c.readerLoop(ctx, p) })
	return nil
}

// Forks the server with its three pipes, the reaper, the writer and the stdin close watcher
func startServerProc(argv []string, env map[string]string, cwd string) (*serverProc, error) {
	path, err := lookPathIn(argv[0], env, cwd)
	if err != nil {
		return nil, pyOSError(err, argv[0])
	}
	inR, inW, err := stdinPipe()
	if err != nil {
		return nil, pyOSError(err, "")
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		inR.Close()
		inW.Close()
		return nil, pyOSError(err, "")
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		inR.Close()
		inW.Close()
		outR.Close()
		outW.Close()
		return nil, pyOSError(err, "")
	}
	cmd := &exec.Cmd{Path: path, Args: argv, Env: environList(env), Dir: cwd, Stdin: inR, Stdout: outW, Stderr: errW}
	newSession(cmd)
	startErr := cmd.Start()
	inR.Close()
	outW.Close()
	errW.Close()
	if startErr != nil {
		inW.Close()
		outR.Close()
		errR.Close()
		var pe *os.PathError
		if errors.As(startErr, &pe) {
			return nil, pyOSError(pe.Err, argv[0])
		}
		return nil, pyOSError(startErr, argv[0])
	}
	p := &serverProc{cmd: cmd, pid: cmd.Process.Pid, stdin: inW, stdout: outR, stderr: errR,
		reader: bufio.NewReaderSize(outR, 64*1024), exited: make(chan struct{}), out: newOutbox(), writer: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		p.code = exitCodeOf(cmd.ProcessState)
		if exitClosesStdin {
			p.closing.Store(true)
		}
		close(p.exited)
	}()
	go func() {
		defer close(p.writer)
		p.out.drain(inW, func() { p.closing.Store(true) })
	}()
	p.unwatch = watchPipeClose(inW, func() { p.closing.Store(true) })
	return p, nil
}

// Keeps reading server stderr so the pipe never fills, remembering the last lines for failure reports
func (c *Client) drainStderr(ctx context.Context, p *serverProc) {
	r := bufio.NewReaderSize(p.stderr, 64*1024)
	for {
		var line []byte
		var overrun bool
		var err error
		c.l.unlocked(func() { line, overrun, err = readStderrLine(r) })
		if overrun {
			c.logf(slog.LevelWarn, "[%s] stderr: line exceeded stream limit, discarding", c.ServerID)
			continue
		}
		if len(line) == 0 && err != nil {
			return
		}
		text := strings.TrimRightFunc(decodeUTF8Replace(line), pystr.IsSpace)
		if text != "" {
			text = runePrefix(text, 1000)
			c.logf(slog.LevelDebug, "[%s] stderr: %s", c.ServerID, text)
			c.stderrTail = append(c.stderrTail, text)
			if n := len(c.stderrTail); n > stderrTailLines {
				c.stderrTail = c.stderrTail[n-stderrTailLines:]
			}
		}
		if err != nil {
			return
		}
	}
}

// One stderr line up to the stream limit, overrun true when the limit cleared the buffer
func readStderrLine(r *bufio.Reader) ([]byte, bool, error) {
	var line []byte
	for {
		chunk, err := r.ReadSlice('\n')
		line = append(line, chunk...)
		if len(line) > streamLimit {
			return nil, true, nil
		}
		if err == nil {
			return line, false, nil
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			return line, false, err
		}
	}
}

// Human rendering of the server exit status, empty while the process lives
func (c *Client) describeExit() string {
	var code int
	switch {
	case c.exitCode != nil:
		code = *c.exitCode
	case c.proc != nil && c.proc.returncode() != nil:
		code = *c.proc.returncode()
	default:
		return ""
	}
	if code < 0 {
		return fmt.Sprintf("server exited on signal %d", -code)
	}
	return fmt.Sprintf("server exited with code %d", code)
}

// Exit status and the last stderr lines of a failed spawn or initialize
func (c *Client) failureDetails() string {
	var parts []string
	if d := c.describeExit(); d != "" {
		parts = append(parts, d)
	}
	if len(c.stderrTail) > 0 {
		parts = append(parts, "last stderr lines: "+strings.Join(c.stderrTail, " | "))
	}
	return strings.Join(parts, "; ")
}

// Routes one server message, an error ending the reader as an escaping exception would
func (c *Client) dispatch(msg any) error {
	kind, key := ClassifyMessage(msg)
	switch kind {
	case KindResponse:
		return c.dispatchResponse(key, msg.(map[string]any))
	case KindRequest:
		m := msg.(map[string]any)
		c.l.spawn(c.l.ctx, func(ctx context.Context) { c.dispatchRequest(ctx, key, m) })
	case KindNotification:
		c.dispatchNotification(key, msg.(map[string]any))
	default:
		c.logf(slog.LevelWarn, "[%s] dropping invalid message: %s", c.ServerID, pyrepr.Repr(msg))
	}
	return nil
}

// Dispatches server messages until EOF or a framing error, then retires the client on an unexpected close
func (c *Client) readerLoop(ctx context.Context, p *serverProc) {
	decode := c.rt.jsonDecoder()
	for {
		var msg any
		var ok bool
		var err error
		c.l.unlocked(func() { msg, ok, err = ReadMessage(p.reader, decode) })
		if err != nil {
			var pe *ProtocolError
			if errors.As(err, &pe) && ctx.Err() == nil {
				c.logf(slog.LevelWarn, "[%s] protocol error in reader loop: %s", c.ServerID, err)
			}
			break
		}
		if !ok {
			c.logf(slog.LevelDebug, "[%s] server closed stdout cleanly", c.ServerID)
			break
		}
		if c.dispatch(msg) != nil {
			break
		}
	}
	unexpected := !c.stopping && liveState(c.state)
	if unexpected {
		c.state = "error"
	}
	for _, fut := range c.pending {
		if !fut.isDone() {
			fut.setError(protocolErrorf("server connection closed"))
		}
	}
	c.pending = map[int64]*future[any]{}
	if unexpected {
		c.cleanupProcess(context.WithoutCancel(ctx), c.readerTask)
	}
}

func (c *Client) workspaceFolderList() ([]any, error) {
	out := make([]any, len(c.workspaceFolders))
	for i, r := range c.workspaceFolders {
		f, err := folder(r)
		if err != nil {
			return nil, err
		}
		out[i] = f
	}
	return out, nil
}

// Attaches another root to a running multi root server, recording it before announcing it
func (c *Client) addWorkspaceFolder(ctx context.Context, root string) error {
	for _, f := range c.workspaceFolders {
		if f == root {
			return nil
		}
	}
	c.workspaceFolders = append(c.workspaceFolders, root)
	f, err := folder(root)
	if err != nil {
		return err
	}
	return clientSendNotification(c, ctx, "workspace/didChangeWorkspaceFolders",
		map[string]any{"event": map[string]any{"added": []any{f}, "removed": []any{}}})
}

// Detaches a root from a running multi root server so it keeps serving its siblings
func (c *Client) removeWorkspaceFolder(ctx context.Context, root string) error {
	idx := -1
	for i, f := range c.workspaceFolders {
		if f == root {
			idx = i
			break
		}
	}
	if idx < 0 {
		return nil
	}
	c.workspaceFolders = append(c.workspaceFolders[:idx:idx], c.workspaceFolders[idx+1:]...)
	f, err := folder(root)
	if err != nil {
		return err
	}
	return clientSendNotification(c, ctx, "workspace/didChangeWorkspaceFolders",
		map[string]any{"event": map[string]any{"added": []any{}, "removed": []any{f}}})
}

// Sends initialize, records the sync kind, then initialized and the configuration push
func (c *Client) initialize(ctx context.Context) error {
	rootURI, err := FileURI(c.WorkspaceRoot)
	if err != nil {
		return err
	}
	folders, err := c.workspaceFolderList()
	if err != nil {
		return err
	}
	params := map[string]any{
		"rootUri": rootURI, "rootPath": c.WorkspaceRoot, "processId": os.Getpid(),
		"workspaceFolders": folders, "initializationOptions": c.initOptions, "capabilities": clientCapabilities,
	}
	result, err := waitFor(ctx, InitializeTimeout, func(ctx context.Context) (any, error) {
		return c.sendRequest(ctx, "initialize", params)
	})
	if err != nil {
		return err
	}
	resultMap, ok := result.(map[string]any)
	if !ok {
		return attributeError(result)
	}
	caps := pyOr(resultMap["capabilities"], map[string]any{})
	capsMap, ok := caps.(map[string]any)
	if !ok {
		return attributeError(caps)
	}
	sync := capsMap["textDocumentSync"]
	if m, ok := sync.(map[string]any); ok {
		sync = m["change"]
	}
	c.syncKind = 1
	if n, ok := pyExactInt(sync); ok {
		c.syncKind = int(n)
	}
	if err := clientSendNotification(c, ctx, "initialized", map[string]any{}); err != nil {
		return err
	}
	if len(c.initOptions) > 0 {
		return clientSendNotification(c, ctx, "workspace/didChangeConfiguration", map[string]any{"settings": c.initOptions})
	}
	return nil
}

// AttributeError a get call on a non dict raises
func attributeError(v any) error {
	return &pyExc{typ: "AttributeError", text: fmt.Sprintf("'%s' object has no attribute 'get'", pyrepr.TypeName(v))}
}

// Graceful shutdown and exit, a grace period for the server to honour exit, then the hard kill
func (c *Client) shutdown(ctx context.Context) error {
	if c.stopping {
		return nil
	}
	c.stopping = true
	var err error
	if c.isRunning() {
		_, rerr := waitFor(ctx, 2*time.Second, func(ctx context.Context) (any, error) {
			return c.sendRequest(ctx, "shutdown", nil)
		})
		if rerr != nil && !isLSPFailure(rerr) {
			err = rerr
		}
		if err == nil {
			if nerr := clientSendNotification(c, ctx, "exit", nil); nerr != nil && isCancellation(nerr) {
				err = nerr
			}
		}
		if err == nil {
			if p := c.proc; p != nil && p.returncode() == nil {
				if _, werr := c.l.awaitAny(ctx, shutdownGrace, p.exited); werr != nil {
					err = werr
				}
			}
		}
	}
	c.state = "stopped"
	c.cleanupProcess(context.WithoutCancel(ctx), nil)
	return err
}

// Whether an error is a timeout, a request error or a protocol error rather than a cancellation
func isLSPFailure(err error) bool {
	var pe *ProtocolError
	var re *RequestError
	var te *TimeoutError
	return errors.As(err, &pe) || errors.As(err, &re) || errors.As(err, &te)
}

// Cancels the pipe tasks and reaps or hard kills the process tree, releasing its group
func (c *Client) cleanupProcess(ctx context.Context, current *task) {
	c.l.unlocked(func() { c.cleanupSem <- struct{}{} })
	defer func() { <-c.cleanupSem }()
	tasks := []*task{c.readerTask, c.stderrTask}
	c.readerTask, c.stderrTask = nil, nil
	p := c.proc
	c.proc = nil
	var live []*task
	for _, t := range tasks {
		if t != nil && t != current && !t.finished() {
			t.cancel()
			live = append(live, t)
		}
	}
	if p != nil {
		p.stdout.Close()
		p.stderr.Close()
	}
	for _, t := range live {
		c.l.await(ctx, t.done)
	}
	if p == nil {
		return
	}
	defer c.closeStdin(p)
	if rc := p.returncode(); rc != nil {
		if c.exitCode == nil {
			c.exitCode = rc
		}
		c.releaseGroup(p.pid)
		return
	}
	c.l.unlocked(func() { killServerTree(p) })
	c.l.awaitAny(ctx, shutdownGrace, p.exited)
	if rc := p.returncode(); c.exitCode == nil && rc != nil {
		c.exitCode = rc
	}
	c.releaseGroup(p.pid)
}

// Stops the watcher, closes the stdin pipe so a parked write fails, then joins the writer
func (c *Client) closeStdin(p *serverProc) {
	p.out.close()
	p.closing.Store(true)
	c.l.unlocked(func() {
		p.unwatch()
		p.stdin.Close()
		<-p.writer
	})
}

// Hands the server's process group back once it is empty
func (c *Client) releaseGroup(pid int) {
	c.l.unlocked(func() { c.rt.ReleaseProcessGroupOnceEmpty(pid, nil, 0) })
}

// Buffers an envelope behind every earlier one, waiting only while the buffer is above its high water mark
func (c *Client) write(ctx context.Context, msg map[string]any) error {
	data, err := EncodeMessage(msg)
	if err != nil {
		return err
	}
	p := c.proc
	paused, resumed := p.out.put(data)
	if p.closing.Load() || resumed == nil {
		if err := c.l.sleep(ctx, 0); err != nil {
			return err
		}
		return errConnectionLost
	}
	for paused {
		if err := c.l.await(ctx, resumed); err != nil {
			return err
		}
		p.out.mu.Lock()
		resumed = p.out.resumed
		p.out.mu.Unlock()
		paused = p.out.stillPaused()
	}
	if p.closing.Load() {
		return errConnectionLost
	}
	return nil
}

// ConnectionResetError a drain raises once the pipe is gone
var errConnectionLost = &pyExc{typ: "ConnectionResetError", text: "Connection lost"}

func (c *Client) requireOpen(method string) error {
	if !c.connectionIsOpen() {
		return protocolErrorf("cannot send %s: server connection closed", pyrepr.StrRepr(method))
	}
	return nil
}

// Python text of a pipe write error
func writeErrorText(err error) string { return errText(err) }

// Sends a request and waits for its response, a cancellation dropping the pending entry
func (c *Client) sendRequest(ctx context.Context, method string, params any) (any, error) {
	if err := c.requireOpen(method); err != nil {
		return nil, err
	}
	id := c.nextID
	c.nextID++
	fut := newFuture[any]()
	c.pending[id] = fut
	if err := c.write(ctx, MakeRequest(id, method, params)); err != nil {
		delete(c.pending, id)
		if isCancellation(err) {
			return nil, err
		}
		return nil, protocolErrorf("send failed for %s: %s", pyrepr.StrRepr(method), writeErrorText(err))
	}
	defer delete(c.pending, id)
	if err := c.l.await(ctx, fut.done); err != nil {
		return nil, err
	}
	return fut.value, fut.err
}

// Sends a request retrying ContentModified with exponential backoff, other errors propagating
func (c *Client) sendRequestWithRetry(ctx context.Context, method string, params any, timeout time.Duration) (any, error) {
	for attempt := 0; ; attempt++ {
		v, err := waitFor(ctx, timeout, func(ctx context.Context) (any, error) { return c.sendRequest(ctx, method, params) })
		var re *RequestError
		if err == nil || !errors.As(err, &re) || re.Code != ErrorContentModified || attempt >= maxContentModifiedRetries {
			return v, err
		}
		if err := c.l.sleep(ctx, retryBaseDelay*time.Duration(1<<attempt)); err != nil {
			return nil, err
		}
	}
}

// Sends a notification through the client, swapped by tests that observe the wire
var clientSendNotification = (*Client).sendNotification

// Sends a notification, write failures logged and swallowed
func (c *Client) sendNotification(ctx context.Context, method string, params any) error {
	if err := c.requireOpen(method); err != nil {
		return err
	}
	if err := c.write(ctx, MakeNotification(method, params)); err != nil {
		if isCancellation(err) {
			return err
		}
		c.logf(slog.LevelDebug, "[%s] notify %s failed: %s", c.ServerID, method, writeErrorText(err))
	}
	return nil
}

// Answers a server request, silently doing nothing when the pipe is gone
func (c *Client) sendReply(ctx context.Context, msg map[string]any) {
	if p := c.proc; p != nil && p.stdin != nil && !p.closing.Load() {
		c.write(ctx, msg)
	}
}

// Resolves the pending request a response answers
func (c *Client) dispatchResponse(id any, msg map[string]any) error {
	key, ok := pendingKey(id)
	if !ok {
		switch id.(type) {
		case []any, map[string]any:
			return &pyExc{typ: "TypeError", text: fmt.Sprintf("unhashable type: '%s'", pyrepr.TypeName(id))}
		}
		return nil
	}
	fut := c.pending[key]
	if fut == nil || fut.isDone() {
		return nil
	}
	errObj, hasErr := msg["error"]
	if !hasErr {
		fut.setResult(msg["result"])
		return nil
	}
	e := pyOr(errObj, map[string]any{})
	em, ok := e.(map[string]any)
	if !ok {
		return attributeError(e)
	}
	code, err := pyInt(dgetOr(em, "code", int64(-32000)))
	if err != nil {
		return err
	}
	fut.setError(NewRequestError(int(code), pyrepr.Str(dgetOr(em, "message", "unknown")), em["data"]))
	return nil
}

// RelatedDocuments is the relatedDocuments object of a pull response with its uris in server order
type RelatedDocuments struct {
	URIs    []string
	Reports map[string]any
}

// Pending map key of a response id, numbers comparing as Python dict keys do
func pendingKey(id any) (int64, bool) {
	if n, ok := pyExactInt(id); ok {
		return n, true
	}
	if f, ok := id.(float64); ok && f == float64(int64(f)) {
		return int64(f), true
	}
	return 0, false
}

// Answers one server request, unknown methods with method not found
func (c *Client) dispatchRequest(ctx context.Context, id any, msg map[string]any) {
	var method any = ""
	if m, ok := msg["method"]; ok {
		method = m
	}
	switch method.(type) {
	case []any, map[string]any:
		return
	}
	params := msg["params"]
	var reply map[string]any
	switch name, _ := method.(string); name {
	case "window/workDoneProgress/create", "client/registerCapability", "client/unregisterCapability", "workspace/diagnostic/refresh":
		reply = MakeResponse(id, nil)
	case "workspace/workspaceFolders":
		folders, err := c.workspaceFolderList()
		if err != nil {
			c.logf(slog.LevelWarn, "[%s] request handler %s failed: %s", c.ServerID, name, errText(err))
			reply = MakeErrorResponse(id, -32000, "handler failed: "+errText(err), nil)
		} else {
			reply = MakeResponse(id, folders)
		}
	case "workspace/configuration":
		result, err := c.handleWorkspaceConfiguration(params)
		if err != nil {
			c.logf(slog.LevelWarn, "[%s] request handler %s failed: %s", c.ServerID, name, err)
			reply = MakeErrorResponse(id, -32000, "handler failed: "+err.Error(), nil)
		} else {
			reply = MakeResponse(id, result)
		}
	default:
		reply = MakeErrorResponse(id, ErrorMethodNotFound, "method not found: "+pyrepr.Str(method), nil)
	}
	c.sendReply(ctx, reply)
}

func (c *Client) dispatchNotification(method any, msg map[string]any) {
	if s, ok := method.(string); ok && s == "textDocument/publishDiagnostics" {
		c.handlePublishDiagnostics(msg["params"])
	}
}

// Walks dotted section paths through the initialization options, null when missing
func (c *Client) handleWorkspaceConfiguration(params any) (any, error) {
	p, ok := params.(map[string]any)
	if !ok {
		return []any{nil}, nil
	}
	var items []any
	switch t := pyOr(p["items"], []any{}).(type) {
	case []any:
		items = t
	case map[string]any:
		items = make([]any, len(t))
	case string:
		items = make([]any, utf8.RuneCountInString(t))
	default:
		return nil, fmt.Errorf("'%s' object is not iterable", pyrepr.TypeName(t))
	}
	out := make([]any, 0, len(items))
	for _, item := range items {
		out = append(out, c.configSection(item))
	}
	return out, nil
}

func (c *Client) configSection(item any) any {
	m, ok := item.(map[string]any)
	if !ok {
		return nil
	}
	section := m["section"]
	if !pyrepr.Truthy(section) || len(c.initOptions) == 0 {
		if len(c.initOptions) == 0 {
			return nil
		}
		return c.initOptions
	}
	var cur any = c.initOptions
	for _, part := range strings.Split(pyrepr.Str(section), ".") {
		cm, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		next, has := cm[part]
		if !has {
			return nil
		}
		cur = next
	}
	return cur
}

// Diagnostic list of a decoded JSON list, nil maps standing in for entries that are not dicts
func diagList(v any) []map[string]any {
	list, ok := v.([]any)
	if !ok {
		return []map[string]any{}
	}
	out := make([]map[string]any, len(list))
	for i, e := range list {
		if m, ok := e.(map[string]any); ok {
			out[i] = m
		}
	}
	return out
}

// Stores a push tagged with its echoed version or the current one, the seed push untagged
func (c *Client) handlePublishDiagnostics(params any) {
	p, ok := params.(map[string]any)
	if !ok {
		return
	}
	uri, ok := p["uri"].(string)
	if !ok {
		return
	}
	diagnostics := pyOr(p["diagnostics"], []any{})
	doc := c.docs.setdefault(URIToPath(uri), func() *docState { return newDocState(-1, "") })
	isSeed := c.seedFirstPush && !doc.seedSeen
	doc.seedSeen = true
	doc.push = diagList(diagnostics)
	if isSeed {
		return
	}
	if n, ok := pyExactInt(p["version"]); ok {
		doc.pushVersion = int(n)
	} else {
		doc.pushVersion = doc.version
	}
	c.pushCounter++
	c.pushEvent.fire()
}

// Sends didOpen the first time and didChange after, returning the new document version
func (c *Client) openFile(ctx context.Context, path, languageID string, text *string) (int, error) {
	if !c.isRunning() {
		return 0, protocolErrorf("client not running")
	}
	absPath, err := pypath.Abspath(path)
	if err != nil {
		return 0, err
	}
	var content string
	if text == nil {
		raw, err := os.ReadFile(absPath)
		if err != nil {
			var pe *os.PathError
			if errors.As(err, &pe) {
				return 0, protocolErrorf("cannot read %s: %s", absPath, pyOSError(pe.Err, pe.Path).text)
			}
			return 0, protocolErrorf("cannot read %s: %s", absPath, err)
		}
		content = AsDiskRead(string(raw))
	} else {
		content = AsDiskRead(*text)
	}
	uri, err := FileURI(absPath)
	if err != nil {
		return 0, err
	}
	doc := c.docs.get(absPath)
	if doc != nil && doc.version < 0 {
		doc = nil
	}
	changeType := 2
	if doc == nil {
		changeType = 1
	}
	if err := clientSendNotification(c, ctx, "workspace/didChangeWatchedFiles",
		map[string]any{"changes": []any{map[string]any{"uri": uri, "type": changeType}}}); err != nil {
		return 0, err
	}
	if doc == nil {
		c.docs.pop(absPath)
		c.docs.set(absPath, newDocState(0, content))
		if err := clientSendNotification(c, ctx, "textDocument/didOpen", map[string]any{"textDocument": map[string]any{
			"uri": uri, "languageId": languageID, "version": 0, "text": content}}); err != nil {
			return 0, err
		}
		if err := c.evictLRUDocs(ctx); err != nil {
			return 0, err
		}
		return 0, nil
	}
	c.docs.set(absPath, c.docs.pop(absPath))
	change := map[string]any{"text": content}
	if c.syncKind == 2 {
		change["range"] = map[string]any{"start": map[string]any{"line": 0, "character": 0}, "end": endPosition(doc.text)}
	}
	newVersion := doc.version + 1
	doc.version, doc.text = newVersion, content
	if err := clientSendNotification(c, ctx, "textDocument/didChange", map[string]any{
		"textDocument": map[string]any{"uri": uri, "version": newVersion}, "contentChanges": []any{change}}); err != nil {
		return 0, err
	}
	return newVersion, nil
}

// Drops the least recently touched documents past the cap, closing those the server has open
func (c *Client) evictLRUDocs(ctx context.Context) error {
	for c.docs.len() > maxTrackedFiles {
		oldPath, old := c.docs.oldest()
		c.docs.pop(oldPath)
		if old.version >= 0 {
			uri, err := FileURI(oldPath)
			if err != nil {
				return err
			}
			if err := clientSendNotification(c, ctx, "textDocument/didClose",
				map[string]any{"textDocument": map[string]any{"uri": uri}}); err != nil {
				return err
			}
		}
	}
	return nil
}

// Sends didSave, which some linters need before they re-scan
func (c *Client) saveFile(ctx context.Context, path string) error {
	if !c.isRunning() {
		return nil
	}
	absPath, err := pypath.Abspath(path)
	if err != nil {
		return err
	}
	uri, err := FileURI(absPath)
	if err != nil {
		return err
	}
	return clientSendNotification(c, ctx, "textDocument/didSave",
		map[string]any{"textDocument": map[string]any{"uri": uri}})
}

// Pulls document diagnostics into the pull store tagged with the version at send time
func (c *Client) pullDocumentDiagnostics(ctx context.Context, path string) error {
	absPath, err := pypath.Abspath(path)
	if err != nil {
		return err
	}
	sentVersion := -1
	if doc := c.docs.get(absPath); doc != nil {
		sentVersion = doc.version
	}
	uri, err := FileURI(absPath)
	if err != nil {
		return err
	}
	result, err := c.sendRequestWithRetry(ctx, "textDocument/diagnostic",
		map[string]any{"textDocument": map[string]any{"uri": uri}}, diagnosticsRequestTimeout)
	if err != nil {
		if !isLSPFailure(err) {
			return err
		}
		var re *RequestError
		if errors.As(err, &re) && re.Code == ErrorMethodNotFound {
			c.pullOK = false
		}
		c.logf(slog.LevelDebug, "[%s] document diagnostic pull failed: %s", c.ServerID, err)
		return nil
	}
	res, ok := result.(map[string]any)
	if !ok {
		return nil
	}
	type report struct {
		path string
		body any
		tag  *int
	}
	reports := []report{{absPath, res, &sentVersion}}
	switch related := res["relatedDocuments"].(type) {
	case RelatedDocuments:
		for _, u := range related.URIs {
			reports = append(reports, report{URIToPath(u), related.Reports[u], nil})
		}
	case map[string]any:
		uris := make([]string, 0, len(related))
		for u := range related {
			uris = append(uris, u)
		}
		sort.Strings(uris)
		for _, u := range uris {
			reports = append(reports, report{URIToPath(u), related[u], nil})
		}
	}
	for _, r := range reports {
		items, ok := dget(r.body, "items").([]any)
		if !ok {
			continue
		}
		d := c.docs.setdefault(r.path, func() *docState { return newDocState(-1, "") })
		d.pull = diagList(items)
		if r.tag == nil {
			d.pullVersion = d.version
		} else {
			d.pullVersion = *r.tag
		}
	}
	return nil
}

// Diagnostics wait swapped by tests that record the budget each wait receives
var clientWaitForDiagnostics = (*Client).waitForDiagnostics

// Waits for fresh diagnostics at a version, false meaning no data rather than no errors
func (c *Client) waitForDiagnostics(ctx context.Context, path string, version int, mode string, timeout time.Duration) (bool, error) {
	if timeout <= 0 {
		timeout = DiagnosticsDocumentWait
		if mode == "full" {
			timeout = DiagnosticsFullWait
		}
	}
	deadline := time.Now().Add(timeout)
	absPath, err := pypath.Abspath(path)
	if err != nil {
		return false, err
	}
	var pull, push *task
	if c.pullOK {
		pull = c.l.spawn(ctx, func(ctx context.Context) { c.pullDocumentDiagnostics(ctx, absPath) })
	}
	push = c.l.spawn(ctx, func(ctx context.Context) { c.waitForFreshPush(ctx, absPath, version, timeout) })
	defer func() {
		for _, t := range []*task{pull, push} {
			if t != nil && !t.finished() {
				t.cancel()
				c.l.await(context.WithoutCancel(ctx), t.done)
			}
		}
	}()
	for {
		if !c.connectionIsOpen() {
			return false, protocolErrorf("server connection closed while waiting for diagnostics")
		}
		if doc := c.docs.get(absPath); doc != nil && doc.freshAt(version) {
			return true, nil
		}
		remaining := time.Until(deadline)
		var live []<-chan struct{}
		for _, t := range []*task{pull, push} {
			if t != nil && !t.finished() {
				live = append(live, t.done)
			}
		}
		if remaining <= 0 || len(live) == 0 {
			return false, nil
		}
		if _, err := c.l.awaitAny(ctx, remaining, live...); err != nil {
			return false, err
		}
	}
}

// Blocks until the next push or the timeout, true when a push woke the waiter
func (c *Client) awaitPush(ctx context.Context, timeout time.Duration) (bool, error) {
	c.pushEvent.clear()
	return c.l.awaitAny(ctx, timeout, c.pushEvent.ch)
}

// Waits for a fresh push at the version, then debounces the paired push some servers send
func (c *Client) waitForFreshPush(ctx context.Context, path string, version int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	baseline := c.pushCounter
	for {
		if doc := c.docs.get(path); doc != nil && doc.freshPushAt(version) {
			debounceBaseline := c.pushCounter
			debounceDeadline := time.Now().Add(pushDebounce)
			for c.pushCounter == debounceBaseline {
				remaining := time.Until(debounceDeadline)
				if remaining <= 0 {
					break
				}
				woke, err := c.awaitPush(ctx, remaining)
				if err != nil {
					return err
				}
				if !woke {
					break
				}
			}
			return nil
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil
		}
		if c.pushCounter > baseline {
			baseline = c.pushCounter
			continue
		}
		if _, err := c.awaitPush(ctx, min(remaining, 500*time.Millisecond)); err != nil {
			return err
		}
	}
}

// Waits for the seed push of a document on a seeding server and returns what it carried
func (c *Client) waitForSeed(ctx context.Context, absPath string, timeout time.Duration) ([]map[string]any, bool, error) {
	deadline := time.Now().Add(timeout)
	for {
		if doc := c.docs.get(absPath); doc != nil && doc.seedSeen {
			return append([]map[string]any{}, doc.push...), true, nil
		}
		remaining := time.Until(deadline)
		if remaining <= 0 || !c.connectionIsOpen() {
			return nil, false, nil
		}
		if _, err := c.awaitPush(ctx, min(remaining, 500*time.Millisecond)); err != nil {
			return nil, false, err
		}
	}
}

// Whether the server's first push per document is a seed
func (c *Client) seedsFirstPush() bool { return c.seedFirstPush }

// Text last synced for a path, false when never opened
func (c *Client) documentText(path string) (string, bool, error) {
	absPath, err := pypath.Abspath(path)
	if err != nil {
		return "", false, err
	}
	if doc := c.docs.get(absPath); doc != nil && doc.version >= 0 {
		return doc.text, true, nil
	}
	return "", false, nil
}

// Merged and deduped push and pull diagnostics, stores counting only once fresh when asked
func (c *Client) diagnosticsFor(path string, freshOnly bool) ([]map[string]any, error) {
	absPath, err := pypath.Abspath(path)
	if err != nil {
		return nil, err
	}
	doc := c.docs.get(absPath)
	if doc == nil {
		return []map[string]any{}, nil
	}
	var push, pull []map[string]any
	if !freshOnly || doc.freshPushAt(doc.version) {
		push = doc.push
	}
	if !freshOnly || doc.freshPullAt(doc.version) {
		pull = doc.pull
	}
	return dedupe(push, pull)
}

// Content deduplicated union of diagnostic lists, entries that are not dicts dropped
func dedupe(lists ...[]map[string]any) ([]map[string]any, error) {
	seen := map[string]bool{}
	out := []map[string]any{}
	for _, lst := range lists {
		for _, d := range lst {
			if d == nil {
				continue
			}
			key, err := DiagnosticKey(d)
			if err != nil {
				return nil, err
			}
			if !seen[key] {
				seen[key] = true
				out = append(out, d)
			}
		}
	}
	return out, nil
}

// Dict value of x or an empty dict, the AttributeError a later get raises for any other truthy value
func dictOr(v any) (map[string]any, error) {
	v = pyOr(v, map[string]any{})
	m, ok := v.(map[string]any)
	if !ok {
		return nil, attributeError(v)
	}
	return m, nil
}

// DiagnosticKey is the content equality key of severity, code, source, message and range
func DiagnosticKey(d map[string]any) (string, error) {
	rng, err := dictOr(d["range"])
	if err != nil {
		return "", err
	}
	start, err := dictOr(rng["start"])
	if err != nil {
		return "", err
	}
	end, err := dictOr(rng["end"])
	if err != nil {
		return "", err
	}
	code := ""
	if v, ok := d["code"]; ok && v != nil {
		code = pyrepr.Str(v)
	}
	return strings.Join([]string{
		pyrepr.Str(pyOr(d["severity"], int64(1))), code, pyrepr.Str(pyOr(d["source"], "")),
		pystr.Strip(pyrepr.Str(pyOr(d["message"], ""))),
		pyrepr.Str(dgetOr(start, "line", int64(0))) + ":" + pyrepr.Str(dgetOr(start, "character", int64(0))) + "-" +
			pyrepr.Str(dgetOr(end, "line", int64(0))) + ":" + pyrepr.Str(dgetOr(end, "character", int64(0))),
	}, "\x00"), nil
}
