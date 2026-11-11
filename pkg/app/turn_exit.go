package app

import "github.com/nickheyer/icebath/pkg/kanban"

// TransientProviderReasons are failure reasons a Kanban retry can heal, the kanban table shared with workers
var TransientProviderReasons = kanban.TransientProviderReasons

// TerminalProviderReasons are failure reasons needing an operator change, the kanban table shared with workers
var TerminalProviderReasons = kanban.TerminalProviderReasons

// TurnExitOptions carries the worker identity and credential failure flags for the exit code mapping
type TurnExitOptions = kanban.TurnExitOptions

// TurnExitCode maps a turn result onto the process exit code, preserving completion, interruption and provider outcomes
func TurnExitCode(result map[string]any, o TurnExitOptions) int {
	return kanban.TurnExitCode(result, o)
}

// CredentialFailureFlags reads the exit code credential flags from an error raised before any turn ran
func CredentialFailureFlags(err error) (rateLimited, terminal, found bool) {
	return kanban.CredentialFailureFlags(err)
}
