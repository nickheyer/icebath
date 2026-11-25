package memory

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"unicode"

	"github.com/mattn/go-isatty"
	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/provider"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
)

var (
	migrationAttemptedMu sync.Mutex
	migrationAttempted   = map[[2]string]bool{}
)

// Seams the migration reaches the plugin manager and the terminal through, swapped by tests
var migrationSteps struct {
	providerPresent func(ctx context.Context, name, h string) bool
	catalogSource   func(name string) (string, bool)
	catalogEntry    func(name string) (*plugins.PluginCatalogEntry, error)
	installInto     func(ctx context.Context, h string) MemoryInstallFunc
	interactive     func() bool
}

// Install function bound to one home, the error stands for a raised install exception
type MemoryInstallFunc func(name string) (map[string]any, error)

// Effective memory.provider of a home, empty when unset, an error when the memory block is not a mapping
func ConfiguredMemoryProvider(ctx context.Context, h string) (string, error) {
	sel, err := pm.ReadHomeSelection(ctx, h)
	if err != nil {
		return "", err
	}
	raw := sel["memory"]
	if !anyutil.IsTrue(raw) {
		return "", nil
	}
	mem, ok := raw.(map[string]any)
	if !ok {
		return "", &kindError{class: "AttributeError", msg: fmt.Sprintf("config memory section is %s where a mapping was expected", errchain.Kind(raw))}
	}
	if !anyutil.IsTrue(mem["provider"]) {
		return "", nil
	}
	return strings.TrimSpace(anyutil.String(mem["provider"])), nil
}

// True when the provider resolves for the home through bundled or its user plugins
func MemoryProviderPresent(ctx context.Context, name, h string) bool {
	return FindProvider(home.WithOverride(ctx, h), nil, name) != nil
}

// Live catalog entry name shipping the provider, false when the catalog has no such plugin
func CatalogSource(name string) (string, bool) {
	entry := plugins.GetLiveCatalogEntry(context.Background(), name)
	if entry == nil {
		return "", false
	}
	return entry.Name, true
}

// Install command for a catalog plugin of the category, empty when the in tree catalog lacks it or the lookup fails
func CatalogInstallHint(ctx context.Context, name, category string) string {
	if name == "" {
		return ""
	}
	entry, err := migrationSteps.catalogEntry(name)
	if err != nil || entry == nil || (category != "" && entry.Category != category) {
		return ""
	}
	return provider.PluginInstallCommand(name, home.Dir(ctx))
}

// Provider the home still needs from the catalog, empty when nothing is due or a catalog miss was reported
func pendingMemoryProvider(ctx context.Context, h string, say func(string)) (string, error) {
	name, err := ConfiguredMemoryProvider(ctx, h)
	if err != nil {
		return "", err
	}
	if IsCoreMemoryProvider(name) || migrationSteps.providerPresent(ctx, name, h) {
		return "", nil
	}
	if _, ok := migrationSteps.catalogSource(name); !ok {
		say(fmt.Sprintf("  \u26a0 Memory provider '%s' is configured but not installed and not in the plugin catalog. "+
			"Install it with `%s` or change memory.provider.", name, provider.PluginInstallCommand("<source>", h)))
		return "", nil
	}
	return name, nil
}

// Runs one install and reports its outcome, interrupted when the context ended during the install
func installPendingMemory(ctx context.Context, h, name string, install MemoryInstallFunc, say func(string)) (ok, interrupted bool) {
	result, err := install(name)
	if ctx.Err() != nil {
		return false, true
	}
	if err != nil {
		result = map[string]any{"ok": false, "error": err.Error()}
	}
	if anyutil.IsTrue(result["ok"]) {
		say(fmt.Sprintf("  \u2713 Memory provider '%s' was configured but not installed \u2014 installed its plugin from the catalog "+
			"(memory.provider and your stored memories are unchanged; check its settings with "+
			"`icebath memory status`).", name))
		return true, false
	}
	reason := "unknown error"
	if anyutil.IsTrue(result["error"]) {
		reason = anyutil.String(result["error"])
	}
	reason = strings.TrimRight(reason, ". ")
	say(fmt.Sprintf("  \u26a0 Memory provider '%s' is configured but not installed, and its plugin could not be installed from the catalog: "+
		"%s. Run `%s`.", name, reason, provider.PluginInstallCommand(name, h)))
	return false, false
}

func migrationInteractive() bool {
	return isatty.IsTerminal(os.Stdin.Fd()) && isatty.IsTerminal(os.Stdout.Fd())
}

// Install through the plugin manager into the home, dependency questions left to the user
func memoryInstallInto(ctx context.Context, h string) MemoryInstallFunc {
	return func(name string) (map[string]any, error) {
		return provider.PluginHooks().Install(home.WithOverride(ctx, h), name, false)
	}
}

// Installs the configured catalog provider into every profile home missing it, grouped by provider, the enumeration error unhandled
func MigrateAllMemoryHomes(ctx context.Context, say func(string)) ([]string, error) {
	labelled := func(h string) func(string) {
		return func(message string) {
			say(fmt.Sprintf("  [%s] %s", provider.PluginHomeLabel(h), strings.TrimLeftFunc(message, unicode.IsSpace)))
		}
	}
	type group struct {
		name  string
		homes []string
	}
	var groups []*group
	index := map[string]*group{}
	homes, err := provider.PluginHooks().DependencyHomes()
	if err != nil {
		return nil, err
	}
	for _, h := range homes {
		name, err := pendingMemoryProvider(ctx, h, labelled(h))
		if err != nil {
			pkgLog().Debug(fmt.Sprintf("memory provider migration skipped for %s: %s", h, err.Error()))
			continue
		}
		if name == "" {
			continue
		}
		g := index[name]
		if g == nil {
			g = &group{name: name}
			index[name] = g
			groups = append(groups, g)
		}
		g.homes = append(g.homes, h)
	}
	var installed []string
	cancelled := false
	for _, g := range groups {
		if len(g.homes) > 1 && migrationSteps.interactive() {
			labels := make([]string, len(g.homes))
			for i, h := range g.homes {
				labels[i] = provider.PluginHomeLabel(h)
			}
			say(fmt.Sprintf("  Memory provider '%s' is configured in %d profiles (%s); your answers to its dependency "+
				"questions apply to all of them.", g.name, len(g.homes), strings.Join(labels, ", ")))
		}
		gctx := plugins.WithSharedDependencyAnswers(ctx)
		for i, h := range g.homes {
			ok, interrupted := installPendingMemory(gctx, h, g.name, migrationSteps.installInto(gctx, h), labelled(h))
			if interrupted {
				cancelled = true
				break
			}
			if ok {
				installed = append(installed, g.name)
				continue
			}
			if rest := g.homes[i+1:]; len(rest) > 0 {
				labels := make([]string, len(rest))
				commands := make([]string, len(rest))
				for j, r := range rest {
					labels[j] = provider.PluginHomeLabel(r)
					commands[j] = "`" + provider.PluginInstallCommand(g.name, r) + "`"
				}
				say(fmt.Sprintf("  \u26a0 Memory provider '%s' was not installed for %s either. Run %s.",
					g.name, strings.Join(labels, ", "), strings.Join(commands, ", ")))
			}
			break
		}
		if cancelled {
			say("  \u26a0 Memory provider migration cancelled. Profiles already migrated keep their plugin; " +
				"run `icebath plugins install <name>` (with `-p <profile>`) for the rest.")
			break
		}
	}
	return installed, nil
}

// Tells the user how to install a configured provider that resolved nowhere, once per process per home and name
func ReportMissingMemoryProvider(ctx context.Context, name string, say func(string)) error {
	h := home.Dir(ctx)
	key := [2]string{home.Key(h), name}
	migrationAttemptedMu.Lock()
	seen := migrationAttempted[key]
	migrationAttempted[key] = true
	migrationAttemptedMu.Unlock()
	if seen {
		return nil
	}
	report := func(message string) {
		pkgLog().Warn(message)
		if say == nil {
			return
		}
		defer func() {
			if r := recover(); r != nil {
				pkgLog().Debug("Memory migration notification failed", "panic", r)
			}
		}()
		say(message)
	}
	pending, err := pendingMemoryProvider(ctx, h, report)
	if err != nil {
		return err
	}
	if pending == name {
		report(fmt.Sprintf("\u26a0 Memory provider '%s' is not installed, so external memory is off for this session. "+
			"It never shipped with Icebath, so Icebath installs it only when you ask: run `%s`.", name, provider.PluginInstallCommand(name, h)))
	}
	return nil
}

// Binds the migration seams to the plugin manager and the terminal
func registerMemoryProviderMigration() {
	migrationSteps.providerPresent = MemoryProviderPresent
	migrationSteps.catalogSource = CatalogSource
	migrationSteps.catalogEntry = plugins.GetPluginCatalogEntry
	migrationSteps.installInto = memoryInstallInto
	migrationSteps.interactive = migrationInteractive
}
