package tools

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

// ProfileSecretSource is the secret scope and dotenv bookkeeping of pkg/provider the child env builders read
type ProfileSecretSource interface {
	ManagedDotenvKeys() []string
	SourceSuppliedNames() []string
	BuildProfileSecretScope(profileHome string) map[string]string
	CurrentSecretScope(ctx context.Context) map[string]string
}

// ProfileSecrets is installed by the provider wiring before any child environment is built
var ProfileSecrets ProfileSecretSource

// Secret source hook, panicking with the owner named when it was never installed
func profileSecrets() ProfileSecretSource {
	if ProfileSecrets == nil {
		panic("tools: the provider wiring must install ProfileSecrets before a child environment is built")
	}
	return ProfileSecrets
}

// Terminal temp cache pruning

// Hours a terminal temp artifact may sit idle before pruning
const TerminalTempMaxIdleHours = 24.0

// Background artifact triplets judged per group by their newest member
var bgGroupRe = regexp.MustCompile(`^(icebath_bg_[A-Za-z0-9_-]+)\.(log|pid|exit)$`)

// Managed terminal temp dir under the home cache
func defaultTerminalTempDir(env *Env) string {
	return filepath.Join(envHome(env), "cache", "terminal")
}

// Reports whether path or anything beneath it was written at or after cutoff, unreadable counts as touched
func subtreeTouchedSince(path string, cutoff time.Time) bool {
	st, err := os.Lstat(path)
	if err != nil {
		return true
	}
	if !st.ModTime().Before(cutoff) {
		return true
	}
	if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return false
	}
	stack := []string{path}
	for len(stack) > 0 {
		current := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		entries, err := os.ReadDir(current)
		if err != nil {
			return true
		}
		for _, child := range entries {
			full := filepath.Join(current, child.Name())
			cst, err := os.Lstat(full)
			if err != nil {
				return true
			}
			isLink := cst.Mode()&os.ModeSymlink != 0
			if !cst.ModTime().Before(cutoff) && !(fileLinkCount(cst) > 1 && cst.Mode().IsRegular()) {
				return true
			}
			if cst.IsDir() && !isLink {
				stack = append(stack, full)
			}
		}
	}
	return false
}

// CleanupTerminalTempCache deletes managed temp artifacts idle for maxAgeHours and returns the count removed
func CleanupTerminalTempCache(env *Env, maxAgeHours float64) int {
	root := defaultTerminalTempDir(env)
	cutoff := time.Now().Add(-time.Duration(maxAgeHours * float64(time.Hour)))
	entries, err := os.ReadDir(root)
	if err != nil {
		return 0
	}
	type candidate struct {
		path  string
		name  string
		isDir bool
	}
	var candidates []candidate
	groupNewest := map[string]time.Time{}
	for _, e := range entries {
		full := filepath.Join(root, e.Name())
		st, err := os.Lstat(full)
		if err != nil {
			continue
		}
		candidates = append(candidates, candidate{full, e.Name(), st.IsDir()})
		if m := bgGroupRe.FindStringSubmatch(e.Name()); m != nil {
			if st.ModTime().After(groupNewest[m[1]]) {
				groupNewest[m[1]] = st.ModTime()
			}
		}
	}
	groupTouched := func(stem string) bool {
		for _, sfx := range []string{"log", "pid", "exit"} {
			st, err := os.Lstat(filepath.Join(root, stem+"."+sfx))
			if err != nil {
				if os.IsNotExist(err) {
					continue
				}
				return true
			}
			if !st.ModTime().Before(cutoff) {
				return true
			}
		}
		return false
	}
	removed := 0
	for _, c := range candidates {
		if m := bgGroupRe.FindStringSubmatch(c.name); m != nil {
			if !groupNewest[m[1]].Before(cutoff) || groupTouched(m[1]) {
				continue
			}
		} else if subtreeTouchedSince(c.path, cutoff) {
			continue
		}
		if c.isDir {
			os.RemoveAll(c.path)
		} else {
			os.Remove(c.path)
		}
		if _, err := os.Lstat(c.path); os.IsNotExist(err) {
			removed++
		}
	}
	return removed
}

// Prunes the managed temp dir at most once per process
var terminalTempPruneOnce sync.Once

// Runs the idle prune once per process for CLI only installs
func pruneTerminalTempOnce(env *Env) {
	terminalTempPruneOnce.Do(func() {
		CleanupTerminalTempCache(env, TerminalTempMaxIdleHours)
	})
}

// Windows and MSYS path translation

// Native drive path spelling
var nativeDriveRe = regexp.MustCompile(`^([a-zA-Z]):[\\/]*(.*)$`)

// Resolves the initial cwd to an absolute host path, anchoring a relative name once
func resolveLocalInitialCwd(cwd string) string {
	expanded := ""
	if cwd != "" {
		expanded = expandUser(cwd)
	} else if wd, err := os.Getwd(); err == nil {
		expanded = wd
	}
	if isWindowsHost {
		expanded = msysToWindowsPath(expanded)
		if isWindowsDrivePath(expanded) || strings.HasPrefix(expanded, `\\`) {
			return expanded
		}
	}
	if filepath.IsAbs(expanded) {
		return expanded
	}
	candidate, err := filepath.Abs(expanded)
	if err != nil {
		candidate = expanded
	}
	current, _ := os.Getwd()
	if st, err := os.Stat(candidate); err != nil || !st.IsDir() {
		wanted := splitPathParts(expanded)
		have := splitPathParts(current)
		if len(wanted) > 0 && len(wanted) <= len(have) && pathPartsEqual(have[len(have)-len(wanted):], wanted) {
			return current
		}
	}
	return candidate
}

// Non empty path components
func splitPathParts(p string) []string {
	var parts []string
	for _, s := range strings.FieldsFunc(p, func(r rune) bool { return r == '/' || r == '\\' }) {
		if s != "" && s != "." {
			parts = append(parts, s)
		}
	}
	return parts
}

// Component wise equality of two path part lists
func pathPartsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Rewrites a native drive path to the Git Bash form on Windows, no op elsewhere
func windowsToMsysPath(cwd string) string {
	if !isWindowsHost || cwd == "" {
		return cwd
	}
	m := nativeDriveRe.FindStringSubmatch(cwd)
	if m == nil {
		return cwd
	}
	tail := strings.TrimLeft(strings.ReplaceAll(m[2], `\`, "/"), "/")
	return "/" + strings.ToLower(m[1]) + "/" + tail
}

// Path safe to embed in a Git Bash script on Windows
func bashSafePath(p string) string {
	if isWindowsHost && p != "" {
		return strings.ReplaceAll(windowsToMsysPath(p), `\`, "/")
	}
	return p
}

// Quotes a path for interpolation into a Git Bash script
func quoteBashPath(p string) string {
	return shQuote(bashSafePath(p))
}

// Reports whether this process can chdir into path
func cwdUsable(p string) bool {
	st, err := os.Stat(p)
	if err != nil || !st.IsDir() {
		return false
	}
	return dirSearchable(p)
}

// The cwd if enterable, else the nearest usable ancestor, else the system temp dir
func resolveSafeCwd(env *Env, cwd string) string {
	cwd = msysToWindowsPath(cwd)
	if cwd != "" && cwdUsable(cwd) {
		return cwd
	}
	if st, err := os.Stat(cwd); cwd != "" && err == nil && st.IsDir() {
		toolsLogger(env).Warn(fmt.Sprintf("Configured terminal cwd %q exists but is not accessible to this user (uid=%s) — falling back to the nearest usable directory. If this is a gateway/cron process, check for root-owned paths leaking into terminal.cwd / TERMINAL_CWD (#65583).", cwd, currentUID()))
	}
	parent := ""
	if cwd != "" {
		parent = filepath.Dir(cwd)
	}
	for parent != "" && !cwdUsable(parent) {
		next := filepath.Dir(parent)
		if next == parent {
			return os.TempDir()
		}
		parent = next
	}
	if parent == "" {
		return os.TempDir()
	}
	return parent
}

// Child process environment construction

// Session variables the gateway bridges freshly onto every command's process environment
var SessionContextEnvVars = []string{
	"ICEBATH_SESSION_PLATFORM", "ICEBATH_SESSION_SOURCE", "ICEBATH_SESSION_CHAT_ID",
	"ICEBATH_SESSION_CHAT_TYPE", "ICEBATH_SESSION_CHAT_NAME", "ICEBATH_SESSION_THREAD_ID",
	"ICEBATH_SESSION_USER_ID", "ICEBATH_SESSION_USER_ID_ALT", "ICEBATH_SESSION_USER_NAME",
	"ICEBATH_SESSION_SCOPE_ID", "ICEBATH_SESSION_KEY", "ICEBATH_SESSION_ID",
	"ICEBATH_UI_SESSION_ID", "ICEBATH_SESSION_MESSAGE_ID", "ICEBATH_SESSION_PROFILE",
	"ICEBATH_CRON_SESSION", "ICEBATH_SESSION_PARENT_CHAT_ID",
	"ICEBATH_CRON_AUTO_DELIVER_PLATFORM", "ICEBATH_CRON_AUTO_DELIVER_CHAT_ID", "ICEBATH_CRON_AUTO_DELIVER_THREAD_ID",
}

// Env.Values key holding the bound session context variables of the current task
const sessionContextValuesKey = "session.context"

// Env.Values key set once any session has been bound in this process
const sessionContextEngagedKey = "session.context_engaged"

// BindSessionContext stores the bound session variables and marks the session context engaged
func BindSessionContext(env *Env, vars map[string]string) {
	if env == nil || env.Values == nil {
		return
	}
	copyVars := map[string]string{}
	for k, v := range vars {
		copyVars[k] = v
	}
	env.Values.Store(sessionContextValuesKey, copyVars)
	env.Values.Store(sessionContextEngagedKey, true)
}

// Bound session variables and whether any session was ever bound
func sessionContextState(env *Env) (map[string]string, bool) {
	if env == nil || env.Values == nil {
		return nil, false
	}
	engaged := false
	if v, ok := env.Values.Load(sessionContextEngagedKey); ok {
		engaged, _ = v.(bool)
	}
	bound, _ := env.Values.Load(sessionContextValuesKey)
	vars, _ := bound.(map[string]string)
	return vars, engaged
}

// Bridges the ICEBATH_HOME of the served profile and the subprocess HOME contract
func applyProfileHome(env *Env, child map[string]string) {
	if env != nil && env.Home != "" && env.Home != home.Process() {
		child["ICEBATH_HOME"] = env.Home
	}
	applySubprocessHomeEnv(env, child)
}

// Bridges the bound session variables, stripping unbound names once a session context is engaged
func injectSessionContextEnv(env *Env, child map[string]string) {
	vars, engaged := sessionContextState(env)
	for _, name := range SessionContextEnvVars {
		if value, ok := vars[name]; ok {
			child[name] = value
		} else if engaged {
			delete(child, name)
		}
	}
}

// First party platform credential prefixes carved out of the terminal blocklist
var terminalFirstPartyEnvPrefixes = []string{"BUZZ_"}

// Pure case folded prefix check of a first party credential name
func matchesTerminalFirstPartyPrefix(name string) bool {
	upper := strings.ToUpper(name)
	for _, p := range terminalFirstPartyEnvPrefixes {
		if strings.HasPrefix(upper, p) {
			return true
		}
	}
	return false
}

// Reports whether this process or session operates as a Buzz agent
func buzzTerminalContextActive(env *Env) bool {
	if os.Getenv("BUZZ_MANAGED_AGENT") != "" {
		return true
	}
	return strings.ToLower(strings.TrimSpace(sessionValue(env, "platform", "ICEBATH_SESSION_PLATFORM"))) == "buzz"
}

// Reports whether a first party credential is entitled to reach terminal children here
func isTerminalFirstPartyEnv(env *Env, name string) bool {
	return matchesTerminalFirstPartyPrefix(name) && buzzTerminalContextActive(env)
}

// Prefix a caller uses to force a managed name through the secret filter
const providerEnvForcePrefix = "_ICEBATH_FORCE_"

// Copies items into out dropping managed secrets, unwrapping forced names when allowed
func filterSecretEnv(env *Env, items, out map[string]string, unwrapForce bool, pluginStrip map[string]bool) error {
	stripFolded := map[string]bool{}
	for k := range pluginStrip {
		stripFolded[strings.ToUpper(k)] = true
	}
	keys := make([]string, 0, len(items))
	for k := range items {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		value := items[key]
		if strings.HasPrefix(key, providerEnvForcePrefix) {
			if !unwrapForce {
				continue
			}
			key = key[len(providerEnvForcePrefix):]
			if !IsIcebathInternalSecret(key) {
				out[key] = value
			}
			continue
		}
		if IsIcebathInternalSecret(key) || stripFolded[strings.ToUpper(key)] {
			continue
		}
		firstParty := isTerminalFirstPartyEnv(env, key)
		passthrough := IsEnvPassthrough(env, key)
		if IsProviderEnvBlocklisted(env, key) && !(passthrough || firstParty) {
			continue
		}
		resolved := &value
		if passthrough && !firstParty {
			r, err := ResolvePassthroughValue(env, key, &value)
			if err != nil {
				return err
			}
			resolved = r
		}
		if resolved != nil {
			out[key] = *resolved
		}
	}
	return nil
}

// KanbanHomeResolver names the board root this lineage lives under, installed by the kanban package
var KanbanHomeResolver func(env *Env) string

// Marker carrying the delegate child fence across a process spawn
const delegatedChildEnvMarker = "ICEBATH_DELEGATED_CHILD_CONTEXT"

// Worker identity keys scrubbed from every descendant
var kanbanEnvKeys = []string{
	"ICEBATH_KANBAN_TASK", "ICEBATH_KANBAN_RUN_ID", "ICEBATH_KANBAN_CLAIM_LOCK",
	"ICEBATH_KANBAN_GOAL_MODE", "ICEBATH_KANBAN_GOAL_MAX_TURNS",
}

// Board root the fence applies to, the legacy 1 when it cannot be resolved
func fencedKanbanRoot(env *Env) string {
	if KanbanHomeResolver == nil {
		return "1"
	}
	if root := KanbanHomeResolver(env); root != "" {
		return root
	}
	return "1"
}

// Removes worker identity while retaining the board and an inherited write fence
func scrubKanbanEnv(env *Env, child map[string]string) map[string]string {
	cleaned := map[string]string{}
	for k, v := range child {
		cleaned[k] = v
	}
	for _, k := range kanbanEnvKeys {
		delete(cleaned, k)
	}
	if cleaned["ICEBATH_SESSION_SOURCE"] == "kanban" {
		delete(cleaned, "ICEBATH_SESSION_SOURCE")
	}
	inherited := child[delegatedChildEnvMarker]
	if inherited != "" && inherited != "1" {
		cleaned[delegatedChildEnvMarker] = inherited
	} else {
		cleaned[delegatedChildEnvMarker] = fencedKanbanRoot(env)
	}
	return cleaned
}

// Carries the delegate descendant denial across a real process spawn
func delegatedChildSubprocessEnv(env *Env, child map[string]string) map[string]string {
	if !(isDelegatedChildProcessContext(env) || os.Getenv("ICEBATH_KANBAN_TASK") != "" ||
		child["ICEBATH_KANBAN_TASK"] != "" || child[delegatedChildEnvMarker] != "") {
		return child
	}
	return scrubKanbanEnv(env, child)
}

// Guards shared by every spawn surface, returns the possibly new map
func finalizeChildEnv(env *Env, child map[string]string) map[string]string {
	applyProfileHome(env, child)
	injectSessionContextEnv(env, child)
	stripIcebathOwnedPythonpathAndRuntimeMarkers(child)
	StripSupervisedProcessIdentity(child)
	applyWindowsMsysBashEnvDefaults(child)
	return delegatedChildSubprocessEnv(env, child)
}

// One source of a scrubbed env and whether forced names unwrap from it
type envPart struct {
	items       map[string]string
	unwrapForce bool
}

// Filters every part into one env, rewrites PATH and applies the shared guards
func scrubbedEnv(env *Env, parts []envPart, pluginStrip map[string]bool, fixPath func(string) string) (map[string]string, error) {
	out := map[string]string{}
	for _, p := range parts {
		if err := filterSecretEnv(env, p.items, out, p.unwrapForce, pluginStrip); err != nil {
			return nil, err
		}
	}
	present := map[string]bool{}
	for k := range out {
		present[k] = true
	}
	for k, v := range ScopedPassthroughAdditions(env, present) {
		if !pluginStrip[k] {
			out[k] = v
		}
	}
	if pathKey, ok := pathEnvKey(out); ok {
		out[pathKey] = prependIcebathBinDir(fixPath(out[pathKey]))
	}
	return finalizeChildEnv(env, out), nil
}

// SanitizeSubprocessEnv filters managed secrets from a background, PTY, worker or script child environment
func SanitizeSubprocessEnv(env *Env, base, extra map[string]string) (map[string]string, error) {
	return scrubbedEnv(env, []envPart{{base, false}, {extra, true}}, pluginTerminalEnvStripKeys(), func(p string) string { return p })
}

// Env with the user's tools first on PATH for background commands
func terminalEnv(child map[string]string) map[string]string {
	if pathKey, ok := pathEnvKey(child); ok {
		child[pathKey] = userToolsFirst(child[pathKey])
	}
	return child
}

// IcebathSubprocessEnv sanitizes a non terminal child environment without skill passthrough
func IcebathSubprocessEnv(env *Env, inheritCredentials bool, base map[string]string) map[string]string {
	child := map[string]string{}
	if base != nil {
		for k, v := range base {
			child[k] = v
		}
	} else {
		child = environMap()
	}
	scrubCredentials(env, child, inheritCredentials)
	if _, ok := child["PYTHONUTF8"]; !ok {
		child["PYTHONUTF8"] = "1"
	}
	return finalizeChildEnv(env, child)
}

// Current process environment as a map
func environMap() map[string]string {
	out := map[string]string{}
	for _, kv := range os.Environ() {
		if i := strings.IndexByte(kv, '='); i > 0 {
			out[kv[:i]] = kv[i+1:]
		}
	}
	return out
}

// Strips tier one secrets always and tier two provider credentials unless inherited, in place
func scrubCredentials(env *Env, child map[string]string, inheritCredentials bool) {
	stripFolded := alwaysStripFolded()
	for k := range pluginTerminalEnvStripKeys() {
		stripFolded[strings.ToUpper(k)] = true
	}
	requireAdapterSecretSources()
	for k := range RegisteredAdapterSecretEnv(env) {
		stripFolded[strings.ToUpper(k)] = true
	}
	for key := range child {
		upper := strings.ToUpper(key)
		if stripFolded[upper] || (!inheritCredentials && IsProviderEnvBlocklisted(env, key)) ||
			strings.HasPrefix(key, providerEnvForcePrefix) || IsIcebathInternalSecret(key) {
			delete(child, key)
		}
	}
}

// SubprocessEnvOptions tunes BuildSubprocessEnv
type SubprocessEnvOptions struct {
	Base               map[string]string
	InheritProfileHome bool
	ScrubSecrets       bool
	Extra              map[string]string
	StripLaunchProfile bool
}

// BuildSubprocessEnv is the single factory for child process environments
func BuildSubprocessEnv(ctx context.Context, env *Env, o SubprocessEnvOptions) (map[string]string, error) {
	child := map[string]string{}
	if o.Base != nil {
		for k, v := range o.Base {
			child[k] = v
		}
	} else {
		child = environMap()
	}
	if o.StripLaunchProfile {
		StripLaunchProfileEnv(env, child, "")
		if isMultiplexActive(env) {
			for k, v := range profileSecrets().CurrentSecretScope(ctx) {
				child[k] = v
			}
			RestoreManagedEnv(child)
		}
	}
	if o.ScrubSecrets {
		return SanitizeSubprocessEnv(env, child, o.Extra)
	}
	if o.InheritProfileHome {
		applyProfileHome(env, child)
	}
	for k, v := range o.Extra {
		child[k] = v
	}
	StripSupervisedProcessIdentity(child)
	return delegatedChildSubprocessEnv(env, child), nil
}

// ServedProfileChildEnv builds the env of a child acting for the active or a routed profile
func ServedProfileChildEnv(ctx context.Context, env *Env, base map[string]string, targetHome string, inheritCredentials bool) (map[string]string, error) {
	child := map[string]string{}
	if base != nil {
		for k, v := range base {
			child[k] = v
		}
	} else {
		child = IcebathSubprocessEnv(env, inheritCredentials, nil)
	}
	target := targetHome
	if target == "" && env != nil && env.Home != home.Process() {
		target = env.Home
	}
	if target != "" {
		child["ICEBATH_HOME"] = target
		applyScratchTmpEnv(env, child)
		if isRoutedHome(target) {
			StripLaunchProfileEnv(env, child, target)
			scrubCredentials(env, child, false)
		}
	}
	if inheritCredentials {
		var secrets map[string]string
		if target != "" {
			secrets = profileSecrets().BuildProfileSecretScope(target)
		} else {
			secrets = profileSecrets().CurrentSecretScope(ctx)
			if secrets == nil && isMultiplexActive(env) {
				return nil, fmt.Errorf("%w: served_profile_child_env(inherit_credentials=True) called with no target home and no profile secret scope bound while multiplexing is on; the child would inherit the launch profile's credentials. Bind the profile scope (or pass target_home) at the spawn site.", ErrUnscopedSecret)
			}
		}
		for k, v := range secrets {
			child[k] = v
		}
	}
	return child, nil
}

// HostGatewayChildEnv is the env of the host gateway with the default profile's secrets
func HostGatewayChildEnv(ctx context.Context, env *Env, base map[string]string) (map[string]string, error) {
	return ServedProfileChildEnv(ctx, env, base, home.AccountDefault(), true)
}

// Reports whether targetHome is not the launch home of this process
func isRoutedHome(targetHome string) bool {
	left, err1 := filepath.EvalSymlinks(expandUser(targetHome))
	right, err2 := filepath.EvalSymlinks(home.Process())
	if err1 != nil || err2 != nil {
		return true
	}
	return normHomePath(left) != normHomePath(right)
}

// Keys of every non managed dotenv file loaded into this process environment
var launchDotenv = struct {
	sync.Mutex
	keys map[string]bool
}{keys: map[string]bool{}}

// RecordLaunchDotenvKeys remembers names a non managed dotenv load put into the process environment
func RecordLaunchDotenvKeys(keys []string) {
	launchDotenv.Lock()
	defer launchDotenv.Unlock()
	for _, k := range keys {
		launchDotenv.keys[k] = true
	}
}

// Names any non managed dotenv load published so far
func launchDotenvKeys() []string {
	launchDotenv.Lock()
	defer launchDotenv.Unlock()
	out := make([]string, 0, len(launchDotenv.keys))
	for k := range launchDotenv.keys {
		out = append(out, k)
	}
	return out
}

// StripLaunchProfileEnv drops the launch profile's residue from a child env built for a routed profile
func StripLaunchProfileEnv(env *Env, child map[string]string, targetHome string) map[string]string {
	target := targetHome
	if target == "" && env != nil && env.Home != home.Process() {
		target = env.Home
	}
	if target == "" || !isRoutedHome(target) {
		return child
	}
	launchHome := home.Process()
	managed := map[string]bool{}
	for _, k := range profileSecrets().ManagedDotenvKeys() {
		managed[strings.ToUpper(k)] = true
	}
	residue := map[string]bool{}
	consider := func(key string) {
		upper := strings.ToUpper(key)
		if (!IsGlobalEnv(upper) || strings.HasPrefix(upper, "TERMINAL_")) && !managed[upper] {
			residue[upper] = true
		}
	}
	for k := range config.LoadEnvFile(home.EnvPath(launchHome)) {
		consider(k)
	}
	for _, k := range launchDotenvKeys() {
		consider(k)
	}
	for _, v := range terminalConfigEnvMap {
		consider(v)
	}
	for _, k := range profileSecrets().SourceSuppliedNames() {
		consider(k)
	}
	for key := range child {
		if residue[strings.ToUpper(key)] {
			delete(child, key)
		}
	}
	return StripProfileGateEnv(child)
}

// RestoreManagedEnv re applies the administrator managed dotenv values over a routed child env
func RestoreManagedEnv(child map[string]string) map[string]string {
	for _, key := range profileSecrets().ManagedDotenvKeys() {
		if v, ok := os.LookupEnv(key); ok {
			child[key] = v
		}
	}
	return child
}

// Process identity and gate policy

// Active venv markers that must never leak into a child
var activeVenvMarkerVars = []string{"VIRTUAL_ENV", "CONDA_PREFIX", "PYTHONHOME"}

// Identity a supervisor hands to the one process it started
var supervisedProcessIdentityVars = []string{
	"ICEBATH_LAUNCHD_LABEL", "ICEBATH_GATEWAY_EXTERNAL_SUPERVISOR", "NOTIFY_SOCKET", "WATCHDOG_USEC", "WATCHDOG_PID",
}

// GatewayProfilePinEnv keeps a child on the gateway's profile after its supervisor markers are dropped
const GatewayProfilePinEnv = "ICEBATH_GATEWAY_PROFILE_PIN"

// CarryGatewayProfilePin marks the env pinned while it still holds a pinning supervisor marker
func CarryGatewayProfilePin(child map[string]string) map[string]string {
	if child["ICEBATH_SUPERVISED_CHILD"] != "" || truthyEnvValue(child["ICEBATH_GATEWAY_EXTERNAL_SUPERVISOR"]) {
		child[GatewayProfilePinEnv] = "1"
	}
	return child
}

// StripSupervisedProcessIdentity drops the supervisor identity vars in place keeping the profile pin
func StripSupervisedProcessIdentity(child map[string]string) map[string]string {
	CarryGatewayProfilePin(child)
	for _, name := range supervisedProcessIdentityVars {
		delete(child, name)
	}
	return child
}

// Gate shaped suffixes of platform authorization variables
var profileGateEnvMarkers = []string{
	"_ALLOWED_", "_ALLOW_ALL_", "_ALLOW_FROM", "_ALLOW_BOTS", "_ALLOW_PUBLIC_", "_IGNORED_CHANNELS",
	"_NO_THREAD_CHANNELS", "_FREE_RESPONSE_CHANNELS", "_BACKFILL_CHANNELS", "_GROUP_ALLOWED",
}

// Built in platform names owning authorization gates
var builtinPlatformNames = []string{
	"local", "telegram", "discord", "whatsapp", "whatsapp_cloud", "slack", "signal", "mattermost", "matrix",
	"email", "sms", "dingtalk", "api_server", "webhook", "msgraph_webhook", "feishu", "wecom", "wecom_callback",
	"weixin", "bluebubbles", "qqbot", "yuanbao", "relay",
}

// Gate owners that are not a platform value
var extraGateEnvPrefixes = []string{"GATEWAY", "QQ"}

// RegisteredPlatformNames lists plugin platforms loaded at runtime, installed by the gateway package
var RegisteredPlatformNames func() []string

// BundledPlatformNames lists bundled platform plugin directory names and aliases, installed by pkg/plugins
var BundledPlatformNames func() []string

// Upper cased env prefix of a platform name
func gatePrefixOf(name string) string {
	return strings.ReplaceAll(strings.ToUpper(name), "-", "_")
}

// Upper cased owners of authorization gates, static plus runtime registered adapters
func platformGateEnvPrefixes() map[string]bool {
	names := map[string]bool{}
	for _, n := range extraGateEnvPrefixes {
		names[n] = true
	}
	for _, n := range builtinPlatformNames {
		names[gatePrefixOf(n)] = true
	}
	if BundledPlatformNames != nil {
		for _, n := range BundledPlatformNames() {
			if n != "" {
				names[gatePrefixOf(n)] = true
			}
		}
	}
	if RegisteredPlatformNames != nil {
		for _, n := range RegisteredPlatformNames() {
			if n != "" {
				names[gatePrefixOf(n)] = true
			}
		}
	}
	return names
}

// IsProfileGateEnv reports whether a name is a platform authorization gate a routed child must not inherit
func IsProfileGateEnv(name string, prefixes map[string]bool) bool {
	upper := strings.ToUpper(name)
	if strings.HasPrefix(upper, "ICEBATH_") || strings.HasPrefix(upper, "HERMES_") || strings.HasPrefix(upper, "_") {
		return false
	}
	marked := false
	for _, m := range profileGateEnvMarkers {
		if strings.Contains(upper, m) {
			marked = true
			break
		}
	}
	if !marked {
		return false
	}
	if prefixes == nil {
		prefixes = platformGateEnvPrefixes()
	}
	for p := range prefixes {
		if strings.HasPrefix(upper, p+"_") {
			return true
		}
	}
	return false
}

// StripProfileGateEnv drops every authorization gate from the env in place
func StripProfileGateEnv(child map[string]string) map[string]string {
	prefixes := platformGateEnvPrefixes()
	for key := range child {
		if IsProfileGateEnv(key, prefixes) {
			delete(child, key)
		}
	}
	return child
}

// Credential keys owned by plugin registered terminal backends
func pluginTerminalEnvStripKeys() map[string]bool {
	keys := map[string]bool{}
	for _, name := range TerminalBackendNames() {
		p := GetTerminalBackend(name)
		if p == nil {
			continue
		}
		for _, k := range p.StripEnvKeys() {
			keys[k] = true
		}
	}
	return keys
}

// Tier one secrets stripped from every spawned subprocess even under inherited credentials
var alwaysStripKeysStatic = []string{
	"GH_TOKEN", "GITHUB_TOKEN", "GITHUB_APP_ID", "GITHUB_APP_PRIVATE_KEY_PATH",
	"GITHUB_APP_INSTALLATION_ID",
	"TELEGRAM_BOT_TOKEN", "DISCORD_BOT_TOKEN", "SLACK_BOT_TOKEN", "SLACK_APP_TOKEN",
	"SLACK_SIGNING_SECRET", "GATEWAY_ALLOWED_USERS", "GATEWAY_ALLOW_ALL_USERS",
	"GATEWAY_RELAY_ID", "GATEWAY_RELAY_SECRET", "GATEWAY_RELAY_DELIVERY_KEY",
	"EMAIL_PASSWORD", "ICEBATH_DASHBOARD_SESSION_TOKEN",
	"ICEBATH_DASHBOARD_BASIC_AUTH_PASSWORD", "ICEBATH_DASHBOARD_BASIC_AUTH_SECRET",
	"ICEBATH_DASHBOARD_OIDC_CLIENT_SECRET", "ICEBATH_DASHBOARD_DRAIN_SECRET",
	"MODAL_TOKEN_ID", "MODAL_TOKEN_SECRET", "DAYTONA_API_KEY",
}

// Left core names that are secrets rather than private settings
var leftCoreSecretEnv = []string{
	"HASS_TOKEN", "ICEBATH_LANGFUSE_SECRET_KEY", "ICEBATH_LANGFUSE_PUBLIC_KEY",
	"HERMES_LANGFUSE_SECRET_KEY", "HERMES_LANGFUSE_PUBLIC_KEY", "CAMOFOX_API_KEY", "COPILOT_GITHUB_TOKEN",
}

// Upper cased tier one strip set including every declared adapter secret
func alwaysStripFolded() map[string]bool {
	folded := map[string]bool{}
	for _, k := range alwaysStripKeysStatic {
		folded[strings.ToUpper(k)] = true
	}
	for _, k := range leftCoreSecretEnv {
		folded[strings.ToUpper(k)] = true
	}
	for k := range buildAdapterSecretEnv() {
		folded[strings.ToUpper(k)] = true
	}
	return folded
}

// Runtime owned PYTHONPATH stripping

// Removes PYTHONPATH entries owned by the running runtime root and then the venv marker vars
func stripIcebathOwnedPythonpathAndRuntimeMarkers(child map[string]string) {
	stripIcebathOwnedPythonpath(child)
	for _, m := range activeVenvMarkerVars {
		delete(child, m)
	}
}

// Spellings of the runtime root the launchers may emit
func icebathRepoRootAliases() []string {
	root := RunningSourceRoot()
	if root == "" {
		return nil
	}
	aliases := []string{root}
	if resolved, err := filepath.EvalSymlinks(root); err == nil && normHomePath(resolved) != normHomePath(root) {
		aliases = append(aliases, resolved)
	}
	return aliases
}

// Removes exact runtime root entries from PYTHONPATH keeping every user path byte for byte
func stripIcebathOwnedPythonpath(child map[string]string) {
	pp := child["PYTHONPATH"]
	if pp == "" {
		return
	}
	owned := icebathRepoRootAliases()
	entries := strings.Split(pp, string(os.PathListSeparator))
	var kept []string
	for _, e := range entries {
		strip := false
		if e != "" {
			for _, o := range owned {
				if normHomePath(e) == normHomePath(o) {
					strip = true
					break
				}
			}
		}
		if !strip {
			kept = append(kept, e)
		}
	}
	if len(kept) > 0 {
		child["PYTHONPATH"] = strings.Join(kept, string(os.PathListSeparator))
	} else {
		delete(child, "PYTHONPATH")
	}
}

// Shell discovery

// Resolves the bash binary commands run with
func findBash() (string, error) {
	if p, err := exec.LookPath("bash"); err == nil {
		return p, nil
	}
	if isWindowsHost {
		for _, c := range gitForWindowsBashCandidates() {
			if st, err := os.Stat(c); err == nil && !st.IsDir() {
				return c, nil
			}
		}
	}
	return "", fmt.Errorf("No shell found. icebath needs bash (Git for Windows on Windows). Install bash or Git for Windows and retry.")
}

// Well known Git for Windows bash locations
func gitForWindowsBashCandidates() []string {
	var out []string
	for _, root := range []string{os.Getenv("ProgramFiles"), os.Getenv("ProgramFiles(x86)"), os.Getenv("LocalAppData")} {
		if root == "" {
			continue
		}
		out = append(out, filepath.Join(root, "Git", "bin", "bash.exe"), filepath.Join(root, "Git", "usr", "bin", "bash.exe"),
			filepath.Join(root, "Programs", "Git", "bin", "bash.exe"))
	}
	return out
}

// Git Bash coreutils dirs in profile order, cached once
var gitBashBinDirsCache struct {
	sync.Once
	dirs []string
}

// Git Bash coreutils dirs on Windows, empty elsewhere
func gitBashBinDirs() []string {
	gitBashBinDirsCache.Do(func() {
		if isWindowsHost {
			gitBashBinDirsCache.dirs = computeGitBashBinDirs()
		}
	})
	return gitBashBinDirsCache.dirs
}

// Resolves mingw and usr bin dirs beside the bash binary
func computeGitBashBinDirs() []string {
	bash, err := findBash()
	if err != nil {
		return nil
	}
	parent := filepath.Dir(filepath.Dir(bash))
	root := parent
	if strings.ToLower(filepath.Base(parent)) == "usr" {
		root = filepath.Dir(parent)
	}
	seen := map[string]bool{}
	var dirs []string
	for _, sub := range []string{"mingw64/bin", "mingw32/bin", "usr/local/bin", "usr/bin", "bin"} {
		d := filepath.Join(root, filepath.FromSlash(sub))
		if st, err := os.Stat(d); err == nil && st.IsDir() && !seen[d] {
			seen[d] = true
			dirs = append(dirs, d)
		}
	}
	return dirs
}

// Prepends the dirs missing from the path keeping listed ones in place
func prependMissingPathEntries(existing string, dirs []string) string {
	sep := string(os.PathListSeparator)
	var entries []string
	present := map[string]bool{}
	for _, e := range strings.Split(existing, sep) {
		if e != "" {
			entries = append(entries, e)
			present[e] = true
		}
	}
	var missing []string
	for _, d := range dirs {
		if !present[d] {
			missing = append(missing, d)
		}
	}
	if len(missing) == 0 {
		return existing
	}
	return strings.Join(append(missing, entries...), sep)
}

// Path with Git Bash dirs first on Windows so coreutils beat System32 lookalikes
func userToolsFirst(existing string) string {
	bashDirs := gitBashBinDirs()
	if len(bashDirs) == 0 {
		return existing
	}
	keys := map[string]bool{}
	for _, d := range bashDirs {
		keys[normHomePath(d)] = true
	}
	sep := string(os.PathListSeparator)
	var rest []string
	for _, p := range strings.Split(existing, sep) {
		if p != "" && !keys[normHomePath(p)] {
			rest = append(rest, p)
		}
	}
	return strings.Join(append(append([]string{}, bashDirs...), rest...), sep)
}

// POSIX sh family shells that understand the background spawn invocation
var spawnCompatibleShells = map[string]bool{"bash": true, "zsh": true, "sh": true, "dash": true, "ksh": true, "mksh": true}

// FindShell is the user's login shell for background spawning when it is an sh family shell, else bash
func FindShell() (string, error) {
	userShell := ""
	if !isWindowsHost {
		userShell = os.Getenv("SHELL")
	}
	if userShell != "" && spawnCompatibleShells[filepath.Base(userShell)] {
		if st, err := os.Stat(userShell); err == nil && st.Mode().IsRegular() && st.Mode()&0o111 != 0 {
			return userShell, nil
		}
	}
	return findBash()
}

// PATH completion for the terminal subshell

// Standard PATH entries for environments with a minimal PATH
const sanePath = "/opt/homebrew/bin:/opt/homebrew/sbin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// Directory holding the icebath executable resolved once
var icebathBinDirCache struct {
	sync.Once
	dir string
}

// Directory holding the icebath executable, empty when none is found
func resolveIcebathBinDir() string {
	icebathBinDirCache.Do(func() {
		candidate := ""
		if which, err := exec.LookPath("icebath"); err == nil {
			candidate = filepath.Dir(which)
		} else if exe, err := os.Executable(); err == nil {
			base := strings.ToLower(filepath.Base(exe))
			if base == "icebath" || strings.HasPrefix(base, "icebath.") {
				candidate = filepath.Dir(exe)
			}
		}
		if st, err := os.Stat(candidate); candidate != "" && err == nil && st.IsDir() {
			icebathBinDirCache.dir = candidate
		}
	})
	return icebathBinDirCache.dir
}

// Prepends the icebath install dir when it is missing from the path
func prependIcebathBinDir(existing string) string {
	binDir := resolveIcebathBinDir()
	if binDir == "" {
		return prependMissingPathEntries(existing, nil)
	}
	return prependMissingPathEntries(existing, []string{binDir})
}

// Existing managed runtime dirs of the home for the subshell PATH
func managedRuntimePathEntries(env *Env) []string {
	bin := filepath.Join(envHome(env), "bin")
	if st, err := os.Stat(bin); err == nil && st.IsDir() {
		return []string{bin}
	}
	return nil
}

// The user's local bin when it exists
func userLocalBinEntries() []string {
	h, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	localBin := filepath.Join(h, ".local", "bin")
	if st, err := os.Stat(localBin); err == nil && st.IsDir() {
		return []string{localBin}
	}
	return nil
}

// Normalised POSIX PATH with empty entries dropped, duplicates collapsed and sane entries appended
func appendMissingSanePathEntries(env *Env, existing string) string {
	if isWindowsHost {
		return existing
	}
	var ordered []string
	seen := map[string]bool{}
	add := func(e string) {
		if e != "" && !seen[e] {
			seen[e] = true
			ordered = append(ordered, e)
		}
	}
	for _, e := range strings.Split(existing, ":") {
		add(e)
	}
	for _, e := range strings.Split(sanePath, ":") {
		add(e)
	}
	for _, e := range managedRuntimePathEntries(env) {
		add(e)
	}
	for _, e := range userLocalBinEntries() {
		add(e)
	}
	return strings.Join(ordered, ":")
}

// Disables MSYS argument path conversion on Windows unless the user set the knobs
func applyWindowsMsysBashEnvDefaults(child map[string]string) {
	if !isWindowsHost {
		return
	}
	if _, ok := child["MSYS_NO_PATHCONV"]; !ok {
		child["MSYS_NO_PATHCONV"] = "1"
	}
	if _, ok := child["MSYS2_ARG_CONV_EXCL"]; !ok {
		child["MSYS2_ARG_CONV_EXCL"] = "*"
	}
}

// PATH key of the env without altering Windows casing, false when a Windows env has none
func pathEnvKey(runEnv map[string]string) (string, bool) {
	if !isWindowsHost {
		return "PATH", true
	}
	for k := range runEnv {
		if strings.ToUpper(k) == "PATH" {
			return k, true
		}
	}
	return "", false
}

// SessionToolchainEnviron replaces the daemon's toolchain with the bound client's, installed by the agent package
var SessionToolchainEnviron func(env *Env, base map[string]string) map[string]string

// BotDesktopPublishedEnv is the running Bot Desktop seat env of the profile, installed by the desktop package
var BotDesktopPublishedEnv func(env *Env) map[string]string

// Run environment with a sane PATH, provider var stripping and the backend's own env on top
func makeRunEnv(env *Env, childEnv map[string]string) (map[string]string, error) {
	base := StripLaunchProfileEnv(env, environMap(), "")
	if SessionToolchainEnviron != nil {
		base = SessionToolchainEnviron(env, base)
	}
	for k, v := range childEnv {
		base[k] = v
	}
	runEnv, err := scrubbedEnv(env, []envPart{{base, true}}, map[string]bool{}, func(p string) string {
		return userToolsFirst(appendMissingSanePathEntries(env, p))
	})
	if err != nil {
		return nil, err
	}
	if BotDesktopPublishedEnv != nil {
		if published := BotDesktopPublishedEnv(env); len(published) > 0 {
			for k, v := range published {
				runEnv[k] = v
			}
			delete(runEnv, "WAYLAND_DISPLAY")
		}
	}
	return runEnv, nil
}

// Login shell init files

// Shell init files and the auto source bashrc flag from config
func readTerminalShellInitConfig(env *Env) ([]string, bool) {
	var files []string
	auto := true
	if terminal := cfgTree(env).Map("terminal"); terminal != nil {
		if raw, ok := terminal["shell_init_files"].([]any); ok {
			for _, f := range raw {
				if s, ok := f.(string); ok && s != "" {
					files = append(files, s)
				}
			}
		}
		if v, ok := terminal["auto_source_bashrc"]; ok && v != nil {
			auto = config.Truthy(v)
		}
	}
	return files, auto
}

// Files to source before the login shell snapshot, expanded and existing only
func resolveShellInitFiles(env *Env) []string {
	explicit, autoBashrc := readTerminalShellInitConfig(env)
	candidates := explicit
	if len(candidates) == 0 && autoBashrc && !isWindowsHost {
		candidates = []string{"~/.profile", "~/.bash_profile", "~/.bashrc"}
	}
	var resolved []string
	for _, raw := range candidates {
		p := expandPathVars(raw)
		if st, err := os.Stat(p); p != "" && err == nil && st.Mode().IsRegular() {
			resolved = append(resolved, p)
		}
	}
	return resolved
}

// Prepends guarded silent source lines for the init files
func prependShellInit(cmd string, files []string) string {
	if len(files) == 0 {
		return cmd
	}
	prelude := []string{"set +e"}
	for _, f := range files {
		safe := strings.ReplaceAll(f, "'", `'\''`)
		prelude = append(prelude, "[ -r '"+safe+"' ] && . '"+safe+"' 2>/dev/null || true")
	}
	return strings.Join(prelude, "\n") + "\n" + cmd
}

// Prepends a line re appending the parent PATH dirs a login profile dropped
func appendDroppedPathEntries(cmd, parentPath string) string {
	if isWindowsHost {
		return cmd
	}
	seen := map[string]bool{}
	var entries []string
	for _, d := range strings.Split(parentPath, ":") {
		if d == "" || seen[d] {
			continue
		}
		seen[d] = true
		if filepath.IsAbs(d) {
			if st, err := os.Stat(d); err != nil || !st.IsDir() {
				continue
			}
		}
		entries = append(entries, shQuote(d))
	}
	if len(entries) == 0 {
		return cmd
	}
	restore := "for __icebath_p in " + strings.Join(entries, " ") + "; do " +
		`case ":$PATH:" in *":$__icebath_p:"*) ;; *) PATH="${PATH:+$PATH:}$__icebath_p" ;; esac; ` +
		"done; unset __icebath_p; export PATH"
	return restore + "\n" + cmd
}

// Same host readings of a start time fingerprint may drift by this many centiseconds
const startTimeDriftTolerance = 200

// Reports whether two start time fingerprints name the same process incarnation
func startTimeFingerprintsMatch(recorded, current int64) bool {
	diff := current - recorded
	if diff < 0 {
		diff = -diff
	}
	return diff <= startTimeDriftTolerance
}

// Local environment

// localEnv runs commands directly on the host through a fresh bash per call with a session snapshot
type localEnv struct {
	baseEnv
}

// NewLocalEnvironment builds the host backend and captures its login shell snapshot
func NewLocalEnvironment(ctx context.Context, env *Env, cwd string, timeout int, childEnv map[string]string, persistent bool) *localEnv {
	l := &localEnv{}
	l.passwordlessSudoProbeSupported = true
	l.profileScopedPassthrough = true
	l.isLocal = true
	l.usesHostFilesystem = true
	if timeout <= 0 {
		timeout = 60
	}
	initBaseEnv(&l.baseEnv, l, env, resolveLocalInitialCwd(cwd), timeout, childEnv, persistent)
	l.initSession(ctx)
	return l
}

// EnvType names the backend
func (l *localEnv) EnvType() string { return "local" }

// First party names present in the env, excluded from the shared session snapshot
func (l *localEnv) additionalProfileScopedPassthroughNames() []string {
	merged := environMap()
	for k, v := range l.childEnv {
		merged[k] = v
	}
	var names []string
	for name := range merged {
		if matchesTerminalFirstPartyPrefix(name) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// TempDir is a shell safe writable temp dir, TERMINAL_TEMP_DIR then TMPDIR family then the home cache
func (l *localEnv) TempDir() string {
	lookup := func(key string) string {
		if v := l.childEnv[key]; v != "" {
			return v
		}
		return os.Getenv(key)
	}
	if isWindowsHost {
		for _, key := range []string{"TERMINAL_TEMP_DIR", "TMPDIR"} {
			c := lookup(key)
			if st, err := os.Stat(c); c != "" && filepath.IsAbs(c) && err == nil && st.IsDir() {
				return filepath.ToSlash(c)
			}
		}
		cacheDir := defaultTerminalTempDir(l.env)
		os.MkdirAll(cacheDir, 0o755)
		pruneTerminalTempOnce(l.env)
		return strings.ReplaceAll(cacheDir, `\`, "/")
	}
	posix := func(p string) string {
		if t := strings.TrimRight(p, "/"); t != "" {
			return t
		}
		return "/"
	}
	for _, key := range []string{"TERMINAL_TEMP_DIR", "TMPDIR", "TMP", "TEMP"} {
		c := lookup(key)
		if c == "" || !strings.HasPrefix(c, "/") {
			continue
		}
		if key == "TERMINAL_TEMP_DIR" {
			if st, err := os.Stat(c); err != nil || !st.IsDir() {
				continue
			}
		}
		return posix(c)
	}
	cacheDir := defaultTerminalTempDir(l.env)
	if err := os.MkdirAll(cacheDir, 0o755); err == nil && strings.HasPrefix(cacheDir, "/") && dirWritable(cacheDir) {
		pruneTerminalTempOnce(l.env)
		return posix(cacheDir)
	}
	fallback := os.TempDir()
	if !strings.HasPrefix(fallback, "/") {
		if abs, err := filepath.Abs(fallback); err == nil {
			fallback = abs
		}
	}
	return posix(fallback)
}

// Git Bash friendly cd target on Windows
func (l *localEnv) quoteCwdForCd(cwd string) string {
	return quoteCwdForCd(windowsToMsysPath(cwd))
}

// Rewrites native Windows paths before quoting for Git Bash
func (l *localEnv) quoteShellPath(p string) string {
	return quoteBashPath(p)
}

// Swaps the cwd for a usable directory when it vanished or became inaccessible
func (l *localEnv) recoverCwd() {
	current := l.Cwd()
	safe := resolveSafeCwd(l.env, current)
	if safe == current {
		return
	}
	if safe != msysToWindowsPath(current) {
		toolsLogger(l.env).Warn(fmt.Sprintf("LocalEnvironment cwd %q is missing on disk; falling back to %q so terminal commands keep working.", current, safe))
	}
	l.SetCwd(safe)
}

// Spawns bash in its own session with the scrubbed run env and the owner death supervisor covering its group
func (l *localEnv) runBash(ctx context.Context, cmd string, login bool, timeout int, stdinData *string) (ProcessHandle, error) {
	bash, err := findBash()
	if err != nil {
		return nil, err
	}
	runEnv, err := makeRunEnv(l.env, l.childEnv)
	if err != nil {
		return nil, err
	}
	if login {
		pathKey, _ := pathEnvKey(runEnv)
		cmd = appendDroppedPathEntries(cmd, runEnv[pathKey])
		cmd = prependShellInit(cmd, resolveShellInitFiles(l.env))
	}
	args := []string{}
	if login {
		args = append(args, "-l")
	}
	args = append(args, "-c", cmd)
	l.recoverCwd()
	p, err := popenBash(append([]string{bash}, args...), stdinData, func(c *exec.Cmd) {
		c.Env = envSlice(runEnv)
		c.Dir = l.Cwd()
		startNewSession(c)
	})
	if err != nil {
		return nil, err
	}
	if !isWindowsHost {
		pid := p.Pid()
		ownProcessGroups([]int{pid})
		p.ownedPgid = pid
		if pgid, err := processGroupOf(pid); err == nil {
			p.pgid = pgid
		}
		if start, ok := processStartTime(pid); ok {
			p.pgidStart = fmt.Sprint(start)
		}
	}
	return p, nil
}

// Base wait, then hands the command's group back from the owner death supervisor
func (l *localEnv) waitForProcess(ctx context.Context, p ProcessHandle, o waitOptions) *ExecResult {
	defer func() {
		if ep, ok := p.(*ExecProcess); ok && ep.ownedPgid != 0 {
			owned := ep.ownedPgid
			ep.ownedPgid = 0
			releaseProcessGroups([]int{owned})
		}
	}()
	return l.baseEnv.waitForProcess(ctx, p, o)
}

// Kills the entire process group of the command
func (l *localEnv) killProcess(p ProcessHandle) {
	ep, ok := p.(*ExecProcess)
	if !ok {
		p.Kill()
		return
	}
	if err := killLocalProcessTree(ep); err != nil {
		p.Kill()
	}
}

// SIGKILLs the whole group with no grace for a host that exits next
func (l *localEnv) forceKillProcess(p ProcessHandle) {
	ep, ok := p.(*ExecProcess)
	if !ok {
		p.Kill()
		return
	}
	forceKillLocalProcessTree(ep)
}

// Base semantics plus MSYS normalisation and a rollback when the reported cwd does not exist
func (l *localEnv) extractCwdFromOutput(result *ExecResult) {
	prev := l.Cwd()
	l.baseEnv.extractCwdFromOutput(result)
	if cur := l.Cwd(); cur != prev {
		normalized := msysToWindowsPath(cur)
		if st, err := os.Stat(normalized); normalized != "" && err == nil && st.IsDir() {
			l.SetCwd(normalized)
			result.Cwd = normalized
		} else {
			l.SetCwd(prev)
			result.CwdObserved = false
			result.Cwd = ""
		}
	}
}

// Cleanup removes the snapshot, cwd file and orphaned atomic write temps
func (l *localEnv) Cleanup(forceRemove bool) error {
	stale, _ := filepath.Glob(l.snapshotPath + ".tmp.*")
	for _, f := range append([]string{l.snapshotPath, l.cwdFile}, stale...) {
		os.Remove(f)
	}
	return nil
}

// Environment slice from a map with sorted keys
func envSlice(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+m[k])
	}
	return out
}

// Local backend provider

// localBackend describes the host backend for the registry
type localBackend struct{}

func (localBackend) Name() string        { return "local" }
func (localBackend) DisplayName() string { return "Local" }
func (localBackend) Description() string {
	return "Run commands directly on this machine with a persistent shell snapshot"
}
func (localBackend) EnvDescription() string                 { return "the local machine" }
func (localBackend) IsRemote() bool                         { return false }
func (localBackend) IsContainer() bool                      { return false }
func (localBackend) SkipContainerGuards() bool              { return false }
func (localBackend) CachePathBase() string                  { return "" }
func (localBackend) DefaultCwd() string                     { return "." }
func (localBackend) StripEnvKeys() []string                 { return nil }
func (localBackend) SessionIsolatedWhenNonpersistent() bool { return false }
func (localBackend) SupportsStreams() bool                  { return false }

// IsAvailable reports whether a bash binary can be found
func (localBackend) IsAvailable() bool {
	_, err := findBash()
	return err == nil
}

// CheckRequirements fails when no bash is available
func (localBackend) CheckRequirements(env *Env, cfg map[string]any) error {
	_, err := findBash()
	return err
}

// Probe reports the bash binary in use
func (localBackend) Probe() (string, string) {
	bash, err := findBash()
	if err != nil {
		return "unavailable", err.Error()
	}
	return "ok", bash
}

// SetupInstructions explains how to make the host backend usable
func (localBackend) SetupInstructions(env *Env) []string {
	return []string{"Install bash (Git for Windows on Windows) so it is on PATH."}
}

// DoctorChecks reports the bash lookup
func (localBackend) DoctorChecks() []DoctorCheck {
	bash, err := findBash()
	if err != nil {
		return []DoctorCheck{{OK: false, Label: "bash", Detail: err.Error()}}
	}
	return []DoctorCheck{{OK: true, Label: "bash", Detail: bash}}
}

// CreateEnvironment builds the host environment with terminal.persistent_shell honoured
func (localBackend) CreateEnvironment(ctx context.Context, env *Env, opts CreateOptions) (Environment, error) {
	persistent := true
	if v, ok := opts.ContainerConfig["persistent"]; ok && v != nil {
		persistent = config.Truthy(v)
	}
	return NewLocalEnvironment(ctx, env, opts.Cwd, opts.Timeout, nil, persistent), nil
}

func init() {
	registerBuiltinBackend(localBackend{})
}
