package acpclient

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math/big"
	"os"
	"os/exec"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyhttpx"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pypwd"
	"github.com/nickheyer/icebath/pkg/internal/pyre"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	defaultTimeoutSeconds = 900.0
	acpAuthRequired       = -32000
)

// Python exception raised by the ACP client, Kind naming its class
type Error struct {
	Kind string
	Msg  string
}

func (e *Error) Error() string { return e.Msg }

// Python exception class name the error stands for
func (e *Error) PyTypeName() string { return e.Kind }

func pyErr(kind, format string, args ...any) error {
	return &Error{Kind: kind, Msg: fmt.Sprintf(format, args...)}
}

var acpProbe = struct {
	sync.Mutex
	verdicts map[string]bool
}{verdicts: map[string]bool{}}

var acpFlagRE = pyre.MustCompile(`(?m)(?:^|[\s\[])--acp(?:[\s=\],]|$)`)

// Runs the help probe like subprocess.run with captured utf-8 output, replaced by tests at this seam
var probeRun = func(ctx context.Context, argv []string) (*pysubprocess.Completed, error) {
	return pysubprocess.RunWith(ctx, argv, pysubprocess.Options{Timeout: 5 * time.Second})
}

// Tri state verdict whether the help text advertises the acp flag, nil when the probe was inconclusive
func acpSupported(ctx context.Context, command string, args []string) *bool {
	yes := true
	if !containsString(args, "--acp") {
		return &yes
	}
	acpProbe.Lock()
	cached, ok := acpProbe.verdicts[command]
	acpProbe.Unlock()
	if ok {
		return &cached
	}
	probe, err := probeRun(ctx, []string{command, "--help"})
	if err != nil || probe.Returncode != 0 {
		return nil
	}
	matched, err := acpFlagRE.MatchString(pystr.DecodeUTF8Replace(probe.Stdout))
	if err != nil {
		return nil
	}
	acpProbe.Lock()
	acpProbe.verdicts[command] = matched
	acpProbe.Unlock()
	return &matched
}

// Stable HOME for child agents, the temp dir as the last resort so a child never starts without one
func resolveHomeDir() string {
	if h := pystr.Strip(os.Getenv("HOME")); h != "" {
		return h
	}
	if expanded := pypath.Expanduser("~"); expanded != "" && expanded != "~" {
		return expanded
	}
	tmp := func() string {
		dir, _ := pypath.Gettempdir()
		return dir
	}
	if runtime.GOOS == "windows" {
		return tmp()
	}
	entry, err := pypwd.Getpwuid(os.Getuid())
	if err != nil {
		return tmp()
	}
	if dir := pystr.Strip(entry.Dir); dir != "" {
		return dir
	}
	return tmp()
}

// Child environment with provider credentials kept, a stable HOME and the subprocess home policy applied
func buildSubprocessEnv(ctx context.Context, env *tools.Env) (map[string]string, error) {
	child, err := tools.IcebathSubprocessEnvChecked(env, true, nil)
	if err != nil {
		return nil, err
	}
	child["HOME"] = resolveHomeDir()
	home.ApplySubprocessHomeEnv(ctx, child)
	return child, nil
}

// JSON-RPC success reply
func jsonrpcResult(messageID, result any) *pyjson.Object {
	o := pyjson.NewObject()
	o.Set("jsonrpc", "2.0")
	o.Set("id", messageID)
	o.Set("result", result)
	return o
}

// JSON-RPC error reply with its code and message
func jsonrpcError(messageID any, code int, message string) *pyjson.Object {
	e := pyjson.NewObject()
	e.Set("code", code)
	e.Set("message", message)
	o := pyjson.NewObject()
	o.Set("jsonrpc", "2.0")
	o.Set("id", messageID)
	o.Set("error", e)
	return o
}

// Model entry filter that rejects nothing
func neverDisabled(any) (bool, error) { return false, nil }

// Whether a value is a decoded JSON object or a Go string keyed map
func isDict(v any) bool {
	switch v.(type) {
	case *pyjson.Object, map[string]any:
		return true
	}
	return false
}

// dict.get of a decoded object, nil for a missing key
func dictGet(v any, key string) any {
	switch d := v.(type) {
	case *pyjson.Object:
		x, _ := d.Get(key)
		return x
	case map[string]any:
		return d[key]
	}
	return nil
}

// Key membership of a decoded object
func dictHas(v any, key string) bool {
	switch d := v.(type) {
	case *pyjson.Object:
		_, ok := d.Get(key)
		return ok
	case map[string]any:
		_, ok := d[key]
		return ok
	}
	return false
}

// dict.get on a value that may not be a dict, the AttributeError Python raises for any other type
func attrGet(v any, key string) (any, error) {
	if isDict(v) {
		return dictGet(v, key), nil
	}
	return nil, pyErr("AttributeError", "'%s' object has no attribute 'get'", pyrepr.TypeName(v))
}

// Value or its fallback when falsy, the Python or operator
func or(v, fallback any) any {
	if pyrepr.Truthy(v) {
		return v
	}
	return fallback
}

// Stripped str of a value, falsy values reading as empty
func strField(v any) string {
	if !pyrepr.Truthy(v) {
		return ""
	}
	return pystr.Strip(pyrepr.Str(v))
}

// Items a for loop over the value yields, the TypeError for a value that is not iterable
func iterate(v any) ([]any, error) {
	switch x := v.(type) {
	case nil:
		return nil, nil
	case []any:
		return x, nil
	case []map[string]any:
		out := make([]any, len(x))
		for i, m := range x {
			out[i] = m
		}
		return out, nil
	case string:
		out := []any{}
		for _, r := range pystr.Runes(x) {
			out = append(out, string(r))
		}
		return out, nil
	case *pyjson.Object:
		out := make([]any, len(x.Keys))
		for i, k := range x.Keys {
			out[i] = k
		}
		return out, nil
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out := make([]any, len(keys))
		for i, k := range keys {
			out[i] = k
		}
		return out, nil
	}
	if rv := reflect.ValueOf(v); rv.Kind() == reflect.Slice || rv.Kind() == reflect.Array {
		out := make([]any, rv.Len())
		for i := range out {
			out[i] = rv.Index(i).Interface()
		}
		return out, nil
	}
	return nil, pyErr("TypeError", "'%s' object is not iterable", pyrepr.TypeName(v))
}

// Ordered unique ids of the entries the filter does not reject
func enabledIDList(entries any, key string, disabled func(any) (bool, error)) ([]string, error) {
	items, err := iterate(or(entries, []any{}))
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	result := []string{}
	for _, entry := range items {
		if !isDict(entry) {
			continue
		}
		value := strField(dictGet(entry, key))
		if value == "" || seen[value] {
			continue
		}
		off, err := disabled(entry)
		if err != nil {
			return nil, err
		}
		if off {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	return result, nil
}

// First config option of the session whose category or id is model, nil when none is advertised
func modelConfigOption(session any) (any, error) {
	items, err := iterate(or(dictGet(session, "configOptions"), []any{}))
	if err != nil {
		return nil, err
	}
	for _, option := range items {
		if !isDict(option) {
			continue
		}
		if pyrepr.Equal(dictGet(option, "category"), "model") || pyrepr.Equal(dictGet(option, "id"), "model") {
			return option, nil
		}
	}
	return nil, nil
}

// Account authorized model ids session/new advertises in ACP v1 or its legacy extension
func sessionModelIDs(session any, disabled func(any) (bool, error)) ([]string, error) {
	option, err := modelConfigOption(session)
	if err != nil {
		return nil, err
	}
	if pyrepr.Truthy(option) {
		return enabledIDList(dictGet(option, "options"), "value", disabled)
	}
	return legacySessionModelIDs(session, disabled)
}

// Model ids of the pre stabilization availableModels extension
func legacySessionModelIDs(session any, disabled func(any) (bool, error)) ([]string, error) {
	available, err := attrGet(or(dictGet(session, "models"), pyjson.NewObject()), "availableModels")
	if err != nil {
		return nil, err
	}
	return enabledIDList(available, "modelId", disabled)
}

// ACP request selecting the model for the session, false for unknown, disabled or session default ids
func modelSelectionRequest(session any, requestedModel, placeholder string, disabled func(any) (bool, error)) (string, *pyjson.Object, bool, error) {
	sessionID := strField(dictGet(session, "sessionId"))
	requestedModel = pystr.Strip(requestedModel)
	if sessionID == "" || requestedModel == "" || requestedModel == placeholder {
		return "", nil, false, nil
	}
	option, err := modelConfigOption(session)
	if err != nil {
		return "", nil, false, err
	}
	params := pyjson.NewObject()
	params.Set("sessionId", sessionID)
	if pyrepr.Truthy(option) {
		enabled, err := enabledIDList(dictGet(option, "options"), "value", disabled)
		if err != nil {
			return "", nil, false, err
		}
		if !containsString(enabled, requestedModel) {
			return "", nil, false, nil
		}
		params.Set("configId", pyrepr.Str(or(dictGet(option, "id"), "model")))
		params.Set("value", requestedModel)
		return "session/set_config_option", params, true, nil
	}
	available, err := legacySessionModelIDs(session, disabled)
	if err != nil {
		return "", nil, false, err
	}
	if len(available) > 0 && !containsString(available, requestedModel) {
		return "", nil, false, nil
	}
	params.Set("modelId", requestedModel)
	return "session/set_model", params, true, nil
}

// Resolved path confined to the resolved session cwd, refusing namespace, relative and escaping paths
func ensurePathWithinCwd(pathText, cwd, verb string) (string, error) {
	if ntErr := tools.NTNamespaceError(pathText, verb); ntErr != "" {
		return "", pyErr("PermissionError", "%s", ntErr)
	}
	if !pypath.IsAbs(pypath.PathStr(pathText)) {
		return "", pyErr("PermissionError", "ACP file-system paths must be absolute.")
	}
	resolved, err := pypath.Resolve(pathText)
	if err != nil {
		return "", err
	}
	root, err := pypath.Resolve(cwd)
	if err != nil {
		return "", err
	}
	if _, ok := pypath.PathRelativeTo(resolved, root); !ok {
		return "", pyErr("PermissionError", "Path '%s' is outside the session cwd '%s'.", resolved, root)
	}
	return resolved, nil
}

// Wall clock seconds of a number or a timeout object, its largest component winning
func effectiveTimeout(timeout any) float64 {
	if f, ok := pyNumber(timeout); ok {
		return f
	}
	var candidates []float64
	switch t := timeout.(type) {
	case pyhttpx.Timeout:
		candidates = durationSeconds(t.Read, t.Write, t.Connect, t.Pool)
	case *pyhttpx.Timeout:
		if t != nil {
			candidates = durationSeconds(t.Read, t.Write, t.Connect, t.Pool)
		}
	default:
		rv := reflect.ValueOf(timeout)
		for rv.Kind() == reflect.Pointer && !rv.IsNil() {
			rv = rv.Elem()
		}
		if rv.Kind() == reflect.Struct {
			for _, name := range []string{"Read", "Write", "Connect", "Pool", "Timeout"} {
				if f := rv.FieldByName(name); f.IsValid() && f.CanInterface() {
					if n, ok := pyNumber(f.Interface()); ok {
						candidates = append(candidates, n)
					}
				}
			}
		}
	}
	if len(candidates) == 0 {
		return defaultTimeoutSeconds
	}
	best := candidates[0]
	for _, c := range candidates[1:] {
		best = max(best, c)
	}
	return best
}

// Seconds of the configured phases, an unset zero phase left out
func durationSeconds(ds ...time.Duration) []float64 {
	var out []float64
	for _, d := range ds {
		if d != 0 {
			out = append(out, d.Seconds())
		}
	}
	return out
}

// Float of a Python int, float or bool value
func pyNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case bool:
		if n {
			return 1, true
		}
		return 0, true
	case *big.Int:
		f, _ := new(big.Float).SetInt(n).Float64()
		return f, true
	case *float64:
		if n != nil {
			return *n, true
		}
		return 0, false
	}
	if i, ok := pyrepr.Int(v); ok {
		f, _ := new(big.Float).SetInt(i).Float64()
		return f, true
	}
	return 0, false
}

// Python int value of a JSON number, bools counting as ints
func pyInt(v any) (*big.Int, bool) {
	if b, ok := v.(bool); ok {
		if b {
			return big.NewInt(1), true
		}
		return big.NewInt(0), true
	}
	return pyrepr.Int(v)
}

// Answers fs/read_text_file with the redacted text inside the cwd, an optional line window applied
func fsReadTextFile(env *tools.Env, params any, cwd string) (any, error) {
	pathText, err := paramString(params, "path")
	if err != nil {
		return nil, err
	}
	path, err := ensurePathWithinCwd(pathText, cwd, "Read")
	if err != nil {
		return nil, err
	}
	if block := tools.ReadBlockError(env, path); block != "" {
		return nil, pyErr("PermissionError", "%s", block)
	}
	content := ""
	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		if content, err = pystr.DecodeUTF8SigFile(raw); err != nil {
			return nil, err
		}
		content = strings.ReplaceAll(strings.ReplaceAll(content, "\r\n", "\n"), "\r", "\n")
	case errors.Is(err, fs.ErrNotExist):
	default:
		return nil, err
	}
	line, limit := dictGet(params, "line"), dictGet(params, "limit")
	if l, ok := pyInt(line); ok && l.Cmp(big.NewInt(1)) > 0 {
		lines := pystr.Splitlines(content, true)
		start := clampIndex(new(big.Int).Sub(l, big.NewInt(1)), len(lines))
		end := len(lines)
		if n, ok := pyInt(limit); ok && n.Sign() > 0 {
			end = clampIndex(new(big.Int).Add(new(big.Int).Sub(l, big.NewInt(1)), n), len(lines))
		}
		content = ""
		if start < end {
			content = strings.Join(lines[start:end], "")
		}
	}
	out := pyjson.NewObject()
	if content != "" {
		content = tools.RedactSensitiveTextWith(env, content, tools.RedactOptions{Force: true})
	}
	out.Set("content", content)
	return out, nil
}

// Non negative slice index clamped to the sequence size
func clampIndex(i *big.Int, size int) int {
	if i.Cmp(big.NewInt(int64(size))) > 0 {
		return size
	}
	return int(i.Int64())
}

// str of a params entry with falsy values reading as empty, the AttributeError for params that are no dict
func paramString(params any, key string) (string, error) {
	v, err := attrGet(params, key)
	if err != nil {
		return "", err
	}
	if !pyrepr.Truthy(v) {
		return "", nil
	}
	return pyrepr.Str(v), nil
}

// Answers fs/write_text_file inside the cwd unless the deny list or an approval gate blocks the path
func fsWriteTextFile(env *tools.Env, params any, cwd string) (any, error) {
	pathText, err := paramString(params, "path")
	if err != nil {
		return nil, err
	}
	path, err := ensurePathWithinCwd(pathText, cwd, "Write")
	if err != nil {
		return nil, err
	}
	if denied := tools.WriteDeniedError(env, path, "Write", false); denied != "" {
		return nil, pyErr("PermissionError", "%s", denied)
	}
	if tools.IsWriteApprovalRequired(env, path) {
		return nil, pyErr("PermissionError", "Write denied: '%s' requires interactive approval and cannot be written through the ACP file bridge.", path)
	}
	if err := os.MkdirAll(pypath.PathParent(path), 0o777); err != nil {
		return nil, err
	}
	content, _ := paramString(params, "content")
	if runtime.GOOS == "windows" {
		content = strings.ReplaceAll(content, "\n", "\r\n")
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o666)
	if err != nil {
		return nil, err
	}
	if pos, bad := firstSurrogate(content); bad {
		_ = f.Close()
		return nil, pyErr("UnicodeEncodeError", "'utf-8' codec can't encode character '\\u%04x' in position %d: surrogates not allowed", surrogateAt(content, pos), pos)
	}
	_, werr := f.WriteString(content)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return nil, werr
	}
	return nil, nil
}

// Code point position of the first WTF-8 encoded lone surrogate, false when the text is valid UTF-8
func firstSurrogate(s string) (int, bool) {
	for i, r := range pystr.Runes(s) {
		if r >= 0xd800 && r <= 0xdfff {
			return i, true
		}
	}
	return 0, false
}

// Surrogate code point at a code point position
func surrogateAt(s string, pos int) rune { return pystr.Runes(s)[pos] }

// Whether the list holds the string
func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// Warns through the default logger with Python percent formatting already applied
func logWarning(format string, args ...any) { slog.Warn(fmt.Sprintf(format, args...)) }

// Start error of the agent CLI, true when Python raises FileNotFoundError for it
func isNotFound(err error) bool {
	return errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist)
}
