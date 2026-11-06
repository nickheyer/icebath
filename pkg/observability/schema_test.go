package observability

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestV3SchemaAcceptsExactlyTheContractValues(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("schemas", "icebath.shared_metrics.v4.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Defs map[string]map[string]any `json:"$defs"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	byName := map[string]map[string]any{}
	for _, def := range schema.Defs {
		if props, ok := def["properties"].(map[string]any); ok {
			if name, ok := props["name"].(map[string]any)["const"].(string); ok {
				byName[name] = props
			}
		}
	}
	for _, metric := range []string{UpdateRunMetric, UpdateStageMetric, ProcessExitMetric} {
		dims := byName[metric]["dimensions"].(map[string]any)
		if oneOf, ok := dims["oneOf"].([]any); ok {
			dims = oneOf[0].(map[string]any)
		}
		fields := dims["properties"].(map[string]any)
		contract := counterDimensionValues[metric]
		if len(fields) != len(contract) {
			t.Fatalf("%s: schema fields %v, contract fields %v", metric, fields, contract)
		}
		for field, spec := range fields {
			allowed, ok := contract[field].(ValueSet)
			if !ok {
				t.Fatalf("%s.%s is not a closed static set", metric, field)
			}
			enum := ValueSet{}
			for _, v := range spec.(map[string]any)["enum"].([]any) {
				enum[v.(string)] = struct{}{}
			}
			if !enum.Equal(allowed) {
				t.Fatalf("%s.%s: schema %v, contract %v", metric, field, enum.Sorted(), allowed.Sorted())
			}
		}
	}
}
