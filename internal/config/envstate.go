package config

import (
	"bytes"
	"context"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"

	"gopkg.in/yaml.v3"

	"github.com/nickheyer/icebath/internal/home"
)

// Secret scope of the profile a task serves, answered by the package that binds scopes to tasks
type SecretScope interface {
	Lookup(ctx context.Context, name string) (value string, found bool, err error)
	Bound(ctx context.Context) bool
	Multiplexed(ctx context.Context) bool
	Routed(ctx context.Context) bool
	Store(ctx context.Context, name string, value *string) bool
}

// What one external secret source did in one pass, Error empty when its fetch succeeded
type SecretSourceReport struct {
	Name            string
	Label           string
	Applied         []string
	SkippedExisting []string
	Error           string
	ErrorKind       string
	Warnings        []string
}

// Source that set one variable and whether it may beat the env file for it on later reloads
type AppliedSecret struct {
	Source        string
	Authoritative bool
}

// Outcome of one pass over every enabled external secret source
type SecretReport struct {
	Sources    []SecretSourceReport
	Provenance map[string]AppliedSecret
	Conflicts  []string
}

// External secret sources of the program, fetched and applied into the environment a pass hands over
type SecretSources interface {
	Apply(ctx context.Context, home string, secrets *yaml.Node, env map[string]string) *SecretReport
	Enabled(ctx context.Context, home string, secrets *yaml.Node) []string
	Remediation(ctx context.Context, home, source, errorKind string, secrets *yaml.Node) string
	Label(ctx context.Context, home, source string) string
}

// Wires the secret scope every scoped read, config reference and env publish goes through
func WithSecretScope(scope SecretScope) Option {
	return func(s *Store) { s.envs.scope, s.envs.wired = scope, scope != nil }
}

// Wires the external secret sources the env loader applies after the env files
func WithSecretSources(sources SecretSources) Option {
	return func(s *Store) { s.envs.sources = sources }
}

// Wires the stores beyond the env file that follow a saved provider credential, its pool entries and suppressions
func WithCredentialSaved(fn func(ctx context.Context, name string)) Option {
	return func(s *Store) { s.envs.credentialSaved = fn }
}

// Wires the stores that drop a removed provider credential, returning the pool entries pruned and the providers touched
func WithCredentialRemoved(fn func(ctx context.Context, name string) (pruned, providers []string, err error)) Option {
	return func(s *Store) { s.envs.credentialRemoved = fn }
}

// Where a recorded redaction policy was read from
type PolicySource int

// Sources of a redaction policy, the env file value winning over the config value
const (
	PolicyEnvFile PolicySource = iota
	PolicyConfig
)

// Receiver of the redaction policy each loaded env file and home config carries, set false when absent
func OnRedactionPolicy(fn func(homeKey string, source PolicySource, raw string, set bool)) Option {
	return func(s *Store) { s.envs.policy = fn }
}

// Hands the redaction policy one load read to the receiver, when one is wired
func (st *envState) notePolicy(homeKey string, source PolicySource, raw string, set bool) {
	if st.policy != nil {
		st.policy(homeKey, source, raw, set)
	}
}

// Value one load published for a variable, with what the variable held before the first publish
type publishedVar struct {
	baseline    string
	hadBaseline bool
	value       string
	pass        int
}

// Value a secret source wrote into the process, with what the variable held before the first write
type secretWrite struct {
	source   string
	value    string
	prior    string
	hadPrior bool
}

// Parsed env file kept while its bytes stay the same
type envFileMemo struct {
	raw    []byte
	names  []string
	values map[string]string
}

// Env file memo entries kept, one per profile home in practice
const envFileMemoMax = 64

// Environment state of one process, what its loads published and what each home got from secret sources
type envState struct {
	scope             SecretScope
	wired             bool
	sources           SecretSources
	credentialSaved   func(ctx context.Context, name string)
	credentialRemoved func(ctx context.Context, name string) (pruned, providers []string, err error)
	policy            func(homeKey string, source PolicySource, raw string, set bool)
	notify            func(ctx context.Context, text string)
	registry          *EnvRegistry
	safeWorker        bool

	mu               sync.Mutex
	published        map[string]publishedVar
	passes           int
	loadedKeys       map[string]bool
	managedKeys      map[string]bool
	warnedKeys       map[string]bool
	warnedUTF32      map[string]bool
	warnedUnreadable map[string]bool
	skipLogged       map[string]bool

	hydrateMu     sync.Mutex
	secretMu      sync.Mutex
	secretSources map[string]string
	supplied      map[string]bool
	valuesByHome  map[string]map[string]string
	restoreByHome map[string]map[string]string
	writesByHome  map[string]map[string]secretWrite
	appliedHomes  map[string]bool

	fileMu    sync.Mutex
	files     map[string]envFileMemo
	fileOrder []string
}

// Fresh environment state with the built in registry
func newEnvState() *envState {
	return &envState{
		registry:         newEnvRegistry(),
		published:        map[string]publishedVar{},
		loadedKeys:       map[string]bool{},
		managedKeys:      map[string]bool{},
		warnedKeys:       map[string]bool{},
		warnedUTF32:      map[string]bool{},
		warnedUnreadable: map[string]bool{},
		skipLogged:       map[string]bool{},
		secretSources:    map[string]string{},
		supplied:         map[string]bool{},
		valuesByHome:     map[string]map[string]string{},
		restoreByHome:    map[string]map[string]string{},
		writesByHome:     map[string]map[string]secretWrite{},
		appliedHomes:     map[string]bool{},
		files:            map[string]envFileMemo{},
	}
}

// Fills the document layer seams with the env file parser, the bootstrap step and the reference lookup
func (s *Store) bindEnv() {
	if s.envs.scope == nil {
		s.envs.scope = &homeScope{s: s}
	}
	s.envs.notify = s.notify
	s.parseEnvFile = func(data []byte) (map[string]string, error) {
		_, values := tokenizeEnv(string(data))
		return values, nil
	}
	s.bootstrapEnv = func(ctx context.Context) error { return s.publishDeployment(ctx, "", "") }
	s.env = s.referenceLookup
}

// Stable key of a home or file path, its resolved form
func (s *Store) homeKey(path string) string {
	if key, err := s.homes.Key(path); err == nil {
		return key
	}
	return filepath.Clean(path)
}

// Reports two paths naming the same home
func (s *Store) sameHome(a, b string) bool { return s.homeKey(a) == s.homeKey(b) }

// Names and values of the env file at path as the tokenizer reads them, empty when absent or unreadable
func (st *envState) loadFile(path string) ([]string, map[string]string) {
	raw, err := os.ReadFile(path)
	st.fileMu.Lock()
	defer st.fileMu.Unlock()
	if err != nil {
		st.forgetFile(path)
		return nil, map[string]string{}
	}
	if memo, ok := st.files[path]; ok && bytes.Equal(memo.raw, raw) {
		st.touchFile(path)
		return slices.Clone(memo.names), maps.Clone(memo.values)
	}
	names, values := tokenizeEnv(tokenizerText(raw))
	st.files[path] = envFileMemo{raw: raw, names: slices.Clone(names), values: maps.Clone(values)}
	st.touchFile(path)
	for len(st.fileOrder) > envFileMemoMax {
		delete(st.files, st.fileOrder[0])
		st.fileOrder = st.fileOrder[1:]
	}
	return names, values
}

// Marks a memo entry as the most recently used, the caller holding the file lock
func (st *envState) touchFile(path string) {
	st.fileOrder = append(slices.DeleteFunc(st.fileOrder, func(p string) bool { return p == path }), path)
}

// Drops one memo entry, the caller holding the file lock
func (st *envState) forgetFile(path string) {
	delete(st.files, path)
	st.fileOrder = slices.DeleteFunc(st.fileOrder, func(p string) bool { return p == path })
}

// Drops every memo entry so the next read parses the file again
func (st *envState) forgetFiles() {
	st.fileMu.Lock()
	defer st.fileMu.Unlock()
	st.files = map[string]envFileMemo{}
	st.fileOrder = nil
}

// Scope of a process that binds none, process variables for its own home, files for any other
type homeScope struct{ s *Store }

// Value of a name, from the process for global names and the launch home, else from the home files
func (h *homeScope) Lookup(ctx context.Context, name string) (string, bool, error) {
	if IsGlobalEnv(name) || !h.Routed(ctx) {
		v, ok := os.LookupEnv(name)
		return v, ok, nil
	}
	profile, err := h.s.Env().Profile(ctx)
	if err != nil {
		return "", false, err
	}
	v, ok := profile[name]
	return v, ok, nil
}

// No task has a scope bound
func (h *homeScope) Bound(context.Context) bool { return false }

// The process is no multiplexer
func (h *homeScope) Multiplexed(context.Context) bool { return false }

// Reports a context whose home override names another home than the one the process launched for
func (h *homeScope) Routed(ctx context.Context) bool {
	override := home.Override(ctx)
	if override == "" {
		return false
	}
	launch, err := home.RoutingHome()
	return err != nil || !h.s.sameHome(override, launch)
}

// There is no bound scope to write into
func (h *homeScope) Store(context.Context, string, *string) bool { return false }

// Value a config reference reads, the bound scope when one is bound and the process variables otherwise
func (s *Store) referenceLookup(ctx context.Context, name string) (string, bool) {
	st := s.envs
	if st.wired && !st.scope.Bound(ctx) {
		return os.LookupEnv(name)
	}
	v, ok, err := st.scope.Lookup(ctx, name)
	return v, ok && err == nil
}
