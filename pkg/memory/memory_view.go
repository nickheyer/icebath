package memory

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/tools"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
)

// ProviderFieldView is one setting a memory provider asks for
type ProviderFieldView struct {
	Key, Label string
	// Value in force, the stored one or else the default, never that of a secret
	Value  string
	Secret bool
	// Whether the secret already holds a value in the environment
	SecretSet bool
	// Only values the setting takes, empty for free text
	Choices []string
	// Where a key for the setting is handed out
	URL string
	// Settings that must hold these values for this one to count
	When map[string]string
}

// ProviderView is one memory provider that can be set up
type ProviderView struct {
	Name string
	// The provider new sessions use
	Active bool
	// Brings a setup of its own that takes the place of its settings
	OwnSetup bool
	Fields   []ProviderFieldView
}

// OverviewView is the state of memory for one home: the built-in stores, the provider in use and every provider that can be set up
type OverviewView struct {
	// Provider new sessions use, empty for the built-in memory alone
	Provider                                       string
	MemoryEnabled, UserProfileEnabled, ToolEnabled bool
	// Whether the plugin of the provider in use is installed, and whether the provider is ready with what it needs
	Installed, Ready bool
	// Why the provider in use is not ready, or why its plugin is turned off
	Problem   string
	Providers []ProviderView
}

// SetupRefusal is a memory change refused for what was asked or for what its preparation found
type SetupRefusal struct{ Message string }

func (e *SetupRefusal) Error() string { return e.Message }

// Default of a schema field: its own, or the one its default_from maps from another setting of the provider config
func schemaFieldDefault(field, providerConfig map[string]any) any {
	def := field["default"]
	if df, ok := field["default_from"].(map[string]any); ok && len(df) > 0 {
		ref := providerConfig[anyutil.String(dictGet(df, "field", ""))]
		if refMap, ok := dictGet(df, "map", map[string]any{}).(map[string]any); ok && anyutil.IsTrue(ref) {
			if s, isStr := ref.(string); isStr {
				if v, in := refMap[s]; in {
					def = v
				}
			}
		}
	}
	return def
}

// One schema field as a screen shows it, with the value in force from the provider config
func schemaFieldView(field, providerConfig map[string]any) ProviderFieldView {
	key := anyutil.String(field["key"])
	view := ProviderFieldView{Key: key, Label: key, Secret: anyutil.IsTrue(field["secret"])}
	if desc, ok := field["description"]; ok {
		view.Label = anyutil.String(desc)
	}
	view.URL, _ = field["url"].(string)
	if when, ok := field["when"].(map[string]any); ok && len(when) > 0 {
		view.When = make(map[string]string, len(when))
		for k, v := range when {
			view.When[k] = anyutil.String(v)
		}
	}
	if view.Secret {
		envVar, _ := field["env_var"].(string)
		view.SecretSet = envVar != "" && os.Getenv(envVar) != ""
		return view
	}
	if choices, _ := field["choices"].([]any); len(choices) > 0 {
		for _, c := range choices {
			view.Choices = append(view.Choices, anyutil.String(c))
		}
	}
	effective, has := providerConfig[key]
	if !has || (len(view.Choices) == 0 && !anyutil.IsTrue(effective)) {
		effective = schemaFieldDefault(field, providerConfig)
	}
	if anyutil.IsTrue(effective) {
		view.Value = anyutil.String(effective)
	}
	return view
}

// Whether the memory tool is on for the terminal and has what it needs
func (m *MemorySetup) memoryToolEnabled(ctx context.Context, cfg map[string]any) bool {
	return tools.Default.PlatformTools(ctx, "", config.Tree(cfg), "cli", tools.PlatformToolsOptions{})["memory"] && checkMemoryRequirements(m.Env)
}

// Overview is the state of memory for the home: the built-in stores, the provider in use and every provider that can be set up with its settings
func (m *MemorySetup) Overview(ctx context.Context) (*OverviewView, error) {
	cfg, err := m.loadConfig(ctx)
	if err != nil {
		return nil, err
	}
	memCfg, _ := dictGet(cfg, "memory", map[string]any{}).(map[string]any)
	active, _ := dictGet(memCfg, "provider", "").(string)
	out := &OverviewView{Provider: active, MemoryEnabled: anyutil.IsTrue(dictGet(memCfg, "memory_enabled", true)),
		UserProfileEnabled: anyutil.IsTrue(dictGet(memCfg, "user_profile_enabled", true)), ToolEnabled: m.memoryToolEnabled(ctx, cfg)}
	providers := m.availableProviders(ctx)
	defer dismissSetupProviders(ctx, providers)
	for _, sp := range providers {
		providerConfig, _ := memCfg[sp.Name].(map[string]any)
		if providerConfig == nil {
			providerConfig = map[string]any{}
		}
		_, own := sp.Provider.(postSetupProvider)
		view := ProviderView{Name: sp.Name, Active: sp.Name == active, OwnSetup: own && providerHasHook(ctx, sp.Provider, "post_setup")}
		for _, field := range sp.Provider.ConfigSchema() {
			view.Fields = append(view.Fields, schemaFieldView(field, providerConfig))
		}
		out.Providers = append(out.Providers, view)
	}
	if active == "" {
		return out, nil
	}
	if parked, ok := ParkedProviderNotice(ctx, m.Env, active); ok && parked != "" {
		out.Problem = parked
		return out, nil
	}
	if match := findSetupProvider(providers, active); match != nil {
		out.Installed = true
		if out.Ready = match.Provider.Available(ctx); !out.Ready {
			out.Problem = match.Provider.UnavailableReason(ctx)
		}
	}
	return out, nil
}

// Stores the provider as the one new sessions use, then its own config and the keys it needs
func (m *MemorySetup) activateProvider(ctx context.Context, cfg map[string]any, chosen setupProvider, providerConfig map[string]any, envWrites []envWrite) error {
	cfg["memory"].(map[string]any)["provider"] = chosen.Name
	if err := m.saveConfig(ctx, cfg); err != nil {
		return err
	}
	if len(providerConfig) > 0 {
		if err := chosen.Provider.SaveConfig(providerConfig, envHome(m.Env)); err != nil {
			return fmt.Errorf("failed to write provider config: %w", err)
		}
	}
	if len(envWrites) > 0 {
		return m.writeEnvVars(ctx, envWrites, "")
	}
	return nil
}

// Configure sets a provider up from the values given by setting and makes it the one new sessions use: its dependencies are installed, its own setup runs when it brings one, else every setting that counts takes its value, a setting left empty keeping what it has
func (m *MemorySetup) Configure(ctx context.Context, name string, values map[string]string) error {
	providers := m.availableProviders(ctx)
	defer dismissSetupProviders(ctx, providers)
	match := findSetupProvider(providers, name)
	if match == nil {
		return &SetupRefusal{Message: "memory provider not found: " + name}
	}
	if err := m.installDependencies(ctx, match.Name); err != nil {
		var install *pm.InstallError
		if errors.As(err, &install) {
			return &SetupRefusal{Message: install.Error()}
		}
		return err
	}
	cfg, err := m.loadConfig(ctx)
	if err != nil {
		return err
	}
	if took, err := m.postSetupHook(ctx, match.Provider, cfg); took || err != nil {
		return err
	}
	memCfg := cfg["memory"].(map[string]any)
	providerConfig, ok := memCfg[match.Name].(map[string]any)
	if !ok {
		providerConfig = map[string]any{}
	}
	var envWrites []envWrite
	for _, field := range match.Provider.ConfigSchema() {
		if when, ok := field["when"].(map[string]any); ok && len(when) > 0 && !whenMatches(providerConfig, when) {
			continue
		}
		key, value := anyutil.String(field["key"]), values[anyutil.String(field["key"])]
		envVar, _ := field["env_var"].(string)
		choices, _ := field["choices"].([]any)
		switch secret := anyutil.IsTrue(field["secret"]); {
		case secret:
			if value != "" && envVar != "" {
				envWrites = append(envWrites, envWrite{envVar, value})
			}
		case len(choices) > 0:
			if value == "" {
				current, has := providerConfig[key]
				if !has {
					current = schemaFieldDefault(field, providerConfig)
				}
				value = anyutil.String(choices[0])
				if anyutil.IsTrue(current) {
					value = anyutil.String(current)
				}
			}
			picked := false
			for _, c := range choices {
				if anyutil.String(c) == value {
					providerConfig[key], picked = c, true
					break
				}
			}
			if !picked {
				return &SetupRefusal{Message: key + " does not take " + value}
			}
		case value != "":
			providerConfig[key] = value
			if envVar != "" {
				envWrites = append(envWrites, envWrite{envVar, value})
			}
		}
	}
	return m.activateProvider(ctx, cfg, *match, providerConfig, envWrites)
}

// TurnOffProvider clears the provider of the home in the context, so only the built-in memory stays in use
func TurnOffProvider(ctx context.Context) error {
	h := home.Dir(ctx)
	tree, err := config.Load(h, configLookup(&tools.Env{Home: h}))
	if err != nil {
		return err
	}
	cfg := map[string]any(tree.Clone())
	mem, ok := cfg["memory"].(map[string]any)
	if !ok {
		mem = map[string]any{}
		cfg["memory"] = mem
	}
	mem["provider"] = ""
	return config.SaveConfig(ctx, h, cfg, config.SaveOptions{})
}

// Built-in memory files a reset of the target erases, each with what it holds: all, memory or user
func builtinMemoryFiles(target string) [][2]string {
	var files [][2]string
	if target == "all" || target == "memory" {
		files = append(files, [2]string{"MEMORY.md", "agent notes"})
	}
	if target == "all" || target == "user" {
		files = append(files, [2]string{"USER.md", "user profile"})
	}
	return files
}

// Built-in memory files of the target that exist in the home in the context
func existingBuiltinMemory(ctx context.Context, target string) [][2]string {
	dir := filepath.Join(home.Dir(ctx), "memories")
	var existing [][2]string
	for _, f := range builtinMemoryFiles(target) {
		if _, err := os.Stat(filepath.Join(dir, f[0])); err == nil {
			existing = append(existing, f)
		}
	}
	return existing
}

// EraseBuiltinMemory deletes the built-in memory files of the target in the home in the context and answers the names of the ones it deleted
func EraseBuiltinMemory(ctx context.Context, target string) ([]string, error) {
	if len(builtinMemoryFiles(target)) == 0 {
		return nil, &SetupRefusal{Message: "memory has no store named " + target}
	}
	return eraseBuiltinMemory(ctx, existingBuiltinMemory(ctx, target), io.Discard)
}

// Deletes the listed built-in memory files of the home in the context, saying each on the writer, and answers their names
func eraseBuiltinMemory(ctx context.Context, files [][2]string, out io.Writer) ([]string, error) {
	dir := filepath.Join(home.Dir(ctx), "memories")
	names := make([]string, 0, len(files))
	for _, f := range files {
		if err := os.Remove(filepath.Join(dir, f[0])); err != nil {
			return names, err
		}
		fmt.Fprintf(out, "  ✓ Deleted %s (%s)\n", f[0], f[1])
		names = append(names, f[0])
	}
	return names, nil
}
