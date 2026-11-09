package provider

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"
)

const (
	opRunTimeout         = 30 * time.Second
	opDefaultTokenEnv    = "OP_SERVICE_ACCOUNT_TOKEN"
	opDiskCacheBasename  = "op_cache.json"
	opMissingBinaryHint  = "Install the 1Password CLI (https://developer.1password.com/docs/cli/get-started/) or set secrets.onepassword.binary_path."
	opAuthRemediationTxt = "Run `icebath secrets onepassword token` to paste a fresh service-account token ({token_env}), or `op signin` for an interactive session."
)

// Minimal allowlisted child env, OP_SESSION_* and the token are added dynamically
var opEnvAllowlist = []string{
	"PATH", "HOME", "USERPROFILE", "APPDATA", "LOCALAPPDATA", "SystemRoot", "TMPDIR", "TMP", "TEMP",
	"XDG_CONFIG_HOME", "XDG_RUNTIME_DIR", "OP_CONFIG_DIR", "OP_ACCOUNT", "OP_CONNECT_HOST", "OP_CONNECT_TOKEN",
	"OP_LOAD_DESKTOP_APP_SETTINGS",
}

var opErrorRules = []SecretErrorRule{
	{SecretErrorTimeout, []string{"timed out"}},
	{SecretErrorBinaryMissing, []string{"not found on path", "not an executable", "failed to invoke"}},
	{SecretErrorAuthFailed, []string{"unauthorized", "not signed in", "session expired", "authentication", "401", "403"}},
	{SecretErrorEmptyValue, []string{"empty value"}},
	{SecretErrorNetwork, []string{"network", "connection", "resolve host", "dns"}},
}

func classifyOpError(message string) SecretErrorKind {
	return ClassifySecretCLIError(message, opErrorRules)
}

var opStore = NewSecretCache(opDiskCacheBasename)

// Keeps valid env names bound to stripped op references, the rest become warnings
func validateOpReferences(references map[string]any) (map[string]string, []string) {
	valid := map[string]string{}
	var warnings []string
	names := make([]string, 0, len(references))
	for n := range references {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		ref := references[name]
		s, isStr := ref.(string)
		switch {
		case !IsValidEnvName(name):
			warnings = append(warnings, fmt.Sprintf("Skipping %q: not a valid env-var name", name))
		case !isStr:
			warnings = append(warnings, fmt.Sprintf("Skipping %q: reference is not a string", name))
		case !strings.HasPrefix(strings.TrimSpace(s), "op://"):
			warnings = append(warnings, fmt.Sprintf("Skipping %q: %q is not an op:// secret reference", name, fmt.Sprint(ref)))
		default:
			valid[name] = strings.TrimSpace(s)
		}
	}
	return valid, warnings
}

// Fingerprint over everything op would authenticate with so a new identity never sees old cached values
func opAuthFingerprint(environ *SourceEnvironment, tokenEnv string) string {
	parts := []string{"token=" + environ.Value(tokenEnv), "account=" + environ.Value("OP_ACCOUNT"),
		"connect_host=" + environ.Value("OP_CONNECT_HOST"), "connect_token=" + environ.Value("OP_CONNECT_TOKEN")}
	snapshot := environ.Snapshot()
	keys := make([]string, 0)
	for k := range snapshot {
		if strings.HasPrefix(k, "OP_SESSION_") {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		parts = append(parts, k+"="+snapshot[k])
	}
	return SecretFingerprint(strings.Join(parts, "\n"))
}

func opRefsFingerprint(references map[string]string) string {
	names := make([]string, 0, len(references))
	for n := range references {
		names = append(names, n)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, n := range names {
		parts = append(parts, n+"="+references[n])
	}
	return SecretFingerprint(strings.Join(parts, "\n"))
}

// FindOp resolves a usable op binary, a pinned path is used verbatim and never falls back to PATH
func FindOp(binaryPath string) string {
	if binaryPath != "" {
		info, err := os.Stat(binaryPath)
		if err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
			return ""
		}
		return binaryPath
	}
	found, err := exec.LookPath("op")
	if err != nil {
		return ""
	}
	return found
}

func opScrub(text string) string {
	return strings.TrimSpace(strings.ReplaceAll(ScrubANSI(text), "\x1b", ""))
}

func opChildEnv(environ *SourceEnvironment, tokenValue string) map[string]string {
	env := map[string]string{}
	snapshot := environ.Snapshot()
	for _, k := range opEnvAllowlist {
		if v, ok := snapshot[k]; ok {
			env[k] = v
		}
	}
	for k, v := range snapshot {
		if strings.HasPrefix(k, "OP_SESSION_") {
			env[k] = v
		}
	}
	if tokenValue != "" {
		env["OP_SERVICE_ACCOUNT_TOKEN"] = tokenValue
	}
	env["NO_COLOR"] = "1"
	return env
}

// Resolves one op reference, any failure including an exit 0 empty value is an error
func runOpRead(op, reference, account, tokenValue string, environ *SourceEnvironment) (string, error) {
	cmd := []string{op, "read"}
	if account != "" {
		cmd = append(cmd, "--account", account)
	}
	cmd = append(cmd, "--", reference)
	proc, err := RunCLI(cmd, opChildEnv(environ, tokenValue), opRunTimeout, "op", fmt.Sprintf("op read timed out after %.0fs for %q", opRunTimeout.Seconds(), reference), os.Stdin)
	if err != nil {
		return "", err
	}
	if proc.ReturnCode != 0 {
		msg := opScrub(proc.Stderr)
		if len(msg) > 200 {
			msg = msg[:200]
		}
		if msg != "" {
			return "", fmt.Errorf("op read failed for %q: %s", reference, msg)
		}
		return "", fmt.Errorf("op read exited %d for %q", proc.ReturnCode, reference)
	}
	value := strings.TrimRight(proc.Stdout, "\r\n")
	if strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("op read returned an empty value for %q", reference)
	}
	return value, nil
}

// OnePasswordFetchOptions parameterize one resolution of op references
type OnePasswordFetchOptions struct {
	References map[string]any
	Account    string
	TokenEnv   string
	Binary     string
	BinaryPath string
	UseCache   bool
	CacheTTL   float64
	HomePath   string
	Environ    *SourceEnvironment
}

// FetchOnePasswordSecrets resolves references to values, only a complete pull is cached
func FetchOnePasswordSecrets(opts OnePasswordFetchOptions) (map[string]string, []string, error) {
	valid, warnings := validateOpReferences(opts.References)
	if len(valid) == 0 {
		return map[string]string{}, warnings, nil
	}
	environ := opts.Environ
	if environ == nil {
		environ = ProcessSourceEnvironment()
	}
	tokenEnv := opts.TokenEnv
	if tokenEnv == "" {
		tokenEnv = opDefaultTokenEnv
	}
	tokenValue := strings.TrimSpace(environ.Value(tokenEnv))
	cacheKey := fmt.Sprintf("%s|%s|%s", opAuthFingerprint(environ, tokenEnv), opts.Account, opRefsFingerprint(valid))
	if opts.UseCache {
		cached, ok, err := opStore.Lookup(cacheKey, opts.CacheTTL, opts.HomePath, nil)
		if err != nil {
			return nil, nil, err
		}
		if ok {
			out := map[string]string{}
			for k, v := range cached.Secrets {
				out[k] = v
			}
			return out, warnings, nil
		}
	}
	op := opts.Binary
	if op == "" {
		op = FindOp(opts.BinaryPath)
	}
	if op == "" {
		return nil, warnings, errors.New("op CLI not found.  Install the 1Password CLI (https://developer.1password.com/docs/cli/get-started/) or set secrets.onepassword.binary_path to its absolute location.")
	}
	secrets := map[string]string{}
	readErrors := 0
	names := make([]string, 0, len(valid))
	for n := range valid {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		value, err := runOpRead(op, valid[name], opts.Account, tokenValue, environ)
		if err != nil {
			warnings = append(warnings, err.Error())
			readErrors++
			continue
		}
		secrets[name] = value
	}
	if opts.UseCache && readErrors == 0 && len(secrets) > 0 {
		copyOf := map[string]string{}
		for k, v := range secrets {
			copyOf[k] = v
		}
		opStore.Store(cacheKey, CachedFetch{Secrets: copyOf, FetchedAt: nowEpoch()}, opts.CacheTTL, opts.HomePath)
	}
	return secrets, warnings, nil
}

func opMissingBinaryError(binaryPath string) string {
	if binaryPath != "" {
		return fmt.Sprintf("secrets.onepassword.binary_path (%q) is not an executable op binary.", binaryPath)
	}
	return "secrets.onepassword.enabled is true but the op CLI was not found on PATH.  Install it (https://developer.1password.com/docs/cli/get-started/) or set secrets.onepassword.binary_path."
}

// OnePasswordApplyOptions parameterize a sync apply onto the process environment
type OnePasswordApplyOptions struct {
	Enabled                bool
	Env                    map[string]any
	Account                string
	ServiceAccountTokenEnv string
	BinaryPath             string
	OverrideExisting       bool
	CacheTTL               float64
	HomePath               string
}

// ApplyOnePasswordSecrets resolves configured references and sets them on the process environment
func ApplyOnePasswordSecrets(opts OnePasswordApplyOptions) *SecretFetchResult {
	result := NewSecretFetchResult()
	if !opts.Enabled {
		return result
	}
	tokenEnv := opts.ServiceAccountTokenEnv
	if tokenEnv == "" {
		tokenEnv = opDefaultTokenEnv
	}
	valid, warnings := validateOpReferences(opts.Env)
	result.Warnings = append(result.Warnings, warnings...)
	guarded := func(name string) bool {
		return name == tokenEnv || (!opts.OverrideExisting && os.Getenv(name) != "")
	}
	toFetch := map[string]any{}
	names := make([]string, 0, len(valid))
	for n := range valid {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if guarded(n) {
			result.Skipped = append(result.Skipped, n)
			continue
		}
		toFetch[n] = valid[n]
	}
	if len(toFetch) == 0 {
		return result
	}
	binary := FindOp(opts.BinaryPath)
	result.BinaryPath = binary
	if binary == "" {
		result.Error = opMissingBinaryError(opts.BinaryPath)
		result.failed = true
		return result
	}
	secrets, fetchWarnings, err := FetchOnePasswordSecrets(OnePasswordFetchOptions{References: toFetch, Account: opts.Account, TokenEnv: tokenEnv,
		Binary: binary, UseCache: true, CacheTTL: opts.CacheTTL, HomePath: opts.HomePath})
	if err != nil {
		result.Error = err.Error()
		result.failed = true
		return result
	}
	result.Secrets = secrets
	result.Warnings = append(result.Warnings, fetchWarnings...)
	applied := make([]string, 0, len(secrets))
	for n := range secrets {
		applied = append(applied, n)
	}
	sort.Strings(applied)
	for _, name := range applied {
		if guarded(name) {
			if !containsString(result.Skipped, name) {
				result.Skipped = append(result.Skipped, name)
			}
			continue
		}
		_ = os.Setenv(name, secrets[name])
		result.Applied = append(result.Applied, name)
	}
	return result
}

// OnePasswordSource is 1Password as a mapped source
type OnePasswordSource struct{}

func (s *OnePasswordSource) Spec() SecretSourceSpec {
	return SecretSourceSpec{APIVersion: SecretSourceAPIVersion, Name: "onepassword", Label: "1Password", Shape: "mapped", Scheme: "op",
		TokenEnvKey: "service_account_token_env", DefaultTokenEnv: opDefaultTokenEnv, OverrideExistingDefault: true,
		RemediationHints: map[SecretErrorKind]string{SecretErrorAuthFailed: opAuthRemediationTxt, SecretErrorAuthExpired: opAuthRemediationTxt, SecretErrorBinaryMissing: opMissingBinaryHint}}
}

func (s *OnePasswordSource) ConfigSchema() map[string]map[string]any {
	return map[string]map[string]any{
		"enabled":                   {"description": "Master switch", "default": false},
		"env":                       {"description": "Map of ENV_VAR -> op://vault/item/field reference", "default": map[string]any{}},
		"account":                   {"description": "op --account shorthand (empty = default account)", "default": ""},
		"service_account_token_env": {"description": "Env var holding the service-account token (unset = desktop/interactive session)", "default": opDefaultTokenEnv},
		"binary_path":               {"description": "Pin the op binary (empty = resolve via PATH)", "default": ""},
		"cache_ttl_seconds":         {"description": "Disk+memory cache TTL; 0 disables", "default": 300},
		"override_existing":         {"description": "Resolved values overwrite .env/shell values", "default": true},
	}
}

func (s *OnePasswordSource) Fetch(cfg map[string]any, homePath string, environ *SourceEnvironment) *SecretFetchResult {
	result := NewSecretFetchResult()
	envMap, _ := cfg["env"].(map[string]any)
	valid, warnings := validateOpReferences(envMap)
	result.Warnings = append(result.Warnings, warnings...)
	if len(valid) == 0 {
		if len(warnings) == 0 {
			result.Fail("secrets.onepassword.enabled is true but the env: map is empty.  Add ENV_VAR: op://vault/item/field entries.", SecretErrorNotConfigured)
		}
		return result
	}
	binaryPath := anyString(cfg["binary_path"])
	binary := findOp(binaryPath)
	result.BinaryPath = binary
	if binary == "" {
		return result.Fail(opMissingBinaryError(binaryPath), SecretErrorBinaryMissing)
	}
	references := map[string]any{}
	for k, v := range valid {
		references[k] = v
	}
	ttl := CoerceFloat(cfg["cache_ttl_seconds"], 300)
	if _, ok := cfg["cache_ttl_seconds"]; !ok {
		ttl = 300
	}
	secrets, fetchWarnings, err := fetchOnePasswordSecrets(OnePasswordFetchOptions{References: references, Account: anyString(cfg["account"]),
		TokenEnv: SecretSourceTokenEnv(s.Spec(), cfg), Binary: binary, UseCache: true, CacheTTL: ttl, HomePath: homePath, Environ: environ})
	if err != nil {
		return result.Fail(err.Error(), classifyOpError(err.Error()))
	}
	result.Secrets = secrets
	result.Warnings = append(result.Warnings, fetchWarnings...)
	return result
}

// ClearOnePasswordCaches drops the in process and disk caches after a token rotation
func ClearOnePasswordCaches(homePath string) { opStore.Clear(homePath) }
