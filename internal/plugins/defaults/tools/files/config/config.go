package config

import toolsconfig "github.com/nickheyer/icebath/internal/tools/config"

const (
	Key = toolsconfig.Key + ".files"
	// Most characters one read_file call returns
	ReadMaxChars = Key + ".read_max_chars"
	// Ask before edits of files that steer agents, such as AGENTS.md
	ProtectInstructions = Key + ".protected_instruction_files"
	// More file name patterns of files that steer agents
	ExtraInstructions = Key + ".protected_instruction_extra_patterns"
	// Folders the file tools may write in, empty for any
	WriteSafeRoot = Key + ".write_safe_root"
	// Files of this machine read, written and searched directly rather than through shell commands
	NativeRead = Key + ".native_read"
	// Refuses a write to a file the agent has not read in full or that changed since it read it
	StateGuard = Key + ".state_guard"
	// Variable that turns the stale write guard off for a run, whatever the setting says
	NoStateGuardEnv = "ICEBATH_DISABLE_FILE_STATE_GUARD"
)
