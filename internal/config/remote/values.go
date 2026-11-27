package remote

import (
	"fmt"
	"log/slog"
	"math"
	"math/big"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Largest integer a JSON number carries exactly
const maxSafeInteger = 1<<53 - 1

// Value the plane refuses to store, nothing sent
type ValueError struct{ Message string }

func (e *ValueError) Error() string { return e.Message }

func shownPath(path Path) string {
	if len(path) == 0 {
		return "<root>"
	}
	return Encode(path)
}

func extend(path Path, seg string) Path {
	return append(append(Path{}, path...), seg)
}

// Value as the plane stores it in JSON, a ValueError naming the key when it has no such form
func toWire(value any, path Path) (any, error) {
	switch v := value.(type) {
	case nil, string, bool:
		return v, nil
	case int:
		return wireInt(big.NewInt(int64(v)), path), nil
	case int64:
		return wireInt(big.NewInt(v), path), nil
	case uint64:
		return wireInt(new(big.Int).SetUint64(v), path), nil
	case float32:
		return toWire(float64(v), path)
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return nil, &ValueError{shownPath(path) + ": NaN/Infinity cannot be stored in Remote Config"}
		}
		return v, nil
	case time.Time:
		// A YAML timestamp, as text with its microseconds when it has any and its zone offset
		text := v.Format("2006-01-02T15:04:05")
		if micro := v.Nanosecond() / 1000; micro != 0 {
			text += fmt.Sprintf(".%06d", micro)
		}
		return text + v.Format("-07:00"), nil
	case []string:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = item
		}
		return out, nil
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			wire, err := toWire(item, path)
			if err != nil {
				return nil, err
			}
			out[i] = wire
		}
		return out, nil
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			wire, err := toWire(item, extend(path, key))
			if err != nil {
				return nil, err
			}
			out[key] = wire
		}
		return out, nil
	case map[any]any:
		out := make(map[string]any, len(v))
		for k, item := range v {
			key, err := wireKey(k, path)
			if err != nil {
				return nil, err
			}
			if _, dup := out[key]; dup {
				return nil, &ValueError{fmt.Sprintf("%s: two keys become the same string key %s", Encode(extend(path, key)), strconv.Quote(key))}
			}
			wire, err := toWire(item, extend(path, key))
			if err != nil {
				return nil, err
			}
			out[key] = wire
		}
		return out, nil
	}
	if rv := reflect.ValueOf(value); rv.CanInt() {
		return wireInt(big.NewInt(rv.Int()), path), nil
	} else if rv.CanUint() {
		return wireInt(new(big.Int).SetUint64(rv.Uint()), path), nil
	}
	kind := fmt.Sprintf("%T", value)
	if _, binary := value.([]byte); binary {
		kind = "binary"
	}
	return nil, &ValueError{fmt.Sprintf("%s: a %s value cannot be stored in Remote Config", shownPath(path), kind)}
}

// Integer as JSON carries it exactly, a decimal string with a warning beyond what a JSON number holds
func wireInt(n *big.Int, path Path) any {
	if n.IsInt64() && n.Int64() >= -maxSafeInteger && n.Int64() <= maxSafeInteger {
		return int(n.Int64())
	}
	slog.Warn(fmt.Sprintf("config path %s: integer %s is outside ±(2^53-1); sent as a decimal string", shownPath(path), n.String()))
	return n.String()
}

// Key of a mapping as the plane stores it, a ValueError for a key that is neither text nor a whole number
func wireKey(key any, path Path) (string, error) {
	switch k := key.(type) {
	case string:
		return k, nil
	case int:
		return strconv.Itoa(k), nil
	case int64:
		return strconv.FormatInt(k, 10), nil
	case uint64:
		return strconv.FormatUint(k, 10), nil
	}
	return "", &ValueError{fmt.Sprintf("%s: a %T mapping key (%v) cannot be stored in Remote Config; quote it in YAML", shownPath(path), key, key)}
}

// What the plane counts as a secret, the same rules it refuses literal secrets by
var (
	secretKeys = []string{"api_key", "apikey", "key", "token", "access_token", "refresh_token", "id_token", "secret", "client_secret", "password",
		"passwd", "authorization", "private_key", "bearer", "jwt"}
	secretSuffixes    = []string{"_api_key", "_token", "_secret", "_password", "_key", "_access_key"}
	nonSecretSuffixes = []string{"_url", "_host", "_user", "_id", "_domain", "_scheme"}
	envKeys           = []string{"OPENROUTER_API_KEY", "OPENAI_API_KEY", "ANTHROPIC_API_KEY", "VOICE_TOOLS_OPENAI_KEY", "EXA_API_KEY", "PARALLEL_API_KEY",
		"FIRECRAWL_API_KEY", "FIRECRAWL_API_URL", "TAVILY_API_KEY", "PERPLEXITY_API_KEY", "API_SERVER_KEY", "BROWSERBASE_API_KEY",
		"BROWSERBASE_PROJECT_ID", "BROWSER_USE_API_KEY", "FAL_KEY", "TELEGRAM_BOT_TOKEN", "DISCORD_BOT_TOKEN", "TERMINAL_SSH_HOST",
		"TERMINAL_SSH_USER", "TERMINAL_SSH_KEY", "SUDO_PASSWORD", "SLACK_BOT_TOKEN", "SLACK_APP_TOKEN", "GITHUB_TOKEN"}
	envSuffixes    = []string{"_API_KEY", "_TOKEN", "_SECRET"}
	envPrefixes    = []string{"TERMINAL_SSH"}
	nonSecretPaths = [][]string{{"voice", "record_key"}, {"terminal", "docker_shared_container_key"}, {"browser", "camofox", "session_key"}}
	// The only value a secret shaped key may hold on the plane: a reference to a variable
	placeholder = regexp.MustCompile(`^\$\{[A-Za-z_][A-Za-z0-9_]*\}$`)
)

func hasSuffix(s string, suffixes []string) bool {
	return slices.ContainsFunc(suffixes, func(x string) bool { return strings.HasSuffix(s, x) })
}

// Whether a key at the top names a variable of the .env
func envConfigKey(key string) bool {
	if strings.Contains(key, ".") {
		return false
	}
	up := strings.ToUpper(key)
	return slices.Contains(envKeys, up) || hasSuffix(up, envSuffixes) ||
		slices.ContainsFunc(envPrefixes, func(p string) bool { return strings.HasPrefix(up, p) })
}

// Whether a key holds a secret by its shape, list positions left out of its segments
func secretPath(segments []string) bool {
	for _, p := range nonSecretPaths {
		if slices.Equal(p, segments) {
			return false
		}
	}
	last := segments[len(segments)-1]
	leaf := strings.ReplaceAll(strings.ToLower(last), "-", "_")
	if len(segments) == 1 && envConfigKey(last) {
		return !hasSuffix(leaf, nonSecretSuffixes)
	}
	return slices.Contains(secretKeys, leaf) || hasSuffix(leaf, secretSuffixes)
}

// Visits the value and everything inside it, items of a list taking the key of their list
func walk(value any, path Path, visit func(Path, any) bool) bool {
	if !visit(path, value) {
		return false
	}
	switch v := value.(type) {
	case map[string]any:
		for _, k := range sorted(v) {
			if !walk(v[k], extend(path, k), visit) {
				return false
			}
		}
	case []any:
		for _, item := range v {
			if !walk(item, path, visit) {
				return false
			}
		}
	}
	return true
}

// First key at or under the path of a set whose value is a secret itself rather than a reference to one, false when none is
func secretLiteral(path Path, value any) (Path, bool) {
	var found Path
	walk(value, path, func(p Path, node any) bool {
		if len(p) == 0 || !secretPath(p) {
			return true
		}
		switch n := node.(type) {
		case nil, bool, map[string]any, []any:
			return true
		case string:
			if n == "" || placeholder.MatchString(n) {
				return true
			}
		}
		found = p
		return false
	})
	return found, found != nil
}
