package tuigateway

import (
	"encoding/json"
	"slices"
	"strconv"
)

// Edit approval policy of an editor session
type EditApprovalPolicy string

func (EditApprovalPolicy) Values() []string { return []string{"ask", "workspace_session", "session"} }

// Settings, inventory and live state of a session, every field optional so a sender fills what it knows and a patch carries what changed
type SessionInfo struct {
	Model                    *string                     `json:"model,omitzero"`
	Provider                 *string                     `json:"provider,omitzero"`
	ReasoningEffort          *string                     `json:"reasoning_effort,omitzero"`
	ReasoningEffortWire      *string                     `json:"reasoning_effort_wire,omitzero"`
	ServiceTier              *string                     `json:"service_tier,omitzero"`
	Fast                     *bool                       `json:"fast,omitzero"`
	Yolo                     *bool                       `json:"yolo,omitzero"`
	ApprovalMode             *string                     `json:"approval_mode,omitzero"`
	Tools                    map[string][]string         `json:"tools,omitzero"`
	Skills                   map[string][]string         `json:"skills,omitzero"`
	Cwd                      *string                     `json:"cwd,omitzero"`
	Branch                   *string                     `json:"branch,omitzero"`
	Project                  *ProjectRef                 `json:"project,omitzero"`
	TerminalBackend          *string                     `json:"terminal_backend,omitzero"`
	Personality              *string                     `json:"personality,omitzero"`
	Running                  *bool                       `json:"running,omitzero"`
	TurnStartedAt            *float64                    `json:"turn_started_at,omitzero"`
	Title                    *string                     `json:"title,omitzero"`
	StoredSessionID          *string                     `json:"stored_session_id,omitzero"`
	Version                  *string                     `json:"version,omitzero"`
	ReleaseDate              *string                     `json:"release_date,omitzero"`
	UpdateBehind             any                         `json:"update_behind,omitzero"`
	UpdateCommand            *string                     `json:"update_command,omitzero"`
	Usage                    *Usage                      `json:"usage,omitzero"`
	ProfileName              *string                     `json:"profile_name,omitzero"`
	ProfileID                *string                     `json:"profile_id,omitzero"`
	RedactSecrets            *bool                       `json:"redact_secrets,omitzero"`
	MCPServers               []McpServerStatus           `json:"mcp_servers,omitzero"`
	SystemPrompt             *string                     `json:"system_prompt,omitzero"`
	CredentialWarning        *string                     `json:"credential_warning,omitzero"`
	Login                    *SessionLogin               `json:"login,omitzero"`
	PoolKey                  *string                     `json:"pool_key,omitzero"`
	CredentialHint           *string                     `json:"credential_hint,omitzero"`
	ConfigWarning            *string                     `json:"config_warning,omitzero"`
	Lazy                     *bool                       `json:"lazy,omitzero"`
	Source                   *string                     `json:"source,omitzero"`
	GatewayProtocol          *string                     `json:"gateway_protocol,omitzero"`
	EditApprovalPolicy       *EditApprovalPolicy         `json:"edit_approval_policy,omitzero"`
	LaunchRequest            map[string]any              `json:"launch_request,omitzero"`
	LaunchOptions            *SessionLaunchOptions       `json:"launch_options,omitzero"`
	StartedAt                *float64                    `json:"started_at,omitzero"`
	LastActiveAt             *float64                    `json:"last_active_at,omitzero"`
	APIMode                  *string                     `json:"api_mode,omitzero"`
	ToolCount                *int64                      `json:"tool_count,omitzero"`
	ContextLength            *int64                      `json:"context_length,omitzero"`
	EnabledToolsets          []string                    `json:"enabled_toolsets,omitzero"`
	ToolAvailability         map[string]any              `json:"tool_availability,omitzero"`
	ToolAvailabilityWarnings []string                    `json:"tool_availability_warnings,omitzero"`
	UnknownToolsets          []string                    `json:"unknown_toolsets,omitzero"`
	ProjectSkillsLoaded      *int64                      `json:"project_skills_loaded,omitzero"`
	ProjectSkillsUntrusted   *UntrustedProjectSkills     `json:"project_skills_untrusted,omitzero"`
	AuthorityEpoch           *int64                      `json:"authority_epoch,omitzero"`
	Revision                 *int64                      `json:"revision,omitzero"`
	ExecutionGeneration      *int64                      `json:"execution_generation,omitzero"`
	Pending                  []CanonicalPendingAdmission `json:"pending,omitzero"`
	PendingSubmissions       []PendingSubmission         `json:"pending_submissions,omitzero"`
}

// How a session runs as its launch options left it, the values its next turn reads
type SessionLaunchOptions struct {
	Checkpoints      bool `json:"checkpoints"`
	PassSessionID    bool `json:"pass_session_id"`
	IgnoreRules      bool `json:"ignore_rules"`
	SafeMode         bool `json:"safe_mode"`
	IgnoreUserConfig bool `json:"ignore_user_config"`
	// Most tool calls of one request, absent for no limit
	MaxTurns *int64 `json:"max_turns,omitzero"`
	// Seconds one run may take, absent for no limit
	RunBudget *float64 `json:"run_budget,omitzero"`
	// Skills the launch wrote into the system prompt
	Skills []string `json:"skills,omitzero"`
	// Model endpoint the session calls, absent when its provider sets none
	BaseURL *string `json:"base_url,omitzero"`
}

// Skills found under a project root the user has not trusted, left unloaded until it is
type UntrustedProjectSkills struct {
	Root  string `json:"root"`
	Count int64  `json:"count"`
}

// One prompt a forwarding owner queued for the session, shown as a user bubble once it starts
type PendingSubmission struct {
	AdmissionID string `json:"admission_id"`
	Status      string `json:"status"`
	User        string `json:"user"`
	InputID     string `json:"input_id"`
}

// Kind of event a goal waits for
type WaitBarrierType string

func (WaitBarrierType) Values() []string { return []string{"until", "session", "pid"} }

// What an active goal waits for, a time, a session or a process
type WaitBarrier struct {
	Type      WaitBarrierType `json:"type"`
	UntilAt   *float64        `json:"until_at,omitzero"`
	SessionID *string         `json:"session_id,omitzero"`
	PID       *int64          `json:"pid,omitzero"`
	Reason    string          `json:"reason,omitzero"`
}

// Reads a barrier, the target a gateway of an earlier release sends read as the session id or the pid its type names
func (b *WaitBarrier) UnmarshalJSON(data []byte) error {
	type fields WaitBarrier
	var wire struct {
		fields
		Target any `json:"target"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	if !slices.Contains(wire.Type.Values(), string(wire.Type)) {
		return &FitError{Field: "type", Problem: "wants one of " + enumText(wire.Type.Values()) + ", got " + strconv.Quote(string(wire.Type))}
	}
	*b = WaitBarrier(wire.fields)
	switch target := wire.Target.(type) {
	case string:
		if b.SessionID == nil {
			b.SessionID = &target
		}
	case float64:
		if pid := int64(target); b.PID == nil {
			b.PID = &pid
		}
	}
	return nil
}

// Params of an event frame, the session, payload and the owner stamps a canonical gateway adds
type EventEnvelope struct {
	Type                string         `json:"type"`
	SessionID           string         `json:"session_id,omitzero"`
	Payload             map[string]any `json:"payload,omitzero"`
	ReplayEpoch         *string        `json:"replay_epoch,omitzero"`
	Seq                 *int64         `json:"seq,omitzero"`
	AuthorityEpoch      *int64         `json:"authority_epoch,omitzero"`
	ExecutionGeneration *int64         `json:"execution_generation,omitzero"`
	AdmissionID         *string        `json:"admission_id,omitzero"`
}

// Params of a method a client may route to a named profile
type ProfileParams struct {
	Profile *string `json:"profile,omitzero"`
}

// Whether any provider is configured, for the launch profile or the one named
type SetupStatusResult struct {
	ProviderConfigured *bool   `json:"provider_configured,omitzero"`
	Profile            *string `json:"profile,omitzero"`
	OK                 *bool   `json:"ok,omitzero"`
	Error              *string `json:"error,omitzero"`
}

// Whether any provider is configured for the launch or a named profile, an unknown profile answering not ok with the error
var SetupStatus = NewMethod[ProfileParams, SetupStatusResult]("setup.status")

// Provider a runtime check resolves alone, or none for the route a new session would take, in the launch or a named profile
type SetupRuntimeCheckParams struct {
	Provider *string `json:"provider,omitzero"`
	Profile  *string `json:"profile,omitzero"`
}

// Whether the resolved route can serve a session, with its provider, model and credential source, or the error and its code
type SetupRuntimeCheckResult struct {
	OK        bool    `json:"ok"`
	Provider  *string `json:"provider,omitzero"`
	Model     *string `json:"model,omitzero"`
	Source    *string `json:"source,omitzero"`
	Error     *string `json:"error,omitzero"`
	ErrorCode *string `json:"error_code,omitzero"`
	Profile   *string `json:"profile,omitzero"`
}

// Strict check of the route a session would be built with, one provider alone when named
var SetupRuntimeCheck = NewMethod[SetupRuntimeCheckParams, SetupRuntimeCheckResult]("setup.runtime_check")
