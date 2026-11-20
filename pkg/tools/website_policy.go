package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

const websitePolicyTTL = 30 * time.Second

// WebsitePolicyError reports a malformed website policy
type WebsitePolicyError struct{ Msg string }

func (e *WebsitePolicyError) Error() string { return e.Msg }

// WebsiteRule is one normalized blocklist pattern with its source
type WebsiteRule struct {
	Pattern string
	Source  string
}

// WebsitePolicy is the parsed blocklist of a config file
type WebsitePolicy struct {
	Enabled bool
	Rules   []WebsiteRule
}

// WebsiteBlock describes why a URL was refused by the policy
type WebsiteBlock struct {
	URL     string
	Host    string
	Rule    string
	Source  string
	Message string
}

var websitePolicyCache = struct {
	sync.Mutex
	byPath map[string]websitePolicyEntry
}{byPath: map[string]websitePolicyEntry{}}

type websitePolicyEntry struct {
	policy *WebsitePolicy
	at     time.Time
}

func freshWebsitePolicy(resolvedPath string, now time.Time) *WebsitePolicy {
	websitePolicyCache.Lock()
	defer websitePolicyCache.Unlock()
	if e, ok := websitePolicyCache.byPath[resolvedPath]; ok && now.Sub(e.at) < websitePolicyTTL {
		return e.policy
	}
	return nil
}

// Reduces a rule of host, URL, or host and path form to a lowercase host
func normalizeWebsiteRule(rule any) string {
	s, ok := rule.(string)
	if !ok {
		return ""
	}
	value := strings.ToLower(strings.TrimSpace(s))
	if value == "" || strings.HasPrefix(value, "#") {
		return ""
	}
	if strings.Contains(value, "://") {
		parts := splitURL(value)
		value = parts.netloc
		if value == "" {
			value = parts.path
		}
	}
	host := strings.SplitN(value, "/", 2)[0]
	host = strings.TrimPrefix(strings.TrimRight(strings.TrimSpace(host), "."), "www.")
	return host
}

func blocklistFileRules(env *Env, p string) []string {
	raw, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			envLog(env).Warn("Shared blocklist file not found (skipping)", "path", p)
		} else {
			envLog(env).Warn("Failed to read shared blocklist file (skipping)", "path", p, "error", err)
		}
		return nil
	}
	text := strings.TrimPrefix(string(raw), string(rune(0xFEFF)))
	var out []string
	for _, line := range strings.Split(text, "\n") {
		if r := normalizeWebsiteRule(strings.TrimRight(line, "\r")); r != "" {
			out = append(out, r)
		}
	}
	return out
}

func requireMapping(value any, label string) (map[string]any, error) {
	if value == nil {
		return map[string]any{}, nil
	}
	m, ok := value.(map[string]any)
	if !ok {
		return nil, &WebsitePolicyError{Msg: label + " must be a mapping"}
	}
	return m, nil
}

func loadWebsitePolicyConfig(configPath string) (map[string]any, error) {
	defaults := map[string]any{"enabled": false, "domains": []any{}, "shared_files": []any{}}
	if _, err := os.Stat(configPath); err != nil {
		return defaults, nil
	}
	doc, err := config.ReadDocument(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			return defaults, nil
		}
		if _, isPath := err.(*os.PathError); isPath {
			return nil, &WebsitePolicyError{Msg: fmt.Sprintf("Failed to read config file %s: %v", configPath, err)}
		}
		return nil, &WebsitePolicyError{Msg: fmt.Sprintf("Invalid config YAML at %s: %v", configPath, err)}
	}
	security, err := requireMapping(doc["security"], "security")
	if err != nil {
		return nil, err
	}
	blocklist, err := requireMapping(security["website_blocklist"], "security.website_blocklist")
	if err != nil {
		return nil, err
	}
	for k, v := range blocklist {
		defaults[k] = v
	}
	return defaults, nil
}

func requireList(policy map[string]any, key string) ([]any, error) {
	v, ok := policy[key]
	if !ok || v == nil {
		return []any{}, nil
	}
	list, ok := v.([]any)
	if !ok {
		return nil, &WebsitePolicyError{Msg: "security.website_blocklist." + key + " must be a list"}
	}
	return list, nil
}

func requireBool(policy map[string]any, key string, def bool) (bool, error) {
	v, ok := policy[key]
	if !ok {
		return def, nil
	}
	b, ok := v.(bool)
	if !ok {
		return false, &WebsitePolicyError{Msg: "security.website_blocklist." + key + " must be a boolean"}
	}
	return b, nil
}

// LoadWebsiteBlocklist parses the blocklist policy, caching the default config path for the TTL
func LoadWebsiteBlocklist(env *Env, configPath string) (*WebsitePolicy, error) {
	defaultPath := home.ConfigPath(env.Home)
	explicit := configPath != ""
	if !explicit {
		configPath = defaultPath
	}
	now := time.Now()
	if !explicit {
		if cached := freshWebsitePolicy(configPath, now); cached != nil {
			return cached, nil
		}
	}
	policy, err := loadWebsitePolicyConfig(configPath)
	if err != nil {
		return nil, err
	}
	domains, err := requireList(policy, "domains")
	if err != nil {
		return nil, err
	}
	type pair struct{ pattern, source string }
	var pairs []pair
	for _, d := range domains {
		if p := normalizeWebsiteRule(d); p != "" {
			pairs = append(pairs, pair{p, "config"})
		}
	}
	shared, err := requireList(policy, "shared_files")
	if err != nil {
		return nil, err
	}
	enabled, err := requireBool(policy, "enabled", true)
	if err != nil {
		return nil, err
	}
	for _, sf := range shared {
		s, ok := sf.(string)
		if !ok || strings.TrimSpace(s) == "" {
			continue
		}
		p := expandUser(s)
		if !filepath.IsAbs(p) {
			p = filepath.Join(env.Home, p)
			if resolved, err := filepath.EvalSymlinks(p); err == nil {
				p = resolved
			}
		}
		for _, r := range blocklistFileRules(env, p) {
			pairs = append(pairs, pair{r, p})
		}
	}
	seen := map[pair]bool{}
	result := &WebsitePolicy{Enabled: enabled}
	for _, p := range pairs {
		if seen[p] {
			continue
		}
		seen[p] = true
		result.Rules = append(result.Rules, WebsiteRule{Pattern: p.pattern, Source: p.source})
	}
	if !explicit {
		websitePolicyCache.Lock()
		websitePolicyCache.byPath[configPath] = websitePolicyEntry{policy: result, at: now}
		websitePolicyCache.Unlock()
	}
	return result, nil
}

// Glob matches wildcard rules and parent domain matches bare hosts
func matchHostAgainstRule(host, pattern string) bool {
	if host == "" || pattern == "" {
		return false
	}
	if strings.HasPrefix(pattern, "*.") {
		ok, err := filepath.Match(pattern, host)
		return err == nil && ok
	}
	return host == pattern || strings.HasSuffix(host, "."+pattern)
}

func extractHostFromURLish(raw string) string {
	host, _ := urlHostname(raw, false)
	if host == "" && !strings.Contains(raw, "://") {
		host, _ = urlHostname("//"+raw, false)
	}
	return normalizeHostname(host)
}

// CheckWebsiteAccess returns nil when the URL is allowed else the block metadata, failing open on policy errors
func CheckWebsiteAccess(env *Env, rawURL, configPath string) (*WebsiteBlock, error) {
	if configPath == "" {
		if cached := freshWebsitePolicy(home.ConfigPath(env.Home), time.Now()); cached != nil && !cached.Enabled {
			return nil, nil
		}
	}
	host := extractHostFromURLish(rawURL)
	if host == "" {
		return nil, nil
	}
	policy, err := LoadWebsiteBlocklist(env, configPath)
	if err != nil {
		if configPath != "" {
			return nil, err
		}
		envLog(env).Warn("Website policy config error (failing open)", "error", err)
		return nil, nil
	}
	if !policy.Enabled {
		return nil, nil
	}
	for _, rule := range policy.Rules {
		if matchHostAgainstRule(host, rule.Pattern) {
			envLog(env).Info("Blocked URL by website policy", "url", rawURL, "rule", rule.Pattern, "source", rule.Source)
			return &WebsiteBlock{URL: rawURL, Host: host, Rule: rule.Pattern, Source: rule.Source,
				Message: fmt.Sprintf("Blocked by website policy: '%s' matched rule '%s' from %s", host, rule.Pattern, rule.Source)}, nil
		}
	}
	return nil, nil
}

// Website policy check through the default config path, nil when allowed
func checkWebsiteAccess(env *Env, rawURL string) *WebsiteBlock {
	block, _ := CheckWebsiteAccess(env, rawURL, "")
	return block
}

// Attaches the policy block fields a refused URL carries
func blockedByPolicy(b *WebsiteBlock) map[string]any {
	return map[string]any{"host": b.Host, "rule": b.Rule, "source": b.Source}
}
