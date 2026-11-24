package acputil

import (
	"encoding/json"
	"fmt"

	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Params of session/request_permission: the tool call and the options the user chooses from
type RequestPermissionRequest struct {
	Meta      *jsonutil.Object   `json:"_meta,omitempty"`
	Options   []PermissionOption `json:"options"`
	SessionID string             `json:"sessionId"`
	ToolCall  ToolCallUpdate     `json:"toolCall"`
}

// Fills the request from its JSON object, which must carry options, sessionId and toolCall
func (r *RequestPermissionRequest) UnmarshalJSON(data []byte) error {
	type plain RequestPermissionRequest
	return decodeObject(data, (*plain)(r), "RequestPermissionRequest", "options", "sessionId", "toolCall")
}

// Result of session/request_permission
type RequestPermissionResponse struct {
	Meta    *jsonutil.Object  `json:"_meta,omitempty"`
	Outcome PermissionOutcome `json:"outcome"`
}

// Fills the response from its JSON object, which must carry outcome
func (r *RequestPermissionResponse) UnmarshalJSON(data []byte) error {
	type plain RequestPermissionResponse
	var w struct {
		*plain
		Outcome json.RawMessage `json:"outcome"`
	}
	w.plain = (*plain)(r)
	if err := decodeObject(data, &w, "RequestPermissionResponse", "outcome"); err != nil {
		return err
	}
	outcome, err := decodePermissionOutcome(w.Outcome)
	if err != nil {
		return fmt.Errorf("outcome: %w", err)
	}
	r.Outcome = outcome
	return nil
}

// One choice of a permission request
type PermissionOption struct {
	Meta *jsonutil.Object `json:"_meta,omitempty"`
	// allow_once, allow_always, reject_once or reject_always
	Kind     string `json:"kind"`
	Name     string `json:"name"`
	OptionID string `json:"optionId"`
}

// Fills the option from its JSON object, which must carry kind, name and optionId
func (o *PermissionOption) UnmarshalJSON(data []byte) error {
	type plain PermissionOption
	return decodeObject(data, (*plain)(o), "PermissionOption", "kind", "name", "optionId")
}

// How a permission request ended: *AllowedOutcome or *DeniedOutcome
type PermissionOutcome interface{ isPermissionOutcome() }

func (*AllowedOutcome) isPermissionOutcome() {}
func (*DeniedOutcome) isPermissionOutcome()  {}

// Outcome naming the option the user selected
type AllowedOutcome struct {
	Meta     *jsonutil.Object `json:"_meta,omitempty"`
	OptionID string           `json:"optionId"`
}

// JSON object of the outcome with outcome "selected"
func (o AllowedOutcome) MarshalJSON() ([]byte, error) {
	type plain AllowedOutcome
	return withTag(plain(o), "outcome", "selected")
}

// Fills the outcome from its JSON object, which must carry optionId
func (o *AllowedOutcome) UnmarshalJSON(data []byte) error {
	type plain AllowedOutcome
	return decodeObject(data, (*plain)(o), "AllowedOutcome", "optionId")
}

// Outcome of a request the cancellation of the prompt turn ended
type DeniedOutcome struct{}

// JSON object of the outcome with outcome "cancelled"
func (o DeniedOutcome) MarshalJSON() ([]byte, error) {
	return []byte(`{"outcome":"cancelled"}`), nil
}

// Permission outcome of a JSON object, the variant chosen by its outcome
func decodePermissionOutcome(data []byte) (PermissionOutcome, error) {
	kind, ok := tagOf(data, "outcome")
	if !ok {
		return nil, &MissingFieldError{Type: "PermissionOutcome", Field: "outcome"}
	}
	var outcome PermissionOutcome
	switch kind {
	case "selected":
		outcome = new(AllowedOutcome)
	case "cancelled":
		outcome = new(DeniedOutcome)
	default:
		return nil, &UnknownVariantError{Union: "PermissionOutcome", Key: "outcome", Value: kind}
	}
	if err := json.Unmarshal(data, outcome); err != nil {
		return nil, err
	}
	return outcome, nil
}
