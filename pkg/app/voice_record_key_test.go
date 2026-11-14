package app

import (
	"reflect"
	"testing"
)

func TestNormalizeVoiceRecordKeyForPromptToolkit(t *testing.T) {
	t.Run("test_non_string_falls_back_to_default", func(t *testing.T) {
		for _, raw := range []any{nil, 1, true, map[string]any{}} {
			if got := normalizeVoiceRecordKeyForPromptToolkit(raw); got != "c-b" {
				t.Fatalf("%v normalized to %q", raw, got)
			}
		}
	})
	t.Run("test_super_win_fall_back_to_default_in_cli", func(t *testing.T) {
		for _, raw := range []string{"super+b", "win+o", "windows+o"} {
			if got := normalizeVoiceRecordKeyForPromptToolkit(raw); got != "c-b" {
				t.Fatalf("%q normalized to %q", raw, got)
			}
		}
	})
	t.Run("test_pt_key_to_sequence", func(t *testing.T) {
		for key, want := range map[string][]string{"c-b": {"c-b"}, "a-v": {"escape", "v"}, "a-space": {"escape", "space"}} {
			if got := ptKeyToSequence(key); !reflect.DeepEqual(got, want) {
				t.Fatalf("%q became %q", key, got)
			}
		}
	})
}

func TestVoiceRecordKeyFromConfig(t *testing.T) {
	t.Run("test_missing_record_key_returns_none", func(t *testing.T) {
		if got := voiceRecordKeyFromConfig(map[string]any{"voice": map[string]any{"beep_enabled": true}}); got != nil {
			t.Fatalf("got %v", got)
		}
		if got := voiceRecordKeyFromConfig(map[string]any{}); got != nil {
			t.Fatalf("got %v", got)
		}
	})
	t.Run("test_normalizer_accepts_extractor_output_directly", func(t *testing.T) {
		for _, raw := range []any{nil, true, 1, "cmd+b", []any{"ctrl+b"}} {
			extracted := voiceRecordKeyFromConfig(map[string]any{"voice": raw})
			if got := normalizeVoiceRecordKeyForPromptToolkit(extracted); got != "c-b" {
				t.Fatalf("voice %v normalized to %q", raw, got)
			}
		}
	})
}

func TestFormatVoiceRecordKeyForStatus(t *testing.T) {
	t.Run("test_ctrl_and_alt_letter_keys_render_canonically", func(t *testing.T) {
		for raw, want := range map[string]string{"ctrl+b": "Ctrl+B", "ctrl+o": "Ctrl+O", "alt+r": "Alt+R"} {
			if got := formatVoiceRecordKeyForStatus(raw); got != want {
				t.Fatalf("%q rendered %q", raw, got)
			}
		}
	})
	t.Run("test_non_string_scalar_falls_back_to_ctrl_b_label", func(t *testing.T) {
		for _, raw := range []any{true, 1, nil, map[string]any{}} {
			if got := formatVoiceRecordKeyForStatus(raw); got != "Ctrl+B" {
				t.Fatalf("%v rendered %q", raw, got)
			}
		}
	})
	t.Run("test_malformed_configs_fall_back_to_ctrl_b", func(t *testing.T) {
		for _, raw := range []string{"ctrl+spcae", "ctrl+alt+r", "", "  "} {
			if got := formatVoiceRecordKeyForStatus(raw); got != "Ctrl+B" {
				t.Fatalf("%q rendered %q", raw, got)
			}
		}
	})
}
