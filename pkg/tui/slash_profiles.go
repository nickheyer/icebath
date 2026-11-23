package tui

import (
	"context"
	"path/filepath"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/tuigateway"
	"github.com/nickheyer/icebath/pkg/ui/txt"
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
	params := tuigateway.ProfilesExportParams{Name: name, Output: absolutePath(output)}
	return call(a.gw, tuigateway.ProfilesExport, params, func(res *tuigateway.ProfilesExportResult, err error) tea.Cmd {
		return a.archiveOutput("export", err, func() []string {
			return []string{commandText(a.ctx, "export.exported", map[string]any{"name": res.Name, "path": res.Path}),
				commandText(a.ctx, "export.share_hint", nil)}
		})
	})
}

// Why restoring a backup waits until icebath closes, as the command tree words it
const restoreBackupReason = "It restores a backup over the data this window uses."

// Queues a restore of a home backup zip for after icebath closes, the CLI's icebath import
func (a *app) restoreBackup(arg string) tea.Cmd {
	parts := strings.Fields(arg)
	force := slices.Contains(parts, "--force") || slices.Contains(parts, "-f")
	parts = slices.DeleteFunc(parts, func(p string) bool { return p == "--force" || p == "-f" })
	if len(parts) == 0 {
		return a.showOutput(tr(txt.HubsProfilesRestoreUsage), "import")
	}
	argv := []string{"import", absolutePath(profilesExpandHome(strings.Join(parts, " ")))}
	if force {
		argv = append(argv, "--force")
	}
	return a.queueAfterExitCmd(argv, restoreBackupReason)
}

// Slash commands that export a profile to a shareable archive and restore a home backup after icebath closes, as the CLI's import does
func (a *app) profileArchiveCommands() []slashCommand {
	return []slashCommand{
		{name: "export", help: "export a profile (config, skills, theme) to a shareable archive", usage: "[profile] [-o output.tar.gz]",
			run: func(r *slashRun) tea.Cmd { return tea.Batch(r.ran(), a.exportProfile(r.arg)) }},
		{name: "import", help: "restore an icebath backup zip once icebath closes", usage: "<backup.zip> [--force]",
			run: func(r *slashRun) tea.Cmd { return tea.Batch(r.ran(), a.restoreBackup(r.arg)) }},
	}
}
