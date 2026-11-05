package provider

import (
	"context"
	"github.com/nickheyer/icebath/pkg/tools"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unicode"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

var (
	multiplexMu     sync.Mutex
	multiplexActive bool
	autoPinnedHome  string
	pinnedHome      string
	pinnedHomeSet   bool
)

// PinProcessHome fixes the launch home routed profile decisions compare against, empty releases it
func PinProcessHome(path string) {
	multiplexMu.Lock()
	defer multiplexMu.Unlock()
	pinnedHome, pinnedHomeSet = path, path != ""
}

// ProcessHomePinned reports whether a host pinned the launch home
func ProcessHomePinned() bool {
	multiplexMu.Lock()
	defer multiplexMu.Unlock()
	return pinnedHomeSet
}

// RoutingProcessHome is the pinned launch home, else the process home
func RoutingProcessHome() string {
	multiplexMu.Lock()
	defer multiplexMu.Unlock()
	if pinnedHomeSet {
		return pinnedHome
	}
	return home.Process()
}

// SetMultiplexActive marks the process a profile multiplexer and pins the launch home while it is
func SetMultiplexActive(active bool) {
	tools.SetMultiplexActive(active)
	multiplexMu.Lock()
	defer multiplexMu.Unlock()
	multiplexActive = active
	if active {
		if !pinnedHomeSet {
			autoPinnedHome = home.Process()
			pinnedHome, pinnedHomeSet = autoPinnedHome, true
		}
		return
	}
	if autoPinnedHome != "" {
		if pinnedHomeSet && pinnedHome == autoPinnedHome {
			pinnedHome, pinnedHomeSet = "", false
		}
		autoPinnedHome = ""
	}
}

// MultiplexProcess reports the process wide multiplex flag
func MultiplexProcess() bool {
	multiplexMu.Lock()
	defer multiplexMu.Unlock()
	return multiplexActive
}

type secretScopeKey struct{}
type multiplexCtxKey struct{}
type launchBodyKey struct{}

// SecretScope is the profile secret mapping a task reads credentials through
type SecretScope struct {
	Mapping     map[string]string
	ProfileHome string
	multiplex   bool
	launchBody  func() map[string]string
	override    string
	mu          sync.Mutex
}

// WithSecretScope installs the profile secret mapping on the context, nil clears it
func WithSecretScope(ctx context.Context, secrets map[string]string, profileHome string) context.Context {
	if secrets == nil {
		return context.WithValue(ctx, secretScopeKey{}, (*SecretScope)(nil))
	}
	return context.WithValue(ctx, secretScopeKey{}, &SecretScope{Mapping: secrets, ProfileHome: profileHome})
}

// WithMultiplexContext runs the task under multiplex semantics regardless of the process flag
func WithMultiplexContext(ctx context.Context, active bool) context.Context {
	return context.WithValue(ctx, multiplexCtxKey{}, active)
}

// WithLaunchBodyScope records the launch profile mapping a body entered before multiplexing started
func WithLaunchBodyScope(ctx context.Context, resolver func() map[string]string) context.Context {
	return context.WithValue(ctx, launchBodyKey{}, resolver)
}

func boundScope(ctx context.Context) *SecretScope {
	if ctx == nil {
		return nil
	}
	s, _ := ctx.Value(secretScopeKey{}).(*SecretScope)
	return s
}

// IsMultiplexActive is true in a multiplexing process or for a task under multiplex semantics
func IsMultiplexActive(ctx context.Context) bool {
	if MultiplexProcess() {
		return true
	}
	if ctx == nil {
		return false
	}
	v, _ := ctx.Value(multiplexCtxKey{}).(bool)
	return v
}

// CurrentSecretScope is the active mapping, nil when no scope is installed
func CurrentSecretScope(ctx context.Context) map[string]string {
	if s := boundScope(ctx); s != nil {
		return s.Mapping
	}
	return nil
}

// CurrentSecretScopeHome is the home the active scope was stamped with
func CurrentSecretScopeHome(ctx context.Context) string {
	if s := boundScope(ctx); s != nil {
		return s.ProfileHome
	}
	return ""
}

// ServesRoutedProfile is true when the task runs for a profile other than the process's own
func ServesRoutedProfile(ctx context.Context) bool {
	if IsMultiplexActive(ctx) {
		return true
	}
	own := home.Key(RoutingProcessHome())
	if s := boundScope(ctx); s != nil && s.ProfileHome != "" && home.Key(s.ProfileHome) != own {
		return true
	}
	override := home.Override(ctx)
	return override != "" && home.Key(override) != own
}

// SecretScopeFrom snapshots the context's scope, multiplex and home override into one reader
func SecretScopeFrom(ctx context.Context) *SecretScope {
	s := &SecretScope{}
	if ctx == nil {
		return s
	}
	if bound := boundScope(ctx); bound != nil {
		s.Mapping, s.ProfileHome = bound.Mapping, bound.ProfileHome
	}
	s.multiplex, _ = ctx.Value(multiplexCtxKey{}).(bool)
	s.launchBody, _ = ctx.Value(launchBodyKey{}).(func() map[string]string)
	s.override = home.Override(ctx)
	return s
}

// UnscopedSecretError is a secret read in multiplex mode with no scope installed
type UnscopedSecretError struct {
	SecretName      string
	DeveloperDetail string
}

// NewUnscopedSecretError treats a whitespace bearing name as the developer detail of an older caller
func NewUnscopedSecretError(secretName, developerDetail string) *UnscopedSecretError {
	if secretName != "" && developerDetail == "" && strings.IndexFunc(secretName, unicode.IsSpace) >= 0 {
		secretName, developerDetail = "", secretName
	}
	return &UnscopedSecretError{SecretName: secretName, DeveloperDetail: developerDetail}
}

func (e *UnscopedSecretError) Error() string {
	what := "this profile's API key"
	if e.SecretName != "" {
		what = "this profile's " + e.SecretName
	}
	return "Icebath could not read " + what + " (an internal profile-scoping bug on the multiplexed " +
		"gateway, not your configuration). Run `icebath gateway restart`; if it keeps happening, " +
		"report it with `icebath debug share`."
}

var globalEnvNames = map[string]bool{
	"ICEBATH_HOME": true, "ICEBATH_PROFILE": true, "ICEBATH_GATEWAY_LOCK_DIR": true, "ICEBATH_MAX_ITERATIONS": true,
	"ICEBATH_API_TIMEOUT": true, "ICEBATH_REDACT_SECRETS": true, "ICEBATH_NOUS_TIMEOUT_SECONDS": true, "_ICEBATH_GATEWAY": true,
	"PATH": true, "HOME": true, "USER": true, "LANG": true, "LC_ALL": true, "TZ": true, "PWD": true, "SHELL": true, "TMPDIR": true,
	"VIRTUAL_ENV": true, "PYTHONPATH": true, "SSL_CERT_FILE": true,
	"ICEBATH_KANBAN_DB": true, "ICEBATH_KANBAN_WORKSPACES_ROOT": true, "ICEBATH_KANBAN_BOARD": true,
	"API_SERVER_ENABLED": true, "API_SERVER_HOST": true, "API_SERVER_PORT": true, "API_SERVER_CORS_ORIGINS": true,
	"GATEWAY_RELAY_URL": true, "GATEWAY_RELAY_ENDPOINT": true, "GATEWAY_RELAY_ALLOW_DIRECT_PLATFORMS": true,
	"GATEWAY_RELAY_PLATFORMS": true, "GATEWAY_RELAY_BOT_IDS": true, "GATEWAY_RELAY_ROUTE_KEYS": true,
	"GATEWAY_RELAY_INSTANCE_ID": true, "GATEWAY_RELAY_WAKE_URL": true, "GATEWAY_RELAY_DISPLAY_NAME": true,
}

var globalEnvNamePrefixes = []string{"ICEBATH_KANBAN_", "ICEBATH_TELEGRAM_", "TERMINAL_"}

// IsGlobalEnv is true for process deployment settings that are never profile secrets
func IsGlobalEnv(name string) bool {
	if globalEnvNames[name] {
		return true
	}
	for _, p := range globalEnvNamePrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

func (s *SecretScope) servesRouted() bool {
	if s == nil {
		return MultiplexProcess()
	}
	if MultiplexProcess() || s.multiplex {
		return true
	}
	own := home.Key(RoutingProcessHome())
	if s.Mapping != nil && s.ProfileHome != "" && home.Key(s.ProfileHome) != own {
		return true
	}
	return s.override != "" && home.Key(s.override) != own
}

// Secret resolves an env var name through the scope, failing closed under multiplexing with no scope
func (s *SecretScope) Secret(name string) (string, bool, error) {
	if IsGlobalEnv(name) {
		v, ok := os.LookupEnv(name)
		return v, ok, nil
	}
	if s != nil && s.Mapping != nil {
		s.mu.Lock()
		v, ok := s.Mapping[name]
		s.mu.Unlock()
		if ok {
			return v, true, nil
		}
		if s.servesRouted() {
			return "", false, nil
		}
		v, ok = os.LookupEnv(name)
		return v, ok, nil
	}
	if MultiplexProcess() || (s != nil && s.multiplex) {
		if s != nil && s.launchBody != nil {
			v, ok := s.launchBody()[name]
			return v, ok, nil
		}
		return "", false, NewUnscopedSecretError(name,
			"get_secret("+name+") called with no profile secret scope active while multiplexing is on. "+
				"This credential read must run inside a WithSecretScope context (the per-turn / per-adapter profile scope). "+
				"Reading the process environment here would risk leaking another profile's value.")
	}
	v, ok := os.LookupEnv(name)
	return v, ok, nil
}

// GetSecret is Secret for callers that cannot report an error, an unscoped read panics
func (s *SecretScope) GetSecret(name string) (string, bool) {
	v, ok, err := s.Secret(name)
	if err != nil {
		panic(err)
	}
	return v, ok
}

// GetSecretStr is GetSecret with a default for a genuinely unset secret
func (s *SecretScope) GetSecretStr(name, def string) string {
	v, ok := s.GetSecret(name)
	if !ok {
		return def
	}
	return v
}

// Process environment read through the scope rules with no scope installed
func processSecret(name string) string {
	v, _ := (*SecretScope)(nil).GetSecret(name)
	return v
}

// GetSecret resolves a credential by env var name honoring the scope carried by ctx
func GetSecret(ctx context.Context, name string) (string, bool, error) {
	return SecretScopeFrom(ctx).Secret(name)
}

// GetSecretStr is GetSecret with a default, an unscoped read still panics
func GetSecretStr(ctx context.Context, name, def string) string {
	return SecretScopeFrom(ctx).GetSecretStr(name, def)
}

// SecretSourceLabel is the " (from X)" suffix naming the external source that supplied the env var
func SecretSourceLabel(h, name string) string { return FormatSecretSourceSuffix(h, name) }

// BuildProfileSecretScope assembles a profile's secrets from .env, its secret sources and the managed env
func BuildProfileSecretScope(h string) map[string]string {
	secrets := config.LoadEnvFile(filepath.Join(h, ".env"))
	if secrets == nil {
		secrets = map[string]string{}
	}
	for k, v := range SecretSourceValues(h) {
		if !IsGlobalEnv(k) {
			secrets[k] = v
		}
	}
	if bridged, ok := gatewayBridgedAllowAllUsers(); ok && isProcessHome(h) {
		if _, exists := secrets["GATEWAY_ALLOW_ALL_USERS"]; !exists {
			secrets["GATEWAY_ALLOW_ALL_USERS"] = bridged
		}
	}
	for k, v := range config.LoadManagedEnv() {
		if !IsGlobalEnv(k) {
			secrets[k] = v
		}
	}
	return secrets
}

// Whether the home is the profile this process serves as its own
func isProcessHome(h string) bool {
	a, err := filepath.EvalSymlinks(filepath.Clean(h))
	if err != nil {
		a = filepath.Clean(h)
	}
	b, err := filepath.EvalSymlinks(filepath.Clean(RoutingProcessHome()))
	if err != nil {
		b = filepath.Clean(RoutingProcessHome())
	}
	return a == b
}

// RefreshInstalledSecretScope replaces the bound mapping with a fresh build of the home's secrets
func RefreshInstalledSecretScope(ctx context.Context, h string) bool {
	bound := boundScope(ctx)
	if bound == nil || bound.Mapping == nil {
		return false
	}
	rebuilt := BuildProfileSecretScope(h)
	bound.mu.Lock()
	defer bound.mu.Unlock()
	for k, v := range rebuilt {
		bound.Mapping[k] = v
	}
	for k := range bound.Mapping {
		if _, keep := rebuilt[k]; !keep {
			delete(bound.Mapping, k)
		}
	}
	return true
}

// Reports whether a home is the process home, the exported face for pkg/mcp
func IsProcessHome(h string) bool { return isProcessHome(h) }
