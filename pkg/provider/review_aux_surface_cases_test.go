package provider

import (
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

func TestReviewRegisteredInEveryAuxSurface(t *testing.T) {
	aux, _ := config.Defaults()["auxiliary"].(map[string]any)
	if _, ok := aux["review"]; !ok {
		t.Fatal("review missing from DEFAULT_CONFIG['auxiliary']")
	}
	inPicker := false
	for _, task := range auxTasks {
		inPicker = inPicker || task.Key == "review"
	}
	if !inPicker {
		t.Fatal("review missing from the CLI picker aux tasks")
	}
	inSlots := false
	for _, slot := range auxTaskSlotsBuiltin {
		inSlots = inSlots || slot == "review"
	}
	if !inSlots {
		t.Fatal("review missing from the dashboard aux task slots")
	}
}
