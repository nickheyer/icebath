package commands

import (
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Annotation naming the plugin that added a top level command, read by the tree export
const PluginCommandAnnotation = "icebath_plugin"

// Run class of a command path and its subcommands, with the plain reason and the flags that make it read only
type cliRunRule struct {
	class      tuigateway.CLIRunClass
	reason     string
	insideWith []string
}

const (
	reasonReplacesProgram = "It replaces the program this window runs on."
	reasonServiceGone     = "It stops or replaces the background service this window uses."
	reasonStoreOffline    = "It needs the session store closed, so the background service stops first."
	reasonPackages        = "It changes packages this window runs on."
	reasonPlumbing        = "Called by other icebath processes, not by people."
	reasonNestedUI        = "Opens another icebath inside this one."
	reasonProfileMoves    = "It can move or remove the profile this window runs on."
)

// Command paths that cannot run inside a live terminal UI, the longest matching path deciding a command's class
var cliRunRules = map[string]cliRunRule{
	"update":                    {tuigateway.CLIRunAfterExit, reasonReplacesProgram, []string{"check", "plan", "install-id", "list-venv-holders"}},
	"uninstall":                 {tuigateway.CLIRunAfterExit, "It removes the program this window runs on.", []string{"dry-run"}},
	"import":                    {tuigateway.CLIRunAfterExit, "It restores a backup over the data this window uses.", nil},
	"gateway stop":              {tuigateway.CLIRunAfterExit, reasonServiceGone, nil},
	"gateway install":           {tuigateway.CLIRunAfterExit, reasonServiceGone, nil},
	"gateway uninstall":         {tuigateway.CLIRunAfterExit, reasonServiceGone, nil},
	"gateway migrate":           {tuigateway.CLIRunAfterExit, reasonServiceGone, nil},
	"gateway run":               {tuigateway.CLIRunInternal, "icebath starts the background service itself.", nil},
	"gateway ensure":            {tuigateway.CLIRunInternal, reasonPlumbing, nil},
	"sessions set-journal-mode": {tuigateway.CLIRunAfterExit, reasonStoreOffline, nil},
	"sessions recover":          {tuigateway.CLIRunAfterExit, reasonStoreOffline, nil},
	"sessions repair":           {tuigateway.CLIRunAfterExit, reasonStoreOffline, nil},
	"sessions optimize-storage": {tuigateway.CLIRunAfterExit, reasonStoreOffline, nil},
	"pm install":                {tuigateway.CLIRunAfterExit, reasonPackages, nil},
	"pm repair":                 {tuigateway.CLIRunAfterExit, reasonPackages, nil},
	"pm update":                 {tuigateway.CLIRunAfterExit, reasonPackages, []string{"check"}},
	"pm lock":                   {tuigateway.CLIRunAfterExit, reasonPackages, nil},
	"pm gc":                     {tuigateway.CLIRunAfterExit, reasonPackages, nil},
	"pm environment":            {tuigateway.CLIRunAfterExit, reasonPackages, nil},
	"pm worker":                 {tuigateway.CLIRunInternal, reasonPlumbing, nil},
	"profile use":               {tuigateway.CLIRunAfterExit, "This window stays on its profile until icebath opens again.", nil},
	"profile rename":            {tuigateway.CLIRunAfterExit, reasonProfileMoves, nil},
	"profile delete":            {tuigateway.CLIRunAfterExit, reasonProfileMoves, nil},
	"cron tick":                 {tuigateway.CLIRunInternal, reasonPlumbing, nil},
	"cron classify":             {tuigateway.CLIRunInternal, "A helper scheduled job scripts call.", nil},
	"kanban claim":              {tuigateway.CLIRunInternal, reasonPlumbing, nil},
	"kanban heartbeat":          {tuigateway.CLIRunInternal, reasonPlumbing, nil},
	"kanban notify-subscribe":   {tuigateway.CLIRunInternal, reasonPlumbing, nil},
	"kanban notify-unsubscribe": {tuigateway.CLIRunInternal, reasonPlumbing, nil},
	"kanban daemon":             {tuigateway.CLIRunInternal, "Replaced by the background service, which runs the board itself.", nil},
	"login":                     {tuigateway.CLIRunInternal, "Replaced by icebath auth add.", nil},
	"chat":                      {tuigateway.CLIRunInternal, reasonNestedUI, nil},
	"sessions browse":           {tuigateway.CLIRunInternal, reasonNestedUI, nil},
	"acp":                       {tuigateway.CLIRunInternal, "Started by a code editor, not by people.", nil},
}

// Run class of a command path, a hidden command without a rule counting as plumbing
func cliRunClass(path string, hidden bool) cliRunRule {
	words := strings.Fields(path)
	for n := len(words); n > 0; n-- {
		if rule, ok := cliRunRules[strings.Join(words[:n], " ")]; ok {
			return rule
		}
	}
	if hidden {
		return cliRunRule{class: tuigateway.CLIRunInternal, reason: reasonPlumbing}
	}
	return cliRunRule{class: tuigateway.CLIRunInside}
}

// Tree is every command path of the CLI in tree order with its help, arguments, options and run class
func (c *CLI) Tree() tuigateway.CommandsTreeResult {
	out := tuigateway.CommandsTreeResult{Program: c.Root.Name(), Commands: []tuigateway.CLICommand{}}
	var walk func(cmd *cobra.Command, plugin string, unlisted bool)
	walk = func(cmd *cobra.Command, plugin string, unlisted bool) {
		path := c.pathOf(cmd)
		if strings.HasPrefix(cmd.Name(), "__") || (path != "" && cliTierHidden(path)) {
			return
		}
		if owner := cmd.Annotations[PluginCommandAnnotation]; owner != "" {
			plugin = owner
		}
		if cmd != c.Root && plugin == "" && !listedInParentHelp(cmd) {
			unlisted = true
		}
		out.Commands = append(out.Commands, c.treeCommand(cmd, path, plugin, unlisted))
		for _, sub := range cmd.Commands() {
			walk(sub, plugin, unlisted)
		}
	}
	walk(c.Root, "", false)
	return out
}

// Whether the parent's --help lists the command among its subcommands
func listedInParentHelp(cmd *cobra.Command) bool {
	parent := cmd.Parent()
	if parent == nil {
		return true
	}
	if cmd.Hidden {
		return false
	}
	items, _ := apItems(parent)
	for _, it := range items {
		if it.Kind != 's' {
			continue
		}
		for _, sub := range it.Sub {
			if sub.Name == cmd.Name() {
				return sub.Listed
			}
		}
		return slices.Contains(it.Choices, cmd.Name())
	}
	return cmd.Short != ""
}

// Exported entry of one command, its usage falling back to cobra's line when the argparse shape cannot render
func (c *CLI) treeCommand(cmd *cobra.Command, path, plugin string, unlisted bool) (entry tuigateway.CLICommand) {
	hidden := cmd.Hidden || unlisted
	rule := cliRunClass(path, cmd.Hidden)
	entry = tuigateway.CLICommand{Path: path, Aliases: slices.Clone(cmd.Aliases), Short: cmd.Short, Hidden: hidden,
		Group: hasVisibleSubcommands(cmd), RawArgs: cmd.DisableFlagParsing, Class: rule.class, ClassReason: rule.reason,
		InsideWith: slices.Clone(rule.insideWith), Plugin: plugin}
	if cmd.Long != cmd.Short {
		entry.Long = cmd.Long
	}
	defer func() {
		if r := recover(); r != nil {
			entry.Usage = cmd.UseLine()
			pkgLog().Debug(fmt.Sprintf("command tree export of %q fell back to cobra usage: %v", path, r))
		}
	}()
	items, table := apItems(cmd)
	entry.Usage = strings.Join(strings.Fields(strings.TrimPrefix(strings.TrimSpace(argparseUsage(cmd)), "usage:")), " ")
	entry.Args = c.treeArgs(cmd, items)
	entry.Flags = treeFlags(cmd, items, table, cmd == c.Root)
	return entry
}

// Whether a command has a subcommand a person may type
func hasVisibleSubcommands(cmd *cobra.Command) bool {
	for _, sub := range cmd.Commands() {
		if !strings.HasPrefix(sub.Name(), "__") {
			return true
		}
	}
	return false
}

// Positional arguments of a command, the cobra specs carrying the parse shape and the argparse table the help words
func (c *CLI) treeArgs(cmd *cobra.Command, items []apItem) []tuigateway.CLIArg {
	var table []apItem
	for _, it := range items {
		if it.Kind == 'p' {
			table = append(table, it)
		}
	}
	specs := c.positionals[cmd]
	if len(specs) == 0 {
		var out []tuigateway.CLIArg
		for _, it := range table {
			arg := tuigateway.CLIArg{Name: it.Dest, Type: "string", Choices: slices.Clone(it.Choices), Help: it.Help,
				Optional: it.Nargs == "?" || it.Nargs == "*", Variadic: it.Nargs == "*" || it.Nargs == "+", Remainder: it.Nargs == "..."}
			if len(it.Metavar) > 0 {
				arg.Metavar = strings.Join(it.Metavar, " ")
			}
			out = append(out, arg)
		}
		return out
	}
	out := make([]tuigateway.CLIArg, 0, len(specs))
	for i, s := range specs {
		arg := tuigateway.CLIArg{Name: s.Name, Metavar: s.Metavar, Optional: s.Optional, Variadic: s.Variadic, Remainder: s.Remain,
			Type: "string", Choices: slices.Clone(s.Choices), Default: s.Default, Help: s.Help}
		if s.Int {
			arg.Type = "int"
		}
		if row, ok := tableArg(table, s.Name, i); ok {
			if arg.Help == "" {
				arg.Help = row.Help
			}
			if arg.Metavar == "" && len(row.Metavar) > 0 {
				arg.Metavar = strings.Join(row.Metavar, " ")
			}
			if len(arg.Choices) == 0 {
				arg.Choices = slices.Clone(row.Choices)
			}
		}
		out = append(out, arg)
	}
	return out
}

// Argparse positional row of a cobra spec, matched by name and else by position
func tableArg(table []apItem, name string, index int) (apItem, bool) {
	for _, it := range table {
		if it.Dest == name {
			return it, true
		}
	}
	if index < len(table) {
		return table[index], true
	}
	return apItem{}, false
}

// Options of a command in declaration order, the root's persistent flags marked global
func treeFlags(cmd *cobra.Command, items []apItem, table *apParser, root bool) []tuigateway.CLIFlag {
	declared := table != nil && len(table.Acts) > 0
	local := cmd.LocalFlags()
	all := cmd.Flags()
	sorted := all.SortFlags
	all.SortFlags = false
	defer func() { all.SortFlags = sorted }()
	var out []tuigateway.CLIFlag
	all.VisitAll(func(fl *pflag.Flag) {
		if fl.Name == "help" || local.Lookup(fl.Name) == nil {
			return
		}
		flag := treeFlag(fl)
		row, inTable := tableFlag(items, fl)
		if inTable {
			if len(row.Metavar) > 0 {
				flag.Metavar = strings.Join(row.Metavar, " ")
			}
			if len(flag.Choices) == 0 {
				flag.Choices = slices.Clone(row.Choices)
			}
			flag.Required = flag.Required || row.Required
			flag.Hidden = flag.Hidden || row.Suppress
		} else if declared && !cmd.DisableFlagParsing {
			flag.Hidden = true
		}
		flag.Global = root && cmd.PersistentFlags().Lookup(fl.Name) != nil
		out = append(out, flag)
	})
	return out
}

// Exported shape of one pflag, its value type, default and parse annotations
func treeFlag(fl *pflag.Flag) tuigateway.CLIFlag {
	typ := fl.Value.Type()
	flag := tuigateway.CLIFlag{Name: fl.Name, Shorthand: fl.Shorthand, Aliases: slices.Clone(fl.Annotations["aliases"]), Type: typ,
		Choices: slices.Clone(fl.Annotations["choices"]), Required: len(fl.Annotations[cobra.BashCompOneRequiredFlag]) > 0,
		Hidden: fl.Hidden, Help: fl.Usage, Repeatable: strings.HasSuffix(typ, "Array") || strings.HasSuffix(typ, "Slice") || typ == "count"}
	switch {
	case fl.Annotations["default_none"] != nil, fl.DefValue == "", fl.DefValue == "[]":
	case typ == "bool" && fl.DefValue == "false", typ == "count" && fl.DefValue == "0":
	default:
		flag.Default = fl.DefValue
	}
	if fl.NoOptDefVal != "" && typ != "bool" && typ != "count" {
		flag.ValueOptional = true
	}
	if groups := fl.Annotations["cobra_annotation_mutually_exclusive"]; len(groups) > 0 {
		flag.Exclusive = strings.Fields(groups[0])
	}
	return flag
}

// Argparse option row naming the flag by its long or short spelling
func tableFlag(items []apItem, fl *pflag.Flag) (apItem, bool) {
	for _, it := range items {
		if it.Kind != 'f' {
			continue
		}
		for _, opt := range it.Opts {
			if opt == "--"+fl.Name || (fl.Shorthand != "" && opt == "-"+fl.Shorthand) {
				return it, true
			}
		}
	}
	return apItem{}, false
}

// Exported tree of the last build and the key it was built for
var treeCache struct {
	sync.Mutex
	key  string
	tree *tuigateway.CommandsTreeResult
}

// CachedTree is the tree of the CLI build makes, built again only when key changes; NewCLI writes no package state, so builds elsewhere never race it
func CachedTree(key string, build func() *CLI) tuigateway.CommandsTreeResult {
	treeCache.Lock()
	defer treeCache.Unlock()
	if treeCache.tree != nil && treeCache.key == key {
		return *treeCache.tree
	}
	tree := build().Tree()
	treeCache.key, treeCache.tree = key, &tree
	return tree
}
