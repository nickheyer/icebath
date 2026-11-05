package provider

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"unicode"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

// Credential suffixes sanitized on load because they become HTTP header values
var credentialEnvSuffixes = []string{"_API_KEY", "_TOKEN", "_SECRET", "_KEY"}

type secretSourceWrite struct {
	source string
	value  string
	prior  string
	had    bool
}

var (
	secretSourceCacheMu  sync.Mutex
	secretSourceOfVar    = map[string]string{}
	sourceSuppliedNames  = map[string]bool{}
	secretValuesByHome   = map[string]map[string]string{}
	secretRestoreByHome  = map[string]map[string]string{}
	secretWritesByHome   = map[string]map[string]secretSourceWrite{}
	appliedSecretHomes   = map[string]bool{}
	managedDotenvKeys    = map[string]bool{}
	warnedCredentialKeys = map[string]bool{}
	// Writes secret source status lines, replaced in tests
	secretStatusPrint = func(line string) { fmt.Fprintln(os.Stderr, line) }
)

func homeKey(h string) string {
	resolved, err := filepath.Abs(h)
	if err != nil {
		resolved = h
	}
	if real, err := filepath.EvalSymlinks(resolved); err == nil {
		resolved = real
	}
	return resolved
}

// SecretSourceOf is the source label that supplied the env var, empty for .env and shell keys
func SecretSourceOf(h, envVar string) string {
	secretSourceCacheMu.Lock()
	defer secretSourceCacheMu.Unlock()
	return secretSourceOfVar[envVar]
}

// RecordManagedDotenvKeys remembers the keys the administrator managed .env loaded
func RecordManagedDotenvKeys(keys []string) {
	secretSourceCacheMu.Lock()
	defer secretSourceCacheMu.Unlock()
	for _, k := range keys {
		managedDotenvKeys[k] = true
	}
}

// Every name the report's sources supplied, applied or lost to an existing value, lock held
func recordSuppliedNames(report *ApplyReport) map[string]bool {
	supplied := map[string]bool{}
	for n := range report.Provenance {
		supplied[n] = true
	}
	for _, src := range report.Sources {
		for _, n := range src.SkippedExisting {
			supplied[n] = true
		}
	}
	for n := range supplied {
		sourceSuppliedNames[n] = true
	}
	return supplied
}

// SecretSourceNames are the env var names some profile's external source applied
func SecretSourceNames() []string {
	secretSourceCacheMu.Lock()
	defer secretSourceCacheMu.Unlock()
	out := make([]string, 0, len(secretSourceOfVar))
	for n := range secretSourceOfVar {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// SourceSuppliedNames are the names an external source supplied for any home, applied or skipped
func SourceSuppliedNames() []string {
	secretSourceCacheMu.Lock()
	defer secretSourceCacheMu.Unlock()
	set := map[string]bool{}
	for n := range secretSourceOfVar {
		set[n] = true
	}
	for n := range sourceSuppliedNames {
		set[n] = true
	}
	out := make([]string, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// SecretSourceValues is the external secret value snapshot for a home
func SecretSourceValues(h string) map[string]string {
	secretSourceCacheMu.Lock()
	defer secretSourceCacheMu.Unlock()
	out := map[string]string{}
	for k, v := range secretValuesByHome[homeKey(h)] {
		out[k] = v
	}
	return out
}

// SecretSourceRestoreValues is the subset of a home's snapshot a dotenv reload may re-assert
func SecretSourceRestoreValues(h string) map[string]string {
	secretSourceCacheMu.Lock()
	defer secretSourceCacheMu.Unlock()
	out := map[string]string{}
	for k, v := range secretRestoreByHome[homeKey(h)] {
		out[k] = v
	}
	return out
}

// Secrets section of config.yaml through the cached reader, an unavailable backend panics like SystemExit
func loadSecretsConfig(h string) (map[string]any, error) {
	ctx := context.Background()
	panicOnSystemExit := func(err error) {
		var exit config.SystemExitError
		if errors.As(err, &exit) {
			panic(err)
		}
	}
	path := home.ConfigPath(h)
	exists, err := config.ConfigExists(ctx, path)
	if err != nil {
		panicOnSystemExit(err)
		return nil, err
	}
	if !exists {
		return map[string]any{}, nil
	}
	secrets := func(doc any) map[string]any {
		if section, _ := config.AsMap(doc)["secrets"].(map[string]any); section != nil {
			return section
		}
		return map[string]any{}
	}
	if filepath.Clean(h) == filepath.Clean(RoutingProcessHome()) {
		raw, rerr := config.ReadRawConfig(ctx, h)
		if rerr == nil {
			return secrets(raw), nil
		}
		panicOnSystemExit(rerr)
	}
	doc, err := config.ReadConfigDocReadonly(ctx, path)
	if err != nil {
		panicOnSystemExit(err)
		return map[string]any{}, nil
	}
	return secrets(doc), nil
}

// HydrateProfileSecretSources resolves one profile's sources into a private mapping without touching the process env
func HydrateProfileSecretSources(h string) map[string]string {
	secretSourceCacheMu.Lock()
	defer secretSourceCacheMu.Unlock()
	return hydrateProfileSecretSourcesLocked(h)
}

// Reports whether every external secret source of a home applied, the exported face for pkg/mcp
func SecretHomeApplied(h string) bool {
	secretSourceCacheMu.Lock()
	defer secretSourceCacheMu.Unlock()
	return appliedSecretHomes[homeKey(h)]
}

func hydrateProfileSecretSourcesLocked(h string) map[string]string {
	if workerConfigSnapshotActive() {
		return map[string]string{}
	}
	key := homeKey(h)
	if appliedSecretHomes[key] {
		out := map[string]string{}
		for k, v := range secretValuesByHome[key] {
			out[k] = v
		}
		return out
	}
	delete(secretValuesByHome, key)
	delete(secretRestoreByHome, key)
	cfg, err := loadSecretsConfig(h)
	if err != nil || len(cfg) == 0 {
		return map[string]string{}
	}
	local := map[string]string{}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok && IsGlobalEnv(k) {
			local[k] = v
		}
	}
	for k, v := range config.LoadEnvFile(filepath.Join(h, ".env")) {
		local[k] = v
	}
	if opEnv := filepath.Join(h, ".op.env"); fileExists(opEnv) {
		for k, v := range config.LoadEnvFile(opEnv) {
			if _, has := local[k]; !has {
				local[k] = v
			}
		}
	}
	local["ICEBATH_HOME"] = h
	envBefore := map[string]string{}
	for k, v := range local {
		envBefore[k] = v
	}
	environ := MappingSourceEnvironment(local)
	report := ApplyAllSecretSources(cfg, h, environ)
	if len(report.Sources) == 0 {
		return map[string]string{}
	}
	refuseProtectedEnvFromSources(report, configBackendView(h), envBefore, environ, h)
	allOK := true
	for _, src := range report.Sources {
		if !src.Result.OK() {
			allOK = false
		}
	}
	if allOK {
		appliedSecretHomes[key] = true
	}
	recordSuppliedNames(report)
	values := map[string]string{}
	for name, applied := range report.Provenance {
		value, ok := local[name]
		if !ok {
			continue
		}
		secretSourceOfVar[name] = applied.Source
		values[name] = value
	}
	secretValuesByHome[key] = values
	out := map[string]string{}
	for k, v := range values {
		out[k] = v
	}
	return out
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// ResetSecretSourceCache forgets applied homes so the next load re-pulls, one home or all
func ResetSecretSourceCache(h string) {
	secretSourceCacheMu.Lock()
	defer secretSourceCacheMu.Unlock()
	if h == "" {
		appliedSecretHomes = map[string]bool{}
		secretSourceOfVar = map[string]string{}
		sourceSuppliedNames = map[string]bool{}
		secretValuesByHome = map[string]map[string]string{}
		secretRestoreByHome = map[string]map[string]string{}
		secretWritesByHome = map[string]map[string]secretSourceWrite{}
		return
	}
	key := homeKey(h)
	delete(appliedSecretHomes, key)
	delete(secretValuesByHome, key)
	delete(secretRestoreByHome, key)
}

// FormatSecretSourceSuffix is the from X suffix naming the external source, empty for .env and shell keys
func FormatSecretSourceSuffix(h, envVar string) string {
	source := SecretSourceOf(h, envVar)
	if source == "" {
		return ""
	}
	if source == "bitwarden" {
		return " (from Bitwarden)"
	}
	if registered := GetSecretSource(source, ""); registered != nil && registered.Spec().Label != "" {
		return " (from " + registered.Spec().Label + ")"
	}
	return " (from " + source + ")"
}

// Takes back what a source wrote into the process env once nothing backs it, lock held
func revokeSecretSourceWrites(h string, keep func(name, source string) bool) {
	key := homeKey(h)
	writes := secretWritesByHome[key]
	if len(writes) == 0 {
		return
	}
	ownedByDotenv := map[string]bool{}
	for k := range config.LoadEnvFile(filepath.Join(h, ".env")) {
		ownedByDotenv[k] = true
	}
	for k := range managedDotenvKeys {
		ownedByDotenv[k] = true
	}
	kept := map[string]secretSourceWrite{}
	for name, w := range writes {
		if keep(name, w.source) {
			kept[name] = w
			continue
		}
		if !ownedByDotenv[name] && os.Getenv(name) == w.value {
			if w.had {
				_ = os.Setenv(name, w.prior)
			} else {
				_ = os.Unsetenv(name)
			}
		}
		stillAttributed := false
		for otherKey, other := range secretWritesByHome {
			if otherKey != key {
				if ow, ok := other[name]; ok && ow.source == w.source {
					stillAttributed = true
				}
			}
		}
		if secretSourceOfVar[name] == w.source && !stillAttributed {
			delete(secretSourceOfVar, name)
		}
	}
	if len(kept) > 0 {
		secretWritesByHome[key] = kept
	} else {
		delete(secretWritesByHome, key)
	}
}

// ConfigBackendView is what the secret loader needs to know about the selected config backend
type ConfigBackendView struct {
	Name              string
	SelectorEnv       string
	ProtectedEnvNames map[string]bool
}

// ConfigBackendUnavailableError means the config backend cannot be reached with a source supplied credential
type ConfigBackendUnavailableError struct{ Message string }

func (e *ConfigBackendUnavailableError) Error() string { return e.Message }

// Reverts a source's writes to the config backend's own credential names and refuses a remote backend
func refuseProtectedEnvFromSources(report *ApplyReport, backend ConfigBackendView, environBefore map[string]string, env *SourceEnvironment, routedHome string) {
	supplied := map[string]bool{}
	for n := range report.Provenance {
		supplied[n] = true
	}
	for _, src := range report.Sources {
		for _, n := range src.SkippedExisting {
			supplied[n] = true
		}
	}
	protected := map[string]bool{backend.SelectorEnv: true}
	for n := range backend.ProtectedEnvNames {
		protected[n] = true
	}
	var clash []string
	for n := range supplied {
		if protected[n] {
			clash = append(clash, n)
		}
	}
	if len(clash) == 0 {
		return
	}
	sort.Strings(clash)
	if environBefore != nil {
		for _, name := range clash {
			if prior, had := environBefore[name]; had {
				env.Set(name, prior)
			} else {
				env.Unset(name)
			}
		}
	}
	if len(backend.ProtectedEnvNames) == 0 {
		for _, name := range clash {
			delete(report.Provenance, name)
			for _, src := range report.Sources {
				src.Applied = removeString(src.Applied, name)
				src.SkippedExisting = removeString(src.SkippedExisting, name)
			}
		}
		secretStatusPrint(fmt.Sprintf("  Secret sources: ignored %s from a secrets: source (the config backend is chosen by .env or the process environment only).", backend.SelectorEnv))
		return
	}
	outcome := "Icebath does not start with it."
	if routedHome != "" {
		outcome = fmt.Sprintf("Icebath does not serve profile home %s with it.", routedHome)
	}
	panic(&ConfigBackendUnavailableError{Message: fmt.Sprintf("A secrets: source supplies %s, which the %q config backend uses to reach its config plane. That credential must come from auth.json or .env, never from a secret source; remove the mapping. %s", strings.Join(clash, ", "), backend.Name, outcome)})
}

func removeString(list []string, target string) []string {
	out := list[:0:0]
	for _, s := range list {
		if s != target {
			out = append(out, s)
		}
	}
	return out
}

// Copies into the process env exactly the names the sources set in the staging copy
func publishStagedEnv(staged, environBefore map[string]string) {
	for name, value := range staged {
		if environBefore[name] != value {
			_ = os.Setenv(name, value)
		}
	}
}

// Compact summary of the non ASCII codepoints in a credential value
func formatOffendingChars(value string, limit int) string {
	var seen []string
	for _, ch := range value {
		if ch <= 127 {
			continue
		}
		label := fmt.Sprintf("U+%04X", ch)
		if unicode.IsPrint(ch) {
			label += fmt.Sprintf(" (%q)", string(ch))
		}
		if !containsString(seen, label) {
			seen = append(seen, label)
		}
		if len(seen) >= limit {
			break
		}
	}
	return strings.Join(seen, ", ")
}

// SanitizeLoadedCredentials strips non ASCII from credential env vars, warning once per key
func SanitizeLoadedCredentials() {
	for _, kv := range os.Environ() {
		key, value, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		matches := false
		for _, suffix := range credentialEnvSuffixes {
			if strings.HasSuffix(key, suffix) {
				matches = true
				break
			}
		}
		if !matches || isASCII(value) {
			continue
		}
		var cleaned strings.Builder
		for _, ch := range value {
			if ch <= 127 {
				cleaned.WriteRune(ch)
			}
		}
		_ = os.Setenv(key, cleaned.String())
		secretSourceCacheMu.Lock()
		warned := warnedCredentialKeys[key]
		warnedCredentialKeys[key] = true
		secretSourceCacheMu.Unlock()
		if warned {
			continue
		}
		stripped := len([]rune(value)) - len([]rune(cleaned.String()))
		detail := formatOffendingChars(value, 3)
		if detail == "" {
			detail = "non-printable"
		}
		plural := "s"
		if stripped == 1 {
			plural = ""
		}
		secretStatusPrint(fmt.Sprintf("  Warning: %s contained %d non-ASCII character%s (%s) — stripped so the key can be sent as an HTTP header.", key, stripped, plural, detail))
		secretStatusPrint("  This usually means the key was copy-pasted from a PDF, rich-text editor, or web page that substituted lookalike\n" +
			"  Unicode glyphs for ASCII letters. If authentication fails (e.g. \"API key not valid\"), re-copy the key from the\n" +
			"  provider's dashboard and run `icebath setup` (or edit the .env file in a plain-text editor).")
	}
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] > 127 {
			return false
		}
	}
	return true
}

// ApplyExternalSecretSources pulls secrets from every enabled source into the process env once per home
func ApplyExternalSecretSources(h string) {
	secretSourceCacheMu.Lock()
	defer secretSourceCacheMu.Unlock()
	key := homeKey(h)
	if appliedSecretHomes[key] {
		return
	}
	cfg, err := loadSecretsConfig(h)
	if err != nil {
		return
	}
	if len(cfg) == 0 {
		revokeSecretSourceWrites(h, func(string, string) bool { return false })
		return
	}
	anyEnabled := false
	for _, v := range cfg {
		if section, ok := v.(map[string]any); ok && section["enabled"] == true {
			anyEnabled = true
			break
		}
	}
	if !anyEnabled {
		revokeSecretSourceWrites(h, func(string, string) bool { return false })
		return
	}
	active := EnabledSecretSourceNames(cfg, h)
	revokeSecretSourceWrites(h, func(_ string, source string) bool { return active[source] })
	environBefore := ProcessSourceEnvironment().Snapshot()
	backendBefore := configBackendView(h)
	staged := map[string]string{}
	for k, v := range environBefore {
		staged[k] = v
	}
	stagedEnv := MappingSourceEnvironment(staged)
	report := ApplyAllSecretSources(cfg, h, stagedEnv)
	if len(report.Sources) == 0 {
		return
	}
	refuseProtectedEnvFromSources(report, backendBefore, environBefore, stagedEnv, "")
	publishStagedEnv(staged, environBefore)
	appliedSecretHomes[key] = true
	if report.AppliedAny() {
		secretSourceCacheMu.Unlock()
		SanitizeLoadedCredentials()
		secretSourceCacheMu.Lock()
		for name, applied := range report.Provenance {
			secretSourceOfVar[name] = applied.Source
		}
	}
	values := map[string]string{}
	supplied := recordSuppliedNames(report)
	for name := range supplied {
		if v, ok := os.LookupEnv(name); ok {
			values[name] = v
		}
	}
	recordSecretSourceWrites(key, report, supplied, environBefore)
	if len(values) > 0 {
		secretValuesByHome[key] = values
	}
	restore := map[string]string{}
	for n, a := range report.Provenance {
		if v, ok := values[n]; ok && a.Authoritative {
			restore[n] = v
		}
	}
	secretRestoreByHome[key] = restore
	for _, src := range report.Sources {
		if len(src.Applied) > 0 {
			plural := "s"
			if len(src.Applied) == 1 {
				plural = ""
			}
			secretStatusPrint(fmt.Sprintf("  %s: applied %d secret%s", src.Label, len(src.Applied), plural))
		}
		if src.Result.Error != "" {
			secretStatusPrint(fmt.Sprintf("  %s: %s", src.Label, src.Result.Error))
			if hint := secretRemediationHint(src.Name, src.Result.ErrorKind, cfg, key); hint != "" {
				secretStatusPrint(fmt.Sprintf("  %s: → %s", src.Label, hint))
			}
		}
		for _, warn := range src.Result.Warnings {
			secretStatusPrint(fmt.Sprintf("  %s: %s", src.Label, warn))
		}
	}
	for _, conflict := range report.Conflicts {
		secretStatusPrint("  Secret sources: " + conflict)
	}
}

// Refreshes the write record after one process global pass, a failed source keeps its earlier writes
func recordSecretSourceWrites(key string, report *ApplyReport, supplied map[string]bool, environBefore map[string]string) {
	failed := map[string]bool{}
	for _, src := range report.Sources {
		if !src.Result.OK() {
			failed[src.Name] = true
		}
	}
	previous := secretWritesByHome[key]
	revokeSecretSourceWrites(key, func(name, source string) bool {
		_, inProvenance := report.Provenance[name]
		return inProvenance || supplied[name] || failed[source]
	})
	writes := map[string]secretSourceWrite{}
	for name, rec := range secretWritesByHome[key] {
		if _, inProvenance := report.Provenance[name]; !inProvenance && os.Getenv(name) == rec.value {
			writes[name] = rec
		}
	}
	for name, applied := range report.Provenance {
		current, ok := os.LookupEnv(name)
		if !ok {
			continue
		}
		before, had := environBefore[name]
		prior, priorHad := before, had
		if earlier, exists := previous[name]; exists && earlier.value == before {
			prior, priorHad = earlier.prior, earlier.had
		}
		writes[name] = secretSourceWrite{source: applied.Source, value: current, prior: prior, had: priorHad}
	}
	if len(writes) > 0 {
		secretWritesByHome[key] = writes
	} else {
		delete(secretWritesByHome, key)
	}
}

// The failed source's one line fix it hint
func secretRemediationHint(sourceName string, kind SecretErrorKind, secretsCfg map[string]any, scope string) string {
	source := GetSecretSource(sourceName, scope)
	if source == nil {
		return ""
	}
	return strings.TrimSpace(SecretSourceRemediation(source.Spec(), kind, secretsSection(secretsCfg, sourceName)))
}
