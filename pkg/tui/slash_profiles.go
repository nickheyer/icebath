package tui

import (
	"context"
	"path/filepath"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/profiles/manage"
)

// Catalog line of the shared command texts the terminal UI prints for a slash command
func commandText(ctx context.Context, key string, kwargs map[string]any) string {
	return i18n.T(ctx, "cli.commands."+key, "", kwargs)
}

// Words without the flag and its value, false when the flag has no value after it
func takeFlag(parts []string, flag string) ([]string, string, bool) {
	for i, p := range parts {
		if p != flag {
			continue
		}
		if i+1 >= len(parts) {
			return parts, "", false
		}
		return append(append([]string(nil), parts[:i]...), parts[i+2:]...), parts[i+1], true
	}
	return parts, "", true
}

// Lines of an export of the named profile, the active one when no name is given
func exportProfileLines(ctx context.Context, arg string) []string {
	parts, output, ok := takeFlag(strings.Fields(arg), "-o")
	if !ok {
		return []string{commandText(ctx, "export.usage", nil)}
	}
	name := profiles.GetActiveProfileName(ctx)
	if len(parts) > 0 {
		name = parts[0]
	}
	if name == "" {
		name = "default"
	}
	target := output
	if target == "" {
		path, err := manage.GetProfileExportPath(name, "")
		if err != nil {
			return []string{commandText(ctx, "shared.error", map[string]any{"error": err.Error()})}
		}
		target = path
	}
	result, err := manage.ExportProfile(name, target, nil)
	if err != nil {
		return []string{commandText(ctx, "shared.error", map[string]any{"error": err.Error()})}
	}
	return []string{commandText(ctx, "export.exported", map[string]any{"name": name, "path": result}), commandText(ctx, "export.share_hint", nil)}
}

// Lines of an import of a shared archive as a new profile with its wrapper alias
func importProfileLines(ctx context.Context, arg string) []string {
	parts, name, ok := takeFlag(strings.Fields(arg), "--name")
	if !ok || len(parts) == 0 {
		return []string{commandText(ctx, "import.usage", nil)}
	}
	profileDir, err := manage.ImportProfile(strings.Join(parts, " "), name)
	if err != nil {
		return []string{commandText(ctx, "shared.error", map[string]any{"error": err.Error()})}
	}
	imported := filepath.Base(profileDir)
	lines := []string{commandText(ctx, "import.imported", map[string]any{"name": imported, "path": profileDir})}
	if collision, err := profiles.CheckAliasCollision(imported); err == nil && collision == "" {
		if wrapper, err := profiles.CreateWrapperScript(imported, ""); err == nil && wrapper != "" {
			lines = append(lines, commandText(ctx, "import.wrapper_created", map[string]any{"path": wrapper}))
		}
	}
	return append(lines, commandText(ctx, "import.use_it", map[string]any{"name": imported}))
}

// Runs a slow command off the update loop and prints the lines it returns
func (a *app) linesAsync(name string, lines func() []string) tea.Cmd {
	return func() tea.Msg {
		out := lines()
		return asyncDoneMsg{run: func() tea.Cmd { return a.showOutput(strings.Join(out, "\n"), name) }}
	}
}

// Slash commands that export a profile to a shareable archive and import one as a new profile
func (a *app) profileArchiveCommands() []slashCommand {
	return []slashCommand{
		{name: "export", help: "export a profile (config, skills, theme) to a shareable archive", usage: "[profile] [-o output.tar.gz]",
			run: func(r *slashRun) tea.Cmd {
				arg := r.arg
				return tea.Batch(r.ran(), a.linesAsync("export", func() []string { return exportProfileLines(a.ctx, arg) }))
			}},
		{name: "import", help: "import a shared profile archive as a new profile", usage: "<archive.tar.gz> [--name <name>]",
			run: func(r *slashRun) tea.Cmd {
				arg := r.arg
				return tea.Batch(r.ran(), a.linesAsync("import", func() []string { return importProfileLines(a.ctx, arg) }))
			}},
	}
}
