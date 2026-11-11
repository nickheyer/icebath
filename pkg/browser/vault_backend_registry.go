package browser

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/providerregistry"
)

// Profile scoped login backends registered by trusted in process plugins
var loginBackendRegistry = providerregistry.New[LoginBackend](providerregistry.Options{
	Label: "Login backend", ProviderCls: "LoginBackend", Logger: slog.Default().With("logger", "agent.vault_backend_registry"),
})

var loginBackendNameRe = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// Names and handle prefixes the built in backends own, never registrable by a plugin
func builtinLoginNamespaces() (map[string]bool, []string) {
	type identity struct{ name, prefix string }
	local := NewLocalLoginBackend(context.Background())
	classes := []identity{{local.Name(), local.Prefix()}}
	for _, cls := range externalBackendClasses() {
		classes = append(classes, identity{cls.name, cls.prefix})
	}
	names := map[string]bool{}
	prefixes := make([]string, 0, len(classes))
	for _, c := range classes {
		names[c.name] = true
		names[strings.TrimRight(c.prefix, ":_")] = true
		prefixes = append(prefixes, c.prefix)
	}
	return names, prefixes
}

// Why a name cannot be a plugin backend's namespace, empty when it can
func loginNamespaceError(name any) string {
	s, ok := name.(string)
	if !ok || !loginBackendNameRe.MatchString(s) {
		return "login backend name must be a lowercase identifier"
	}
	names, prefixes := builtinLoginNamespaces()
	if names[s] {
		return "builtin login backend namespace is reserved"
	}
	for _, prefix := range prefixes {
		if strings.HasPrefix(s+":", prefix) {
			return "builtin login backend namespace is reserved"
		}
	}
	return ""
}

// Identity of a backend read under a guard, not plain when reading it panics
func staticLoginIdentity(backend LoginBackend) (name, prefix string, needsUnlock, plain bool) {
	defer func() {
		if recover() != nil {
			plain = false
		}
	}()
	return backend.Name(), backend.Prefix(), backend.NeedsUnlock(), true
}

// The backend's name judged before anything else reads it, a ValueError with a fixed message when refused
func StaticLoginBackendName(provider any) (string, error) {
	backend, ok := provider.(LoginBackend)
	if !ok || providerregistry.IsNil(provider) {
		return "", &providerregistry.ValueError{Message: "login backend must subclass LoginBackend"}
	}
	name, prefix, needsUnlock, plain := staticLoginIdentity(backend)
	if !plain {
		return "", &providerregistry.ValueError{Message: "login backend name and prefix must be plain string attributes"}
	}
	message := loginNamespaceError(name)
	if message == "" && prefix != name+":" {
		message = "login backend prefix must be '<name>:'"
	}
	if message != "" {
		return "", &providerregistry.ValueError{Message: message}
	}
	if needsUnlock {
		return "", &providerregistry.ValueError{Message: "plugin login backends cannot use the Icebath master-password prompt (needs_unlock)"}
	}
	return name, nil
}

// Registers a plugin backend in a scope, refusing a name another plugin already holds there
func RegisterLoginBackendProvider(ctx context.Context, provider LoginBackend, scope string) error {
	name, err := StaticLoginBackendName(provider)
	if err != nil {
		return err
	}
	if loginBackendRegistry.Merged(ctx, scope).Has(name) {
		return &providerregistry.ValueError{Message: fmt.Sprintf("login backend name %s is already registered", pyrepr.Repr(name))}
	}
	if err := loginBackendRegistry.Register(provider, scope); err != nil {
		return err
	}
	if loginBackendRegistry.SnapshotRegistration(name, scope) == provider {
		return nil
	}
	merged := loginBackendRegistry.Merged(ctx, scope)
	for _, key := range merged.Keys() {
		if held, _ := merged.Get(key); held == provider {
			loginBackendRegistry.RestoreRegistration(key, provider, nil, scope)
		}
	}
	return &providerregistry.ValueError{Message: "login backend name and prefix must be plain string attributes"}
}

// Registered backends the profile turned on with vault.<name>.enabled true, wrapped for the vault tool
func EnabledPluginLoginBackends(ctx context.Context, cfg map[string]any) []LoginBackend {
	merged := loginBackendRegistry.Merged(ctx, "")
	names := merged.Keys()
	sort.Strings(names)
	out := []LoginBackend{}
	for _, name := range names {
		provider, _ := merged.Get(name)
		section, isMap := cfg[name].(map[string]any)
		enabled, isBool := section["enabled"].(bool)
		if loginNamespaceError(name) == "" && isMap && isBool && enabled {
			out = append(out, NewPluginLoginBackend(name, provider))
		}
	}
	return out
}

// Registry view judging identity with the static name and registering through the namespace checks
type loginBackendScopedRegistry struct {
	providerregistry.ScopedRegistry
}

func (loginBackendScopedRegistry) Identity(provider any) error {
	_, err := StaticLoginBackendName(provider)
	return err
}

func (loginBackendScopedRegistry) Register(provider any, scope string) (bool, error) {
	backend, ok := provider.(LoginBackend)
	if !ok || providerregistry.IsNil(provider) {
		return false, &providerregistry.ValueError{Message: "login backend must subclass LoginBackend"}
	}
	if err := RegisterLoginBackendProvider(context.Background(), backend, scope); err != nil {
		return false, err
	}
	return true, nil
}

// Plugin facing registry that register_login_backend writes into
func LoginBackendScopedRegistry() plugins.ScopedRegistry {
	return loginBackendScopedRegistry{loginBackendRegistry.Scoped()}
}

// Clears every plugin login backend registration, for tests only
func ResetLoginBackendRegistryForTests() { loginBackendRegistry.ResetForTests() }
