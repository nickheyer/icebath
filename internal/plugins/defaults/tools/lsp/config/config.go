package config

import toolsconfig "github.com/nickheyer/icebath/internal/tools/config"

const (
	Key = toolsconfig.Key + ".lsp"
	// Language servers check the files the agent writes
	Enabled = Key + ".enabled"
	// How much a check waits for after an edit: the edited file, or the fuller result
	WaitMode = Key + ".wait_mode"
	// Seconds a check waits for a server's verdict
	WaitTimeout = Key + ".wait_timeout"
	// Seconds a server that has not started yet gets instead, when longer
	WarmupTimeout = Key + ".warmup_timeout"
	// Seconds before a server that failed is tried again, never for zero
	BrokenRetry = Key + ".broken_retry_seconds"
	// Glob patterns of the folders no server runs in
	ExcludeRoots = Key + ".exclude_roots"
	// Missing servers are installed when a file needs them, or only used once installed
	InstallStrategy = Key + ".install_strategy"
	// npm, pnpm or yarn, which installs the servers written for Node
	PackageManager = Key + ".package_manager"
	// Seconds an unused server runs before it is stopped
	IdleTimeout = Key + ".idle_timeout"
	// By server: its command, variables, initialization options, or disabled; a new name with extensions declares a server
	Servers = Key + ".servers"
	// Folder of the PowerShellEditorServices bundle the PowerShell server runs
	PowerShellBundle = Key + ".powershell_bundle"
)

// Values of the choice settings
const (
	WaitDocument  = "document"
	WaitFull      = "full"
	InstallAuto   = "auto"
	InstallManual = "manual"
)
