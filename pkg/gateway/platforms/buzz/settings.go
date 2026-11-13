package buzz

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/provider"
)

const (
	defaultPollInterval = 4.0
	minPollInterval     = 1.0
)

var (
	hex64RE    = regexp.MustCompile(`^[0-9a-f]{64}$`)
	falseWords = map[string]bool{"false": true, "0": true, "no": true, "off": true}
	keyFields  = []string{"nsec", "private_key_hex", "private_key"}
)

// Whitespace the way Unicode aware string stripping and regex classes see it
func isSpace(r rune) bool { return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f) }

// Text with surrounding whitespace removed
func strip(s string) string { return strings.TrimFunc(s, isSpace) }

// Home relative path expanded against the user's home directory
func expandUser(p string) string {
	if p != "~" && !strings.HasPrefix(p, "~/") && !(runtime.GOOS == "windows" && strings.HasPrefix(p, `~\`)) {
		return p
	}
	h, err := os.UserHomeDir()
	if err != nil || h == "" {
		return p
	}
	return h + p[1:]
}

// Whether the path names an existing regular file
func isFile(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.Mode().IsRegular()
}

// Decoded JSON value with numbers kept exact, nil and an error for malformed text
func decodeJSON(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, fmt.Errorf("trailing data after JSON value")
	}
	return v, nil
}

// Decoded JSON of text, the fallback for blank or malformed text
func jsonOr(text string, fallback any) any {
	if strip(text) == "" {
		return fallback
	}
	v, err := decodeJSON([]byte(text))
	if err != nil {
		return fallback
	}
	return v
}

// Objects of a JSON array text, empty for anything else
func parseJSONList(text string) []map[string]any {
	list, _ := jsonOr(text, nil).([]any)
	out := []map[string]any{}
	for _, item := range list {
		if m, ok := item.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// Display text of a JSON or config scalar, empty for null
func textOf(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case json.Number:
		return x.String()
	case bool:
		if x {
			return "true"
		}
		return "false"
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	}
	return fmt.Sprint(v)
}

// Truthiness of a decoded JSON or config value
func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case json.Number:
		f, err := x.Float64()
		return err != nil || f != 0
	case float64:
		return x != 0
	case int:
		return x != 0
	case int64:
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

// Integer of a JSON number or numeric string, false when it does not convert
func intOf(v any) (int64, bool) {
	switch x := v.(type) {
	case nil:
		return 0, true
	case json.Number:
		if n, err := x.Int64(); err == nil {
			return n, true
		}
		f, err := x.Float64()
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			return 0, false
		}
		return int64(f), true
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return 0, false
		}
		return int64(x), true
	case int:
		return int64(x), true
	case int64:
		return x, true
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	case string:
		n, err := strconv.ParseInt(strip(x), 10, 64)
		return n, err == nil
	}
	return 0, false
}

// Integer of a value, zero when it is falsy or does not convert
func intOr0(v any) int64 {
	if !truthy(v) {
		return 0
	}
	n, _ := intOf(v)
	return n
}

// Entries of a comma separated string or a list
func splitCSV(raw any) []any {
	switch x := raw.(type) {
	case string:
		parts := strings.Split(x, ",")
		out := make([]any, len(parts))
		for i, p := range parts {
			out[i] = p
		}
		return out
	case []any:
		return x
	case []string:
		out := make([]any, len(x))
		for i, s := range x {
			out[i] = s
		}
		return out
	}
	return nil
}

// Lowercase hex of a hex pubkey or npub reference, empty when it is neither
func normalizeUserRef(ref string) string {
	ref = strings.ToLower(strip(ref))
	if ref == "" {
		return ""
	}
	if strings.HasPrefix(ref, "npub1") {
		return npubToHex(ref)
	}
	if hex64RE.MatchString(ref) {
		return ref
	}
	return ""
}

// Hex pubkey set of a comma separated or listed set of hex pubkeys and npubs
func pubkeySet(raw any) map[string]bool {
	out := map[string]bool{}
	for _, entry := range splitCSV(raw) {
		if s, ok := entry.(string); ok {
			if n := normalizeUserRef(s); n != "" {
				out[n] = true
			}
		}
	}
	return out
}

// Scope aware setting reads for one profile, the first lookup failure kept for the caller
type settings struct {
	ctx   context.Context
	extra map[string]any
	err   error
}

// Records the first lookup failure
func (s *settings) fail(err error) {
	if err != nil && s.err == nil {
		s.err = err
	}
}

// Scoped secret read with the startup profile scope fallback
func (s *settings) secret(name string) string {
	v, err := gateway.GetScopedSecret(s.ctx, name, "", true)
	s.fail(err)
	return v
}

// Raw setting, the profile's own env then its extra inside a secondary profile scope, the process env elsewhere
func (s *settings) lookup(env, key string) (any, bool) {
	if gateway.ProfileScoped(s.ctx) {
		v, found, err := gateway.LookupScopedSecret(s.ctx, env, true)
		s.fail(err)
		if found {
			return v, true
		}
		raw, ok := s.extra[key]
		return raw, ok && raw != nil
	}
	return os.LookupEnv(env)
}

// Raw setting, or the extra value and then the default when the setting is unset
func (s *settings) or(env, key string, def any) any {
	if raw, ok := s.lookup(env, key); ok {
		return raw
	}
	if raw, ok := s.extra[key]; ok && raw != nil {
		return raw
	}
	return def
}

// Stripped setting text, the extra value when the setting is blank
func (s *settings) text(env, key string) string {
	raw, _ := s.lookup(env, key)
	if !truthy(raw) {
		raw = s.extra[key]
	}
	return strip(textOf(raw))
}

// Relay base URL
func (s *settings) relayURL() string { return s.text("BUZZ_RELAY_URL", "relay_url") }

// Home channel id for cron and notification delivery
func (s *settings) homeChannel() string { return s.text("BUZZ_HOME_CHANNEL", "home_channel") }

// Resolved buzz binary, an explicit path then buzz on PATH then the home bin directory, empty when none exists
func (s *settings) cliPath() string {
	raw, _ := s.lookup("BUZZ_CLI_PATH", "cli_path")
	configured := strip(textOf(raw))
	if configured == "" {
		configured = textOf(s.extra["cli_path"])
	}
	return resolveCLIPath(configured)
}

// Buzz binary path for a configured location, else buzz on PATH, else the home bin copy
func resolveCLIPath(configured string) string {
	if configured != "" {
		p := expandUser(configured)
		if isFile(p) {
			return p
		}
		return ""
	}
	if found, err := exec.LookPath("buzz"); err == nil {
		return found
	}
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		if fallback := filepath.Join(h, "bin", "buzz"); isFile(fallback) {
			return fallback
		}
	}
	return ""
}

// Channels to watch, empty meaning every joined channel
func (s *settings) channels() []string {
	out := []string{}
	for _, c := range splitCSV(s.or("BUZZ_CHANNELS", "channels", []any{})) {
		if str, ok := c.(string); ok && strip(str) != "" {
			out = append(out, strip(str))
		}
	}
	return out
}

// Seconds between poll sweeps, floored at the minimum and defaulted when unparseable
func (s *settings) pollInterval() float64 {
	raw, _ := s.lookup("BUZZ_POLL_INTERVAL", "poll_interval")
	if !truthy(raw) {
		raw = s.extra["poll_interval"]
		if raw == nil {
			raw = defaultPollInterval
		}
	}
	var v float64
	switch x := raw.(type) {
	case float64:
		v = x
	case int:
		v = float64(x)
	case int64:
		v = float64(x)
	case bool:
		if x {
			v = 1
		}
	case json.Number:
		f, err := x.Float64()
		if err != nil {
			return defaultPollInterval
		}
		v = f
	case string:
		f, err := strconv.ParseFloat(strip(x), 64)
		if err != nil {
			return defaultPollInterval
		}
		v = f
	default:
		return defaultPollInterval
	}
	if math.IsNaN(v) {
		return defaultPollInterval
	}
	return math.Max(minPollInterval, v)
}

// Whether channel messages must address the agent, DMs always dispatching
func (s *settings) requireMention() bool {
	return !falseWords[strings.ToLower(strip(textOf(s.or("BUZZ_REQUIRE_MENTION", "require_mention", true))))]
}

// Inbound transport, auto, websocket or poll
func (s *settings) transport() string {
	raw, _ := s.lookup("BUZZ_TRANSPORT", "transport")
	if !truthy(raw) {
		raw = s.extra["transport"]
		if !truthy(raw) {
			raw = "auto"
		}
	}
	t := strings.ToLower(strip(textOf(raw)))
	switch t {
	case "auto", "websocket", "poll":
		return t
	}
	return "auto"
}

// Reply threading mode, first or all thread replies and off posts flat, reply_in_thread false forcing off
func (s *settings) replyToMode(config *gateway.PlatformConfig) string {
	var modeEnv string
	if gateway.ProfileScoped(s.ctx) {
		modeEnv = s.secret("BUZZ_REPLY_TO_MODE")
	} else {
		modeEnv = os.Getenv("BUZZ_REPLY_TO_MODE")
	}
	mode := modeEnv
	if mode == "" && config != nil && truthy(config.ReplyToMode) {
		mode = textOf(config.ReplyToMode)
	}
	if mode == "" {
		mode = "first"
	}
	mode = strings.ToLower(strip(mode))
	if rit := s.or("BUZZ_REPLY_IN_THREAD", "reply_in_thread", nil); rit != nil && falseWords[strings.ToLower(strip(textOf(rit)))] {
		return "off"
	}
	return mode
}

// Configured credentials file, the scoped secret first and the extra after
func (s *settings) credentialsFile() string {
	if configured := strip(s.secret("BUZZ_CREDENTIALS_FILE")); configured != "" {
		return configured
	}
	return strip(textOf(s.extra["credentials_file"]))
}

// Credentials files to read, the configured one or the buzz CLI records outside multiplexing
func (s *settings) credentialsCandidates() []string {
	if configured := s.credentialsFile(); configured != "" {
		return []string{expandUser(configured)}
	}
	if provider.IsMultiplexActive(s.ctx) {
		return nil
	}
	h, err := os.UserHomeDir()
	if err != nil || h == "" {
		return nil
	}
	matches, err := filepath.Glob(filepath.Join(h, ".config", "buzz", "*credentials*.json"))
	if err != nil {
		return nil
	}
	return matches
}

// First non empty private key field of a credentials record, stripped
func credentialsKey(data map[string]any) string {
	for _, field := range keyFields {
		if value, ok := data[field].(string); ok && strip(value) != "" {
			return strip(value)
		}
	}
	return ""
}

// First credentials record carrying a private key, unreadable and malformed files skipped
func (s *settings) credentialsData() map[string]any {
	for _, path := range s.credentialsCandidates() {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		raw = bytes.TrimPrefix(raw, []byte("\ufeff"))
		if !utf8.Valid(raw) {
			continue
		}
		decoded, err := decodeJSON(raw)
		if err != nil {
			continue
		}
		if data, ok := decoded.(map[string]any); ok && credentialsKey(data) != "" {
			return data
		}
	}
	return map[string]any{}
}

// Nostr private key, the scoped secret first and the credentials record after, never logged
func (s *settings) privateKey() string {
	if key := strip(s.secret("BUZZ_PRIVATE_KEY")); key != "" {
		return key
	}
	return credentialsKey(s.credentialsData())
}

// Compact NIP-OA owner attestation tag JSON, empty when none is configured
func (s *settings) authTag() (string, error) {
	var raw any = strip(s.secret("BUZZ_AUTH_TAG"))
	if raw == "" {
		if strip(s.secret("BUZZ_PRIVATE_KEY")) != "" && s.credentialsFile() == "" {
			return "", nil
		}
		value, ok := s.credentialsData()["auth_tag"]
		if !ok {
			return "", nil
		}
		raw = value
	}
	tag, err := parseAuthTag(raw, "Buzz auth tag")
	if err != nil {
		return "", err
	}
	encoded, err := json.Marshal(tag)
	return string(encoded), err
}
