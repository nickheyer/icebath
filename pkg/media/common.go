// Package media provides image and video generation, text to speech, transcription, voice mode, and wake word tools
package media

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Provider value every managed Nous Subscription picker row writes
const NousManagedProvider = "nous"

// Age in hours after which transient media caches are swept
const MediaCacheMaxAgeHours = 24

// Icebath user agent sent to every vendor HTTP endpoint
const UserAgent = "Icebath-Agent/1.0"

var truthyStrings = map[string]bool{"1": true, "true": true, "yes": true, "on": true}
var falsyStrings = map[string]bool{"0": true, "false": true, "no": true, "off": true, "disabled": true}

// Coerces bool like values using the shared truthy string set, nil yields the default
func truthy(v any, def bool) bool {
	if v == nil {
		return def
	}
	if s, ok := v.(string); ok {
		return truthyStrings[strings.ToLower(strings.TrimSpace(s))]
	}
	if b, ok := config.AsBool(v); ok {
		return b
	}
	return config.Truthy(v)
}

// Coerces common YAML and env bool spellings without treating random strings as true
func configBool(v any, def bool) bool {
	switch t := v.(type) {
	case nil:
		return def
	case bool:
		return t
	case int, int64, float64:
		f, _ := config.AsFloat(t)
		return f != 0
	case string:
		n := strings.ToLower(strings.TrimSpace(t))
		if truthyStrings[n] || n == "enabled" {
			return true
		}
		if falsyStrings[n] {
			return false
		}
	}
	return def
}

// Section of the merged config as a tree, missing or malformed sections read as empty
func section(cfg config.Tree, key string) config.Tree {
	if cfg == nil {
		return config.Tree{}
	}
	sub := cfg.Sub(key)
	if sub == nil {
		return config.Tree{}
	}
	return sub
}

// Stripped string value of a config key or empty
func cfgString(t config.Tree, key string) string {
	if t == nil {
		return ""
	}
	v, ok := t[key]
	if !ok || v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}

// Float value of a config key with a fallback on bad values
func cfgFloat(t config.Tree, key string, def float64) float64 {
	if t == nil {
		return def
	}
	v, ok := t[key]
	if !ok || v == nil {
		return def
	}
	if _, isBool := v.(bool); isBool {
		return def
	}
	if s, ok := v.(string); ok {
		f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
		if err != nil {
			return def
		}
		return f
	}
	f, ok := config.AsFloat(v)
	if !ok {
		return def
	}
	return f
}

// Int value of a config key with a fallback on bad values
func cfgInt(t config.Tree, key string, def int) int {
	if t == nil {
		return def
	}
	v, ok := t[key]
	if !ok || v == nil {
		return def
	}
	if _, isBool := v.(bool); isBool {
		return def
	}
	if s, ok := v.(string); ok {
		f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
		if err != nil {
			return def
		}
		return int(f)
	}
	i, ok := config.AsInt(v)
	if !ok {
		return def
	}
	return i
}

// Positive int value of a config key or zero
func positiveInt(v any) int {
	if v == nil {
		return 0
	}
	if _, isBool := v.(bool); isBool {
		return 0
	}
	if _, isStr := v.(string); isStr {
		return 0
	}
	i, ok := config.AsInt(v)
	if !ok || i <= 0 {
		return 0
	}
	return i
}

// Logger of the environment or a discarding one
func logOf(env *tools.Env) *slog.Logger {
	if env != nil && env.Log != nil {
		return env.Log
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// Config tree of the environment or an empty tree
func cfgOf(env *tools.Env) config.Tree {
	if env == nil || env.Config == nil {
		return config.Tree{}
	}
	return env.Config
}

// Home directory of the environment or the process default
func homeOf(env *tools.Env) string {
	if env != nil && env.Home != "" {
		return env.Home
	}
	return home.Process()
}

// Secret from the home .env file then the process environment
func secret(env *tools.Env, name string) string {
	if env != nil && env.Secrets != nil {
		return strings.TrimSpace(env.Secrets.String(name))
	}
	return strings.TrimSpace(os.Getenv(name))
}

// First non empty secret among several names
func firstSecret(env *tools.Env, names ...string) string {
	for _, n := range names {
		if v := secret(env, n); v != "" {
			return v
		}
	}
	return ""
}

// Random lowercase hex of n bytes
func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(b)
}

// RFC 4122 version 4 identifier
func uuid4() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return randomHex(16)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// Result of one JSON POST attempt that failed
type httpFailure struct {
	Kind      string
	Error     string
	ErrorType string
	Status    int
	Message   string
	Body      []byte
	Header    http.Header
}

// Error message extractor for a failed JSON body
type errorMessageFunc func(status int, body []byte) string

// Value of a key in a decoded JSON object and whether the node is an object at all
func jsonObjectGet(node any, key string) (any, bool, bool) {
	switch o := node.(type) {
	case map[string]any:
		v, has := o[key]
		return v, has, true
	case *pyjson.Object:
		v, has := o.Get(key)
		return v, has, true
	}
	return nil, false, false
}

// Message of the error object in a JSON body, else the first 300 characters of the body
func defaultErrorMessage(_ int, body []byte) string {
	text := clipRunes(pystr.DecodeUTF8Replace(body), 300)
	parsed, err := pyjson.LoadsBytes(body)
	if err != nil {
		return text
	}
	errNode, has, isObj := jsonObjectGet(parsed, "error")
	if !isObj || !has {
		return text
	}
	message, has, isObj := jsonObjectGet(errNode, "message")
	if !isObj || !has {
		return text
	}
	return pyrepr.Str(message)
}

// First n bytes of a string
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// Shared HTTP client with no global timeout so each call sets its own deadline
var httpClient = &http.Client{}

// Connect and read timeouts of a requests call, read bounding each wait on the server
type requestsTimeout struct{ Connect, Read time.Duration }

// Body bytes with every read bounded by the idle timeout, the request cancelled when one stalls
func readWithIdleTimeout(r io.Reader, idle time.Duration, cancel context.CancelFunc) ([]byte, error) {
	if idle <= 0 {
		return io.ReadAll(r)
	}
	timer := time.AfterFunc(idle, cancel)
	defer timer.Stop()
	var buf bytes.Buffer
	chunk := make([]byte, 32<<10)
	for {
		n, err := r.Read(chunk)
		buf.Write(chunk[:n])
		timer.Reset(idle)
		if err == io.EOF {
			return buf.Bytes(), nil
		}
		if err != nil {
			return nil, err
		}
	}
}

// Posts JSON and returns the decoded body or a classified failure, request errors raised unless caught
func postJSON(ctx context.Context, rawURL string, headers map[string]string, payload any, timeout requestsTimeout, label string, msg errorMessageFunc, catchRequestException bool) (any, *httpFailure, error) {
	requestFailure := func(err error) (any, *httpFailure, error) {
		if !catchRequestException {
			return nil, nil, err
		}
		return nil, &httpFailure{Kind: "request", Error: fmt.Sprintf("%s request failed: %v", label, err), ErrorType: "api_error"}, nil
	}
	body, err := pyjson.Dumps(payload, pyjson.AllowNaN(false))
	if err != nil {
		return nil, nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, strings.NewReader(body))
	if err != nil {
		return requestFailure(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", UserAgent)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	transport := &http.Transport{Proxy: http.ProxyFromEnvironment, DialContext: (&net.Dialer{Timeout: timeout.Connect}).DialContext,
		TLSHandshakeTimeout: timeout.Read, ResponseHeaderTimeout: timeout.Read, ForceAttemptHTTP2: true}
	defer transport.CloseIdleConnections()
	resp, err := (&http.Client{Transport: transport}).Do(req)
	if err != nil {
		var urlErr *url.Error
		if errors.As(err, &urlErr) && (strings.Contains(urlErr.Err.Error(), "unsupported protocol scheme") || strings.Contains(urlErr.Err.Error(), "no Host in request URL")) {
			return requestFailure(err)
		}
		return nil, classifyTransportFailure(err, label, timeout.Read), nil
	}
	defer resp.Body.Close()
	raw, err := readWithIdleTimeout(resp.Body, timeout.Read, cancel)
	if err != nil {
		return nil, &httpFailure{Kind: "connection", Error: fmt.Sprintf("%s connection error: %v", label, err), ErrorType: "connection_error"}, nil
	}
	if resp.StatusCode >= 400 && resp.StatusCode < 600 {
		if msg == nil {
			msg = defaultErrorMessage
		}
		message := msg(resp.StatusCode, raw)
		return nil, &httpFailure{Kind: "http", Error: fmt.Sprintf("%s image generation failed (%d): %s", label, resp.StatusCode, message),
			ErrorType: "api_error", Status: resp.StatusCode, Message: message, Body: raw, Header: resp.Header}, nil
	}
	parsed, err := pyjson.LoadsBytes(raw)
	if err != nil {
		return nil, &httpFailure{Kind: "invalid_json", Error: fmt.Sprintf("%s returned invalid JSON: %v", label, err), ErrorType: "invalid_response", Message: err.Error()}, nil
	}
	return parsed, nil, nil
}

// Maps a transport error to timeout, unreachable, or connection failure kinds
func classifyTransportFailure(err error, label string, timeout time.Duration) *httpFailure {
	var urlErr *url.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &urlErr) && urlErr.Timeout()) {
		return &httpFailure{Kind: "timeout", Error: fmt.Sprintf("%s image generation timed out (%ds)", label, int(timeout.Seconds())), ErrorType: "timeout"}
	}
	var opErr *net.OpError
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) || (errors.As(err, &opErr) && opErr.Op == "dial") {
		return &httpFailure{Kind: "unreachable", Error: fmt.Sprintf("%s connection error: %v", label, err), ErrorType: "connection_error"}
	}
	return &httpFailure{Kind: "connection", Error: fmt.Sprintf("%s connection error: %v", label, err), ErrorType: "connection_error"}
}

// GET with bearer auth returning the decoded JSON body
func getJSON(ctx context.Context, rawURL string, headers map[string]string, timeout time.Duration) (map[string]any, int, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("User-Agent", UserAgent)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if resp.StatusCode >= 400 {
		return nil, resp.StatusCode, fmt.Errorf("HTTP %d: %s", resp.StatusCode, clip(string(raw), 300))
	}
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, resp.StatusCode, err
	}
	return parsed, resp.StatusCode, nil
}

// Map value at key when it is a map, else empty
func dictAt(node any, key string) map[string]any {
	m, ok := node.(map[string]any)
	if !ok {
		return map[string]any{}
	}
	v, ok := m[key].(map[string]any)
	if !ok {
		return map[string]any{}
	}
	return v
}

// List value at key when it is a list, else empty
func listAt(node any, key string) []any {
	m, ok := node.(map[string]any)
	if !ok {
		return nil
	}
	v, ok := m[key].([]any)
	if !ok {
		return nil
	}
	return v
}

// String value at key or empty
func strAt(node any, key string) string {
	m, ok := node.(map[string]any)
	if !ok {
		return ""
	}
	switch v := m[key].(type) {
	case string:
		return v
	case nil:
		return ""
	default:
		return ""
	}
}

// Stripped string value at key or empty
func stripAt(node any, key string) string { return strings.TrimSpace(strAt(node, key)) }

// Numeric value at key as float with presence flag
func numAt(node any, key string) (float64, bool) {
	m, ok := node.(map[string]any)
	if !ok {
		return 0, false
	}
	v, ok := m[key]
	if !ok || v == nil {
		return 0, false
	}
	if _, isBool := v.(bool); isBool {
		return 0, false
	}
	return config.AsFloat(v)
}

// String list from a list of any
func stringList(v any) []string {
	var out []string
	switch t := v.(type) {
	case []any:
		for _, item := range t {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
	case []string:
		out = append(out, t...)
	}
	return out
}

// Path of a binary on PATH or in the common Homebrew and local prefixes
func findBinary(name string) string {
	for _, dir := range []string{"/opt/homebrew/bin", "/usr/local/bin"} {
		candidate := filepath.Join(dir, name)
		if st, err := os.Stat(candidate); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			return candidate
		}
	}
	p, err := exec.LookPath(name)
	if err != nil {
		return ""
	}
	return p
}

// Removes a file ignoring missing or locked entries
func removeQuietly(path string) {
	if path != "" {
		os.Remove(path)
	}
}

// Deletes files in a directory matching a glob older than the age, returning the count
func unlinkFilesOlderThan(dir, pattern string, maxAge time.Duration) int {
	matches, err := filepath.Glob(filepath.Join(dir, pattern))
	if err != nil {
		return 0
	}
	cutoff := time.Now().Add(-maxAge)
	removed := 0
	for _, p := range matches {
		st, err := os.Stat(p)
		if err != nil || !st.Mode().IsRegular() || !st.ModTime().Before(cutoff) {
			continue
		}
		if os.Remove(p) == nil {
			removed++
		}
	}
	return removed
}

// Reports whether this process runs inside WSL
func isWSL() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	raw, err := os.ReadFile("/proc/version")
	if err != nil {
		return false
	}
	return strings.Contains(strings.ToLower(string(raw)), "microsoft")
}

// Reports whether this process runs inside Termux on Android
func isTermux() bool {
	if os.Getenv("TERMUX_VERSION") != "" {
		return true
	}
	prefix := os.Getenv("PREFIX")
	return strings.Contains(prefix, "com.termux")
}

// Reports whether this process runs inside a Docker, Podman, or LXC container
func isContainer() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	for _, marker := range []string{"/.dockerenv", "/run/.containerenv"} {
		if _, err := os.Stat(marker); err == nil {
			return true
		}
	}
	raw, err := os.ReadFile("/proc/1/cgroup")
	if err == nil {
		text := strings.ToLower(string(raw))
		for _, needle := range []string{"docker", "lxc", "containerd", "podman", "kubepods"} {
			if strings.Contains(text, needle) {
				return true
			}
		}
	}
	return os.Getenv("container") != ""
}

// Clamps a float into a closed range
func clampFloat(v, lo, hi float64) float64 {
	if math.IsNaN(v) {
		return lo
	}
	return math.Max(lo, math.Min(hi, v))
}

// Clamps an int into a closed range
func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// Working directory for scratch files under the system temp dir
func tempDir(sub string) string {
	dir := filepath.Join(os.TempDir(), sub)
	os.MkdirAll(dir, 0o755)
	return dir
}

// Creates a private scratch directory with a prefix
func makeTempDir(prefix string) (string, error) {
	return os.MkdirTemp("", prefix)
}
