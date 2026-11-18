package tools

import "github.com/nickheyer/icebath/pkg/internal/pyrepr"

// Exit codes of a kanban worker the dispatcher reads, a healable provider failure and one needing an operator
const (
	KanbanRateLimitExitCode        = 75
	KanbanTerminalProviderExitCode = 78
)

// TransientProviderReasons are failure reasons a Kanban retry can heal, so they never spend the task's retry budget
var TransientProviderReasons = map[string]bool{
	"rate_limit": true, "upstream_rate_limit": true, "billing": true, "overloaded": true, "server_error": true, "timeout": true,
}

// TerminalProviderReasons are failure reasons needing an operator change, parking the task immediately
var TerminalProviderReasons = map[string]bool{
	"auth": true, "auth_permanent": true, "model_not_found": true, "ssl_cert_verification": true, "upstream_blocked": true,
}

// TurnExitOptions carries the worker identity and credential failure flags for the exit code mapping
type TurnExitOptions struct {
	KanbanWorker           bool
	CredentialsRateLimited bool
	CredentialsTerminal    bool
	TransientReasons       map[string]bool
	TerminalReasons        map[string]bool
}

// TurnExitCode maps a turn result onto the process exit code, preserving completion, interruption and provider outcomes
func TurnExitCode(result map[string]any, o TurnExitOptions) int {
	transient, terminal := o.TransientReasons, o.TerminalReasons
	if transient == nil {
		transient = TransientProviderReasons
	}
	if terminal == nil {
		terminal = TerminalProviderReasons
	}
	if result == nil {
		if o.CredentialsRateLimited && o.KanbanWorker {
			return KanbanRateLimitExitCode
		}
		if o.CredentialsTerminal && o.KanbanWorker {
			return KanbanTerminalProviderExitCode
		}
		return 1
	}
	if pyrepr.Truthy(result["interrupted"]) {
		return 130
	}
	completed, hasCompleted := result["completed"].(bool)
	if !(pyrepr.Truthy(result["failed"]) || pyrepr.Truthy(result["partial"]) || (hasCompleted && !completed)) {
		return 0
	}
	if o.KanbanWorker {
		reason, _ := result["failure_reason"].(string)
		if transient[reason] {
			return KanbanRateLimitExitCode
		}
		if terminal[reason] {
			return KanbanTerminalProviderExitCode
		}
	}
	return 1
}
