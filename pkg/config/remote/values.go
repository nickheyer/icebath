package remote

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"math/big"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const MaxSafeInteger = 1<<53 - 1

//go:embed testdata/conformance/config-secret-keys.json
var secretKeysFixture []byte

func rootName(path Path) string {
	if len(path) == 0 {
		return "<root>"
	}
	return Encode(path)
}

func extend(path Path, seg string) Path {
	return append(append(Path{}, path...), seg)
}

// Value converted to its JSON wire form, a ConfigValueError naming the path when it has none
func ToWire(value any, path Path) (any, error) {
	switch v := value.(type) {
	case nil, string, bool:
		return v, nil
	case int:
		return wireInt(big.NewInt(int64(v)), path), nil
	case int64:
		return wireInt(big.NewInt(v), path), nil
	case uint64:
		return wireInt(new(big.Int).SetUint64(v), path), nil
	case *big.Int:
		return wireInt(v, path), nil
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return nil, config.NewConfigValueError(rootName(path)+": NaN/Infinity cannot be stored in Remote Config", "")
		}
		return v, nil
	case time.Time:
		return isoformat(v), nil
	case []any:
		out := make([]any, len(v))
		for i, e := range v {
			w, err := ToWire(e, path)
			if err != nil {
				return nil, err
			}
			out[i] = w
		}
		return out, nil
	case map[string]any:
		out := make(map[string]any, len(v))
		keys := config.MappingKeys(v)
		for _, k := range keys {
			w, err := ToWire(v[k], extend(path, k))
			if err != nil {
				return nil, err
			}
			out[k] = w
		}
		config.SetMappingKeys(out, keys)
		return out, nil
	case map[any]any:
		out := map[string]any{}
		var order []string
		for k, e := range v {
			key, err := wireKey(k, path)
			if err != nil {
				return nil, err
			}
			if _, dup := out[key]; dup {
				return nil, config.NewConfigValueError(fmt.Sprintf("%s: two keys become the same string key %s", Encode(extend(path, key)), pyrepr.StrRepr(key)), "")
			}
			w, err := ToWire(e, extend(path, key))
			if err != nil {
				return nil, err
			}
			out[key] = w
			order = append(order, key)
		}
		config.SetMappingKeys(out, order)
		return out, nil
	}
	return nil, config.NewConfigValueError(fmt.Sprintf("%s: a %s value cannot be stored in Remote Config", rootName(path), pyrepr.TypeName(value)), "")
}

// Integer kept when JSON safe, a decimal string with a warning when it is outside the safe range
func wireInt(n *big.Int, path Path) any {
	if n.IsInt64() && n.Int64() >= -MaxSafeInteger && n.Int64() <= MaxSafeInteger {
		return int(n.Int64())
	}
	slog.Warn(fmt.Sprintf("config path %s: integer %s is outside ±(2^53-1); sent as a decimal string", rootName(path), n.String()))
	return n.String()
}

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
	case *big.Int:
		return k.String(), nil
	}
	return "", config.NewConfigValueError(fmt.Sprintf("%s: a %s mapping key (%s) cannot be stored in Remote Config; quote it in YAML",
		rootName(path), pyrepr.TypeName(key), pyrepr.Repr(key)), "")
}

// Python isoformat of an aware datetime with microseconds when present
func isoformat(t time.Time) string {
	s := t.Format("2006-01-02T15:04:05")
	if us := t.Nanosecond() / 1000; us != 0 {
		s += fmt.Sprintf(".%06d", us)
	}
	return s + t.Format("-07:00")
}

type secretRules struct {
	secretKeys, envKeys                                       map[string]bool
	secretSuffixes, nonSecretSuffixes, envSuffixes, envPrefix []string
	placeholder                                               *regexp.Regexp
	nonSecretPaths                                            [][]string
}

var secretRulesOnce = sync.OnceValue(func() *secretRules {
	var data struct {
		SecretKeys           []string   `json:"secretKeys"`
		SecretKeySuffixes    []string   `json:"secretKeySuffixes"`
		NonSecretKeySuffixes []string   `json:"nonSecretKeySuffixes"`
		EnvConfigKeys        []string   `json:"envConfigKeys"`
		EnvConfigKeySuffixes []string   `json:"envConfigKeySuffixes"`
		EnvConfigKeyPrefixes []string   `json:"envConfigKeyPrefixes"`
		PlaceholderPattern   string     `json:"placeholderPattern"`
		NonSecretPaths       [][]string `json:"nonSecretPaths"`
	}
	raw := strings.TrimPrefix(string(secretKeysFixture), "\ufeff")
	if err := json.Unmarshal([]byte(raw), &data); err != nil {
		panic(fmt.Sprintf("vendored config-secret-keys.json is unreadable: %v", err))
	}
	set := func(keys []string) map[string]bool {
		out := make(map[string]bool, len(keys))
		for _, k := range keys {
			out[k] = true
		}
		return out
	}
	return &secretRules{
		secretKeys: set(data.SecretKeys), envKeys: set(data.EnvConfigKeys),
		secretSuffixes: data.SecretKeySuffixes, nonSecretSuffixes: data.NonSecretKeySuffixes,
		envSuffixes: data.EnvConfigKeySuffixes, envPrefix: data.EnvConfigKeyPrefixes,
		placeholder:    regexp.MustCompile(`^(?:` + data.PlaceholderPattern + `)$`),
		nonSecretPaths: data.NonSecretPaths,
	}
})

func hasAnySuffix(s string, suffixes []string) bool {
	return slices.ContainsFunc(suffixes, func(x string) bool { return strings.HasSuffix(s, x) })
}

func isEnvConfigKey(key string, rules *secretRules) bool {
	if strings.Contains(key, ".") {
		return false
	}
	up := pystr.Upper(key)
	return rules.envKeys[up] || hasAnySuffix(up, rules.envSuffixes) ||
		slices.ContainsFunc(rules.envPrefix, func(p string) bool { return strings.HasPrefix(up, p) })
}

// Secret shaped key rule on decoded segments with list indices already dropped
func IsSecretPath(segments []string) bool {
	rules := secretRulesOnce()
	for _, p := range rules.nonSecretPaths {
		if slices.Equal(p, segments) {
			return false
		}
	}
	last := segments[len(segments)-1]
	leaf := strings.ReplaceAll(pystr.Lower(last), "-", "_")
	if len(segments) == 1 && isEnvConfigKey(last, rules) {
		return !hasAnySuffix(leaf, rules.nonSecretSuffixes)
	}
	return rules.secretKeys[leaf] || hasAnySuffix(leaf, rules.secretSuffixes)
}

// Visits value and every node inside it, list elements inheriting the path of their list
func walk(value any, path Path, visit func(Path, any) bool) bool {
	if !visit(path, value) {
		return false
	}
	switch v := value.(type) {
	case map[string]any:
		for _, k := range config.MappingKeys(v) {
			if !walk(v[k], extend(path, k), visit) {
				return false
			}
		}
	case []any:
		for _, e := range v {
			if !walk(e, path, visit) {
				return false
			}
		}
	}
	return true
}

// First path under a set of value at path that holds a secret literal, false when none does
func SecretLiteralPath(path Path, value any) (Path, bool) {
	placeholder := secretRulesOnce().placeholder
	var found Path
	walk(value, path, func(p Path, node any) bool {
		if len(p) == 0 || !IsSecretPath(p) {
			return true
		}
		switch n := node.(type) {
		case nil, bool:
			return true
		case string:
			if n == "" || placeholder.MatchString(n) {
				return true
			}
			found = p
			return false
		case int, int64, uint64, float64, *big.Int:
			found = p
			return false
		}
		return true
	})
	return found, found != nil
}
