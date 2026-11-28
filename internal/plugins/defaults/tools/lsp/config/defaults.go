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
		About:  "Programs that check the code the agent writes for errors",
		Area:   true,
		Group:  config.GroupAbilities,
		Order:  14,
		Settings: []config.Setting{
			{Key: Enabled, Title: "Check code", About: "Check the files the agent edits for problems, using language servers.", Default: true},
			{Key: InstallWhenNeeded, Title: "Install when needed", About: "Install a missing language server the first time a file needs it.",
				Help: "When off, only the servers you installed are used.", Default: true},
			{Key: WaitMode, Title: "What to wait for", About: "How much a check waits for after an edit before it reports problems.", Default: WaitDocument,
				Options: config.Options(
					config.Option{Value: WaitDocument, Title: "Edited file", About: "A shorter wait, for the edited file alone"},
					config.Option{Value: WaitFull, Title: "Full result", About: "A longer wait, for a fuller result"},
				)},
			{Key: ExcludeRoots, Title: "Skipped folders", About: "Folders where code is never checked.",
				Help: "A folder or a pattern, such as ~/big-monorepo or /srv/*/vendor.", Default: []string{}, Entry: &config.Entry{Noun: "folder"},
				Path: &config.Path{Kind: config.PathFolder}},
			{Key: Servers, Title: "Servers", About: "Language servers you added, and your changes to the others.", Default: map[string]any{}, Custom: true,
				Entry: &config.Entry{Noun: "server"}},
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
				Default: "", Zero: "Profile's lsp folder", Env: []string{"PSES_BUNDLE_PATH"}, Advanced: true, Path: &config.Path{Kind: config.PathFolder}},
		},
	}})
}

// Settings of one server, each a field of its entry under Servers, which its page shows as option rows; whether it is off is the page's
// to change with its own row. Program is the name its program goes by, for the example of its command. Added is a server a person added,
// whose command is the whole command line and whose file types and project files are theirs to give
func Fields(prefix, program string, added bool) []config.Setting {
	command := config.Setting{Key: prefix + "." + Command, Title: "Command", About: "The program to run in place of the one icebath finds.",
		Help: "Such as /usr/local/bin/" + program + ".", Default: "", Zero: "The one icebath finds", Path: &config.Path{Kind: config.PathProgram, Exists: true}}
	if added {
		// A whole command line, which is no path
		command.About, command.Help, command.Zero, command.Path = "The command that starts the server.", "It is split as a shell splits a line.", "", nil
	}
	fields := []config.Setting{command,
		{Key: prefix + "." + Env, Title: "Variables", About: "Variables the server gets as it starts.", Default: map[string]any{},
			Entry: &config.Entry{Noun: "variable", Name: "Variable name", Fields: []config.Setting{
				{Title: "Value", About: "The text that the variable holds.", Default: ""},
			}}},
		{Key: prefix + "." + Init, Title: "Start options", About: "Options handed to the server as it starts.",
			Help: "Such as the ones its own documentation lists. A group of options opens as a list of its own.", Default: map[string]any{},
			Entry: &config.Entry{Noun: "option", Name: "Option name"}},
		{Key: prefix + "." + Disabled, Title: "Off", About: "Never start this server.", Default: false, Custom: true},
	}
	if !added {
		return fields
	}
	return append(fields,
		config.Setting{Key: prefix + "." + Extensions, Title: "File types", About: "The file endings the server checks.",
			Help: "Such as .go or .mod, or the whole name of a file such as Dockerfile.", Default: []string{},
			Entry: &config.Entry{Noun: "file type"}},
		config.Setting{Key: prefix + "." + RootMarkers, Title: "Project files", About: "Files that mark the top folder of a project.",
			Help: "Such as go.mod. The folder that holds one is where the server starts.", Default: []string{},
			Entry: &config.Entry{Noun: "project file"}},
		config.Setting{Key: prefix + "." + LanguageID, Title: "Language name", About: "The name of the language the server is told for each file.",
			Help: "Such as go. Leave it empty and icebath tells it the name it knows for the file type.", Default: "", Zero: "Known from the file type",
			Advanced: true},
		config.Setting{Key: prefix + "." + Description, Title: "Description", About: "A few words about the server, shown with it.", Default: "",
			Advanced: true},
	)
}
