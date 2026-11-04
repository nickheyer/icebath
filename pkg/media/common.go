// Package media provides image and video generation, text to speech, transcription, voice mode, and wake word tools
package media

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
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

// Voice provider API key, explicit config value, then the .env and process environment
func resolveProviderSecret(env *tools.Env, envVar, configValue string) string {
	if v := strings.TrimSpace(configValue); v != "" {
		return v
	}
	return secret(env, envVar)
}

// OpenAI audio key, the dedicated voice tools override first then the general key
func resolveOpenAIAudioKey(env *tools.Env) string {
	return firstSecret(env, "VOICE_TOOLS_OPENAI_KEY", "OPENAI_API_KEY")
}

var selectionNameKeys = map[string][]string{
	"browser":   {"cloud_provider"},
	"web":       {"backend"},
	"image_gen": {"provider", "backend", "cloud_provider"},
	"video_gen": {"provider", "backend", "cloud_provider"},
	"tts":       {"provider", "backend", "cloud_provider"},
	"stt":       {"provider", "backend", "cloud_provider"},
}

// Persisted picker selection, nous for the managed row, a vendor name, or empty when never configured
func readSelection(cfg config.Tree, sec string) string {
	raw := section(cfg, sec)
	if len(raw) == 0 {
		return ""
	}
	if truthy(raw["use_gateway"], false) {
		return NousManagedProvider
	}
	keys, ok := selectionNameKeys[sec]
	if !ok {
		keys = []string{"provider", "backend", "cloud_provider"}
	}
	for _, k := range keys {
		if v, ok := raw[k]; ok && v != nil {
			if text := strings.ToLower(strings.TrimSpace(fmt.Sprint(v))); text != "" {
				return text
			}
		}
	}
	return ""
}

// Uniform error text for a selected but broken provider
func selectionError(sec, selection, failure string) string {
	return fmt.Sprintf("%s is configured to use %s (set via icebath tools), but %s. Run 'icebath tools' to change it.", sec, selection, failure)
}

// Reports whether FAL_KEY carries a non blank value
func falKeyConfigured(env *tools.Env) bool {
	return secret(env, "FAL_KEY") != ""
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

// Cache directory under the home for one media kind, created on demand
func cacheDir(env *tools.Env, kind string) (string, error) {
	dir := filepath.Join(home.CacheDir(homeOf(env)), filepath.FromSlash(kind))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

// Timestamped unique cache file path for one media kind
func cachePath(env *tools.Env, kind, prefix, ext string) (string, error) {
	dir, err := cacheDir(env, kind)
	if err != nil {
		return "", err
	}
	name := fmt.Sprintf("%s_%s_%s.%s", prefix, time.Now().Format("20060102_150405"), randomHex(4), ext)
	return filepath.Join(dir, name), nil
}

// Writes raw bytes into the cache and returns the absolute path
func saveBytes(env *tools.Env, kind string, raw []byte, prefix, ext string) (string, error) {
	path, err := cachePath(env, kind, prefix, ext)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// Decodes base64 into the cache and returns the absolute path
func saveB64(env *tools.Env, kind, b64, prefix, ext string) (string, error) {
	raw, err := decodeB64(b64)
	if err != nil {
		return "", err
	}
	return saveBytes(env, kind, raw, prefix, ext)
}

// Base64 decode tolerant of missing padding and url safe alphabets
func decodeB64(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if raw, err := base64.StdEncoding.DecodeString(s); err == nil {
		return raw, nil
	}
	if raw, err := base64.RawStdEncoding.DecodeString(strings.TrimRight(s, "=")); err == nil {
		return raw, nil
	}
	if raw, err := base64.URLEncoding.DecodeString(s); err == nil {
		return raw, nil
	}
	return base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
}

// Download options for cached media URLs
type saveURLOptions struct {
	Prefix       string
	Timeout      time.Duration
	MaxBytes     int64
	ContentTypes map[string]string
	URLExts      []string
	DefaultExt   string
	Label        string
	EmptyError   string
	Headers      map[string]string
	RequireKnown bool
	Trusted      bool
}

var redirectStatuses = map[int]bool{301: true, 302: true, 303: true, 307: true, 308: true}

const maxSaveURLRedirects = 5

// Streams a provider supplied URL into the cache with SSRF checks on every hop and a size cap
func saveURL(ctx context.Context, env *tools.Env, kind, rawURL string, o saveURLOptions) (string, error) {
	current, hopHeaders, trusted := rawURL, o.Headers, o.Trusted
	client := &http.Client{Timeout: o.Timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for hop := 0; hop <= maxSaveURLRedirects; hop++ {
		if trusted {
			if isAlwaysBlockedURL(current) {
				return "", fmt.Errorf("%s URL targets an always-blocked address: %s", o.Label, current)
			}
		} else if !isSafeURL(env, current) {
			return "", fmt.Errorf("%s URL failed the SSRF safety check: %s", o.Label, current)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, current, nil)
		if err != nil {
			return "", err
		}
		for k, v := range hopHeaders {
			req.Header.Set(k, v)
		}
		resp, err := client.Do(req)
		if err != nil {
			return "", err
		}
		if redirectStatuses[resp.StatusCode] {
			loc := resp.Header.Get("Location")
			resp.Body.Close()
			if loc == "" {
				return "", fmt.Errorf("%s download redirected without a Location: %s", o.Label, current)
			}
			base, err := url.Parse(current)
			if err != nil {
				return "", err
			}
			next, err := base.Parse(loc)
			if err != nil {
				return "", err
			}
			current, hopHeaders, trusted = next.String(), nil, false
			continue
		}
		path, err := writeCachedResponse(env, kind, resp, current, rawURL, o)
		resp.Body.Close()
		return path, err
	}
	return "", fmt.Errorf("%s download exceeded %d redirects: %s", o.Label, maxSaveURLRedirects, rawURL)
}

// Writes one HTTP response body to the cache, choosing the extension from content type then URL suffix
func writeCachedResponse(env *tools.Env, kind string, resp *http.Response, current, original string, o saveURLOptions) (string, error) {
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return "", fmt.Errorf("%s download failed (HTTP %d): %s", o.Label, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	ctype := strings.ToLower(strings.TrimSpace(strings.SplitN(resp.Header.Get("Content-Type"), ";", 2)[0]))
	ext, known := o.ContentTypes[ctype]
	if o.RequireKnown && !known {
		if ctype == "" {
			ctype = "(missing)"
		}
		return "", fmt.Errorf("%s download returned unexpected Content-Type %s", o.Label, ctype)
	}
	if !known {
		urlPath := strings.ToLower(strings.SplitN(current, "?", 2)[0])
		ext = o.DefaultExt
		for _, e := range o.URLExts {
			if strings.HasSuffix(urlPath, "."+e) {
				ext = e
				if e == "jpeg" {
					ext = "jpg"
				}
				break
			}
		}
	}
	path, err := cachePath(env, kind, o.Prefix, ext)
	if err != nil {
		return "", err
	}
	fh, err := os.Create(path)
	if err != nil {
		return "", err
	}
	written, copyErr := io.Copy(fh, io.LimitReader(resp.Body, o.MaxBytes+1))
	fh.Close()
	if copyErr != nil {
		os.Remove(path)
		return "", copyErr
	}
	if written > o.MaxBytes {
		os.Remove(path)
		return "", fmt.Errorf("%s at %s exceeds %dMB cap; refusing to cache.", o.Label, original, o.MaxBytes/(1024*1024))
	}
	if written == 0 {
		os.Remove(path)
		return "", errors.New(strings.ReplaceAll(o.EmptyError, "{url}", original))
	}
	return path, nil
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

// Default error text, the error.message of a JSON body else its first 300 chars
func defaultErrorMessage(_ int, body []byte) string {
	var parsed map[string]any
	if json.Unmarshal(body, &parsed) == nil {
		if e, ok := parsed["error"].(map[string]any); ok {
			if m, ok := e["message"].(string); ok && m != "" {
				return m
			}
		}
	}
	return clip(string(body), 300)
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

// Posts JSON with a deadline and returns the decoded body or a classified failure
func postJSON(ctx context.Context, rawURL string, headers map[string]string, payload any, timeout time.Duration, label string, msg errorMessageFunc) (map[string]any, *httpFailure) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, &httpFailure{Kind: "request", Error: fmt.Sprintf("%s request failed: %v", label, err), ErrorType: "api_error"}
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bytes.NewReader(body))
	if err != nil {
		return nil, &httpFailure{Kind: "request", Error: fmt.Sprintf("%s request failed: %v", label, err), ErrorType: "api_error"}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", UserAgent)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, classifyTransportFailure(err, label, timeout)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if resp.StatusCode >= 400 {
		if msg == nil {
			msg = defaultErrorMessage
		}
		message := msg(resp.StatusCode, raw)
		return nil, &httpFailure{Kind: "http", Error: fmt.Sprintf("%s image generation failed (%d): %s", label, resp.StatusCode, message),
			ErrorType: "api_error", Status: resp.StatusCode, Message: message, Body: raw, Header: resp.Header}
	}
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, &httpFailure{Kind: "invalid_json", Error: fmt.Sprintf("%s returned invalid JSON: %v", label, err), ErrorType: "invalid_response", Message: err.Error()}
	}
	return parsed, nil
}

// Maps a transport error to timeout, unreachable, or connection failure kinds
func classifyTransportFailure(err error, label string, timeout time.Duration) *httpFailure {
	var urlErr *url.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &urlErr) && urlErr.Timeout()) {
		return &httpFailure{Kind: "timeout", Error: fmt.Sprintf("%s image generation timed out (%ds)", label, int(timeout.Seconds())), ErrorType: "timeout"}
	}
	text := err.Error()
	if strings.Contains(text, "no such host") || strings.Contains(text, "connection refused") || strings.Contains(text, "lookup ") {
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

// Trims and lowercases a candidate name
func lowerKey(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// Credential store file names under the home that must never be read as media sources
var credentialFileNames = []string{"auth.json", "auth.lock", ".anthropic_oauth.json", ".env", "webhook_subscriptions.json", filepath.Join("auth", "google_oauth.json")}

var readDeniedDirs = []struct{ sub, dirMsg, fileMsg string }{
	{"mcp-tokens", "is the icebath MCP token directory and cannot be read directly.", "is an icebath MCP token file and cannot be read directly."},
	{"browser-profile", "is the icebath real-profile browser snapshot directory (copied cookies/logins) and cannot be read directly.", "is inside the icebath real-profile browser snapshot (copied cookies/logins) and cannot be read directly."},
	{"vault", "is the icebath credential vault directory and cannot be read directly (secrets are filled server-side by browser_vault).", "is inside the icebath credential vault (encrypted secrets + local key) and cannot be read directly (browser_vault resolves them server-side)."},
	{"webapp", "is the icebath Webapp sign-in directory and cannot be read directly.", "is an icebath Webapp sign-in file (the browser credential or its sign-in records) and cannot be read directly."},
}

var blockedProjectEnvBasenames = map[string]bool{".env": true, ".env.local": true, ".env.production": true, ".env.development": true, ".env.staging": true, ".env.test": true}

const didSuffix = " (Defense-in-depth, not a security boundary; the terminal tool can still bypass.)"

// Resolves symlinks and relative segments, falling back to the cleaned absolute path
func resolvePath(p string) string {
	expanded := expandUser(p)
	abs, err := filepath.Abs(expanded)
	if err != nil {
		abs = expanded
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real
	}
	return filepath.Clean(abs)
}

// Expands a leading tilde to the user home directory
func expandUser(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, "~\\") {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, p[1:])
		}
	}
	return p
}

// Reports whether child is base or lies under it
func isUnder(child, base string) bool {
	rel, err := filepath.Rel(base, child)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// Error text when a read targets a denied credential store, cache, or project env file
func readBlockError(env *tools.Env, path string) string {
	resolved := resolvePath(path)
	homes := []string{resolvePath(homeOf(env))}
	if root := home.RootOf(homeOf(env)); root != "" && resolvePath(root) != homes[0] {
		homes = append(homes, resolvePath(root))
	}
	reason := ""
	for _, h := range homes {
		if isUnder(resolved, filepath.Join(h, "skills", ".hub")) {
			reason = "is an internal icebath cache file and cannot be read directly to prevent prompt injection. Use the skills_list or skill_view tools instead."
		}
	}
	if reason == "" {
		for _, h := range homes {
			for _, name := range credentialFileNames {
				if resolved == resolvePath(filepath.Join(h, name)) {
					reason = "is an icebath credential store and cannot be read directly. Provider tools consume these credentials through internal channels." + didSuffix
				}
			}
		}
	}
	if reason == "" {
	outer:
		for _, d := range readDeniedDirs {
			for _, h := range homes {
				blocked := resolvePath(filepath.Join(h, d.sub))
				if isUnder(resolved, blocked) {
					if resolved == blocked {
						reason = d.dirMsg + didSuffix
					} else {
						reason = d.fileMsg + didSuffix
					}
					break outer
				}
			}
		}
	}
	if reason == "" && blockedProjectEnvBasenames[strings.ToLower(filepath.Base(resolved))] {
		reason = "is a secret-bearing environment file and cannot be read to prevent credential leakage. If you need to check the file structure, read .env.example instead." + didSuffix
	}
	if reason == "" {
		return ""
	}
	return fmt.Sprintf("Access denied: %s %s", path, reason)
}

// Image magic byte table in detection order
var imageMagic = []struct {
	checks []struct {
		off int
		sig []byte
	}
	mime string
}{
	{[]struct {
		off int
		sig []byte
	}{{0, []byte("\x89PNG\r\n\x1a\n")}}, "image/png"},
	{[]struct {
		off int
		sig []byte
	}{{0, []byte{0xff, 0xd8, 0xff}}}, "image/jpeg"},
	{[]struct {
		off int
		sig []byte
	}{{0, []byte("GIF87a")}}, "image/gif"},
	{[]struct {
		off int
		sig []byte
	}{{0, []byte("GIF89a")}}, "image/gif"},
	{[]struct {
		off int
		sig []byte
	}{{0, []byte("RIFF")}, {8, []byte("WEBP")}}, "image/webp"},
	{[]struct {
		off int
		sig []byte
	}{{0, []byte("BM")}}, "image/bmp"},
	{[]struct {
		off int
		sig []byte
	}{{4, []byte("ftyp")}, {8, []byte("avif")}}, "image/avif"},
	{[]struct {
		off int
		sig []byte
	}{{4, []byte("ftyp")}, {8, []byte("avis")}}, "image/avif"},
	{[]struct {
		off int
		sig []byte
	}{{0, []byte("II*\x00")}}, "image/tiff"},
	{[]struct {
		off int
		sig []byte
	}{{0, []byte("MM\x00*")}}, "image/tiff"},
	{[]struct {
		off int
		sig []byte
	}{{0, []byte{0, 0, 1, 0}}}, "image/x-icon"},
}

var heicBrands = []string{"heic", "heix", "hevc", "hevx", "mif1", "msf1", "heim", "heis"}

// Image MIME type from magic bytes or empty when unrecognised
func sniffImageMime(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	for _, m := range imageMagic {
		match := true
		for _, c := range m.checks {
			if len(raw) < c.off+len(c.sig) || !bytes.Equal(raw[c.off:c.off+len(c.sig)], c.sig) {
				match = false
				break
			}
		}
		if match {
			return m.mime
		}
	}
	if len(raw) >= 12 && bytes.Equal(raw[4:8], []byte("ftyp")) {
		brand := strings.ToLower(string(raw[8:12]))
		for _, b := range heicBrands {
			if brand == b {
				return "image/heic"
			}
		}
	}
	head := bytes.ToLower(bytes.TrimLeft(raw[:min(len(raw), 512)], " \t\r\n\xef\xbb\xbf"))
	if (bytes.HasPrefix(head, []byte("<?xml")) || bytes.HasPrefix(head, []byte("<svg"))) && bytes.Contains(head, []byte("<svg")) {
		return "image/svg+xml"
	}
	return ""
}

// MIME type by media file extension for the local source inliner
func mimeByExtension(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".bmp":
		return "image/bmp"
	case ".svg":
		return "image/svg+xml"
	case ".tif", ".tiff":
		return "image/tiff"
	case ".heic", ".heif":
		return "image/heic"
	case ".avif":
		return "image/avif"
	case ".mp4", ".m4v":
		return "video/mp4"
	case ".mov":
		return "video/quicktime"
	case ".webm":
		return "video/webm"
	case ".mkv":
		return "video/x-matroska"
	case ".wav":
		return "audio/wav"
	case ".mp3":
		return "audio/mpeg"
	case ".ogg", ".oga":
		return "audio/ogg"
	case ".flac":
		return "audio/flac"
	case ".m4a":
		return "audio/mp4"
	}
	return ""
}

// Reports whether a source string is an http, https, or data reference
func isRemoteRef(s string) bool {
	l := strings.ToLower(strings.TrimSpace(s))
	return strings.HasPrefix(l, "http://") || strings.HasPrefix(l, "https://") || strings.HasPrefix(l, "data:")
}

// Reports whether a source string is an http or https URL
func isHTTPRef(s string) bool {
	l := strings.ToLower(strings.TrimSpace(s))
	return strings.HasPrefix(l, "http://") || strings.HasPrefix(l, "https://")
}

// Base64 data URL for raw bytes with the given MIME type
func dataURL(mime string, raw []byte) string {
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(raw)
}

// Splits a data URL into its declared MIME type and decoded bytes
func parseDataURL(s string) (string, []byte, error) {
	header, payload, ok := strings.Cut(s, ",")
	if !ok {
		return "", nil, errors.New("data: URL is missing a comma separator")
	}
	if !strings.Contains(header, ";base64") {
		return "", nil, errors.New("data: URL must be base64-encoded")
	}
	declared := strings.TrimSpace(strings.SplitN(strings.TrimPrefix(header, "data:"), ";", 2)[0])
	if declared == "" {
		declared = "application/octet-stream"
	}
	raw, err := decodeB64(payload)
	if err != nil {
		return "", nil, fmt.Errorf("invalid base64 in data: URL: %v", err)
	}
	return declared, raw, nil
}

// Maximum raw bytes ingested from one media source
const maxIngestBytes = 50 * 1024 * 1024

// Resolves a local path, data URL, or http URL to bytes and a verified image MIME type
func resolveImageSource(ctx context.Context, env *tools.Env, src string, permitVideo bool) ([]byte, string, error) {
	s := strings.TrimSpace(src)
	if s == "" {
		return nil, "", errors.New("image_url is required")
	}
	var data []byte
	var declared string
	switch {
	case strings.HasPrefix(s, "data:"):
		var err error
		declared, data, err = parseDataURL(s)
		if err != nil {
			return nil, "", err
		}
	case isHTTPRef(s):
		if !isSafeURL(env, s) {
			return nil, "", errors.New("blocked: unsafe or private URL")
		}
		raw, err := downloadBytes(ctx, s, maxIngestBytes)
		if err != nil {
			return nil, "", err
		}
		data = raw
	default:
		candidate := s
		if strings.HasPrefix(strings.ToLower(s), "file://") {
			candidate = s[len("file://"):]
		}
		if blocked := readBlockError(env, candidate); blocked != "" {
			return nil, "", errors.New(blocked)
		}
		path := expandUser(candidate)
		st, err := os.Stat(path)
		if err != nil || !st.Mode().IsRegular() {
			return nil, "", fmt.Errorf("media file not found: '%s'", path)
		}
		if st.Size() > maxIngestBytes {
			return nil, "", errors.New("media exceeds size limit")
		}
		data, err = os.ReadFile(path)
		if err != nil {
			return nil, "", err
		}
		declared = mimeByExtension(path)
	}
	if len(data) > maxIngestBytes {
		return nil, "", errors.New("media exceeds size limit")
	}
	if mime := sniffImageMime(data); mime != "" {
		return data, mime, nil
	}
	if bytes.Contains(bytes.ToLower(data[:min(len(data), 4096)]), []byte("<svg")) {
		return data, "image/svg+xml", nil
	}
	if permitVideo {
		if strings.HasPrefix(declared, "video/") {
			return data, declared, nil
		}
		if len(data) > 12 && bytes.Equal(data[4:8], []byte("ftyp")) {
			return data, "video/mp4", nil
		}
		return nil, "", errors.New("source is not a recognized video (mp4 expected)")
	}
	return nil, "", errors.New("source is not a recognized image")
}

// Downloads a URL into memory with a size cap
func downloadBytes(ctx context.Context, rawURL string, maxBytes int64) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", UserAgent)
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d fetching %s", resp.StatusCode, rawURL)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > maxBytes {
		return nil, errors.New("media exceeds size limit")
	}
	return raw, nil
}

// Converts a path like source into a data URL, URL shaped sources pass through
func localSourceToDataURL(ctx context.Context, env *tools.Env, src string, permitVideo bool) (string, error) {
	s := strings.TrimSpace(src)
	if s == "" || isRemoteRef(s) {
		return src, nil
	}
	data, mime, err := resolveImageSource(ctx, env, s, permitVideo)
	if err != nil {
		return "", err
	}
	return dataURL(mime, data), nil
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
