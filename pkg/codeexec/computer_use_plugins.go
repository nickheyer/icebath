package codeexec

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	defaultComputerUseBackend = "cua"
	cuUserNamespace           = "_icebath_user_computer_use"
	cuaProviderDescription    = "cua-driver (built-in) \u2014 background computer use on macOS, Windows and Linux; does not steal the cursor or focus. Default when computer_use.backend is unset."
)

// Error raised when the configured computer use provider is missing or fails to load
type lookupError struct{ msg string }

func (e *lookupError) Error() string { return e.msg }

// Bundled provider shipped inside the binary, the in process load its register(ctx) performs
type bundledComputerUseProvider struct {
	name        string
	description string
	load        func(env *tools.Env) ComputerUseProvider
}

// Bundled providers in directory order, cua the only one upstream ships
var bundledComputerUseProviders = []bundledComputerUseProvider{
	{name: "cua", description: cuaProviderDescription, load: func(env *tools.Env) ComputerUseProvider { return newCuaDriverProvider(env) }},
}

// One discovered provider, a bundled entry or a user plugin directory
type cuProviderDir struct {
	name    string
	dir     string
	bundled *bundledComputerUseProvider
}

// Context the loader and a hosted provider run under, scoped to the env home
func cuProviderContext(env *tools.Env) context.Context {
	return home.WithOverride(context.Background(), envHome(env))
}

// Whether the first 8192 characters of __init__.py mention the computer use provider contract
func isComputerUseDir(path string) bool {
	raw, err := os.ReadFile(filepath.Join(path, "__init__.py"))
	if err != nil {
		return false
	}
	source := strings.ToValidUTF8(strings.TrimPrefix(string(raw), "\ufeff"), "\ufffd")
	if utf8.RuneCountInString(source) > 8192 {
		source = string([]rune(source)[:8192])
	}
	return strings.Contains(source, "register_computer_use_provider") || strings.Contains(source, "ComputerUseProvider")
}

// Bundled then user providers by name, a bundled name winning a collision
func iterComputerUseProviderDirs(env *tools.Env) ([]cuProviderDir, error) {
	var dirs []cuProviderDir
	seen := map[string]bool{}
	for i := range bundledComputerUseProviders {
		b := &bundledComputerUseProviders[i]
		dirs = append(dirs, cuProviderDir{name: b.name, bundled: b})
		seen[b.name] = true
	}
	userDir, ok := plugins.UserPluginsDir(cuProviderContext(env))
	if !ok {
		return dirs, nil
	}
	children, err := plugins.IterPluginDirs(userDir)
	if err != nil {
		return nil, err
	}
	for _, child := range children {
		if name := filepath.Base(child); !seen[name] && isComputerUseDir(child) {
			dirs = append(dirs, cuProviderDir{name: name, dir: child})
		}
	}
	return dirs, nil
}

// Name and plugin.yaml description of every provider, listing never loads one
func discoverComputerUseProviders(env *tools.Env) ([][2]string, error) {
	dirs, err := iterComputerUseProviderDirs(env)
	if err != nil {
		return nil, err
	}
	out := make([][2]string, 0, len(dirs))
	for _, d := range dirs {
		description := ""
		if d.bundled != nil {
			description = d.bundled.description
		} else {
			description = plugins.ReadPluginDescription(d.dir)
		}
		out = append(out, [2]string{d.name, description})
	}
	return out, nil
}

// Provider location of a name, bundled first then user installed
func findComputerUseProviderDir(env *tools.Env, name string) (cuProviderDir, bool, error) {
	dirs, err := iterComputerUseProviderDirs(env)
	if err != nil {
		return cuProviderDir{}, false, err
	}
	for _, d := range dirs {
		if d.name == name {
			return d, true, nil
		}
	}
	return cuProviderDir{}, false, nil
}

// Named provider loaded, nil when it is not found or fails to load
func loadComputerUseProvider(env *tools.Env, name string) (ComputerUseProvider, error) {
	d, ok, err := findComputerUseProviderDir(env, name)
	if err != nil || !ok {
		return nil, err
	}
	instance, _ := plugins.LoadNamed(name, d.dir, func(string) (any, error) {
		p, err := loadComputerUseProviderFromDir(env, d)
		if p == nil {
			return nil, err
		}
		return p, err
	}, "Computer-use provider", "provider").(ComputerUseProvider)
	return instance, nil
}

// Bundled providers load in process, user code in the plugin host unless plugins.isolation is host
func loadComputerUseProviderFromDir(env *tools.Env, d cuProviderDir) (ComputerUseProvider, error) {
	if d.bundled != nil {
		return d.bundled.load(env), nil
	}
	if st, err := os.Stat(filepath.Join(d.dir, "__init__.py")); err != nil || st.IsDir() {
		return nil, nil
	}
	ctx := cuProviderContext(env)
	module := cuUserNamespace + "." + d.name
	what := fmt.Sprintf("plugin %s (loaded as %s)", pyrepr.Repr(d.name), module)
	if refusal := plugins.InProcessImportRefusal(ctx, what, "user", nil); refusal != "" {
		slog.Warn(refusal)
		return nil, nil
	}
	obj, err := plugins.LoadComputerUseProviderInHost(ctx, d.dir, module)
	if err != nil || obj == nil {
		return nil, err
	}
	return newHostedComputerUseProvider(ctx, obj)
}

// The computer_use.backend value of the active profile, cua when unset or blank
func configuredBackendName(env *tools.Env) string {
	raw, _ := computerUseCfg(env)["backend"].(string)
	if s := pystr.Strip(raw); s != "" {
		return s
	}
	return defaultComputerUseBackend
}

// The one provider computer_use.backend selects, a lookupError naming the available ones otherwise
func getActiveProvider(env *tools.Env) (ComputerUseProvider, error) {
	name := configuredBackendName(env)
	provider, err := loadComputerUseProvider(env, name)
	if err != nil {
		return nil, err
	}
	if provider != nil {
		return provider, nil
	}
	found, err := discoverComputerUseProviders(env)
	if err != nil {
		return nil, err
	}
	names := make([]string, len(found))
	for i, f := range found {
		names[i] = f[0]
	}
	_, installed, err := findComputerUseProviderDir(env, name)
	if err != nil {
		return nil, err
	}
	state := "is not installed"
	if installed {
		state = "could not be loaded (see the log)"
	}
	return nil, &lookupError{msg: fmt.Sprintf("computer_use.backend is %s, but that computer-use provider %s (available: %s). "+
		"Install it under ~/.icebath/plugins/<name>/ or pick a backend with `icebath tools`.", pyrepr.Repr(name), state, strings.Join(names, ", "))}
}

// Built in cua provider driving cua-driver over MCP
type CuaDriverProvider struct {
	ProviderDefaults
	env *tools.Env
}

// Provider bound to the env whose home and config its backends read
func newCuaDriverProvider(env *tools.Env) *CuaDriverProvider {
	return &CuaDriverProvider{ProviderDefaults: ProviderDefaults{ID: "cua", Label: "cua-driver (background)"}, env: env}
}

// Fresh cua-driver backend in the requested permission mode
func (p *CuaDriverProvider) CreateBackend(permissionMode string) (ComputerUseBackend, error) {
	return newCuaDriverBackend(p.env, permissionMode)
}

// Supported platform with a cua-driver binary, or a desktop placed inside the terminal backend
func (p *CuaDriverProvider) IsAvailable() (bool, error) {
	switch cuPlatform {
	case "darwin", "win32", "linux":
	default:
		return false, nil
	}
	ok, err := cuaDriverBinaryAvailable()
	if err != nil || ok {
		return ok, err
	}
	return BotDesktopPlacementWhere(p.env) == "terminal", nil
}
