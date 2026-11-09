package memory

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"

	"github.com/mattn/go-isatty"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Memory providers that shipped bundled in core, the only ones unattended consent covers
var leftCoreMemoryProviders = map[string]bool{
	"hindsight": true, "honcho": true, "mem0": true, "supermemory": true, "openviking": true, "retaindb": true,
	"byterover": true, "holographic": true,
}

var (
	migrationAttemptedMu sync.Mutex
	migrationAttempted   = map[[2]string]bool{}
)

// Seams the migration reaches the plugin manager and the terminal through, swapped by tests
var migrationSteps struct {
	providerPresent func(ctx context.Context, name, h string) bool
	catalogSource   func(name string) (string, bool)
	catalogEntry    func(name string) (*plugins.PluginCatalogEntry, error)
	installInto     func(ctx context.Context, h string, consent *bool) MemoryInstallFunc
	interactive     func() bool
	lazyInstalls    func(ctx context.Context) bool
}

// Install function bound to one home, the error stands for a raised install exception
type MemoryInstallFunc func(name string) (map[string]any, error)

// Effective memory.provider of a home, empty when unset, an error when the memory block is not a mapping
func ConfiguredMemoryProvider(h string) (string, error) {
	sel, err := pm.ReadHomeSelection(h)
	if err != nil {
		return "", err
	}
	raw := sel["memory"]
	if !pyTruthy(raw) {
		return "", nil
	}
	mem, ok := raw.(map[string]any)
	if !ok {
		return "", &pyError{class: "AttributeError", msg: fmt.Sprintf("'%s' object has no attribute 'get'", pyTypeName(raw))}
	}
	if !pyTruthy(mem["provider"]) {
		return "", nil
	}
	return pyStrip(pyStr(mem["provider"])), nil
}

// True when the provider resolves for the home through bundled, its user plugins or an entry point with a directory
func MemoryProviderPresent(ctx context.Context, name, h string) bool {
	r := FindProvider(home.WithOverride(ctx, h), nil, name)
	return r != nil && hasProviderDir(r)
}

// Live catalog entry name shipping the provider, false when the catalog has no such plugin
func CatalogSource(name string) (string, bool) {
	return provider.PluginHooks().CatalogEntry(name)
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
	name, err := ConfiguredMemoryProvider(h)
	if err != nil {
		return "", err
	}
	if IsCoreMemoryProvider(name) || migrationSteps.providerPresent(ctx, name, h) {
		return "", nil
	}
	if _, ok := migrationSteps.catalogSource(name); !ok {
		say(fmt.Sprintf("  \u26a0 Memory provider '%s' is configured but not installed and not in the plugin catalog. "+
			"Install it with `icebath plugins install <source>` or change memory.provider.", name))
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
	if pyTruthy(result["ok"]) {
		say(fmt.Sprintf("  \u2713 Memory provider '%s' moved out of core \u2014 installed its plugin from the catalog "+
			"(memory.provider and your stored memories are unchanged; check its settings with "+
			"`icebath memory status`).", name))
		return true, false
	}
	reason := "unknown error"
	if pyTruthy(result["error"]) {
		reason = pyStr(result["error"])
	}
	reason = pystr.RstripChars(reason, ". ")
	say(fmt.Sprintf("  \u26a0 Memory provider '%s' moved out of core and could not be installed automatically: "+
		"%s. Run `%s`.", name, reason, provider.PluginInstallCommand(name, h)))
	return false, false
}

// Installs the configured provider's catalog plugin into a home where it is gone, the installed name or empty
func MigrateMemoryHome(ctx context.Context, h string, install MemoryInstallFunc, say func(string)) (string, error) {
	name, err := pendingMemoryProvider(ctx, h, say)
	if err != nil || name == "" {
		return "", err
	}
	if ok, _ := installPendingMemory(ctx, h, name, install, say); ok {
		return name, nil
	}
	return "", nil
}

func migrationInteractive() bool {
	return isatty.IsTerminal(os.Stdin.Fd()) && isatty.IsTerminal(os.Stdout.Fd())
}

// Consent of a terminal less run, the lazy install policy of the home in ctx
func migrationUnattendedConsent(ctx context.Context) bool {
	return !migrationSteps.interactive() && migrationSteps.lazyInstalls(ctx)
}

// Install through the plugin manager into the home, consent nil meaning left core providers on unattended consent
func memoryInstallInto(ctx context.Context, h string, consent *bool) MemoryInstallFunc {
	return func(name string) (map[string]any, error) {
		bound := home.WithOverride(ctx, h)
		yes := leftCoreMemoryProviders[name] && migrationUnattendedConsent(bound)
		if consent != nil {
			yes = *consent
		}
		return provider.PluginHooks().Install(bound, name, yes)
	}
}

// Migrates every profile home sharing the environment grouped by provider and consent, the enumeration error unhandled like Python
func MigrateAllMemoryHomes(ctx context.Context, say func(string)) ([]string, error) {
	labelled := func(h string) func(string) {
		return func(message string) {
			say(fmt.Sprintf("  [%s] %s", provider.PluginHomeLabel(h), pystr.Lstrip(message)))
		}
	}
	type group struct {
		name  string
		homes []string
	}
	var groups []*group
	index := map[[2]string]*group{}
	homes, err := provider.PluginHooks().DependencyHomes()
	if err != nil {
		return nil, err
	}
	for _, h := range homes {
		name, err := pendingMemoryProvider(ctx, h, labelled(h))
		if err != nil {
			slog.Default().Debug(fmt.Sprintf("memory provider migration skipped for %s: %s", h, err.Error()))
			continue
		}
		if name == "" {
			continue
		}
		key := [2]string{name, fmt.Sprint(migrationUnattendedConsent(home.WithOverride(ctx, h)))}
		g := index[key]
		if g == nil {
			g = &group{name: name}
			index[key] = g
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
			ok, interrupted := installPendingMemory(gctx, h, g.name, migrationSteps.installInto(gctx, h, nil), labelled(h))
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

// Agent init recovery of a configured provider that resolved nowhere, once per process per home and name
func RecoverMemoryProviderAtStartup(ctx context.Context, name string, say func(string)) (bool, error) {
	h := home.Dir(ctx)
	key := [2]string{home.Key(h), name}
	migrationAttemptedMu.Lock()
	seen := migrationAttempted[key]
	migrationAttempted[key] = true
	migrationAttemptedMu.Unlock()
	if seen {
		return false, nil
	}
	report := func(message string) {
		slog.Default().Warn(message)
		if say == nil {
			return
		}
		defer func() {
			if r := recover(); r != nil {
				slog.Default().Debug("Memory migration notification failed", "panic", r)
			}
		}()
		say(message)
	}
	command := provider.PluginInstallCommand(name, h)
	if !migrationSteps.lazyInstalls(ctx) {
		report(fmt.Sprintf("\u26a0 Memory provider '%s' is not installed, so external memory is off for this session. "+
			"security.allow_lazy_installs is off, so Icebath did not fetch it: run `%s`.", name, command))
		return false, nil
	}
	if !leftCoreMemoryProviders[name] {
		pending, err := pendingMemoryProvider(ctx, h, report)
		if err != nil {
			return false, err
		}
		if pending == name {
			report(fmt.Sprintf("\u26a0 Memory provider '%s' is not installed, so external memory is off for this session. "+
				"It never shipped with Icebath, so Icebath installs it only when you ask: run `%s`.", name, command))
		}
		return false, nil
	}
	if provider.LeftCoreFailedRecently(h, name) {
		report(fmt.Sprintf("\u26a0 Memory provider '%s' is not installed, so external memory is off for this session. "+
			"Its automatic install failed recently: run `%s`.", name, command))
		return false, nil
	}
	yes := true
	got, err := MigrateMemoryHome(ctx, h, migrationSteps.installInto(ctx, h, &yes), report)
	if err != nil {
		return false, err
	}
	if got == name {
		provider.NoteLeftCoreSuccess(h, name)
	} else {
		provider.NoteLeftCoreFailure(h, name)
	}
	return got == name, nil
}

// Binds the migration seams to the plugin manager, the terminal and the lazy install policy
func registerMemoryProviderMigration() {
	migrationSteps.providerPresent = MemoryProviderPresent
	migrationSteps.catalogSource = CatalogSource
	migrationSteps.catalogEntry = plugins.GetPluginCatalogEntry
	migrationSteps.installInto = memoryInstallInto
	migrationSteps.interactive = migrationInteractive
	migrationSteps.lazyInstalls = pm.LazyInstallsAllowedFor
}
