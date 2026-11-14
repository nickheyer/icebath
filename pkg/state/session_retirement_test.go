package state

import (
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func TestLoadTerminalResultExportUnwrapsTheEncodedEnvelope(t *testing.T) {
	result := pyjson.ObjectOf("result", pyjson.ObjectOf("cron_result", []any{true, "doc", "answer", nil}), "usage", pyjson.NewObject())
	text, err := pyjson.Dumps(encodeTerminalResult(result))
	if err != nil {
		t.Fatal(err)
	}
	got, err := LoadTerminalResult(text)
	if err != nil || !pyjson.Equal(got, result) {
		t.Fatalf("loaded = %v, %v", got, err)
	}
}
