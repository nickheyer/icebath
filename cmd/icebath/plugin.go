package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/harness"
	"github.com/nickheyer/icebath/pkg/plugin"
	"github.com/nickheyer/icebath/pkg/profiles"
)

// Usage of the plugin subcommand
const pluginUsage = `usage: icebath plugin <command> [arguments]

  marketplace add <source> [-name name]   register a marketplace, owner/repo, owner/repo@ref, a git url, an http url or a path
  marketplace remove <name>               forget a marketplace
  marketplace list [-json]                registered marketplaces
  marketplace update [name]               fetch a marketplace again, every one when none is named
  install <plugin[@marketplace]> [-scope user|project] [-config key=value]...   install a plugin and turn it on
  uninstall <plugin[@marketplace]> [-keep-data]                                 remove an installed plugin
  enable <plugin[@marketplace]> [-scope user|project]                           turn a plugin on
  disable <plugin[@marketplace]> [-scope user|project]                          turn a plugin off
  update <plugin[@marketplace]>           fetch an installed plugin again
  list [-json] [-available]               installed plugins, or every plugin the marketplaces offer
  configure <plugin@marketplace> [-values-stdin] [key=value]...                 set the options of a plugin
  validate <path>                         check a plugin directory, a plugin.json or a marketplace.json
  prune [-dry-run]                        uninstall plugins installed only as dependencies that nothing needs`

// Carries out icebath plugin <command>, returning the exit code
func runPlugin(args []string, cwd string) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprintln(os.Stderr, pluginUsage)
		return 0
	}
	configDir, err := profiles.ConfigDir()
	if err != nil {
		return fail(err)
	}
	store := plugin.NewStore(configDir)
	store.Progress = func(text string) { fmt.Fprintln(os.Stderr, text) }
	ctx := context.Background()
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "marketplace", "market", "marketplaces":
		return runMarketplace(ctx, store, rest)
	case "install", "i", "add":
		return pluginInstall(ctx, store, rest, cwd)
	case "uninstall", "remove", "rm":
		return pluginUninstall(store, rest, cwd)
	case "enable", "disable":
		return pluginEnable(store, rest, cwd, cmd == "enable")
	case "update":
		return pluginUpdate(ctx, store, rest)
	case "list", "ls":
		return pluginList(store, rest, cwd)
	case "configure", "config":
		return pluginConfigure(store, rest, cwd)
	case "validate":
		return pluginValidate(rest)
	case "prune", "autoremove":
		return pluginPrune(store, rest)
	}
	fmt.Fprintf(os.Stderr, "icebath plugin: unknown command %q\n%s\n", cmd, pluginUsage)
	return 2
}

func fail(err error) int {
	fmt.Fprintln(os.Stderr, "icebath plugin:", err)
	return 1
}

// Splits flags of the form -name value or -flag out of the arguments, leaving the positional ones
func splitFlags(args []string, valued ...string) ([]string, map[string]string) {
	flags := map[string]string{}
	var out []string
	takes := map[string]bool{}
	for _, v := range valued {
		takes[v] = true
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") || a == "-" {
			out = append(out, a)
			continue
		}
		name := strings.TrimLeft(a, "-")
		if k, v, ok := strings.Cut(name, "="); ok {
			if takes[k] && k == "config" {
				flags[k] = strings.TrimSpace(flags[k] + "\n" + v)
				continue
			}
			flags[k] = v
			continue
		}
		if takes[name] && i+1 < len(args) {
			if name == "config" {
				flags[name] = strings.TrimSpace(flags[name] + "\n" + args[i+1])
			} else {
				flags[name] = args[i+1]
			}
			i++
			continue
		}
		flags[name] = "true"
	}
	return out, flags
}

func runMarketplace(ctx context.Context, store *plugin.Store, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, pluginUsage)
		return 2
	}
	pos, flags := splitFlags(args[1:], "name")
	switch args[0] {
	case "add":
		if len(pos) == 0 {
			return fail(errors.New("marketplace add needs a source"))
		}
		src, err := plugin.ParseMarketSource(pos[0])
		if err != nil {
			return fail(err)
		}
		m, err := store.AddMarketplace(ctx, src, flags["name"])
		if err != nil {
			return fail(err)
		}
		if flags["json"] != "" {
			return printJSON(map[string]any{"marketplaceName": m.Name, "installedRoot": m.Root, "plugins": m.Names()})
		}
		fmt.Printf("added marketplace %s with %d plugins\n", m.Name, len(m.Plugins))
		for _, p := range m.Problems {
			fmt.Fprintln(os.Stderr, "  "+p)
		}
		return 0
	case "remove", "rm":
		if len(pos) == 0 {
			return fail(errors.New("marketplace remove needs a name"))
		}
		if err := store.RemoveMarketplace(pos[0]); err != nil {
			return fail(err)
		}
		fmt.Println("removed marketplace", pos[0])
		return 0
	case "list", "ls":
		known, err := store.Known()
		if err != nil {
			return fail(err)
		}
		names := make([]string, 0, len(known))
		for name := range known {
			names = append(names, name)
		}
		sort.Strings(names)
		if flags["json"] != "" {
			return printJSON(known)
		}
		if len(names) == 0 {
			fmt.Println("no marketplace is registered")
			return 0
		}
		for _, name := range names {
			k := known[name]
			fmt.Printf("%-24s %-40s %s\n", name, k.Source.String(), k.LastUpdated.Local().Format("2006-01-02 15:04"))
		}
		return 0
	case "update", "upgrade":
		names := pos
		if len(names) == 0 {
			known, err := store.Known()
			if err != nil {
				return fail(err)
			}
			for name := range known {
				names = append(names, name)
			}
			sort.Strings(names)
		}
		code := 0
		for _, name := range names {
			m, err := store.UpdateMarketplace(ctx, name)
			if err != nil {
				fmt.Fprintln(os.Stderr, "icebath plugin:", name+":", err)
				code = 1
				continue
			}
			fmt.Printf("updated marketplace %s, %d plugins\n", m.Name, len(m.Plugins))
		}
		return code
	}
	fmt.Fprintf(os.Stderr, "icebath plugin: marketplace takes add, remove, list or update\n")
	return 2
}

// Enablement saved for a plugin in the user or project configuration file
func saveEnabled(cwd, id, scope string, on bool) error {
	return saveConfigField(cwd, scope, []string{"plugins", "enabled", id}, on, false)
}

func pluginInstall(ctx context.Context, store *plugin.Store, args []string, cwd string) int {
	pos, flags := splitFlags(args, "scope", "config", "marketplace")
	if len(pos) == 0 {
		return fail(errors.New("install needs a plugin name or name@marketplace"))
	}
	scope := flags["scope"]
	if scope == "" {
		scope = plugin.ScopeUser
	}
	if src := flags["marketplace"]; src != "" {
		ms, err := plugin.ParseMarketSource(src)
		if err != nil {
			return fail(err)
		}
		if _, err := store.AddMarketplace(ctx, ms, ""); err != nil && !strings.Contains(err.Error(), "already registered") {
			return fail(err)
		}
	}
	id, in, err := store.Install(ctx, pos[0], scope)
	if err != nil {
		return fail(err)
	}
	store.EnsureData(id)
	if err := saveEnabled(cwd, id, scope, true); err != nil {
		return fail(err)
	}
	values := map[string]string{}
	for _, line := range strings.Split(flags["config"], "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			values[k] = v
		}
	}
	if len(values) > 0 {
		if err := saveOptions(cwd, id, values); err != nil {
			return fail(err)
		}
	}
	p, err := plugin.Load(in.InstallPath, plugin.LoadOptions{ID: id, Entry: in.Entry, Marketplace: in.Marketplace, Data: store.DataDir(id), Options: values, Entry_: store.EntryOf(in)})
	if err != nil {
		return fail(err)
	}
	if flags["json"] != "" {
		return printJSON(map[string]any{"pluginId": id, "name": p.Name, "marketplaceName": in.Marketplace, "version": in.Version, "installedPath": in.InstallPath, "scope": scope, "missing": p.Missing})
	}
	fmt.Printf("installed %s %s into %s\n", id, in.Version, in.InstallPath)
	for _, prob := range p.Problems {
		fmt.Fprintln(os.Stderr, "  "+prob)
	}
	if len(p.Missing) > 0 {
		fmt.Fprintf(os.Stderr, "  %s needs values for %s, set them with icebath plugin configure %s key=value\n", id, strings.Join(p.Missing, ", "), id)
	}
	return 0
}

// Id of an installed plugin from a name that may leave the marketplace out
func resolveInstalled(store *plugin.Store, name string) (string, error) {
	installed, err := store.Installed()
	if err != nil {
		return "", err
	}
	if _, ok := installed[name]; ok {
		return name, nil
	}
	var found []string
	for id, in := range installed {
		if in.Entry == name {
			found = append(found, id)
		}
	}
	sort.Strings(found)
	switch len(found) {
	case 0:
		return "", fmt.Errorf("plugin %s is not installed", name)
	case 1:
		return found[0], nil
	}
	return "", fmt.Errorf("%s is installed from several marketplaces, name one of %s", name, strings.Join(found, ", "))
}

func pluginUninstall(store *plugin.Store, args []string, cwd string) int {
	pos, flags := splitFlags(args, "scope")
	if len(pos) == 0 {
		return fail(errors.New("uninstall needs a plugin"))
	}
	id, err := resolveInstalled(store, pos[0])
	if err != nil {
		return fail(err)
	}
	if err := store.Uninstall(id, flags["keep-data"] != ""); err != nil {
		return fail(err)
	}
	for _, scope := range []string{plugin.ScopeUser, plugin.ScopeProject} {
		saveConfigField(cwd, scope, []string{"plugins", "enabled", id}, nil, true)
	}
	if flags["json"] != "" {
		return printJSON(map[string]any{"pluginId": id, "uninstalled": true})
	}
	fmt.Println("uninstalled", id)
	return 0
}

func pluginEnable(store *plugin.Store, args []string, cwd string, on bool) int {
	pos, flags := splitFlags(args, "scope")
	if len(pos) == 0 && flags["all"] == "" {
		return fail(errors.New("name a plugin"))
	}
	scope := flags["scope"]
	if scope == "" {
		scope = plugin.ScopeUser
	}
	var ids []string
	if flags["all"] != "" {
		installed, err := store.Installed()
		if err != nil {
			return fail(err)
		}
		for id := range installed {
			ids = append(ids, id)
		}
		sort.Strings(ids)
	} else {
		id, err := resolveInstalled(store, pos[0])
		if err != nil {
			if !strings.Contains(pos[0], "@") {
				return fail(err)
			}
			id = pos[0]
		}
		ids = []string{id}
	}
	for _, id := range ids {
		if err := saveEnabled(cwd, id, scope, on); err != nil {
			return fail(err)
		}
		if on {
			fmt.Println("enabled", id)
		} else {
			fmt.Println("disabled", id)
		}
	}
	return 0
}

func pluginUpdate(ctx context.Context, store *plugin.Store, args []string) int {
	pos, flags := splitFlags(args, "scope")
	if len(pos) == 0 {
		return fail(errors.New("update needs a plugin"))
	}
	id, err := resolveInstalled(store, pos[0])
	if err != nil {
		return fail(err)
	}
	in, changed, err := store.Update(ctx, id)
	if err != nil {
		return fail(err)
	}
	if flags["json"] != "" {
		return printJSON(map[string]any{"pluginId": id, "version": in.Version, "updated": changed})
	}
	if changed {
		fmt.Printf("updated %s to %s\n", id, in.Version)
	} else {
		fmt.Printf("%s is up to date at %s\n", id, in.Version)
	}
	return 0
}

func pluginList(store *plugin.Store, args []string, cwd string) int {
	_, flags := splitFlags(args)
	if flags["available"] != "" {
		markets, errs, err := store.Marketplaces()
		if err != nil {
			return fail(err)
		}
		installed, _ := store.Installed()
		type offer struct {
			ID          string `json:"id"`
			Marketplace string `json:"marketplace"`
			Description string `json:"description,omitempty"`
			Version     string `json:"version,omitempty"`
			Installed   bool   `json:"installed"`
		}
		var offers []offer
		for _, m := range markets {
			for _, e := range m.Plugins {
				id := e.Name + "@" + m.Name
				_, have := installed[id]
				offers = append(offers, offer{ID: id, Marketplace: m.Name, Description: e.Description, Version: e.Version, Installed: have})
			}
		}
		for name, err := range errs {
			fmt.Fprintf(os.Stderr, "marketplace %s: %v\n", name, err)
		}
		if flags["json"] != "" {
			return printJSON(map[string]any{"available": offers})
		}
		if len(offers) == 0 {
			fmt.Println("no marketplace offers anything, add one with icebath plugin marketplace add")
			return 0
		}
		for _, o := range offers {
			mark := " "
			if o.Installed {
				mark = "*"
			}
			fmt.Printf("%s %-40s %-10s %s\n", mark, o.ID, o.Version, o.Description)
		}
		return 0
	}
	installed, err := store.Installed()
	if err != nil {
		return fail(err)
	}
	enabled := enabledFromConfig(cwd)
	ids := make([]string, 0, len(installed))
	for id := range installed {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	if flags["json"] != "" {
		type row struct {
			ID          string `json:"id"`
			Version     string `json:"version"`
			Scope       string `json:"scope"`
			InstallPath string `json:"installPath"`
			Enabled     bool   `json:"enabled"`
		}
		var rows []row
		for _, id := range ids {
			in := installed[id]
			on, set := enabled[id]
			rows = append(rows, row{ID: id, Version: in.Version, Scope: in.Scope, InstallPath: in.InstallPath, Enabled: !set || on})
		}
		return printJSON(map[string]any{"installed": rows})
	}
	if len(ids) == 0 {
		fmt.Println("no plugin is installed")
		return 0
	}
	for _, id := range ids {
		in := installed[id]
		state := "enabled"
		if on, set := enabled[id]; set && !on {
			state = "disabled"
		}
		fmt.Printf("%-40s %-14s %-9s %s\n", id, in.Version, state, in.InstallPath)
	}
	return 0
}

func pluginConfigure(store *plugin.Store, args []string, cwd string) int {
	pos, flags := splitFlags(args)
	if len(pos) == 0 {
		return fail(errors.New("configure needs a plugin@marketplace"))
	}
	id, err := resolveInstalled(store, pos[0])
	if err != nil {
		return fail(err)
	}
	values := map[string]string{}
	if flags["values-stdin"] != "" {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return fail(err)
		}
		if err := json.Unmarshal(data, &values); err != nil {
			return fail(fmt.Errorf("stdin must hold a JSON object of strings: %w", err))
		}
	}
	for _, kv := range pos[1:] {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return fail(fmt.Errorf("%q is not key=value", kv))
		}
		values[k] = v
	}
	if len(values) == 0 {
		return fail(errors.New("give key=value pairs or -values-stdin"))
	}
	installed, _ := store.Installed()
	secrets := map[string]string{}
	if in, ok := installed[id]; ok {
		m, err := plugin.ReadManifest(in.InstallPath)
		if err != nil {
			return fail(err)
		}
		for k, v := range values {
			opt, ok := m.UserConfig[k]
			if !ok {
				return fail(fmt.Errorf("plugin %s has no option %s", id, k))
			}
			if opt.Sensitive {
				secrets[k] = v
				delete(values, k)
			}
		}
	}
	if len(secrets) > 0 {
		if err := store.SaveSecrets(id, secrets); err != nil {
			return fail(err)
		}
	}
	if len(values) > 0 {
		if err := saveOptions(cwd, id, values); err != nil {
			return fail(err)
		}
	}
	for k, v := range secrets {
		values[k] = "(secret, " + fmt.Sprint(len(v)) + " characters)"
	}
	if flags["json"] != "" {
		return printJSON(map[string]any{"pluginId": id, "configured": values})
	}
	fmt.Println("configured", id)
	return 0
}

func pluginValidate(args []string) int {
	pos, flags := splitFlags(args)
	if len(pos) == 0 {
		return fail(errors.New("validate needs a path"))
	}
	path := pos[0]
	var problems, warnings []string
	st, err := os.Stat(path)
	if err != nil {
		return fail(err)
	}
	switch {
	case !st.IsDir() && strings.HasSuffix(path, "marketplace.json"):
		m, err := plugin.ReadMarketplace(path)
		if err != nil {
			problems = append(problems, err.Error())
			break
		}
		warnings = append(warnings, m.Problems...)
		if m.Description == "" {
			warnings = append(warnings, "the marketplace has no description")
		}
	case !st.IsDir():
		data, err := os.ReadFile(path)
		if err != nil {
			return fail(err)
		}
		m, err := plugin.ParseManifest(data, path)
		if err != nil {
			problems = append(problems, err.Error())
			break
		}
		warnings = append(warnings, m.Warnings...)
	default:
		if catalog, err := findCatalogIn(path); err == nil {
			m, err := plugin.ReadMarketplace(catalog)
			if err != nil {
				problems = append(problems, err.Error())
			} else {
				warnings = append(warnings, m.Problems...)
			}
		}
		p, err := plugin.Load(path, plugin.LoadOptions{})
		if err != nil {
			problems = append(problems, err.Error())
			break
		}
		warnings = append(warnings, p.Problems...)
		for _, s := range p.Skills {
			for _, prob := range s.Problems {
				warnings = append(warnings, "skill "+s.Name+": "+prob)
			}
		}
		for _, a := range p.Agents {
			for _, prob := range a.Problems {
				warnings = append(warnings, "agent "+a.Name+": "+prob)
			}
		}
		if _, err := os.Stat(path + "/CLAUDE.md"); err == nil {
			warnings = append(warnings, "CLAUDE.md at the plugin root is not loaded")
		}
	}
	if flags["json"] != "" {
		printJSON(map[string]any{"path": path, "errors": problems, "warnings": warnings, "valid": len(problems) == 0})
	} else {
		for _, p := range problems {
			fmt.Println("error:", p)
		}
		for _, w := range warnings {
			fmt.Println("warning:", w)
		}
		if len(problems) == 0 {
			fmt.Println("valid")
		}
	}
	if len(problems) > 0 || flags["strict"] != "" && len(warnings) > 0 {
		return 1
	}
	return 0
}

// Catalog file under a directory, from the marketplace file names
func findCatalogIn(dir string) (string, error) {
	for _, f := range plugin.MarketplaceFiles {
		p := dir + "/" + f
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", os.ErrNotExist
}

func pluginPrune(store *plugin.Store, args []string) int {
	_, flags := splitFlags(args)
	ids, err := store.Prunable()
	if err != nil {
		return fail(err)
	}
	if len(ids) == 0 {
		fmt.Println("nothing to prune")
		return 0
	}
	for _, id := range ids {
		if flags["dry-run"] != "" {
			fmt.Println("would uninstall", id)
			continue
		}
		if err := store.Uninstall(id, false); err != nil {
			return fail(err)
		}
		fmt.Println("uninstalled", id)
	}
	return 0
}

func printJSON(v any) int {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return fail(err)
	}
	return 0
}

// Writes one field into the configuration file of the scope, the user file or the project file under cwd
func saveConfigField(cwd, scope string, path []string, value any, unset bool) error {
	layers, err := harness.ConfigLayers(cwd)
	if err != nil {
		return err
	}
	file := layers[0]
	switch scope {
	case plugin.ScopeProject, plugin.ScopeLocal:
		if len(layers) < 2 {
			return errors.New("no project directory to save into")
		}
		file = layers[1]
	case "", plugin.ScopeUser:
	default:
		return fmt.Errorf("scope %q is not user, project or local", scope)
	}
	return harness.SaveTo(file, harness.Field{Path: path, Value: value, Unset: unset})
}

// Writes option values of a plugin into the user configuration file
func saveOptions(cwd, id string, values map[string]string) error {
	layers, err := harness.ConfigLayers(cwd)
	if err != nil {
		return err
	}
	fields := make([]harness.Field, 0, len(values))
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fields = append(fields, harness.Field{Path: []string{"plugins", "config", id, k}, Value: values[k]})
	}
	return harness.SaveTo(layers[0], fields...)
}

// Enablement by plugin id as the configuration files under cwd hold it
func enabledFromConfig(cwd string) map[string]bool {
	cfg, err := harness.Discover(cwd)
	if err != nil {
		return map[string]bool{}
	}
	if cfg.Plugins.Enabled == nil {
		return map[string]bool{}
	}
	return cfg.Plugins.Enabled
}
