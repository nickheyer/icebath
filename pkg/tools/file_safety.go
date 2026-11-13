package tools

import (
	"context"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Secret value caches and the sidecar token, as home relative POSIX paths
var secretCacheStores = []string{
	"bws_cache.json", "bws_cache.enc.json", "op_cache.json", "photon-sidecar.json",
	"cache/bws_cache.json", "cache/bws_cache.enc.json", "cache/op_cache.json", "runtime/photon-sidecar.json",
}

// Home relative credential directories never written by the file tools
var homeCredentialDirs = []string{".ssh", ".aws", ".gnupg", ".kube", ".docker", ".azure", ".config/gh", ".config/gcloud"}

// Home subpaths the generic file tools must never rewrite
var protectedHomeSubpaths = []string{"state.db", "sessions", "mcp-tokens", "pairing", "vault", "browser-profile", "webapp"}

// Secret bearing project local env file basenames blocked anywhere on disk
var blockedProjectEnvBasenames = setOf(".env", ".env.local", ".env.development", ".env.production", ".env.test", ".env.staging", ".envrc")

const defenseInDepthSuffix = " (Defense-in-depth — not a security boundary; the terminal tool can still bypass.)"

var credentialFileNames = append([]string{
	"auth.json", "auth.lock", ".anthropic_oauth.json", ".env", "webhook_subscriptions.json",
	filepath.Join("auth", "google_oauth.json"),
}, secretCacheStores...)

type readDeniedDir struct {
	subdir  string
	dirMsg  string
	fileMsg string
}

var readDeniedDirs = []readDeniedDir{
	{"mcp-tokens",
		"is the icebath MCP token directory and cannot be read directly.",
		"is an icebath MCP token file and cannot be read directly."},
	{"browser-profile",
		"is the icebath real-profile browser snapshot directory (copied cookies/logins) and cannot be read directly.",
		"is inside the icebath real-profile browser snapshot (copied cookies/logins) and cannot be read directly."},
	{"vault",
		"is the icebath credential vault directory and cannot be read directly (secrets are filled server-side by browser_vault).",
		"is inside the icebath credential vault (encrypted secrets + local key) and cannot be read directly (browser_vault resolves them server-side)."},
	{"webapp",
		"is the icebath Webapp sign-in directory and cannot be read directly.",
		"is an icebath Webapp sign-in file (the browser credential or its sign-in records) and cannot be read directly."},
}

// Home directory of the active profile
func envHome(env *Env) string {
	if env != nil && strings.TrimSpace(env.Home) != "" {
		return env.Home
	}
	return home.Process()
}

// Resolved active home and the root it lives under, deduplicated
func icebathDirs(env *Env) []string {
	h := envHome(env)
	out := []string{resolveNonStrict(h)}
	if root := home.RootOf(h); root != "" {
		r := resolveNonStrict(root)
		if r != out[0] {
			out = append(out, r)
		}
	}
	return out
}

// IcebathDirs exports the resolved active home and root pair for the context reference guards
func IcebathDirs(env *Env) []string { return icebathDirs(env) }

// HomeCredentialDirs exports the credential directories under the user home the read guards refuse
func HomeCredentialDirs() []string { return append([]string(nil), homeCredentialDirs...) }

// Reports whether a raw path uses the Windows NT or device namespace
func isNTNamespacePath(path string) bool {
	s := strings.ReplaceAll(path, "/", `\`)
	if strings.HasPrefix(s, `\??\`) || strings.HasPrefix(s, `\\.\`) {
		return true
	}
	if strings.HasPrefix(s, `\\?\`) {
		upper := strings.ToUpper(s[4:])
		return strings.HasPrefix(upper, `UNC\`) || strings.HasPrefix(upper, `GLOBALROOT\`)
	}
	return false
}

// IsNTNamespacePath exports the raw Windows namespace check for callers that must never resolve the path
func IsNTNamespacePath(path string) bool { return isNTNamespacePath(path) }

// Error for an NT or device namespace path, empty otherwise
func ntNamespaceError(path, verb string) string {
	if !isNTNamespacePath(path) {
		return ""
	}
	return verb + " denied: '" + path + "' uses a Windows NT/device namespace prefix (\\??\\, \\\\.\\, \\\\?\\UNC\\, or GLOBALROOT). " +
		"These paths bypass normal path normalization and can trigger outbound SMB authentication (NTLM credential leak) merely by being resolved. " +
		"Use a normal absolute path instead."
}

// Every home the write guards must cover, realpathed
func guardHomes(env *Env, path string) []string {
	homes := map[string]bool{}
	if h := userHomeDir(); h != "" {
		homes[h] = true
	}
	if profileHome := filepath.Join(envHome(env), "home"); isDir(profileHome) {
		homes[profileHome] = true
	}
	if len(path) > 1 && path[0] == '~' && path[1] != '/' && path[1] != '\\' {
		name := strings.FieldsFunc(path[1:], func(r rune) bool { return r == '/' || r == '\\' })[0]
		if u, err := user.Lookup(name); err == nil && u.HomeDir != "" {
			homes[u.HomeDir] = true
		}
	}
	out := make([]string, 0, len(homes))
	for h := range homes {
		out = append(out, resolveNonStrict(h))
	}
	sort.Strings(out)
	return out
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

func isRegularFile(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode().IsRegular()
}

// Exact sensitive paths that must never be written for one home
func buildWriteDeniedPaths(env *Env, h string) map[string]bool {
	paths := []string{
		filepath.Join(h, ".ssh", "authorized_keys"), filepath.Join(h, ".ssh", "id_rsa"), filepath.Join(h, ".ssh", "id_ed25519"),
		filepath.Join(h, ".netrc"), filepath.Join(h, ".pgpass"), filepath.Join(h, ".npmrc"), filepath.Join(h, ".pypirc"),
		filepath.Join(h, ".git-credentials"),
		"/etc/sudoers", "/etc/passwd", "/etc/shadow",
	}
	for _, base := range icebathDirs(env) {
		for _, f := range append([]string{".env", ".anthropic_oauth.json", filepath.Join("auth", "google_oauth.json")}, secretCacheStores...) {
			paths = append(paths, filepath.Join(base, filepath.FromSlash(f)))
		}
	}
	out := map[string]bool{}
	for _, p := range paths {
		out[resolveNonStrict(p)] = true
	}
	return out
}

// Sensitive directory prefixes that must never be written for one home
func buildWriteDeniedPrefixes(h string) []string {
	var paths []string
	for _, d := range homeCredentialDirs {
		paths = append(paths, filepath.Join(h, filepath.FromSlash(d)))
	}
	paths = append(paths, "/etc/sudoers.d", "/etc/systemd")
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		out = append(out, resolveNonStrict(p)+string(filepath.Separator))
	}
	return out
}

// Paths needing human approval to write for one home
func buildWriteApprovalPaths(h string) map[string]bool {
	return map[string]bool{resolveNonStrict(filepath.Join(h, ".ssh", "config")): true}
}

// Resolved ICEBATH_WRITE_SAFE_ROOT entries
func safeWriteRoots(env *Env) []string {
	raw := os.Getenv("ICEBATH_WRITE_SAFE_ROOT")
	if raw == "" && env != nil && env.Secrets != nil {
		raw, _ = env.Secrets.Get("ICEBATH_WRITE_SAFE_ROOT")
	}
	var roots []string
	for _, p := range strings.Split(raw, string(os.PathListSeparator)) {
		if p != "" {
			roots = append(roots, realPath(p))
		}
	}
	sort.Strings(roots)
	return roots
}

var protectedRuntimeOnce sync.Once
var protectedRuntimePaths [][2]string

// Resolved paths of the running binary and its directory with their descriptions
func protectedRuntime() [][2]string {
	protectedRuntimeOnce.Do(func() {
		exe, err := os.Executable()
		if err != nil || exe == "" {
			return
		}
		real := resolveNonStrict(exe)
		protectedRuntimePaths = append(protectedRuntimePaths, [2]string{real, "the icebath binary this runtime is running from"})
	})
	return protectedRuntimePaths
}

// Description of the protected runtime path touched, empty when none, follow false keeps the entry
func isProtectedPath(path string, follow bool) string {
	var resolved string
	if follow {
		resolved = realPath(path)
	} else {
		parent, leaf := splitEntry(expandTilde(path))
		if parent == "" {
			parent = "."
		}
		resolved = filepath.Join(resolveNonStrict(parent), leaf)
	}
	if resolved == "" {
		return ""
	}
	for _, entry := range protectedRuntime() {
		if resolved == entry[0] || isUnder(resolved, entry[0]) || isUnder(entry[0], resolved) {
			return entry[1]
		}
	}
	return ""
}

// Credential, safe root or namespace verdict for an already resolved path
func classifyResolvedWriteDenial(env *Env, homes []string, resolved string) string {
	for _, h := range homes {
		if buildWriteApprovalPaths(h)[resolved] {
			return ""
		}
	}
	for _, h := range homes {
		if buildWriteDeniedPaths(env, h)[resolved] {
			return "credential"
		}
		for _, prefix := range buildWriteDeniedPrefixes(h) {
			if strings.HasPrefix(resolved, prefix) {
				return "credential"
			}
		}
	}
	for _, base := range icebathDirs(env) {
		for _, sub := range protectedHomeSubpaths {
			if isUnder(resolved, resolveNonStrict(filepath.Join(base, sub))) {
				return "credential"
			}
		}
	}
	roots := safeWriteRoots(env)
	if len(roots) > 0 {
		inside := false
		for _, root := range roots {
			if isUnder(resolved, root) {
				inside = true
				break
			}
		}
		if !inside {
			return "safe_root"
		}
	}
	return ""
}

// Returns credential, safe_root, nt_namespace or empty when writes are allowed
func classifyWriteDenial(env *Env, path string, entry bool) string {
	if isNTNamespacePath(path) {
		return "nt_namespace"
	}
	homes := guardHomes(env, path)
	resolved := realPath(path)
	if isProtectedPath(path, true) != "" || (entry && isProtectedPath(path, false) != "") {
		return "credential"
	}
	denial := classifyResolvedWriteDenial(env, homes, resolved)
	if denial != "" || !entry {
		return denial
	}
	parent, leaf := splitEntry(expandTilde(path))
	if parent == "" {
		parent = "."
	}
	return classifyResolvedWriteDenial(env, homes, filepath.Join(resolveNonStrict(parent), leaf))
}

// IsWriteDenied reports a path on the protected credential and system write denylist
func IsWriteDenied(env *Env, path string) bool { return classifyWriteDenial(env, path, false) != "" }

// Model facing error when writes to the path are blocked, empty otherwise
func writeDeniedError(env *Env, path, verb string, entry bool) string {
	denial := classifyWriteDenial(env, path, entry)
	switch denial {
	case "":
		return ""
	case "safe_root":
		return verb + " denied: '" + path + "' is outside ICEBATH_WRITE_SAFE_ROOT (" +
			strings.Join(safeWriteRoots(env), string(os.PathListSeparator)) + "). Unset the variable or add this path's directory prefix."
	case "nt_namespace":
		return ntNamespaceError(path, verb)
	}
	return verb + " denied: '" + path + "' is a protected system/credential file."
}

// Model facing write block error for the verb, empty when the path may be written
func WriteDeniedError(env *Env, path, verb string, entry bool) string {
	return writeDeniedError(env, path, verb, entry)
}

// NT or device namespace refusal for the verb, empty for an ordinary path
func NTNamespaceError(path, verb string) string { return ntNamespaceError(path, verb) }

// Reports whether the path is approval gated like the SSH client config
func isWriteApprovalRequired(env *Env, path string) bool {
	resolved := realPath(path)
	for _, h := range guardHomes(env, path) {
		if buildWriteApprovalPaths(h)[resolved] {
			return true
		}
	}
	return false
}

// IsWriteApprovalRequired reports a path whose write needs explicit approval
func IsWriteApprovalRequired(env *Env, path string) bool { return isWriteApprovalRequired(env, path) }

// Error when a read targets a denied icebath path, empty otherwise
func readBlockError(env *Env, path string) string {
	if e := ntNamespaceError(path, "Read"); e != "" {
		return e
	}
	resolved := realPath(path)
	dirs := icebathDirs(env)
	reason := ""
	for _, hd := range dirs {
		if isUnder(resolved, filepath.Join(hd, "skills", ".hub")) {
			reason = "is an internal icebath cache file and cannot be read directly to prevent prompt injection. Use the skills_list or skill_view tools instead."
			break
		}
	}
	if reason == "" {
	names:
		for _, name := range credentialFileNames {
			for _, hd := range dirs {
				if resolved == resolveNonStrict(filepath.Join(hd, name)) {
					reason = "is an icebath credential store and cannot be read directly. Provider tools consume these credentials through internal channels." + defenseInDepthSuffix
					break names
				}
			}
		}
	}
	if reason == "" {
	dirsLoop:
		for _, dd := range readDeniedDirs {
			for _, hd := range dirs {
				blocked := resolveNonStrict(filepath.Join(hd, dd.subdir))
				if isUnder(resolved, blocked) {
					if resolved == blocked {
						reason = dd.dirMsg + defenseInDepthSuffix
					} else {
						reason = dd.fileMsg + defenseInDepthSuffix
					}
					break dirsLoop
				}
			}
		}
	}
	if reason == "" && blockedProjectEnvBasenames[strings.ToLower(filepath.Base(resolved))] {
		reason = "is a secret-bearing environment file and cannot be read to prevent credential leakage. If you need to check the file structure, read .env.example instead." + defenseInDepthSuffix
	}
	if reason == "" {
		return ""
	}
	return "Access denied: " + path + " " + reason
}

// Refusal message for a read of a denied icebath path, empty when the read is allowed
func ReadBlockError(env *Env, path string) string { return readBlockError(env, path) }

// ResolveActiveProfileName names the profile the active home sits in under the root profiles dir, default on any failure
func ResolveActiveProfileName(ctx context.Context) string {
	h, err := pypath.Resolve(home.Dir(ctx))
	if err != nil {
		return "default"
	}
	root, err := pypath.Resolve(home.Root())
	if err != nil {
		return "default"
	}
	rel, ok := pypath.PathRelativeTo(h, pypath.PathJoin(root, "profiles"))
	if !ok || rel == "." {
		return "default"
	}
	name, _, _ := strings.Cut(rel, "/")
	return name
}

const sandboxMirrorWarning = "Sandbox-mirror write blocked by soft guard: %s sits under '%s', which is %s " +
	"Use the host-side tool for authoritative state (e.g. ``memory`` for memories), or address the host path directly. " +
	"To bypass %s with ``cross_profile=True``. (Defense-in-depth — not a security boundary; the terminal tool can still bypass.)"

type mirrorInfo struct {
	target string
	root   string
	inner  string
}

// Classifies a write target as a per task sandbox mirror of icebath state
func classifySandboxMirrorTarget(path string) *mirrorInfo {
	target := realPath(path)
	parts := strings.Split(target, string(filepath.Separator))
	for i, part := range parts {
		if part == "sandboxes" && i+5 < len(parts) && parts[i+3] == "home" && parts[i+4] == ".icebath" {
			inner := ""
			if i+5 < len(parts) {
				inner = filepath.Join(parts[i+5:]...)
			}
			return &mirrorInfo{target: target, root: strings.Join(parts[:i+5], string(filepath.Separator)), inner: inner}
		}
	}
	return nil
}

func formatMirrorWarning(info *mirrorInfo, body, bypass string) string {
	if info == nil {
		return ""
	}
	body = strings.ReplaceAll(body, "{inner_path}", "'"+info.inner+"'")
	return fmt.Sprintf(sandboxMirrorWarning, info.target, info.root, body, bypass)
}

// Soft guard warning when the path lands in a sandbox mirror, empty otherwise
func sandboxMirrorWarningFor(path string) string {
	return formatMirrorWarning(classifySandboxMirrorTarget(path),
		"a per-task mirror created by a non-local terminal backend (docker/daytona/etc.). Writes here land on a copy that the host icebath process never reads — the authoritative file is likely {inner_path} under the real ICEBATH_HOME.",
		"this guard after explicit user direction, retry the call")
}

// Soft guard warning when the path lands in the container side bind mounted home mirror
func containerMirrorWarningFor(path, mirrorPrefix string) string {
	if mirrorPrefix == "" {
		return ""
	}
	target, mirror := realPath(path), realPath(mirrorPrefix)
	if !isUnder(target, mirror) {
		return ""
	}
	inner, _ := filepath.Rel(mirror, target)
	return formatMirrorWarning(&mirrorInfo{target: target, root: mirror, inner: filepath.ToSlash(inner)},
		"the container's bind-mounted home — a per-task mirror that the host icebath process never reads. The authoritative file is {inner_path} under the real ICEBATH_HOME.",
		"after explicit user direction, retry")
}

// ── Secret redaction for file content shown to the model ─────────────────

var prefixPatterns = []string{
	`sk-[A-Za-z0-9_-](?:\.?[A-Za-z0-9_-]){9,}`,
	`ctx7sk-[A-Za-z0-9_-]{10,}`,
	`ghp_[A-Za-z0-9]{10,}`, `github_pat_[A-Za-z0-9_]{10,}`, `gho_[A-Za-z0-9]{10,}`, `ghu_[A-Za-z0-9]{10,}`,
	`ghs_[A-Za-z0-9]{10,}`, `ghr_[A-Za-z0-9]{10,}`,
	`xapp-\d+-[A-Za-z0-9-]{10,}`, `xox[baprs]-[A-Za-z0-9-]{10,}`,
	`AIza[A-Za-z0-9_-]{30,}`, `pplx-[A-Za-z0-9]{10,}`, `fal_[A-Za-z0-9_-]{10,}`, `fc-[A-Za-z0-9]{10,}`,
	`bb_live_[A-Za-z0-9_-]{10,}`, `gAAAA[A-Za-z0-9_=-]{20,}`, `AKIA[A-Z0-9]{16}`,
	`sk_live_[A-Za-z0-9]{10,}`, `sk_test_[A-Za-z0-9]{10,}`, `rk_live_[A-Za-z0-9]{10,}`,
	`SG\.[A-Za-z0-9_-]{10,}`, `hf_[A-Za-z0-9]{10,}`, `r8_[A-Za-z0-9]{10,}`, `npm_[A-Za-z0-9]{10,}`,
	`pypi-[A-Za-z0-9_-]{10,}`, `dop_v1_[A-Za-z0-9]{10,}`, `doo_v1_[A-Za-z0-9]{10,}`,
	`am_(?:org_)?[A-Za-z0-9]{20,}`, `sk_[A-Za-z0-9_]{10,}`, `tvly-[A-Za-z0-9]{10,}`, `exa_[A-Za-z0-9]{10,}`,
	`gsk_[A-Za-z0-9]{10,}`, `syt_[A-Za-z0-9]{10,}`, `retaindb_[A-Za-z0-9]{10,}`, `hsk-[A-Za-z0-9]{10,}`,
	`mem0_[A-Za-z0-9]{10,}`, `brv_[A-Za-z0-9]{10,}`, `xai-[A-Za-z0-9]{30,}`, `ntn_[A-Za-z0-9]{10,}`,
	`fw-[A-Za-z0-9]{30,}`, `fw_[A-Za-z0-9]{30,}`, `fpk_[A-Za-z0-9]{30,}`,
	`glpat-[A-Za-z0-9_\-]{10,}`, `gloas-[A-Za-z0-9_\-]{10,}`, `gldt-[A-Za-z0-9_\-]{10,}`, `glrt-[A-Za-z0-9_.\-]{10,}`,
	`glrtr-[A-Za-z0-9_.\-]{10,}`, `glcbt-[A-Za-z0-9_\-]{10,}`, `glptt-[A-Za-z0-9_\-]{10,}`, `glft-[A-Za-z0-9_\-]{10,}`,
	`glimt-[A-Za-z0-9_\-]{10,}`, `glagent-[A-Za-z0-9_\-]{10,}`, `glsoat-[A-Za-z0-9_\-]{10,}`, `glffct-[A-Za-z0-9_\-]{10,}`,
	`glwt-[A-Za-z0-9_\-]{10,}`, `GR1348941[A-Za-z0-9_\-]{10,}`, `pk-lf-[A-Za-z0-9\-]{8,}`,
}

// Leading literal characters of a regex up to its first metacharacter
func literalPrefix(pattern string) string {
	for i, ch := range pattern {
		if strings.ContainsRune(`[(\.?*+|{^$`, ch) {
			return pattern[:i]
		}
	}
	return pattern
}

var prefixSubstrings = func() []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range prefixPatterns {
		lit := literalPrefix(p)
		if lit != "" && !seen[lit] {
			seen[lit] = true
			out = append(out, lit)
		}
	}
	return out
}()

var prefixAltRe = regexp.MustCompile("(?:" + strings.Join(prefixPatterns, "|") + ")")
var zhipuRe = regexp.MustCompile(`[0-9a-f]{32}\.[A-Za-z0-9]{16,}`)
var controlCharsRe = regexp.MustCompile(`[\x00-\x1f\x7f\x{200b}-\x{200f}\x{2028}-\x{202f}\x{2060}\x{feff}]`)
var displayControlRe = regexp.MustCompile(`[\x00-\x1f\x7f\x{80}-\x{9f}\x{200b}-\x{200f}\x{202a}-\x{202e}\x{2060}-\x{2064}]`)
var tokenBodyChars = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_-."

const secretEnvNames = `(?:API_?KEY|KEY|TOKEN|SECRET|PASSWORD|PASSWD|PASS|PW|CREDENTIAL|AUTH)`
const secretCfgNames = `(?:api[ _.\-]?key|token|secret|passwd|password|credential|auth)`
const yamlCfgNames = `(?:api[ _.\-]?key|token|secret|passwd|password|credential)`
const lineNumberGutter = `(?:[0-9]+(?:[|:\-]|\t)[ \t]*)?`
const jsonKeyNames = `(?:api_?[Kk]ey|token|secret|password|access_token|refresh_token|auth_token|bearer|secret_value|raw_secret|secret_input|key_material)`

var envAssignRe = regexp.MustCompile(`([A-Z0-9_]{0,50}` + secretEnvNames + `[A-Z0-9_]{0,50})\s*=\s*(['"]?)(\S+)`)
var envAssignLowerRe = regexp.MustCompile(`(?i)([a-z0-9_]+_(?:key|pass|pw|token|secret|password|passwd|credential|auth))\s*=\s*(['"]?)(\S+)`)
var cfgSecretWordRe = regexp.MustCompile(`(?i)` + secretCfgNames)
var envLookupValueRe = regexp.MustCompile(`^(?:os\.(?:getenv|environ)|process\.env|\$ENV\{)`)
var cfgDottedRe = regexp.MustCompile(`(?i)([A-Za-z0-9_\-]+\.[A-Za-z0-9_.\-]*` + secretCfgNames + `[A-Za-z0-9_.\-]*|[A-Za-z0-9_.\-]*` + secretCfgNames + `[A-Za-z0-9_.\-]*\.[A-Za-z0-9_.\-]+)=(['"]?)([^\s&]+)`)
var cfgAnchoredRe = regexp.MustCompile(`(?im)(^[ \t]*` + lineNumberGutter + `(?:export[ \t]+)?[A-Za-z0-9_\-]*` + secretCfgNames + `[A-Za-z0-9_\-]*)=(['"]?)([^\s&]+)`)
var yamlAssignRe = regexp.MustCompile(`(?im)(^[ \t]*` + lineNumberGutter + `[A-Za-z0-9_.\-]*` + yamlCfgNames + `[A-Za-z0-9_.\-]*)(:[ \t]*)([^\s&]+)`)
var keyKeywordRe = regexp.MustCompile(`(?i)(?:api|auth|access|refresh|session|secret)[ _.\\-]?(?:key|token)|token|secret|passwd|password|pass|pw|credential|auth|key`)
var strongKeyKeywordRe = regexp.MustCompile(`(?i)(?:api|auth|access|refresh|session|id|bearer)[ _.\\-]?(?:key|token)|key[ _.\\-]?material|secret|passwd|password|pass|pw|credential|auth|bearer`)
var passwordKeyRe = regexp.MustCompile(`(?i)passwd|password|pass|pw`)

const shellVarRef = `\$(?:\{[A-Za-z_]\w*[^}]*\}|[A-Za-z_]\w*)`

var pathOrVarValueRe = regexp.MustCompile(`^(?:` + shellVarRef + `|\$\(|~|/)(?:[\w./:-]|` + shellVarRef + `)*$`)
var jsonFieldRe = regexp.MustCompile(`(?i)("` + jsonKeyNames + `")\s*:\s*"([^"]+)"`)
var jsonEscapedKeyRe = regexp.MustCompile(`(?i)(\\+)"` + jsonKeyNames + ``)
var pythonReprFieldRe = regexp.MustCompile(`'([A-Za-z_][A-Za-z0-9_]*)'(\s*:\s*)(?:([bB]?)'((?:\\.|[^'\\])+)'|([bB]?)"((?:\\.|[^"\\])+)")`)
var authHeaderRe = regexp.MustCompile(`(?i)((?:Proxy-)?Authorization:\s*)([A-Za-z][\w.+-]*\s+)?([^\s"']+)`)
var secretHeaderRe = regexp.MustCompile(`(?i)((?:x-api-key|x-goog-api-key|api-key|apikey|x-api-token|x-auth-token|x-access-token)\s*:\s*)(\S+)`)

// Header names that carry API credentials and are redacted from MCP displays
var SecretHeaderNameList = []string{"x-api-key", "x-goog-api-key", "api-key", "apikey", "x-api-token", "x-auth-token", "x-access-token"}

var telegramRe = regexp.MustCompile(`(bot)?(\d{8,}):([-A-Za-z0-9_]{30,})`)
var privateKeyRe = regexp.MustCompile(`-----BEGIN[A-Z ]*PRIVATE KEY-----[\s\S]*?-----END[A-Z ]*PRIVATE KEY-----`)
var dbConnstrRe = regexp.MustCompile(`(?i)((?:postgres(?:ql)?|mysql|mongodb(?:\+srv)?|redis|amqp)://[^:\s]+:)([^@\s]+)(@)`)
var urlBareTokenRe = regexp.MustCompile(`(?i)((?:https?|wss?|git|ssh|ftp|ftps|sftp)://)([^\s:@/]{8,})(@[^\s]+)`)
var jwtRe = regexp.MustCompile(`eyJ[A-Za-z0-9_-]{10,}(?:\.[A-Za-z0-9_=-]{4,}){0,2}`)
var webappLinkRe = regexp.MustCompile(`(icebath-session=|/webapp/launch#(?:[^\s#]*?&)?ticket=)[A-Za-z0-9_-]{20,}`)
var signalPhoneRe = regexp.MustCompile(`\+[1-9]\d{6,14}`)
var formBodyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]*=[^&\s]*(?:&[A-Za-z_][A-Za-z0-9_.-]*=[^&\s]*)+$`)
var hexRe = regexp.MustCompile(`^[A-Fa-f0-9]+$`)
var opaqueTokenRe = regexp.MustCompile(`^[A-Za-z0-9_./+=-]+$`)

var sensitiveQueryParams = setOf(
	"access_token", "refresh_token", "id_token", "token", "api_key", "apikey",
	"client_secret", "password", "auth", "jwt", "session", "secret", "key",
	"code", "signature", "x-amz-signature", "x-goog-signature", "sig",
)

// Sorted names of the sensitive URL query parameters
func SensitiveQueryParamNames() []string {
	names := make([]string, 0, len(sensitiveQueryParams))
	for name := range sensitiveQueryParams {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

var pythonReprSecretKeys = setOf(
	"access_token", "refresh_token", "id_token", "auth_token", "token", "api_key", "apikey", "client_secret",
	"secret", "password", "passwd", "private_key", "credential", "credentials", "authorization", "bearer",
	"secret_value", "raw_secret", "secret_input", "key_material",
)
var pythonReprEnvSuffixes = []string{"_API_KEY", "_TOKEN", "_SECRET", "_PASSWORD", "_PASSWD", "_CREDENTIAL", "_CREDENTIALS"}
var pythonReprCredentialSuffixes = []string{"apikey", "api_key", "token", "secret", "password", "passwd", "credential", "credentials"}

var vaultRedactionLock sync.Mutex
var vaultRedactionValues = map[string][]string{}

const vaultRedactionMaxPerProfile = 64

// Profile home keying the vault buckets so one profile never scrubs with another's values
func vaultScope(ctx context.Context) string {
	return filepath.Clean(home.Dir(ctx))
}

func vaultScopeOf(env *Env) string {
	if env != nil && env.Home != "" {
		return filepath.Clean(env.Home)
	}
	return vaultScope(context.Background())
}

// RegisterVaultRedactionValue registers a vault secret under the process home profile
func RegisterVaultRedactionValue(value string) {
	RegisterVaultRedactionValueIn(context.Background(), value)
}

// RegisterVaultRedactionValueIn registers a vault secret and its CR LF stripped form for the context profile
func RegisterVaultRedactionValueIn(ctx context.Context, value string) {
	if value == "" {
		return
	}
	normalized := strings.ReplaceAll(strings.ReplaceAll(value, "\r", ""), "\n", "")
	scope := vaultScope(ctx)
	vaultRedactionLock.Lock()
	defer vaultRedactionLock.Unlock()
	bucket := vaultRedactionValues[scope]
	for _, v := range []string{value, normalized} {
		if v == "" {
			continue
		}
		kept := bucket[:0:0]
		for _, old := range bucket {
			if old != v {
				kept = append(kept, old)
			}
		}
		bucket = append(kept, v)
	}
	for len(bucket) > vaultRedactionMaxPerProfile {
		bucket = bucket[1:]
	}
	vaultRedactionValues[scope] = bucket
}

// ClearVaultRedactionValues drops the context profile's registered vault values
func ClearVaultRedactionValues(ctx context.Context) {
	vaultRedactionLock.Lock()
	defer vaultRedactionLock.Unlock()
	delete(vaultRedactionValues, vaultScope(ctx))
}

// Scrubs every vault value registered for the env profile, longest first
func redactRegisteredVaultValues(env *Env, text string) string {
	if text == "" {
		return text
	}
	vaultRedactionLock.Lock()
	values := append([]string{}, vaultRedactionValues[vaultScopeOf(env)]...)
	vaultRedactionLock.Unlock()
	sort.SliceStable(values, func(i, j int) bool { return len([]rune(values[i])) > len([]rune(values[j])) })
	for _, v := range values {
		if strings.Contains(text, v) {
			text = strings.ReplaceAll(text, v, "«redacted-vault-secret»")
		}
	}
	return text
}

// Effective redaction switch from the env file first and the config second, on by default
func redactionEnabled(env *Env) bool {
	if env != nil && env.Secrets != nil {
		if raw, ok := env.Secrets.Get("ICEBATH_REDACT_SECRETS"); ok && strings.TrimSpace(raw) != "" {
			switch strings.ToLower(strings.TrimSpace(raw)) {
			case "1", "true", "yes", "on":
				return true
			}
			return false
		}
	}
	if env != nil && env.Config != nil {
		return env.Config.Bool("security.redact_secrets", true)
	}
	return true
}

// Submatch index arrays of every match that passes the boundary check, retrying one rune past a rejected start
func scanMatches(re *regexp.Regexp, text string, ok func(loc []int) bool) [][]int {
	var out [][]int
	pos := 0
	for pos <= len(text) {
		loc := re.FindStringSubmatchIndex(text[pos:])
		if loc == nil {
			break
		}
		for i := range loc {
			if loc[i] >= 0 {
				loc[i] += pos
			}
		}
		if ok == nil || ok(loc) {
			out = append(out, loc)
			pos = loc[1]
			if loc[1] == loc[0] {
				pos++
			}
			continue
		}
		_, size := utf8.DecodeRuneInString(text[loc[0]:])
		if size == 0 {
			size = 1
		}
		pos = loc[0] + size
	}
	return out
}

// Rebuilds text with each matched span replaced by the callback result
func replaceSpans(text string, locs [][]int, fn func(loc []int) string) string {
	if len(locs) == 0 {
		return text
	}
	var b strings.Builder
	last := 0
	for _, loc := range locs {
		b.WriteString(text[last:loc[0]])
		b.WriteString(fn(loc))
		last = loc[1]
	}
	b.WriteString(text[last:])
	return b.String()
}

func group(text string, loc []int, i int) string {
	if loc[2*i] < 0 {
		return ""
	}
	return text[loc[2*i]:loc[2*i+1]]
}

func prevRune(text string, at int) (rune, bool) {
	if at <= 0 {
		return 0, false
	}
	r, _ := utf8.DecodeLastRuneInString(text[:at])
	return r, true
}

func nextRune(text string, at int) (rune, bool) {
	if at >= len(text) {
		return 0, false
	}
	r, _ := utf8.DecodeRuneInString(text[at:])
	return r, true
}

func isTokenBoundaryRune(r rune) bool {
	return r == '_' || r == '-' || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
}

// Boundary check rejecting a prefix token glued to identifier characters
func prefixBoundaryOK(text string) func(loc []int) bool {
	return func(loc []int) bool {
		if r, ok := prevRune(text, loc[0]); ok && isTokenBoundaryRune(r) {
			return false
		}
		if r, ok := nextRune(text, loc[1]); ok && isTokenBoundaryRune(r) {
			return false
		}
		return true
	}
}

func hasKnownPrefixSubstring(text string) bool {
	for _, sub := range currentPrefixSubstrings() {
		if strings.Contains(text, sub) {
			return true
		}
	}
	return false
}

// Masks a display secret keeping head and tail characters above the floor
func maskSecret(value string, head, tail, floor int) string {
	value = displayControlRe.ReplaceAllString(value, "")
	if value == "" {
		return ""
	}
	runes := pystr.Runes(value)
	if len(runes) < floor {
		return "***"
	}
	return pystr.FromRunes(runes[:head]) + "..." + pystr.FromRunes(runes[len(runes)-tail:])
}

// Exported maskSecret
func MaskDisplaySecret(value string, head, tail, floor int) string {
	return maskSecret(value, head, tail, floor)
}

func maskToken(token string) string {
	if token == "" {
		return "***"
	}
	return maskSecret(token, 6, 4, 18)
}

// Non reusable sentinel keeping only the vendor prefix label
func maskTokenNonreusable(token string) string {
	for _, sub := range currentPrefixSubstrings() {
		if token != "" && strings.HasPrefix(token, sub) {
			return "«redacted:" + sub + "…»"
		}
	}
	return "«redacted-secret»"
}

// Masks prefix tokens whose body is split by control or zero width characters
func maskControlSplitTokens(text string, mask func(string) string) string {
	if !controlCharsRe.MatchString(text) {
		return text
	}
	var stripped strings.Builder
	var origIdx []int
	for i, r := range text {
		if controlCharsRe.MatchString(string(r)) {
			continue
		}
		origIdx = append(origIdx, i)
		stripped.WriteRune(r)
	}
	s := stripped.String()
	runeStart := map[int]int{}
	idx := 0
	for i := range s {
		runeStart[i] = idx
		idx++
	}
	type repl struct {
		start, end int
		text       string
	}
	var repls []repl
	for _, loc := range prefixScan(s) {
		startOrig := origIdx[runeStart[loc[0]]]
		lastRuneIdx := runeStart[loc[1]]
		if _, ok := runeStart[loc[1]]; !ok {
			lastRuneIdx = len(origIdx)
		}
		lastByte := origIdx[lastRuneIdx-1]
		_, size := utf8.DecodeRuneInString(text[lastByte:])
		endOrig := lastByte + size
		span := text[startOrig:endOrig]
		if (strings.Contains(span, "\n") || strings.Contains(span, "\r")) && prefixSearch(span) {
			continue
		}
		clean := true
		for _, r := range span {
			if !strings.ContainsRune(tokenBodyChars, r) && !controlCharsRe.MatchString(string(r)) {
				clean = false
				break
			}
		}
		if clean && (endOrig >= len(text) || text[endOrig] != '=') {
			repls = append(repls, repl{startOrig, endOrig, mask(s[loc[0]:loc[1]])})
		}
	}
	var locs [][]int
	for _, r := range repls {
		locs = append(locs, []int{r.start, r.end})
	}
	i := 0
	return replaceSpans(text, locs, func(loc []int) string {
		out := repls[i].text
		i++
		return out
	})
}

func isWordStart(s []rune, i int) bool {
	if i == 0 {
		return true
	}
	prev, cur := s[i-1], s[i]
	if !unicode.IsLetter(prev) || (unicode.IsUpper(cur) && unicode.IsLower(prev)) {
		return true
	}
	return unicode.IsUpper(cur) && unicode.IsUpper(prev) && i+1 < len(s) && unicode.IsLower(s[i+1])
}

func isWordEnd(s []rune, j int, allowPlural bool) bool {
	if j >= len(s) {
		return true
	}
	cur := s[j]
	if !unicode.IsLetter(cur) || (unicode.IsUpper(cur) && unicode.IsLower(s[j-1])) {
		return true
	}
	return allowPlural && (cur == 's' || cur == 'S') && isWordEnd(s, j+1, false)
}

// Reports whether the keyword regex matches the key at a word boundary
func hasWordBoundedKeyword(key string, re *regexp.Regexp) bool {
	runes := []rune(key)
	byteToRune := map[int]int{}
	ri := 0
	for bi := range key {
		byteToRune[bi] = ri
		ri++
	}
	byteToRune[len(key)] = ri
	for _, loc := range re.FindAllStringIndex(key, -1) {
		if isWordStart(runes, byteToRune[loc[0]]) && isWordEnd(runes, byteToRune[loc[1]], true) {
			return true
		}
	}
	return false
}

func keyHasSecretKeyword(key string) bool { return hasWordBoundedKeyword(key, keyKeywordRe) }

// Credential like shape test for ambiguous token and key values
func looksLikeOpaqueCredential(value string) bool {
	if value == "***" || strings.HasPrefix(value, "«redacted:") {
		return true
	}
	n := pystr.Len(value)
	if n >= 16 && hexRe.MatchString(value) {
		return true
	}
	if n >= 20 && opaqueTokenRe.MatchString(value) {
		return true
	}
	if n < 12 {
		return false
	}
	classes := 0
	if strings.ContainsAny(value, "abcdefghijklmnopqrstuvwxyz") {
		classes++
	}
	if strings.ContainsAny(value, "ABCDEFGHIJKLMNOPQRSTUVWXYZ") {
		classes++
	}
	if strings.ContainsAny(value, "0123456789") {
		classes++
	}
	return classes >= 2
}

// Reports whether a dot free segment is long and mixes case and digits like a secret
func opaquePathSegment(seg string) bool {
	if strings.Contains(seg, ".") || pystr.Len(seg) < 16 {
		return false
	}
	return strings.ContainsAny(seg, "abcdefghijklmnopqrstuvwxyz") && strings.ContainsAny(seg, "ABCDEFGHIJKLMNOPQRSTUVWXYZ") &&
		strings.ContainsAny(seg, "0123456789")
}

// Shared gate for the ENV, JSON and YAML assignment passes
func shouldRedactAssignment(key, value string, checkKeyword bool) bool {
	if envLookupValueRe.MatchString(value) {
		return false
	}
	if value == "***" || strings.HasPrefix(value, "«redacted") {
		return false
	}
	if checkKeyword && !keyHasSecretKeyword(key) {
		return false
	}
	if pathOrVarValueRe.MatchString(value) && !hasWordBoundedKeyword(key, passwordKeyRe) {
		opaque := false
		for _, seg := range strings.Split(value, "/") {
			if opaquePathSegment(seg) {
				opaque = true
				break
			}
		}
		if value[0] == '$' || !opaque {
			return false
		}
	}
	return hasWordBoundedKeyword(key, strongKeyKeywordRe) || looksLikeOpaqueCredential(value)
}

// Splits a greedy quoted value the way the backreferenced regex would, returning quote, value and the consumed end
func greedyQuotedValue(text string, loc []int, quoteGroup, valueGroup int) (string, string, int) {
	quote := group(text, loc, quoteGroup)
	run := group(text, loc, valueGroup)
	runStart := loc[2*valueGroup]
	if quote == "" {
		return "", run, loc[1]
	}
	idx := strings.LastIndex(run, quote)
	if idx >= 1 {
		return quote, run[:idx], runStart + idx + 1
	}
	return "", quote + run, loc[1]
}

// Splits a lazy config value whose closing quote must end the run
func lazyQuotedValue(text string, loc []int, quoteGroup, valueGroup int) (string, string) {
	quote := group(text, loc, quoteGroup)
	run := group(text, loc, valueGroup)
	if quote == "" {
		return "", run
	}
	if len(run) >= 2 && strings.HasSuffix(run, quote) {
		return quote, run[:len(run)-1]
	}
	return "", quote + run
}

func isCfgKeyRune(r rune) bool {
	return r == '_' || r == '.' || r == '-' || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
}

// ENV, config, JSON and YAML assignment passes
func redactAssignments(text string, maskNonreusable bool) string {
	mask := maskToken
	if maskNonreusable {
		mask = maskTokenNonreusable
	}
	if strings.Contains(text, "=") {
		envSub := func(re *regexp.Regexp, boundary func(loc []int) bool) {
			locs := scanSubMatches(re, text, false, boundary, func(loc []int) (bool, int) {
				_, value, end := greedyQuotedValue(text, loc, 2, 3)
				return shouldRedactAssignment(group(text, loc, 1), value, true), end
			})
			if len(locs) == 0 {
				return
			}
			var b strings.Builder
			last := 0
			for _, loc := range locs {
				quote, value, end := greedyQuotedValue(text, loc, 2, 3)
				b.WriteString(text[last:loc[0]])
				b.WriteString(group(text, loc, 1) + "=" + quote + mask(value) + quote)
				last = end
			}
			b.WriteString(text[last:])
			text = b.String()
		}
		envSub(envAssignRe, nil)
		if !strings.Contains(text, "://") {
			envSub(envAssignLowerRe, func(loc []int) bool {
				r, ok := prevRune(text, loc[0])
				return !ok || !(r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r))
			})
		}
		if !strings.Contains(text, "://") && cfgSecretWordRe.MatchString(text) {
			cfgSub := func(re *regexp.Regexp, lineAnchored bool, boundary func(loc []int) bool) {
				locs := scanSubMatches(re, text, lineAnchored, boundary, func(loc []int) (bool, int) {
					_, value := lazyQuotedValue(text, loc, 2, 3)
					return shouldRedactAssignment(group(text, loc, 1), value, true), loc[1]
				})
				text = replaceSpans(text, locs, func(loc []int) string {
					quote, value := lazyQuotedValue(text, loc, 2, 3)
					return group(text, loc, 1) + "=" + quote + mask(value) + quote
				})
			}
			cfgSub(cfgDottedRe, false, func(loc []int) bool {
				r, ok := prevRune(text, loc[0])
				return !ok || !isCfgKeyRune(r)
			})
			cfgSub(cfgAnchoredRe, true, nil)
		}
	}
	if strings.Contains(text, ":") && strings.Contains(text, `"`) {
		locs := scanSubMatches(jsonFieldRe, text, false, nil, func(loc []int) (bool, int) {
			return shouldRedactAssignment(group(text, loc, 1), group(text, loc, 2), false), loc[1]
		})
		text = replaceSpans(text, locs, func(loc []int) string {
			return group(text, loc, 1) + `: "` + mask(group(text, loc, 2)) + `"`
		})
		if strings.Contains(text, `\"`) {
			text = redactEscapedJSONFields(text, mask)
		}
	}
	if strings.Contains(text, ":") && strings.Contains(text, "'") {
		text = redactPythonReprFields(text)
	}
	if strings.Contains(text, ":") && !strings.Contains(text, "://") {
		locs := scanSubMatches(yamlAssignRe, text, true, nil, func(loc []int) (bool, int) {
			value := group(text, loc, 3)
			if strings.HasPrefix(value, "'") || strings.HasPrefix(value, `"`) {
				return false, loc[1]
			}
			return shouldRedactAssignment(group(text, loc, 1), value, true), loc[1]
		})
		text = replaceSpans(text, locs, func(loc []int) string {
			return group(text, loc, 1) + group(text, loc, 2) + mask(group(text, loc, 3))
		})
	}
	return text
}

// Masks credential fields inside a JSON encoded string whose quotes are backslash escaped
func redactEscapedJSONFields(text string, mask func(string) string) string {
	type span struct {
		start, end int
		out        string
	}
	var spans []span
	pos := 0
	for pos < len(text) {
		loc := jsonEscapedKeyRe.FindStringSubmatchIndex(text[pos:])
		if loc == nil {
			break
		}
		start, keyEnd := pos+loc[0], pos+loc[1]
		slashes := text[pos+loc[2] : pos+loc[3]]
		rest := text[keyEnd:]
		closer := slashes + `"`
		if !strings.HasPrefix(rest, closer) {
			pos = start + 1
			continue
		}
		i := len(closer)
		for i < len(rest) && (rest[i] == ' ' || rest[i] == '\t' || rest[i] == '\n' || rest[i] == '\r') {
			i++
		}
		if i >= len(rest) || rest[i] != ':' {
			pos = start + 1
			continue
		}
		i++
		for i < len(rest) && (rest[i] == ' ' || rest[i] == '\t' || rest[i] == '\n' || rest[i] == '\r') {
			i++
		}
		if !strings.HasPrefix(rest[i:], closer) {
			pos = start + 1
			continue
		}
		valueStart := i + len(closer)
		j := valueStart
		for j < len(rest) && rest[j] != '"' && rest[j] != '\\' {
			j++
		}
		if j == valueStart || !strings.HasPrefix(rest[j:], closer) {
			pos = start + 1
			continue
		}
		value := rest[valueStart:j]
		end := keyEnd + j + len(closer)
		key := text[start:keyEnd] + closer
		if shouldRedactAssignment(key, value, false) {
			spans = append(spans, span{start, end, key + ": " + closer + mask(value) + closer})
		}
		pos = end
	}
	var locs [][]int
	for _, s := range spans {
		locs = append(locs, []int{s.start, s.end})
	}
	i := 0
	return replaceSpans(text, locs, func(loc []int) string {
		out := spans[i].out
		i++
		return out
	})
}

func isPythonReprSecretKey(key string) bool {
	folded := strings.ToLower(key)
	if pythonReprSecretKeys[folded] {
		return true
	}
	if key == strings.ToUpper(key) && key != strings.ToLower(key) {
		for _, suffix := range pythonReprEnvSuffixes {
			if strings.HasSuffix(key, suffix) {
				return true
			}
		}
	}
	for _, suffix := range pythonReprCredentialSuffixes {
		if strings.HasSuffix(folded, suffix) {
			return true
		}
	}
	return false
}

// Reports whether a field name denotes a secret
func IsSecretFieldName(key string) bool { return isPythonReprSecretKey(key) }

// Fully masks credential fields in Python mapping repr output
func redactPythonReprFields(text string) string {
	return pythonReprFieldRe.ReplaceAllStringFunc(text, func(m string) string {
		loc := pythonReprFieldRe.FindStringSubmatchIndex(m)
		key := group(m, loc, 1)
		if !isPythonReprSecretKey(key) {
			return m
		}
		var prefix, quote, value string
		if loc[8] >= 0 {
			prefix, quote, value = group(m, loc, 3), "'", group(m, loc, 4)
		} else {
			prefix, quote, value = group(m, loc, 5), `"`, group(m, loc, 6)
		}
		if envLookupValueRe.MatchString(value) || strings.Contains(value, "***") || strings.HasPrefix(value, "«redacted:") {
			return m
		}
		return "'" + key + "'" + group(m, loc, 2) + prefix + quote + "***" + quote
	})
}

// Replaces values of sensitive k=v pairs with asterisks
func redactQueryString(query string) string {
	if query == "" {
		return query
	}
	pairs := strings.Split(query, "&")
	for i, pair := range pairs {
		key, _, found := strings.Cut(pair, "=")
		if found && sensitiveQueryParams[strings.ToLower(key)] {
			pairs[i] = key + "=***"
		}
	}
	return strings.Join(pairs, "&")
}

// Redacts sensitive values when the whole text is a clean form body
func redactFormBody(text string) string {
	trimmed := strings.TrimSpace(text)
	if text == "" || strings.Contains(text, "\n") || !strings.Contains(text, "&") || !formBodyRe.MatchString(trimmed) {
		return text
	}
	return redactQueryString(trimmed)
}

// Database connection string passwords and bare token URL userinfo
func redactURLCredentials(text string, codeFile bool) string {
	text = dbConnstrRe.ReplaceAllStringFunc(text, func(m string) string {
		loc := dbConnstrRe.FindStringSubmatchIndex(m)
		pw := group(m, loc, 2)
		if codeFile && strings.HasPrefix(pw, "{") && strings.HasSuffix(pw, "}") {
			return m
		}
		return group(m, loc, 1) + "***" + group(m, loc, 3)
	})
	return urlBareTokenRe.ReplaceAllStringFunc(text, func(m string) string {
		loc := urlBareTokenRe.FindStringSubmatchIndex(m)
		return group(m, loc, 1) + maskToken(group(m, loc, 2)) + group(m, loc, 3)
	})
}

func redactPhone(phone string) string {
	keep := 4
	if len(phone) <= 8 {
		keep = 2
	}
	return phone[:keep] + "****" + phone[len(phone)-keep:]
}

// Applies every redaction pattern to file content returned to the model
func redactFileText(env *Env, text string, secretFile bool) string {
	if text == "" {
		return text
	}
	text = redactRegisteredVaultValues(env, text)
	if !redactionEnabled(env) {
		return text
	}
	codeFile := !secretFile
	if hasKnownPrefixSubstring(text) {
		text = maskControlSplitTokens(text, maskTokenNonreusable)
		text = replaceSpans(text, prefixScan(text), func(loc []int) string {
			return maskTokenNonreusable(text[loc[0]:loc[1]])
		})
	}
	if strings.Contains(text, ".") {
		text = replaceSpans(text, scanMatches(zhipuRe, text, func(loc []int) bool {
			if r, ok := prevRune(text, loc[0]); ok && (isTokenBoundaryRune(r) || r == '.') {
				return false
			}
			if r, ok := nextRune(text, loc[1]); ok && (isTokenBoundaryRune(r) || r == '.') {
				return false
			}
			return true
		}), func(loc []int) string { return maskTokenNonreusable(text[loc[0]:loc[1]]) })
	}
	if !codeFile {
		text = redactAssignments(text, true)
	}
	if strings.Contains(text, "uthorization") || strings.Contains(text, "UTHORIZATION") {
		text = authHeaderRe.ReplaceAllStringFunc(text, func(m string) string {
			loc := authHeaderRe.FindStringSubmatchIndex(m)
			return group(m, loc, 1) + group(m, loc, 2) + maskToken(group(m, loc, 3))
		})
	}
	if strings.Contains(text, ":") {
		text = secretHeaderRe.ReplaceAllStringFunc(text, func(m string) string {
			loc := secretHeaderRe.FindStringSubmatchIndex(m)
			return group(m, loc, 1) + maskToken(group(m, loc, 2))
		})
		text = replaceSpans(text, scanMatches(telegramRe, text, func(loc []int) bool {
			r, ok := prevRune(text, loc[0])
			return !ok || !unicode.IsDigit(r)
		}), func(loc []int) string {
			return group(text, loc, 1) + group(text, loc, 2) + ":***"
		})
	}
	if strings.Contains(text, "BEGIN") && strings.Contains(text, "-----") {
		text = privateKeyRe.ReplaceAllString(text, "[REDACTED PRIVATE KEY]")
	}
	if strings.Contains(text, "://") {
		text = redactURLCredentials(text, codeFile)
	}
	if strings.Contains(text, "eyJ") {
		text = jwtRe.ReplaceAllStringFunc(text, maskToken)
	}
	if strings.Contains(text, "icebath-session=") || strings.Contains(text, "/webapp/launch#") {
		text = replaceSpans(text, scanMatches(webappLinkRe, text, func(loc []int) bool {
			if !strings.HasPrefix(text[loc[0]:], "icebath-session=") {
				return true
			}
			r, ok := prevRune(text, loc[0])
			return !ok || !isTokenBoundaryRune(r)
		}), func(loc []int) string { return group(text, loc, 1) + "***" })
	}
	if strings.Contains(text, "&") && strings.Contains(text, "=") {
		text = redactFormBody(text)
	}
	if strings.Contains(text, "+") {
		text = replaceSpans(text, scanMatches(signalPhoneRe, text, func(loc []int) bool {
			r, ok := nextRune(text, loc[1])
			return !ok || !(unicode.IsLetter(r) || unicode.IsDigit(r))
		}), func(loc []int) string { return redactPhone(text[loc[0]:loc[1]]) })
	}
	return text
}

var shellRcBasenames = setOf(".bashrc", ".bash_profile", ".bash_login", ".profile", ".zshrc", ".zprofile", ".zlogin", ".zshenv")

// Reports whether an absolute config.yaml path sits under the active home or root
func isUnderIcebathHome(env *Env, path string) bool {
	target := realPath(path)
	for _, h := range icebathDirs(env) {
		if isUnder(target, h) {
			return true
		}
	}
	return false
}

// Reports whether a path names a secret bearing file whose read must run the assignment passes
func isSecretFileArg(env *Env, arg string) bool {
	path := strings.ReplaceAll(strings.Trim(arg, `"'`), `\`, "/")
	homeRef := false
	for _, prefix := range []string{"$ICEBATH_HOME/", "${ICEBATH_HOME}/"} {
		if strings.HasPrefix(path, prefix) {
			path = path[len(prefix):]
			homeRef = true
			break
		}
	}
	for _, prefix := range []string{"$HOME/", "${HOME}/"} {
		if strings.HasPrefix(path, prefix) {
			path = path[len(prefix):]
			break
		}
	}
	if strings.Contains(path, "$") {
		return false
	}
	var parts []string
	for _, part := range strings.Split(path, "/") {
		if part != "" {
			parts = append(parts, strings.ToLower(part))
		}
	}
	if len(parts) == 0 {
		return false
	}
	last := parts[len(parts)-1]
	if blockedProjectEnvBasenames[last] || shellRcBasenames[last] {
		return true
	}
	if last != "config.yaml" && !strings.HasPrefix(last, "config.yaml.good.") && !strings.HasPrefix(last, "config.yaml.corrupt.") {
		return false
	}
	if homeRef {
		return true
	}
	for _, part := range parts[:len(parts)-1] {
		if part == ".icebath" {
			return true
		}
	}
	return isUnderIcebathHome(env, path)
}

// Boolean config value at a path, the fallback when the env resolves no tree
func cfgBool(env *Env, path string, fallback bool) bool {
	if tree := homeConfigTree(env); tree != nil {
		return tree.Bool(path, fallback)
	}
	return fallback
}

// Integer config value at a path, the fallback when the env resolves no tree
func cfgInt(env *Env, path string, fallback int) int {
	if tree := homeConfigTree(env); tree != nil {
		return tree.Int(path, fallback)
	}
	return fallback
}

// String config value at a path, the fallback when the env resolves no tree
func cfgString(env *Env, path, fallback string) string {
	if tree := homeConfigTree(env); tree != nil {
		return tree.String(path, fallback)
	}
	return fallback
}

// Attached tree, else the readonly config of the home the env names, nil without either
func homeConfigTree(env *Env) config.Tree {
	if env == nil || (env.Config == nil && strings.TrimSpace(env.Home) == "") {
		return nil
	}
	tree, err := liveConfigTree(env)
	if err != nil {
		return nil
	}
	return tree
}

// Tree attached to the env, else the readonly config of its home loaded at call time
func liveConfigTree(env *Env) (config.Tree, error) {
	if env != nil && env.Config != nil {
		return env.Config, nil
	}
	h := envHome(env)
	lookup := config.NewSecrets(h).Lookup()
	if env != nil && env.Secrets != nil {
		lookup = env.Secrets.Lookup()
	}
	return config.LoadReadonly(home.WithOverride(context.Background(), h), h, lookup)
}

// Config tree every tool reads, the defaults when the env resolves none
func cfgTree(env *Env) config.Tree {
	if tree := homeConfigTree(env); tree != nil {
		return tree
	}
	return config.Defaults()
}
