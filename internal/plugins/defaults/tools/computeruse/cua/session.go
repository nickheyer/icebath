package cua

import (
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
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

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/pkg/strs"
)

// A driver call that ran past its deadline, whose effect on the screen is unknown
var errDeadline = errors.New("the cua-driver call did not answer in time")

// What a driver tool call came to, flattened from every transport's shape
type envelope struct {
	Data       any
	Images     []string
	Mimes      []string
	Structured map[string]any
	IsError    bool
}

// Messages of calls whose effect on the screen is unknown, so nothing replays them
var unknownOutcomes = map[string]string{
	"transport_outcome_unknown": "cua-driver transport failed during {name}; the action outcome is unknown, so Icebath did not replay it. Take " +
		"fresh state before deciding whether to act again.",
	"timeout_outcome_unknown": "cua-driver MCP call {name} timed out; the action outcome is unknown and may still have taken effect on the remote " +
		"screen. The session has been marked suspect and will be recreated before the next computer-use call. Take fresh state before deciding " +
		"whether to act again.",
}

// Failed envelope of a call whose outcome is unknown
func unknownOutcome(name string, cause error, code string) *envelope {
	message := strings.ReplaceAll(unknownOutcomes[code], "{name}", name)
	return &envelope{IsError: true, Data: message, Images: []string{}, Mimes: []string{}, Structured: map[string]any{"ok": false, "code": code,
		"message": message, "operation": name, "next_step": "fresh_state", "detail": cause.Error()}}
}

// Flattens a tools/call result: the text parts joined, parsed when they hold JSON, the images and their types
func extract(result map[string]any) *envelope {
	out := &envelope{Images: []string{}, Mimes: []string{}, IsError: result["isError"] == true}
	var chunks []string
	content, _ := result["content"].([]any)
	for _, raw := range content {
		part, _ := raw.(map[string]any)
		switch part["type"] {
		case "text":
			if text, _ := part["text"].(string); text != "" {
				chunks = append(chunks, text)
			} else {
				chunks = append(chunks, "")
			}
		case "image":
			if data, _ := part["data"].(string); data != "" {
				mime, _ := part["mimeType"].(string)
				out.Images, out.Mimes = append(out.Images, data), append(out.Mimes, mime)
			}
		}
	}
	if len(chunks) > 0 {
		var kept []string
		for _, c := range chunks {
			if c != "" {
				kept = append(kept, c)
			}
		}
		joined := strings.Join(kept, "\n")
		out.Data = joined
		if s := strings.TrimSpace(joined); strings.HasPrefix(s, "{") || strings.HasPrefix(s, "[") {
			var parsed any
			if json.Unmarshal([]byte(joined), &parsed) == nil {
				out.Data = parsed
			}
		}
	}
	if structured, ok := result["structuredContent"].(map[string]any); ok && len(structured) > 0 {
		out.Structured = structured
	}
	return out
}

// Text of a refused call, for telling one kind of refusal from another
func (e *envelope) text() string {
	var chunks []string
	for _, value := range []any{e.Data, e.Structured} {
		switch v := value.(type) {
		case nil:
		case string:
			chunks = append(chunks, v)
		case map[string]any:
			if len(v) > 0 {
				text, _ := json.Marshal(v)
				chunks = append(chunks, string(text))
			}
		default:
			text, _ := json.Marshal(v)
			chunks = append(chunks, string(text))
		}
	}
	return strings.Join(chunks, "\n")
}

// The driver's refusal of a call because the session it was declared under ended, which declaring it again revives
func (e *envelope) endedSession() bool {
	if e == nil || !e.IsError {
		return false
	}
	message := strings.ToLower(e.text())
	return strings.Contains(message, "session") && strings.Contains(message, "start_session") &&
		(strings.Contains(message, "has ended") || strings.Contains(message, "session ended"))
}

// Calls that open and close a session, exempt from the restarts before other calls
var lifecycleCalls = map[string]bool{"start_session": true, "end_session": true}

// Reads that are safe to run again after the transport broke
var replaySafe = map[string]bool{"get_cursor_position": true, "get_displays": true, "get_screen_size": true, "get_window_state": true, "list_apps": true,
	"list_windows": true}

// Longest the server may take to be ready
const readyTimeout = 30 * time.Second

// A connection to the driver: its MCP server started on first use and again after it dropped, the CLI as the transport of last resort
type session struct {
	driver *Driver
	config *config.Config
	daemon *daemon

	mu      sync.Mutex
	started atomic.Bool

	state        sync.Mutex
	client       *client
	capabilities map[string]map[string]bool
	schemas      map[string]map[string]any

	suspect    atomic.Bool
	declared   string
	generation int
	// Told when the transport is replaced, so what the old one minted is forgotten
	reset func()
}

func newSession(d *Driver, cfg *config.Config, daemon *daemon) *session {
	return &session{driver: d, config: cfg, daemon: daemon, capabilities: map[string]map[string]bool{}, schemas: map[string]map[string]any{}}
}

// Starts the server, reached within the ready timeout, saying where it stuck when it did not
func (s *session) open(ctx context.Context) error {
	phase := "binary-check"
	type opened struct {
		c   *client
		err error
	}
	done := make(chan opened, 1)
	var mu sync.Mutex
	set := func(p string) {
		mu.Lock()
		phase = p
		mu.Unlock()
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), readyTimeout)
	defer cancel()
	go func() {
		c, err := s.connect(ctx, set)
		done <- opened{c, err}
	}()
	var got opened
	select {
	case got = <-done:
	case <-ctx.Done():
		go func() {
			if late := <-done; late.c != nil {
				late.c.close()
			}
		}()
		mu.Lock()
		stuck := phase
		mu.Unlock()
		return fmt.Errorf("cua-driver session never reached ready (timeout %ds; stuck in phase: %s). Run `icebath computer-use doctor` and check "+
			"the icebath log for the phase timings.", int(readyTimeout.Seconds()), cmp.Or(stuck, "unknown"))
	}
	if got.err != nil {
		return fmt.Errorf("cua-driver session setup failed: %w", got.err)
	}
	s.state.Lock()
	s.client = got.c
	s.state.Unlock()
	s.generation++
	if s.generation > 1 {
		s.transportReset()
	}
	return nil
}

// Spawns the server the private daemon, the profile's override or the managed driver runs, and handshakes with it
func (s *session) connect(ctx context.Context, phase func(string)) (*client, error) {
	start := time.Now()
	binary, err := s.driver.Command("")
	if err != nil {
		return nil, err
	}
	// A screen in the terminal backend's sandbox is driven by the driver inside it, which needs none here
	var sandboxed []string
	if s.daemon == nil || binary == "" {
		if sandboxed, err = s.driver.Sandboxed(ctx, s.config); err != nil {
			return nil, err
		}
	}
	if binary == "" && sandboxed == nil {
		return nil, errors.New(InstallHint)
	}
	phase("manifest-discovery")
	var command string
	var args []string
	var env map[string]string
	switch {
	case s.daemon != nil:
		if command, args, err = s.daemon.proxy(); err != nil {
			return nil, err
		}
		env = s.daemon.env()
	case sandboxed != nil:
		command, args, env = sandboxed[0], sandboxed[1:], map[string]string{"PATH": os.Getenv("PATH")}
	default:
		command, args = s.driver.launch(ctx, s.config, binary)
		env = s.driver.Env(s.config)
	}
	manifested := time.Now()
	c, err := open(command, args, env, s.driver.host.Supervisor)
	if err != nil {
		return nil, err
	}
	phase("mcp-initialize")
	if _, err := c.initialize(ctx); err != nil {
		c.close()
		return nil, err
	}
	initialized := time.Now()
	phase("capability-discovery")
	s.discover(ctx, c)
	slog.Info(fmt.Sprintf("cua-driver session ready in %.1fs (manifest=%.1fs, mcp_init=%.1fs)", time.Since(start).Seconds(),
		manifested.Sub(start).Seconds(), initialized.Sub(manifested).Seconds()))
	return c, nil
}

// Keeps the capabilities and input schema of each tool tools/list names
func (s *session) discover(ctx context.Context, c *client) {
	capabilities, schemas := map[string]map[string]bool{}, map[string]map[string]any{}
	defer func() {
		s.state.Lock()
		s.capabilities, s.schemas = capabilities, schemas
		s.state.Unlock()
	}()
	list, err := c.tools(ctx)
	if err != nil {
		slog.Debug("cua-driver tools/list capability discovery failed", "error", err)
		return
	}
	tools, _ := list["tools"].([]any)
	for _, raw := range tools {
		tool, _ := raw.(map[string]any)
		name, ok := tool["name"].(string)
		if !ok {
			continue
		}
		set := map[string]bool{}
		listed, _ := tool["capabilities"].([]any)
		for _, c := range listed {
			if text, ok := c.(string); ok {
				set[text] = true
			}
		}
		capabilities[name] = set
		schema, _ := tool["inputSchema"].(map[string]any)
		if schema == nil {
			schema, _ = tool["input_schema"].(map[string]any)
		}
		if schema == nil {
			schema = map[string]any{}
		}
		schemas[name] = schema
	}
}

// Opens the server once
func (s *session) start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started.Load() {
		return nil
	}
	if err := s.open(ctx); err != nil {
		return err
	}
	s.started.Store(true)
	return nil
}

// Closes the server
func (s *session) stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started.Swap(false) {
		s.closeClient()
	}
}

func (s *session) closeClient() {
	s.state.Lock()
	c := s.client
	s.client = nil
	s.state.Unlock()
	if c != nil {
		if err := c.close(); err != nil {
			slog.Warn("cua-driver shutdown error", "error", err)
		}
	}
}

func (s *session) transportReset() {
	if s.reset != nil {
		s.reset()
	}
}

// The driver lists the capability for the tool, or for any tool when none is named
func (s *session) supports(capability, tool string) bool {
	s.state.Lock()
	defer s.state.Unlock()
	if tool != "" {
		return s.capabilities[tool][capability]
	}
	for _, set := range s.capabilities {
		if set[capability] {
			return true
		}
	}
	return false
}

// The driver lists the tool
func (s *session) has(tool string) bool {
	s.state.Lock()
	defer s.state.Unlock()
	_, ok := s.capabilities[tool]
	return ok
}

// The tool's live input schema takes the property
func (s *session) takes(tool, property string) bool {
	s.state.Lock()
	defer s.state.Unlock()
	props, _ := s.schemas[tool]["properties"].(map[string]any)
	_, ok := props[property]
	return ok
}

func (s *session) discovered() bool {
	s.state.Lock()
	defer s.state.Unlock()
	return len(s.capabilities) > 0
}

// The stream broke in a way reconnecting fixes
func closedErr(err error) bool {
	return errors.Is(err, errClosed) || errors.Is(err, syscall.EPIPE) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, io.ErrClosedPipe)
}

// The private daemon's proxy was congested, which a retry or the CLI transport gets past
func transientErr(err error) bool {
	msg := err.Error()
	for _, needle := range []string{"Resource temporarily unavailable", "os error 35", "daemon transport error", "daemon proxy"} {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}

// One call on the live client within the timeout
func (s *session) once(ctx context.Context, name string, args map[string]any, timeout time.Duration) (*envelope, error) {
	s.state.Lock()
	c := s.client
	s.state.Unlock()
	if c == nil {
		return nil, errors.New("the computer use driver session has no MCP client")
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	raw, err := c.call(ctx, name, args)
	if errors.Is(err, context.DeadlineExceeded) {
		return nil, errDeadline
	}
	if err != nil {
		return nil, err
	}
	return extract(raw), nil
}

// Declares the session's public label again, logging the driver's refusal
func (s *session) redeclare(ctx context.Context, timeout time.Duration, failure string) (bool, error) {
	args := map[string]any{"session": nil}
	if s.declared != "" {
		args["session"] = s.declared
	}
	result, err := s.once(ctx, "start_session", args, timeout)
	if err != nil {
		return false, err
	}
	if result.IsError {
		slog.Warn(fmt.Sprintf(failure, s.declared, result.text()))
	}
	return !result.IsError, nil
}

// Starts a stopped session, or replaces the server altogether, then declares the session's label again
func (s *session) recreate(ctx context.Context, name string, timeout time.Duration, message string, restart, clearSuspect bool) error {
	slog.Warn(fmt.Sprintf(message, name))
	if !restart {
		if err := s.start(ctx); err != nil {
			return err
		}
	} else {
		err := func() error {
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.started.Swap(false) {
				s.closeClient()
			}
			s.state.Lock()
			s.capabilities, s.schemas = map[string]map[string]bool{}, map[string]map[string]any{}
			s.state.Unlock()
			if err := s.open(ctx); err != nil {
				return err
			}
			s.started.Store(true)
			return nil
		}()
		if err != nil {
			return err
		}
		if clearSuspect {
			s.suspect.Store(false)
		}
	}
	if s.declared != "" {
		if _, err := s.redeclare(ctx, timeout, "cua-driver public session label %s could not be restored: %s"); err != nil {
			return err
		}
	}
	return nil
}

// One driver tool call: a suspect or stopped session made again first, a timeout failing closed, a dropped stream reconnected once and
// only reads run again, a congested daemon falling back to the CLI for reads
func (s *session) call(ctx context.Context, name string, args map[string]any, timeout time.Duration) (*envelope, error) {
	if !lifecycleCalls[name] {
		if s.suspect.Load() {
			if err := s.recreate(ctx, name, timeout, "cua-driver session suspect after earlier MCP timeout; recreating before %s", true, true); err != nil {
				return nil, err
			}
		}
		if !s.started.Load() {
			if err := s.recreate(ctx, name, timeout, "cua-driver session not active on %s; (re)starting before call", false, false); err != nil {
				return nil, err
			}
		}
	}
	if !s.started.Load() {
		return nil, errors.New("cua-driver session not started")
	}
	result, err := s.once(ctx, name, args, timeout)
	if err != nil {
		switch {
		case errors.Is(err, errDeadline):
			s.suspect.Store(true)
			slog.Warn(fmt.Sprintf("cua-driver MCP timed out on %s; marking session suspect for recreation before the next call", name))
			return unknownOutcome(name, err, "timeout_outcome_unknown"), nil
		case transientErr(err):
			if !replaySafe[name] {
				s.transportReset()
				return unknownOutcome(name, err, "transport_outcome_unknown"), nil
			}
			slog.Warn(fmt.Sprintf("cua-driver MCP transport failed on %s (%v); falling back to CLI transport", name, err))
			return s.viaCLI(ctx, name, args, timeout)
		case !closedErr(err):
			return nil, err
		}
		if rerr := s.recreate(ctx, name, timeout, "cua-driver MCP session closed during %s; reconnecting once", true, false); rerr != nil {
			return nil, rerr
		}
		if !replaySafe[name] {
			return unknownOutcome(name, err, "transport_outcome_unknown"), nil
		}
		if result, err = s.once(ctx, name, args, timeout); err != nil {
			return nil, err
		}
	}
	declared, _ := args["session"].(string)
	switch {
	case name == "start_session" && !result.IsError && declared != "":
		s.declared = declared
	case result.endedSession():
		if s.declared != "" && !lifecycleCalls[name] {
			slog.Warn(fmt.Sprintf("cua-driver session %s ended during %s; reviving and retrying once", s.declared, name))
			revived, err := s.redeclare(ctx, timeout, "cua-driver session %s could not be revived: %s")
			if err != nil {
				return nil, err
			}
			if revived {
				if result, err = s.once(ctx, name, args, timeout); err != nil {
					return nil, err
				}
			}
		}
	case name == "end_session" && !result.IsError && s.declared != "" && declared == s.declared:
		s.declared = ""
	}
	return result, nil
}

// Attempts of the CLI transport, backing off from half a second doubling
const cliAttempts = 4

// Runs `cua-driver call` until it prints JSON, failing at once when the machine wide daemon the CLI needs is down
func cliJSON(ctx context.Context, argv []string, env map[string]string, name string, timeout time.Duration) (any, error) {
	backoff, last := 500*time.Millisecond, ""
	for attempt := range cliAttempts {
		out, err := run(ctx, argv, max(15*time.Second, timeout), env, nil)
		if err != nil {
			return nil, fmt.Errorf("cua-driver CLI fallback for %s failed to spawn: %v", name, err)
		}
		text := strings.TrimSpace(out.stdout)
		last = cmp.Or(strs.Head(text, 200), strs.Head(out.stderr, 200))
		if strings.Contains(text, "daemon is not running") || strings.Contains(out.stderr, "daemon is not running") {
			return nil, fmt.Errorf("cua-driver CLI fallback for %s unavailable: the machine-wide cua-driver daemon is not running (the CLI transport "+
				"requires it; the MCP runtime does not).", name)
		}
		start := -1
		for _, i := range []int{strings.Index(text, "{"), strings.Index(text, "[")} {
			if i != -1 && (start == -1 || i < start) {
				start = i
			}
		}
		if start != -1 {
			var parsed any
			if json.Unmarshal([]byte(text[start:]), &parsed) == nil {
				return parsed, nil
			}
		}
		if attempt < cliAttempts-1 {
			slog.Warn(fmt.Sprintf("cua-driver CLI fallback for %s got no JSON (attempt %d/%d); retrying in %.1fs", name, attempt+1, cliAttempts,
				backoff.Seconds()))
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff):
			}
			backoff *= 2
		}
	}
	return nil, fmt.Errorf("cua-driver CLI fallback for %s returned no JSON after %d attempts: %s", name, cliAttempts, last)
}

// A `cua-driver call` reply as an envelope, its screenshot read from the file it was asked to write when it inlined none
func cliEnvelope(parsed any, shot string) *envelope {
	obj, ok := parsed.(map[string]any)
	if !ok {
		return &envelope{Images: []string{}}
	}
	out := &envelope{Images: []string{}, Structured: obj, IsError: obj["isError"] == true || obj["is_error"] == true}
	image, _ := obj["screenshot_png_b64"].(string)
	path, _ := obj["screenshot_file_path"].(string)
	if path = cmp.Or(path, shot); image == "" && path != "" {
		if raw, err := os.ReadFile(path); err == nil {
			image = base64.StdEncoding.EncodeToString(raw)
		} else if !errors.Is(err, os.ErrNotExist) {
			slog.Debug(fmt.Sprintf("cua-driver CLI fallback: failed reading %s: %v", path, err))
		}
	}
	if tree, ok := obj["tree_markdown"]; ok {
		out.Data = tree
		if count, ok := obj["element_count"]; ok && count != nil && tree != nil {
			out.Data = fmt.Sprintf("%v elements\n%v", number(count), tree)
		}
	}
	if image != "" {
		out.Images = []string{image}
	}
	return out
}

// A JSON number as Python prints it, whole ones without a fraction
func number(v any) any {
	if f, ok := v.(float64); ok && f == float64(int64(f)) {
		return int64(f)
	}
	return v
}

// The call made through `cua-driver call`, the screenshot of get_window_state routed through a temp file
func (s *session) viaCLI(ctx context.Context, name string, args map[string]any, timeout time.Duration) (*envelope, error) {
	call := map[string]any{}
	for k, v := range args {
		call[k] = v
	}
	shot := ""
	if _, given := call["screenshot_out_file"]; name == "get_window_state" && !given {
		f, err := os.CreateTemp("", "cua_shot_*.png")
		if err != nil {
			return nil, err
		}
		shot = f.Name()
		f.Close()
		call["screenshot_out_file"] = shot
		defer os.Remove(shot)
	}
	binary, err := s.driver.Command("")
	if err != nil {
		return nil, err
	}
	if binary == "" {
		return nil, errors.New(InstallHint)
	}
	env, socket := s.driver.Env(s.config), []string{}
	if s.daemon != nil {
		command, _, err := s.daemon.proxy()
		if err != nil {
			return nil, err
		}
		binary, env, socket = command, s.daemon.env(), []string{"--socket", s.daemon.socket}
	}
	encoded, err := json.Marshal(call)
	if err != nil {
		return nil, err
	}
	parsed, err := cliJSON(ctx, append([]string{binary, "call", name, string(encoded)}, socket...), env, name, timeout)
	if err != nil {
		return nil, err
	}
	return cliEnvelope(parsed, shot), nil
}
