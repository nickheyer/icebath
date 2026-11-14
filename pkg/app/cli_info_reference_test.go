package app

import (
	"encoding/json"
	"testing"
)

// Reference output of the hermes commands as real hermes rendered them
type infoReference struct {
	Columns int                        `json:"columns"`
	Cases   map[string]json.RawMessage `json:"cases"`
}

func (r infoReference) text(t *testing.T, name string) string {
	t.Helper()
	var s string
	if err := json.Unmarshal(r.Cases[name], &s); err != nil {
		t.Fatalf("case %s: %v", name, err)
	}
	return s
}

func ptr[T any](v T) *T { return &v }
