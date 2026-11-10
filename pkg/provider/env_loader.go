package provider

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf16"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/tools"
)

// One variable this process published from a dotenv file
type dotenvRecord struct {
	baseline    string
	hadBaseline bool
	value       string
	pass        int
}

// Options of one full dotenv load
type DotenvLoadOptions struct {
	Home                string
	ProjectEnv          string
	SkipExternalSecrets bool
}

var (
	envLoaderLog = slog.Default()

	envLoaderWarnMu       sync.Mutex
	warnedUTF32Paths      = map[string]bool{}
	scopedSkipLogged      = map[string]bool{}
	warnedUnreadablePaths = map[string]bool{}

	dotenvMu        sync.Mutex
	dotenvPublished = map[string]dotenvRecord{}
	dotenvPasses    int

	spawnCredentialKeys = map[string]bool{"ICEBATH_DASHBOARD_SESSION_TOKEN": true}

	// Seams for the updater skip, the external source pass and the terminal bridge, replaced in tests
	skipExternalSecretSourcesFn  = pm.ShouldSkipExternalSecretSources
	applyExternalSecretSourcesFn = ApplyExternalSecretSources
	terminalConfigBridgeFn       = applyTerminalConfigToEnv
)

func init() {
	config.RegisterConfigBootstrapEnv(applyConfigBootstrapEnv)
}

// Routing keys scrubbed at load when the profile .env does not define them
func profileManagedEnvKeys() []string {
	return append([]string{"ICEBATH_ACP_AUTH_METHOD", "ICEBATH_ACP_AUTO_APPROVE"}, leftCoreProfileEnv()...)
}

// Names a dotenv file assigns through the profile scope tokenizer, empty keys included
func envKeysDefinedInDotenv(path string) map[string]bool {
	out := map[string]bool{}
	for k := range config.LoadEnvFile(path) {
		out[k] = true
	}
	return out
}

// Removes inherited routing keys the existing .env leaves undefined
func clearKnownKeysMissingFromDotenv(path string) {
	if _, err := os.Stat(path); err != nil {
		return
	}
	defined := envKeysDefinedInDotenv(path)
	for _, key := range profileManagedEnvKeys() {
		if _, ok := os.LookupEnv(key); ok && !defined[key] {
			_ = os.Unsetenv(key)
		}
	}
}

// Values dotenv loads last wrote to the name matched case insensitively
func DotenvPublishedValues(name string) []string {
	folded := strings.ToUpper(name)
	dotenvMu.Lock()
	defer dotenvMu.Unlock()
	seen := map[string]bool{}
	var out []string
	for key, rec := range dotenvPublished {
		if strings.ToUpper(key) == folded && !seen[rec.value] {
			seen[rec.value] = true
			out = append(out, rec.value)
		}
	}
	return out
}

// Names the administrator managed .env loaded into the process
func ManagedDotenvKeys() []string {
	secretSourceCacheMu.Lock()
	defer secretSourceCacheMu.Unlock()
	out := make([]string, 0, len(managedDotenvKeys))
	for k := range managedDotenvKeys {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Uninterpolated assignments of one dotenv file in file order
func parseDotenvFile(path string) ([]dotenvAssignment, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parseDotenvText(config.DecodeEnvBytes(raw)), nil
}

// Process environment with values other passes published peeled back to their baselines, caller holds dotenvMu
func dotenvLookupEnv(loadPass int, anyPass bool) map[string]string {
	env := ProcessSourceEnvironment().Snapshot()
	for name, rec := range dotenvPublished {
		cur, ok := env[name]
		if (anyPass || rec.pass != loadPass) && ok && cur == rec.value {
			if rec.hadBaseline {
				env[name] = rec.baseline
			} else {
				delete(env, name)
			}
		}
	}
	return env
}

// Expands variable references line by line, earlier lines winning under override and the environment otherwise
func resolveDotenvAssignments(assignments []dotenvAssignment, lookupEnv map[string]string, override bool) []dotenvAssignment {
	var out []dotenvAssignment
	index := map[string]int{}
	lookup := func(name string) (string, bool) {
		if !override {
			if v, ok := lookupEnv[name]; ok {
				return v, true
			}
		}
		if i, ok := index[name]; ok {
			return out[i].value, true
		}
		v, ok := lookupEnv[name]
		return v, ok
	}
	for _, a := range assignments {
		if a.set {
			a.value = expandDotenvVariables(a.value, lookup)
		}
		if i, ok := index[a.name]; ok {
			out[i] = a
			continue
		}
		index[a.name] = len(out)
		out = append(out, a)
	}
	return out
}

// What a load would publish from the home .env then the managed .env, read only and expanded against the process env
func resolveHomeDotenv(h string) map[string]string {
	layers := []string{filepath.Join(h, ".env")}
	if dir := config.ManagedDir(); dir != "" {
		layers = append(layers, filepath.Join(dir, ".env"))
	}
	dotenvMu.Lock()
	lookupEnv := dotenvLookupEnv(0, true)
	dotenvMu.Unlock()
	out := map[string]string{}
	for _, path := range layers {
		assignments, err := parseDotenvFile(path)
		if err != nil {
			continue
		}
		for _, a := range resolveDotenvAssignments(assignments, lookupEnv, true) {
			if a.set {
				lookupEnv[a.name] = a.value
				out[a.name] = a.value
			}
		}
	}
	return out
}

// Exported form of the read only home dotenv resolution
func ResolveHomeDotenv(h string) map[string]string { return resolveHomeDotenv(h) }

func nextDotenvPassLocked() int {
	p := dotenvPasses
	dotenvPasses++
	return p
}

// Loads one dotenv file into the process env with variables resolved against pre publish baselines
func loadDotenvWithFallback(path string, override bool, loadPass int, managed bool) error {
	assignments, err := parseDotenvFile(path)
	if err != nil {
		return err
	}
	dotenvMu.Lock()
	if loadPass < 0 {
		loadPass = nextDotenvPassLocked()
	}
	for _, a := range resolveDotenvAssignments(assignments, dotenvLookupEnv(loadPass, false), override) {
		if !a.set {
			continue
		}
		current, present := os.LookupEnv(a.name)
		if !override && present {
			continue
		}
		rec, recorded := dotenvPublished[a.name]
		ours := recorded && present && current == rec.value
		if spawnCredentialKeys[a.name] && current != "" && !ours {
			continue
		}
		next := dotenvRecord{baseline: current, hadBaseline: present, value: a.value, pass: loadPass}
		if ours {
			next.baseline, next.hadBaseline = rec.baseline, rec.hadBaseline
		}
		if err := os.Setenv(a.name, a.value); err != nil {
			dotenvMu.Unlock()
			return err
		}
		dotenvPublished[a.name] = next
	}
	dotenvMu.Unlock()
	names := make([]string, 0, len(assignments))
	for _, a := range assignments {
		names = append(names, a.name)
	}
	if managed {
		RecordManagedDotenvKeys(names)
	} else {
		tools.RecordLaunchDotenvKeys(names)
	}
	SanitizeLoadedCredentials()
	return nil
}

// Loads one dotenv file with override as its own pass, the direct reload path
func LoadDotenvFile(path string, override bool) error {
	return loadDotenvWithFallback(path, override, -1, false)
}

// Assignments of one dotenv file as the loader sees it after the sanitizer, UTF-16 decoded and NULs dropped
func dotenvAssignments(path string) ([]dotenvAssignment, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var text string
	if hasUTF16BOM(raw) {
		decoded, ok := decodeUTF16Strict(raw)
		if ok {
			text = translateUniversalNewlines(decoded)
		} else {
			text = latin1(raw)
		}
	} else {
		text = config.DecodeEnvBytes(raw)
	}
	return parseDotenvText(strings.ReplaceAll(text, "\x00", "")), nil
}

func hasUTF16BOM(raw []byte) bool {
	return len(raw) >= 2 && ((raw[0] == 0xFF && raw[1] == 0xFE) || (raw[0] == 0xFE && raw[1] == 0xFF))
}

func hasUTF32BOM(raw []byte) bool {
	return len(raw) >= 4 && ((raw[0] == 0xFF && raw[1] == 0xFE && raw[2] == 0 && raw[3] == 0) ||
		(raw[0] == 0 && raw[1] == 0 && raw[2] == 0xFE && raw[3] == 0xFF))
}

// Text of BOM led UTF-16 bytes, false on a truncated unit or an unpaired surrogate
func decodeUTF16Strict(raw []byte) (string, bool) {
	big := raw[0] == 0xFE
	body := raw[2:]
	if len(body)%2 != 0 {
		return "", false
	}
	units := make([]uint16, len(body)/2)
	for i := range units {
		if big {
			units[i] = uint16(body[2*i])<<8 | uint16(body[2*i+1])
		} else {
			units[i] = uint16(body[2*i+1])<<8 | uint16(body[2*i])
		}
	}
	for i := 0; i < len(units); i++ {
		u := units[i]
		if u >= 0xD800 && u <= 0xDBFF {
			if i+1 >= len(units) || units[i+1] < 0xDC00 || units[i+1] > 0xDFFF {
				return "", false
			}
			i++
		} else if u >= 0xDC00 && u <= 0xDFFF {
			return "", false
		}
	}
	return string(utf16.Decode(units)), true
}

func latin1(raw []byte) string {
	runes := make([]rune, len(raw))
	for i, b := range raw {
		runes[i] = rune(b)
	}
	return string(runes)
}

// Text with CRLF and lone CR line endings read as LF like Python universal newlines
func translateUniversalNewlines(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
}

// Lines of text after universal newline translation, each keeping its newline
func universalReadlines(s string) []string {
	parts := strings.SplitAfter(translateUniversalNewlines(s), "\n")
	if len(parts) > 0 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	return parts
}

// Process env as a full load leaves it for the names the home, op, project and managed dotenv layers define
func bootstrapEnv(h, projectEnv string) map[string]string {
	if h == "" {
		h = RoutingProcessHome()
	}
	env := ProcessSourceEnvironment().Snapshot()
	type layer struct {
		path     string
		override bool
	}
	userEnv := filepath.Join(h, ".env")
	layers := []layer{{userEnv, true}}
	if os.Getenv("OP_SERVICE_ACCOUNT_TOKEN") == "" {
		layers = append(layers, layer{filepath.Join(h, ".op.env"), false})
	}
	if projectEnv != "" {
		_, err := os.Stat(userEnv)
		layers = append(layers, layer{projectEnv, err != nil})
	}
	if dir := config.ManagedDir(); dir != "" {
		layers = append(layers, layer{filepath.Join(dir, ".env"), true})
	}
	for _, l := range layers {
		assignments, err := dotenvAssignments(l.path)
		if err != nil {
			continue
		}
		for _, a := range resolveDotenvAssignments(assignments, env, l.override) {
			if !a.set {
				continue
			}
			if _, present := env[a.name]; l.override || !present {
				env[a.name] = a.value
			}
		}
	}
	return env
}

// Config backend kind a load would select for the home, decided without loading or booting anything
func SelectedConfigBackendName(h string) string {
	if kind := strings.ToLower(strings.TrimSpace(bootstrapEnv(h, "")[config.BackendEnv])); kind != "" {
		return kind
	}
	return "file"
}

// Publishes the selected backend's deployment names from the dotenv layers with the values a full load gives them
func applyConfigBootstrapEnv(_ context.Context, backendNames func(kind string) []string, h, projectEnv string) error {
	if workerConfigSnapshotActive() {
		return nil
	}
	env := bootstrapEnv(h, projectEnv)
	kind := strings.ToLower(strings.TrimSpace(env[config.BackendEnv]))
	if kind == "" {
		kind = "file"
	}
	for _, name := range append([]string{config.BackendEnv}, backendNames(kind)...) {
		value, ok := env[name]
		if !ok {
			continue
		}
		if cur, present := os.LookupEnv(name); !present || cur != value {
			_ = os.Setenv(name, value)
		}
	}
	return nil
}

// Rewrites a UTF-16 or untidy .env as clean UTF-8 before the load, refusing UTF-32 and undecodable heads
func sanitizeEnvFileIfNeeded(path string) {
	if _, err := os.Stat(path); err != nil {
		return
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	forceRewrite := false
	var original []string
	switch {
	case hasUTF32BOM(raw):
		key := homeKey(path)
		envLoaderWarnMu.Lock()
		first := !warnedUTF32Paths[key]
		warnedUTF32Paths[key] = true
		envLoaderWarnMu.Unlock()
		if first {
			envLoaderLog.Warn(fmt.Sprintf("Skipping .env sanitize for %s: UTF-32 BOM detected; leaving file untouched to avoid corruption", path))
		}
		return
	case hasUTF16BOM(raw):
		text, ok := decodeUTF16Strict(raw)
		if !ok {
			return
		}
		original = universalReadlines(text)
		forceRewrite = true
	default:
		original = universalReadlines(decodeUTF8Replace(raw))
		if len(original) > 0 && strings.HasPrefix(original[0], "\ufffd") {
			return
		}
	}
	stripped := make([]string, len(original))
	for i, line := range original {
		stripped[i] = strings.ReplaceAll(line, "\x00", "")
	}
	sanitized := config.SanitizeEnvLines(stripped)
	if !forceRewrite && equalLines(sanitized, original) {
		return
	}
	content := strings.Join(sanitized, "")
	if runtime.GOOS == "windows" {
		content = strings.ReplaceAll(content, "\n", "\r\n")
	}
	_ = writeBeside(path, []byte(content))
}

func equalLines(a, b []string) bool {
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

// Text of UTF-8 bytes with an optional BOM, each maximal invalid subpart replaced like errors replace
func decodeUTF8Replace(raw []byte) string {
	if len(raw) >= 3 && raw[0] == 0xEF && raw[1] == 0xBB && raw[2] == 0xBF {
		raw = raw[3:]
	}
	var sb strings.Builder
	for i := 0; i < len(raw); {
		r, size := decodeRuneAt(raw[i:])
		if r < 0 {
			sb.WriteRune('\ufffd')
			i += size
			continue
		}
		sb.Write(raw[i : i+size])
		i += size
	}
	return sb.String()
}

// Next rune of b, negative with the maximal invalid subpart length when b does not start a valid sequence
func decodeRuneAt(b []byte) (rune, int) {
	c := b[0]
	if c < 0x80 {
		return rune(c), 1
	}
	lo, hi, need := byte(0x80), byte(0xBF), 0
	switch {
	case c >= 0xC2 && c <= 0xDF:
		need = 1
	case c == 0xE0:
		lo, need = 0xA0, 2
	case c >= 0xE1 && c <= 0xEC, c == 0xEE, c == 0xEF:
		need = 2
	case c == 0xED:
		hi, need = 0x9F, 2
	case c == 0xF0:
		lo, need = 0x90, 3
	case c >= 0xF1 && c <= 0xF3:
		need = 3
	case c == 0xF4:
		hi, need = 0x8F, 3
	default:
		return -1, 1
	}
	n := 1
	for k := 0; k < need; k++ {
		if n >= len(b) || b[n] < lo || b[n] > hi {
			return -1, n
		}
		lo, hi = 0x80, 0xBF
		n++
	}
	return 0, n
}

// Publishes data onto path through a fsynced temp staged beside the resolved target and an atomic replace
func writeBeside(path string, data []byte) error {
	real := publishPath(path)
	linkDir := filepath.Dir(path)
	stageDir := filepath.Dir(real)
	tmp, err := os.CreateTemp(stageDir, ".env_*.tmp")
	if err != nil && errors.Is(err, fs.ErrPermission) && stageDir != linkDir {
		tmp, err = os.CreateTemp(linkDir, ".env_*.tmp")
	}
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := atomicReplaceFile(name, real); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}

// Rename target, a symlink's real file and the path itself otherwise
func publishPath(target string) string {
	st, err := os.Lstat(target)
	if err != nil || st.Mode()&os.ModeSymlink == 0 {
		return target
	}
	if resolved, err := filepath.EvalSymlinks(target); err == nil {
		return resolved
	}
	return target
}

func isCrossDeviceReplaceError(err error) bool {
	return errors.Is(err, syscall.EXDEV) || errors.Is(err, syscall.EBUSY)
}

// Windows sharing, lock or access refusal on a rename another handle holds
func isContendedReplaceError(err error) bool {
	var errno syscall.Errno
	if runtime.GOOS != "windows" || !errors.As(err, &errno) {
		return false
	}
	return errno == 5 || errno == 32 || errno == 33
}

// Renames tmp onto target, copying across devices and rewriting in place after a contended Windows rename
func atomicReplaceFile(tmp, target string) error {
	err := os.Rename(tmp, target)
	if err == nil {
		return nil
	}
	contended := isContendedReplaceError(err)
	if !isCrossDeviceReplaceError(err) && !contended {
		return err
	}
	if contended {
		for attempt := 1; attempt <= 4; attempt++ {
			time.Sleep(JitteredBackoff(attempt, BackoffOptions{BaseDelay: 20 * time.Millisecond, MaxDelay: 100 * time.Millisecond, JitterRatio: 0.5}))
			retryErr := os.Rename(tmp, target)
			if retryErr == nil {
				return nil
			}
			if isCrossDeviceReplaceError(retryErr) {
				contended = false
				break
			}
			if !isContendedReplaceError(retryErr) {
				return retryErr
			}
		}
	}
	data, err := os.ReadFile(tmp)
	if err != nil {
		return err
	}
	flags := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	if contended {
		flags = os.O_WRONLY
	}
	f, err := os.OpenFile(target, flags, 0o666)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Truncate(int64(len(data))); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if !contended {
		if st, err := os.Stat(tmp); err == nil {
			_ = os.Chmod(target, st.Mode().Perm())
		}
	}
	return os.Remove(tmp)
}

// Reports whether the path exists but this process may not read it
func DotenvUnreadable(path string) bool {
	f, err := os.Open(path)
	if err == nil {
		f.Close()
		return false
	}
	return errors.Is(err, fs.ErrPermission)
}

// Loads the profile .env, false when it is unreadable and managed scope pins the terminal backend
func loadUserDotenv(path string, loadPass int) (bool, error) {
	err := loadDotenvWithFallback(path, true, loadPass, false)
	if err == nil {
		return true, nil
	}
	if !errors.Is(err, fs.ErrPermission) || config.PinnedTerminalBackend() == "" {
		return false, err
	}
	envLoaderWarnMu.Lock()
	first := !warnedUnreadablePaths[path]
	warnedUnreadablePaths[path] = true
	envLoaderWarnMu.Unlock()
	if first {
		envLoaderLog.Warn(fmt.Sprintf("Cannot read %s; managed scope pins terminal.backend, so it loads as absent", path))
	}
	return false, nil
}

// Loads the home .env over shell exports, the op and project files filling gaps and the managed .env last
func LoadIcebathDotenv(ctx context.Context, opts DotenvLoadOptions) ([]string, error) {
	if workerConfigSnapshotActive() {
		return nil, nil
	}
	homePath := opts.Home
	if homePath == "" {
		homePath = home.Process()
	}
	launchHome := homeKey(RoutingProcessHome())
	if IsMultiplexActive(ctx) && home.Override(ctx) != "" &&
		(homeKey(homePath) != launchHome || homeKey(home.Dir(ctx)) != launchHome) {
		key := homeKey(homePath)
		envLoaderWarnMu.Lock()
		first := !scopedSkipLogged[key]
		scopedSkipLogged[key] = true
		envLoaderWarnMu.Unlock()
		if first {
			envLoaderLog.Debug(fmt.Sprintf("multiplex: skipping process-global dotenv load for routed profile home %s (credentials resolve via the profile scope)", homePath))
		}
		if !opts.SkipExternalSecrets && !skipExternalSecretSourcesFn() {
			HydrateProfileSecretSources(homePath)
		}
		return nil, nil
	}
	var loaded []string
	userEnv := filepath.Join(homePath, ".env")
	dotenvMu.Lock()
	loadPass := nextDotenvPassLocked()
	dotenvMu.Unlock()
	if err := config.BootstrapDeployment(ctx, homePath, opts.ProjectEnv); err != nil {
		return nil, err
	}
	if fileExists(userEnv) {
		sanitizeEnvFileIfNeeded(userEnv)
	}
	if opts.ProjectEnv != "" && fileExists(opts.ProjectEnv) {
		sanitizeEnvFileIfNeeded(opts.ProjectEnv)
	}
	if fileExists(userEnv) {
		ok, err := loadUserDotenv(userEnv, loadPass)
		if err != nil {
			return nil, err
		}
		if ok {
			loaded = append(loaded, userEnv)
			clearKnownKeysMissingFromDotenv(userEnv)
		}
	}
	opEnv := filepath.Join(homePath, ".op.env")
	if fileExists(opEnv) && os.Getenv("OP_SERVICE_ACCOUNT_TOKEN") == "" {
		if err := loadDotenvWithFallback(opEnv, false, loadPass, false); err != nil {
			return nil, err
		}
	}
	if opts.ProjectEnv != "" && fileExists(opts.ProjectEnv) {
		if err := loadDotenvWithFallback(opts.ProjectEnv, !fileExists(userEnv), loadPass, false); err != nil {
			return nil, err
		}
		loaded = append(loaded, opts.ProjectEnv)
	}
	for name, value := range SecretSourceRestoreValues(homePath) {
		if cur, ok := os.LookupEnv(name); !ok || cur != value {
			_ = os.Setenv(name, value)
		}
	}
	if err := config.BootstrapDeployment(ctx, homePath, opts.ProjectEnv); err != nil {
		return nil, err
	}
	backend, err := config.GetConfigBackend(ctx)
	if err != nil {
		return nil, err
	}
	if err := backend.Boot(ctx, homePath); err != nil {
		return nil, err
	}
	if !opts.SkipExternalSecrets && !skipExternalSecretSourcesFn() {
		applyExternalSecretSourcesFn(homePath)
	}
	if err := applyManagedEnv(loadPass); err != nil {
		return nil, err
	}
	reapplyTerminalConfigBridge(ctx, homePath)
	return loaded, nil
}

// Re-asserts the config terminal keys over the reloaded env when the load targets the process home
func reapplyTerminalConfigBridge(ctx context.Context, homePath string) {
	if homeKey(homePath) != homeKey(RoutingProcessHome()) {
		return
	}
	_ = terminalConfigBridgeFn(ctx)
}

// Loads the administrator managed .env last with override, recording its names as policy
func applyManagedEnv(loadPass int) error {
	dir := config.ManagedDir()
	if dir == "" {
		return nil
	}
	managedEnv := filepath.Join(dir, ".env")
	if !fileExists(managedEnv) {
		return nil
	}
	sanitizeEnvFileIfNeeded(managedEnv)
	return loadDotenvWithFallback(managedEnv, true, loadPass, true)
}
