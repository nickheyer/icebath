package gateway

import (
	"slices"
	"testing"
)

func TestDisplayWordSettingsAcceptOnlyTheirOwnWords(t *testing.T) {
	cfg := map[string]any{}
	got, err := setDisplay(cfg, "theme", "dark")
	if err != nil || got["value"] != "dark" || displaySection(cfg)["tui_theme"] != "dark" {
		t.Fatalf("theme dark gave %v, %v, %v", got, err, cfg)
	}
	if _, err := setDisplay(cfg, "theme", "sepia"); !isRuntimeRefusal(err, "invalid_params") {
		t.Fatalf("an unknown theme word gave %v", err)
	}
	words := displayWords()
	if words["indicator"].field != "tui_status_indicator" || len(words["indicator"].allowed) == 0 || !slices.Contains(words["theme"].allowed, "auto") {
		t.Fatalf("word settings %v", words)
	}
}

func TestTranscriptDrainLockIsTheOneLockOfTheStore(t *testing.T) {
	store := &SessionStore{}
	lock := store.transcriptDrainLock()
	lock.Lock()
	if store.transcriptDrainLock().TryLock() {
		t.Fatal("a second holder took the drain lock")
	}
	lock.Unlock()
}
