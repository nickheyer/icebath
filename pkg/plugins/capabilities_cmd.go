package plugins

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/home"
)

// Discovered plugin row the plugin verbs resolve names against, the name version description source dir key tuple
type discoveredPlugin = PluginEntry

// Writes plugins.entries.<plugin>.<key> as a boolean into config.yaml
func setPluginEntryFlag(h, pluginID, key string, value bool) error {
	cfg, err := readUserConfig(h)
	if err != nil {
		return err
	}
	childMap(childMap(childMap(cfg, "plugins"), "entries"), pluginID)[key] = value
	return saveUserConfig(h, cfg)
}

// Normalized capabilities a manifest declares
func declaredCapabilitiesFromManifest(manifest map[string]any, name string) []string {
	if name == "" {
		name = "?"
	}
	return ParseDeclaredCapabilities(manifest["capabilities"], name)
}

// Declared capabilities of an installed or bundled plugin by key, empty when it is unknown
func declaredCapabilitiesForKey(ctx context.Context, key string) ([]string, error) {
	entry, err := findPluginEntry(ctx, key)
	if err != nil || entry == nil {
		return nil, err
	}
	if entry.Source == "entrypoint" {
		for _, m := range discoverEntrypointManifests(ctx) {
			if key == m.Key || key == m.Name {
				return append([]string{}, m.Capabilities...), nil
			}
		}
		return nil, nil
	}
	if entry.Dir == "" {
		return nil, nil
	}
	manifest, err := readManifest(entry.Dir)
	if err != nil {
		return nil, err
	}
	return declaredCapabilitiesFromManifest(manifest, entry.Name), nil
}

// Shows the capability consent screen and records the decision, granted true only on interactive consent
func capabilityConsent(ctx context.Context, console *Console, pluginID string, declared []string, consentContext string) (bool, error) {
	h := home.Dir(ctx)
	cfg, err := readUserConfig(h)
	if err != nil {
		return false, err
	}
	pending := PendingCapabilities(cfg, pluginID, declared)
	if len(pending) == 0 {
		if len(declared) > 0 {
			if err := RecordConsent(h, pluginID, nil, declared); err != nil {
				return false, err
			}
		}
		return true, nil
	}
	verb := "now requests"
	if consentContext == "install" {
		verb = "requests"
	}
	console.Print(fmt.Sprintf("\n  [yellow]Plugin [bold]%s[/bold] %s the following capabilities:[/yellow]", pluginID, verb))
	for _, c := range pending {
		console.Print(fmt.Sprintf("    [bold]%s[/bold] — %s", c, CapabilityRegistry[c].Description))
	}
	console.Print("  [dim]Granting trusts the plugin author with these host surfaces. " +
		"This is consent, not a sandbox — plugins run as regular Python in-process.[/dim]")
	if !isTTY() {
		console.Print("  [yellow]Non-interactive session: capabilities NOT granted (fail closed).[/yellow] Run " +
			fmt.Sprintf("`icebath plugins capabilities %s` to review and `icebath plugins enable %s` to grant interactively.", pluginID, pluginID))
		return false, nil
	}
	if askYes("  Grant these capabilities? [y/N] ", nil) {
		if err := RecordConsent(h, pluginID, pending, declared); err != nil {
			return false, err
		}
		console.Print(fmt.Sprintf("  [green]✓[/green] Granted: %s ([dim]plugins.entries.%s.granted_capabilities[/dim])", strings.Join(pending, ", "), pluginID))
		return true, nil
	}
	console.Print(fmt.Sprintf("  [dim]Declined. %s stays enabled with these capabilities off; it should degrade gracefully "+
		"(ctx.has_capability()). Re-run `icebath plugins enable %s` to grant later.[/dim]", pluginID, pluginID))
	return false, nil
}

// Consent screen for install and update, whose callers act on a config failure and not on the decision
func runCapabilityConsent(ctx context.Context, console *Console, name string, declared []string, consentContext string) error {
	_, err := capabilityConsent(ctx, console, name, declared, consentContext)
	return err
}

// Prints declared against granted capabilities for one plugin or every plugin holding any
func CmdCapabilities(ctx context.Context, name *string, out io.Writer) error {
	console := NewConsole(out)
	h := home.Dir(ctx)
	if name != nil {
		hit, err := findPluginEntry(ctx, *name)
		if err != nil {
			return err
		}
		if hit == nil {
			return fail(console, unknownPluginMessage(*name, false))
		}
		resolved := hit.Key
		if resolved == "" {
			resolved = hit.Name
		}
		name = &resolved
	}
	cfg, err := readUserConfig(h)
	if err != nil {
		return err
	}
	type row struct {
		key, source        string
		declared           []string
		granted, effective map[string]bool
	}
	var rows []row
	entries, err := discoverAllPlugins(ctx)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		key := entry.Key
		if key == "" {
			key = entry.Name
		}
		if name != nil && *name != key && *name != entry.Name {
			continue
		}
		declared, err := declaredCapabilitiesForKey(ctx, key)
		if err != nil {
			return err
		}
		effective := map[string]bool{}
		for c := range CapabilityRegistry {
			if CapabilityGranted(cfg, key, c) {
				effective[c] = true
			}
		}
		if len(declared) == 0 && len(effective) == 0 && name == nil {
			continue
		}
		rows = append(rows, row{key, entry.Source, declared, GrantedCapabilities(cfg, key), effective})
	}
	if name != nil && len(rows) == 0 {
		return fail(console, unknownPluginMessage(*name, false))
	}
	if len(rows) == 0 {
		console.Print("[dim]No plugins declare or hold capabilities.[/dim]")
		return nil
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].key != rows[j].key {
			return rows[i].key < rows[j].key
		}
		return rows[i].source < rows[j].source
	})
	for _, r := range rows {
		console.Print(fmt.Sprintf("[bold]%s[/bold] [dim](%s)[/dim]", r.key, r.source))
		if len(r.declared) == 0 {
			console.Print("  declared: [dim](none)[/dim]")
		}
		declaredSet := map[string]bool{}
		for _, c := range r.declared {
			declaredSet[c] = true
			mark := "[green]granted[/green] [dim](via legacy allow_* key — deprecated)[/dim]"
			if !r.effective[c] {
				mark = "[yellow]not granted[/yellow]"
			} else if r.granted[c] {
				mark = "[green]granted[/green]"
			}
			console.Print(fmt.Sprintf("  %s: %s", c, mark))
		}
		var extra []string
		for c := range r.effective {
			if !declaredSet[c] {
				extra = append(extra, c)
			}
		}
		sort.Strings(extra)
		for _, c := range extra {
			console.Print(fmt.Sprintf("  %s: [green]granted[/green] [dim](not declared in manifest)[/dim]", c))
		}
	}
	return nil
}

// Asks for and persists the allow_tool_override grant, a nil choice prompting on the terminal
func resolveToolOverrideGrant(ctx context.Context, console *Console, key string, allow *bool) error {
	granted := false
	if allow == nil {
		granted = askYes("[yellow]Allow this plugin to replace built-in tools (e.g. shell_exec, write_file)?[/yellow]\n"+
			"  This is a privileged capability: an override can intercept everything the agent routes through that tool.\n"+
			"  Grant it? [y/N] ", nil)
	} else {
		granted = *allow
	}
	if err := setPluginEntryFlag(home.Dir(ctx), key, "allow_tool_override", granted); err != nil {
		return err
	}
	if granted {
		console.Print(fmt.Sprintf("[green]✓[/green] Granted [bold]%s[/bold] permission to override built-in tools ([dim]plugins.entries.%s.allow_tool_override: true[/dim]).", key, key))
	} else {
		console.Print(fmt.Sprintf("[dim]%s may not override built-in tools. Re-run `icebath plugins enable %s --allow-tool-override` to grant this later.[/dim]", key, key))
	}
	return nil
}
