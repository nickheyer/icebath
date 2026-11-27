package config

import (
	"github.com/nickheyer/icebath/internal/config"
	toolsconfig "github.com/nickheyer/icebath/internal/tools/config"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{
		Key:    Key,
		Parent: toolsconfig.Key,
		Title:  "Files",
		About:  "File reads and writes, and files that need your approval",
		Order:  20,
		Settings: []config.Setting{
			{Key: ProtectInstructions, Title: "Protect instruction files", About: "Ask before the agent edits files that steer agents, such as AGENTS.md.",
				Help: "Covers AGENTS.md, CLAUDE.md, SOUL.md and .cursorrules.", Default: true},
			{Key: ExtraInstructions, Title: "More protected files", About: "More file names that the agent must ask about before it edits them.",
				Help: "Patterns such as *.prompt.md.", Default: []string{}},
			{Key: WriteSafeRoot, Title: "Writable folders", About: "The folders that the file tools may write in.",
				Help: "Separate several folders as in PATH. Protected files still ask first.", Default: "", Zero: "Anywhere",
				Env: []string{"ICEBATH_WRITE_SAFE_ROOT"}},
			{Key: ReadMaxChars, Title: "Read limit", About: "The most text that one file read returns.",
				Help: "A longer read stops at a whole line, and the agent reads on from there.", Default: 100000, Counts: "characters", Least: config.Num(1),
				Advanced: true},
			{Key: NativeRead, Title: "Direct file access", About: "Read, write and search files on this computer directly instead of through commands.",
				Default: true, Env: []string{"ICEBATH_NATIVE_FILE_READ"}, Advanced: true},
			{Key: DisableStateGuard, Title: "Skip the stale-write check", About: "Let the agent replace files it has not read in full, and edit over newer changes.",
				Default: false, Env: []string{"ICEBATH_DISABLE_FILE_STATE_GUARD"}, Advanced: true},
		},
	}})
}
