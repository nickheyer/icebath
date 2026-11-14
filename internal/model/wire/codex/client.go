// Codex app-server sessions over newline-delimited JSON-RPC on the child's standard streams
package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/internal/model/wire"
	"github.com/nickheyer/icebath/internal/proc"
)

const (
	transportLostCode = -32000
	stderrKeep        = 500
	lineLimit         = 64 << 20
)

var minVersion = [3]int{0, 125, 0}

var (
	ErrTransportLost  = errors.New("codex app-server transport is gone")
	ErrRequestTimeout = errors.New("codex app-server request timed out")
	ErrBinaryMissing  = errors.New("codex CLI not found")
	ErrVersion        = errors.New("codex CLI version is unusable")
	ErrEnvRequired    = errors.New("codex: the child environment must be given, filtered of icebath's own secrets")
	versionRE         = regexp.MustCompile(`(\d+)\.(\d+)\.(\d+)`)
)

type RPCError struct {
	Code    int
	Message string
	Data    json.RawMessage
	lost    bool
}

func (e *RPCError) Error() string {
	if e.lost {
		return e.Message
	}
	return fmt.Sprintf("codex app-server error %d: %s", e.Code, e.Message)
}

func (e *RPCError) Is(target error) bool { return target == ErrTransportLost && e.lost }

func lostError(message string) *RPCError {
	return &RPCError{Code: transportLostCode, Message: message, lost: true}
}

type queue struct {
	mu    sync.Mutex
	items []wire.Value
	ready chan struct{}
}

func newQueue() *queue { return &queue{ready: make(chan struct{}, 1)} }

func (q *queue) put(v wire.Value) {
	q.mu.Lock()
	q.items = append(q.items, v)
	q.mu.Unlock()
	select {
	case q.ready <- struct{}{}:
	default:
	}
}

func (q *queue) pop() (wire.Value, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.items) == 0 {
		return wire.Value{}, false
	}
	v := q.items[0]
	q.items = q.items[1:]
	return v, true
}

func (q *queue) take(ctx context.Context, timeout time.Duration) (wire.Value, bool) {
	if v, ok := q.pop(); ok || timeout <= 0 {
		return v, ok
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case <-q.ready:
			if v, ok := q.pop(); ok {
				return v, true
			}
		case <-timer.C:
			return q.pop()
		case <-ctx.Done():
			return wire.Value{}, false
		}
	}
}

type client struct {
	tree     *proc.Tree
	pid      int
	stdin    io.WriteCloser
	writeMu  sync.Mutex
	mu       sync.Mutex
	nextID   int64
	pending  map[int64]chan wire.Value
	closed   bool
	notes    *queue
	requests *queue
	stderrMu sync.Mutex
	stderr   []string
	errDone  chan struct{}
	exited   chan struct{}
}

func startClient(bin, home string, extraArgs, env []string) (*client, error) {
	if bin == "" {
		bin = "codex"
	}
	cmd := exec.Command(bin, append([]string{"app-server"}, extraArgs...)...)
	if env == nil {
		return nil, ErrEnvRequired
	}
	spawnEnv := append([]string(nil), env...)
	hasRustLog := false
	for _, kv := range spawnEnv {
		if strings.HasPrefix(kv, "RUST_LOG=") {
			hasRustLog = true
		}
	}
	if home != "" {
		spawnEnv = append(spawnEnv, "CODEX_HOME="+home)
	}
	if !hasRustLog {
		spawnEnv = append(spawnEnv, "RUST_LOG=warn")
	}
	cmd.Env = spawnEnv
	proc.Hide(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	tree, err := proc.StartTree(cmd)
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w at %q, install with: npm i -g @openai/codex", ErrBinaryMissing, bin)
		}
		return nil, err
	}
	c := &client{tree: tree, pid: cmd.Process.Pid, stdin: stdin, nextID: 1, pending: map[int64]chan wire.Value{},
		notes: newQueue(), requests: newQueue(), errDone: make(chan struct{}), exited: make(chan struct{})}
	outDone := make(chan struct{})
	go c.readStdout(stdout, outDone)
	go c.readStderr(stderr)
	go func() {
		<-outDone
		<-c.errDone
		_ = tree.Wait()
		close(c.exited)
	}()
	return c, nil
}

func (c *client) appendStderr(line string) {
	c.stderrMu.Lock()
	defer c.stderrMu.Unlock()
	c.stderr = append(c.stderr, line)
	if len(c.stderr) > stderrKeep {
		c.stderr = append([]string(nil), c.stderr[len(c.stderr)-stderrKeep:]...)
	}
}

func (c *client) stderrTail(n int) []string {
	if !c.alive() {
		select {
		case <-c.errDone:
		case <-time.After(time.Second):
		}
	}
	c.stderrMu.Lock()
	defer c.stderrMu.Unlock()
	if n > len(c.stderr) {
		n = len(c.stderr)
	}
	return append([]string(nil), c.stderr[len(c.stderr)-n:]...)
}

func (c *client) alive() bool {
	select {
	case <-c.exited:
		return false
	default:
		return true
	}
}

func (c *client) failPending(reason string) {
	c.mu.Lock()
	pending := c.pending
	c.pending = map[int64]chan wire.Value{}
	c.mu.Unlock()
	if len(pending) == 0 {
		return
	}
	inner := (&wire.Dict{}).SetInt("code", transportLostCode).SetString("message", reason)
	synthetic := (&wire.Dict{}).Set("error", inner.JSON()).SetBool("transportLost", true).Value()
	for _, ch := range pending {
		ch <- synthetic
	}
}

func (c *client) readStdout(r io.Reader, done chan struct{}) {
	defer close(done)
	defer c.failPending("codex app-server stdout closed")
	reader := wire.NewJSONLineReader(r, lineLimit)
	for {
		raw, err := reader.Next(context.Background())
		if err == io.EOF {
			return
		}
		if err != nil {
			var bad *wire.StreamError
			if errors.As(err, &bad) {
				c.appendStderr("<non-json on stdout> " + truncate(bad.Data(), 200))
				continue
			}
			c.appendStderr("<stdout reader error> " + err.Error())
			return
		}
		msg, perr := wire.ParseValue(raw)
		if perr != nil || !msg.IsObject() {
			continue
		}
		c.dispatch(msg)
	}
}

func (c *client) dispatch(msg wire.Value) {
	id := msg.Get("id")
	switch {
	case id.Exists() && (msg.Get("result").Exists() || msg.Get("error").Exists()):
		n, ok := id.Int()
		if !ok {
			return
		}
		c.mu.Lock()
		ch := c.pending[n]
		delete(c.pending, n)
		c.mu.Unlock()
		if ch != nil {
			ch <- msg
		}
	case msg.Get("method").Exists():
		if id.Exists() {
			c.requests.put(msg)
		} else {
			c.notes.put(msg)
		}
	}
}

func (c *client) readStderr(r io.Reader) {
	defer close(c.errDone)
	buf := make([]byte, 0, 4096)
	chunk := make([]byte, 4096)
	for {
		n, err := r.Read(chunk)
		buf = append(buf, chunk[:n]...)
		for {
			i := strings.IndexByte(string(buf), '\n')
			if i < 0 {
				if len(buf) > 1<<20 {
					c.appendStderr(strings.ToValidUTF8(string(buf), "�"))
					buf = buf[:0]
				}
				break
			}
			c.appendStderr(strings.TrimRight(strings.ToValidUTF8(string(buf[:i]), "�"), " \t\r\n"))
			buf = buf[i+1:]
		}
		if err != nil {
			if len(buf) > 0 {
				c.appendStderr(strings.TrimRight(strings.ToValidUTF8(string(buf), "�"), " \t\r\n"))
			}
			return
		}
	}
}

func encodeRPC(d *wire.Dict) []byte {
	text, ok := wire.FormatJSON(d.JSON(), wire.TextLayout{Spaced: true})
	if !ok {
		text = string(d.JSON())
	}
	return append(wire.ScrubSurrogates([]byte(text)), '\n')
}

func (c *client) send(d *wire.Dict) error {
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return lostError("codex app-server client is closed")
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if _, err := c.stdin.Write(encodeRPC(d)); err != nil {
		return lostError("codex app-server stdin closed unexpectedly: " + err.Error())
	}
	return nil
}

func orEmptyObject(params json.RawMessage) json.RawMessage {
	if len(params) == 0 {
		return json.RawMessage("{}")
	}
	return params
}

func (c *client) request(ctx context.Context, method string, params json.RawMessage, timeout time.Duration) (wire.Value, error) {
	ch := make(chan wire.Value, 1)
	c.mu.Lock()
	rid := c.nextID
	c.nextID++
	c.pending[rid] = ch
	c.mu.Unlock()
	drop := func() {
		c.mu.Lock()
		delete(c.pending, rid)
		c.mu.Unlock()
	}
	if err := c.send((&wire.Dict{}).Set("id", wire.Int(rid)).SetString("method", method).Set("params", orEmptyObject(params))); err != nil {
		drop()
		return wire.Value{}, err
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case msg := <-ch:
		if fault := msg.Get("error"); fault.Exists() {
			code, ok := fault.Get("code").Int()
			if !ok {
				code = -1
			}
			out := &RPCError{Code: int(code), Message: fault.Get("message").Text(), lost: msg.Get("transportLost").True()}
			if data := fault.Get("data"); data.Exists() {
				out.Data = data.Raw()
			}
			return wire.Value{}, out
		}
		if result := msg.Get("result"); result.IsObject() {
			return result, nil
		}
		empty, _ := wire.ParseValue([]byte("{}"))
		return empty, nil
	case <-timer.C:
		drop()
		return wire.Value{}, fmt.Errorf("%w: method %q after %s", ErrRequestTimeout, method, timeout)
	case <-ctx.Done():
		drop()
		return wire.Value{}, ctx.Err()
	}
}

func (c *client) notify(method string) error {
	return c.send((&wire.Dict{}).SetString("method", method).Set("params", json.RawMessage("{}")))
}

func (c *client) respond(id json.RawMessage, result json.RawMessage) error {
	return c.send((&wire.Dict{}).Set("id", id).Set("result", result))
}

func (c *client) respondError(id json.RawMessage, code int, message string) error {
	fault := (&wire.Dict{}).SetInt("code", code).SetString("message", message)
	return c.send((&wire.Dict{}).Set("id", id).Set("error", fault.JSON()))
}

func (c *client) initialize(ctx context.Context, name, title, version string) error {
	info := (&wire.Dict{}).SetString("name", name).SetString("title", title).SetString("version", version)
	params := (&wire.Dict{}).Set("clientInfo", info.JSON()).Set("capabilities", json.RawMessage("{}"))
	if _, err := c.request(ctx, "initialize", params.JSON(), 10*time.Second); err != nil {
		return err
	}
	return c.notify("initialized")
}

func (c *client) close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.mu.Unlock()
	c.failPending("codex app-server client is closing")
	var descendants []proc.Identity
	root, rootErr := proc.Identify(c.pid)
	if rootErr == nil {
		if table, err := proc.ReadTable(); err == nil {
			descendants = table.Descendants(root)
		}
	}
	_ = c.stdin.Close()
	if rootErr == nil {
		_ = root.Signal(syscall.SIGTERM)
	}
	select {
	case <-c.exited:
	case <-time.After(3 * time.Second):
		_ = c.tree.Kill()
		select {
		case <-c.exited:
		case <-time.After(time.Second):
		}
	}
	if len(descendants) > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		proc.Stop(ctx, descendants, time.Second)
	}
}

func truncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n])
}

func ParseVersion(output string) ([3]int, bool) {
	m := versionRE.FindStringSubmatch(output)
	if m == nil {
		return [3]int{}, false
	}
	var v [3]int
	for i := range v {
		v[i], _ = strconv.Atoi(m[i+1])
	}
	return v, true
}

func CheckBinary(ctx context.Context, bin string) (string, error) {
	if bin == "" {
		bin = "codex"
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.Command(bin, "--version")
	proc.Hide(cmd)
	res, err := proc.Run(ctx, cmd)
	if err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("%w: codex --version timed out", ErrVersion)
		}
		return "", fmt.Errorf("%w at %q, install with: npm i -g @openai/codex", ErrBinaryMissing, bin)
	}
	if !res.Exit.Success() {
		return "", fmt.Errorf("%w: codex --version exited %d: %s", ErrVersion, res.Exit.Code, strings.TrimSpace(wire.MaskCredentials(string(res.Stderr))))
	}
	version, ok := ParseVersion(string(res.Stdout))
	if !ok {
		return "", fmt.Errorf("%w: could not parse codex version from %q", ErrVersion, truncate(string(res.Stdout), 200))
	}
	have := fmt.Sprintf("%d.%d.%d", version[0], version[1], version[2])
	for i := range version {
		if version[i] != minVersion[i] {
			if version[i] < minVersion[i] {
				return "", fmt.Errorf("%w: codex %s is older than required %d.%d.%d, run: npm i -g @openai/codex", ErrVersion, have, minVersion[0], minVersion[1], minVersion[2])
			}
			break
		}
	}
	return have, nil
}
