package tools

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

func readOnboardingConfig(t *testing.T, path string) map[string]any {
	t.Helper()
	loaded, err := config.ReadUserConfigRaw(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	return loaded
}

func seenFlags(t *testing.T, cfg map[string]any) map[string]any {
	t.Helper()
	section, _ := cfg["onboarding"].(map[string]any)
	seen, _ := section["seen"].(map[string]any)
	if seen == nil {
		t.Fatalf("no onboarding.seen in %v", cfg)
	}
	return seen
}

func TestIsSeenEmptyConfigUnseen(t *testing.T) {
	if IsSeen(map[string]any{}, OnboardingBusyInputFlag) {
		t.Fatal("empty config seen")
	}
}

func TestIsSeenSeenFlagTrue(t *testing.T) {
	cfg := map[string]any{"onboarding": map[string]any{"seen": map[string]any{OnboardingBusyInputFlag: true}}}
	if !IsSeen(cfg, OnboardingBusyInputFlag) {
		t.Fatal("true flag unseen")
	}
}

func TestIsSeenSeenFlagFalsy(t *testing.T) {
	cfg := map[string]any{"onboarding": map[string]any{"seen": map[string]any{OnboardingBusyInputFlag: false}}}
	if IsSeen(cfg, OnboardingBusyInputFlag) {
		t.Fatal("false flag seen")
	}
}

func TestMarkSeenPreservesOtherConfig(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("model:\n  default: claude-sonnet-4.6\ndisplay:\n  skin: default\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !MarkSeen(context.Background(), cfgPath, OnboardingBusyInputFlag) {
		t.Fatal("mark_seen failed")
	}
	loaded := readOnboardingConfig(t, cfgPath)
	if loaded["model"].(map[string]any)["default"] != "claude-sonnet-4.6" || loaded["display"].(map[string]any)["skin"] != "default" {
		t.Fatalf("other config lost: %v", loaded)
	}
	if seenFlags(t, loaded)[OnboardingBusyInputFlag] != true {
		t.Fatalf("flag not set: %v", loaded)
	}
}

func TestMarkSeenIdempotent(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	MarkSeen(context.Background(), cfgPath, OnboardingBusyInputFlag)
	first := readOnboardingConfig(t, cfgPath)
	MarkSeen(context.Background(), cfgPath, OnboardingBusyInputFlag)
	second := readOnboardingConfig(t, cfgPath)
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("second mark changed the config: %v vs %v", first, second)
	}
}

func TestRoundTripMarkThenIsSeen(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	if !MarkSeen(context.Background(), cfgPath, OnboardingBusyInputFlag) {
		t.Fatal("mark_seen failed")
	}
	loaded := readOnboardingConfig(t, cfgPath)
	if !IsSeen(loaded, OnboardingBusyInputFlag) || IsSeen(loaded, OnboardingToolProgressFlag) {
		t.Fatalf("flags %v", loaded)
	}
}

func TestRoundTripMarkBothFlagsIndependently(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	MarkSeen(context.Background(), cfgPath, OnboardingBusyInputFlag)
	MarkSeen(context.Background(), cfgPath, OnboardingToolProgressFlag)
	loaded := readOnboardingConfig(t, cfgPath)
	if !IsSeen(loaded, OnboardingBusyInputFlag) || !IsSeen(loaded, OnboardingToolProgressFlag) {
		t.Fatalf("flags %v", loaded)
	}
}

func TestDetectOpenclawResidueReturnsTrueWhenOpenclawDirPresent(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".openclaw"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !DetectOpenclawResidue(dir) {
		t.Fatal("residue directory not detected")
	}
}

func TestDetectOpenclawResidueReturnsFalseWhenPathIsAFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".openclaw"), []byte("oops"), 0o644); err != nil {
		t.Fatal(err)
	}
	if DetectOpenclawResidue(dir) {
		t.Fatal("stray file detected as residue")
	}
}

func TestProfileBuildModeNonMappingConfigSafe(t *testing.T) {
	if ProfileBuildMode("not a dict") != "ask" {
		t.Fatal("string config is not ask")
	}
	if ProfileBuildMode(map[string]any{"onboarding": "nope"}) != "ask" {
		t.Fatal("string onboarding section is not ask")
	}
}

func TestFirstContactTurnNoteReturnsProfileDirectiveAndMarksSeen(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	cfg := map[string]any{"onboarding": map[string]any{"profile_build": "ask"}}
	note := FirstContactTurnNote(context.Background(), cfg, cfgPath, true, false, false)
	if note == nil || *note != strings.TrimSpace(ProfileBuildDirective()) {
		t.Fatalf("note %v", note)
	}
	if seenFlags(t, readOnboardingConfig(t, cfgPath))[OnboardingProfileBuildFlag] != true {
		t.Fatal("profile build offer not latched")
	}
}

func TestFirstContactTurnNoteEveryFirstContactNotePutsARealTaskFirst(t *testing.T) {
	dir := t.TempDir()
	for _, mode := range []string{"ask", "off"} {
		note := FirstContactTurnNote(context.Background(), map[string]any{"onboarding": map[string]any{"profile_build": mode}},
			filepath.Join(dir, mode+".yaml"), true, false, false)
		if note == nil || !strings.Contains(*note, OnboardingTaskFirstClause) {
			t.Fatalf("%s: note %v", mode, note)
		}
	}
}

func TestFirstContactTurnNoteReturnsNoneWhenNotFirstContact(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	if FirstContactTurnNote(context.Background(), map[string]any{}, cfgPath, false, false, false) != nil {
		t.Fatal("note for a non empty history")
	}
	if FirstContactTurnNote(context.Background(), map[string]any{}, cfgPath, true, true, false) != nil {
		t.Fatal("note for an install with prior sessions")
	}
	if _, err := os.Stat(cfgPath); !os.IsNotExist(err) {
		t.Fatalf("config written: %v", err)
	}
}
