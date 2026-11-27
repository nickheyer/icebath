package config

import toolsconfig "github.com/nickheyer/icebath/internal/tools/config"

const (
	Key = toolsconfig.Key + ".code_execution"
	// Longest one run may take, in seconds
	Timeout = Key + ".timeout"
	// Most tool calls one run may make
	MaxToolCalls = Key + ".max_tool_calls"
	// Code sessions kept open at once, and how long an idle one stays, in seconds
	MaxSessionKernels = Key + ".max_session_kernels"
	KernelIdleTimeout = Key + ".kernel_idle_timeout"
	// Folder scripts run in, one of the modes
	Mode = Key + ".mode"
)

// Folders scripts run in: the session's working folder, or a scratch one of their own
const (
	Project = "project"
	Strict  = "strict"
)
