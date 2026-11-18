package tuigateway

// Verdict on a dangerous command
type ApprovalChoice string

func (ApprovalChoice) Values() []string { return []string{"once", "session", "always", "deny"} }

// Dangerous command awaiting a verdict, the command redacted by the gateway, extra keys of the approval kept
type ApprovalRequestParams struct {
	SessionID        string           `json:"session_id"`
	RequestID        string           `json:"request_id"`
	Command          string           `json:"command,omitzero"`
	Description      string           `json:"description,omitzero"`
	Choices          []ApprovalChoice `json:"choices,omitzero"`
	AllowPermanent   *bool            `json:"allow_permanent,omitzero"`
	AllowSession     *bool            `json:"allow_session,omitzero"`
	SmartDenied      *bool            `json:"smart_denied,omitzero"`
	ToolName         *string          `json:"tool_name,omitzero"`
	GatewaySessionID *string          `json:"gateway_session_id,omitzero"`
	Extra            map[string]any   `json:"-" wire:"extra"`
}

// Verdict on an approval, all settling every approval the session holds
type ApprovalAnswer struct {
	Choice ApprovalChoice `json:"choice"`
	All    *bool          `json:"all,omitzero"`
}

// One question of a clarify request, its offered choices and whether several may be picked
type ClarifyQuestion struct {
	QID         string   `json:"qid"`
	Question    string   `json:"question"`
	Choices     []string `json:"choices,omitzero"`
	MultiSelect bool     `json:"multi_select,omitzero"`
}

// Questions the agent asks, the answers already locked riding along on a reconnect, null for a skipped one
type ClarifyRequestParams struct {
	SessionID string             `json:"session_id"`
	Questions []ClarifyQuestion  `json:"questions"`
	Answers   map[string]*string `json:"answers,omitzero"`
}

// How the viewer's own clarify panel ended
type ClarifyOutcome string

func (ClarifyOutcome) Values() []string { return []string{"submitted", "timed_out", "cancelled"} }

// Answers to a whole clarify request keyed by question id, none at all cancelling every question
type ClarifyAnswer struct {
	Answers map[string]*string `json:"answers,omitzero"`
	Outcome *ClarifyOutcome    `json:"outcome,omitzero"`
}

// Sudo password the terminal tool needs, for the command it runs
type SudoRequestParams struct {
	SessionID string `json:"session_id"`
	Command   string `json:"command,omitzero"`
}

// Value of a named environment variable a skill or setup flow needs
type SecretRequestParams struct {
	SessionID string         `json:"session_id"`
	EnvVar    string         `json:"env_var"`
	Prompt    string         `json:"prompt"`
	Metadata  map[string]any `json:"metadata,omitzero"`
}

// Master password of a password manager the session unlocks
type VaultUnlockRequestParams struct {
	SessionID   string `json:"session_id"`
	Backend     string `json:"backend"`
	DisplayName string `json:"display_name"`
}

// Login of a site the agent is about to fill, to keep in the vault
type VaultSaveLoginRequestParams struct {
	SessionID string `json:"session_id"`
	Origin    string `json:"origin"`
	Site      string `json:"site"`
}

// One time code the user reads from a device
type VaultCodeRequestParams struct {
	SessionID string  `json:"session_id"`
	Site      *string `json:"site,omitzero"`
	Hint      *string `json:"hint,omitzero"`
}

// Sudo password for the bot screen package install of a profile, asked of no session
type DisplayInstallSudoParams struct {
	SessionID  string `json:"session_id"`
	ProfileKey string `json:"profile_key"`
}

// Answer to a one value prompt, empty when skipped, a saved login as the JSON of identifier and password
type ValueAnswer struct {
	Value string `json:"value"`
}

// Verdict on the approval of a session on the method table route, by request id, all of them or the oldest
type ApprovalRespondParams struct {
	SessionID string          `json:"session_id"`
	Profile   *string         `json:"profile,omitzero"`
	Choice    *ApprovalChoice `json:"choice,omitzero"`
	All       *bool           `json:"all,omitzero"`
	RequestID *string         `json:"request_id,omitzero"`
}

// Approvals a verdict settled
type ApprovalRespondResult struct {
	Resolved int64 `json:"resolved"`
}

// Verdict on the approval prompt of a running generation
type CanonicalApprovalParams struct {
	Profile             *string        `json:"profile,omitzero"`
	SessionID           string         `json:"session_id"`
	ExecutionGeneration int64          `json:"execution_generation"`
	PromptID            string         `json:"prompt_id"`
	Choice              ApprovalChoice `json:"choice"`
}

// Answer to the clarify prompt of a running generation, empty when skipped
type CanonicalClarifyParams struct {
	SessionID           string `json:"session_id"`
	ExecutionGeneration int64  `json:"execution_generation"`
	PromptID            string `json:"prompt_id"`
	Answer              string `json:"answer"`
}

// Whether an answer settled its prompt or found it settled already
type PromptResponseStatus string

func (PromptResponseStatus) Values() []string { return []string{"resolved", "already_resolved"} }

// Outcome of an answer to a shared prompt
type PromptResponseResult struct {
	Status   PromptResponseStatus `json:"status"`
	PromptID string               `json:"prompt_id"`
}

// One answer of a batch clarify request, null skipping the question
type ClarifyLockParams struct {
	RequestID  string  `json:"request_id"`
	QuestionID string  `json:"question_id"`
	Answer     *string `json:"answer,omitzero"`
	Profile    *string `json:"profile,omitzero"`
}

// Whether the batch still waited for the lock
type ClarifyLockStatus string

func (ClarifyLockStatus) Values() []string { return []string{"ok", "expired"} }

// Questions of the batch still unanswered, the lock that empties it settling the request
type ClarifyLockResult struct {
	Status    ClarifyLockStatus `json:"status"`
	Remaining []string          `json:"remaining,omitzero"`
}

// Request the gateway withdrew, its method and why
type RequestCancelPayload struct {
	ID     string `json:"id"`
	Method string `json:"method"`
	Reason string `json:"reason"`
}

// Approvals of a session an interrupt, reap or teardown dropped, each settled as a deny the user never gave
type ApprovalCancelledPayload struct {
	SessionID       string   `json:"session_id"`
	StoredSessionID string   `json:"stored_session_id"`
	Reason          string   `json:"reason"`
	CancelledCount  int64    `json:"cancelled_count"`
	RequestIDs      []string `json:"request_ids"`
}

// Shared prompt a viewer or the owner settled
type PromptSettledPayload struct {
	PromptID            string `json:"prompt_id"`
	ExecutionGeneration int64  `json:"execution_generation"`
}

// A dangerous command awaits the user's verdict
var ApprovalRequest = NewRequest[ApprovalRequestParams, ApprovalAnswer]("approval")

// The clarify tool asks the user one to five questions
var ClarifyRequest = NewRequest[ClarifyRequestParams, ClarifyAnswer]("clarify")

// Sudo password for the terminal tool, never stored
var SudoRequest = NewRequest[SudoRequestParams, ValueAnswer]("sudo")

// Value of a named environment variable for a skill or setup flow
var SecretRequest = NewRequest[SecretRequestParams, ValueAnswer]("secret")

// Master password unlocking a password manager for the session
var VaultUnlockRequest = NewRequest[VaultUnlockRequestParams, ValueAnswer]("vault.unlock_prompt")

// Login to keep for a site the agent is about to fill
var VaultSaveLoginRequest = NewRequest[VaultSaveLoginRequestParams, ValueAnswer]("vault.save_login")

// One time or two factor code the user reads from a device
var VaultCodeRequest = NewRequest[VaultCodeRequestParams, ValueAnswer]("vault.code")

// Sudo password for the bot screen package install
var DisplayInstallSudoRequest = NewRequest[DisplayInstallSudoParams, ValueAnswer]("display.install.sudo")

// Verdict on a dangerous command on the method table route
var ApprovalRespond = NewMethod[ApprovalRespondParams, ApprovalRespondResult]("approval.respond")

// Locks one answer of a batch clarify request on the method table route, editable until every question is locked
var ClarifyLock = NewMethod[ClarifyLockParams, ClarifyLockResult]("clarify.lock")

// Generation fenced verdict on an approval prompt
var CanonicalApprovalRespond = NewCanonicalMethod[CanonicalApprovalParams, PromptResponseResult]("approval.respond")

// Generation fenced answer to a clarify prompt
var CanonicalClarifyRespond = NewCanonicalMethod[CanonicalClarifyParams, PromptResponseResult]("clarify.respond")

// Locks one answer of the owner's batch clarify request, editable until every question is locked
var CanonicalClarifyLock = NewCanonicalMethod[ClarifyLockParams, ClarifyLockResult]("clarify.lock")

// An open request was withdrawn, its card closes
var RequestCancelEvent = NewEvent[RequestCancelPayload]("request.cancel")

// Approvals of a session were dropped and settled as deny
var ApprovalCancelledEvent = NewEvent[ApprovalCancelledPayload]("approval.cancelled")

// The running generation asks the viewers to approve a command
var ApprovalRequestEvent = NewEvent[CanonicalPendingPrompt]("approval.request")

// The running generation asks the viewers a question
var ClarifyRequestEvent = NewEvent[CanonicalPendingPrompt]("clarify.request")

// An approval prompt was answered
var ApprovalSettledEvent = NewEvent[PromptSettledPayload]("approval.settled")

// A clarify prompt was answered
var ClarifySettledEvent = NewEvent[PromptSettledPayload]("clarify.settled")
