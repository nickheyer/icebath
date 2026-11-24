package acputil

import (
	"encoding/json"

	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Config option of a session: *SessionConfigOptionSelect or *SessionConfigOptionBoolean
type SessionConfigOption interface{ isSessionConfigOption() }

func (*SessionConfigOptionSelect) isSessionConfigOption()  {}
func (*SessionConfigOptionBoolean) isSessionConfigOption() {}

// Config option holding one value out of a list of choices, flat or in groups
type SessionConfigOptionSelect struct {
	CurrentValue string `json:"currentValue"`
	// Flat choices, written under options when Groups is nil
	Options []SessionConfigSelectOption `json:"-"`
	// Grouped choices, written under options in place of Options when not nil
	Groups      []SessionConfigSelectGroup `json:"-"`
	Meta        *jsonutil.Object           `json:"_meta,omitempty"`
	Category    *string                    `json:"category,omitempty"`
	Description *string                    `json:"description,omitempty"`
	ID          string                     `json:"id"`
	Name        string                     `json:"name"`
}

// JSON object of the option with its choices under options and type "select"
func (o SessionConfigOptionSelect) MarshalJSON() ([]byte, error) {
	var options any = o.Options
	if o.Groups != nil {
		options = o.Groups
	}
	return withTag(struct {
		CurrentValue string           `json:"currentValue"`
		Options      any              `json:"options"`
		Meta         *jsonutil.Object `json:"_meta,omitempty"`
		Category     *string          `json:"category,omitempty"`
		Description  *string          `json:"description,omitempty"`
		ID           string           `json:"id"`
		Name         string           `json:"name"`
	}{o.CurrentValue, options, o.Meta, o.Category, o.Description, o.ID, o.Name}, "type", "select")
}

// Fills the option from its JSON object, which must carry currentValue, options, id and name; options whose first item names a group fill Groups, others Options
func (o *SessionConfigOptionSelect) UnmarshalJSON(data []byte) error {
	type plain SessionConfigOptionSelect
	var w struct {
		*plain
		Options []json.RawMessage `json:"options"`
	}
	w.plain = (*plain)(o)
	if err := decodeObject(data, &w, "SessionConfigOptionSelect", "currentValue", "options", "id", "name"); err != nil {
		return err
	}
	o.Options, o.Groups = nil, nil
	if len(w.Options) > 0 && hasMember(w.Options[0], "group") {
		groups, err := decodeEach(w.Options, "options", decodeValue[SessionConfigSelectGroup])
		o.Groups = groups
		return err
	}
	options, err := decodeEach(w.Options, "options", decodeValue[SessionConfigSelectOption])
	o.Options = options
	return err
}

// Config option switched on or off
type SessionConfigOptionBoolean struct {
	CurrentValue bool             `json:"currentValue"`
	Meta         *jsonutil.Object `json:"_meta,omitempty"`
	Category     *string          `json:"category,omitempty"`
	Description  *string          `json:"description,omitempty"`
	ID           string           `json:"id"`
	Name         string           `json:"name"`
}

// JSON object of the option with type "boolean"
func (o SessionConfigOptionBoolean) MarshalJSON() ([]byte, error) {
	type plain SessionConfigOptionBoolean
	return withTag(plain(o), "type", "boolean")
}

// Fills the option from its JSON object, which must carry currentValue, id and name
func (o *SessionConfigOptionBoolean) UnmarshalJSON(data []byte) error {
	type plain SessionConfigOptionBoolean
	return decodeObject(data, (*plain)(o), "SessionConfigOptionBoolean", "currentValue", "id", "name")
}

// One choice of a select config option
type SessionConfigSelectOption struct {
	Meta        *jsonutil.Object `json:"_meta,omitempty"`
	Description *string          `json:"description,omitempty"`
	Name        string           `json:"name"`
	Value       string           `json:"value"`
}

// Fills the choice from its JSON object, which must carry name and value
func (o *SessionConfigSelectOption) UnmarshalJSON(data []byte) error {
	type plain SessionConfigSelectOption
	return decodeObject(data, (*plain)(o), "SessionConfigSelectOption", "name", "value")
}

// Named group of choices of a select config option
type SessionConfigSelectGroup struct {
	Meta    *jsonutil.Object            `json:"_meta,omitempty"`
	Group   string                      `json:"group"`
	Name    string                      `json:"name"`
	Options []SessionConfigSelectOption `json:"options"`
}

// Fills the group from its JSON object, which must carry group, name and options
func (g *SessionConfigSelectGroup) UnmarshalJSON(data []byte) error {
	type plain SessionConfigSelectGroup
	return decodeObject(data, (*plain)(g), "SessionConfigSelectGroup", "group", "name", "options")
}

// Params of session/set_config_option for a boolean option
type SetSessionConfigOptionBooleanRequest struct {
	Meta      *jsonutil.Object `json:"_meta,omitempty"`
	ConfigID  string           `json:"configId"`
	SessionID string           `json:"sessionId"`
	Value     bool             `json:"value"`
}

// JSON object of the request with type "boolean" before its value
func (r SetSessionConfigOptionBooleanRequest) MarshalJSON() ([]byte, error) {
	text, err := jsonutil.Encode(struct {
		Meta      *jsonutil.Object `json:"_meta,omitempty"`
		ConfigID  string           `json:"configId"`
		SessionID string           `json:"sessionId"`
		Type      string           `json:"type"`
		Value     bool             `json:"value"`
	}{r.Meta, r.ConfigID, r.SessionID, "boolean", r.Value})
	if err != nil {
		return nil, err
	}
	return []byte(text), nil
}

// Fills the request from its JSON object, which must carry configId, sessionId and value
func (r *SetSessionConfigOptionBooleanRequest) UnmarshalJSON(data []byte) error {
	type plain SetSessionConfigOptionBooleanRequest
	return decodeObject(data, (*plain)(r), "SetSessionConfigOptionBooleanRequest", "configId", "sessionId", "value")
}

// Params of session/set_config_option for a select option
type SetSessionConfigOptionSelectRequest struct {
	Meta      *jsonutil.Object `json:"_meta,omitempty"`
	ConfigID  string           `json:"configId"`
	SessionID string           `json:"sessionId"`
	Value     string           `json:"value"`
}

// Fills the request from its JSON object, which must carry configId, sessionId and value
func (r *SetSessionConfigOptionSelectRequest) UnmarshalJSON(data []byte) error {
	type plain SetSessionConfigOptionSelectRequest
	return decodeObject(data, (*plain)(r), "SetSessionConfigOptionSelectRequest", "configId", "sessionId", "value")
}

// Result of session/set_config_option: every config option of the session after the change
type SetSessionConfigOptionResponse struct {
	Meta          *jsonutil.Object      `json:"_meta,omitempty"`
	ConfigOptions []SessionConfigOption `json:"configOptions"`
}

// Fills the response from its JSON object, which must carry configOptions
func (r *SetSessionConfigOptionResponse) UnmarshalJSON(data []byte) error {
	type plain SetSessionConfigOptionResponse
	var w struct {
		*plain
		ConfigOptions []json.RawMessage `json:"configOptions"`
	}
	w.plain = (*plain)(r)
	if err := decodeObject(data, &w, "SetSessionConfigOptionResponse", "configOptions"); err != nil {
		return err
	}
	options, err := decodeEach(w.ConfigOptions, "configOptions", decodeSessionConfigOption)
	r.ConfigOptions = options
	return err
}

// Set config option request as an agent receives it, boolean and select alike: Value is a bool or a string
type SetConfigOptionParams struct {
	Meta      *jsonutil.Object
	ConfigID  string
	SessionID string
	Value     any
}

// Params of session/set_config_option decoded as the boolean request when typed boolean, else as the select request
func decodeSetConfigOption(params []byte) (*SetConfigOptionParams, error) {
	if kind, _ := tagOf(params, "type"); kind == "boolean" {
		req, err := decodeParams[SetSessionConfigOptionBooleanRequest](params)
		if err != nil {
			return nil, err
		}
		return &SetConfigOptionParams{Meta: req.Meta, ConfigID: req.ConfigID, SessionID: req.SessionID, Value: req.Value}, nil
	}
	req, err := decodeParams[SetSessionConfigOptionSelectRequest](params)
	if err != nil {
		return nil, err
	}
	return &SetConfigOptionParams{Meta: req.Meta, ConfigID: req.ConfigID, SessionID: req.SessionID, Value: req.Value}, nil
}

// Config option of a JSON object, the variant chosen by its type
func decodeSessionConfigOption(data []byte) (SessionConfigOption, error) {
	kind, ok := tagOf(data, "type")
	if !ok {
		return nil, &MissingFieldError{Type: "SessionConfigOption", Field: "type"}
	}
	var option SessionConfigOption
	switch kind {
	case "select":
		option = new(SessionConfigOptionSelect)
	case "boolean":
		option = new(SessionConfigOptionBoolean)
	default:
		return nil, &UnknownVariantError{Union: "SessionConfigOption", Key: "type", Value: kind}
	}
	if err := json.Unmarshal(data, option); err != nil {
		return nil, err
	}
	return option, nil
}
