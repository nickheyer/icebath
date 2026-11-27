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
		About:  "Reading, writing, editing and searching files, and the files the agent may not touch",
		Settings: []config.Setting{
			{Key: ReadMaxChars, Title: "File read limit", About: "Most characters returned from one file read; a longer read is cut at a whole line", Default: 100000},
			{Key: ProtectInstructions, Title: "Protect instruction files", About: "Ask before the agent edits files that steer agents: AGENTS.md, CLAUDE.md, SOUL.md and .cursorrules", Default: true},
			{Key: ExtraInstructions, Title: "Extra protected files", About: "More file names the agent needs approval to edit, as patterns such as *.prompt.md", Default: []string{}},
			{Key: WriteSafeRoot, Title: "Write safe root", About: "Folders the file tools may write in, separated as in PATH; empty lets them write anywhere not protected", Default: "", Env: []string{"ICEBATH_WRITE_SAFE_ROOT"}},
			{Key: NativeRead, Title: "Direct file access", About: "Read, write and search the files of this machine directly rather than through shell commands", Default: true, Env: []string{"ICEBATH_NATIVE_FILE_READ"}},
			{Key: DisableStateGuard, Title: "Disable stale write guard", About: "Let write_file replace files the session hasn't read in full, and edits go ahead over changes made since the last read", Default: false, Env: []string{"ICEBATH_DISABLE_FILE_STATE_GUARD"}},
		},
	}})
}
