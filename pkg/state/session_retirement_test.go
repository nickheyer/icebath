package state

import (
	"testing"

	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

func TestLoadTerminalResultExportUnwrapsTheEncodedEnvelope(t *testing.T) {
	result := jsonutil.ObjectOf("result", jsonutil.ObjectOf("cron_result", []any{true, "doc", "answer", nil}), "usage", jsonutil.NewObject())
	text, err := jsonutil.Encode(encodeTerminalResult(result))
	if err != nil {
		t.Fatal(err)
	}
	got, err := LoadTerminalResult(text)
	if err != nil || !anyutil.Equal(got, result) {
		t.Fatalf("loaded = %v, %v", got, err)
	}
}
