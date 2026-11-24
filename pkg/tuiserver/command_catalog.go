package tuiserver

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/settings"
	"github.com/nickheyer/icebath/pkg/skills"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// One builtin registry command as the palette catalog reads it
type CatalogCommand struct {
	Name, Category, Description string
	Aliases                     []string
	GatewayOnly                 bool
}

// Builtin registry rows in registry order and the subcommand table of pkg/commands, assigned in cmd/icebath/wiring.go
var CatalogRegistry func(ctx context.Context) ([]CatalogCommand, map[string][]string)

// Plugin owned registry commands sorted by name with their subcommand table, assigned in cmd/icebath/wiring.go
var RegistryPluginCommands func(ctx context.Context) ([]CatalogCommand, map[string][]string)

var catalogHidden = map[string]bool{"sethome": true, "set-home": true, "commands": true, "approve": true, "deny": true}

var catalogExtras = [][3]string{
	{"/density", "Toggle compact display mode", "TUI"},
	{"/logs", "Show recent gateway log lines", "TUI"},
}

// Accumulated pairs, canonical names, ordered categories and plugin commands of the catalog
type commandCatalogAcc struct {
	pairs          [][2]string
	canon          map[string]string
	categories     []tuigateway.CommandCategory
	pluginCommands map[string]string
	chatCommands   map[string]string
}

// Adds one listed command to the pairs, its category and the canonical map
func (cat *commandCatalogAcc) add(key, desc, category string) {
	cat.canon[strings.ToLower(key)] = key
	cat.pairs = append(cat.pairs, [2]string{key, desc})
	group := cat.category(category)
	group.Pairs = append(group.Pairs, [2]string{key, desc})
}

// Category of a name, created empty at first sight in insertion order
func (cat *commandCatalogAcc) category(name string) *tuigateway.CommandCategory {
	for i := range cat.categories {
		if cat.categories[i].Name == name {
			return &cat.categories[i]
		}
	}
	cat.categories = append(cat.categories, tuigateway.CommandCategory{Name: name, Pairs: [][2]string{}})
	return &cat.categories[len(cat.categories)-1]
}

// Whether a lower cased command key already has a canonical name
func (cat *commandCatalogAcc) claimed(key string) bool {
	_, found := cat.canon[strings.ToLower(key)]
	return found
}

// Registry commands with their aliases canonicalized, then the TUI extras the registry does not claim
func catalogRegistry(cat *commandCatalogAcc, rows []CatalogCommand) {
	for _, cmd := range rows {
		if cmd.GatewayOnly {
			for _, name := range append([]string{cmd.Name}, cmd.Aliases...) {
				cat.chatCommands[strings.ToLower(name)] = cmd.Description
			}
		}
		if catalogHidden[cmd.Name] || cmd.GatewayOnly {
			continue
		}
		cat.add("/"+cmd.Name, cmd.Description, cmd.Category)
		for _, alias := range cmd.Aliases {
			cat.canon[strings.ToLower("/"+alias)] = "/" + cmd.Name
		}
	}
	for _, extra := range catalogExtras {
		if !cat.claimed(extra[0]) {
			cat.add(extra[0], extra[1], extra[2])
		}
	}
}

// Quick commands of the config under User commands, the category present even when every entry is malformed
func catalogQuickCommands(ctx context.Context, cat *commandCatalogAcc) error {
	qcmds := settings.QuickCommands.Get(ctx)
	if len(qcmds) == 0 {
		return nil
	}
	cat.category("User commands")
	names := make([]string, 0, len(qcmds))
	for name := range qcmds {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, qname := range names {
		qc, isDict := qcmds[qname].(map[string]any)
		if !isDict {
			continue
		}
		qtype, hasType := qc["type"]
		if !hasType {
			qtype = ""
		}
		command, hasCommand := qc["command"]
		if !hasCommand {
			command = ""
		}
		target, hasTarget := qc["target"]
		if !hasTarget {
			target = ""
		}
		defaults := map[string]string{"exec": "exec: " + pyrepr.Str(command), "alias": "alias → " + pyrepr.Str(target)}
		var desc string
		if described := qc["description"]; pyrepr.Truthy(described) {
			desc = pyrepr.Str(described)
		} else if text, ok := qtype.(string); ok && defaults[text] != "" {
			desc = defaults[text]
		} else if pyrepr.Truthy(qtype) {
			desc = pyrepr.Str(qtype)
		} else {
			desc = "quick command"
		}
		cat.add("/"+qname, desc, "User commands")
	}
	return nil
}

// Plugin slash commands the registry does not claim then the plugin owned registry commands, returning their subcommands
func catalogPluginCommands(ctx context.Context, cat *commandCatalogAcc) map[string][]string {
	pluginCmds := plugins.GetPluginCommands(ctx)
	registered, registeredSubs := RegistryPluginCommands(ctx)
	if len(pluginCmds) > 0 || len(registered) > 0 {
		cat.category("Plugin commands")
	}
	names := make([]string, 0, len(pluginCmds))
	for name := range pluginCmds {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, pname := range names {
		info := pluginCmds[pname]
		if info == nil {
			continue
		}
		key := "/" + pname
		desc := info.Description
		if desc == "" {
			desc = "Plugin command"
		}
		cat.pluginCommands[pname] = desc
		if cat.claimed(key) {
			continue
		}
		cat.add(key, desc, "Plugin commands")
	}
	for _, cmd := range registered {
		key := "/" + cmd.Name
		if cat.claimed(key) {
			continue
		}
		cat.add(key, cmd.Description, "Plugin commands")
		for _, alias := range cmd.Aliases {
			cat.canon[strings.ToLower("/"+alias)] = key
		}
	}
	return registeredSubs
}

// Skill pairs with their usage and origin, returning the built in name collision notice of skills
func catalogSkills(ctx context.Context, cat *commandCatalogAcc, skillsOut map[string]tuigateway.SkillCatalogEntry) (string, error) {
	usage, originOf := skillUsageLookup(ctx)
	scope := skills.NewScope(home.Dir(ctx), nil)
	cwd, err := tools.ResolveAgentCwd(tools.ToolEnvFrom(ctx))
	if err != nil {
		return "", err
	}
	scope.Cwd = cwd
	commands, err := scope.GetInteractiveSkillCommands(ctx)
	if err != nil {
		return "", err
	}
	keys := make([]string, 0, len(commands))
	for key := range commands {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, k := range keys {
		info := commands[k]
		desc, hasDesc := info["description"]
		if !hasDesc {
			desc = "Skill"
		}
		cat.pairs = append(cat.pairs, [2]string{k, pyrepr.Str(desc)})
		name := strings.TrimLeft(k, "/")
		if raw := info["name"]; pyrepr.Truthy(raw) {
			name = pyrepr.Str(raw)
		}
		skillsOut[k] = tuigateway.SkillCatalogEntry{Usage: usage(name).Int64(), Origin: originOf(name)}
	}
	var names []string
	for _, s := range scope.FindAllSkills(false) {
		names = append(names, pyrepr.Str(s["name"]))
	}
	sort.Strings(names)
	var notes []string
	for _, name := range names {
		if note, ok := skills.SkillCommandCollisionNote(name); ok && note != "" {
			notes = append(notes, note)
		}
	}
	return strings.Join(notes, "; "), nil
}

// Registry backed slash metadata by category, each discovery source degrading into the warning on failure
func CommandCatalog(ctx context.Context, scope func(context.Context) (context.Context, error)) (tuigateway.CommandsCatalogResult, error) {
	cat := &commandCatalogAcc{canon: map[string]string{}, pluginCommands: map[string]string{}, chatCommands: map[string]string{}, pairs: [][2]string{}}
	rows, subcommands := CatalogRegistry(ctx)
	catalogRegistry(cat, rows)
	warning, pluginError := "", ""
	skillsOut := map[string]tuigateway.SkillCatalogEntry{}
	scoped := ctx
	if scope != nil {
		var err error
		if scoped, err = scope(ctx); err != nil {
			return tuigateway.CommandsCatalogResult{}, err
		}
	}
	if err := catalogQuickCommands(scoped, cat); err != nil {
		pkgLog().Debug("quick_commands discovery failed", "error", err)
		warning = fmt.Sprintf("quick_commands discovery unavailable: %s", err)
	}
	var pluginSubcommands map[string][]string
	if failure := catchPanic(func() { pluginSubcommands = catalogPluginCommands(scoped, cat) }); failure != nil {
		err := fmt.Errorf("%v", failure)
		pkgLog().Debug("plugin command discovery failed", "error", err)
		pluginError = err.Error()
		if warning == "" {
			warning = fmt.Sprintf("plugin command discovery unavailable: %s", err)
		}
	}
	if note, err := catalogSkills(scoped, cat, skillsOut); err != nil {
		pkgLog().Debug("skill discovery failed", "error", err)
		warning = fmt.Sprintf("skill discovery unavailable: %s", err)
	} else if warning == "" {
		warning = note
	}
	sub := map[string][]string{}
	for key, values := range subcommands {
		sub[key] = values
	}
	for key, values := range pluginSubcommands {
		if _, done := sub[key]; !done {
			sub[key] = values
		}
	}
	if cat.categories == nil {
		cat.categories = []tuigateway.CommandCategory{}
	}
	return tuigateway.CommandsCatalogResult{Pairs: cat.pairs, Sub: sub, Canon: cat.canon, Categories: cat.categories, Skills: skillsOut,
		SkillCount: int64(len(skillsOut)), Warning: warning, PluginDiscoveryError: pluginError, PluginCommands: cat.pluginCommands,
		ChatCommands: cat.chatCommands}, nil
}

// Slash popup rows for the text, skills and bundles read inside the scope of the calling session, for both routes
func SlashCompletions(ctx context.Context, text string, scope func(context.Context) (context.Context, error)) (tuigateway.CompleteSlashResult, error) {
	raw, err := slashCompletions(ctx, text, scope)
	if err != nil {
		return tuigateway.CompleteSlashResult{}, err
	}
	return typedRow[tuigateway.CompleteSlashResult]("complete.slash", raw), nil
}
