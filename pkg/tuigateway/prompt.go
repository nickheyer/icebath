package tuigateway

import "reflect"

// How the method table route took a prompt, streamed now or held behind the running turn
type PromptSubmitStatus string

func (PromptSubmitStatus) Values() []string {
	return []string{"streaming", "queued", "steered", "redirected"}
}

// Prompt on the method table route, a rewind needing confirm_truncate and one truncation target
type PromptSubmitParams struct {
	SessionID                 string  `json:"session_id"`
	Profile                   *string `json:"profile,omitzero"`
	Text                      string  `json:"text,omitzero"`
	DisplayKind               *string `json:"display_kind,omitzero"`
	Interrupted               *bool   `json:"interrupted,omitzero"`
	Queued                    *bool   `json:"queued,omitzero"`
	Surface                   *string `json:"surface,omitzero"`
	VoiceContext              *string `json:"voice_context,omitzero"`
	VoiceTurn                 *bool   `json:"voice_turn,omitzero"`
	TitlePreview              *string `json:"title_preview,omitzero"`
	TruncateBeforeUserOrdinal *int64  `json:"truncate_before_user_ordinal,omitzero"`
	TruncateBeforeRowID       *int64  `json:"truncate_before_row_id,omitzero"`
	TruncateBeforeMessageID   *string `json:"truncate_before_message_id,omitzero"`
	ConfirmTruncate           *bool   `json:"confirm_truncate,omitzero"`
	ConfirmEmptyTruncate      *bool   `json:"confirm_empty_truncate,omitzero"`
	ConfirmDeepTruncate       *bool   `json:"confirm_deep_truncate,omitzero"`
	RebindSurvivorRowIDs      []int64 `json:"rebind_survivor_row_ids,omitzero"`
}

// Answer of the method table prompt.submit, status absent only when a typed stop phrase ended the voice chat
type PromptSubmitResult struct {
	Status             *PromptSubmitStatus `json:"status,omitzero"`
	VoiceStopped       *bool               `json:"voice_stopped,omitzero"`
	UserRowID          *int64              `json:"user_row_id,omitzero"`
	SurvivorUserRowIDs []*int64            `json:"survivor_user_row_ids,omitzero"`
	SurvivorRowIDMap   map[string]*int64   `json:"survivor_row_id_map,omitzero"`
	AdmissionID        *string             `json:"admission_id,omitzero"`
	Forwarded          *bool               `json:"forwarded,omitzero"`
}

// Task of a side agent on a session
type SideAgentParams struct {
	SessionID string  `json:"session_id"`
	Profile   *string `json:"profile,omitzero"`
	Text      string  `json:"text"`
}

// Id the side agent's completion event carries
type TaskIDResult struct {
	TaskID string `json:"task_id"`
}

// Session whose running turn stops, optionally only while a given hosted task runs it
type SessionInterruptParams struct {
	SessionID            string  `json:"session_id"`
	Profile              *string `json:"profile,omitzero"`
	ExpectedHostedTaskID *string `json:"expected_hosted_task_id,omitzero"`
}

// Whether a turn was running to stop
type InterruptStatus string

func (InterruptStatus) Values() []string { return []string{"interrupted", "not_interrupted"} }

// Outcome of a method table interrupt
type SessionInterruptResult struct {
	Status      InterruptStatus `json:"status"`
	Interrupted *bool           `json:"interrupted,omitzero"`
}

// Correction text for the running turn on the method table route
type SessionCorrectionParams struct {
	SessionID string  `json:"session_id"`
	Profile   *string `json:"profile,omitzero"`
	Text      string  `json:"text"`
}

// Verdict on a correction, delivered to the running turn, held for the next one or refused
type CorrectionStatus string

func (CorrectionStatus) Values() []string { return []string{"queued", "redirected", "rejected"} }

// Verdict of a method table correction with the text it was given
type SessionCorrectionResult struct {
	Status CorrectionStatus `json:"status"`
	Text   string           `json:"text"`
}

// Image a canonical prompt carries, a path the gateway reads and its media type
type CanonicalAttachment struct {
	Path string `json:"path"`
	Mime string `json:"mime"`
}

// Author a bot or room relay names for a turn
type CanonicalTurnAuthor struct {
	ID     *string `json:"id,omitzero"`
	Name   *string `json:"name,omitzero"`
	IsBot  bool    `json:"is_bot,omitzero"`
	Origin *string `json:"origin,omitzero"`
}

// Model one turn runs on instead of the session's own
type CanonicalModelOverride struct {
	Model     string  `json:"model"`
	Provider  *string `json:"provider,omitzero"`
	Reasoning *string `json:"reasoning,omitzero"`
}

// Display kind a canonical prompt may ask for, hidden keeping the bubble out of the transcript
type HiddenDisplayKind string

func (HiddenDisplayKind) Values() []string { return []string{"hidden"} }

// Prompt admitted into the owner's durable queue, input_id naming the client's own submission
type CanonicalSubmitParams struct {
	Profile       *string                 `json:"profile,omitzero"`
	SessionID     string                  `json:"session_id"`
	Text          string                  `json:"text"`
	SubmissionID  *string                 `json:"submission_id,omitzero"`
	InputID       *string                 `json:"input_id,omitzero"`
	Queued        bool                    `json:"queued,omitzero"`
	Attachments   []CanonicalAttachment   `json:"attachments,omitzero"`
	Finite        bool                    `json:"finite,omitzero"`
	Unattended    bool                    `json:"unattended,omitzero"`
	Surface       *string                 `json:"surface,omitzero"`
	VoiceContext  *string                 `json:"voice_context,omitzero"`
	Interrupted   bool                    `json:"interrupted,omitzero"`
	VoiceTurn     bool                    `json:"voice_turn,omitzero"`
	DisplayKind   *HiddenDisplayKind      `json:"display_kind,omitzero"`
	TitlePreview  *string                 `json:"title_preview,omitzero"`
	TurnAuthor    *CanonicalTurnAuthor    `json:"turn_author,omitzero"`
	APIText       *string                 `json:"api_text,omitzero"`
	ModelOverride *CanonicalModelOverride `json:"model_override,omitzero"`
	ClarifyBatch  bool                    `json:"clarify_batch,omitzero"`
}

// Receipt of one admission in the owner's durable queue
type AdmissionReceipt struct {
	AdmissionID         string                          `json:"admission_id"`
	Ref                 CanonicalSessionRef             `json:"ref"`
	Sequence            int64                           `json:"sequence"`
	Status              CanonicalPendingAdmissionStatus `json:"status"`
	Outcome             *string                         `json:"outcome"`
	AuthorityEpoch      int64                           `json:"authority_epoch"`
	ExecutionGeneration *int64                          `json:"execution_generation"`
}

// Answer of a typed stop phrase that ended the voice chat with nothing admitted
type VoiceStopped struct {
	VoiceStopped bool `json:"voice_stopped"`
}

// Answer of a canonical prompt.submit, the admission receipt or voice_stopped alone
type SubmitAnswer struct {
	*AdmissionReceipt
	VoiceStopped bool `json:"voice_stopped,omitzero"`
}

func (SubmitAnswer) Variants() []reflect.Type {
	return []reflect.Type{typeOf[AdmissionReceipt](), typeOf[VoiceStopped]()}
}

// Running generation a canonical control is fenced to
type CanonicalControlParams struct {
	Profile             *string `json:"profile,omitzero"`
	SessionID           string  `json:"session_id"`
	ExecutionGeneration int64   `json:"execution_generation"`
}

// Correction text for the running generation of a canonical session
type CanonicalCorrectionParams struct {
	Profile             *string `json:"profile,omitzero"`
	SessionID           string  `json:"session_id"`
	ExecutionGeneration int64   `json:"execution_generation"`
	Text                string  `json:"text"`
}

// Verdict of a canonical correction, delivered false marking a refusal the turn never saw
type CorrectionAck struct {
	Status              CorrectionStatus `json:"status"`
	Text                string           `json:"text"`
	ExecutionGeneration int64            `json:"execution_generation"`
	AuthorityEpoch      int64            `json:"authority_epoch"`
	Delivered           *bool            `json:"delivered,omitzero"`
}

// Answer of a canonical steer or redirect, the verdict or voice_stopped alone
type CorrectionAnswer struct {
	*CorrectionAck
	VoiceStopped bool `json:"voice_stopped,omitzero"`
}

func (CorrectionAnswer) Variants() []reflect.Type {
	return []reflect.Type{typeOf[CorrectionAck](), typeOf[VoiceStopped]()}
}

// State the execution of a canonical session is in
type ExecutionState string

func (ExecutionState) Values() []string {
	return []string{"idle", "running", "waiting", "unknown", "terminal"}
}

// Fences of a canonical session after a control, its revision, generation and execution state
type CanonicalSessionHandle struct {
	Ref                 CanonicalSessionRef `json:"ref"`
	InstanceID          string              `json:"instance_id"`
	AuthorityEpoch      int64               `json:"authority_epoch"`
	Revision            int64               `json:"revision"`
	ExecutionGeneration int64               `json:"execution_generation"`
	ExecutionState      ExecutionState      `json:"execution_state"`
}

// Queued admission replaced by an edited prompt that goes to the back of the queue under a new submission id
type CanonicalReplaceQueuedParams struct {
	Profile      *string               `json:"profile,omitzero"`
	SessionID    string                `json:"session_id"`
	AdmissionID  string                `json:"admission_id"`
	SubmissionID string                `json:"submission_id"`
	Text         string                `json:"text"`
	Attachments  []CanonicalAttachment `json:"attachments,omitzero"`
	DisplayKind  *HiddenDisplayKind    `json:"display_kind,omitzero"`
	TitlePreview *string               `json:"title_preview,omitzero"`
}

// One admission of a session, with the committed result of a terminal one when asked
type PromptReceiptParams struct {
	SessionID     string `json:"session_id"`
	AdmissionID   string `json:"admission_id"`
	IncludeResult *bool  `json:"include_result,omitzero"`
}

// Receipt of an admission, a terminal one asked with include_result carrying its structured result and usage
type PromptReceiptResult struct {
	AdmissionReceipt
	Result any            `json:"result,omitzero"`
	Usage  map[string]any `json:"usage,omitzero"`
}

// Admission of a session a client withdraws
type PromptCancelParams struct {
	SessionID   string `json:"session_id"`
	AdmissionID string `json:"admission_id"`
}

// Admission lost across an owner restart and the generation it ran in
type PromptResolveUnknownParams struct {
	SessionID           string `json:"session_id"`
	AdmissionID         string `json:"admission_id"`
	ExecutionGeneration int64  `json:"execution_generation"`
}

// Display hints of a transcript row or a pending prompt, the card text and the count of agents a batch finished
type DisplayMetadata struct {
	DisplayText *string        `json:"display_text,omitzero"`
	TaskCount   *int64         `json:"task_count,omitzero"`
	Extra       map[string]any `json:"-" wire:"extra"`
}

// Failure of a session outside a turn, agent start, model switch, compression or resume
type ErrorPayload struct {
	Message string `json:"message"`
}

// Start of a turn, an automation turn carrying the card and model text a viewer paints
type MessageStartPayload struct {
	DisplayKind          *string `json:"display_kind,omitzero"`
	DisplayText          *string `json:"display_text,omitzero"`
	NotificationCategory *string `json:"notification_category,omitzero"`
	Text                 *string `json:"text,omitzero"`
	TurnID               *string `json:"turn_id,omitzero"`
}

// Streamed reply chunk, null text ending a segment and the turn stamps letting a client drop a resent chunk
type MessageDeltaPayload struct {
	Text     *string `json:"text"`
	Rendered *string `json:"rendered,omitzero"`
	Verbose  *bool   `json:"verbose,omitzero"`
	TurnID   *string `json:"turn_id,omitzero"`
	Segment  *int64  `json:"segment,omitzero"`
	Offset   *int64  `json:"offset,omitzero"`
}

// Reasoning or thinking text, verbose when the session shows verbose reasoning
type StreamDeltaPayload struct {
	Text     string  `json:"text"`
	Rendered *string `json:"rendered,omitzero"`
	Verbose  *bool   `json:"verbose,omitzero"`
}

// Commentary beside tool calls sealed as its own segment
type MessageInterimPayload struct {
	Text            string  `json:"text"`
	AlreadyStreamed bool    `json:"already_streamed"`
	TurnID          *string `json:"turn_id,omitzero"`
	Segment         *int64  `json:"segment,omitzero"`
}

// How a turn ended
type TurnStatus string

func (TurnStatus) Values() []string { return []string{"complete", "error", "interrupted"} }

// Provider out of credits or payment block with the page that settles it
type BillingBlock struct {
	Provider      string  `json:"provider"`
	ProviderLabel string  `json:"provider_label"`
	Model         string  `json:"model"`
	BillingURL    *string `json:"billing_url"`
	Message       string  `json:"message"`
	Unverified    *bool   `json:"unverified,omitzero"`
}

// Advisory layer, code and retryability of a turn failure, with the provider and when its limit lifts
type ErrorSurface struct {
	Layer     string         `json:"layer"`
	Code      string         `json:"code"`
	Retryable bool           `json:"retryable"`
	Provider  *string        `json:"provider,omitzero"`
	Model     *string        `json:"model,omitzero"`
	ResetsAt  *float64       `json:"resets_at,omitzero"`
	Extra     map[string]any `json:"-" wire:"extra"`
}

// Stored row ids of a turn, complete only when every row and the final body are accounted for
type PersistedTurn struct {
	RowIDs              []int64 `json:"row_ids"`
	Complete            bool    `json:"complete"`
	UserRowID           *int64  `json:"user_row_id,omitzero"`
	FinalAssistantRowID *int64  `json:"final_assistant_row_id,omitzero"`
	UserRowIDs          []int64 `json:"user_row_ids,omitzero"`
	SubmissionID        *string `json:"submission_id,omitzero"`
}

// End of a turn, its final text, usage and outcome, a canonical turn naming its admission
type MessageCompletePayload struct {
	Text                *string        `json:"text,omitzero"`
	Content             *string        `json:"content,omitzero"`
	AdmissionID         *string        `json:"admission_id,omitzero"`
	Outcome             *string        `json:"outcome,omitzero"`
	Usage               *Usage         `json:"usage,omitzero"`
	Status              *TurnStatus    `json:"status,omitzero"`
	Reasoning           *string        `json:"reasoning,omitzero"`
	Warning             *string        `json:"warning,omitzero"`
	ResponsePreviewed   *bool          `json:"response_previewed,omitzero"`
	ResponseReused      *bool          `json:"response_reused,omitzero"`
	ResponseTransformed *bool          `json:"response_transformed,omitzero"`
	Billing             *BillingBlock  `json:"billing,omitzero"`
	FailureReason       *string        `json:"failure_reason,omitzero"`
	Rendered            *string        `json:"rendered,omitzero"`
	Error               *string        `json:"error,omitzero"`
	Recoverable         *bool          `json:"recoverable,omitzero"`
	ErrorSurface        *ErrorSurface  `json:"error_surface,omitzero"`
	Partial             *bool          `json:"partial,omitzero"`
	PersistedTurn       *PersistedTurn `json:"persisted_turn,omitzero"`
}

// Transient status line and its kind, status, lifecycle, compacting, goal, loop, heartbeat or process
type StatusUpdatePayload struct {
	Kind string `json:"kind"`
	Text string `json:"text"`
}

// Affection the user's message showed
type ReactionPayload struct {
	Kind string `json:"kind"`
}

// Line of text an event carries alone, a review summary or a line the agent printed
type TextPayload struct {
	Text string `json:"text"`
}

// Answer of a side agent, a side question naming the question it answered
type SideAgentCompletePayload struct {
	TaskID   string  `json:"task_id"`
	Text     string  `json:"text"`
	Question *string `json:"question,omitzero"`
}

// Output of one mixture of agents reference model, its place among the references
type MoaReferencePayload struct {
	Label string `json:"label"`
	Text  string `json:"text"`
	Index *int64 `json:"index,omitzero"`
	Count *int64 `json:"count,omitzero"`
}

// Mixture of agents aggregator that started
type MoaAggregatingPayload struct {
	Aggregator string `json:"aggregator"`
}

// References a mixture of agents turn has heard back from
type MoaProgressPayload struct {
	Label     string `json:"label"`
	RefsDone  int64  `json:"refs_done"`
	RefsTotal int64  `json:"refs_total"`
}

// Phase a mixture of agents turn moved to, aggregator once the references are in
type MoaPhasePayload struct {
	Phase      string  `json:"phase"`
	RefsDone   *int64  `json:"refs_done,omitzero"`
	RefsTotal  *int64  `json:"refs_total,omitzero"`
	Aggregator *string `json:"aggregator,omitzero"`
}

// Refusal the gateway gives params that lack a field the method requires
func MissingParam(method, field string) error {
	return &ParamsRefusal{Method: method, Fit: &FitError{Field: field, Problem: problemRequired}}
}

// Refusal the gateway gives params carrying a field the method does not declare
func UnknownParam(method, field string) error {
	return &ParamsRefusal{Method: method, Fit: &FitError{Field: field, Problem: problemUnknown}}
}

// Sends a prompt on the method table route, a busy session queueing, steering or redirecting it
var PromptSubmit = NewMethod[PromptSubmitParams, PromptSubmitResult]("prompt.submit")

// Runs a task on a fresh agent in the background, its answer arriving as background.complete
var PromptBackground = NewMethod[SideAgentParams, TaskIDResult]("prompt.background")

// Asks a side question over the live conversation, its answer arriving as btw.complete
var PromptBtw = NewMethod[SideAgentParams, TaskIDResult]("prompt.btw")

// Stops the running turn and its speech on the method table route
var SessionInterrupt = NewMethod[SessionInterruptParams, SessionInterruptResult]("session.interrupt")

// Puts text into the next tool result of the running turn on the method table route
var SessionSteer = NewMethod[SessionCorrectionParams, SessionCorrectionResult]("session.steer")

// Redirects the running turn on the method table route, held for the next turn while the agent builds
var SessionRedirect = NewMethod[SessionCorrectionParams, SessionCorrectionResult]("session.redirect")

// Admits a prompt into the owner's durable queue
var CanonicalPromptSubmit = NewCanonicalMethod[CanonicalSubmitParams, SubmitAnswer]("prompt.submit")

// Stops the running generation of a canonical session
var CanonicalSessionInterrupt = NewCanonicalMethod[CanonicalControlParams, CanonicalSessionHandle]("session.interrupt")

// Puts text into the next tool result of the running generation
var CanonicalSessionSteer = NewCanonicalMethod[CanonicalCorrectionParams, CorrectionAnswer]("session.steer")

// Redirects the running generation, held for the next turn while the agent builds
var CanonicalSessionRedirect = NewCanonicalMethod[CanonicalCorrectionParams, CorrectionAnswer]("session.redirect")

// Replaces a queued admission with an edited prompt at the back of the queue in one write
var CanonicalPromptReplaceQueued = NewCanonicalMethod[CanonicalReplaceQueuedParams, AdmissionReceipt]("prompt.replace_queued")

// Current receipt of one admission the caller submitted or controls
var CanonicalPromptReceipt = NewCanonicalMethod[PromptReceiptParams, PromptReceiptResult]("prompt.receipt")

// Withdraws a queued admission, interrupting a started one in the same call
var CanonicalPromptCancel = NewCanonicalMethod[PromptCancelParams, AdmissionReceipt]("prompt.cancel")

// Acknowledges a turn lost across an owner restart so the queue behind it resumes, never requeueing it
var CanonicalPromptResolveUnknown = NewCanonicalMethod[PromptResolveUnknownParams, AdmissionReceipt]("prompt.resolve_unknown")

// Session failure outside a turn
var ErrorEvent = NewEvent[ErrorPayload]("error")

// A turn began streaming
var MessageStartEvent = NewEvent[MessageStartPayload]("message.start")

// One streamed chunk of the reply
var MessageDeltaEvent = NewEvent[MessageDeltaPayload]("message.delta")

// Commentary beside tool calls sealed as its own segment
var MessageInterimEvent = NewEvent[MessageInterimPayload]("message.interim")

// The turn ended
var MessageCompleteEvent = NewEvent[MessageCompletePayload]("message.complete")

// One streamed chunk of the model's reasoning
var ReasoningDeltaEvent = NewEvent[StreamDeltaPayload]("reasoning.delta")

// A whole reasoning block of a provider that does not stream it
var ReasoningAvailableEvent = NewEvent[StreamDeltaPayload]("reasoning.available")

// Thinking text of the agent's thinking callback
var ThinkingDeltaEvent = NewEvent[StreamDeltaPayload]("thinking.delta")

// Transient status line
var StatusUpdateEvent = NewEvent[StatusUpdatePayload]("status.update")

// Affection detected in the user's message
var ReactionEvent = NewEvent[ReactionPayload]("reaction")

// Background review of the last turn finished
var ReviewSummaryEvent = NewEvent[TextPayload]("review.summary")

// Status line the agent printed during a local viewer's turn
var AgentPrintEvent = NewEvent[TextPayload]("agent.print")

// A background side agent finished
var BackgroundCompleteEvent = NewEvent[SideAgentCompletePayload]("background.complete")

// A side question was answered
var BtwCompleteEvent = NewEvent[SideAgentCompletePayload]("btw.complete")

// One mixture of agents reference answered
var MoaReferenceEvent = NewEvent[MoaReferencePayload]("moa.reference")

// The mixture of agents aggregator started
var MoaAggregatingEvent = NewEvent[MoaAggregatingPayload]("moa.aggregating")

// Mixture of agents references answered so far
var MoaProgressEvent = NewEvent[MoaProgressPayload]("moa.progress")

// A mixture of agents turn changed phase
var MoaPhaseEvent = NewEvent[MoaPhasePayload]("moa.phase")
