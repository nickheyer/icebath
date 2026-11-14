package secret

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/fileutil"
	"github.com/nickheyer/icebath/internal/home"
)

// Bound on one op read and the variable op itself reads its service account token from
const (
	opRunTimeout       = 30 * time.Second
	opDefaultTokenEnv  = "OP_SERVICE_ACCOUNT_TOKEN"
	opDefaultTTL       = 300.0
	opMissingBinaryFix = "Install the 1Password CLI (https://developer.1password.com/docs/cli/get-started/) or set secrets.onepassword.binary_path."
	opAuthFix          = "Run `icebath secrets onepassword token` to paste a fresh service-account token ({token_env}), or `op signin` for an interactive session."
)

// Variables an op child may see, the session and token variables added per run
var opEnvAllowlist = []string{
	"PATH", "HOME", "USERPROFILE", "APPDATA", "LOCALAPPDATA", "SystemRoot",
	"TMPDIR", "TMP", "TEMP", "XDG_CONFIG_HOME", "XDG_RUNTIME_DIR", "OP_CONFIG_DIR",
	"OP_ACCOUNT", "OP_CONNECT_HOST", "OP_CONNECT_TOKEN", "OP_LOAD_DESKTOP_APP_SETTINGS",
}

// Failure classes of op messages, the first matching rule winning
var opErrorRules = []errorRule{
	{Timeout, []string{"timed out"}},
	{BinaryMissing, []string{"not found on path", "not an executable", "failed to invoke"}},
	{AuthFailed, []string{"unauthorized", "not signed in", "session expired", "authentication", "401", "403"}},
	{EmptyValue, []string{"empty value"}},
	{Network, []string{"network", "connection", "resolve host", "dns"}},
}

// 1Password through the op program, one read per mapped reference
type OnePasswordSource struct {
	cache *secretCache
}

// Source with an empty cache
func NewOnePasswordSource() *OnePasswordSource {
	return &OnePasswordSource{cache: newSecretCache(home.OnePasswordCache)}
}

// Valid bindings of variable names to op references with a warning for each binding left out
func validateReferences(bindings []binding) (map[string]string, []string) {
	valid := map[string]string{}
	var warnings []string
	for _, b := range bindings {
		ref := strings.TrimSpace(b.value)
		switch {
		case !ValidEnvName(b.key):
			warnings = append(warnings, fmt.Sprintf("Skipping '%s': not a valid env-var name", b.key))
		case !b.isString:
			warnings = append(warnings, fmt.Sprintf("Skipping '%s': reference is not a string", b.key))
		case !strings.HasPrefix(ref, "op://"):
			warnings = append(warnings, fmt.Sprintf("Skipping '%s': '%s' is not an op:// secret reference", b.key, b.value))
		default:
			valid[b.key] = ref
		}
	}
	return valid, warnings
}

// Fingerprint over everything op would authenticate with, so another identity never reads cached values
func opAuthFingerprint(env map[string]string, tokenEnv string) string {
	parts := []string{
		"token=" + env[tokenEnv], "account=" + env["OP_ACCOUNT"],
		"connect_host=" + env["OP_CONNECT_HOST"], "connect_token=" + env["OP_CONNECT_TOKEN"],
	}
	for _, name := range sortedKeys(env) {
		if strings.HasPrefix(name, "OP_SESSION_") {
			parts = append(parts, name+"="+env[name])
		}
	}
	return fingerprint(strings.Join(parts, "\n"))
}

// Keys of a string map in sorted order
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Fingerprint of the reference bindings
func opRefsFingerprint(refs map[string]string) string {
	lines := make([]string, 0, len(refs))
	for _, name := range sortedKeys(refs) {
		lines = append(lines, name+"="+refs[name])
	}
	return fingerprint(strings.Join(lines, "\n"))
}

// Usable op program, a pinned path used verbatim and never replaced by the search path, empty when there is none
func FindOp(binaryPath string) string {
	if binaryPath != "" {
		if !fileutil.Accessible(binaryPath, fileutil.AccessExec) {
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

// Environment of an op child from the fetch view, the token normalised to the name op reads
func opChildEnv(env map[string]string, token string) []string {
	child := map[string]string{}
	for _, name := range opEnvAllowlist {
		if v, ok := env[name]; ok {
			child[name] = v
		}
	}
	for name, value := range env {
		if strings.HasPrefix(name, "OP_SESSION_") {
			child[name] = value
		}
	}
	if token != "" {
		child[opDefaultTokenEnv] = token
	}
	child["NO_COLOR"] = "1"
	return envList(child)
}

// Resolves one reference, failing on any error and on an empty value that would wipe a good credential
func runOpRead(ctx context.Context, op, reference, account string, env []string) (string, error) {
	argv := []string{op, "read"}
	if account != "" {
		argv = append(argv, "--account", account)
	}
	argv = append(argv, "--", reference)
	res, err := runCLI(ctx, argv, env, opRunTimeout, "op",
		fmt.Sprintf("op read timed out after %.0fs for '%s'", opRunTimeout.Seconds(), reference), os.Stdin)
	if err != nil {
		return "", err
	}
	if res.Code != 0 {
		detail := scrubTerminal(res.Stderr)
		if runes := []rune(detail); len(runes) > 200 {
			detail = string(runes[:200])
		}
		if detail != "" {
			return "", fmt.Errorf("op read failed for '%s': %s", reference, detail)
		}
		return "", fmt.Errorf("op read exited %d for '%s'", res.Code, reference)
	}
	value := strings.TrimRight(res.Stdout, "\r\n")
	if strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("op read returned an empty value for '%s'", reference)
	}
	return value, nil
}

// What one 1Password pull reads
type OnePasswordFetch struct {
	// Variable names bound to op references, already validated
	References map[string]string
	Account    string
	// Variable holding the service account token, the op default when empty
	TokenEnv string
	// op program to run
	Binary string
	// Seconds a complete pull stays cached, zero disabling both cache layers
	CacheTTL float64
	Home     string
	// Environment view auth material is read from
	Env map[string]string
}

// Error for a pull attempted with no op program
var ErrNoOp = errors.New("op CLI not found.  Install the 1Password CLI (https://developer.1password.com/docs/cli/get-started/) or set secrets.onepassword.binary_path to its absolute location.")

// Resolves the references to values with a warning per failed read, a complete pull cached, ErrNoOp without a program
func (s *OnePasswordSource) FetchSecrets(ctx context.Context, in OnePasswordFetch) (map[string]string, []string, error) {
	if len(in.References) == 0 {
		return map[string]string{}, nil, nil
	}
	tokenEnv := in.TokenEnv
	if tokenEnv == "" {
		tokenEnv = opDefaultTokenEnv
	}
	token := strings.TrimSpace(in.Env[tokenEnv])
	auth, refs := opAuthFingerprint(in.Env, tokenEnv), opRefsFingerprint(in.References)
	memoryKey := strings.Join([]string{auth, in.Account, in.Home, refs}, "|")
	diskKey := strings.Join([]string{auth, in.Account, refs}, "|")
	if cached, ok := s.cache.lookup(in.Home, memoryKey, diskKey, in.CacheTTL, nil); ok {
		out := make(map[string]string, len(cached.secrets))
		for name, value := range cached.secrets {
			out[name] = value
		}
		return out, nil, nil
	}
	if in.Binary == "" {
		return nil, nil, ErrNoOp
	}
	env := opChildEnv(in.Env, token)
	secrets := map[string]string{}
	var warnings []string
	failed := 0
	for _, name := range sortedKeys(in.References) {
		value, err := runOpRead(ctx, in.Binary, in.References[name], in.Account, env)
		if err != nil {
			if ctx.Err() != nil {
				return nil, nil, ctx.Err()
			}
			warnings = append(warnings, err.Error())
			failed++
			continue
		}
		secrets[name] = value
	}
	if failed == 0 && len(secrets) > 0 {
		kept := make(map[string]string, len(secrets))
		for name, value := range secrets {
			kept[name] = value
		}
		s.cache.store(ctx, in.Home, memoryKey, diskKey, cachedFetch{secrets: kept, fetchedAt: nowSeconds()}, in.CacheTTL)
	}
	return secrets, warnings, nil
}

// Message for a section whose op program cannot be found
func opMissingBinaryError(binaryPath string) string {
	if binaryPath != "" {
		return fmt.Sprintf("secrets.onepassword.binary_path ('%s') is not an executable op binary.", binaryPath)
	}
	return "secrets.onepassword.enabled is true but the op CLI was not found on PATH.  Install it " +
		"(https://developer.1password.com/docs/cli/get-started/) or set secrets.onepassword.binary_path."
}

// Drops the memory and disk caches of a home, for after a token rotation
func (s *OnePasswordSource) ClearCaches(homeDir string) { s.cache.clear(homeDir) }

// Section key and provenance label
func (*OnePasswordSource) Name() string { return "onepassword" }

// Words status lines use
func (*OnePasswordSource) Label() string { return "1Password" }

// Explicit bindings, outranking bulk sources
func (*OnePasswordSource) Shape() Shape { return Mapped }

// Prefix of op references
func (*OnePasswordSource) Scheme() string { return "op" }

// Reports the master switch
func (*OnePasswordSource) Enabled(cfg config.Raw) bool {
	return sectionOf(cfg).truthy("enabled", false)
}

// An explicit binding beats a stale env file line unless told otherwise
func (*OnePasswordSource) Override(cfg config.Raw) bool {
	return sectionOf(cfg).truthy("override_existing", true)
}

// Variable holding the service account token
func (*OnePasswordSource) TokenEnv(cfg config.Raw) string {
	if name := sectionOf(cfg).text("service_account_token_env"); name != "" {
		return name
	}
	return opDefaultTokenEnv
}

// The token variable, which no source may overwrite
func (s *OnePasswordSource) Protected(cfg config.Raw) []string { return []string{s.TokenEnv(cfg)} }

// Budget of the whole fetch
func (*OnePasswordSource) Timeout(cfg config.Raw) time.Duration { return fetchTimeout(cfg) }

// Next step for a failed fetch
func (s *OnePasswordSource) Remediation(kind ErrorKind, cfg config.Raw) string {
	return remediationFor(kind, map[ErrorKind]string{AuthFailed: opAuthFix, AuthExpired: opAuthFix, BinaryMissing: opMissingBinaryFix},
		"onepassword", s.TokenEnv(cfg))
}

// Resolves every mapped reference of the section
func (s *OnePasswordSource) Fetch(ctx context.Context, in FetchInput) Result {
	cfg := sectionOf(in.Config)
	var result Result
	valid, warnings := validateReferences(cfg.bindings("env"))
	result.Warnings = append(result.Warnings, warnings...)
	if len(valid) == 0 {
		if len(warnings) == 0 {
			return result.fail("secrets.onepassword.enabled is true but the env: map is empty.  Add ENV_VAR: op://vault/item/field entries.", NotConfigured)
		}
		return result
	}
	binaryPath := cfg.text("binary_path")
	binary := FindOp(binaryPath)
	result.Binary = binary
	if binary == "" {
		return result.fail(opMissingBinaryError(binaryPath), BinaryMissing)
	}
	secrets, fetchWarnings, err := s.FetchSecrets(ctx, OnePasswordFetch{
		References: valid, Account: cfg.text("account"), TokenEnv: s.TokenEnv(in.Config), Binary: binary,
		CacheTTL: cfg.float("cache_ttl_seconds", opDefaultTTL), Home: in.Home, Env: in.Env,
	})
	if err != nil {
		return result.fail(err.Error(), classify(err.Error(), opErrorRules))
	}
	result.Secrets = secrets
	result.Warnings = append(result.Warnings, fetchWarnings...)
	return result
}

// How a sync of 1Password references into the running process behaves
type OnePasswordSync struct {
	// Variable names bound to op references as configured
	References map[string]string
	Account    string
	TokenEnv   string
	BinaryPath string
	// Lets resolved values replace variables the process already has
	OverrideExisting bool
	CacheTTL         float64
	Home             string
}

// What a sync did
type OnePasswordSyncResult struct {
	Secrets  map[string]string
	Applied  []string
	Skipped  []string
	Warnings []string
	Err      string
	Binary   string
}

// Resolves the references and sets them on the process, the token variable and satisfied names skipped before any read
func (s *OnePasswordSource) SyncProcess(ctx context.Context, o OnePasswordSync) OnePasswordSyncResult {
	var result OnePasswordSyncResult
	tokenEnv := o.TokenEnv
	if tokenEnv == "" {
		tokenEnv = opDefaultTokenEnv
	}
	var bindings []binding
	for _, name := range sortedKeys(o.References) {
		bindings = append(bindings, binding{key: name, value: o.References[name], isString: true})
	}
	valid, warnings := validateReferences(bindings)
	result.Warnings = append(result.Warnings, warnings...)
	guarded := func(name string) bool {
		return name == tokenEnv || (!o.OverrideExisting && os.Getenv(name) != "")
	}
	toFetch := map[string]string{}
	for _, name := range sortedKeys(valid) {
		if guarded(name) {
			result.Skipped = append(result.Skipped, name)
			continue
		}
		toFetch[name] = valid[name]
	}
	if len(toFetch) == 0 {
		return result
	}
	result.Binary = FindOp(o.BinaryPath)
	if result.Binary == "" {
		result.Err = opMissingBinaryError(o.BinaryPath)
		return result
	}
	secrets, fetchWarnings, err := s.FetchSecrets(ctx, OnePasswordFetch{
		References: toFetch, Account: o.Account, TokenEnv: tokenEnv, Binary: result.Binary,
		CacheTTL: o.CacheTTL, Home: o.Home, Env: envMap(os.Environ()),
	})
	if err != nil {
		result.Err = err.Error()
		return result
	}
	result.Secrets = secrets
	result.Warnings = append(result.Warnings, fetchWarnings...)
	for _, name := range sortedKeys(secrets) {
		if guarded(name) {
			result.Skipped = append(result.Skipped, name)
			continue
		}
		if err := os.Setenv(name, secrets[name]); err != nil {
			result.Warnings = append(result.Warnings, fmt.Sprintf("could not set %s: %v", name, err))
			continue
		}
		result.Applied = append(result.Applied, name)
	}
	return result
}
