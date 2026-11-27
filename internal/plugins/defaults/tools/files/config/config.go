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
	// Lets writes go ahead without the full read or the unchanged file the stale write guard asks for
	DisableStateGuard = Key + ".disable_state_guard"
)
