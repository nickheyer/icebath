package acpclient

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyshlex"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Launch defaults, labels and vendor hooks a provider customises, the class attributes of hermes subclasses
type Vendor struct {
	DefaultCommand           string
	DefaultArgs              []string
	CommandEnvVars           []string
	ArgsEnvVar               string
	MarkerBaseURL            string
	PlaceholderModel         string
	Label                    string
	MissingCommandHint       string
	AuthRequiredHint         *string
	UnsupportedTransportHint string
	ModelEntryDisabled       func(entry any) bool
	EarlyExitError           func(stderrText string) (string, bool)
}

// Vendor carrying the generic base defaults every provider starts from
func BaseVendor() Vendor {
	return Vendor{
		MarkerBaseURL: "acp://agent", PlaceholderModel: "acp", Label: "ACP",
		UnsupportedTransportHint: "Either install a CLI release that supports --acp, or point the provider at a working command/args pair.",
		ModelEntryDisabled:       func(any) bool { return false },
		EarlyExitError:           func(string) (string, bool) { return "", false },
	}
}

// Constructor keywords of the client, empty values falling back to the vendor defaults
type Options struct {
	APIKey         string
	BaseURL        string
	DefaultHeaders map[string]string
	ACPCommand     string
	ACPArgs        []string
	ACPCwd         string
	Command        string
	Args           []string
	Env            *tools.Env
}

// OpenAI client shaped facade over one ACP agent CLI spoken to on stdio
type Client struct {
	Vendor
	APIKey         string
	BaseURL        string
	defaultHeaders map[string]string
	acpCommand     string
	acpArgs        []string
	acpCwd         string
	env            *tools.Env

	mu        sync.Mutex
	processes map[*process]bool
	closed    bool
}

// Client with resolved command, args and workspace cwd, env overrides of the vendor winning over its defaults
func New(ctx context.Context, v Vendor, o Options) (*Client, error) {
	env := o.Env
	if env == nil {
		env = &tools.Env{Home: home.Dir(ctx)}
	}
	c := &Client{Vendor: v, env: env, processes: map[*process]bool{}}
	c.APIKey = firstNonEmpty(o.APIKey, v.PlaceholderModel)
	c.BaseURL = firstNonEmpty(o.BaseURL, v.MarkerBaseURL)
	c.defaultHeaders = map[string]string{}
	for k, val := range o.DefaultHeaders {
		c.defaultHeaders[k] = val
	}
	c.acpCommand = firstNonEmpty(o.ACPCommand, o.Command)
	if c.acpCommand == "" {
		c.acpCommand = c.resolveCommand()
	}
	switch {
	case len(o.ACPArgs) > 0:
		c.acpArgs = append([]string{}, o.ACPArgs...)
	case len(o.Args) > 0:
		c.acpArgs = append([]string{}, o.Args...)
	default:
		args, err := c.resolveArgs()
		if err != nil {
			return nil, err
		}
		c.acpArgs = args
	}
	cwd := o.ACPCwd
	if cwd == "" {
		resolved, err := tools.ResolveAgentCwd(env)
		if err != nil {
			return nil, err
		}
		cwd = resolved
	}
	resolved, err := pypath.Resolve(cwd)
	if err != nil {
		return nil, err
	}
	c.acpCwd = resolved
	return c, nil
}

// First non empty string
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// Headers the caller passed at construction
func (c *Client) DefaultHeaders() map[string]string { return c.defaultHeaders }

// Whether no session process is live, true once the last one drained or close ran
func (c *Client) IsClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

// Launch command from the first set command env var, else the vendor default
func (c *Client) resolveCommand() string {
	for _, name := range c.CommandEnvVars {
		if v := pystr.Strip(os.Getenv(name)); v != "" {
			return v
		}
	}
	return c.DefaultCommand
}

// Launch args from the shell split args env var, else the vendor defaults
func (c *Client) resolveArgs() ([]string, error) {
	override := ""
	if c.ArgsEnvVar != "" {
		override = pystr.Strip(os.Getenv(c.ArgsEnvVar))
	}
	split, err := pyshlex.SplitPOSIX(override, false)
	if err != nil {
		return nil, pyErr("ValueError", "%s", err.Error())
	}
	if len(split) > 0 {
		return split, nil
	}
	return append([]string{}, c.DefaultArgs...), nil
}

// Request failure text, the vendor auth hint appended for the auth required code
func (c *Client) requestError(method string, errValue any) string {
	var shown any = errValue
	if isDict(errValue) {
		if m := dictGet(errValue, "message"); pyrepr.Truthy(m) {
			shown = m
		}
	}
	text := fmt.Sprintf("%s %s failed: %s", c.Label, method, pyrepr.Str(shown))
	authRequired := isDict(errValue) && pyrepr.Equal(dictGet(errValue, "code"), acpAuthRequired)
	if authRequired && c.AuthRequiredHint != nil && *c.AuthRequiredHint != "" {
		return text + " " + *c.AuthRequiredHint
	}
	return text
}

// Install hint after a failed start, naming the command env vars when the vendor has no hint
func (c *Client) missingCommandHint() string {
	if c.MissingCommandHint != "" {
		return c.MissingCommandHint
	}
	if len(c.CommandEnvVars) > 0 {
		return "Install the agent CLI or set " + strings.Join(c.CommandEnvVars, "/") + "."
	}
	return "Install the agent CLI."
}

// One spawned agent CLI with its stdin, exit status and output pumps
type process struct {
	cmd        *exec.Cmd
	stdin      io.WriteCloser
	done       chan struct{}
	returncode int
	inbox      *inbox
	stderrMu   sync.Mutex
	stderrTail []string
	stderrDone chan struct{}
}

// Exit status when the process ended, false while it runs
func (p *process) poll() (int, bool) {
	select {
	case <-p.done:
		return p.returncode, true
	default:
		return 0, false
	}
}

// Waits up to the duration for exit, true when the process ended
func (p *process) wait(d time.Duration) bool {
	select {
	case <-p.done:
		return true
	case <-time.After(d):
		return false
	}
}

// Asks the process to end, killing it when it does not within two seconds
func terminateProcess(p *process) {
	if _, ended := p.poll(); ended {
		_ = p.stdin.Close()
		return
	}
	var err error
	if runtime.GOOS == "windows" {
		err = p.cmd.Process.Kill()
	} else {
		err = p.cmd.Process.Signal(syscall.SIGTERM)
	}
	if (err != nil && !errors.Is(err, os.ErrProcessDone)) || !p.wait(2*time.Second) {
		_ = p.cmd.Process.Kill()
	}
	_ = p.stdin.Close()
}

// Reaps one session's own child, flipping closed only when no sibling session is live
func (c *Client) releaseProcess(p *process) {
	c.mu.Lock()
	delete(c.processes, p)
	if len(c.processes) == 0 {
		c.closed = true
	}
	c.mu.Unlock()
	terminateProcess(p)
}

// Terminates every live session process and marks the client closed
func (c *Client) Close() error {
	c.mu.Lock()
	procs := make([]*process, 0, len(c.processes))
	for p := range c.processes {
		procs = append(procs, p)
	}
	c.processes = map[*process]bool{}
	c.closed = true
	c.mu.Unlock()
	for _, p := range procs {
		terminateProcess(p)
	}
	return nil
}

// Starts argv with piped stdio in the cwd under the env, the Popen call tests replace at this seam
var startProcess = func(argv []string, cwd string, childEnv map[string]string) (*process, error) {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = cwd
	cmd.Env = envList(childEnv)
	cmd.SysProcAttr = hideWindowAttr()
	var opened []*os.File
	pipe := func() (*os.File, *os.File, error) {
		r, w, err := os.Pipe()
		if err == nil {
			opened = append(opened, r, w)
		}
		return r, w, err
	}
	closeAll := func() {
		for _, f := range opened {
			_ = f.Close()
		}
	}
	inR, inW, err := pipe()
	if err != nil {
		return nil, err
	}
	outR, outW, err := pipe()
	if err != nil {
		closeAll()
		return nil, err
	}
	errR, errW, err := pipe()
	if err != nil {
		closeAll()
		return nil, err
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = inR, outW, errW
	if err := cmd.Start(); err != nil {
		closeAll()
		return nil, err
	}
	_, _, _ = inR.Close(), outW.Close(), errW.Close()
	p := &process{cmd: cmd, stdin: inW, done: make(chan struct{}), inbox: newInbox(), stderrDone: make(chan struct{})}
	go func() {
		pumpLines(outR, func(line string) { p.inbox.put(decodeLine(line)) })
		_ = outR.Close()
	}()
	go func() {
		pumpLines(errR, func(line string) {
			p.stderrMu.Lock()
			p.stderrTail = append(p.stderrTail, strings.TrimRight(line, "\n"))
			if len(p.stderrTail) > 40 {
				p.stderrTail = p.stderrTail[len(p.stderrTail)-40:]
			}
			p.stderrMu.Unlock()
		})
		_ = errR.Close()
		close(p.stderrDone)
	}()
	go func() {
		err := cmd.Wait()
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			p.returncode = pysubprocess.Returncode(exitErr)
		}
		close(p.done)
	}()
	return p, nil
}

// Name sorted NAME=value list of an environment map
func envList(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

// Feeds each universal newline line of the stream, decoded as utf-8 with replacement, to the sink
func pumpLines(r io.Reader, sink func(string)) {
	br := bufio.NewReader(r)
	var pending []byte
	flush := func() {
		sink(pystr.DecodeUTF8Replace(pending) + "\n")
		pending = pending[:0]
	}
	for {
		b, err := br.ReadByte()
		if err != nil {
			if len(pending) > 0 {
				sink(pystr.DecodeUTF8Replace(pending))
			}
			return
		}
		switch b {
		case '\n':
			flush()
		case '\r':
			if next, err := br.Peek(1); err == nil && next[0] == '\n' {
				_, _ = br.ReadByte()
			}
			flush()
		default:
			pending = append(pending, b)
		}
	}
}

// Decoded JSON of one stdout line, a raw record for anything that does not parse
func decodeLine(line string) any {
	v, err := pyjson.Loads(line)
	if err != nil {
		raw := pyjson.NewObject()
		raw.Set("raw", strings.TrimRight(line, "\n"))
		return raw
	}
	return v
}

// Unbounded queue of decoded agent messages
type inbox struct {
	mu    sync.Mutex
	items []any
	ready chan struct{}
}

func newInbox() *inbox { return &inbox{ready: make(chan struct{}, 1)} }

// Appends a message and wakes a waiting reader
func (q *inbox) put(v any) {
	q.mu.Lock()
	q.items = append(q.items, v)
	q.mu.Unlock()
	select {
	case q.ready <- struct{}{}:
	default:
	}
}

// Next message within the timeout, false when none arrived
func (q *inbox) get(timeout time.Duration) (any, bool) {
	deadline := time.Now().Add(timeout)
	for {
		q.mu.Lock()
		if len(q.items) > 0 {
			v := q.items[0]
			q.items = q.items[1:]
			q.mu.Unlock()
			return v, true
		}
		q.mu.Unlock()
		left := time.Until(deadline)
		if left <= 0 {
			return nil, false
		}
		select {
		case <-q.ready:
		case <-time.After(left):
		}
	}
}

// Starts the CLI after the acp probe, mapping a missing binary to the install hint and tracking the child
func (c *Client) spawn(ctx context.Context) (*process, error) {
	if verdict := acpSupported(ctx, c.acpCommand, c.acpArgs); verdict != nil && !*verdict {
		preview := "(none)"
		if len(c.acpArgs) > 0 {
			preview = strings.Join(c.acpArgs[:min(3, len(c.acpArgs))], " ")
		}
		return nil, pyErr("RuntimeError", "ACP transport not supported by '%s': `%s` is rejected as an unknown option. This "+
			"usually means the CLI is an older release (e.g. Claude Code v2.x) or a different tool than expected. %s",
			c.acpCommand, preview, c.UnsupportedTransportHint)
	}
	childEnv, err := buildSubprocessEnv(ctx, c.env)
	if err != nil {
		return nil, err
	}
	p, err := startProcess(append([]string{c.acpCommand}, c.acpArgs...), c.acpCwd, childEnv)
	if err != nil {
		if isNotFound(err) {
			return nil, pyErr("RuntimeError", "Could not start %s command '%s'. %s", c.Label, c.acpCommand, c.missingCommandHint())
		}
		return nil, err
	}
	c.mu.Lock()
	c.processes[p] = true
	c.closed = false
	c.mu.Unlock()
	return p, nil
}

// One live ACP session, its session/new result and the process serving it
type session struct {
	c                 *Client
	proc              *process
	result            any
	deadline          time.Time
	nextID            int64
	allowFileRequests bool
}

// Spawns the CLI, runs initialize and session/new under one budget, the child reaped on any failure
func (c *Client) openSession(ctx context.Context, timeoutSeconds float64, allowFileRequests bool) (*session, error) {
	proc, err := c.spawn(ctx)
	if err != nil {
		return nil, err
	}
	s := &session{c: c, proc: proc, deadline: time.Now().Add(time.Duration(timeoutSeconds * float64(time.Second))), allowFileRequests: allowFileRequests}
	if _, err := s.request(ctx, "initialize", initializeParams(), nil, nil); err != nil {
		s.close()
		return nil, err
	}
	params := pyjson.NewObject()
	params.Set("cwd", c.acpCwd)
	params.Set("mcpServers", []any{})
	result, err := s.request(ctx, "session/new", params, nil, nil)
	if err != nil {
		s.close()
		return nil, err
	}
	result = or(result, pyjson.NewObject())
	sid, err := attrGet(result, "sessionId")
	if err != nil {
		s.close()
		return nil, err
	}
	if strField(sid) == "" {
		s.close()
		return nil, pyErr("RuntimeError", "%s did not return a sessionId.", c.Label)
	}
	s.result = result
	return s, nil
}

// Reaps the session's own process
func (s *session) close() { s.c.releaseProcess(s.proc) }

// Runs the body inside one session and reaps its process afterwards
func (c *Client) withSession(ctx context.Context, timeoutSeconds float64, allowFileRequests bool, body func(s *session) error) error {
	s, err := c.openSession(ctx, timeoutSeconds, allowFileRequests)
	if err != nil {
		return err
	}
	defer s.close()
	return body(s)
}

// Parameters of the initialize request
func initializeParams() *pyjson.Object {
	fsCaps := pyjson.NewObject()
	fsCaps.Set("readTextFile", true)
	fsCaps.Set("writeTextFile", true)
	caps := pyjson.NewObject()
	caps.Set("fs", fsCaps)
	info := pyjson.NewObject()
	info.Set("name", "icebath-agent")
	info.Set("title", "Icebath Agent")
	info.Set("version", "0.0.0")
	o := pyjson.NewObject()
	o.Set("protocolVersion", 1)
	o.Set("clientCapabilities", caps)
	o.Set("clientInfo", info)
	return o
}

// Line terminator text mode stdio writes on this platform
func textNewline() string {
	if runtime.GOOS == "windows" {
		return "\r\n"
	}
	return "\n"
}

// Sends one request and waits for its reply under the session budget, serving agent requests meanwhile
func (s *session) request(ctx context.Context, method string, params any, textParts, reasoningParts *[]string) (any, error) {
	s.nextID++
	requestID := s.nextID
	msg := pyjson.NewObject()
	msg.Set("jsonrpc", "2.0")
	msg.Set("id", requestID)
	msg.Set("method", method)
	msg.Set("params", params)
	line, err := pyjson.Dumps(msg)
	if err != nil {
		return nil, err
	}
	if _, werr := io.WriteString(s.proc.stdin, line+textNewline()); werr != nil {
		if !errors.Is(werr, syscall.EPIPE) && !errors.Is(werr, syscall.EINVAL) && !errors.Is(werr, os.ErrClosed) {
			return nil, werr
		}
		s.proc.wait(5 * time.Second)
		if _, ended := s.proc.poll(); !ended {
			return nil, werr
		}
	}
	for time.Now().Before(s.deadline) {
		if _, ended := s.proc.poll(); ended {
			break
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		m, ok := s.proc.inbox.get(100 * time.Millisecond)
		if !ok {
			continue
		}
		handled, err := s.c.handleServerMessage(m, s.proc.stdin, s.c.acpCwd, textParts, reasoningParts, s.allowFileRequests)
		if err != nil {
			return nil, err
		}
		if handled || !pyrepr.Equal(dictGet(m, "id"), requestID) {
			continue
		}
		if dictHas(m, "error") {
			return nil, pyErr("RuntimeError", "%s", s.c.requestError(method, or(dictGet(m, "error"), pyjson.NewObject())))
		}
		return dictGet(m, "result"), nil
	}
	if code, ended := s.proc.poll(); ended {
		select {
		case <-s.proc.stderrDone:
		case <-time.After(time.Second):
		}
		s.proc.stderrMu.Lock()
		stderrText := pystr.Strip(strings.Join(s.proc.stderrTail, "\n"))
		s.proc.stderrMu.Unlock()
		if vendorErr, ok := s.c.EarlyExitError(stderrText); ok {
			return nil, pyErr("RuntimeError", "%s", vendorErr)
		}
		if stderrText == "" {
			stderrText = fmt.Sprintf("exit code %d", code)
		}
		return nil, pyErr("RuntimeError", "%s process exited early: %s", s.c.Label, stderrText)
	}
	return nil, pyErr("TimeoutError", "Timed out waiting for %s response to %s.", s.c.Label, method)
}

// Enabled models a short lived authenticated session advertises, file requests refused meanwhile
func (c *Client) ListModels(ctx context.Context, timeoutSeconds float64) ([]string, error) {
	var models []string
	err := c.withSession(ctx, timeoutSeconds, false, func(s *session) error {
		var err error
		models, err = sessionModelIDs(s.result, c.ModelEntryDisabled)
		return err
	})
	return models, err
}

// Sends the prompt in a fresh session after selecting the model over ACP, returning text and reasoning
func (c *Client) runPrompt(ctx context.Context, promptText string, timeoutSeconds float64, model string) (string, string, error) {
	requestedModel := pystr.Strip(model)
	var text, reasoning string
	err := c.withSession(ctx, timeoutSeconds, true, func(s *session) error {
		sessionID := strField(dictGet(s.result, "sessionId"))
		if requestedModel != "" && requestedModel != c.PlaceholderModel {
			method, params, ok, err := modelSelectionRequest(s.result, requestedModel, c.PlaceholderModel, c.ModelEntryDisabled)
			if err == nil && ok {
				_, err = s.request(ctx, method, params, nil, nil)
			} else if err == nil {
				logWarning("%s does not offer model %s; using the session default.", c.Label, pyrepr.Repr(requestedModel))
			}
			if err != nil {
				if ctxErr := ctx.Err(); ctxErr != nil {
					return ctxErr
				}
				logWarning("%s model selection for %s failed; continuing with the session default: %s", c.Label, pyrepr.Repr(requestedModel), exceptionText(err))
			}
		}
		textParts, reasoningParts := []string{}, []string{}
		part := pyjson.NewObject()
		part.Set("type", "text")
		part.Set("text", promptText)
		prompt := pyjson.NewObject()
		prompt.Set("sessionId", sessionID)
		prompt.Set("prompt", []any{part})
		if _, err := s.request(ctx, "session/prompt", prompt, &textParts, &reasoningParts); err != nil {
			return err
		}
		text, reasoning = strings.Join(textParts, ""), strings.Join(reasoningParts, "")
		return nil
	})
	return text, reasoning, err
}

// Python str of an exception the client raised or an OS error it hit
func exceptionText(err error) string {
	var pe *Error
	if errors.As(err, &pe) {
		return pe.Msg
	}
	var pathErr *fs.PathError
	var errno syscall.Errno
	if errors.As(err, &pathErr) || errors.As(err, &errno) {
		return pypath.OSErrorText(err)
	}
	return pyrepr.ExceptionMessage(err)
}

// Consumes one agent message, true when it was a notification or a request this client answered
func (c *Client) handleServerMessage(msg any, stdin io.Writer, cwd string, textParts, reasoningParts *[]string, allowFileRequests bool) (bool, error) {
	rawMethod, err := attrGet(msg, "method")
	if err != nil {
		return false, err
	}
	method, ok := rawMethod.(string)
	if !ok {
		return false, nil
	}
	if method == "session/update" {
		update, err := attrGet(or(dictGet(msg, "params"), pyjson.NewObject()), "update")
		if err != nil {
			return false, err
		}
		update = or(update, pyjson.NewObject())
		content, err := attrGet(update, "content")
		if err != nil {
			return false, err
		}
		content = or(content, pyjson.NewObject())
		chunkText := ""
		if isDict(content) {
			if t := dictGet(content, "text"); pyrepr.Truthy(t) {
				chunkText = pyrepr.Str(t)
			}
		}
		var sink *[]string
		switch strField(dictGet(update, "sessionUpdate")) {
		case "agent_message_chunk":
			sink = textParts
		case "agent_thought_chunk":
			sink = reasoningParts
		}
		if chunkText != "" && sink != nil {
			*sink = append(*sink, chunkText)
		}
		return true, nil
	}
	if stdin == nil {
		return true, nil
	}
	messageID := dictGet(msg, "id")
	var response *pyjson.Object
	switch method {
	case "session/request_permission":
		outcome := pyjson.NewObject()
		outcome.Set("outcome", "cancelled")
		result := pyjson.NewObject()
		result.Set("outcome", outcome)
		response = jsonrpcResult(messageID, result)
	case "fs/read_text_file", "fs/write_text_file":
		if !allowFileRequests {
			response = jsonrpcError(messageID, -32601, "File access is unavailable during model discovery.")
			break
		}
		handler := fsReadTextFile
		if method == "fs/write_text_file" {
			handler = fsWriteTextFile
		}
		result, herr := handler(c.env, or(dictGet(msg, "params"), pyjson.NewObject()), cwd)
		if herr != nil {
			response = jsonrpcError(messageID, -32602, exceptionText(herr))
		} else {
			response = jsonrpcResult(messageID, result)
		}
	default:
		response = jsonrpcError(messageID, -32601, fmt.Sprintf("ACP client method '%s' is not supported by Icebath yet.", method))
	}
	line, err := pyjson.Dumps(response)
	if err != nil {
		return false, err
	}
	if _, err := io.WriteString(stdin, line+textNewline()); err != nil {
		return false, err
	}
	return true, nil
}
