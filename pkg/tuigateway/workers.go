package tuigateway

import "errors"

// Producer claim every worker verb proves, the execution, its process and the adoption secret
type WorkerScopeParams struct {
	ProfileID   string  `json:"profile_id"`
	SessionID   string  `json:"session_id"`
	ExecutionID string  `json:"execution_id"`
	Generation  int64   `json:"generation"`
	Pid         int64   `json:"pid"`
	Birth       float64 `json:"birth"`
	Secret      string  `json:"secret"`
}

// Claim of a compute worker registering its execution
type WorkerRegisterParams struct {
	WorkerScopeParams
	Kind string `json:"kind"`
}

type WorkerKind string

func (WorkerKind) Values() []string { return []string{"cron", "child", "compute", "kanban"} }

type WorkerStatus string

func (WorkerStatus) Values() []string {
	return []string{"registered", "running", "unknown", "terminal"}
}

// One worker execution row without its adoption digest
type WorkerExecution struct {
	ExecutionID  string       `json:"execution_id"`
	SessionID    string       `json:"session_id"`
	Kind         WorkerKind   `json:"kind"`
	OwnerEpoch   int64        `json:"owner_epoch"`
	Generation   int64        `json:"generation"`
	Status       WorkerStatus `json:"status"`
	LastSequence int64        `json:"last_sequence"`
}

// Sequence fenced typed write of an adopted worker
type WorkerPersistParams struct {
	WorkerScopeParams
	Epoch     int64          `json:"epoch"`
	Sequence  int64          `json:"sequence"`
	Operation string         `json:"operation"`
	Payload   map[string]any `json:"payload"`
}

// Identity tuple of one A2A input and its text
type A2AForwardParams struct {
	Agent     string `json:"agent"`
	Tenant    string `json:"tenant"`
	Peer      string `json:"peer"`
	ContextID string `json:"context_id"`
	InputID   string `json:"input_id"`
	Text      string `json:"text"`
}

// Admission of a forwarded input, the conversation it landed in and the saved result of a finished one
type A2AForwardResult struct {
	AdmissionReceipt
	SessionID string `json:"session_id"`
	Result    any    `json:"result"`
}

// One message a bot relay hands to a profile, its id the delivery key
type BotDeliverParams struct {
	Profile              *string        `json:"profile,omitzero"`
	Message              string         `json:"message"`
	ID                   string         `json:"id"`
	SessionID            *string        `json:"session_id,omitzero"`
	Author               map[string]any `json:"author,omitzero"`
	NotificationCategory *string        `json:"notification_category,omitzero"`
}

// Receipt of a delivery, naming its admission unless the queue refused it before anything was committed
type BotDeliverResult struct {
	Status           string  `json:"status"`
	DeliveryID       string  `json:"delivery_id"`
	ProfileHome      string  `json:"profile_home"`
	SessionID        string  `json:"session_id"`
	AdmissionID      *string `json:"admission_id,omitzero"`
	Refused          *bool   `json:"refused,omitzero"`
	Message          string  `json:"message"`
	Reply            *string `json:"reply,omitzero"`
	RetryAdmissionID *string `json:"retry_admission_id,omitzero"`
	Error            *string `json:"error,omitzero"`
	Reason           *string `json:"reason,omitzero"`
}

// Fields of a receipt without the admission rule
type botDeliverFields BotDeliverResult

// Refuses a receipt that is neither admitted nor a refused failed receipt without an admission
func (r *BotDeliverResult) UnmarshalJSON(data []byte) error {
	var fields botDeliverFields
	if err := Strict(data, &fields); err != nil {
		return err
	}
	refused := fields.Refused != nil && *fields.Refused
	if fields.Refused != nil && !refused {
		return errors.New("refused is true or absent")
	}
	if (fields.AdmissionID == nil) != refused || refused && fields.Status != "failed" {
		return errors.New("a deliver result names its admission, or is a refused failed receipt without one")
	}
	*r = BotDeliverResult(fields)
	return nil
}

type ConnectorErrorReason string

func (ConnectorErrorReason) Values() []string {
	return []string{"invalid_params", "not_owner", "connector_request_failed", "unknown_target", "link_still_valid", "reissue_refused",
		"unknown_operation", "invalid_answer", "connectors_unavailable"}
}

// Registers a compute worker execution on an idle session
var WorkerRegister = NewCanonicalMethod[WorkerRegisterParams, WorkerExecution]("worker.register")

// Adopts a registered execution after the live producer claim is verified
var WorkerAdopt = NewCanonicalMethod[WorkerScopeParams, WorkerExecution]("worker.adopt")

// Durable write of an adopted worker answered with the operation's receipt
var WorkerPersist = NewCanonicalMethod[WorkerPersistParams, map[string]any]("worker.persist")

// Forwards one A2A input into the conversation its identity tuple names
var A2AForward = NewCanonicalMethod[A2AForwardParams, A2AForwardResult]("a2a.forward")

// Hands one bot message to a profile's conversation
var BotRelayDeliver = NewCanonicalMethod[BotDeliverParams, BotDeliverResult]("bot_relay.deliver")

// Settles a delivery the sender was unsure of
var BotRelaySettle = NewCanonicalMethod[BotDeliverParams, BotDeliverResult]("bot_relay.settle")
