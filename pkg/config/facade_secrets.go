package config

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/home"
)

// Reads an env name through the registered secret scope, the plain process env when none is registered
func scopedEnvironGet(ctx context.Context, key string) (string, bool, error) {
	if fn := hooks().ScopedSecret; fn != nil {
		return fn(ctx, key)
	}
	v, ok := os.LookupEnv(key)
	return v, ok, nil
}

// Value from the scoped process env, else the home .env, a failed scoped read returned as an error
func GetEnvValue(ctx context.Context, h, key string) (string, bool, error) {
	v, ok, err := scopedEnvironGet(ctx, key)
	if err != nil || ok {
		return v, ok, err
	}
	v, ok = LoadEnv(h)[key]
	return v, ok, nil
}

// Credential from the home .env first, an unresolved op reference yielding to the scoped value
func GetEnvValuePreferDotenv(ctx context.Context, h, key string) (string, error) {
	raw := LoadEnv(h)[key]
	if strings.HasPrefix(strings.TrimLeft(raw, " \t\n\r\v\f"), "op://") {
		scoped, _, err := scopedEnvironGet(ctx, key)
		if err != nil {
			return "", err
		}
		if scoped != "" {
			return scoped, nil
		}
	}
	if raw != "" {
		return raw, nil
	}
	v, _, err := scopedEnvironGet(ctx, key)
	return v, err
}

// Publishes a persisted .env change to the active scope and to the process env unless a routed profile is served
func publishEnvValue(ctx context.Context, key string, value *string) {
	process := true
	if fn := hooks().PublishScopedEnv; fn != nil {
		process = fn(ctx, key, value)
	}
	if !process {
		return
	}
	if value == nil {
		os.Unsetenv(key)
		return
	}
	os.Setenv(key, *value)
}

// Prints the .env write refusal for key and reports whether the write is blocked
func envWriteBlocked(ctx context.Context, h, key, action string) bool {
	refusal := EnvWriteRefusal(ctx, h, key, action)
	if refusal != "" {
		fmt.Fprintln(EnvRefusalOut, refusal)
	}
	return refusal != ""
}

// Error carrying the refusal when the .env write lock forbids key
func RequireEnvWritable(ctx context.Context, h, key, action string) error {
	if refusal := EnvWriteRefusal(ctx, h, key, action); refusal != "" {
		return &EnvValueError{refusal}
	}
	return nil
}

// Managed scope file path for refusal messages, or a generic label without a managed dir
func managedSource(filename string) string {
	if dir := ManagedDir(); dir != "" {
		return filepath.Join(dir, filename)
	}
	return "the managed scope"
}

// Removes key from the home .env and the published env, reporting whether a line defined it
func RemoveHomeEnvValue(ctx context.Context, h, key string) (bool, error) {
	if envWriteBlocked(ctx, h, key, "remove") {
		return false, nil
	}
	if !envVarNameRe.MatchString(key) {
		return false, &EnvValueError{fmt.Sprintf("Invalid environment variable name: %s", pyStringRepr(key))}
	}
	path := home.EnvPath(h)
	if _, err := os.Stat(path); err != nil {
		publishEnvValue(ctx, key, nil)
		return false, nil
	}
	found := false
	err := withEnvWriteLock(path, func() error {
		lines, err := readEnvLinesFile(path)
		if err != nil {
			return err
		}
		kept := make([]string, 0, len(lines))
		for _, line := range lines {
			if !envLineDefines(line, key) {
				kept = append(kept, line)
			}
		}
		found = len(kept) < len(lines)
		if !found {
			return nil
		}
		return writeEnvLinesMode(ctx, h, path, kept, true)
	})
	if err != nil {
		return false, err
	}
	publishEnvValue(ctx, key, nil)
	InvalidateEnvCache()
	return found, nil
}

var endpointSlugRe = regexp.MustCompile(`[^A-Z0-9]+`)

// Env name holding a custom endpoint key, one slot per endpoint identity
func CustomEndpointKeyEnv(identity string) string {
	slug := strings.Trim(endpointSlugRe.ReplaceAllString(strings.ToUpper(identity), "_"), "_")
	if slug == "" {
		return "ICEBATH_CUSTOM_API_KEY"
	}
	return "ICEBATH_CUSTOM_" + slug + "_API_KEY"
}

// Writes both Anthropic credential slots, the token slot first unless tokenFirst is false
func writeAnthropicSlots(token, apiKey string, save func(name, value string) error, tokenFirst bool) error {
	order := [][2]string{{"ANTHROPIC_TOKEN", token}, {"ANTHROPIC_API_KEY", apiKey}}
	if !tokenFirst {
		order[0], order[1] = order[1], order[0]
	}
	for _, slot := range order {
		if err := save(slot[0], slot[1]); err != nil {
			return err
		}
	}
	return nil
}

// Home .env writer used when an Anthropic slot writer gets no save function
func homeEnvSaver(ctx context.Context, h string, save func(name, value string) error) func(name, value string) error {
	if save != nil {
		return save
	}
	return func(name, value string) error { return SaveEnvValueContext(ctx, h, name, value) }
}

// Persists an Anthropic OAuth or setup token and clears the API key slot
func SaveAnthropicOAuthToken(ctx context.Context, h, value string, save func(name, value string) error) error {
	return writeAnthropicSlots(value, "", homeEnvSaver(ctx, h, save), true)
}

// Clears both Anthropic slots so the Claude Code credential files are used
func UseAnthropicClaudeCodeCredentials(ctx context.Context, h string, save func(name, value string) error) error {
	return writeAnthropicSlots("", "", homeEnvSaver(ctx, h, save), true)
}

// Persists an Anthropic API key and clears the OAuth token slot
func SaveAnthropicAPIKey(ctx context.Context, h, value string, save func(name, value string) error) error {
	return writeAnthropicSlots("", value, homeEnvSaver(ctx, h, save), false)
}

// Outcome of a secure env save through the credential lifecycle
type SecureSaveResult struct {
	Success   bool
	StoredAs  string
	Validated bool
}

// Saves a credential through the provider credential lifecycle, refreshing config mirrors of the old value
func SaveEnvValueSecure(ctx context.Context, h, key, value string) (SecureSaveResult, error) {
	save := hooks().SaveProviderEnvCredential
	if save == nil {
		return SecureSaveResult{}, errors.New("provider credential lifecycle is not registered")
	}
	if err := save(ctx, h, key, value); err != nil {
		return SecureSaveResult{}, err
	}
	return SecureSaveResult{Success: true, StoredAs: key, Validated: false}, nil
}

var displayControlRe = regexp.MustCompile("[\x00-\x1f\x7f\u0080-\u009f​-‏‪-‮⁠-⁤]")

// Masks a secret for display as head...tail, shorter values become stars and empty ones the empty label
func MaskSecret(value, empty string) string {
	if value != "" {
		value = displayControlRe.ReplaceAllString(value, "")
	}
	if value == "" {
		return empty
	}
	runes := []rune(value)
	if len(runes) < 12 {
		return "***"
	}
	return string(runes[:4]) + "..." + string(runes[len(runes)-4:])
}

// Masked API key for display, a dim not set label when empty
func RedactKey(key string) string {
	return MaskSecret(key, colorize("(not set)", ansiDim))
}

const ansiDim = "\033[2m"

var secretConfigKeys = map[string]bool{
	"api_key": true, "apikey": true, "key": true, "token": true, "access_token": true, "refresh_token": true,
	"id_token": true, "secret": true, "client_secret": true, "password": true, "passwd": true,
	"authorization": true, "private_key": true, "bearer": true, "jwt": true,
}

// Exported lowercase config key names whose values are secrets
var SecretConfigKeys = secretConfigKeys

var (
	secretConfigKeySuffixes = []string{"_api_key", "_token", "_secret", "_password", "_key", "_access_key"}
	nonSecretKeySuffixes    = []string{"_url", "_host", "_user", "_id", "_domain", "_scheme"}
	envTemplateRe           = regexp.MustCompile(`^\$\{[A-Za-z_][A-Za-z0-9_]*\}$`)
)

var envConfigKeys = map[string]bool{
	"OPENROUTER_API_KEY": true, "OPENAI_API_KEY": true, "ANTHROPIC_API_KEY": true, "VOICE_TOOLS_OPENAI_KEY": true,
	"EXA_API_KEY": true, "PARALLEL_API_KEY": true, "FIRECRAWL_API_KEY": true, "FIRECRAWL_API_URL": true,
	"FIRECRAWL_GATEWAY_URL": true, "TOOL_GATEWAY_URL": true, "CONNECTOR_GATEWAY_URL": true,
	"TOOL_GATEWAY_DOMAIN": true, "TOOL_GATEWAY_SCHEME": true, "TOOL_GATEWAY_USER_TOKEN": true,
	"TAVILY_API_KEY": true, "PERPLEXITY_API_KEY": true, "API_SERVER_KEY": true, "BROWSERBASE_API_KEY": true,
	"BROWSERBASE_PROJECT_ID": true, "BROWSER_USE_API_KEY": true, "FAL_KEY": true, "TELEGRAM_BOT_TOKEN": true,
	"DISCORD_BOT_TOKEN": true, "TERMINAL_SSH_HOST": true, "TERMINAL_SSH_USER": true, "TERMINAL_SSH_KEY": true,
	"SUDO_PASSWORD": true, "SLACK_BOT_TOKEN": true, "SLACK_APP_TOKEN": true, "GITHUB_TOKEN": true, "HONCHO_API_KEY": true,
}

func hasAnySuffix(s string, suffixes []string) bool {
	for _, suf := range suffixes {
		if strings.HasSuffix(s, suf) {
			return true
		}
	}
	return false
}

// Reports whether config set routes this credential shaped dotless key to .env
func IsEnvConfigKey(key string) bool {
	if strings.Contains(key, ".") {
		return false
	}
	upper := strings.ToUpper(key)
	return envConfigKeys[upper] || hasAnySuffix(upper, []string{"_API_KEY", "_TOKEN", "_SECRET"}) ||
		strings.HasPrefix(upper, "TERMINAL_SSH")
}

// Reports whether the last segment of a config key names a credential value
func isSecretConfigKey(key string) bool {
	leaf := key
	if i := strings.LastIndex(key, "."); i >= 0 {
		leaf = key[i+1:]
	}
	leaf = strings.ReplaceAll(strings.ToLower(leaf), "-", "_")
	if IsEnvConfigKey(key) {
		return !hasAnySuffix(leaf, nonSecretKeySuffixes)
	}
	return secretConfigKeys[leaf] || hasAnySuffix(leaf, secretConfigKeySuffixes)
}

// Copy of a config value with credential shaped keys masked, env templates left visible
func RedactConfigValue(value any) any { return redactConfigValue(value, 0) }

func redactConfigValue(value any, depth int) any {
	if depth > 20 {
		return value
	}
	switch v := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		keys := MappingKeys(v)
		for _, k := range keys {
			item := v[k]
			if s, ok := item.(string); ok && isSecretConfigKey(k) && s != "" && !envTemplateRe.MatchString(s) {
				out[k] = MaskSecret(s, "")
				continue
			}
			out[k] = redactConfigValue(item, depth+1)
		}
		SetMappingKeys(out, keys)
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = redactConfigValue(item, depth+1)
		}
		return out
	}
	return value
}

var (
	redactMu     sync.RWMutex
	redactSwitch func(ctx context.Context) bool
)

// Installs the effective redaction switch for the active profile, called from the redaction owner init
func RegisterRedactionSwitch(enabled func(ctx context.Context) bool) {
	redactMu.Lock()
	redactSwitch = enabled
	redactMu.Unlock()
}

// Effective redaction switch, redacting when no owner has registered one as the security default does
func redactEnabled(ctx context.Context) bool {
	redactMu.RLock()
	fn := redactSwitch
	redactMu.RUnlock()
	if fn == nil {
		return true
	}
	return fn(ctx)
}
