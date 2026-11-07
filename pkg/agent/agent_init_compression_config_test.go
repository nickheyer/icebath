package agent

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

func compressionConfigAgent() *AIAgent {
	return &AIAgent{model: "m", provider: "openrouter", apiMode: "chat_completions", quietMode: true}
}

func TestThresholdTokensDefaultAndNullOptOut(t *testing.T) {
	defaults := map[string]any(config.Defaults())
	shippedTokens, _ := config.CfgGet(defaults, "compression", "threshold_tokens")
	shippedThreshold, _ := config.CfgGet(defaults, "compression", "threshold")
	for _, tc := range []struct {
		cfg  map[string]any
		want any
	}{
		{map[string]any{}, shippedTokens},
		{map[string]any{"compression": map[string]any{"threshold_tokens": nil}}, nil},
	} {
		cs, err := parseCompressionConfig(compressionConfigAgent(), tc.cfg)
		if err != nil {
			t.Fatal(err)
		}
		var got any
		if cs.ThresholdTokens != nil {
			got = *cs.ThresholdTokens
		}
		if got != tc.want || cs.Threshold != shippedThreshold {
			t.Fatalf("cfg %v threshold_tokens %v want %v threshold %v want %v", tc.cfg, got, tc.want, cs.Threshold, shippedThreshold)
		}
	}
}

func TestConfigFlagReachesTheCompressorThroughTheLoader(t *testing.T) {
	if v, _ := config.CfgGet(map[string]any(config.Defaults()), "compression", "prepare_ahead"); v != false {
		t.Fatalf("shipped prepare_ahead %v", v)
	}
	h := filepath.Join(t.TempDir(), "profile")
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	load := func() map[string]any {
		tree, err := config.LoadReadonly(t.Context(), h, config.EnvRefLookup)
		if err != nil {
			t.Fatal(err)
		}
		return map[string]any(tree)
	}
	if cs, err := parseCompressionConfig(compressionConfigAgent(), load()); err != nil || cs.PrepareAhead {
		t.Fatalf("default prepare_ahead %v %v", cs.PrepareAhead, err)
	}
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte("compression:\n  prepare_ahead: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if cs, err := parseCompressionConfig(compressionConfigAgent(), load()); err != nil || !cs.PrepareAhead {
		t.Fatalf("configured prepare_ahead %v %v", cs.PrepareAhead, err)
	}
}

func TestCompressionConfigDefaultsCoercionsAndGarbage(t *testing.T) {
	cs, err := parseCompressionConfig(compressionConfigAgent(), map[string]any{"compression": map[string]any{
		"max_attempts": 0, "protect_first_n": -2, "tail_mode": " LEGACY ", "min_tail_user_messages": 0, "model_thresholds": map[string]any{"gpt": 0.7, "flag": true, "n": 1},
		"codex_app_server_auto": "Bogus", "codex_responses_native": "off", "codex_responses_compact_threshold": 2.5, "in_place": nil, "micro_compact_every_n_turns": "x",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if cs.MaxAttempts != 3 || cs.ProtectFirst != 0 || cs.TailMode != "legacy" || cs.MinTailUsers != 1 || len(cs.ModelThresholds) != 2 || cs.ModelThresholds["n"] != 1 {
		t.Fatalf("coercions %+v", cs)
	}
	if cs.CodexAppServerAuto != "native" || cs.CodexResponsesNative || cs.CodexResponsesCompactThreshold != nil || !cs.InPlace || cs.MicroCompactEveryNTurns != 1 || !cs.Enabled || cs.TargetRatio != 0.20 || cs.ProtectLast != 20 {
		t.Fatalf("defaults %+v", cs)
	}
	if _, err := parseCompressionConfig(compressionConfigAgent(), map[string]any{"compression": map[string]any{"protect_last_n": "many"}}); err == nil {
		t.Fatal("garbage protect_last_n must raise")
	}
	if _, err := parseCompressionConfig(&AIAgent{apiMode: "codex_app_server"}, map[string]any{"compression": map[string]any{"checkpoint_required": "yes"}}); err == nil {
		t.Fatal("checkpoint_required on codex_app_server must be refused")
	}
}

func TestInvalidConfigIntPrintHonorsPolicyLogAlways(t *testing.T) {
	for _, setting := range []any{nil, false, true} {
		h := t.TempDir()
		t.Setenv("ICEBATH_HOME", h)
		display := map[string]any{}
		if setting != nil {
			display["suppress_warning_notifications"] = setting
		}
		raw, _ := json.Marshal(map[string]any{"display": display})
		if err := os.WriteFile(filepath.Join(h, "config.yaml"), raw, 0o644); err != nil {
			t.Fatal(err)
		}
		var errOut, logOut bytes.Buffer
		prevErr, prevLog := invalidConfigStderr, slog.Default()
		invalidConfigStderr = &errOut
		slog.SetDefault(slog.New(slog.NewTextHandler(&logOut, nil)))
		warnInvalidConfigInt(t.Context(), "model.context_length in config.yaml", "256K", "must be a plain integer", "auto-detection", "", &AIAgent{platform: "cli"})
		invalidConfigStderr = prevErr
		slog.SetDefault(prevLog)
		if strings.Contains(errOut.String(), "⚠ Invalid model.context_length") != (setting != true) {
			t.Fatalf("setting %v printed %q", setting, errOut.String())
		}
		if !strings.Contains(logOut.String(), "Invalid model.context_length") {
			t.Fatalf("setting %v log %q", setting, logOut.String())
		}
	}
}
