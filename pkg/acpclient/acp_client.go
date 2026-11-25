package acpclient

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/tools"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	executil "github.com/nickheyer/icebath/pkg/utils/exec"
	httputil "github.com/nickheyer/icebath/pkg/utils/http"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	pathutil "github.com/nickheyer/icebath/pkg/utils/path"
	regexutil "github.com/nickheyer/icebath/pkg/utils/regex"
	textutil "github.com/nickheyer/icebath/pkg/utils/text"
	userutil "github.com/nickheyer/icebath/pkg/utils/user"
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
func (e *Error) ErrorKind() string { return e.Kind }

func kindErr(kind, format string, args ...any) error {
	return &Error{Kind: kind, Msg: fmt.Sprintf(format, args...)}
}

var acpProbe = struct {
	sync.Mutex
	verdicts map[string]bool
}{verdicts: map[string]bool{}}

var acpFlagRE = regexutil.MustCompile(`(?m)(?:^|[\s\[])--acp(?:[\s=\],]|$)`)

// Runs the help probe like subprocess.run with captured utf-8 output, replaced by tests at this seam
var probeRun = func(ctx context.Context, argv []string) (*executil.Completed, error) {
	return executil.Run(ctx, argv, executil.Options{Timeout: 5 * time.Second})
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
	if err != nil || probe.ExitCode != 0 {
		return nil
	}
	matched, err := acpFlagRE.MatchString(strings.ToValidUTF8(string(probe.Stdout), "\uFFFD"))
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
	if h := strings.TrimSpace(os.Getenv("HOME")); h != "" {
		return h
	}
	if expanded := pathutil.ExpandUser("~"); expanded != "" && expanded != "~" {
		return expanded
	}
	tmp := func() string {
		dir, _ := pathutil.TempDir()
		return dir
	}
	if runtime.GOOS == "windows" {
		return tmp()
	}
	entry, err := userutil.LookupID(os.Getuid())
	if err != nil {
		return tmp()
	}
	if dir := strings.TrimSpace(entry.Home); dir != "" {
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
func jsonrpcResult(messageID, result any) *jsonutil.Object {
	o := jsonutil.NewObject()
	o.Set("jsonrpc", "2.0")
	o.Set("id", messageID)
	o.Set("result", result)
	return o
}

// JSON-RPC error reply with its code and message
func jsonrpcError(messageID any, code int, message string) *jsonutil.Object {
	e := jsonutil.NewObject()
	e.Set("code", code)
	e.Set("message", message)
	o := jsonutil.NewObject()
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
	case *jsonutil.Object, map[string]any:
		return true
	}
	return false
}

// dict.get of a decoded object, nil for a missing key
func dictGet(v any, key string) any {
	switch d := v.(type) {
	case *jsonutil.Object:
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
	case *jsonutil.Object:
		_, ok := d.Get(key)
		return ok
	case map[string]any:
		_, ok := d[key]
		return ok
	}
	return false
}

// Value under key of a decoded object, an error naming the key and what was found for a value that is not an object
func attrGet(v any, key string) (any, error) {
	if isDict(v) {
		return dictGet(v, key), nil
	}
	return nil, kindErr("AttributeError", "cannot read %q from %s where a mapping was expected", key, errchain.Kind(v))
}

// Value or its fallback when falsy, the Python or operator
func or(v, fallback any) any {
	if anyutil.IsTrue(v) {
		return v
	}
	return fallback
}

// Stripped str of a value, falsy values reading as empty
func strField(v any) string {
	if !anyutil.IsTrue(v) {
		return ""
	}
	return strings.TrimSpace(anyutil.String(v))
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
		for _, r := range []rune(x) {
			out = append(out, string(r))
		}
		return out, nil
	case *jsonutil.Object:
		out := make([]any, x.Len())
		for i, k := range x.Keys() {
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
	return nil, kindErr("TypeError", "'%s' object is not iterable", anyutil.Kind(v))
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
		if anyutil.Equal(dictGet(option, "category"), "model") || anyutil.Equal(dictGet(option, "id"), "model") {
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
	if anyutil.IsTrue(option) {
		return enabledIDList(dictGet(option, "options"), "value", disabled)
	}
	return legacySessionModelIDs(session, disabled)
}

// Model ids of the pre stabilization availableModels extension
func legacySessionModelIDs(session any, disabled func(any) (bool, error)) ([]string, error) {
	available, err := attrGet(or(dictGet(session, "models"), jsonutil.NewObject()), "availableModels")
	if err != nil {
		return nil, err
	}
	return enabledIDList(available, "modelId", disabled)
}

// ACP request selecting the model for the session, false for unknown, disabled or session default ids
func modelSelectionRequest(session any, requestedModel, placeholder string, disabled func(any) (bool, error)) (string, *jsonutil.Object, bool, error) {
	sessionID := strField(dictGet(session, "sessionId"))
	requestedModel = strings.TrimSpace(requestedModel)
	if sessionID == "" || requestedModel == "" || requestedModel == placeholder {
		return "", nil, false, nil
	}
	option, err := modelConfigOption(session)
	if err != nil {
		return "", nil, false, err
	}
	params := jsonutil.NewObject()
	params.Set("sessionId", sessionID)
	if anyutil.IsTrue(option) {
		enabled, err := enabledIDList(dictGet(option, "options"), "value", disabled)
		if err != nil {
			return "", nil, false, err
		}
		if !containsString(enabled, requestedModel) {
			return "", nil, false, nil
		}
		params.Set("configId", anyutil.String(or(dictGet(option, "id"), "model")))
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
		return "", kindErr("PermissionError", "%s", ntErr)
	}
	if !filepath.IsAbs(filepath.Clean(pathText)) {
		return "", kindErr("PermissionError", "ACP file-system paths must be absolute.")
	}
	resolved, err := pathutil.Resolve(pathText)
	if err != nil {
		return "", err
	}
	root, err := pathutil.Resolve(cwd)
	if err != nil {
		return "", err
	}
	if _, ok := pathutil.RelativeTo(resolved, root); !ok {
		return "", kindErr("PermissionError", "Path '%s' is outside the session cwd '%s'.", resolved, root)
	}
	return resolved, nil
}

// Wall clock seconds of a number or a timeout object, its largest component winning
func effectiveTimeout(timeout any) float64 {
	if f, ok := numberOf(timeout); ok {
		return f
	}
	var candidates []float64
	switch t := timeout.(type) {
	case httputil.Timeout:
		candidates = durationSeconds(t.Read, t.Write, t.Connect, t.Pool)
	case *httputil.Timeout:
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
					if n, ok := numberOf(f.Interface()); ok {
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

// Float of a number or bool value, true and false as 1 and 0
func numberOf(v any) (float64, bool) {
	switch n := v.(type) {
	case bool:
		if n {
			return 1, true
		}
		return 0, true
	case *float64:
		if n != nil {
			return *n, true
		}
		return 0, false
	}
	return anyutil.Float(v)
}

// Integer value of a JSON number, bools counting as 1 and 0
func wholeNumber(v any) (int64, bool) {
	if b, ok := v.(bool); ok {
		if b {
			return 1, true
		}
		return 0, true
	}
	return anyutil.Int(v)
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
		return nil, kindErr("PermissionError", "%s", block)
	}
	content := ""
	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		if content, err = textutil.DecodeUTF8(textutil.TrimBOM(raw)); err != nil {
			return nil, err
		}
		content = strings.ReplaceAll(strings.ReplaceAll(content, "\r\n", "\n"), "\r", "\n")
	case errors.Is(err, fs.ErrNotExist):
	default:
		return nil, err
	}
	line, limit := dictGet(params, "line"), dictGet(params, "limit")
	if l, ok := wholeNumber(line); ok && l > 1 {
		lines := slices.Collect(strings.Lines(content))
		start := clampIndex(l-1, len(lines))
		end := len(lines)
		if n, ok := wholeNumber(limit); ok && n > 0 {
			end = start + clampIndex(n, len(lines)-start)
		}
		content = ""
		if start < end {
			content = strings.Join(lines[start:end], "")
		}
	}
	out := jsonutil.NewObject()
	if content != "" {
		content = tools.RedactSensitiveTextWith(env, content, tools.RedactOptions{Force: true})
	}
	out.Set("content", content)
	return out, nil
}

// Non negative slice index clamped to the sequence size
func clampIndex(i int64, size int) int {
	if i > int64(size) {
		return size
	}
	return int(i)
}

// str of a params entry with falsy values reading as empty, the AttributeError for params that are no dict
func paramString(params any, key string) (string, error) {
	v, err := attrGet(params, key)
	if err != nil {
		return "", err
	}
	if !anyutil.IsTrue(v) {
		return "", nil
	}
	return anyutil.String(v), nil
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
		return nil, kindErr("PermissionError", "%s", denied)
	}
	if tools.IsWriteApprovalRequired(env, path) {
		return nil, kindErr("PermissionError", "Write denied: '%s' requires interactive approval and cannot be written through the ACP file bridge.", path)
	}
	if err := os.MkdirAll(filepath.Dir(filepath.Clean(path)), 0o777); err != nil {
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
		return nil, kindErr("UnicodeEncodeError", "'utf-8' codec can't encode character '\\u%04x' in position %d: surrogates not allowed", surrogateAt(content, pos), pos)
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
	for i, r := range []rune(s) {
		if r >= 0xd800 && r <= 0xdfff {
			return i, true
		}
	}
	return 0, false
}

// Surrogate code point at a code point position
func surrogateAt(s string, pos int) rune { return []rune(s)[pos] }

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
func logWarning(format string, args ...any) { pkgLog().Warn(fmt.Sprintf(format, args...)) }

// Start error of the agent CLI, true when Python raises FileNotFoundError for it
func isNotFound(err error) bool {
	return errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist)
}
