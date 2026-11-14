package plugins

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrich"
)

// Filters of icebath plugins list
type ListOptions struct {
	NoBundled, User, Enabled, Plain, JSON bool
}

var statusMarkup = map[string]string{"disabled": "[red]disabled[/red]", "enabled": "[green]enabled[/green]"}

// Rows left after the bundled and enabled only list filters
func filterPluginEntries(h string, entries []PluginEntry, opts ListOptions, enabled, disabled map[string]bool) ([]PluginEntry, error) {
	filtered := entries
	if opts.NoBundled || opts.User {
		var kept []PluginEntry
		for _, e := range filtered {
			if e.Source != "bundled" {
				kept = append(kept, e)
			}
		}
		filtered = kept
	}
	if opts.Enabled {
		active := categoryActiveNames(h)
		var kept []PluginEntry
		for _, e := range filtered {
			status, err := entryStatus(h, e, enabled, disabled, active)
			if err != nil {
				return nil, err
			}
			if status == "enabled" {
				kept = append(kept, e)
			}
		}
		filtered = kept
	}
	return filtered, nil
}

// Activation state of one discovered row
func entryStatus(h string, e PluginEntry, enabled, disabled, active map[string]bool) (string, error) {
	return pluginStatus(h, e, enabled, disabled, active, e.Dir != "")
}

// Lists bundled and user plugins with their activation state, provenance and catalog removals
func CmdList(ctx context.Context, opts ListOptions, out io.Writer) error {
	console := NewConsole(out)
	h := home.Dir(ctx)
	entries, err := discoverAllPlugins(ctx)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		console.Print("[dim]No plugins installed.[/dim]")
		console.Print("[dim]Install with:[/dim] icebath plugins install owner/repo")
		return nil
	}
	enabled, disabled := getEnabledSet(h), getDisabledSet(h)
	if entries, err = filterPluginEntries(h, entries, opts, enabled, disabled); err != nil {
		return err
	}
	pins, err := readInstallMetadata(h)
	if err != nil {
		return err
	}
	removedEntries := ResolvedRemovedEntries(ctx)
	active := categoryActiveNames(h)
	type listRow struct {
		name, status, version, description, source string
		removed                                    any
	}
	rows := make([]listRow, 0, len(entries))
	for _, e := range entries {
		status, err := pluginStatus(h, e, enabled, disabled, active, e.Dir != "")
		if err != nil {
			return err
		}
		source, err := catalogAnnotation(h, e.Dir)
		if err != nil {
			return err
		}
		if source == "" {
			source = pinAnnotation(h, e.Name, pins)
		}
		if source == "" {
			source = e.Source
		}
		var removed any
		reason, ok, err := removedAnnotation(h, e.Name, e.Dir, removedEntries)
		if err != nil {
			return err
		}
		if ok {
			removed = reason
		}
		rows = append(rows, listRow{e.Name, status, e.Version, e.Description, source, removed})
	}
	if opts.JSON {
		list := make([]any, 0, len(rows))
		for _, r := range rows {
			o := pyjson.NewObject()
			o.Set("name", r.name)
			o.Set("status", r.status)
			o.Set("version", r.version)
			o.Set("description", r.description)
			o.Set("source", r.source)
			o.Set("removed", r.removed)
			list = append(list, o)
		}
		text, err := pyjson.Dumps(list, pyjson.Indent(2))
		if err != nil {
			return err
		}
		fmt.Fprintln(out, text)
		return nil
	}
	if opts.Plain {
		for _, r := range rows {
			fmt.Fprintf(out, "%-12s %-8s %-8s %s\n", r.status, r.source, r.version, r.name)
		}
		return nil
	}
	if len(entries) == 0 {
		console.Print("[dim]No plugins matched the selected filters.[/dim]")
		return nil
	}
	table := newPluginTable("Plugins", [][2]string{{"Name", "bold"}, {"Status", ""}, {"Version", "dim"}, {"Description", ""}, {"Source", "dim"}})
	userDir, err := pluginsDir(h)
	if err != nil {
		return err
	}
	provClasses := map[string]string{}
	for _, p := range PluginsProvenance(ctx, userDir) {
		provClasses[p.Name] = string(p.Class)
	}
	var removedLines []string
	for _, r := range rows {
		klass := provClasses[r.name]
		status, ok := statusMarkup[r.status]
		if !ok {
			status = "[yellow]not enabled[/yellow]"
		}
		name := r.name
		if r.removed != nil {
			name = fmt.Sprintf("[red]%s ✗[/red]", name)
			removedLines = append(removedLines, fmt.Sprintf("[red]✗ %s[/red] was removed from the plugin catalog: %s", name, r.removed))
		}
		table.AddRow(name, status, r.version, r.description, r.source)
		if (r.source == "user" || r.source == "git") && klass != "" && klass != "git" {
			table.AddRow("", "", "", fmt.Sprintf("[dim]provenance: %s[/dim]", klass), "")
		}
	}
	console.Print("")
	console.printTable(table)
	for _, line := range removedLines {
		console.Print(line)
	}
	console.Print("")
	console.Print("[dim]Compact view:[/dim] icebath plugins list --plain --no-bundled")
	console.Print("[dim]Interactive toggle:[/dim] icebath plugins")
	console.Print("[dim]Enable/disable:[/dim] icebath plugins enable/disable <name>")
	console.Print("[dim]Plugins are opt-in by default — only 'enabled' plugins load.[/dim]")
	return nil
}

// icebath plugins show printing one plugin's details to out
func CmdShow(ctx context.Context, name string, out io.Writer) error {
	return cmdShow(ctx, name, NewConsole(out))
}

// Details of one plugin with its isolation, sign in notice and declared emits and listens
func cmdShow(ctx context.Context, name string, console *Console) error {
	h := home.Dir(ctx)
	match, err := findPluginEntry(ctx, name)
	if err != nil {
		return err
	}
	if match == nil {
		console.Print(fmt.Sprintf("[red]Plugin '%s' not found.[/red]", name))
		return fail(console, "[dim]List installed plugins:[/dim] icebath plugins list")
	}
	manifest := map[string]any{}
	if match.Dir != "" {
		if manifest, err = readManifest(match.Dir); err != nil {
			return err
		}
	}
	emits, err := pyStrItems(manifest["emits"])
	if err != nil {
		return err
	}
	listens, err := pyStrItems(manifest["listens"])
	if err != nil {
		return err
	}
	status, err := pluginStatus(h, *match, getEnabledSet(h), getDisabledSet(h), nil, false)
	if err != nil {
		return err
	}
	title := fmt.Sprintf("[bold]%s[/bold]", match.Name)
	if match.Version != "" {
		title += fmt.Sprintf(" [dim]v%s[/dim]", match.Version)
	}
	console.Print("")
	console.Print(title)
	if match.Description != "" {
		console.Print(match.Description)
	}
	console.Print("[dim]Status:[/dim] " + status)
	console.Print("[dim]Source:[/dim] " + match.Source)
	console.Print("[dim]Key:[/dim] " + match.Key)
	if match.Dir != "" && match.Source != "bundled" {
		audit, err := auditPluginDir(match.Dir, manifest)
		if err != nil {
			return err
		}
		mode := IsolationMode(ctx, nil)
		runsIn := "Icebath process"
		if mode == IsolationHost && audit.HostReady() {
			runsIn = "plugin host"
		} else if mode == IsolationHost {
			runsIn = "refused (plugins.isolation: host)"
		}
		console.Print(fmt.Sprintf("[dim]Runs in:[/dim] %s [dim]— %s[/dim]", runsIn, audit.Summary()))
	}
	svc := installedServices()
	if svc == nil || svc.RequiresAuthNotice == nil {
		return &ServiceMissingError{Name: "RequiresAuthNotice"}
	}
	if notice := svc.RequiresAuthNotice(manifest); notice != "" {
		console.Print("[dim]Sign-in:[/dim] " + notice)
	}
	console.Print("[dim]Emits:[/dim] " + orNone(emits))
	console.Print("[dim]Listens:[/dim] " + orNone(listens))
	console.Print("")
	return nil
}

func orNone(joined []string) string {
	if len(joined) == 0 {
		return "[dim](none)[/dim]"
	}
	return strings.Join(joined, ", ")
}

func newPluginTable(title string, columns [][2]string) *pyrich.Table {
	t := &pyrich.Table{Title: title}
	for _, c := range columns {
		t.Columns = append(t.Columns, pyrich.Column{Header: c[0], Style: c[1]})
	}
	return t
}

// Prints a table fitted to the console width
func (c *Console) printTable(t *pyrich.Table) {
	fmt.Fprintln(c.Out, t.RenderWidth(c.Color, c.width()))
}
