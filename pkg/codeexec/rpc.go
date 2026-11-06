package codeexec

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyshlex"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

var terminalBlockedParams = []string{"background", "pty", "notify", "notify_on_complete", "watch_patterns",
	"heartbeat", "persist_on_release"}

// Runs one allowed, budgeted sandbox tool call and returns the tool result text
type dispatchFunc func(ctx context.Context, toolName string, args any) string

// Dispatch through the registry for the task, with the decoded args and their JSON text in request order
// Registry dispatch every sandbox tool call goes through, swapped by tests
var handleFunctionCall = tools.HandleFunctionCall

func defaultDispatch(env *tools.Env, taskID string) dispatchFunc {
	return func(ctx context.Context, toolName string, args any) string {
		call := &tools.Call{Name: toolName, TaskID: taskID, Env: env}
		call.Args, _ = pyjson.Plain(args).(map[string]any)
		raw, err := pyjson.Dumps(args, pyjson.EnsureASCII(false))
		if err != nil {
			return toolError(err.Error())
		}
		call.RawArgs = raw
		return handleFunctionCall(ctx, call)
	}
}

func toolError(msg string) string { return tools.ErrorResult(errors.New(msg)) }

// Shell command creating root or its subdirs owner only, repairing modes of dirs that already existed
func privateDirsCmd(root string, subdirs ...string) string {
	made := subdirs
	if len(made) == 0 {
		made = []string{root}
	}
	quote := func(ds []string) string {
		out := make([]string, len(ds))
		for i, d := range ds {
			out[i] = pyshlex.Quote(d)
		}
		return strings.Join(out, " ")
	}
	return fmt.Sprintf("umask 077 && mkdir -p %s && chmod 700 %s", quote(made), quote(append([]string{root}, subdirs...)))
}

// Runs cmd from the filesystem root and fails on a non zero exit with the command output only
func executeChecked(ctx context.Context, envr tools.Environment, cmd, what string, timeout int, stdin *string) (*tools.ExecResult, error) {
	result, err := envr.Execute(ctx, cmd, tools.ExecOptions{Cwd: "/", Timeout: timeout, Stdin: stdin})
	if err != nil {
		return nil, err
	}
	if result.ReturnCode != 0 {
		return nil, fmt.Errorf("%s failed: %s", what, pyrepr.StrRepr(result.Output))
	}
	return result, nil
}

// Writes content owner only to a remote path with the base64 payload on stdin, never in argv
func remoteWrite(ctx context.Context, envr tools.Environment, remotePath, content string, atomic bool, timeout int, check bool) (*tools.ExecResult, error) {
	encoded := base64.StdEncoding.EncodeToString(pystr.EncodeUTF8Replace(content))
	target := pyshlex.Quote(remotePath)
	write := "base64 -d > " + target
	if atomic {
		write = fmt.Sprintf("base64 -d > %s.tmp && mv -f %s.tmp %s", target, target, target)
	}
	cmd := "umask 077 && " + write
	if check {
		return executeChecked(ctx, envr, cmd, "remote file ship for "+pyrepr.StrRepr(remotePath), timeout, &encoded)
	}
	return envr.Execute(ctx, cmd, tools.ExecOptions{Cwd: "/", Timeout: timeout, Stdin: &encoded})
}

// Constant time token check where an empty server token fails closed
func rpcTokenOK(request *pyjson.Object, token string) bool {
	if token == "" {
		return false
	}
	got := ""
	if v, ok := request.Get("token"); ok && pyrepr.Truthy(v) {
		got = pyrepr.Str(v)
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1
}

// One in-script tool call record for the execute_code result
type toolCallEntry struct {
	Tool        string  `json:"tool"`
	ArgsPreview string  `json:"args_preview"`
	Duration    float64 `json:"duration"`
	Error       string  `json:"error,omitempty"`
}

// Allow list, call budget and call log one execution shares across its RPC transports
type rpcBudget struct {
	mu       sync.Mutex
	allowed  map[string]bool
	maxCalls int
	calls    int
	log      []toolCallEntry
}

func newRPCBudget(allowed []string, maxCalls int) *rpcBudget {
	b := &rpcBudget{allowed: map[string]bool{}, maxCalls: maxCalls}
	for _, name := range allowed {
		b.allowed[name] = true
	}
	return b
}

func (b *rpcBudget) callCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls
}

func (b *rpcBudget) entries() []toolCallEntry {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]toolCallEntry(nil), b.log...)
}

func (b *rpcBudget) sortedAllowed() []string {
	out := make([]string, 0, len(b.allowed))
	for k := range b.allowed {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func clipRunes(s string, n int) string {
	if pystr.Len(s) <= n {
		return s
	}
	return pystr.FromRunes(pystr.Runes(s)[:n])
}

// Enforces the allow list and budget then dispatches one authenticated request, refusals cost nothing
func handleRPCRequest(ctx context.Context, request *pyjson.Object, budget *rpcBudget, dispatch dispatchFunc, callStart time.Time, where string, log func(string, ...any)) string {
	var toolName any = ""
	if v, ok := request.Get("tool"); ok {
		toolName = v
	}
	var toolArgs any = pyjson.NewObject()
	if v, ok := request.Get("args"); ok {
		toolArgs = v
	}
	name, isStr := toolName.(string)
	if !isStr || !budget.allowed[name] {
		return toolError(fmt.Sprintf("Tool '%s' is not available in execute_code. Available: %s",
			pyrepr.Str(toolName), strings.Join(budget.sortedAllowed(), ", ")))
	}
	budget.mu.Lock()
	exhausted := budget.calls >= budget.maxCalls
	budget.mu.Unlock()
	if exhausted {
		return toolError(fmt.Sprintf("Tool call limit reached (%d). No more tool calls allowed in this execution.", budget.maxCalls))
	}
	argsObj, isObj := toolArgs.(*pyjson.Object)
	if name == "terminal" && isObj {
		for _, p := range terminalBlockedParams {
			argsObj.Delete(p)
		}
	}
	result := func() (result string) {
		defer func() {
			if r := recover(); r != nil {
				log("Tool call failed in "+where, "error", fmt.Sprint(r))
				result = toolError(fmt.Sprint(r))
			}
		}()
		return dispatch(tools.ThreadScopedSilence(ctx), name, toolArgs)
	}()
	entry := toolCallEntry{
		Tool:        name,
		ArgsPreview: clipRunes(pyrepr.Str(toolArgs), 80),
		Duration:    roundTo(time.Since(callStart).Seconds(), 2),
		Error:       resultError(result),
	}
	budget.mu.Lock()
	budget.calls++
	budget.log = append(budget.log, entry)
	budget.mu.Unlock()
	return result
}

// Value rounded to digits decimals from its exact binary value like Python round
func roundTo(v float64, digits int) float64 {
	out, _ := strconv.ParseFloat(strconv.FormatFloat(v, 'f', digits, 64), 64)
	return out
}

// Error text of a JSON object tool result, else empty
func resultError(result string) string {
	if !strings.HasPrefix(result, "{") || !strings.Contains(result, `"error"`) {
		return ""
	}
	body, err := pyjson.Loads(result)
	if err != nil {
		return ""
	}
	obj, ok := body.(*pyjson.Object)
	if !ok {
		return ""
	}
	value, _ := obj.Get("error")
	if !pyrepr.Truthy(value) {
		return ""
	}
	return clipRunes(pyrepr.Str(value), 300)
}

// Failed in-script tool calls since start, at most five, for the execute_code result
func toolErrorsSince(log []toolCallEntry, start int) []map[string]any {
	out := []map[string]any{}
	if start > len(log) {
		start = len(log)
	}
	for _, e := range log[start:] {
		if e.Error != "" {
			out = append(out, map[string]any{"tool": e.Tool, "error": e.Error})
			if len(out) == 5 {
				break
			}
		}
	}
	return out
}

// Decodes one request line as strict UTF-8 JSON, the Python error text on failure
func decodeRPCLine(line []byte) (any, error) {
	if !utf8.Valid(line) {
		pos := 0
		for pos < len(line) {
			r, size := utf8.DecodeRune(line[pos:])
			if r == utf8.RuneError && size <= 1 {
				break
			}
			pos += size
		}
		return nil, pystr.UTF8DecodeError(line, pos)
	}
	return pyjson.Loads(string(line))
}

type deadliner interface{ SetDeadline(time.Time) error }

// Accepts one client and serves newline delimited JSON requests until it disconnects, idles 300s or stop fires
func rpcServerLoop(ctx context.Context, env *tools.Env, ln net.Listener, taskID string, budget *rpcBudget, stop <-chan struct{}, token string, dispatch dispatchFunc) {
	if dispatch == nil {
		dispatch = defaultDispatch(env, taskID)
	}
	log := func(msg string, args ...any) { logger(env).Debug(msg, args...) }
	var conn net.Conn
	for conn == nil {
		select {
		case <-stop:
			return
		default:
		}
		if d, ok := ln.(deadliner); ok {
			_ = d.SetDeadline(time.Now().Add(50 * time.Millisecond))
		}
		c, err := ln.Accept()
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			log("RPC listener socket error", "error", err)
			return
		}
		conn = c
	}
	defer func() {
		if err := conn.Close(); err != nil {
			log("RPC conn close error", "error", err)
		}
	}()
	buf := []byte{}
	chunk := make([]byte, 65536)
	for {
		_ = conn.SetReadDeadline(time.Now().Add(300 * time.Second))
		n, err := conn.Read(chunk)
		if n > 0 {
			buf = append(buf, chunk[:n]...)
			for {
				idx := bytes.IndexByte(buf, '\n')
				if idx < 0 {
					break
				}
				line := bytes.Trim(buf[:idx], " \t\n\r\x0b\x0c")
				buf = buf[idx+1:]
				if len(line) == 0 {
					continue
				}
				callStart := time.Now()
				var resp string
				decoded, derr := decodeRPCLine(line)
				if derr != nil {
					resp = toolError("Invalid RPC request: " + derr.Error())
				} else {
					request, ok := decoded.(*pyjson.Object)
					if !ok {
						log("RPC request is not a JSON object", "type", pyrepr.TypeName(decoded))
						return
					}
					if rpcTokenOK(request, token) {
						resp = handleRPCRequest(ctx, request, budget, dispatch, callStart, "sandbox", log)
					} else {
						resp = toolError("Unauthorized RPC request")
					}
				}
				if _, werr := conn.Write([]byte(resp + "\n")); werr != nil {
					log("RPC listener socket error", "error", werr)
					return
				}
			}
		}
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				log("RPC listener socket timeout")
			}
			return
		}
	}
}

func stopped(stop <-chan struct{}) bool {
	select {
	case <-stop:
		return true
	default:
		return false
	}
}

func waitStop(stop <-chan struct{}, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-stop:
	case <-t.C:
	}
}

// Polls the remote filesystem for request files and answers them, dropping malformed or unauthorized ones
func rpcPollLoop(ctx context.Context, env *tools.Env, envr tools.Environment, rpcDir, taskID string, budget *rpcBudget, stop <-chan struct{}, token string, dispatch dispatchFunc) {
	if dispatch == nil {
		dispatch = defaultDispatch(env, taskID)
	}
	log := func(msg string, args ...any) { logger(env).Debug(msg, args...) }
	pollInterval := 100 * time.Millisecond
	quotedDir := pyshlex.Quote(rpcDir)
	exec := func(cmd string, timeout int) (*tools.ExecResult, error) {
		return envr.Execute(ctx, cmd, tools.ExecOptions{Cwd: "/", Timeout: timeout})
	}
	pollOnce := func() error {
		ls, err := exec(fmt.Sprintf("ls -1 %s/req_* 2>/dev/null || true", quotedDir), 10)
		if err != nil {
			return err
		}
		output := pystr.Strip(ls.Output)
		if output == "" {
			return nil
		}
		var reqFiles []string
		for _, line := range strings.Split(output, "\n") {
			f := pystr.Strip(line)
			if f != "" && !strings.HasSuffix(f, ".tmp") && strings.Contains(f, "/req_") {
				reqFiles = append(reqFiles, f)
			}
		}
		sort.Strings(reqFiles)
		for _, reqFile := range reqFiles {
			if stopped(stop) {
				break
			}
			callStart := time.Now()
			quoted := pyshlex.Quote(reqFile)
			read, err := exec("cat "+quoted, 10)
			if err != nil {
				return err
			}
			remove := func() error { _, err := exec("rm -f "+quoted, 5); return err }
			decoded, derr := pyjson.Loads(read.Output)
			if derr != nil {
				log("Malformed RPC request", "file", reqFile)
				if err := remove(); err != nil {
					return err
				}
				continue
			}
			request, ok := decoded.(*pyjson.Object)
			if !ok {
				return fmt.Errorf("'%s' object has no attribute 'get'", pyrepr.TypeName(decoded))
			}
			if !rpcTokenOK(request, token) {
				log("Unauthorized RPC request", "file", reqFile)
				if err := remove(); err != nil {
					return err
				}
				continue
			}
			var seq any = int64(0)
			if v, ok := request.Get("seq"); ok {
				seq = v
			}
			seqText, ferr := pyrepr.Format(seq, "06d")
			if _, isInt := pyrepr.Int(seq); !isInt {
				if _, isBool := seq.(bool); !isBool {
					ferr = errors.New("malformed seq")
				}
			}
			if ferr != nil {
				log("RPC request with malformed seq", "file", reqFile)
				if err := remove(); err != nil {
					return err
				}
				continue
			}
			result := handleRPCRequest(ctx, request, budget, dispatch, callStart, "remote sandbox", log)
			if _, err := remoteWrite(ctx, envr, rpcDir+"/res_"+seqText, result, true, 60, false); err != nil {
				return err
			}
			if err := remove(); err != nil {
				return err
			}
		}
		return nil
	}
	for !stopped(stop) {
		ls := pollOnce()
		if ls != nil && !stopped(stop) {
			log("RPC poll error", "error", ls)
		}
		if !stopped(stop) {
			waitStop(stop, pollInterval)
		}
	}
}
