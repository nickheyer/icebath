package tools

import (
	"bufio"
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/terminal"
	"github.com/nickheyer/icebath/internal/tools"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Options a script's terminal call may not take, since its commands run in the foreground of the script
var foregroundOnly = []string{"background", "pty", "notify", "notify_on_complete", "watch_patterns", "heartbeat", "persist_on_release"}

// Runs one tool call a script made, its arguments as JSON
type dispatch func(ctx context.Context, tool string, args json.RawMessage) string

// One call a script made, for the result of the run
type made struct {
	tool    string
	preview string
	seconds float64
	err     string
}

// Tools one run may call, how many calls it may make, and those it made
type budget struct {
	mu      sync.Mutex
	allowed []string
	max     int
	calls   int
	log     []made
}

func newBudget(allowed []string, max int) *budget {
	return &budget{allowed: slices.Sorted(slices.Values(allowed)), max: max}
}

func (b *budget) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls
}

// Starts a new cell of a kept kernel: its calls counted afresh, the log of the calls before it kept, where the cell's own begin
func (b *budget) restart() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls = 0
	return len(b.log)
}

// Failed calls since the index, five at most, for the result of the run
func (b *budget) failures(since int) []toolError {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := []toolError{}
	for _, m := range b.log[min(since, len(b.log)):] {
		if m.err != "" {
			out = append(out, toolError{Tool: m.tool, Error: m.err})
			if len(out) == 5 {
				break
			}
		}
	}
	return out
}

// A failed call a script made, as the result of the run lists it
type toolError struct {
	Tool  string `json:"tool"`
	Error string `json:"error"`
}

// One call a script asks for
type request struct {
	Tool  json.RawMessage `json:"tool"`
	Args  json.RawMessage `json:"args"`
	Token json.RawMessage `json:"token"`
	Seq   json.RawMessage `json:"seq"`
}

// Whether the request carries the token of the run, an empty token of the run refusing every request
func (r request) authorized(token string) bool {
	var got string
	if json.Unmarshal(r.Token, &got) != nil || token == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1
}

// Runs one authorized request within the run's tools and budget; a refused call costs nothing
func (b *budget) handle(ctx context.Context, r request, run dispatch) string {
	start := time.Now()
	var name string
	if json.Unmarshal(r.Tool, &name) != nil || !slices.Contains(b.allowed, name) {
		shown := strings.Trim(string(r.Tool), `"`)
		return tools.ErrorResult(fmt.Errorf("Tool '%s' is not available in execute_code. Available: %s", shown, strings.Join(b.allowed, ", ")))
	}
	b.mu.Lock()
	exhausted := b.calls >= b.max
	b.mu.Unlock()
	if exhausted {
		return tools.ErrorResult(fmt.Errorf("Tool call limit reached (%d). No more tool calls allowed in this execution.", b.max))
	}
	args := bytes.TrimSpace(r.Args)
	if len(args) == 0 || string(args) == "null" {
		args = []byte("{}")
	}
	if name == "terminal" {
		var fields map[string]json.RawMessage
		if json.Unmarshal(args, &fields) == nil {
			for _, option := range foregroundOnly {
				delete(fields, option)
			}
			args, _ = json.Marshal(fields)
		}
	}
	result := func() (result string) {
		defer func() {
			if p := recover(); p != nil {
				slog.Debug("Tool call failed in execute_code", "tool", name, "error", p)
				result = tools.ErrorResult(fmt.Errorf("%v", p))
			}
		}()
		return run(ctx, name, args)
	}()
	entry := made{tool: name, preview: strs.Head(string(args), 80), seconds: rounded(time.Since(start).Seconds()), err: failure(result)}
	b.mu.Lock()
	b.calls++
	b.log = append(b.log, entry)
	b.mu.Unlock()
	return result
}

// Seconds to two places, as results report durations
func rounded(seconds float64) float64 {
	return math.Round(seconds*100) / 100
}

// Error a JSON object result names, empty for any other result
func failure(result string) string {
	if !strings.HasPrefix(result, "{") || !strings.Contains(result, `"error"`) {
		return ""
	}
	var body map[string]any
	if json.Unmarshal([]byte(result), &body) != nil {
		return ""
	}
	switch e := body["error"].(type) {
	case nil, bool:
		if e == true {
			return "True"
		}
		return ""
	case string:
		return strs.Head(e, 300)
	default:
		text, _ := json.Marshal(e)
		return strs.Head(string(text), 300)
	}
}

// Result on one line of the socket: a JSON result as it is, compacted, any other text as a JSON string the module decodes
func line(result string) []byte {
	var out bytes.Buffer
	if json.Valid([]byte(result)) && json.Compact(&out, []byte(result)) == nil {
		return append(out.Bytes(), '\n')
	}
	text, _ := json.Marshal(result)
	return append(text, '\n')
}

// Serves the requests of one client of the socket until it disconnects, is idle for 300 seconds or the run stops; false when the listener
// itself failed
func serveOne(ctx context.Context, ln net.Listener, token string, b *budget, stop <-chan struct{}, run dispatch) bool {
	var conn net.Conn
	for conn == nil {
		if stopped(stop) {
			return true
		}
		if d, ok := ln.(interface{ SetDeadline(time.Time) error }); ok {
			d.SetDeadline(time.Now().Add(50 * time.Millisecond))
		}
		c, err := ln.Accept()
		if err != nil {
			if ne, ok := errors.AsType[net.Error](err); ok && ne.Timeout() {
				continue
			}
			slog.Debug("RPC listener socket error", "error", err)
			return false
		}
		conn = c
	}
	defer conn.Close()
	reader := bufio.NewReaderSize(conn, 65536)
	for !stopped(stop) {
		conn.SetReadDeadline(time.Now().Add(300 * time.Second))
		raw, err := reader.ReadBytes('\n')
		if text := bytes.TrimSpace(raw); len(text) > 0 && (err == nil || bytes.HasSuffix(raw, []byte("\n"))) {
			var resp string
			var r request
			switch {
			case !utf8.Valid(text) || !json.Valid(text):
				resp = tools.ErrorResult(errors.New("Invalid RPC request: the request is not valid JSON text"))
			case json.Unmarshal(text, &r) != nil:
				slog.Debug("RPC request is not a JSON object")
				return true
			case !r.authorized(token):
				resp = tools.ErrorResult(errors.New("Unauthorized RPC request"))
			default:
				resp = b.handle(ctx, r, run)
			}
			if _, werr := conn.Write(line(resp)); werr != nil {
				slog.Debug("RPC listener socket error", "error", werr)
				return true
			}
		}
		if err != nil {
			if ne, ok := errors.AsType[net.Error](err); ok && ne.Timeout() {
				slog.Debug("RPC listener socket timeout")
			}
			return true
		}
	}
	return true
}

func stopped(stop <-chan struct{}) bool {
	select {
	case <-stop:
		return true
	default:
		return false
	}
}

// Command run in a sandbox from its root, inside a subshell so the folder the session's commands start in stays as it was
func inSandbox(ctx context.Context, env terminal.Environment, command string, timeout time.Duration, stdin *string) (*terminal.Result, error) {
	return env.Execute(ctx, "( cd / && "+command+" )", terminal.Exec{Timeout: timeout, Stdin: stdin, Raw: true})
}

// Output of a command run in a sandbox, an error for a failure of the transport or an exit other than zero
func checked(ctx context.Context, env terminal.Environment, command, what string, timeout time.Duration, stdin *string) (*terminal.Result, error) {
	result, err := inSandbox(ctx, env, command, timeout, stdin)
	if err != nil {
		return nil, err
	}
	if result.Code != 0 {
		return nil, fmt.Errorf("%s failed: %s", what, strings.TrimSpace(result.Output))
	}
	return result, nil
}

// Writes content to a sandbox path readable by its owner alone, the bytes going in on stdin, never in the command line
func ship(ctx context.Context, env terminal.Environment, path, content string, atomic bool) error {
	_, err := checked(ctx, env, "umask 077 && "+writeCommand(path, atomic), "remote file ship for "+path, 30*time.Second,
		new(base64.StdEncoding.EncodeToString([]byte(content))))
	return err
}

func writeCommand(path string, atomic bool) string {
	target := quote(path)
	if atomic {
		return fmt.Sprintf("base64 -d > %s.tmp && mv -f %s.tmp %s", target, target, target)
	}
	return "base64 -d > " + target
}

// Shell command making the folders, readable by their owner alone, repairing the mode of those that were there
func private(root string, subdirs ...string) string {
	made := subdirs
	if len(made) == 0 {
		made = []string{root}
	}
	return fmt.Sprintf("umask 077 && mkdir -p %s && chmod 700 %s", quoteAll(made), quoteAll(append([]string{root}, subdirs...)))
}

func quote(s string) string {
	if s == "" {
		return "''"
	}
	if strings.IndexFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("@%+=:,./-_", r))
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

func quoteAll(items []string) string {
	out := make([]string, len(items))
	for i, item := range items {
		out[i] = quote(item)
	}
	return strings.Join(out, " ")
}

// Answers the request files a sandboxed script leaves in dir until the run stops: malformed, unauthorized and badly numbered requests
// dropped
func poll(ctx context.Context, env terminal.Environment, dir, token string, b *budget, stop <-chan struct{}, run dispatch) {
	quoted := quote(dir)
	once := func() error {
		listed, err := inSandbox(ctx, env, fmt.Sprintf("ls -1 %s/req_* 2>/dev/null || true", quoted), 10*time.Second, nil)
		if err != nil {
			return err
		}
		var files []string
		for _, f := range strings.Split(strings.TrimSpace(listed.Output), "\n") {
			if f = strings.TrimSpace(f); f != "" && !strings.HasSuffix(f, ".tmp") && strings.Contains(f, "/req_") {
				files = append(files, f)
			}
		}
		slices.Sort(files)
		for _, file := range files {
			if stopped(stop) {
				return nil
			}
			read, err := inSandbox(ctx, env, "cat "+quote(file), 10*time.Second, nil)
			if err != nil {
				return err
			}
			remove := func() error {
				_, err := inSandbox(ctx, env, "rm -f "+quote(file), 5*time.Second, nil)
				return err
			}
			var r request
			var seq int
			switch {
			case json.Unmarshal([]byte(read.Output), &r) != nil:
				slog.Debug("Malformed RPC request", "file", file)
			case !r.authorized(token):
				slog.Debug("Unauthorized RPC request", "file", file)
			case !sequence(r.Seq, &seq):
				slog.Debug("RPC request with malformed seq", "file", file)
			default:
				result := b.handle(ctx, r, run)
				if _, err := inSandbox(ctx, env, "umask 077 && "+writeCommand(fmt.Sprintf("%s/res_%06d", dir, seq), true), 60*time.Second,
					new(base64.StdEncoding.EncodeToString([]byte(result)))); err != nil {
					return err
				}
			}
			if err := remove(); err != nil {
				return err
			}
		}
		return nil
	}
	for !stopped(stop) {
		if err := once(); err != nil && !stopped(stop) {
			slog.Debug("RPC poll error", "error", err)
		}
		select {
		case <-stop:
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// Number of a request file, absent counting as zero and a boolean as one or zero
func sequence(raw json.RawMessage, seq *int) bool {
	switch string(bytes.TrimSpace(raw)) {
	case "":
		*seq = 0
		return true
	case "true":
		*seq = 1
		return true
	case "false":
		*seq = 0
		return true
	}
	var n float64
	if json.Unmarshal(raw, &n) != nil || n != math.Trunc(n) {
		return false
	}
	*seq = int(n)
	return true
}
