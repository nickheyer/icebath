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
		About:  "Language servers that check the files the agent writes and tell it of the errors an edit brought in",
		Settings: []config.Setting{
			{Key: Enabled, Title: "Code checking", About: "Check edited files for problems using language servers", Default: true},
			{Key: WaitMode, Title: "Check wait mode", About: "How much to wait for after an edit before reporting problems", Default: WaitDocument, Options: config.Options(
				config.Option{Value: WaitDocument, Title: "Edited file", About: "Shorter wait for the edited file"},
				config.Option{Value: WaitFull, Title: "Full", About: "Longer wait for a fuller result"},
			)},
			{Key: WaitTimeout, Title: "Check wait time", About: "Longest wait in seconds for code problems after an edit", Default: 5 * time.Second},
			{Key: WarmupTimeout, Title: "Code server warmup", About: "Seconds a language server that has not started yet gets before its first check, when longer than the check " +
				"wait time; zero means no extra wait", Default: time.Duration(0)},
			{Key: BrokenRetry, Title: "Retry failed servers", About: "Seconds to wait before retrying a code server that failed; zero never retries until restart",
				Default: time.Duration(0)},
			{Key: ExcludeRoots, Title: "Skip folders", About: "Folders, as glob patterns such as ~/big-monorepo or /srv/*/vendor, where code checking is never used",
				Default: []string{}},
			{Key: InstallStrategy, Title: "Install language servers", About: "Whether missing language servers are installed for you", Default: InstallAuto, Options: config.Options(
				config.Option{Value: InstallAuto, Title: "Automatic", About: "Install missing servers when needed"},
				config.Option{Value: InstallManual, Title: "Manual", About: "Use only servers you installed"},
			)},
			{Key: PackageManager, Title: "Node package manager", About: "Tool used to install language servers written for Node", Default: "npm", Options: config.Options(
				config.Option{Value: "npm", Title: "npm", About: "Use npm"},
				config.Option{Value: "pnpm", Title: "pnpm", About: "Use pnpm"},
				config.Option{Value: "yarn", Title: "yarn", About: "Use yarn"},
			)},
			{Key: IdleTimeout, Title: "Server idle time", About: "Seconds an unused language server runs before it is stopped, at least 30; zero keeps them running", Default: 10 * time.Minute},
			{Key: Servers, Title: "Language servers", About: "Per-server overrides by server id: command, env, initialization_options or disabled; a new id with command and " +
				"extensions (and root_markers, description, language_id) declares a server of your own", Default: map[string]any{}},
			{Key: PowerShellBundle, Title: "PowerShell editor services", About: "Folder of the PowerShellEditorServices bundle the PowerShell language server runs; " +
				"empty looks in lsp/PowerShellEditorServices of the profile", Default: "", Env: []string{"PSES_BUNDLE_PATH"}},
		},
	}})
}
