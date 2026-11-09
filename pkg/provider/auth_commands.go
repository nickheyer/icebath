package provider

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
)

// Providers that support OAuth login in addition to API keys
var authOAuthCapableProviders = map[string]bool{"anthropic": true, "nous": true, "openai-codex": true, "xai-oauth": true, "qwen-oauth": true, "minimax-oauth": true, "openrouter": true}

// OAuth capable providers that default to OAuth when --type is omitted, OpenRouter stays API key first
var authOAuthDefaultProviders = map[string]bool{"anthropic": true, "nous": true, "openai-codex": true, "xai-oauth": true, "qwen-oauth": true, "minimax-oauth": true}

// ExternalLoginProviders are the providers whose sibling CLI login icebath may borrow
var ExternalLoginProviders = map[string]bool{"anthropic": true, "openai-codex": true}

var authCmdProviderAliases = map[string]string{
	"or": "openrouter", "open-router": "openrouter", "grok-oauth": "xai-oauth",
	"xai-oauth": "xai-oauth", "x-ai-oauth": "xai-oauth", "xai-grok-oauth": "xai-oauth"}

// AuthExitError is the SystemExit an auth subcommand raises, a message exits 1 and a bare code exits with it
type AuthExitError struct {
	Code    int
	Message string
}

func (e *AuthExitError) Error() string {
	if e.Message == "" {
		return "exit " + strconv.Itoa(e.Code)
	}
	return e.Message
}

func authExit(message string) error { return &AuthExitError{Code: 1, Message: message} }

// Exit code and message of a SystemExit shaped error, false for any other error
func authSystemExit(err error) (int, string, bool) {
	var exit *AuthExitError
	if errors.As(err, &exit) {
		return exit.Code, exit.Message, true
	}
	var oauthExit *OAuthLoginExitError
	if errors.As(err, &oauthExit) {
		return oauthExit.Code, authErrText(oauthExit.Err), true
	}
	var nousExit *NousLoginExitError
	if errors.As(err, &nousExit) {
		return nousExit.Code, authErrText(nousExit.Err), true
	}
	var pluginErr *PluginAuthError
	if errors.As(err, &pluginErr) {
		return 1, pluginErr.Message, true
	}
	var unknown *UnknownAuthProviderError
	if errors.As(err, &unknown) {
		return 1, unknown.Error(), true
	}
	return 0, "", false
}

func authErrText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// One icebath auth invocation over its parsed namespace and output streams
type authCLI struct {
	ctx  context.Context
	env  PoolEnv
	ns   map[string]any
	out  io.Writer
	errw io.Writer
}

func (c *authCLI) with(ns map[string]any) *authCLI {
	return &authCLI{ctx: c.ctx, env: c.env, ns: ns, out: c.out, errw: c.errw}
}

var authCmdInputFn = func(out io.Writer, prompt string) (string, error) { return pickerStdinInput(out, prompt) }
var authCmdLineInputFn = lineInput
var authCmdMaskedSecretPromptFn = maskedSecretPrompt
var authCmdStdinIsTTYFn = func() bool { return streamIsTTY(os.Stdin) }

// Stripped answer from the reader, default input, false when the user hits EOF or Ctrl-C
func (c *authCLI) ask(prompt string, reader func(string) (string, error)) (string, bool) {
	if reader == nil {
		reader = func(p string) (string, error) { return authCmdInputFn(c.out, p) }
	}
	answer, err := reader(prompt)
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(answer), true
}

// str(value or "") of a namespace or config value
func authTextOr(v any) string {
	if !pyTruthy(v) {
		return ""
	}
	return pyStr(v)
}

func authInt(v any) (int, bool) {
	switch t := v.(type) {
	case int:
		return t, true
	case int64:
		return int(t), true
	case float64:
		return int(t), true
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(t))
		return n, err == nil
	}
	return 0, false
}

func authInt64(v any) int64 {
	switch t := v.(type) {
	case int:
		return int64(t)
	case int64:
		return t
	case float64:
		return int64(t)
	case string:
		n, _ := strconv.ParseInt(strings.TrimSpace(t), 10, 64)
		return n
	}
	return 0
}

// The timeout flag in seconds as a duration, the fallback when unset or not positive
func authTimeout(ns map[string]any, fallback time.Duration) time.Duration {
	seconds := 0.0
	switch t := ns["timeout"].(type) {
	case float64:
		seconds = t
	case int:
		seconds = float64(t)
	case string:
		seconds, _ = strconv.ParseFloat(strings.TrimSpace(t), 64)
	}
	if seconds <= 0 {
		return fallback
	}
	return time.Duration(seconds * float64(time.Second))
}

// The --priority flag, false when omitted or left at the parser's -1 sentinel
func authWantedPriority(ns map[string]any) (int, bool) {
	n, ok := authInt(ns["priority"])
	if !ok || n == -1 {
		return 0, false
	}
	return n, true
}

func authSortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Six hex characters like uuid4().hex[:6]
func authNewEntryID() string {
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

// Argparse style argv of the namespace for a plugin auth handler
func (c *authCLI) pluginArgs() []string {
	var args []string
	if v := authTextOr(c.ns["provider"]); v != "" {
		args = append(args, v)
	}
	if v := authTextOr(c.ns["target"]); v != "" {
		args = append(args, v)
	}
	if v := authTextOr(c.ns["auth_type"]); v != "" {
		args = append(args, "--type", v)
	}
	for _, key := range []string{"label", "api_key", "portal_url", "inference_url", "client_id", "scope", "ca_bundle"} {
		if v := authTextOr(c.ns[key]); v != "" {
			args = append(args, "--"+strings.ReplaceAll(key, "_", "-"), v)
		}
	}
	if n, ok := authWantedPriority(c.ns); ok {
		args = append(args, "--priority", strconv.Itoa(n))
	}
	if timeout := authTimeout(c.ns, 0); timeout > 0 {
		args = append(args, "--timeout", strconv.FormatFloat(timeout.Seconds(), 'f', -1, 64))
	}
	for _, key := range []string{"no_browser", "browser", "insecure"} {
		if nsFlag(c.ns, key) {
			args = append(args, "--"+strings.ReplaceAll(key, "_", "-"))
		}
	}
	return args
}

func (c *authCLI) loadConfig() map[string]any {
	cfg := LoadConfigSafe(envHome(c.env))
	if cfg == nil {
		return map[string]any{}
	}
	return cfg
}

// Configured provider entries with their legacy custom pool key and runtime slug
func (c *authCLI) customProviderEntries() []map[string]any {
	var result []map[string]any
	for _, entry := range CompatibleCustomProviders(c.loadConfig()) {
		name, _ := entry["name"].(string)
		if strings.TrimSpace(name) == "" {
			continue
		}
		row := maps.Clone(entry)
		row["name"] = strings.TrimSpace(name)
		row["pool_key"] = CustomPoolPrefix + normalizeCustomPoolName(name)
		row["provider_key"] = strings.TrimSpace(authTextOr(entry["provider_key"]))
		result = append(result, row)
	}
	return result
}

// The canonical providers.<key> entry of a provider, nil for blank or custom: input
func (c *authCLI) configuredProviderEntry(provider string) map[string]any {
	normalized := strings.ToLower(strings.TrimSpace(provider))
	if normalized == "" || strings.HasPrefix(normalized, CustomPoolPrefix) {
		return nil
	}
	for _, entry := range c.customProviderEntries() {
		if strings.ToLower(entry["provider_key"].(string)) == normalized {
			return entry
		}
	}
	return nil
}

// Legacy names and keyed providers resolved to their credential pool id, empty when unknown
func (c *authCLI) resolveCustomProviderInput(raw string) string {
	normalized := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(raw)), " ", "-")
	if normalized == "" {
		return ""
	}
	if strings.HasPrefix(normalized, CustomPoolPrefix) {
		return normalized
	}
	for _, entry := range c.customProviderEntries() {
		providerKey := strings.ToLower(entry["provider_key"].(string))
		if providerKey != "" && providerKey == normalized {
			return providerKey
		}
		if normalizeCustomPoolName(entry["name"].(string)) == normalized {
			if providerKey != "" {
				return providerKey
			}
			return entry["pool_key"].(string)
		}
	}
	return ""
}

func (c *authCLI) normalizeProvider(provider string) string {
	normalized := strings.ToLower(strings.TrimSpace(provider))
	if alias := authCmdProviderAliases[normalized]; alias != "" {
		return alias
	}
	if resolved := c.resolveCustomProviderInput(normalized); resolved != "" {
		return resolved
	}
	if alias := authPluginAliases(c.ctx)[normalized]; alias != "" {
		return alias
	}
	return normalized
}

// Moves a keyed provider's old custom: pool into its runtime slug and drops the legacy models cache
func (c *authCLI) migrateLegacyCustomPoolKey(provider, legacyKey string) error {
	migrated := false
	err := c.env.Store.Update(func(data AuthData) (bool, error) {
		pool, ok := data["credential_pool"].(map[string]any)
		if !ok {
			return false, nil
		}
		legacyEntries, ok := pool[legacyKey].([]any)
		if !ok || len(legacyEntries) == 0 {
			return false, nil
		}
		var merged []any
		if current, ok := pool[provider].([]any); ok {
			merged = append(merged, current...)
		}
		knownIDs := map[string]bool{}
		for _, e := range merged {
			if m, ok := e.(map[string]any); ok && authTextOr(m["id"]) != "" {
				knownIDs[authTextOr(m["id"])] = true
			}
		}
		for _, e := range legacyEntries {
			entryID := ""
			if m, ok := e.(map[string]any); ok {
				entryID = authTextOr(m["id"])
			}
			if entryID == "" || !knownIDs[entryID] {
				merged = append(merged, e)
				if entryID != "" {
					knownIDs[entryID] = true
				}
			}
		}
		pool[provider] = merged
		delete(pool, legacyKey)
		migrated = true
		return true, nil
	})
	if err != nil {
		return err
	}
	if migrated {
		ClearProviderModelsCache(c.ctx, legacyKey)
	}
	return nil
}

func (c *authCLI) providerBaseURL(provider string) string {
	if provider == "openrouter" {
		return home.OpenRouterBaseURL
	}
	if strings.HasPrefix(provider, CustomPoolPrefix) {
		return strings.TrimSpace(authTextOr(getCustomProviderConfig(provider, c.loadConfig())["base_url"]))
	}
	if configured := c.configuredProviderEntry(provider); configured != nil {
		return strings.TrimSpace(authTextOr(configured["base_url"]))
	}
	if p, ok := registryProfileCtx(c.ctx, provider); ok {
		return p.BaseURL
	}
	return ""
}

func (c *authCLI) isKnownProvider(provider string, configured map[string]any) bool {
	_, inRegistry := registryProfileCtx(c.ctx, provider)
	return inRegistry || provider == "openrouter" || strings.HasPrefix(provider, CustomPoolPrefix) || configured != nil
}

// Did you mean over the known provider ids plus the two commands that list or pick them
func (c *authCLI) unknownProviderExit(provider string) error {
	known := map[string]bool{"openrouter": true}
	for _, row := range canonicalRegistryRows(c.ctx) {
		known[row.Name] = true
	}
	for _, entry := range c.customProviderEntries() {
		known[entry["name"].(string)] = true
	}
	if moved := ProviderInstallHint(c.ctx, provider); moved != "" {
		return authExit(fmt.Sprintf("Unknown provider '%s'. %s", provider, moved))
	}
	hint := ""
	if close := difflibCloseMatches(provider, authSortedKeys(known), 3, 0.5); len(close) > 0 {
		hint = fmt.Sprintf(" Did you mean %s?", strings.Join(close, ", "))
	}
	return authExit(fmt.Sprintf("Unknown provider '%s'.%s Run `icebath auth` to see the provider list, or "+
		"`icebath model` to pick one interactively.", provider, hint))
}

func authDisplaySource(source string) string {
	if strings.HasPrefix(source, "manual:") {
		return strings.SplitN(source, ":", 2)[1]
	}
	return source
}

// Label, retry window flag, http codes, reason substrings and message substrings, first match wins
type authExhaustedClass struct {
	label         string
	retryWindow   bool
	codes         map[int]bool
	reasonTokens  []string
	messageTokens []string
}

var authExhaustedClasses = []authExhaustedClass{
	{"rate-limited", true, map[int]bool{429: true},
		[]string{"rate_limit", "usage_limit", "quota", "exhausted"},
		[]string{"rate limit", "usage limit", "quota", "too many requests"}},
	{"auth failed", false, map[int]bool{401: true, 403: true},
		[]string{"invalid_token", "invalid_grant", "unauthorized", "forbidden", "auth"},
		[]string{"unauthorized", "forbidden", "expired", "revoked", "invalid token", "authentication"}},
}

func authContainsAny(text string, tokens []string) bool {
	for _, t := range tokens {
		if strings.Contains(text, t) {
			return true
		}
	}
	return false
}

func classifyExhaustedStatus(entry PooledCredential) (string, bool) {
	reason := strings.ToLower(strings.TrimSpace(entry.LastErrorReason))
	message := strings.ToLower(strings.TrimSpace(entry.LastErrorMessage))
	for _, class := range authExhaustedClasses {
		if class.codes[entry.LastErrorCode] || authContainsAny(reason, class.reasonTokens) || authContainsAny(message, class.messageTokens) {
			return class.label, class.retryWindow
		}
	}
	return "exhausted", true
}

// Exhaustion suffix of a list row with the classified cause and the wait left before retry
func formatExhaustedStatus(entry PooledCredential) string {
	if entry.LastStatus != StatusExhausted {
		return ""
	}
	label, showRetryWindow := classifyExhaustedStatus(entry)
	reasonText := ""
	if strings.TrimSpace(entry.LastErrorReason) != "" {
		reasonText = " " + entry.LastErrorReason
	}
	code := ""
	if entry.LastErrorCode != 0 {
		code = fmt.Sprintf(" (%d)", entry.LastErrorCode)
	}
	head := " " + label + reasonText + code
	if !showRetryWindow {
		return head + " (re-auth may be required)"
	}
	until, ok := exhaustedUntil(entry, false)
	if !ok {
		return head
	}
	remaining := int(math.Ceil(until - float64(time.Now().UnixNano())/1e9))
	if remaining <= 0 {
		return head + " (ready to retry)"
	}
	minutes, seconds := remaining/60, remaining%60
	hours, minutes := minutes/60, minutes%60
	days, hours := hours/24, hours%24
	values := []int{days, hours, minutes, seconds}
	units := []string{"d", "h", "m", "s"}
	first := 3
	for i, v := range values {
		if v != 0 || i == 3 {
			first = i
			break
		}
	}
	var parts []string
	for i := first; i < len(values) && i < first+2; i++ {
		parts = append(parts, fmt.Sprintf("%d%s", values[i], units[i]))
	}
	return fmt.Sprintf("%s (%s left)", head, strings.Join(parts, " "))
}

var authCmdAnthropicOAuthLoginFn = func(c *authCLI) map[string]any {
	return RunHermesOAuthLoginPure(c.env, func(label string) (string, bool) {
		answer, err := authCmdInputFn(c.out, label)
		return answer, err == nil
	})
}
var authCmdMinimaxOAuthLoginFn = minimaxOAuthLogin
var authCmdOpenrouterPKCELoginFn = OpenrouterPKCELogin
var authCmdCodexOAuthLoginFn = codexOAuthLogin

func (c *authCLI) anthropicOAuthLogin() (map[string]any, error) {
	creds := authCmdAnthropicOAuthLoginFn(c)
	if len(creds) == 0 {
		return nil, authExit("Anthropic OAuth login did not return credentials.")
	}
	return creds, nil
}

func (c *authCLI) qwenOAuthLogin() (map[string]any, error) {
	creds, err := resolveQwenRuntimeCredentials(c.env, false)
	if err != nil {
		return nil, err
	}
	if err := markQwenOAuthActive(c.env, creds); err != nil {
		return nil, err
	}
	return creds, nil
}

func (c *authCLI) codexLogin() (map[string]any, error) {
	return authCmdCodexOAuthLoginFn(c.env, c.out, CodexLoginArgs{Browser: nsFlag(c.ns, "browser"), NoBrowser: nsFlag(c.ns, "no_browser"), Timeout: authTimeout(c.ns, 0)})
}

func authCodexPoolSource(creds map[string]any) string {
	if authTextOr(creds["source"]) == "loopback_pkce" {
		return SourceManual + ":loopback_pkce"
	}
	return SourceManualDeviceCode
}

func authNested(creds map[string]any, keys ...string) any {
	var current any = creds
	for _, key := range keys {
		m, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current = m[key]
	}
	return current
}

// Per provider parameters of the generic icebath auth add <provider> --type oauth path
type authOAuthAddSpec struct {
	login         func(c *authCLI) (map[string]any, error)
	token         func(creds map[string]any) string
	source        func(creds map[string]any) string
	fields        func(c *authCLI, creds map[string]any, provider string, entry *PooledCredential)
	activateFirst bool
	authType      string
}

func authConstSource(source string) func(map[string]any) string {
	return func(map[string]any) string { return source }
}

var authOAuthAddSpecs = map[string]authOAuthAddSpec{
	"anthropic": {
		login:  (*authCLI).anthropicOAuthLogin,
		token:  func(creds map[string]any) string { return authTextOr(creds["access_token"]) },
		source: authConstSource(SourceManual + ":hermes_pkce"),
		fields: func(c *authCLI, creds map[string]any, provider string, entry *PooledCredential) {
			entry.RefreshToken = authTextOr(creds["refresh_token"])
			entry.ExpiresAtMS = authInt64(creds["expires_at_ms"])
			entry.BaseURL = c.providerBaseURL(provider)
		}},
	"openai-codex": {
		login:  (*authCLI).codexLogin,
		token:  func(creds map[string]any) string { return authTextOr(authNested(creds, "tokens", "access_token")) },
		source: authCodexPoolSource,
		fields: func(c *authCLI, creds map[string]any, provider string, entry *PooledCredential) {
			entry.RefreshToken = authTextOr(authNested(creds, "tokens", "refresh_token"))
			entry.BaseURL = authTextOr(creds["base_url"])
			entry.LastRefresh = authTextOr(creds["last_refresh"])
		},
		activateFirst: true},
	"xai-oauth": {
		login: func(c *authCLI) (map[string]any, error) {
			return xaiOAuthDeviceCodeLoginFn(c.env, c.out, authTimeout(c.ns, 20*time.Second), !nsFlag(c.ns, "no_browser"))
		},
		token:  func(creds map[string]any) string { return authTextOr(authNested(creds, "tokens", "access_token")) },
		source: authConstSource(SourceManualDeviceCode),
		fields: func(c *authCLI, creds map[string]any, provider string, entry *PooledCredential) {
			entry.RefreshToken = authTextOr(authNested(creds, "tokens", "refresh_token"))
			entry.BaseURL = authTextOr(creds["base_url"])
			if entry.BaseURL == "" {
				entry.BaseURL = DefaultXAIOAuthBaseURL
			}
			entry.LastRefresh = authTextOr(creds["last_refresh"])
		},
		activateFirst: true},
	"qwen-oauth": {
		login:  (*authCLI).qwenOAuthLogin,
		token:  func(creds map[string]any) string { return authTextOr(creds["api_key"]) },
		source: authConstSource(SourceManual + ":qwen_cli"),
		fields: func(c *authCLI, creds map[string]any, provider string, entry *PooledCredential) {
			entry.BaseURL = authTextOr(creds["base_url"])
		}},
	"minimax-oauth": {
		login: func(c *authCLI) (map[string]any, error) {
			return authCmdMinimaxOAuthLoginFn(c.env, c.out, "", !nsFlag(c.ns, "no_browser"), authTimeout(c.ns, 15*time.Second))
		},
		token:  func(creds map[string]any) string { return authTextOr(creds["access_token"]) },
		source: authConstSource(SourceManual + ":minimax_oauth"),
		fields: func(c *authCLI, creds map[string]any, provider string, entry *PooledCredential) {
			entry.RefreshToken = authTextOr(creds["refresh_token"])
			entry.BaseURL = authTextOr(creds["inference_base_url"])
		}},
	"openrouter": {
		login: func(c *authCLI) (map[string]any, error) {
			return authCmdOpenrouterPKCELoginFn(c.env, c.out, !nsFlag(c.ns, "no_browser"), authTimeout(c.ns, 300*time.Second))
		},
		token:  func(creds map[string]any) string { return authTextOr(creds["api_key"]) },
		source: authConstSource(SourceManual + ":openrouter_pkce"),
		fields: func(c *authCLI, creds map[string]any, provider string, entry *PooledCredential) {
			entry.BaseURL = c.providerBaseURL(provider)
		},
		authType: AuthTypeAPIKey},
}

// icebath auth add nous --type oauth, a shared credential import else a device code login
func (c *authCLI) addNousOAuthCredential(provider string) (PooledCredential, error) {
	customLabel := strings.TrimSpace(authTextOr(c.ns["label"]))
	timeout := authTimeout(c.ns, 15*time.Second)
	persist := func(creds map[string]any, what string) (PooledCredential, error) {
		entry, present, err := persistNousCredentials(c.env, creds, customLabel)
		if err != nil {
			return PooledCredential{}, err
		}
		handoff, err := guestHandoffDrain(c.env)
		if err != nil {
			return PooledCredential{}, err
		}
		settleAfterUpgrade(c.env, creds)
		shownLabel := entry.Label
		if !present {
			shownLabel = LabelFromToken(authTextOr(creds["access_token"]), provider+"-oauth-1")
		}
		deviceCode := ""
		if what == "Saved" {
			deviceCode = "device-code "
		}
		fmt.Fprintf(c.out, "%s %s OAuth %scredentials: \"%s\"\n", what, provider, deviceCode, shownLabel)
		for _, view := range handoff {
			for _, line := range guestHandoffResultLines(view) {
				fmt.Fprintf(c.out, "  %s\n", line)
			}
		}
		return entry, nil
	}
	if shared, ok := readSharedNousState(c.env); ok && !isGuestState(shared) {
		found := "Found existing shared Nous OAuth credentials"
		if path := nousSharedStorePath(c.env); path != "" {
			found = "Found existing Nous OAuth credentials at " + path
		}
		fmt.Fprintln(c.out)
		fmt.Fprintln(c.out, found)
		doImport, ok := c.ask("Import these credentials? [Y/n]: ", nil)
		if !ok || map[string]bool{"": true, "y": true, "yes": true}[strings.ToLower(doImport)] {
			fmt.Fprintln(c.out, "Rehydrating Nous session from shared credentials...")
			if rehydrated, ok := tryImportSharedNousState(c.env, timeout); ok {
				return persist(rehydrated, "Imported")
			}
			fmt.Fprintln(c.out, "Could not refresh shared credentials — falling back to device-code login.")
		}
	}
	creds, err := nousDeviceCodeLoginFn(c.env, c.out, NousDeviceLoginArgs{
		PortalBaseURL: authTextOr(c.ns["portal_url"]), InferenceBaseURL: authTextOr(c.ns["inference_url"]),
		ClientID: authTextOr(c.ns["client_id"]), Scope: authTextOr(c.ns["scope"]),
		OpenBrowser: !nsFlag(c.ns, "no_browser"), Timeout: timeout,
		Insecure: nsFlag(c.ns, "insecure"), CABundle: authTextOr(c.ns["ca_bundle"])})
	if err != nil {
		return PooledCredential{}, err
	}
	return persist(creds, "Saved")
}

// Clears every suppression of the provider, re-adding a credential means the user wants auth back on
func (c *authCLI) unsuppressProviderSources(provider string) {
	for src := range c.env.Store.SuppressedCredentialSources(provider) {
		_, _ = c.env.Store.UnsuppressCredentialSource(provider, src)
	}
}

func (c *authCLI) addAPIKeyCredential(provider string, pool *Pool) (PooledCredential, error) {
	token := strings.TrimSpace(authTextOr(c.ns["api_key"]))
	if token == "" {
		prompted, err := authCmdMaskedSecretPromptFn("Paste your API key: ")
		if err != nil {
			return PooledCredential{}, err
		}
		token = strings.TrimSpace(prompted)
	}
	if token == "" {
		return PooledCredential{}, authExit("No API key provided.")
	}
	defaultLabel := fmt.Sprintf("api-key-%d", len(pool.Entries())+1)
	label := strings.TrimSpace(authTextOr(c.ns["label"]))
	if label == "" && authCmdStdinIsTTYFn() {
		answer, err := authCmdLineInputFn(fmt.Sprintf("Label (optional, default: %s): ", defaultLabel))
		if err != nil {
			return PooledCredential{}, err
		}
		label = strings.TrimSpace(answer)
	}
	if label == "" {
		label = defaultLabel
	}
	entry, err := pool.AddEntry(PooledCredential{
		Provider: provider, ID: authNewEntryID(), Label: label, AuthType: AuthTypeAPIKey,
		Priority: 0, Source: SourceManual, AccessToken: token, BaseURL: c.providerBaseURL(provider)})
	if err != nil {
		return PooledCredential{}, err
	}
	fmt.Fprintf(c.out, "Added %s credential #%d: \"%s\"\n", provider, len(pool.Entries()), label)
	return entry, nil
}

// icebath auth add, the plugin seam first, then the known provider gate, migration, pool add and placement
func (c *authCLI) add() error {
	provider := c.normalizeProvider(authTextOr(c.ns["provider"]))
	if owned, err := dispatchPluginAuth("add", c.pluginArgs(), provider); err != nil || owned {
		return err
	}
	configured := c.configuredProviderEntry(provider)
	if !c.isKnownProvider(provider, configured) {
		return c.unknownProviderExit(provider)
	}
	if err := pluginMissingAuthHandlerError(provider, "add"); err != nil {
		return err
	}
	if configured != nil {
		if err := c.migrateLegacyCustomPoolKey(provider, configured["pool_key"].(string)); err != nil {
			return err
		}
	}
	isCustom := strings.HasPrefix(provider, CustomPoolPrefix)
	requestedType := strings.ToLower(strings.TrimSpace(authTextOr(c.ns["auth_type"])))
	switch {
	case requestedType == "api-key":
		requestedType = AuthTypeAPIKey
	case requestedType == "":
		requestedType = AuthTypeAPIKey
		if authOAuthDefaultProviders[provider] && !isCustom {
			requestedType = AuthTypeOAuth
		}
	}
	pool := LoadPool(c.env, provider)
	if !isCustom {
		c.unsuppressProviderSources(provider)
	}
	entry, err := c.addCredential(provider, pool, requestedType)
	if err != nil {
		var authErr *AuthError
		if errors.As(err, &authErr) {
			return authExit("Login failed: " + formatAuthError(authErr))
		}
		var notSaved *CredentialNotSavedError
		if errors.As(err, &notSaved) {
			return authExit(notSaved.Error())
		}
		return err
	}
	if wanted, ok := authWantedPriority(c.ns); ok {
		placed := LoadPool(c.env, provider)
		moved, ok := placed.MoveEntry(entry.ID, wanted)
		if !ok {
			return fmt.Errorf("provider: credential %s vanished from the %s pool before placement", entry.ID, provider)
		}
		c.reportPriority(provider, placed, moved, wanted, "Placed", "at")
	}
	return nil
}

// One pool entry per login so every added account rotates on its own token pair
func (c *authCLI) addCredential(provider string, pool *Pool, requestedType string) (PooledCredential, error) {
	if requestedType == AuthTypeAPIKey {
		return c.addAPIKeyCredential(provider, pool)
	}
	if provider == "nous" {
		return c.addNousOAuthCredential(provider)
	}
	spec, ok := authOAuthAddSpecs[provider]
	if !ok {
		return PooledCredential{}, authExit(fmt.Sprintf("`icebath auth add %s` is not implemented for auth type %s yet.", provider, requestedType))
	}
	creds, err := spec.login(c)
	if err != nil {
		return PooledCredential{}, err
	}
	token := spec.token(creds)
	label := strings.TrimSpace(authTextOr(c.ns["label"]))
	if label == "" {
		label = LabelFromToken(token, fmt.Sprintf("%s-oauth-%d", provider, len(pool.Entries())+1))
	}
	authType := spec.authType
	if authType == "" {
		authType = AuthTypeOAuth
	}
	entry := PooledCredential{Provider: provider, ID: authNewEntryID(), Label: label, AuthType: authType, Priority: 0,
		Source: spec.source(creds), AccessToken: token}
	spec.fields(c, creds, provider, &entry)
	existing := pool.Entries()
	entry, err = pool.AddEntry(entry)
	if err != nil {
		return PooledCredential{}, err
	}
	if spec.activateFirst && len(existing) == 0 {
		if err := c.env.Store.MarkProviderActiveIfUnset(provider); err != nil {
			return PooledCredential{}, err
		}
	}
	fmt.Fprintf(c.out, "Added %s OAuth credential #%d: \"%s\"\n", provider, len(pool.Entries()), entry.Label)
	if provider == "openai-codex" {
		c.warnSameCodexAccount(token, existing)
	}
	return entry, nil
}

// Warns when a fresh Codex login is the same OpenAI account as a pooled credential
func (c *authCLI) warnSameCodexAccount(token string, existing []PooledCredential) {
	account, subject, ok := codexPrincipalIdentity(token)
	if !ok {
		return
	}
	for position, sibling := range existing {
		if sa, ss, sok := codexPrincipalIdentity(sibling.AccessToken); sok && sa == account && ss == subject {
			fmt.Fprintf(c.errw, "warning: this login is the same OpenAI account as openai-codex credential #%d "+
				"(\"%s\"). Both logins share one token family, so OpenAI will revoke the older one "+
				"and you gain no extra quota. Log into a different account instead, or keep just one "+
				"(`icebath auth remove openai-codex %d`).\n", position+1, sibling.Label, position+1)
			return
		}
	}
}

// Prints the effective priority and says why it differs from the request when it does
func (c *authCLI) reportPriority(provider string, pool *Pool, moved PooledCredential, requested int, verb, prep string) {
	fmt.Fprintf(c.out, "%s %s credential \"%s\" %s priority %d (#%d in `icebath auth list %s`)\n",
		verb, provider, moved.Label, prep, moved.Priority, moved.Priority+1, provider)
	size := len(pool.Entries())
	if moved.Priority != requested {
		reason := "anthropic keeps manually added credentials ahead of seeded ones"
		if requested < 0 || requested >= size {
			reason = fmt.Sprintf("the pool has %d credentials, so it was clamped", size)
		}
		fmt.Fprintf(c.errw, "note: requested priority %d; effective priority is %d because %s.\n", requested, moved.Priority, reason)
	}
	strategy := GetPoolStrategy(c.env.config(), provider)
	if strategy != StrategyFillFirst {
		fmt.Fprintf(c.errw, "note: %s uses the %s strategy; priority only orders fill_first selection.\n", provider, strategy)
	}
}

// icebath auth priority <provider> <target> <priority>, reorders one pooled credential
func (c *authCLI) priority() error {
	provider := c.normalizeProvider(authTextOr(c.ns["provider"]))
	pool := LoadPool(c.env, provider)
	target := c.ns["target"]
	index, matched, errText := pool.ResolveTarget(authTextOr(target))
	if index == 0 {
		return authExit(fmt.Sprintf("%s Provider: %s.", errText, provider))
	}
	requested, ok := authInt(c.ns["priority"])
	if !ok {
		return fmt.Errorf("provider: auth priority needs an integer priority, got %s", pyRepr(c.ns["priority"]))
	}
	moved, ok := pool.MoveEntry(matched.ID, requested)
	if !ok {
		return authExit(fmt.Sprintf("No credential matching \"%s\" for provider %s.", pyStr(target), provider))
	}
	c.reportPriority(provider, pool, moved, requested, "Set", "to")
	return nil
}
