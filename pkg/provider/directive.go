package provider

import (
	"encoding/json"
	"net/http"
)

// Headers and body fields sent with a turn that asks for one directive
type Directive struct {
	Header http.Header                `json:"header,omitempty"`
	Extra  map[string]json.RawMessage `json:"extra,omitempty"`
}

// Deep copy sharing nothing with the original
func (d Directive) Clone() Directive {
	d.Header = d.Header.Clone()
	if d.Extra != nil {
		extra := make(map[string]json.RawMessage, len(d.Extra))
		for k, v := range d.Extra {
			extra[k] = append(json.RawMessage(nil), v...)
		}
		d.Extra = extra
	}
	return d
}
