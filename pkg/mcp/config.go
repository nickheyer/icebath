package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Logger every MCP diagnostic goes through, read at call time so tests can swap the default
func mcpLogger() *slog.Logger { return slog.Default() }

// Python str of a config value
func pyStr(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case nil:
		return "None"
	case bool:
		if x {
			return "True"
		}
		return "False"
	case float64:
		return pyFloatRepr(x)
	case float32:
		return pyFloatRepr(float64(x))
	case []any, map[string]any, []string:
		return pyRepr(v)
	}
	return fmt.Sprint(v)
}

// Python repr of a config value with sorted mapping keys
func pyRepr(v any) string {
	switch x := v.(type) {
	case string:
		return pyReprStr(x)
	case []any:
		parts := make([]string, len(x))
		for i, item := range x {
			parts[i] = pyRepr(item)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case []string:
		parts := make([]string, len(x))
		for i, item := range x {
			parts[i] = pyReprStr(item)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case map[string]any:
		keys := orderedKeys(x)
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = pyReprStr(k) + ": " + pyRepr(x[k])
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	return pyStr(v)
}

// Python repr of a string, single quoted unless only double quotes avoid escaping
func pyReprStr(s string) string {
	quote := byte('\'')
	if strings.Contains(s, "'") && !strings.Contains(s, `"`) {
		quote = '"'
	}
	var b strings.Builder
	b.WriteByte(quote)
	for _, r := range s {
		switch {
		case r == rune(quote) || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte(quote)
	return b.String()
}

// Python repr of a float using the shortest round trip digits and repr exponent thresholds
func pyFloatRepr(f float64) string {
	switch {
	case math.IsNaN(f):
		return "nan"
	case math.IsInf(f, 1):
		return "inf"
	case math.IsInf(f, -1):
		return "-inf"
	}
	sci := strconv.FormatFloat(f, 'e', -1, 64)
	sign := ""
	if strings.HasPrefix(sci, "-") {
		sign, sci = "-", sci[1:]
	}
	mant, expText, _ := strings.Cut(sci, "e")
	exp, _ := strconv.Atoi(expText)
	digits := strings.Replace(mant, ".", "", 1)
	if exp < -4 || exp >= 16 {
		m := digits[:1]
		if len(digits) > 1 {
			m += "." + digits[1:]
		}
		es := fmt.Sprintf("%+03d", exp)
		return sign + m + "e" + es
	}
	if exp < 0 {
		return sign + "0." + strings.Repeat("0", -exp-1) + digits
	}
	if len(digits) <= exp+1 {
		return sign + digits + strings.Repeat("0", exp+1-len(digits)) + ".0"
	}
	return sign + digits[:exp+1] + "." + digits[exp+1:]
}

// Python truthiness of a config value
func pyTruthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case int:
		return x != 0
	case int64:
		return x != 0
	case uint64:
		return x != 0
	case float64:
		return x != 0
	case []any:
		return len(x) > 0
	case []string:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	return true
}

// Sorted keys of a mapping, the stable stand in for Python insertion order
func orderedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Bytes decoded as UTF-8 with one replacement character per maximal invalid subpart like errors=replace
func decodeReplace(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	var sb strings.Builder
	for i := 0; i < len(b); {
		r, size := utf8.DecodeRune(b[i:])
		if r != utf8.RuneError || size > 1 {
			sb.Write(b[i : i+size])
			i += size
			continue
		}
		sb.WriteRune(utf8.RuneError)
		i += maximalSubpart(b[i:])
	}
	return sb.String()
}

// Length of the longest prefix that starts a well formed UTF-8 sequence, at least one byte
func maximalSubpart(b []byte) int {
	c := b[0]
	var need int
	lo, hi := byte(0x80), byte(0xBF)
	switch {
	case c >= 0xC2 && c <= 0xDF:
		need = 1
	case c == 0xE0:
		need, lo = 2, 0xA0
	case c >= 0xE1 && c <= 0xEC, c == 0xEE, c == 0xEF:
		need = 2
	case c == 0xED:
		need, hi = 2, 0x9F
	case c == 0xF0:
		need, lo = 3, 0x90
	case c >= 0xF1 && c <= 0xF3:
		need = 3
	case c == 0xF4:
		need, hi = 3, 0x8F
	default:
		return 1
	}
	n := 1
	for k := 0; k < need && n < len(b); k++ {
		x := b[n]
		if k == 0 {
			if x < lo || x > hi {
				return n
			}
		} else if x < 0x80 || x > 0xBF {
			return n
		}
		n++
	}
	return n
}

// Seconds an MCP tool call may run when nothing more specific is configured
const defaultToolTimeout = 300.0

// Seconds the first connection to a server may take
const defaultConnectTimeout = 60.0

// Keepalive cadence for remote servers and the floor that stops a tiny interval busy looping
const (
	defaultKeepaliveInterval = 180.0
	minKeepaliveInterval     = 5.0
)

// Per server tool call timeout, the server key over timeouts.mcp.tool_call over the 300 second default
func resolveToolTimeout(cfg map[string]any, tree config.Tree) float64 {
	if perServer, ok := cfg["timeout"]; ok && perServer != nil {
		if f, ok := config.AsFloat(perServer); ok {
			return f
		}
		mcpLogger().Warn(fmt.Sprintf("MCP config timeout must be a number of seconds; ignoring %s", pyRepr(perServer)))
	}
	if d, ok := resolveConfiguredTimeout(tree, "mcp.tool_call", time.Duration(defaultToolTimeout*float64(time.Second))); ok {
		return d.Seconds()
	}
	return defaultToolTimeout
}

// Bare env var name of a reference body with a Cursor style env prefix removed
func envRefName(ref string) string {
	ref = strings.TrimSpace(ref)
	if strings.HasPrefix(ref, "env:") {
		ref = strings.TrimSpace(ref[len("env:"):])
	}
	return ref
}

// Env copy with directory first on PATH unless already present
func prependPath(env map[string]string, directory string) map[string]string {
	updated := make(map[string]string, len(env)+1)
	for k, v := range env {
		updated[k] = v
	}
	if directory != "" {
		var parts []string
		for _, p := range strings.Split(updated["PATH"], string(os.PathListSeparator)) {
			if p != "" {
				parts = append(parts, p)
			}
		}
		found := false
		for _, p := range parts {
			if p == directory {
				found = true
				break
			}
		}
		if !found {
			parts = append([]string{directory}, parts...)
		}
		if len(parts) > 0 {
			updated["PATH"] = strings.Join(parts, string(os.PathListSeparator))
		} else {
			updated["PATH"] = directory
		}
	}
	return updated
}

// Integer view of a config value the way Python int coerces it, ok false where int raises
func pyInt(value any) (int, bool) {
	switch v := value.(type) {
	case bool:
		if v {
			return 1, true
		}
		return 0, true
	case int:
		return v, true
	case int64:
		return int(v), true
	case uint64:
		return int(v), true
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return 0, false
		}
		return int(v), true
	case string:
		s := strings.TrimSpace(v)
		if !underscoresValid(s) {
			return 0, false
		}
		if i, err := strconv.Atoi(strings.ReplaceAll(s, "_", "")); err == nil {
			return i, true
		}
	}
	return 0, false
}

// Reports whether every underscore in a numeric literal sits between two digits as PEP 515 requires
func underscoresValid(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] != '_' {
			continue
		}
		if i == 0 || i == len(s)-1 || s[i-1] < '0' || s[i-1] > '9' || s[i+1] < '0' || s[i+1] > '9' {
			return false
		}
	}
	return true
}

// Float view of a config value the way Python float coerces it, ok false where float raises
func pyFloat(value any) (float64, bool) {
	switch v := value.(type) {
	case bool:
		if v {
			return 1, true
		}
		return 0, true
	case int, int64, uint64, float64:
		return config.AsFloat(v)
	case string:
		s := strings.ToLower(strings.TrimSpace(v))
		switch strings.TrimLeft(s, "+-") {
		case "inf", "infinity", "nan":
			f, err := strconv.ParseFloat(s, 64)
			return f, err == nil
		}
		if !underscoresValid(s) || strings.ContainsAny(s, "xXpP") {
			return 0, false
		}
		f, err := strconv.ParseFloat(strings.ReplaceAll(s, "_", ""), 64)
		return f, err == nil
	}
	return 0, false
}

// Config value coerced to an integer clamped to minimum, the default on failure
func safeInt(value any, def, minimum int) int {
	n, ok := pyInt(value)
	if !ok {
		return def
	}
	return max(n, minimum)
}

// Config value coerced to a float clamped to minimum, the default on failure or a non finite value
func safeFloat(value any, def, minimum float64) float64 {
	f, ok := pyFloat(value)
	if !ok || math.IsNaN(f) || math.IsInf(f, 0) {
		return def
	}
	return math.Max(f, minimum)
}

var (
	trueWords  = map[string]bool{"true": true, "1": true, "yes": true, "on": true}
	falseWords = map[string]bool{"false": true, "0": true, "no": true, "off": true}
)

// Boolean view of a config value, numbers by truthiness, words from a fixed set, else the default with a warning
func parseBoolish(value any, def bool) bool {
	switch v := value.(type) {
	case nil:
		return def
	case bool:
		return v
	case int, int64, uint64, float64:
		return pyTruthy(v)
	case string:
		lowered := strings.ToLower(strings.TrimSpace(v))
		if trueWords[lowered] {
			return true
		}
		if falseWords[lowered] {
			return false
		}
	}
	mcpLogger().Warn(fmt.Sprintf("MCP config expected a boolean-ish value, got %s; using default=%s", pyRepr(value), pyStr(def)))
	return def
}

// Reports whether a server entry is on, absent null or unparseable counting as on
func ServerEnabled(cfg map[string]any) bool {
	v, ok := cfg["enabled"]
	if !ok {
		v = true
	}
	return parseBoolish(v, true)
}

// Optional positive lifecycle limit in seconds from the entry or its lifecycle block, zero disables
func lifecycleSeconds(cfg map[string]any, key string) (float64, bool) {
	raw, ok := cfg[key]
	if !ok || raw == nil {
		if lc, isMap := cfg["lifecycle"].(map[string]any); isMap {
			raw = lc[key]
		}
	}
	if raw == nil {
		return 0, false
	}
	seconds, ok := pyFloat(raw)
	if !ok {
		mcpLogger().Warn(fmt.Sprintf("MCP config %s must be a number of seconds; ignoring %s", key, pyRepr(raw)))
		return 0, false
	}
	if seconds < 0 {
		mcpLogger().Warn(fmt.Sprintf("MCP config %s must be positive; ignoring %s", key, pyRepr(raw)))
		return 0, false
	}
	if seconds == 0 {
		return 0, false
	}
	return seconds, true
}

// Local time in the logging asctime shape the log commands parse
func stderrTimestamp() string {
	return time.Now().Format("2006-01-02 15:04:05,000")
}

var stampPrefixRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2},\d{3}(?:\s|$)`)

// Line with a leading timestamp unless it already carries one, ending in exactly one newline
func stampLine(line string) string {
	rendered := strings.TrimRight(line, "\r\n")
	if stampPrefixRe.MatchString(rendered) {
		return rendered + "\n"
	}
	return stderrTimestamp() + " " + rendered + "\n"
}

var (
	stderrLogMu sync.Mutex
	stderrLogs  = map[string]*os.File{}
)

// Shared append handle for MCP child stderr cached per profile home, devnull then real stderr on failure
func getStderrLog(ctx context.Context) *os.File {
	h := home.Dir(ctx)
	key := home.Key(h)
	stderrLogMu.Lock()
	defer stderrLogMu.Unlock()
	if fh := stderrLogs[key]; fh != nil {
		return fh
	}
	logDir := home.LogsDir(h)
	var fh *os.File
	_, err := home.Mkdir(ctx, logDir)
	if err == nil {
		fh, err = os.OpenFile(filepath.Join(logDir, "mcp-stderr.log"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	}
	if err != nil {
		mcpLogger().Debug(fmt.Sprintf("Failed to open MCP stderr log, using devnull: %v", err))
		fh, err = os.OpenFile(os.DevNull, os.O_WRONLY, 0)
		if err != nil {
			fh = os.Stderr
		}
	}
	stderrLogs[key] = fh
	return fh
}

// Releases cached stderr log handles of one home key, every home when scope is empty
func closeStderrLogs(scope string) {
	stderrLogMu.Lock()
	defer stderrLogMu.Unlock()
	keys := []string{scope}
	if scope == "" {
		keys = keys[:0]
		for k := range stderrLogs {
			keys = append(keys, k)
		}
	}
	for _, k := range keys {
		fh := stderrLogs[k]
		delete(stderrLogs, k)
		if fh == nil || fh == os.Stderr {
			continue
		}
		if err := fh.Close(); err != nil {
			mcpLogger().Warn(fmt.Sprintf("Could not close MCP stderr log for %s", k), "error", err)
		}
	}
}

// Bytes of child stderr kept readable after the child dies
const stderrTailBytes = 16384

// Child stderr copied into the shared log one stamped line at a time while the last bytes stay readable
type stderrTee struct {
	Sink    *os.File
	log     io.Writer
	mu      sync.Mutex
	tail    []byte
	drained chan struct{}
}

// Starts a tee whose write end the child inherits as its stderr
func newStderrTee(log io.Writer) (*stderrTee, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	t := &stderrTee{Sink: w, log: log, drained: make(chan struct{})}
	go t.pump(r)
	return t, nil
}

// Tee copying child stderr into log as stamped lines, the writer of mcp-stderr.log
func NewStderrTee(log io.Writer) (*stderrTee, error) { return newStderrTee(log) }

// Copies the pipe into the log, stamping whole lines and holding a partial line until its newline or EOF
func (t *stderrTee) pump(r *os.File) {
	defer close(t.drained)
	defer r.Close()
	var pending []byte
	buf := make([]byte, 65536)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			t.mu.Lock()
			t.tail = append(t.tail, chunk...)
			if len(t.tail) > stderrTailBytes {
				t.tail = append([]byte(nil), t.tail[len(t.tail)-stderrTailBytes:]...)
			}
			t.mu.Unlock()
			pending = append(pending, chunk...)
			if idx := strings.LastIndexByte(string(pending), '\n'); idx >= 0 {
				t.writeLines(strings.Split(decodeReplace(pending[:idx]), "\n"))
				pending = append([]byte(nil), pending[idx+1:]...)
			}
		}
		if err != nil {
			break
		}
	}
	if len(pending) > 0 {
		t.writeLines([]string{decodeReplace(pending)})
	}
}

// Writes stamped lines to the log, a closed log is ignored so the child keeps draining
func (t *stderrTee) writeLines(lines []string) {
	if len(lines) == 0 {
		return
	}
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(stampLine(l))
	}
	_, _ = io.WriteString(t.log, b.String())
}

// Closes the write end, waits up to timeout for the reader to drain, and returns the captured tail
func (t *stderrTee) Close(timeout time.Duration) string {
	t.Sink.Close()
	select {
	case <-t.drained:
	case <-time.After(timeout):
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return decodeReplace(t.tail)
}

// Writes the session marker that leads each server's segment of the shared stderr log
func writeStderrLogHeader(ctx context.Context, serverName string) {
	fh := getStderrLog(ctx)
	_, _ = fmt.Fprintf(fh, "\n%s ===== starting MCP server '%s' =====\n", stderrTimestamp(), serverName)
}

// Trailing non blank lines this server last wrote to the shared stderr log, empty when none
func tailServerStderr(ctx context.Context, serverName string, maxBytes, maxLines int) string {
	fh := getStderrLog(ctx)
	path := fh.Name()
	if path == "" || path == os.DevNull || fh == os.Stderr {
		return ""
	}
	raw, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer raw.Close()
	st, err := raw.Stat()
	if err != nil {
		return ""
	}
	start := max(int64(0), st.Size()-int64(maxBytes))
	data := make([]byte, st.Size()-start)
	if _, err := raw.ReadAt(data, start); err != nil && !errors.Is(err, io.EOF) {
		return ""
	}
	chunk := decodeReplace(data)
	cut := strings.LastIndex(chunk, fmt.Sprintf("starting MCP server '%s'", serverName))
	if cut < 0 {
		return ""
	}
	var lines []string
	for _, line := range pySplitlines(chunk[cut:]) {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	return strings.Join(lines, "\n")
}

// Lines split on every boundary str.splitlines recognizes, terminators dropped
func pySplitlines(s string) []string {
	var out []string
	start := 0
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		switch r {
		case '\n', '\r', '\v', '\f', 0x1c, 0x1d, 0x1e, 0x85, 0x2028, 0x2029:
			out = append(out, string(rs[start:i]))
			if r == '\r' && i+1 < len(rs) && rs[i+1] == '\n' {
				i++
			}
			start = i + 1
		}
	}
	if start < len(rs) {
		out = append(out, string(rs[start:]))
	}
	return out
}

// Env vars safe to pass to stdio children because they carry no secrets
var safeEnvKeys = map[string]bool{"PATH": true, "HOME": true, "USER": true, "LANG": true, "LC_ALL": true, "TERM": true, "SHELL": true, "TMPDIR": true}

// Windows process and location vars launcher style tools need, matched case insensitively
var safeEnvKeysCaseInsensitive = map[string]bool{
	"ALLUSERSPROFILE": true, "APPDATA": true, "COMMONPROGRAMFILES": true, "COMMONPROGRAMFILES(X86)": true,
	"COMMONPROGRAMW6432": true, "COMPUTERNAME": true, "COMSPEC": true, "HOMEDRIVE": true, "HOMEPATH": true,
	"LOCALAPPDATA": true, "NUMBER_OF_PROCESSORS": true, "OS": true, "PATHEXT": true, "PROCESSOR_ARCHITECTURE": true,
	"PROGRAMDATA": true, "PROGRAMFILES": true, "PROGRAMFILES(X86)": true, "PROGRAMW6432": true, "PUBLIC": true,
	"SYSTEMDRIVE": true, "SYSTEMROOT": true, "TEMP": true, "TMP": true, "USERDOMAIN": true, "USERNAME": true,
	"USERPROFILE": true, "WINDIR": true,
}

// Filtered env for a stdio child, the safe baseline plus source injected secrets, the pinned home and the entry env
func buildSafeEnv(ctx context.Context, env *tools.Env, userEnv map[string]any, unsetRef func(key string) bool) (map[string]string, error) {
	out := map[string]string{}
	for _, kv := range os.Environ() {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || (runtime.GOOS == "windows" && k == "") {
			continue
		}
		if safeEnvKeys[k] || safeEnvKeysCaseInsensitive[strings.ToUpper(k)] || strings.HasPrefix(k, "XDG_") {
			out[k] = v
		}
	}
	for _, key := range secretSourceNamesFn() {
		value, found, err := getSecret(ctx, env, key)
		if err != nil {
			return nil, err
		}
		if found {
			out[key] = value
		}
	}
	for _, key := range []string{"ICEBATH_KANBAN_DB", "ICEBATH_KANBAN_BOARD"} {
		if v, ok := os.LookupEnv(key); ok {
			out[key] = v
		}
	}
	out[home.EnvHome] = home.Dir(ctx)
	for _, key := range orderedKeys(userEnv) {
		if unsetRef != nil && unsetRef(key) {
			continue
		}
		out[key] = pyStr(userEnv[key])
	}
	return tools.DelegatedChildSubprocessEnv(env, out), nil
}

// Executable path under the config env PATHEXT on Windows, empty when it adds nothing over the parent lookup
func whichWithConfigPathext(command, pathArg string, env map[string]string) string {
	cfgPathext := ""
	for _, k := range sortedStringKeys(env) {
		if strings.ToUpper(k) == "PATHEXT" && strings.TrimSpace(env[k]) != "" {
			cfgPathext = env[k]
			break
		}
	}
	if cfgPathext == "" || cfgPathext == os.Getenv("PATHEXT") {
		return ""
	}
	var exts []string
	for _, e := range strings.Split(cfgPathext, ";") {
		if e != "" {
			exts = append(exts, e)
		}
	}
	candidates := make([]string, 0, len(exts))
	matched := false
	for _, e := range exts {
		candidates = append(candidates, command+e)
		if strings.HasSuffix(strings.ToLower(command), strings.ToLower(e)) {
			matched = true
		}
	}
	if len(candidates) == 0 || matched {
		candidates = []string{command}
	}
	dirs := strings.Split(pathArg, string(os.PathListSeparator))
	if runtime.GOOS == "windows" && !containsString(dirs, ".") {
		dirs = append([]string{"."}, dirs...)
	}
	for _, raw := range dirs {
		dir := raw
		if dir == "" {
			dir = "."
		}
		if st, err := os.Stat(dir); err != nil || !st.IsDir() {
			continue
		}
		for _, c := range candidates {
			resolved := filepath.Join(dir, c)
			if isExecutableFile(resolved) {
				return resolved
			}
		}
	}
	return ""
}

// Reports whether a path is an existing regular file the process may execute
func isExecutableFile(p string) bool {
	st, err := os.Stat(p)
	if err != nil || !st.Mode().IsRegular() {
		return false
	}
	if runtime.GOOS == "windows" {
		return true
	}
	return accessX(p)
}

// Sorted keys of a string map
func sortedStringKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Reports whether a list holds a string
func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// Bare MCP launchers shipped through the package manager keyed to the package providing them
var managedLaunchers = map[string]string{"npx": "npm", "npm": "npm", "node": "npm", "uv": "uv", "uvx": "uv"}

// Managed executable for a bare launcher and the toolchain dirs its children need first, false without a build here
func managedLauncher(ctx context.Context, command string) (string, []string, bool, error) {
	pkgName := managedLaunchers[command]
	missing, err := pmMissingReason(pkgName)
	if err != nil {
		return "", nil, false, err
	}
	if missing != "" {
		return "", nil, false, nil
	}
	if err := pmEnsure(ctx, pkgName); err != nil {
		return "", nil, false, err
	}
	var dirs []string
	if pkgName == "uv" {
		launcher, err := pmUVLauncher(ctx, command)
		if err != nil {
			return "", nil, false, err
		}
		if launcher != "" {
			dirs = []string{filepath.Dir(launcher)}
		}
	} else {
		for _, d := range strings.Split(pmNodeEnv(map[string]string{"PATH": ""})["PATH"], string(os.PathListSeparator)) {
			if d != "" {
				dirs = append(dirs, d)
			}
		}
	}
	executable := ""
	if len(dirs) > 0 {
		executable = pyWhich(command, strings.Join(dirs, string(os.PathListSeparator)), true, os.Getenv("PATHEXT"))
	}
	if executable == "" {
		return "", nil, false, fmt.Errorf("icebath-managed %s is not installed; run `icebath pm install %s`", command, pkgName)
	}
	return executable, dirs, true, nil
}

// Package manager npm or node launcher and dirs swapped for the copies outside a sealed Windows payload
func outsidePackage(ctx context.Context, launcher, managed string, dirs []string) (string, []string, error) {
	moved, err := pmUserFacingCopies(ctx, dirs)
	if err != nil {
		return "", nil, err
	}
	if equalStrings(moved, dirs) {
		return managed, dirs, nil
	}
	if hit := pyWhich(launcher, strings.Join(moved, string(os.PathListSeparator)), true, os.Getenv("PATHEXT")); hit != "" {
		return hit, moved, nil
	}
	return managed, moved, nil
}

// Reports whether two string lists are equal element by element
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Reports whether a dir is one the bootstrap prepends, under the active home or the running binary's own dir
func isManagedBinDir(ctx context.Context, directory string) bool {
	resolved, err := pyResolve(directory)
	if err != nil {
		return false
	}
	if exe, err := os.Executable(); err == nil {
		if r, err := pyResolve(exe); err == nil && filepath.Dir(r) == resolved {
			return true
		}
	}
	h, err := pyResolve(home.Dir(ctx))
	if err != nil {
		return false
	}
	if resolved == filepath.Join(h, "bin") {
		return true
	}
	rel, err := filepath.Rel(h, resolved)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// Absolute path with symlinks resolved as far as they exist, like Path.resolve without strict
func pyResolve(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	if r, err := filepath.EvalSymlinks(abs); err == nil {
		return r, nil
	}
	dir, base := filepath.Split(abs)
	dir = filepath.Clean(dir)
	if dir == abs {
		return abs, nil
	}
	parent, err := pyResolve(dir)
	if err != nil {
		return "", err
	}
	return filepath.Join(parent, base), nil
}

// Windows executable suffixes cmd.exe resolves when no PATHEXT is set
const windowsDefaultPathext = ".COM;.EXE;.BAT;.CMD;.VBS;.VBE;.JS;.JSE;.WSF;.WSH;.MSC"

// Executable suffixes a bare name resolves through, the child PATHEXT then the parent then the OS default
func pathextSuffixes(env map[string]string, windows bool) []string {
	if !windows {
		return []string{""}
	}
	parent := map[string]string{}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			parent[k] = v
		}
	}
	for _, source := range []map[string]string{env, parent} {
		for _, k := range sortedStringKeys(source) {
			if strings.ToUpper(k) != "PATHEXT" || strings.TrimSpace(source[k]) == "" {
				continue
			}
			var exts []string
			for _, e := range strings.Split(source[k], ";") {
				if e != "" {
					exts = append(exts, e)
				}
			}
			if len(exts) > 0 {
				return exts
			}
			break
		}
	}
	var exts []string
	for _, e := range strings.Split(windowsDefaultPathext, ";") {
		if e != "" {
			exts = append(exts, e)
		}
	}
	return exts
}

// First PATH hit for a command outside managed bin dirs, empty when only managed dirs hold it
func firstUserWhichHit(ctx context.Context, command, pathArg string, env map[string]string, windows bool) string {
	exts := pathextSuffixes(env, windows)
	names := []string{}
	suffixed := false
	for _, e := range exts {
		if e != "" && strings.HasSuffix(strings.ToLower(command), strings.ToLower(e)) {
			suffixed = true
		}
	}
	if suffixed {
		names = []string{command}
	} else {
		for _, e := range exts {
			names = append(names, command+e)
		}
	}
	for _, dir := range strings.Split(pathArg, string(os.PathListSeparator)) {
		if dir == "" || isManagedBinDir(ctx, dir) {
			continue
		}
		for _, n := range names {
			c := filepath.Join(dir, n)
			if isExecutableFile(c) {
				return c
			}
		}
	}
	return ""
}

// Executable a bare or pathed command resolves to on a PATH string the way shutil.which searches it
func pyWhich(cmd, path string, hasPath bool, pathext string) string {
	check := func(fn string) bool {
		st, err := os.Stat(fn)
		return err == nil && !st.IsDir() && (runtime.GOOS == "windows" || accessX(fn))
	}
	if strings.ContainsAny(cmd, `/`) || (runtime.GOOS == "windows" && strings.ContainsAny(cmd, `\:`)) {
		if check(cmd) {
			return cmd
		}
		return ""
	}
	if !hasPath {
		path = os.Getenv("PATH")
	}
	if path == "" {
		return ""
	}
	dirs := strings.Split(path, string(os.PathListSeparator))
	files := []string{cmd}
	if runtime.GOOS == "windows" {
		if !containsString(dirs, ".") {
			dirs = append([]string{"."}, dirs...)
		}
		if pathext == "" {
			pathext = windowsDefaultPathext
		}
		var exts []string
		for _, e := range strings.Split(pathext, string(os.PathListSeparator)) {
			if e != "" {
				exts = append(exts, strings.ToLower(e))
			}
		}
		hasExt := false
		for _, e := range exts {
			if strings.HasSuffix(strings.ToLower(cmd), e) {
				hasExt = true
			}
		}
		if hasExt {
			files = []string{cmd}
		} else {
			files = files[:0]
			files = append(files, cmd)
			for _, e := range exts {
				files = append(files, cmd+e)
			}
		}
	}
	seen := map[string]bool{}
	for _, dir := range dirs {
		norm := dir
		if runtime.GOOS == "windows" {
			norm = strings.ToLower(dir)
		}
		if seen[norm] {
			continue
		}
		seen[norm] = true
		for _, f := range files {
			name := f
			if dir != "" {
				name = filepath.Join(dir, f)
			}
			if check(name) {
				return name
			}
		}
	}
	return ""
}

// Launchers that keep managed first resolution instead of stepping past managed dirs
var launcherFamily = map[string]bool{"npx": true, "npm": true, "node": true, "uv": true, "uvx": true}

var windowsLauncherSuffixRe = regexp.MustCompile(`(?i)\.(cmd|exe)$`)

// Stdio command resolved against the exact child env, managed launchers first, bare names on the child PATH only
func resolveStdioCommand(ctx context.Context, command string, env map[string]string) (string, map[string]string, error) {
	resolved := expandUser(strings.TrimSpace(command))
	resolvedEnv := make(map[string]string, len(env))
	for k, v := range env {
		resolvedEnv[k] = v
	}
	launcher := windowsLauncherSuffixRe.ReplaceAllString(resolved, "")
	if _, isLauncher := managedLaunchers[launcher]; isLauncher {
		executable, dirs, ok, err := managedLauncher(ctx, launcher)
		if err != nil {
			return "", nil, err
		}
		if ok {
			if managedLaunchers[launcher] == "npm" {
				if executable, dirs, err = outsidePackage(ctx, launcher, executable, dirs); err != nil {
					return "", nil, err
				}
			}
			keys := map[string]bool{}
			for _, d := range dirs {
				keys[normcase(d)] = true
			}
			parts := append([]string(nil), dirs...)
			for _, p := range strings.Split(resolvedEnv["PATH"], string(os.PathListSeparator)) {
				if p != "" && !keys[normcase(p)] {
					parts = append(parts, p)
				}
			}
			resolvedEnv["PATH"] = strings.Join(parts, string(os.PathListSeparator))
			return executable, resolvedEnv, nil
		}
	}
	if !strings.Contains(resolved, string(os.PathSeparator)) {
		pathArg, hasPath := resolvedEnv["PATH"]
		hit := ""
		if hasPath {
			hit = pyWhich(resolved, pathArg, true, os.Getenv("PATHEXT"))
		}
		if hit == "" && runtime.GOOS == "windows" && len(resolvedEnv) > 0 {
			hit = whichWithConfigPathext(resolved, pathArg, resolvedEnv)
		}
		if hit != "" && !launcherFamily[resolved] {
			if user := firstUserWhichHit(ctx, resolved, pathArg, resolvedEnv, runtime.GOOS == "windows"); user != "" {
				hit = user
			}
		}
		if hit != "" {
			resolved = hit
		}
	}
	if dir := pyDirname(resolved); dir != "" {
		resolvedEnv = prependPath(resolvedEnv, dir)
	}
	return resolved, resolvedEnv, nil
}

// Directory part of a path the way os.path.dirname splits it, empty for a bare name
func pyDirname(p string) string {
	idx := strings.LastIndex(p, "/")
	if runtime.GOOS == "windows" {
		idx = strings.LastIndexAny(p, `/\`)
	}
	if idx < 0 {
		return ""
	}
	head := p[:idx+1]
	if trimmed := strings.TrimRight(head, `/\`); trimmed != "" {
		return trimmed
	}
	return head
}

// Case folded path on Windows, the path itself elsewhere
func normcase(p string) string {
	if runtime.GOOS == "windows" {
		return strings.ToLower(strings.ReplaceAll(p, "/", `\`))
	}
	return p
}

// Launcher paths to try for a name inside an npx cache bin dir, the cmd and exe shims on Windows
func npxBinCandidates(binDir, name string, windows bool) []string {
	if windows {
		return []string{filepath.Join(binDir, name+".cmd"), filepath.Join(binDir, name+".exe")}
	}
	return []string{filepath.Join(binDir, name)}
}

// Strips the -y and --yes flags npx eats before the package spec
func stripYesFlags(args []string) []string {
	rest := append([]string(nil), args...)
	for len(rest) > 0 && (rest[0] == "-y" || rest[0] == "--yes") {
		rest = rest[1:]
	}
	return rest
}

// Reports whether any arg after the spec is a yes flag npx would have eaten
func yesFlagAfterSpec(rest []string) bool {
	for _, a := range rest[1:] {
		if a == "-y" || a == "--yes" {
			return true
		}
	}
	return false
}

// JSON document of a file read with a leading BOM dropped, ok false when unreadable or not JSON
func readJSONSig(path string) (any, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	raw = []byte(strings.TrimPrefix(string(raw), "\ufeff"))
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, false
	}
	return doc, true
}

// Field of a JSON object the way (doc or {}).get(key) reads it, an error where Python raises AttributeError
func jsonGet(doc any, key string) (any, error) {
	if !pyTruthy(doc) {
		return nil, nil
	}
	m, ok := doc.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("'%s' object has no attribute 'get'", jsonTypeName(doc))
	}
	return m[key], nil
}

// Python type name of a decoded JSON value
func jsonTypeName(v any) string {
	switch v.(type) {
	case []any:
		return "list"
	case string:
		return "str"
	case float64:
		return "float"
	case bool:
		return "bool"
	case map[string]any:
		return "dict"
	}
	return "NoneType"
}

// Reports whether needle is in a decoded JSON container the way Python in tests membership
func jsonContains(container any, needle string) (bool, error) {
	switch c := container.(type) {
	case map[string]any:
		_, ok := c[needle]
		return ok, nil
	case []any:
		for _, item := range c {
			if s, ok := item.(string); ok && s == needle {
				return true, nil
			}
		}
		return false, nil
	case string:
		return strings.Contains(c, needle), nil
	}
	return false, fmt.Errorf("argument of type '%s' is not iterable", jsonTypeName(container))
}

// Installed binary and remaining args for npx -y pkg when the package already sits in the npx cache
func npxCachedBin(args []string) (string, []string, bool, error) {
	if len(args) == 0 {
		return "", nil, false, nil
	}
	rest := stripYesFlags(args)
	if len(rest) == 0 || yesFlagAfterSpec(rest) {
		return "", nil, false, nil
	}
	spec := rest[0]
	if strings.Contains(strings.TrimPrefix(spec, "@"), "@") || spec == "" || strings.HasPrefix(spec, "-") {
		return "", nil, false, nil
	}
	cacheRoot := os.Getenv("npm_config_cache")
	if cacheRoot == "" {
		cacheRoot = filepath.Join(expandUser("~"), ".npm")
	}
	npxRoot := filepath.Join(cacheRoot, "_npx")
	if st, err := os.Stat(npxRoot); err != nil || !st.IsDir() {
		return "", nil, false, nil
	}
	entries, err := os.ReadDir(npxRoot)
	if err != nil {
		return "", nil, false, nil
	}
	for _, entry := range entries {
		manifest, ok := readJSONSig(filepath.Join(npxRoot, entry.Name(), "package.json"))
		if !ok {
			continue
		}
		deps, err := jsonGet(manifest, "dependencies")
		if err != nil {
			return "", nil, false, err
		}
		if !pyTruthy(deps) {
			deps = map[string]any{}
		}
		found, err := jsonContains(deps, spec)
		if err != nil {
			continue
		}
		if !found {
			continue
		}
		pkgJSON, ok := readJSONSig(filepath.Join(npxRoot, entry.Name(), "node_modules", spec, "package.json"))
		if !ok {
			continue
		}
		binField, err := jsonGet(pkgJSON, "bin")
		if err != nil {
			return "", nil, false, err
		}
		var name string
		switch b := binField.(type) {
		case string:
			name = pyBasename(spec)
		case map[string]any:
			if len(b) != 1 {
				continue
			}
			for k := range b {
				name = k
			}
		default:
			continue
		}
		binDir := filepath.Join(npxRoot, entry.Name(), "node_modules", ".bin")
		for _, candidate := range npxBinCandidates(binDir, name, runtime.GOOS == "windows") {
			if _, err := os.Stat(candidate); err == nil && accessX(candidate) {
				return candidate, append([]string(nil), rest[1:]...), true, nil
			}
		}
	}
	return "", nil, false, nil
}

var exactVersionRe = regexp.MustCompile(`^\d+\.\d+\.\d+(?:[-+][0-9A-Za-z.+-]+)?$`)

// Ceiling on the packaged node npm install of an npx package
var npxInstallTimeout = 600 * time.Second

// Command running the packaged node npm install, the subprocess seam tests replace
var npxInstallCommand = exec.CommandContext

// Packaged node and bin script argv for npx -y pkg when the command is a sealed Windows payload's own npx
func packagedNpxEntry(ctx context.Context, command string, args []string) (string, []string, bool, error) {
	applies, err := pmUserFacingApplies(ctx)
	if err != nil || !applies {
		return "", nil, false, err
	}
	sealedNpm, err := pmSealedDirs(ctx, "npm")
	if err != nil {
		return "", nil, false, err
	}
	sealedNode, err := pmSealedDirs(ctx, "node")
	if err != nil {
		return "", nil, false, err
	}
	npmDirs := map[string]string{}
	for _, d := range sealedNpm {
		npmDirs[normcase(d)] = d
	}
	npmDir, haveNpm := npmDirs[normcase(pyDirname(command))]
	node := pyWhich("node", strings.Join(sealedNode, string(os.PathListSeparator)), true, os.Getenv("PATHEXT"))
	npmCLI := filepath.Join(npmDir, "node_modules", "npm", "bin", "npm-cli.js")
	if st, err := os.Stat(npmCLI); !haveNpm || node == "" || err != nil || !st.Mode().IsRegular() {
		return "", nil, false, nil
	}
	rest := stripYesFlags(args)
	spec := ""
	if len(rest) > 0 {
		spec = rest[0]
	}
	if spec == "" || strings.HasPrefix(spec, "-") || yesFlagAfterSpec(rest) {
		return "", nil, false, nil
	}
	if strings.Contains(spec, ":") || strings.HasPrefix(spec, ".") || strings.HasPrefix(spec, "/") || strings.HasPrefix(spec, `\`) ||
		strings.HasPrefix(spec, "~") || (strings.Contains(spec, "/") && !strings.HasPrefix(spec, "@")) {
		return "", nil, false, nil
	}
	scope, bare := "", spec
	if strings.HasPrefix(spec, "@") {
		if i := strings.LastIndex(spec, "/"); i >= 0 {
			scope, bare = spec[:i], spec[i+1:]
		} else {
			scope, bare = "", spec
		}
	}
	base, version, _ := strings.Cut(bare, "@")
	name := base
	if scope != "" {
		name = scope + "/" + base
	}
	sum := sha256.Sum256([]byte(spec))
	prefix := filepath.Join(home.Dir(ctx), "cache", "mcp-npx", hex.EncodeToString(sum[:])[:16])
	manifest := filepath.Join(prefix, "node_modules", filepath.FromSlash(name), "package.json")
	isFile := func() bool {
		st, err := os.Stat(manifest)
		return err == nil && st.Mode().IsRegular()
	}
	if !(exactVersionRe.MatchString(version) && isFile()) {
		install := []string{npmCLI, "install", "--prefix", prefix, "--no-save", "--ignore-scripts",
			"--no-audit", "--no-fund", "--loglevel=error", spec}
		runCtx, cancel := context.WithTimeout(ctx, npxInstallTimeout)
		cmd := npxInstallCommand(runCtx, node, install...)
		cmd.Stdin = nil
		var stdout, stderr strings.Builder
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		cancel()
		failed := err != nil
		output := stderr.String()
		if err != nil && cmd.ProcessState == nil {
			output = err.Error()
		} else if output == "" {
			output = stdout.String()
		}
		if failed || !isFile() {
			text := strings.TrimSpace(output)
			if r := []rune(text); len(r) > 500 {
				text = string(r[len(r)-500:])
			}
			mcpLogger().Warn(fmt.Sprintf("npm install of %s with the packaged node failed: %s", spec, text))
			if !isFile() {
				return "", nil, false, nil
			}
		}
	}
	doc, ok := readJSONSig(manifest)
	if !ok {
		return "", nil, false, nil
	}
	binField, err := jsonGet(doc, "bin")
	if err != nil {
		return "", nil, false, nil
	}
	if m, isMap := binField.(map[string]any); isMap {
		if v := m[base]; pyTruthy(v) {
			binField = v
		} else if len(m) == 1 {
			for _, v := range m {
				binField = v
			}
		} else {
			binField = nil
		}
	}
	script, isStr := binField.(string)
	if !isStr {
		return "", nil, false, nil
	}
	return node, append([]string{filepath.Join(filepath.Dir(manifest), filepath.FromSlash(script))}, rest[1:]...), true, nil
}

// Absolute workspace root for workspaceFolder, the authoritative session root else the launch directory
func workspaceFolder(env *tools.Env) string {
	if root, err := tools.AuthoritativeWorkspaceRoot(env); err == nil && root != "" {
		return root
	}
	if env != nil && env.Cwd != "" {
		return env.Cwd
	}
	wd, _ := os.Getwd()
	return wd
}

// Final component of the workspace root, the root itself when it has none
func workspaceBasename(env *tools.Env) string {
	root := workspaceFolder(env)
	if b := pyBasename(strings.TrimRight(root, `/\`)); b != "" {
		return b
	}
	return root
}

// Cursor style case sensitive context variable value, ok false for any other name
func contextVar(env *tools.Env, name string) (string, bool) {
	switch name {
	case "userHome":
		return expandUser("~"), true
	case "workspaceFolder":
		return workspaceFolder(env), true
	case "workspaceFolderBasename":
		return workspaceBasename(env), true
	case "pathSeparator", "/":
		return string(os.PathSeparator), true
	}
	return "", false
}

var envVarPattern = regexp.MustCompile(`\$\{([^}]+)\}`)

// How one templated config string resolved, kept so a re-render retries only what missed
type renderRecord struct {
	template string
	missed   bool
	unsetRef bool
}

// Rendered is one server entry after reference rendering with how each templated string resolved
type Rendered struct {
	Config map[string]any
	strs   map[string]renderRecord
}

// Path key of one config string, components joined by a unit separator
func renderPath(parts ...string) string { return strings.Join(parts, "\x1f") }

// Renders references in a value tree, keeping strings that already resolved and retrying ones that missed
func renderValue(ctx context.Context, env *tools.Env, v any, path string, prior, out map[string]renderRecord) (any, error) {
	switch x := v.(type) {
	case string:
		template := x
		if rec, ok := prior[path]; ok {
			if !rec.missed {
				out[path] = rec
				return x, nil
			}
			template = rec.template
		}
		if !envVarPattern.MatchString(template) {
			return x, nil
		}
		var missed []string
		var firstErr error
		text := envVarPattern.ReplaceAllStringFunc(template, func(raw string) string {
			body := raw[2 : len(raw)-1]
			if val, ok := contextVar(env, strings.TrimSpace(body)); ok {
				return val
			}
			val, found, err := getSecret(ctx, env, envRefName(body))
			if err != nil && firstErr == nil {
				firstErr = err
			}
			if !found || val == "" {
				missed = append(missed, raw)
				return raw
			}
			return val
		})
		if firstErr != nil {
			return nil, firstErr
		}
		out[path] = renderRecord{template: template, missed: len(missed) > 0, unsetRef: len(missed) == 1 && missed[0] == template}
		return text, nil
	case map[string]any:
		m := make(map[string]any, len(x))
		for k, val := range x {
			r, err := renderValue(ctx, env, val, renderPath(path, "k"+k), prior, out)
			if err != nil {
				return nil, err
			}
			m[k] = r
		}
		return m, nil
	case []any:
		l := make([]any, len(x))
		for i, val := range x {
			r, err := renderValue(ctx, env, val, renderPath(path, "i"+strconv.Itoa(i)), prior, out)
			if err != nil {
				return nil, err
			}
			l[i] = r
		}
		return l, nil
	}
	return v, nil
}

// Value with every reference and context variable rendered from the active profile's secrets
func interpolateEnvVars(ctx context.Context, env *tools.Env, v any) (any, error) {
	return renderValue(ctx, env, v, "", nil, map[string]renderRecord{})
}

// Renders one server entry and records how each templated string resolved
func renderServerConfig(ctx context.Context, env *tools.Env, cfg map[string]any) (*Rendered, error) {
	out := map[string]renderRecord{}
	v, err := renderValue(ctx, env, cfg, "", nil, out)
	if err != nil {
		return nil, err
	}
	return &Rendered{Config: v.(map[string]any), strs: out}, nil
}

// Renders the entry again under the current scope, resolved strings final and missed ones retried
func (r *Rendered) Rerender(ctx context.Context, env *tools.Env) (*Rendered, error) {
	out := map[string]renderRecord{}
	v, err := renderValue(ctx, env, r.Config, "", r.strs, out)
	if err != nil {
		return nil, err
	}
	return &Rendered{Config: v.(map[string]any), strs: out}, nil
}

// Reports whether an env entry was one whole reference that resolved to nothing
func (r *Rendered) EnvUnset(key string) bool {
	if r == nil {
		return false
	}
	return r.strs[renderPath("", "kenv", "k"+key)].unsetRef
}

// UnsetReferenceError is a remote entry whose url or headers still carry a literal reference
type UnsetReferenceError struct {
	Server string
	Refs   []string
}

func (e *UnsetReferenceError) Error() string {
	parts := make([]string, len(e.Refs))
	for i, ref := range e.Refs {
		parts[i] = "${" + ref + "}"
	}
	return fmt.Sprintf("MCP server '%s': %s in url/headers is not set in this profile's .env or secret source", e.Server, strings.Join(parts, ", "))
}

// Failure class the status surfaces report for a missing credential
func (e *UnsetReferenceError) FailureClass() string { return "missing_credentials" }

// Fails closed when a remote entry's url or headers still carry a literal reference after rendering
func requireRenderedRemote(serverName string, cfg map[string]any) error {
	if _, ok := cfg["url"]; !ok {
		return nil
	}
	url := cfg["url"]
	if !pyTruthy(url) {
		url = ""
	}
	values := []any{url}
	if headers, ok := cfg["headers"].(map[string]any); ok {
		for _, k := range orderedKeys(headers) {
			values = append(values, headers[k])
		}
	}
	seen := map[string]bool{}
	for _, v := range values {
		for _, m := range envVarPattern.FindAllStringSubmatch(pyStr(v), -1) {
			seen[m[1]] = true
		}
	}
	if len(seen) == 0 {
		return nil
	}
	refs := make([]string, 0, len(seen))
	for ref := range seen {
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	return &UnsetReferenceError{Server: serverName, Refs: refs}
}

var (
	whitespaceWarnedMu sync.Mutex
	whitespaceWarned   = map[[2]string]bool{}
)

// Flags string values with leading or trailing whitespace, warning once per server and key path without the value
func warnHiddenWhitespace(serverName string, cfg map[string]any) []string {
	var flagged []string
	var walk func(v any, path string)
	walk = func(v any, path string) {
		switch x := v.(type) {
		case string:
			if x != pyStrip(x) {
				flagged = append(flagged, path)
			}
		case map[string]any:
			for _, k := range orderedKeys(x) {
				p := k
				if path != "" {
					p = path + "." + k
				}
				walk(x[k], p)
			}
		case []any:
			for i, item := range x {
				walk(item, fmt.Sprintf("%s[%d]", path, i))
			}
		}
	}
	walk(cfg, "")
	whitespaceWarnedMu.Lock()
	defer whitespaceWarnedMu.Unlock()
	for _, p := range flagged {
		key := [2]string{serverName, p}
		if whitespaceWarned[key] {
			continue
		}
		whitespaceWarned[key] = true
		mcpLogger().Warn(fmt.Sprintf("MCP server '%s': config value '%s' has hidden leading or trailing whitespace \u2014 this often "+
			"causes authentication or connection failures. Check for stray spaces/newlines in config.yaml "+
			"(or the referenced env var).", serverName, p))
	}
	return flagged
}

// String with the whitespace str.strip removes trimmed from both ends
func pyStrip(s string) string {
	return strings.TrimFunc(s, func(r rune) bool {
		switch r {
		case ' ', '\t', '\n', '\v', '\f', '\r', 0x1c, 0x1d, 0x1e, 0x1f, 0x85, 0xa0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000:
			return true
		}
		return r >= 0x2000 && r <= 0x200a
	})
}

// Why an entry's cost table is unusable, empty when it parses
func malformedCostTable(name string, cfg any) string {
	m, _ := cfg.(map[string]any)
	if _, err := ConfigCostPolicy(name, m); err != nil {
		return err.Error()
	}
	return ""
}

// Entries with exfiltration shapes or malformed cost tables dropped before any stdio spawn
func filterSuspiciousServers(servers map[string]any) map[string]any {
	safe := map[string]any{}
	for _, name := range orderedKeys(servers) {
		cfg := servers[name]
		var issues []string
		if m, ok := cfg.(map[string]any); ok {
			issues = ValidateServerEntry(name, m)
		}
		if len(issues) > 0 {
			mcpLogger().Warn(fmt.Sprintf("Skipping suspicious MCP server '%s': %s", name, strings.Join(issues, "; ")))
		} else if bad := malformedCostTable(name, cfg); bad != "" {
			mcpLogger().Error(fmt.Sprintf("Skipping MCP server '%s': %s (fix tool_costs/spend_limit/cost_unit in config.yaml)", name, bad))
		} else {
			safe[name] = cfg
		}
	}
	return safe
}

// Merges plugin provided portable servers into safe, native entries winning and keeping the package's cost keys
func portableServers(ctx context.Context, safe map[string]*Rendered, rejected map[string]bool) {
	portable, err := pluginPortableServers(home.Dir(ctx))
	if err != nil {
		mcpLogger().Debug("Failed to load portable MCP servers", "error", err)
		return
	}
	mergePortableServers(portable, safe, rejected)
}

// Merges declared portable servers into safe, native entries winning and keeping the package's cost keys
func mergePortableServers(portable map[string]map[string]any, safe map[string]*Rendered, rejected map[string]bool) {
	asAny := make(map[string]any, len(portable))
	for k, v := range portable {
		asAny[k] = v
	}
	kept := filterSuspiciousServers(asAny)
	for _, name := range orderedKeys(kept) {
		cfg, _ := kept[name].(map[string]any)
		switch native, exists := safe[name]; {
		case rejected[name]:
			mcpLogger().Warn(fmt.Sprintf("Portable MCP server '%s' skipped: the native entry of that name was rejected", name))
		case exists:
			var carried []string
			merged := make(map[string]any, len(native.Config)+len(CostConfigKeys))
			for k, v := range native.Config {
				merged[k] = v
			}
			for _, k := range CostConfigKeys {
				if v, has := cfg[k]; has {
					if _, nativeHas := native.Config[k]; !nativeHas {
						merged[k] = v
						carried = append(carried, k)
					}
				}
			}
			if len(carried) > 0 {
				safe[name] = &Rendered{Config: merged, strs: native.strs}
				mcpLogger().Warn(fmt.Sprintf("Portable MCP server '%s' conflicts with native config; using the native entry with the package's declared %s",
					name, strings.Join(carried, ", ")))
			} else {
				mcpLogger().Warn(fmt.Sprintf("Portable MCP server '%s' conflicts with native config; skipping", name))
			}
		default:
			cp := make(map[string]any, len(cfg))
			for k, v := range cfg {
				cp[k] = v
			}
			safe[name] = &Rendered{Config: cp, strs: map[string]renderRecord{}}
		}
	}
}

// Truthy spellings env flags accept
var truthyStrings = map[string]bool{"1": true, "true": true, "yes": true, "on": true}

// Secret lookup over the session's secrets or the home's .env layered over the process environment
func secretsLookup(env *tools.Env, h string) config.Lookup {
	if env != nil && env.Secrets != nil {
		return env.Secrets.Lookup()
	}
	return config.NewSecrets(h).Lookup()
}

// Every mcp_servers entry of the active profile rendered, suspicious and malformed ones dropped, empty in safe mode
func LoadServerConfigs(ctx context.Context, env *tools.Env) map[string]*Rendered {
	if config.SafeWorkerEnabled() {
		return map[string]*Rendered{}
	}
	if truthyStrings[strings.ToLower(strings.TrimSpace(os.Getenv("ICEBATH_SAFE_MODE")))] {
		return map[string]*Rendered{}
	}
	h := home.Dir(ctx)
	tree, err := config.Load(h, secretsLookup(env, h))
	if err != nil {
		mcpLogger().Debug(fmt.Sprintf("Failed to load MCP config: %v", err))
		return map[string]*Rendered{}
	}
	servers, _ := tree["mcp_servers"].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
	}
	kept := filterSuspiciousServers(servers)
	safe := map[string]*Rendered{}
	for _, name := range orderedKeys(kept) {
		cfg, ok := kept[name].(map[string]any)
		if !ok {
			continue
		}
		r, err := renderServerConfig(ctx, env, cfg)
		if err != nil {
			mcpLogger().Debug(fmt.Sprintf("Failed to load MCP config: %v", err))
			return map[string]*Rendered{}
		}
		warnHiddenWhitespace(name, r.Config)
		safe[name] = r
	}
	rejected := map[string]bool{}
	for name, cfg := range servers {
		if _, ok := kept[name]; !ok && malformedCostTable(name, cfg) != "" {
			rejected[name] = true
		}
	}
	portableServers(ctx, safe, rejected)
	return safe
}

// Identity header name and value of an entry, warning and ignoring an invalid block
func resolveIdentityHeader(ctx context.Context, serverName string, cfg map[string]any) (string, string, bool) {
	raw, present := cfg["identity_header"]
	if !present || raw == nil {
		return "", "", false
	}
	ignore := func(detail string) (string, string, bool) {
		mcpLogger().Warn(fmt.Sprintf("MCP server '%s': identity_header %s \u2014 ignoring", serverName, detail))
		return "", "", false
	}
	block, ok := raw.(map[string]any)
	if !ok {
		return ignore(fmt.Sprintf("must be a mapping with 'name' and 'value'/'value_from' keys (got %s)", pyTypeName(raw)))
	}
	name, ok := block["name"].(string)
	if !ok || pyStrip(name) == "" {
		return ignore("requires a non-empty 'name'")
	}
	valueFrom := "static"
	if vf := block["value_from"]; pyTruthy(vf) {
		s, isStr := vf.(string)
		if !isStr {
			return ignore(fmt.Sprintf("value_from must be 'static' or 'profile' (got %s)", pyRepr(vf)))
		}
		valueFrom = strings.ToLower(pyStrip(s))
	}
	if valueFrom == "profile" {
		return pyStrip(name), activeProfileName(ctx), true
	}
	if valueFrom != "static" {
		return ignore(fmt.Sprintf("value_from must be 'static' or 'profile' (got %s)", pyReprStr(valueFrom)))
	}
	value, ok := block["value"].(string)
	if !ok || pyStrip(value) == "" {
		return ignore("with value_from: static requires a non-empty string 'value'")
	}
	return pyStrip(name), value, true
}

// Python type name of a YAML value
func pyTypeName(v any) string {
	switch v.(type) {
	case nil:
		return "NoneType"
	case bool:
		return "bool"
	case int, int64, uint64:
		return "int"
	case float64:
		return "float"
	case string:
		return "str"
	case []any, []string:
		return "list"
	case map[string]any:
		return "dict"
	}
	return fmt.Sprintf("%T", v)
}

// Headers with the identity header merged in unless an explicit header of that name exists in any casing
func applyIdentityHeader(ctx context.Context, serverName string, cfg map[string]any, headers map[string]string) map[string]string {
	name, value, ok := resolveIdentityHeader(ctx, serverName, cfg)
	if !ok {
		return headers
	}
	for k := range headers {
		if strings.EqualFold(k, name) {
			mcpLogger().Debug(fmt.Sprintf("MCP server '%s': identity_header '%s' already set via explicit headers config \u2014 keeping the explicit value", serverName, name))
			return headers
		}
	}
	headers[name] = value
	return headers
}

// Profile the active home belongs to, default for the platform home, its id under profiles, else custom
func activeProfileName(ctx context.Context) string {
	resolved, err := pyResolve(home.Dir(ctx))
	if err != nil {
		return "custom"
	}
	def, err := pyResolve(home.PlatformDefault())
	if err == nil && resolved == def {
		return "default"
	}
	root, err := pyResolve(filepath.Join(home.PlatformDefault(), home.ProfilesDir))
	if err != nil {
		return "custom"
	}
	if rel, err := filepath.Rel(root, resolved); err == nil && rel != "." && !strings.HasPrefix(rel, "..") &&
		!strings.ContainsRune(rel, filepath.Separator) && home.ProfileIDRe.MatchString(rel) {
		return rel
	}
	return "custom"
}

// Trust tiers a server entry may declare
const (
	TrustFull      = "full"
	TrustUntrusted = "untrusted"
)

// Trust tier of a config value, absent meaning full and anything unrecognized failing closed to untrusted
func normalizeServerTrust(value any) string {
	if value == nil {
		return TrustFull
	}
	text := strings.ToLower(pyStrip(pyStr(value)))
	if text == TrustFull || text == TrustUntrusted {
		return text
	}
	mcpLogger().Warn(fmt.Sprintf("MCP trust: unrecognized trust value %s \u2014 treating as 'untrusted' (valid values: full, untrusted)", pyRepr(value)))
	return TrustUntrusted
}

// ServerConfig is the typed view of one rendered mcp_servers entry
type ServerConfig struct {
	Name                      string
	Raw                       map[string]any
	Command                   string
	Args                      []string
	Env                       map[string]any
	Cwd                       string
	URL                       string
	Headers                   map[string]string
	Transport                 string
	Enabled                   bool
	ToolTimeout               float64
	ConnectTimeout            float64
	KeepaliveInterval         float64
	Lazy                      bool
	IdleTimeout               float64
	MaxLifetime               float64
	ToolsInclude              map[string]bool
	ToolsExclude              map[string]bool
	Resources                 bool
	Prompts                   bool
	Trust                     string
	Costs                     *CostPolicy
	Auth                      string
	OAuth                     map[string]any
	Sampling                  map[string]any
	Elicitation               map[string]any
	Events                    bool
	Protocol                  string
	SkipPreflight             bool
	SSLVerify                 any
	ClientCert                any
	ClientKey                 any
	StrictRedirectHeaders     bool
	IdentityHeader            any
	SupportsParallelToolCalls bool
}

// Reports whether the entry names a remote server reached over HTTP or SSE
func (c *ServerConfig) IsHTTP() bool {
	_, ok := c.Raw["url"]
	return ok
}

// Identity header the entry attaches to remote requests, resolved at connect time
func (c *ServerConfig) ResolveIdentityHeader(ctx context.Context) (string, string, bool) {
	return resolveIdentityHeader(ctx, c.Name, c.Raw)
}

// Typed view of one rendered entry, failing only on a malformed cost table
func ParseServerConfig(name string, cfg map[string]any, tree config.Tree) (*ServerConfig, error) {
	costs, err := ConfigCostPolicy(name, cfg)
	if err != nil {
		return nil, err
	}
	c := &ServerConfig{Name: name, Raw: cfg, Costs: costs}
	if v, ok := cfg["command"]; ok && v != nil {
		c.Command = pyStr(v)
	}
	if args, ok := cfg["args"].([]any); ok {
		for _, a := range args {
			c.Args = append(c.Args, pyStr(a))
		}
	}
	c.Env, _ = cfg["env"].(map[string]any)
	if v, ok := cfg["cwd"]; ok && pyTruthy(v) {
		c.Cwd = pyStr(v)
	}
	if v, ok := cfg["url"]; ok && pyTruthy(v) {
		c.URL = pyStr(v)
	}
	if headers, ok := cfg["headers"].(map[string]any); ok {
		c.Headers = make(map[string]string, len(headers))
		for k, v := range headers {
			c.Headers[k] = pyStr(v)
		}
	}
	if v, ok := cfg["transport"]; ok && v != nil {
		c.Transport = pyStr(v)
	}
	c.Enabled = ServerEnabled(cfg)
	c.ToolTimeout = resolveToolTimeout(cfg, tree)
	c.ConnectTimeout = defaultConnectTimeout
	if v, ok := cfg["connect_timeout"]; ok {
		if f, isNum := pyFloat(v); isNum {
			c.ConnectTimeout = f
		} else {
			mcpLogger().Warn(fmt.Sprintf("MCP server '%s': connect_timeout must be a number of seconds; ignoring %s", name, pyRepr(v)))
		}
	}
	if configured, ok := cfg["keepalive_interval"]; c.IsHTTP() || (ok && configured != nil) {
		interval := defaultKeepaliveInterval
		if ok && configured != nil {
			if f, isNum := pyFloat(configured); isNum {
				interval = f
			} else {
				mcpLogger().Warn(fmt.Sprintf("MCP server '%s': keepalive_interval must be a number of seconds; ignoring %s", name, pyRepr(configured)))
			}
		}
		c.KeepaliveInterval = math.Max(minKeepaliveInterval, interval)
	}
	lazy, ok := cfg["lazy"]
	if !ok {
		lazy = false
	}
	c.Lazy = parseBoolish(lazy, false)
	c.IdleTimeout, _ = lifecycleSeconds(cfg, "idle_timeout_seconds")
	c.MaxLifetime, _ = lifecycleSeconds(cfg, "max_lifetime_seconds")
	toolsFilter, _ := cfg["tools"].(map[string]any)
	c.ToolsInclude = normalizeNameFilter(toolsFilter["include"], "include")
	c.ToolsExclude = normalizeNameFilter(toolsFilter["exclude"], "exclude")
	c.Resources = parseBoolish(toolsFilter["resources"], true)
	c.Prompts = parseBoolish(toolsFilter["prompts"], true)
	c.Trust = normalizeServerTrust(cfg["trust"])
	if v := cfg["auth"]; pyTruthy(v) {
		c.Auth = strings.TrimSpace(strings.ToLower(pyStr(v)))
	}
	c.OAuth, _ = cfg["oauth"].(map[string]any)
	c.Sampling, _ = cfg["sampling"].(map[string]any)
	c.Elicitation, _ = cfg["elicitation"].(map[string]any)
	c.Events = parseBoolish(cfg["events"], false)
	c.Protocol = "auto"
	if v, ok := cfg["protocol"]; ok {
		c.Protocol = strings.TrimSpace(strings.ToLower(pyStr(v)))
	}
	c.SkipPreflight = pyTruthy(cfg["skip_preflight"])
	c.SSLVerify = true
	if v, ok := cfg["ssl_verify"]; ok {
		c.SSLVerify = v
	}
	c.ClientCert, c.ClientKey = cfg["client_cert"], cfg["client_key"]
	c.StrictRedirectHeaders = pyTruthy(cfg["strict_redirect_headers"])
	c.IdentityHeader = cfg["identity_header"]
	parallel, ok := cfg["supports_parallel_tool_calls"]
	if !ok {
		parallel = false
	}
	c.SupportsParallelToolCalls = parseBoolish(parallel, false)
	return c, nil
}

// Credential by env var name from the bound owner scope, else the env's profile secrets
func getSecret(ctx context.Context, env *tools.Env, name string) (string, bool, error) {
	if value, found, bound, err := boundScopeSecret(ctx, name); bound {
		return value, found, err
	}
	var fallback *string
	if v, ok := os.LookupEnv(name); ok {
		fallback = &v
	}
	p, err := tools.ResolvePassthroughValue(env, name, fallback)
	if err != nil {
		return "", false, err
	}
	if p == nil {
		return "", false, nil
	}
	return *p, true, nil
}

// Source applied env var names the child env forwards, swapped by tests that register sources
var secretSourceNamesFn = secretSourceNames
