package memory

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Discovery sources in precedence order, bundled first so a provider dropped in a tree never shadows a shipped one
const (
	SourceBundled    = "bundled"
	SourceUser       = "user"
	SourceProject    = "project"
	SourceEntryPoint = "entry_point"
)

var sourceRank = map[string]int{SourceBundled: 0, SourceUser: 1, SourceProject: 2, SourceEntryPoint: 3}

// Config key recording the plugins an update turned off and why
const disabledByUpdateRecord = "disabled_by_update"

// How a provider load runs, skills register only for the configured active provider
type LoadOptions struct {
	RegisterSkills bool
	// Records a plugin skill the provider registered so switching providers can retract it
	TrackSkill func(qualifiedName string)
}

// CLI subcommand the active memory provider contributes, Setup adds flags and Handler runs it
type ProviderCLI struct {
	Setup   func(cmd *cobra.Command)
	Handler func(cmd *cobra.Command, args []string) error
}

// One discoverable memory provider, bundled ones compiled in and external ones installed by the plugin host
type ProviderRegistration struct {
	Name   string
	Source string
	// Provider directory of a non bundled source, its base name one of the disabled list spellings
	Dir string
	// Home whose plugins directory supplied a user source provider
	Home string
	// Extra names a plugins disabled list may use, such as the manifest name
	Spellings []string
	// Raw plugin.yaml text, the description read from it
	Manifest string
	New      func(ctx context.Context, env *tools.Env, opts LoadOptions) (Provider, error)
	CLI      *ProviderCLI
}

// Description from the plugin manifest, empty when absent or unreadable
func (r *ProviderRegistration) Description() string {
	var meta map[string]any
	if err := yaml.Unmarshal([]byte(strings.TrimPrefix(r.Manifest, "\xef\xbb\xbf")), &meta); err != nil || meta == nil {
		return ""
	}
	if d, ok := meta["description"].(string); ok {
		return d
	}
	return ""
}

// Plugin skill lookups used to resolve and retract provider skills
type PluginSkillRegistry interface {
	FindPluginSkill(qualifiedName string) (string, bool)
	RemovePluginSkill(qualifiedName string)
}

var (
	registryMu    sync.RWMutex
	registrations []*ProviderRegistration
	skillRegistry PluginSkillRegistry
	// Provider skills registered per home key, pruning one profile never retracts a sibling profile's skills
	registeredSkills = map[string]map[string]string{}
)

// Adds a provider to the registry, a bundled name registered twice keeps the first
func RegisterProvider(reg ProviderRegistration) {
	if reg.Source == "" {
		reg.Source = SourceBundled
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	for _, existing := range registrations {
		if existing.Name == reg.Name && existing.Source == reg.Source && existing.Dir == reg.Dir && existing.Home == reg.Home {
			return
		}
	}
	r := reg
	registrations = append(registrations, &r)
}

// Removes external registrations of a source, used when the plugin host rescans its directories
func UnregisterProviders(source, homeDir string) {
	registryMu.Lock()
	defer registryMu.Unlock()
	kept := registrations[:0]
	for _, r := range registrations {
		if r.Source == source && (homeDir == "" || homeKey(r.Home) == homeKey(homeDir)) {
			continue
		}
		kept = append(kept, r)
	}
	registrations = kept
}

// Replaces the plugin manager of the home in effect as the provider skill registry, nil restores it
func SetPluginSkillRegistry(r PluginSkillRegistry) {
	registryMu.Lock()
	defer registryMu.Unlock()
	skillRegistry = r
}

// Skill registry provider skills resolve against, the replacement first and the home plugin manager otherwise
func pluginSkillsLocked(ctx context.Context, env *tools.Env) PluginSkillRegistry {
	if skillRegistry != nil {
		return skillRegistry
	}
	return plugins.GetPluginManager(home.WithOverride(ctx, registryHome(ctx, env)))
}

// Home the registry resolves user sources and config against, the context override first
func registryHome(ctx context.Context, env *tools.Env) string {
	if home.Override(ctx) != "" {
		return home.Dir(ctx)
	}
	return envHome(env)
}

// Config of the home in effect, the session tree when the environment carries one
func registryConfig(ctx context.Context, env *tools.Env) (config.Tree, error) {
	if env != nil && env.Config != nil && home.Override(ctx) == "" {
		return env.Config, nil
	}
	return config.Load(registryHome(ctx, env), configLookup(env))
}

// Registrations visible from the home in effect, in precedence order with names sorted inside a source
func visibleRegistrations(ctx context.Context, env *tools.Env) []*ProviderRegistration {
	key := homeKey(registryHome(ctx, env))
	registryMu.RLock()
	out := make([]*ProviderRegistration, 0, len(registrations))
	for _, r := range registrations {
		if r.Source == SourceUser && homeKey(r.Home) != key {
			continue
		}
		out = append(out, r)
	}
	registryMu.RUnlock()
	out = append(out, scannedRegistrations(ctx, env, out)...)
	sort.SliceStable(out, func(i, j int) bool {
		if sourceRank[out[i].Source] != sourceRank[out[j].Source] {
			return sourceRank[out[i].Source] < sourceRank[out[j].Source]
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// Registration a name resolves to, bundled then user, project, then entry point
func FindProvider(ctx context.Context, env *tools.Env, name string) *ProviderRegistration {
	for _, r := range visibleRegistrations(ctx, env) {
		if r.Name == name {
			return r
		}
	}
	return nil
}

// Discovered provider with its manifest description and availability
type DiscoveredProvider struct {
	Name        string
	Description string
	Available   bool
}

// Every provider as name, description and availability, the first source winning a name collision
func DiscoverMemoryProviders(ctx context.Context, env *tools.Env) []DiscoveredProvider {
	var out []DiscoveredProvider
	seen := map[string]bool{}
	for _, r := range visibleRegistrations(ctx, env) {
		if seen[r.Name] {
			continue
		}
		seen[r.Name] = true
		description := ""
		if r.Source != SourceEntryPoint {
			description = r.Description()
		}
		out = append(out, DiscoveredProvider{Name: r.Name, Description: description, Available: probeAvailability(ctx, env, r)})
	}
	return out
}

// True when a provider loads without side effects and reports available
func probeAvailability(ctx context.Context, env *tools.Env, r *ProviderRegistration) (ok bool) {
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	p, _ := r.New(ctx, env, LoadOptions{})
	if p == nil {
		return false
	}
	return p.Available(ctx)
}

// Active provider name from memory.provider, empty when unset or unreadable
func ActiveMemoryProvider(ctx context.Context, env *tools.Env) string {
	tree, err := registryConfig(ctx, env)
	if err != nil || tree == nil {
		return ""
	}
	name, _ := tree.Get("memory.provider").(string)
	return name
}

// Loads a provider by name, nil when unknown, parked or failing, skills registering only for the active provider
func LoadMemoryProvider(ctx context.Context, env *tools.Env, name string) Provider {
	return loadMemoryProvider(ctx, env, name, name == ActiveMemoryProvider(ctx, env))
}

// Loads a provider by name with an explicit skill registration choice
func LoadMemoryProviderWithSkills(ctx context.Context, env *tools.Env, name string, registerSkills bool) Provider {
	return loadMemoryProvider(ctx, env, name, registerSkills)
}

func loadMemoryProvider(ctx context.Context, env *tools.Env, name string, registerSkills bool) Provider {
	log := envLog(env)
	r := FindProvider(ctx, env, name)
	if r == nil {
		log.Debug(fmt.Sprintf("Memory provider '%s' not found in bundled, user plugins, or entry points", name))
		return nil
	}
	if hasProviderDir(r) && explicitlyDisabled(ctx, env, r) {
		notice, _ := ParkedProviderNotice(ctx, env, name)
		log.Warn(notice)
		return nil
	}
	opts := LoadOptions{RegisterSkills: registerSkills}
	if registerSkills {
		opts.TrackSkill = func(qualified string) { trackProviderSkill(ctx, env, qualified) }
	}
	p, err := instantiate(ctx, env, r, opts)
	if err != nil && p == nil {
		log.Warn(fmt.Sprintf("Failed to load memory provider '%s': %s", name, err))
		return nil
	}
	if err != nil {
		log.Warn(fmt.Sprintf("Memory provider '%s' raised after registering (%s) — using the registered provider; "+
			"later registrations were skipped", name, err))
	}
	if p == nil {
		log.Warn(fmt.Sprintf("Memory provider '%s' loaded but no provider instance found", name))
		return nil
	}
	return p
}

// Runs a registration factory with a provider panic reported as a load error
func instantiate(ctx context.Context, env *tools.Env, r *ProviderRegistration, opts LoadOptions) (p Provider, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			err = fmt.Errorf("%v", rec)
		}
	}()
	return r.New(ctx, env, opts)
}

// Records a registered provider skill under the home in effect with the path the skill registry resolved
func trackProviderSkill(ctx context.Context, env *tools.Env, qualified string) {
	registryMu.Lock()
	defer registryMu.Unlock()
	path, ok := pluginSkillsLocked(ctx, env).FindPluginSkill(qualified)
	if !ok {
		return
	}
	key := homeKey(registryHome(ctx, env))
	if registeredSkills[key] == nil {
		registeredSkills[key] = map[string]string{}
	}
	registeredSkills[key][qualified] = path
}

// Retracts tracked skills that no longer belong to the active provider under the home in effect
func PruneInactiveMemoryProviderSkills(ctx context.Context, env *tools.Env, activeProvider string) {
	key := homeKey(registryHome(ctx, env))
	registryMu.Lock()
	defer registryMu.Unlock()
	registered := registeredSkills[key]
	reg := pluginSkillsLocked(ctx, env)
	for qualified, path := range registered {
		if namespace, _, _ := strings.Cut(qualified, ":"); namespace == activeProvider {
			continue
		}
		if current, ok := reg.FindPluginSkill(qualified); ok && current == path {
			reg.RemovePluginSkill(qualified)
		}
		delete(registered, qualified)
	}
}

// Names a plugins disabled list may use for a registration, the name, its leaf, the directory and manifest names
func providerSpellings(r *ProviderRegistration) map[string]bool {
	names := map[string]bool{r.Name: true}
	if i := strings.LastIndex(r.Name, "/"); i >= 0 {
		names[r.Name[i+1:]] = true
	}
	if r.Dir != "" {
		names[filepath.Base(r.Dir)] = true
	}
	for _, s := range r.Spellings {
		names[s] = true
	}
	delete(names, "")
	return names
}

// Whether a registration has a provider directory, entry points only when their package directory resolved
func hasProviderDir(r *ProviderRegistration) bool {
	return r.Source != SourceEntryPoint || r.Dir != ""
}

// True when a non bundled provider is parked in plugins.disabled under any of its spellings
func explicitlyDisabled(ctx context.Context, env *tools.Env, r *ProviderRegistration) bool {
	if r.Source == SourceBundled {
		return false
	}
	tree, err := registryConfig(ctx, env)
	if err != nil {
		return false
	}
	disabled, ok := tree.Get("plugins.disabled").([]any)
	if !ok {
		return false
	}
	spellings := providerSpellings(r)
	for _, v := range disabled {
		if s, ok := v.(string); ok && spellings[s] {
			return true
		}
	}
	return false
}

// User facing reason a configured provider is not loading because plugins.disabled parks it, false when not parked
func ParkedProviderNotice(ctx context.Context, env *tools.Env, name string) (string, bool) {
	r := FindProvider(ctx, env, name)
	if r == nil || !hasProviderDir(r) || !explicitlyDisabled(ctx, env, r) {
		return "", false
	}
	why := ""
	if tree, err := registryConfig(ctx, env); err == nil {
		if record, ok := tree.Get("plugins." + disabledByUpdateRecord).(map[string]any); ok {
			if recorded, ok := record[name].(map[string]any); ok && pyTruthy(recorded["reason"]) {
				why = fmt.Sprintf(" An update turned it off (%s) and a later update retries it.", pyStr(recorded["reason"]))
			}
		}
	}
	return fmt.Sprintf("⚠ Memory provider '%s' is turned off: plugins.disabled lists it, so external memory is off "+
		"(built-in memory still works).%s Turn it back on with `icebath plugins enable %s`.", name, why, name), true
}

// Whether the configured or named provider resolves to compiled in code ready to construct without import work
func WarmMemoryProvider(ctx context.Context, env *tools.Env, name string) bool {
	if name == "" {
		name = ActiveMemoryProvider(ctx, env)
	}
	if name == "" {
		return false
	}
	r := FindProvider(ctx, env, name)
	return r != nil && r.Source == SourceBundled
}

// CLI command contributed by the active memory provider
type PluginCLICommand struct {
	Name        string
	Help        string
	Description string
	Setup       func(cmd *cobra.Command)
	Handler     func(cmd *cobra.Command, args []string) error
	Plugin      string
}

// CLI commands of the active memory provider only, at most one
func DiscoverPluginCLICommands(ctx context.Context, env *tools.Env) []PluginCLICommand {
	active := ActiveMemoryProvider(ctx, env)
	if active == "" {
		return nil
	}
	r := FindProvider(ctx, env, active)
	if r == nil {
		return nil
	}
	cli := r.CLI
	if cli == nil && r.Source != SourceBundled && r.Dir != "" {
		cli = hostedProviderCLI(ctx, env, r)
	}
	if cli == nil || cli.Setup == nil {
		return nil
	}
	desc := r.Description()
	help := desc
	if help == "" {
		help = fmt.Sprintf("Manage %s memory plugin", active)
	}
	return []PluginCLICommand{{Name: active, Help: help, Description: desc, Setup: cli.Setup, Handler: cli.Handler, Plugin: active}}
}

// Provider registry registers nothing at import, bundled providers register from their own files
func registerMemoryProviderRegistry() {}
