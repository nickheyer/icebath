package tools

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/providerregistry"
)

// Characters a shell word may hold without quoting
var shellSafeWordRe = regexp.MustCompile(`^[\w@%+=:,./-]+$`)

// Quotes one word for a POSIX shell the way shlex.quote does
func shQuote(s string) string {
	if s == "" {
		return "''"
	}
	if shellSafeWordRe.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// Joins argv into one shell quoted command line
func shJoin(argv []string) string {
	parts := make([]string, len(argv))
	for i, a := range argv {
		parts[i] = shQuote(a)
	}
	return strings.Join(parts, " ")
}

// Error of a command line shlex cannot tokenize
var errUnbalancedQuotes = errors.New("No closing quotation")

// Splits a command line into words with POSIX shlex semantics, comments off
func shlexSplit(s string) ([]string, error) {
	var words []string
	var cur strings.Builder
	inWord := false
	i := 0
	for i < len(s) {
		c := s[i]
		switch {
		case c == '\'':
			end := strings.IndexByte(s[i+1:], '\'')
			if end < 0 {
				return nil, errUnbalancedQuotes
			}
			cur.WriteString(s[i+1 : i+1+end])
			inWord = true
			i += end + 2
		case c == '"':
			i++
			closed := false
			for i < len(s) {
				d := s[i]
				if d == '"' {
					closed = true
					i++
					break
				}
				if d == '\\' && i+1 < len(s) && strings.IndexByte("\"\\$`\n", s[i+1]) >= 0 {
					cur.WriteByte(s[i+1])
					i += 2
					continue
				}
				cur.WriteByte(d)
				i++
			}
			if !closed {
				return nil, errUnbalancedQuotes
			}
			inWord = true
		case c == '\\':
			if i+1 < len(s) {
				cur.WriteByte(s[i+1])
				inWord = true
				i += 2
			} else {
				return nil, errors.New("No escaped character")
			}
		case isSpaceByte(c):
			if inWord {
				words = append(words, cur.String())
				cur.Reset()
				inWord = false
			}
			i++
		default:
			cur.WriteByte(c)
			inWord = true
			i++
		}
	}
	if inWord {
		words = append(words, cur.String())
	}
	return words, nil
}

// Splits a command line like shlex in whitespace split mode with punctuation chars as their own tokens
func shlexSplitPunct(s string, punct string, comments bool) ([]string, error) {
	var words []string
	var cur strings.Builder
	inWord := false
	flush := func() {
		if inWord {
			words = append(words, cur.String())
			cur.Reset()
			inWord = false
		}
	}
	i := 0
	for i < len(s) {
		c := s[i]
		switch {
		case comments && c == '#' && !inWord:
			end := strings.IndexByte(s[i:], '\n')
			if end < 0 {
				i = len(s)
			} else {
				i += end
			}
		case c == '\'':
			end := strings.IndexByte(s[i+1:], '\'')
			if end < 0 {
				return nil, errUnbalancedQuotes
			}
			cur.WriteString(s[i+1 : i+1+end])
			inWord = true
			i += end + 2
		case c == '"':
			i++
			closed := false
			for i < len(s) {
				d := s[i]
				if d == '"' {
					closed = true
					i++
					break
				}
				if d == '\\' && i+1 < len(s) && strings.IndexByte("\"\\$`\n", s[i+1]) >= 0 {
					cur.WriteByte(s[i+1])
					i += 2
					continue
				}
				cur.WriteByte(d)
				i++
			}
			if !closed {
				return nil, errUnbalancedQuotes
			}
			inWord = true
		case c == '\\':
			if i+1 < len(s) {
				cur.WriteByte(s[i+1])
				inWord = true
				i += 2
			} else {
				return nil, errors.New("No escaped character")
			}
		case isSpaceByte(c):
			flush()
			i++
		case strings.IndexByte(punct, c) >= 0:
			flush()
			j := i
			for j < len(s) && strings.IndexByte(punct, s[j]) >= 0 {
				j++
			}
			words = append(words, s[i:j])
			i = j
		default:
			cur.WriteByte(c)
			inWord = true
			i++
		}
	}
	flush()
	return words, nil
}

// ExecOptions tunes one command run inside an environment
type ExecOptions struct {
	Cwd     string
	Timeout int
	Stdin   *string
	// Leaves A && B & alone for callers that already produce shell safe wrappers
	NoCompoundRewrite bool
	// Caps retention at tool_output.max_bytes while draining, foreground terminal tool only
	BoundedCapture bool
	// Hands a still running foreground process to the background registry when the host asks
	YieldHandler func(proc ProcessHandle, outputSoFar string) *ExecResult
}

// ExecResult is the outcome of one command run inside an environment
type ExecResult struct {
	Output               string
	ReturnCode           int
	CwdObserved          bool
	Cwd                  string
	PipelineStatus       []int
	OutputTotalChars     int
	FullOutputPath       string
	TimedOut             bool
	Interrupted          bool
	EnvironmentRecreated bool
	StdinError           string
	YieldedSessionID     string
	Pid                  int
}

// ProcessHandle is the duck type every backend returns from runBash
type ProcessHandle interface {
	// Exit code and whether the process has finished
	Poll() (int, bool)
	Kill() error
	// Exit code once the process finishes within the timeout, zero timeout waits forever
	Wait(timeoutSeconds float64) (int, error)
	// Read end of the merged stdout and stderr pipe, never passed through Fd on POSIX
	Stdout() *os.File
	Pid() int
}

// Environment is one terminal backend instance with a persistent shell snapshot and cwd
type Environment interface {
	Execute(ctx context.Context, command string, opts ExecOptions) (*ExecResult, error)
	Cleanup(forceRemove bool) error
	EnvType() string
	Cwd() string
	SetCwd(cwd string)
	TempDir() string
	HostCwd() string
	HostCwdMount() string
	SandboxHome() string
	IsPersistent() bool
	IsSessionScoped() bool
	UsesHostFilesystem() bool
	FetchFile(ctx context.Context, remotePath, localDest string, maxBytes int) error
	FetchRealpath(ctx context.Context, remotePath string) string
}

// BackendProvider describes one pluggable terminal backend and creates its environments
type BackendProvider interface {
	Name() string
	DisplayName() string
	Description() string
	EnvDescription() string
	IsRemote() bool
	IsContainer() bool
	SkipContainerGuards() bool
	CachePathBase() string
	DefaultCwd() string
	StripEnvKeys() []string
	SessionIsolatedWhenNonpersistent() bool
	SupportsStreams() bool
	IsAvailable() bool
	CheckRequirements(env *Env, cfg map[string]any) error
	Probe() (status string, detail string)
	SetupInstructions(env *Env) []string
	DoctorChecks() []DoctorCheck
	CreateEnvironment(ctx context.Context, env *Env, opts CreateOptions) (Environment, error)
}

// DoctorCheck is one row of a backend health report
type DoctorCheck struct {
	OK     bool
	Label  string
	Detail string
}

// CreateOptions carries everything a backend needs to build an environment
type CreateOptions struct {
	Cwd             string
	Timeout         int
	TaskID          string
	Image           string
	ContainerConfig map[string]any
	HostCwd         string
	ProbeOnly       bool
}

// Backends shipped in this package by lower case name
var builtinBackendProviders = struct {
	sync.RWMutex
	providers map[string]BackendProvider
}{providers: map[string]BackendProvider{}}

var terminalEnvRegistryLog = slog.Default().With("logger", "agent.terminal_env_registry")

// Python ValueError a plugin raises by taking a name owned by an in tree backend
func rejectBuiltinBackendCollision(name string) error {
	return &providerregistry.ValueError{Message: fmt.Sprintf("Terminal backend name '%s' is reserved for the built-in %s backend and cannot be registered by a plugin", name, name)}
}

// Plugin terminal backends by lower case name, the global layer plus one layer per profile scope
var terminalEnvRegistry = providerregistry.New[BackendProvider](providerregistry.Options{
	Label: "Terminal environment", ProviderCls: "TerminalEnvironmentProvider", Logger: terminalEnvRegistryLog,
	Normalize: providerregistry.LowerKey, BuiltinNames: builtinBackends, OnBuiltinCollision: rejectBuiltinBackendCollision,
})

// RegisterTerminalBackend adds a plugin backend to the global layer, refusing names reserved by the built in environments
func RegisterTerminalBackend(p BackendProvider) error { return terminalEnvRegistry.Register(p, "") }

// RegisterTerminalBackendScoped adds a plugin backend to one profile scope, empty for the global layer
func RegisterTerminalBackendScoped(p BackendProvider, scope string) error {
	return terminalEnvRegistry.Register(p, scope)
}

// Registers a backend shipped in this package under its reserved name
func registerBuiltinBackend(p BackendProvider) {
	builtinBackendProviders.Lock()
	defer builtinBackendProviders.Unlock()
	builtinBackendProviders.providers[strings.ToLower(strings.TrimSpace(p.Name()))] = p
}

// Backend shipped in this package under a reserved name, nil for every other name
func builtinBackend(name string) BackendProvider {
	builtinBackendProviders.RLock()
	defer builtinBackendProviders.RUnlock()
	return builtinBackendProviders.providers[strings.ToLower(strings.TrimSpace(name))]
}

// GetTerminalBackend returns the built in backend of a name, else the plugin backend of the active scope, else nil
func GetTerminalBackend(name string) BackendProvider {
	return GetTerminalBackendScoped(context.Background(), name, "")
}

// Built in backend of a name, else the plugin backend registered in scope then globally, nil when absent
func GetTerminalBackendScoped(ctx context.Context, name, scope string) BackendProvider {
	if p := builtinBackend(name); p != nil {
		return p
	}
	return terminalEnvRegistry.GetProvider(ctx, name, scope)
}

// TerminalBackendNames lists every built in and active scope plugin backend name sorted
func TerminalBackendNames() []string {
	builtinBackendProviders.RLock()
	names := make([]string, 0, len(builtinBackendProviders.providers))
	for n := range builtinBackendProviders.providers {
		names = append(names, n)
	}
	builtinBackendProviders.RUnlock()
	names = append(names, PluginBackendNames(context.Background(), "")...)
	sort.Strings(names)
	return names
}

// Classification flag of a registered backend, false for built in or unknown names
func providerFlagBool(envType string, flag func(BackendProvider) bool) bool {
	if envType == "" || builtinBackends[envType] {
		return false
	}
	p := GetTerminalBackend(envType)
	if p == nil {
		return false
	}
	return flag(p)
}
