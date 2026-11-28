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
	// A missing server is installed the first time a file needs it, else only servers already installed are used
	InstallWhenNeeded = Key + ".install_when_needed"
	// npm, pnpm or yarn, which installs the servers written for Node
	PackageManager = Key + ".package_manager"
	// Seconds an unused server runs before it is stopped
	IdleTimeout = Key + ".idle_timeout"
	// By server: its command, variables, start options, whether it is off, and for one a person added its file types and project files
	Servers = Key + ".servers"
	// Folder of the PowerShellEditorServices bundle the PowerShell server runs
	PowerShellBundle = Key + ".powershell_bundle"
)

// Fields of one server under Servers
const (
	Command     = "command"
	Extensions  = "extensions"
	RootMarkers = "root_markers"
	LanguageID  = "language_id"
	Description = "description"
	Env         = "env"
	Init        = "initialization_options"
	Disabled    = "disabled"
)

// States of a server, in the words every page, command and message says them
const (
	Running      = "Running"
	Installed    = "Installed"
	NotInstalled = "Not installed"
	Off          = "Off"
	Failed       = "Failed"
)

// Values of the choice settings
const (
	WaitDocument = "document"
	WaitFull     = "full"
)
