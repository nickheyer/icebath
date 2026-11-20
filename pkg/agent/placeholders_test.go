package agent

import (
	"reflect"
	"testing"
)

func TestHiddenInterruptPlaceholderRowIsFreshAndHidden(t *testing.T) {
	a, b := HiddenInterruptPlaceholderRow(), HiddenInterruptPlaceholderRow()
	want := map[string]any{"role": "assistant", "content": "", "display_kind": "hidden", "api_content": "[interrupt: no assistant output for this turn]"}
	if !reflect.DeepEqual(a, want) {
		t.Fatalf("row %v", a)
	}
	a["content"] = "x"
	if b["content"] != "" {
		t.Fatal("rows share storage")
	}
	if LegacyInterruptedPlaceholder != "[response interrupted]" {
		t.Fatal("legacy spelling changed")
	}
}
