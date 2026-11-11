package gateway

import "context"

// Subprotocol of the authenticated local WebSocket family, apart from the legacy Desktop API contract
const CanonicalGatewayProtocol = "hermes-gateway-v1"

// Execution states a session handle reports
const (
	ExecutionIdle     = "idle"
	ExecutionRunning  = "running"
	ExecutionWaiting  = "waiting"
	ExecutionUnknown  = "unknown"
	ExecutionTerminal = "terminal"
)

// Submission intents a client sends with a prompt
const (
	IntentQueue    = "queue"
	IntentSteer    = "steer"
	IntentRedirect = "redirect"
)

// Admission receipt statuses an owner reports
const (
	AdmissionQueued   = "queued"
	AdmissionStarted  = "started"
	AdmissionUnknown  = "unknown"
	AdmissionTerminal = "terminal"
)

// Authenticated caller with its profile, capability set and transport
type Principal struct {
	Subject      string
	ProfileID    string
	Capabilities map[string]bool
	TransportID  string
}

// Profile scoped session identity
type SessionRef struct {
	ProfileID string
	SessionID string
}

// Owner view of one session with its epoch, revision and execution generation
type SessionHandle struct {
	Ref                 SessionRef
	InstanceID          string
	AuthorityEpoch      int64
	Revision            int64
	ExecutionGeneration int64
	ExecutionState      string
}

// Session creation request with its launch policy fields
type SessionCreate struct {
	RequestID string
	Cwd       string
	Source    string
	Policy    map[string]any
}

// Prompt submission with its queue, steer or redirect intent
type Submission struct {
	RequestID string
	Ref       SessionRef
	Payload   map[string]any
	Intent    string
}

// Durable admission state of one submitted input, nil outcome and generation when absent
type AdmissionReceipt struct {
	AdmissionID         string
	Ref                 SessionRef
	Sequence            int64
	Status              string
	Outcome             *string
	AuthorityEpoch      int64
	ExecutionGeneration *int64
}

// Queued admission with its input text, the queued row verbs served and its steering transfer flag
type PendingAdmission struct {
	AdmissionReceipt
	InputID  string
	Text     string
	Controls []string
	Steering bool
}

// Attach snapshot with history, pending admissions, open prompts and the resolved transcript target
type SubscriptionSnapshot struct {
	SubscriptionID   string
	Handle           SessionHandle
	ReplayEpoch      string
	LastSequence     int64
	History          []map[string]any
	Pending          []PendingAdmission
	Prompts          []map[string]any
	TranscriptTarget *string
}

// Replay epoch and sequence an attach resumes from
type ReplayCursor struct {
	Epoch    string
	Sequence int64
}

// Public session authority surface, live execution state never crossing it
type SessionAuthorityProtocol interface {
	Create(ctx context.Context, actor Principal, request SessionCreate) (SessionHandle, error)
	Resolve(ctx context.Context, actor Principal, ref SessionRef) (SessionHandle, error)
	Attach(ctx context.Context, actor Principal, ref SessionRef, history bool, replayFrom *ReplayCursor) (SubscriptionSnapshot, error)
	Detach(ctx context.Context, actor Principal, subscriptionID string, endReason *string) error
	Submit(ctx context.Context, actor Principal, request Submission) (AdmissionReceipt, error)
	Receipt(ctx context.Context, actor Principal, ref SessionRef, admissionID string) (AdmissionReceipt, error)
	CancelQueued(ctx context.Context, actor Principal, ref SessionRef, admissionID string) (AdmissionReceipt, error)
	Interrupt(ctx context.Context, actor Principal, ref SessionRef, generation int64) (SessionHandle, error)
	Respond(ctx context.Context, actor Principal, ref SessionRef, generation int64, promptID string, response map[string]any) (map[string]any, error)
	Mutate(ctx context.Context, actor Principal, ref SessionRef, revision int64, requestID, action string, arguments map[string]any) (SessionHandle, error)
}
