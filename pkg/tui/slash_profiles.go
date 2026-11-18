package tui

import (
	"context"
	"path/filepath"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/i18n"
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

// Path made absolute against the folder this process works in, empty kept empty
func absolutePath(path string) string {
	if path == "" {
		return ""
	}
	if abs, err := filepath.Abs(path); err == nil {
		return abs
	}
	return path
}

// Value of a param that is null when empty
func nullable(text string) any {
	if text == "" {
		return nil
	}
	return text
}

// Shows the lines of a profile archive command, its failure as the shared error line
func (a *app) archiveOutput(name string, err error, lines func() []string) tea.Cmd {
	if err != nil {
		return a.showOutput(commandText(a.ctx, "shared.error", map[string]any{"error": rpcErrorMessage(err)}), name)
	}
	return a.showOutput(strings.Join(lines(), "\n"), name)
}

// Exports the named profile, the connection's own when no name is given, to an archive
func (a *app) exportProfile(arg string) tea.Cmd {
	parts, output, ok := takeFlag(strings.Fields(arg), "-o")
	if !ok {
		return a.showOutput(commandText(a.ctx, "export.usage", nil), "export")
	}
	name := ""
	if len(parts) > 0 {
		name = parts[0]
	}
	return gatewayCall(a.gw, "profiles.export", map[string]any{"name": nullable(name), "output": nullable(absolutePath(output))}, func(v any, err error) tea.Cmd {
		res := asRPCResult(v)
		return a.archiveOutput("export", err, func() []string {
			return []string{commandText(a.ctx, "export.exported", map[string]any{"name": jsValueString(res["name"]), "path": jsValueString(res["path"])}),
				commandText(a.ctx, "export.share_hint", nil)}
		})
	})
}

// Imports a shared archive as a new profile with its wrapper alias
func (a *app) importProfile(arg string) tea.Cmd {
	parts, name, ok := takeFlag(strings.Fields(arg), "--name")
	if !ok || len(parts) == 0 {
		return a.showOutput(commandText(a.ctx, "import.usage", nil), "import")
	}
	params := map[string]any{"archive": absolutePath(strings.Join(parts, " ")), "name": nullable(name)}
	return gatewayCall(a.gw, "profiles.import", params, func(v any, err error) tea.Cmd {
		res := asRPCResult(v)
		return a.archiveOutput("import", err, func() []string {
			imported := jsValueString(res["name"])
			lines := []string{commandText(a.ctx, "import.imported", map[string]any{"name": imported, "path": jsValueString(res["path"])})}
			if wrapper, made := res["wrapper"].(string); made && wrapper != "" {
				lines = append(lines, commandText(a.ctx, "import.wrapper_created", map[string]any{"path": wrapper}))
			}
			return append(lines, commandText(a.ctx, "import.use_it", map[string]any{"name": imported}))
		})
	})
}

// Slash commands that export a profile to a shareable archive and import one as a new profile
func (a *app) profileArchiveCommands() []slashCommand {
	return []slashCommand{
		{name: "export", help: "export a profile (config, skills, theme) to a shareable archive", usage: "[profile] [-o output.tar.gz]",
			run: func(r *slashRun) tea.Cmd { return tea.Batch(r.ran(), a.exportProfile(r.arg)) }},
		{name: "import", help: "import a shared profile archive as a new profile", usage: "<archive.tar.gz> [--name <name>]",
			run: func(r *slashRun) tea.Cmd { return tea.Batch(r.ran(), a.importProfile(r.arg)) }},
	}
}
