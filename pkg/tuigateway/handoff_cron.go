package tuigateway

import (
	"encoding/json"
	"errors"
)

type HandoffRequestParams struct {
	SessionID string  `json:"session_id"`
	Profile   *string `json:"profile,omitzero"`
	Platform  string  `json:"platform"`
}

// Handoff queued for the platform's home channel, the gateway watcher claiming it
type HandoffRequestResult struct {
	Queued     bool   `json:"queued"`
	SessionKey string `json:"session_key"`
	Platform   string `json:"platform"`
	HomeName   string `json:"home_name"`
}

// Handoff row of the session, its state, platform and error, empty texts when none is recorded
type HandoffStateResult struct {
	State    string `json:"state"`
	Platform string `json:"platform"`
	Error    string `json:"error"`
}

type HandoffFailParams struct {
	SessionID string  `json:"session_id"`
	Profile   *string `json:"profile,omitzero"`
	Error     string  `json:"error,omitzero"`
}

// Whether this call failed the handoff, and the state it is in
type HandoffFailResult struct {
	Failed bool   `json:"failed"`
	State  string `json:"state"`
}

// One cron firing, the job, the scheduler's request id and the prompt added to the job's own
type CronRunParams struct {
	JobID       string  `json:"job_id"`
	RequestID   string  `json:"request_id"`
	ExtraPrompt *string `json:"extra_prompt"`
}

type CronSubmitResult struct {
	SessionID   string `json:"session_id"`
	AdmissionID string `json:"admission_id"`
}

type CronAdmissionParams struct {
	SessionID   string `json:"session_id"`
	AdmissionID string `json:"admission_id"`
}

// What a cron run ended with, sent as the list success, output, final response, error
type CronOutcome struct {
	Success       bool
	Output        string
	FinalResponse string
	Error         *string
}

func (o *CronOutcome) UnmarshalJSON(data []byte) error {
	var parts []json.RawMessage
	if err := json.Unmarshal(data, &parts); err != nil || len(parts) != 4 {
		return errors.New("wants the list success, output, final response, error")
	}
	var out CronOutcome
	for i, target := range []any{&out.Success, &out.Output, &out.FinalResponse, &out.Error} {
		if err := json.Unmarshal(parts[i], target); err != nil {
			return errors.New("wants the list success, output, final response, error")
		}
	}
	*o = out
	return nil
}

func (o CronOutcome) MarshalJSON() ([]byte, error) {
	return json.Marshal([]any{o.Success, o.Output, o.FinalResponse, o.Error})
}

// Status of a cron admission, the outcome once terminal, job flags the run set, owner progress, and the job on recover
type CronStatusResult struct {
	Status      string         `json:"status"`
	Result      *CronOutcome   `json:"result"`
	JobFlags    map[string]any `json:"job_flags,omitzero"`
	ProgressAt  *float64       `json:"progress_at,omitzero"`
	Job         map[string]any `json:"job,omitzero"`
	SessionID   string         `json:"session_id,omitzero"`
	AdmissionID string         `json:"admission_id,omitzero"`
}

type CronCancelResult struct {
	Ok bool `json:"ok"`
}

// The dispatcher's current claim on a kanban task
type KanbanRunParams struct {
	Board     string `json:"board"`
	TaskID    string `json:"task_id"`
	RunID     int64  `json:"run_id"`
	ClaimLock string `json:"claim_lock"`
	DB        string `json:"db,omitzero"`
}

type KanbanRunResult struct {
	SessionID string           `json:"session_id"`
	Receipt   AdmissionReceipt `json:"receipt"`
}

// Queues a handoff of the session to a messaging platform's home channel
var HandoffRequest = NewMethod[HandoffRequestParams, HandoffRequestResult]("handoff.request")

// Reads the handoff row of the session
var HandoffState = NewMethod[SessionIDParams, HandoffStateResult]("handoff.state")

// Fails a handoff the watcher has not claimed yet
var HandoffFail = NewMethod[HandoffFailParams, HandoffFailResult]("handoff.fail")

// Admits one cron firing into the owning profile's durable queue
var CronSubmit = NewCanonicalMethod[CronRunParams, CronSubmitResult]("cron.submit")

// Observes a firing again after a scheduler restart, by the same request identity
var CronRecover = NewCanonicalMethod[CronRunParams, CronStatusResult]("cron.recover")

// Status and, once terminal, the outcome of one cron admission
var CronStatus = NewCanonicalMethod[CronAdmissionParams, CronStatusResult]("cron.status")

// Cancels a queued cron admission or latches cancellation on a started one
var CronCancel = NewCanonicalMethod[CronAdmissionParams, CronCancelResult]("cron.cancel")

// Admits the dispatcher's claim on a kanban task, native owner only
var KanbanRun = NewCanonicalMethod[KanbanRunParams, KanbanRunResult]("kanban.run")
