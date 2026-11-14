package config

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/nickheyer/icebath/internal/home"
)

// Raw secrets section of the config of a home, an empty mapping when absent or unreadable
func (e *Env) secretsSection(ctx context.Context, homeDir string) (*yaml.Node, error) {
	doc, err := e.s.RawDocumentReadonly(home.WithOverride(ctx, homeDir))
	if err != nil {
		return nil, err
	}
	if section := childOf(doc.root, "secrets"); isMapping(section) {
		return plainCopy(section), nil
	}
	return emptyMapping(), nil
}

// Reports a secrets section in which some source is switched on with a literal true
func anySourceEnabled(secrets *yaml.Node) bool {
	for i := 1; i < len(secrets.Content); i += 2 {
		enabled := childOf(secrets.Content[i], "enabled")
		if enabled == nil || enabled.Kind != yaml.ScalarNode {
			continue
		}
		if tag, text := scalarKind(enabled); tag == tagBool {
			if v, _ := resolveTagged(tagBool, text); v == "true" {
				return true
			}
		}
	}
	return false
}

// Every name the sources of a report supplied, applied or kept out by a value that already existed
func suppliedNames(report *SecretReport) []string {
	var out []string
	for name := range report.Provenance {
		out = append(out, name)
	}
	for _, src := range report.Sources {
		for _, name := range src.SkippedExisting {
			if !slices.Contains(out, name) {
				out = append(out, name)
			}
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// Records the supplied names of a report as owned by a source and returns them
func (st *envState) recordSupplied(report *SecretReport) []string {
	names := suppliedNames(report)
	st.secretMu.Lock()
	for _, name := range names {
		st.supplied[name] = true
	}
	st.secretMu.Unlock()
	return names
}

// Config backend misuse by a secrets source that supplies a name the backend reaches its config plane with
func protectedSourceError(names []string, backend Backend, routedHome string) error {
	outcome := "icebath does not start with it."
	if routedHome != "" {
		outcome = fmt.Sprintf("icebath does not serve profile home %s with it.", routedHome)
	}
	return &BackendUnavailableError{Message: fmt.Sprintf("A secrets: source supplies %s, which the %s config "+
		"backend uses to reach its config plane. That credential must come from auth.json or .env, never "+
		"from a secret source; remove the mapping. %s", strings.Join(names, ", "), quoted(backend.Name()), outcome)}
}

// Undoes source writes to the backend selector and its protected names in env, failing when the backend protects any
func (e *Env) refuseProtectedNames(ctx context.Context, report *SecretReport, backend Backend, before, env map[string]string, routedHome string) error {
	protected := append([]string{EnvConfigBackend}, backend.ProtectedEnvNames()...)
	var clash []string
	for _, name := range suppliedNames(report) {
		if slices.Contains(protected, name) {
			clash = append(clash, name)
		}
	}
	if len(clash) == 0 {
		return nil
	}
	for _, name := range clash {
		if v, had := before[name]; had {
			env[name] = v
		} else {
			delete(env, name)
		}
	}
	if len(backend.ProtectedEnvNames()) > 0 {
		return protectedSourceError(clash, backend, routedHome)
	}
	for _, name := range clash {
		delete(report.Provenance, name)
		for i := range report.Sources {
			src := &report.Sources[i]
			src.Applied = slices.DeleteFunc(src.Applied, func(n string) bool { return n == name })
			src.SkippedExisting = slices.DeleteFunc(src.SkippedExisting, func(n string) bool { return n == name })
		}
	}
	e.s.notify(ctx, fmt.Sprintf("  Secret sources: ignored %s from a secrets: source (the config backend is chosen by .env or the process environment only).", EnvConfigBackend))
	return nil
}

// Takes back source writes into the process that keep disowns, unless another owner replaced the value since
func (e *Env) revokeSourceWrites(homeDir string, keep func(name, source string) bool) error {
	st := e.s.envs
	key := e.s.homeKey(homeDir)
	st.secretMu.Lock()
	writes := maps.Clone(st.writesByHome[key])
	st.secretMu.Unlock()
	if len(writes) == 0 {
		return nil
	}
	_, owned := st.loadFile(home.EnvFile.In(homeDir))
	st.mu.Lock()
	for name := range st.managedKeys {
		owned[name] = ""
	}
	st.mu.Unlock()
	kept := map[string]secretWrite{}
	for name, w := range writes {
		if keep(name, w.source) {
			kept[name] = w
			continue
		}
		if _, dotenv := owned[name]; !dotenv {
			if cur, present := os.LookupEnv(name); present && cur == w.value {
				var err error
				if w.hadPrior {
					err = setProcessEnv(name, w.prior)
				} else {
					err = os.Unsetenv(name)
				}
				if err != nil {
					return err
				}
			}
		}
		st.secretMu.Lock()
		attributed := false
		for other, records := range st.writesByHome {
			if rec, ok := records[name]; other != key && ok && rec.source == w.source {
				attributed = true
			}
		}
		if st.secretSources[name] == w.source && !attributed {
			delete(st.secretSources, name)
		}
		st.secretMu.Unlock()
	}
	st.secretMu.Lock()
	if len(kept) > 0 {
		st.writesByHome[key] = kept
	} else {
		delete(st.writesByHome, key)
	}
	st.secretMu.Unlock()
	return nil
}

// Refreshes the write records of a home after a pass, revoking a write its still enabled source stopped supplying
func (e *Env) recordSourceWrites(homeDir string, report *SecretReport, supplied []string, before map[string]string) error {
	st := e.s.envs
	key := e.s.homeKey(homeDir)
	var failed []string
	for _, src := range report.Sources {
		if src.Error != "" {
			failed = append(failed, src.Name)
		}
	}
	st.secretMu.Lock()
	previous := maps.Clone(st.writesByHome[key])
	st.secretMu.Unlock()
	err := e.revokeSourceWrites(homeDir, func(name, source string) bool {
		_, applied := report.Provenance[name]
		return applied || slices.Contains(supplied, name) || slices.Contains(failed, source)
	})
	if err != nil {
		return err
	}
	st.secretMu.Lock()
	defer st.secretMu.Unlock()
	writes := map[string]secretWrite{}
	for name, rec := range st.writesByHome[key] {
		if _, applied := report.Provenance[name]; applied {
			continue
		}
		if cur, present := os.LookupEnv(name); present && cur == rec.value {
			writes[name] = rec
		}
	}
	for name, applied := range report.Provenance {
		cur, present := os.LookupEnv(name)
		if !present {
			continue
		}
		prior, hadPrior := before[name]
		if earlier, ok := previous[name]; ok && hadPrior && earlier.value == prior {
			prior, hadPrior = earlier.prior, earlier.hadPrior
		}
		writes[name] = secretWrite{source: applied.Source, value: cur, prior: prior, hadPrior: hadPrior}
	}
	if len(writes) > 0 {
		st.writesByHome[key] = writes
	} else {
		delete(st.writesByHome, key)
	}
	return nil
}

// Puts back the source values a home is authoritative for after the env files overwrote them
func (st *envState) reassertSourceValues(key string) error {
	st.secretMu.Lock()
	restore := maps.Clone(st.restoreByHome[key])
	st.secretMu.Unlock()
	for name, value := range restore {
		if cur, present := os.LookupEnv(name); !present || cur != value {
			if err := setProcessEnv(name, value); err != nil {
				return err
			}
		}
	}
	return nil
}

// Tells the person what each source of a pass did, its error with its fix, its warnings and the conflicts
func (e *Env) reportSourceStatus(ctx context.Context, homeDir string, secrets *yaml.Node, report *SecretReport) {
	for _, src := range report.Sources {
		if n := len(src.Applied); n > 0 {
			plural := "s"
			if n == 1 {
				plural = ""
			}
			e.s.notify(ctx, fmt.Sprintf("  %s: applied %d secret%s", src.Label, n, plural))
		}
		if src.Error != "" {
			e.s.notify(ctx, fmt.Sprintf("  %s: %s", src.Label, src.Error))
			if hint := strings.TrimSpace(e.s.envs.sources.Remediation(ctx, homeDir, src.Name, src.ErrorKind, secrets)); hint != "" {
				e.s.notify(ctx, fmt.Sprintf("  %s: → %s", src.Label, hint))
			}
		}
		for _, warning := range src.Warnings {
			e.s.notify(ctx, fmt.Sprintf("  %s: %s", src.Label, warning))
		}
	}
	for _, conflict := range report.Conflicts {
		e.s.notify(ctx, "  Secret sources: "+conflict)
	}
}

// Pulls the secrets of every enabled source of a home into the process, once per home until reset
func (e *Env) applySecretSources(ctx context.Context, homeDir string) error {
	s, st := e.s, e.s.envs
	key := s.homeKey(homeDir)
	st.secretMu.Lock()
	done := st.appliedHomes[key]
	st.secretMu.Unlock()
	if done {
		return nil
	}
	secrets, err := e.secretsSection(ctx, homeDir)
	if errors.Is(err, ErrBackendUnavailable) {
		return err
	}
	if err != nil {
		return nil
	}
	if len(secrets.Content) == 0 || !anySourceEnabled(secrets) {
		return e.revokeSourceWrites(homeDir, func(string, string) bool { return false })
	}
	if st.sources == nil {
		return ErrNoSecretSources
	}
	active := st.sources.Enabled(ctx, homeDir, secrets)
	if err := e.revokeSourceWrites(homeDir, func(_, source string) bool { return slices.Contains(active, source) }); err != nil {
		return err
	}
	before := processEnviron()
	backend, err := s.backend(ctx)
	if err != nil {
		return err
	}
	staged := maps.Clone(before)
	report := st.sources.Apply(ctx, homeDir, secrets, staged)
	if report == nil || len(report.Sources) == 0 {
		return nil
	}
	if err := e.refuseProtectedNames(ctx, report, backend, before, staged, ""); err != nil {
		return err
	}
	for name, value := range staged {
		if was, had := before[name]; !had || was != value {
			if err := setProcessEnv(name, value); err != nil {
				return err
			}
		}
	}
	st.secretMu.Lock()
	st.appliedHomes[key] = true
	st.secretMu.Unlock()
	if len(report.Provenance) > 0 {
		if err := st.sanitizeCredentials(ctx); err != nil {
			return err
		}
		st.secretMu.Lock()
		for name, applied := range report.Provenance {
			st.secretSources[name] = applied.Source
		}
		st.secretMu.Unlock()
	}
	values := map[string]string{}
	supplied := st.recordSupplied(report)
	for _, name := range supplied {
		if v, present := os.LookupEnv(name); present {
			values[name] = v
		}
	}
	if err := e.recordSourceWrites(homeDir, report, supplied, before); err != nil {
		return err
	}
	restore := map[string]string{}
	for name, applied := range report.Provenance {
		if v, ok := values[name]; ok && applied.Authoritative {
			restore[name] = v
		}
	}
	st.secretMu.Lock()
	if len(values) > 0 {
		st.valuesByHome[key] = values
	}
	st.restoreByHome[key] = restore
	st.secretMu.Unlock()
	e.reportSourceStatus(ctx, homeDir, secrets, report)
	return nil
}

// Resolves the sources of the home of ctx into its own snapshot, the process untouched, retried until every source succeeds
func (e *Env) HydrateSecretSources(ctx context.Context) (map[string]string, error) {
	h, err := e.s.homes.Home(ctx)
	if err != nil {
		return nil, err
	}
	return e.hydrateSecretSources(ctx, h)
}

// Hydration of one profile home under the hydrate lock
func (e *Env) hydrateSecretSources(ctx context.Context, homeDir string) (map[string]string, error) {
	s, st := e.s, e.s.envs
	if s.worker != nil {
		return map[string]string{}, nil
	}
	st.hydrateMu.Lock()
	defer st.hydrateMu.Unlock()
	key := s.homeKey(homeDir)
	st.secretMu.Lock()
	done := st.appliedHomes[key]
	if !done {
		delete(st.valuesByHome, key)
		delete(st.restoreByHome, key)
	}
	st.secretMu.Unlock()
	if done {
		return st.sourceValues(key), nil
	}
	secrets, err := e.secretsSection(ctx, homeDir)
	if errors.Is(err, ErrBackendUnavailable) {
		return nil, err
	}
	if err != nil || len(secrets.Content) == 0 {
		return map[string]string{}, nil
	}
	if st.sources == nil {
		if anySourceEnabled(secrets) {
			return nil, ErrNoSecretSources
		}
		return map[string]string{}, nil
	}
	local := map[string]string{}
	for name, value := range processEnviron() {
		if IsGlobalEnv(name) {
			local[name] = value
		}
	}
	_, file := st.loadFile(home.EnvFile.In(homeDir))
	maps.Copy(local, file)
	if opEnv := home.OpEnvFile.In(homeDir); fileExists(opEnv) {
		_, op := st.loadFile(opEnv)
		for name, value := range op {
			if _, has := local[name]; !has {
				local[name] = value
			}
		}
	}
	local[home.EnvHome] = homeDir
	before := maps.Clone(local)
	report := st.sources.Apply(ctx, homeDir, secrets, local)
	if report == nil || len(report.Sources) == 0 {
		return map[string]string{}, nil
	}
	backend, err := s.backend(ctx)
	if err != nil {
		return nil, err
	}
	if err := e.refuseProtectedNames(ctx, report, backend, before, local, homeDir); err != nil {
		return nil, err
	}
	failed := slices.ContainsFunc(report.Sources, func(src SecretSourceReport) bool { return src.Error != "" })
	st.recordSupplied(report)
	values := map[string]string{}
	st.secretMu.Lock()
	if !failed {
		st.appliedHomes[key] = true
	}
	for name, applied := range report.Provenance {
		if v, ok := local[name]; ok {
			st.secretSources[name] = applied.Source
			values[name] = v
		}
	}
	st.valuesByHome[key] = values
	st.secretMu.Unlock()
	return maps.Clone(values), nil
}

// Forgets what the sources of the home of ctx supplied so its next load pulls them again
func (e *Env) ResetSecretSources(ctx context.Context) error {
	h, err := e.s.homes.Home(ctx)
	if err != nil {
		return err
	}
	st := e.s.envs
	key := e.s.homeKey(h)
	st.secretMu.Lock()
	defer st.secretMu.Unlock()
	delete(st.appliedHomes, key)
	delete(st.valuesByHome, key)
	delete(st.restoreByHome, key)
	return nil
}

// Forgets what the sources of every home supplied, their provenance and their write records
func (e *Env) ResetAllSecretSources() {
	st := e.s.envs
	st.secretMu.Lock()
	defer st.secretMu.Unlock()
	st.appliedHomes = map[string]bool{}
	st.secretSources = map[string]string{}
	st.supplied = map[string]bool{}
	st.valuesByHome = map[string]map[string]string{}
	st.restoreByHome = map[string]map[string]string{}
	st.writesByHome = map[string]map[string]secretWrite{}
}

// Source that supplied a variable, found false for a value from an env file or the shell
func (e *Env) SecretSource(name string) (string, bool) {
	st := e.s.envs
	st.secretMu.Lock()
	defer st.secretMu.Unlock()
	source, ok := st.secretSources[name]
	return source, ok
}

// Every variable name a source of some profile applied, sorted, values resolving only through the scope of that profile
func (e *Env) SecretSourceNames() []string {
	st := e.s.envs
	st.secretMu.Lock()
	defer st.secretMu.Unlock()
	out := make([]string, 0, len(st.secretSources))
	for name := range st.secretSources {
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

// Every variable name a source supplied for any home, applied or kept out by an existing value, sorted
func (e *Env) SourceSuppliedNames() []string {
	st := e.s.envs
	out := e.SecretSourceNames()
	st.secretMu.Lock()
	for name := range st.supplied {
		out = append(out, name)
	}
	st.secretMu.Unlock()
	slices.Sort(out)
	return slices.Compact(out)
}

// Snapshot of the values the sources of the home of ctx supplied, a fresh map on every call
func (e *Env) SecretSourceValues(ctx context.Context) (map[string]string, error) {
	h, err := e.s.homes.Home(ctx)
	if err != nil {
		return nil, err
	}
	return e.s.envs.sourceValues(e.s.homeKey(h)), nil
}

// Copy of the source value snapshot kept under a home key
func (st *envState) sourceValues(key string) map[string]string {
	st.secretMu.Lock()
	defer st.secretMu.Unlock()
	out := map[string]string{}
	maps.Copy(out, st.valuesByHome[key])
	return out
}

// Text naming the source that supplied a variable for status lines, empty for an env file or shell value
func (e *Env) SecretSourceSuffix(ctx context.Context, name string) string {
	source, ok := e.SecretSource(name)
	switch {
	case !ok || source == "":
		return ""
	case source == "bitwarden":
		return " (from Bitwarden)"
	}
	if st := e.s.envs; st.sources != nil {
		if h, err := e.s.homes.Home(ctx); err == nil {
			if label := st.sources.Label(ctx, h, source); label != "" {
				return " (from " + label + ")"
			}
		}
	}
	return " (from " + source + ")"
}

// Raised when a config enables a secret source in a program that wired no secret sources
var ErrNoSecretSources = errors.New("no secret sources are installed to resolve the sources the config enables")
