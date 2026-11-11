package tuiserver

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/skills"
	"github.com/nickheyer/icebath/pkg/tools"
)

// One builtin registry command as the palette catalog reads it
type CatalogCommand struct {
	Name, Category, Description string
	Aliases                     []string
	GatewayOnly                 bool
	DesktopMeta                 map[string]any
}

// Builtin registry rows in registry order and the subcommand table of pkg/commands, assigned in cmd/icebath/wiring.go
var CatalogRegistry func(ctx context.Context) ([]CatalogCommand, map[string][]string)

var catalogHidden = map[string]bool{"sethome": true, "set-home": true, "commands": true, "approve": true, "deny": true}

var catalogExtras = [][3]string{
	{"/density", "Toggle compact display mode", "TUI"},
	{"/logs", "Show recent gateway log lines", "TUI"},
	{"/mouse", "Set mouse tracking preset [on|off|toggle|wheel|buttons|all]", "TUI"},
	{"/sessions", "Switch between live TUI sessions", "TUI"},
}

// Accumulated pairs, canonical names, desktop meta, ordered categories and plugin commands of the catalog
type commandCatalogAcc struct {
	pairs          []any
	canon          *pyjson.Object
	commands       *pyjson.Object
	catMap         *pyjson.Object
	pluginCommands *pyjson.Object
}

// Adds one listed command to the pairs, its category and the canonical map
func (cat *commandCatalogAcc) add(key, desc, category string) {
	cat.canon.Set(strings.ToLower(key), key)
	cat.pairs = append(cat.pairs, []any{key, desc})
	cat.appendCategory(category, []any{key, desc})
}

// Category rows, created empty at first sight in insertion order
func (cat *commandCatalogAcc) category(name string) []any {
	rows, ok := cat.catMap.Get(name)
	if !ok {
		cat.catMap.Set(name, []any{})
		return []any{}
	}
	return rows.([]any)
}

// Appends one row to a category
func (cat *commandCatalogAcc) appendCategory(name string, row []any) {
	cat.catMap.Set(name, append(cat.category(name), row))
}

// Registry commands with their aliases canonicalized, then the TUI extras the registry does not claim
func catalogRegistry(cat *commandCatalogAcc, rows []CatalogCommand) {
	for _, cmd := range rows {
		for _, key := range append([]string{cmd.Name}, cmd.Aliases...) {
			meta := pyjson.NewObject()
			for _, k := range sortedMetaKeys(cmd.DesktopMeta) {
				meta.Set(k, cmd.DesktopMeta[k])
			}
			cat.commands.Set("/"+key, meta)
		}
		if catalogHidden[cmd.Name] || cmd.GatewayOnly {
			continue
		}
		cat.add("/"+cmd.Name, cmd.Description, cmd.Category)
		for _, alias := range cmd.Aliases {
			cat.canon.Set(strings.ToLower("/"+alias), "/"+cmd.Name)
		}
	}
	for _, extra := range catalogExtras {
		if _, claimed := cat.canon.Get(strings.ToLower(extra[0])); !claimed {
			cat.add(extra[0], extra[1], extra[2])
		}
	}
}

// Desktop meta keys in the order the registry builds them
func sortedMetaKeys(meta map[string]any) []string {
	order := []string{"argument_mode", "desktop", "desktop_subcommands"}
	var keys []string
	for _, k := range order {
		if _, ok := meta[k]; ok {
			keys = append(keys, k)
		}
	}
	var rest []string
	for k := range meta {
		if k != "argument_mode" && k != "desktop" && k != "desktop_subcommands" {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	return append(keys, rest...)
}

// Quick commands of the config under User commands, the category present even when every entry is malformed
func catalogQuickCommands(ctx context.Context, cat *commandCatalogAcc) error {
	tree, err := config.LoadReadonly(ctx, home.Dir(ctx), config.EnvRefLookupContext(ctx))
	if err != nil {
		return err
	}
	qcmds, isDict := tree["quick_commands"].(map[string]any)
	if !isDict || len(qcmds) == 0 {
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

// Plugin slash commands the registry does not claim, every plugin command kept with its description
func catalogPluginCommands(ctx context.Context, cat *commandCatalogAcc) {
	pluginCmds := plugins.GetPluginCommands(ctx)
	if len(pluginCmds) > 0 {
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
		cat.pluginCommands.Set(pname, desc)
		if _, claimed := cat.canon.Get(strings.ToLower(key)); claimed {
			continue
		}
		cat.add(key, desc, "Plugin commands")
		var mode any
		switch info.ArgumentMode {
		case "options", "text", "mixed":
			mode = info.ArgumentMode
		default:
			if strings.TrimSpace(info.ArgsHint) != "" {
				mode = "text"
			}
		}
		cat.commands.Set(key, pyjson.ObjectOf("argument_mode", mode, "desktop", nil))
	}
}

// Skill pairs with their usage and origin, returning the built in name collision notice of skills
func catalogSkills(ctx context.Context, cat *commandCatalogAcc, skillsOut *pyjson.Object) (string, error) {
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
		cat.pairs = append(cat.pairs, []any{k, pyrepr.Str(desc)})
		name := strings.TrimLeft(k, "/")
		if raw := info["name"]; pyrepr.Truthy(raw) {
			name = pyrepr.Str(raw)
		}
		skillsOut.Set(k, pyjson.ObjectOf("usage", usage(name), "origin", originOf(name)))
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
func CommandCatalog(ctx context.Context, scope func(context.Context) (context.Context, error)) (map[string]any, error) {
	cat := &commandCatalogAcc{canon: pyjson.NewObject(), commands: pyjson.NewObject(), catMap: pyjson.NewObject(),
		pluginCommands: pyjson.NewObject(), pairs: []any{}}
	rows, subcommands := CatalogRegistry(ctx)
	catalogRegistry(cat, rows)
	warning, pluginError := "", ""
	skillsOut := pyjson.NewObject()
	scoped := ctx
	if scope != nil {
		var err error
		if scoped, err = scope(ctx); err != nil {
			return nil, err
		}
	}
	if err := catalogQuickCommands(scoped, cat); err != nil {
		discoveryLogger().Debug("quick_commands discovery failed", "error", err)
		warning = fmt.Sprintf("quick_commands discovery unavailable: %s", err)
	}
	if failure := catchPanic(func() { catalogPluginCommands(scoped, cat) }); failure != nil {
		err := fmt.Errorf("%v", failure)
		discoveryLogger().Debug("plugin command discovery failed", "error", err)
		pluginError = err.Error()
		if warning == "" {
			warning = fmt.Sprintf("plugin command discovery unavailable: %s", err)
		}
	}
	if note, err := catalogSkills(scoped, cat, skillsOut); err != nil {
		discoveryLogger().Debug("skill discovery failed", "error", err)
		warning = fmt.Sprintf("skill discovery unavailable: %s", err)
	} else if warning == "" {
		warning = note
	}
	sub := pyjson.NewObject()
	for _, row := range rows {
		if values, ok := subcommands["/"+row.Name]; ok {
			sub.Set("/"+row.Name, stringItems(values))
		}
	}
	var rest []string
	for key := range subcommands {
		if _, done := sub.Get(key); !done {
			rest = append(rest, key)
		}
	}
	sort.Strings(rest)
	for _, key := range rest {
		sub.Set(key, stringItems(subcommands[key]))
	}
	categories := make([]any, 0, cat.catMap.Len())
	for _, name := range cat.catMap.Keys {
		pairs, _ := cat.catMap.Get(name)
		categories = append(categories, pyjson.ObjectOf("name", name, "pairs", pairs))
	}
	return map[string]any{"pairs": cat.pairs, "sub": sub, "canon": cat.canon, "commands": cat.commands,
		"categories": categories, "skills": skillsOut, "skill_count": skillsOut.Len(), "warning": warning,
		"plugin_discovery_error": pluginError, "plugin_commands": cat.pluginCommands}, nil
}

// Strings as a JSON list
func stringItems(values []string) []any {
	out := make([]any, len(values))
	for i, v := range values {
		out[i] = v
	}
	return out
}

// Slash popup items for the text, skills and bundles read inside the scope of the calling session
func SlashCompletions(ctx context.Context, text string, scope func(context.Context) (context.Context, error)) (map[string]any, error) {
	return slashCompletions(ctx, text, scope)
}
