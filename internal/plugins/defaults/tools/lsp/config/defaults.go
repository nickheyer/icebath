package config

import (
	"time"

	"github.com/nickheyer/icebath/internal/config"
	toolsconfig "github.com/nickheyer/icebath/internal/tools/config"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{
		Key:    Key,
		Parent: toolsconfig.Key,
		Title:  "Language servers",
		About:  "Checks for errors in the code that the agent edits",
		Order:  70,
		Settings: []config.Setting{
			{Key: Enabled, Title: "Check code", About: "Check the files the agent edits for problems, using language servers.", Default: true},
			{Key: WaitMode, Title: "What to wait for", About: "How much a check waits for after an edit before it reports problems.", Default: WaitDocument,
				Options: config.Options(
					config.Option{Value: WaitDocument, Title: "Edited file", About: "A shorter wait, for the edited file alone"},
					config.Option{Value: WaitFull, Title: "Full result", About: "A longer wait, for a fuller result"},
				)},
			{Key: InstallStrategy, Title: "Install servers", About: "Whether missing language servers are installed for you.", Default: InstallAuto,
				Options: config.Options(
					config.Option{Value: InstallAuto, Title: "Automatic", About: "A missing server is installed when a file needs it"},
					config.Option{Value: InstallManual, Title: "Manual", About: "Only servers you installed yourself are used"},
				)},
			{Key: ExcludeRoots, Title: "Skipped folders", About: "Folders where code is never checked.",
				Help: "Glob patterns such as ~/big-monorepo or /srv/*/vendor.", Default: []string{}},
			{Key: Servers, Title: "Servers", About: "Language servers you add, and changes to the built-in ones.",
				Help: "A new server needs a command and the file extensions that it checks.", Default: map[string]any{},
				Entry: &config.Entry{Noun: "server", Name: "Server ID", Fields: []config.Setting{
					{Key: "command", Title: "Command", About: "The program that starts the server, then its arguments.", Default: []string{}},
					{Key: "extensions", Title: "File extensions", About: "The file extensions that a server you add checks, such as .go.",
						Default: []string{}},
					{Key: "root_markers", Title: "Project markers", About: "Files that mark the top folder of a project, such as go.mod.",
						Default: []string{}},
					{Key: "language_id", Title: "Language ID", About: "The language name that the server is told for each file.", Default: ""},
					{Key: "description", Title: "Description", About: "A short account of the server, shown in /lsp.", Default: ""},
					{Key: "env", Title: "Variables", About: "Variables set for the server, by name.", Default: map[string]any{}},
					{Key: "initialization_options", Title: "Start options", About: "Options handed to the server as it starts.", Default: map[string]any{}},
					{Key: "disabled", Title: "Turned off", About: "Never start this server.", Default: false},
				}}},
			{Key: WaitTimeout, Title: "Check wait", About: "How long a check waits for problems after an edit.", Default: 5 * time.Second, Advanced: true},
			{Key: WarmupTimeout, Title: "Start-up wait", About: "How long a check waits for a server that has not started yet.",
				Help: "Only counts when longer than Check wait.", Default: time.Duration(0), Zero: "No extra wait", Advanced: true},
			{Key: BrokenRetry, Title: "Retry failed servers", About: "How long to wait before a server that failed is tried again.", Default: time.Duration(0),
				Zero: "Not until restart", Advanced: true},
			{Key: IdleTimeout, Title: "Stop when idle", About: "How long an unused language server runs before it is stopped.",
				Help: "At least 30 seconds.", Default: 10 * time.Minute, Zero: "Never", Advanced: true},
			{Key: PackageManager, Title: "Node package manager", About: "Which program installs language servers written for Node.", Default: "npm",
				Options: config.Options(
					config.Option{Value: "npm", Title: "npm", About: "Install with npm"},
					config.Option{Value: "pnpm", Title: "pnpm", About: "Install with pnpm"},
					config.Option{Value: "yarn", Title: "yarn", About: "Install with yarn"},
				), Advanced: true},
			{Key: PowerShellBundle, Title: "PowerShell editor services", About: "The folder of the bundle that the PowerShell language server runs.",
				Default: "", Zero: "Profile's lsp folder", Env: []string{"PSES_BUNDLE_PATH"}, Advanced: true},
		},
	}})
}
