package gatewayrun

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Gateway config with one discord platform holding the given channel overrides
func discordOverrideConfig(overrides map[string]*gateway.ChannelOverride) *gateway.GatewayConfig {
	platform := gateway.NewPlatformConfig()
	platform.Enabled = true
	platform.ChannelOverrides = overrides
	return &gateway.GatewayConfig{Platforms: map[gateway.Platform]*gateway.PlatformConfig{gateway.PlatformDiscord: platform}}
}

func TestGetChannelOverrideNoOverrideWhenChannelNotInOverrides(t *testing.T) {
	config := discordOverrideConfig(map[string]*gateway.ChannelOverride{"999": {Model: "openrouter/healer-alpha"}})
	if got := getChannelOverride(config, gateway.PlatformDiscord, "123", nil, nil); got != nil {
		t.Fatalf("override %+v", got)
	}
}

func TestGetChannelOverrideReturnsOverrideWhenChannelMatches(t *testing.T) {
	ov := &gateway.ChannelOverride{Model: "openrouter/healer-alpha", Provider: "openrouter", SystemPrompt: "You are a summarizer."}
	config := discordOverrideConfig(map[string]*gateway.ChannelOverride{"1234567890": ov})
	result := getChannelOverride(config, gateway.PlatformDiscord, "1234567890", nil, nil)
	if result == nil || result.Model != "openrouter/healer-alpha" || result.Provider != "openrouter" ||
		result.SystemPrompt != "You are a summarizer." {
		t.Fatalf("override %+v", result)
	}
}

func TestGetChannelOverrideThreadIDLookupWhenChatIDMisses(t *testing.T) {
	config := discordOverrideConfig(map[string]*gateway.ChannelOverride{"thread_99": {Model: "topic-model"}})
	thread := "thread_99"
	result := getChannelOverride(config, gateway.PlatformDiscord, "parent_chan", &thread, nil)
	if result == nil || result.Model != "topic-model" {
		t.Fatalf("override %+v", result)
	}
}

func TestResolveModelForChannelUsesChannelOverrideWhenPresent(t *testing.T) {
	runner := allocGatewayRunner()
	runner.config = discordOverrideConfig(map[string]*gateway.ChannelOverride{"chan_1": {Model: "anthropic/claude-opus-4.6"}})
	if model := runner.resolveModelForChannel(context.Background(), gateway.PlatformDiscord, "chan_1", nil, nil, nil); model != "anthropic/claude-opus-4.6" {
		t.Fatalf("model %q", model)
	}
}

func TestGetSystemPromptForChannelUsesChannelOverrideWhenPresent(t *testing.T) {
	runner := allocGatewayRunner()
	runner.config = discordOverrideConfig(map[string]*gateway.ChannelOverride{"chan_1": {SystemPrompt: "You are a coding assistant."}})
	if prompt := runner.getSystemPromptForChannel(context.Background(), gateway.PlatformDiscord, "chan_1", nil, nil); prompt != "You are a coding assistant." {
		t.Fatalf("prompt %q", prompt)
	}
}

func TestBusyTextTimingRejectsNonNumericConfig(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	timing := busyTextTimingFromConfig(map[string]any{"display": map[string]any{
		"busy_text_debounce_seconds": "fast", "busy_text_hard_cap_seconds": -3}})
	if timing != [2]float64{0.35, 1.0} {
		t.Fatalf("timing %v", timing)
	}
	text := logs.String()
	if !strings.Contains(text, "busy_text_debounce_seconds") || !strings.Contains(text, "busy_text_hard_cap_seconds") {
		t.Fatalf("logs %s", text)
	}
}

func TestParseReasoningCommandArgsAcceptsASCIIAndSmartGlobalFlags(t *testing.T) {
	if value, global := parseReasoningCommandArgs("high --global"); value != "high" || !global {
		t.Fatalf("got %q %v", value, global)
	}
	if value, global := parseReasoningCommandArgs("—global xhigh"); value != "xhigh" || !global {
		t.Fatalf("got %q %v", value, global)
	}
}

func TestConfigBoundsAreValidatedWithAWarningNamingTheKey(t *testing.T) {
	cases := []struct {
		cfg       map[string]any
		expected  *[2]int
		warnedKey string
	}{
		{map[string]any{"mode": "off", "min_ms": -5}, nil, ""},
		{map[string]any{"mode": "natural", "min_ms": "oops", "max_ms": "bad"}, &[2]int{800, 2500}, ""},
		{map[string]any{"mode": "custom", "min_ms": 1000, "max_ms": 2000}, &[2]int{1000, 2000}, ""},
		{map[string]any{"mode": "custom", "min_ms": "oops", "max_ms": 1000}, &[2]int{800, 1000}, "human_delay.min_ms"},
		{map[string]any{"mode": "custom", "min_ms": -1, "max_ms": 1000}, &[2]int{800, 1000}, "human_delay.min_ms"},
		{map[string]any{"mode": "custom", "min_ms": 3000, "max_ms": 1000}, &[2]int{800, 2500}, "human_delay.max_ms=1000"},
	}
	for _, c := range cases {
		var logs bytes.Buffer
		previous := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
		got := humanDelayFromConfig(map[string]any{"human_delay": c.cfg})
		slog.SetDefault(previous)
		if (got == nil) != (c.expected == nil) || (got != nil && *got != *c.expected) {
			t.Fatalf("cfg %v got %v", c.cfg, got)
		}
		if c.warnedKey == "" && strings.Contains(logs.String(), "human_delay") {
			t.Fatalf("cfg %v warned %s", c.cfg, logs.String())
		}
		if c.warnedKey != "" && !strings.Contains(logs.String(), c.warnedKey) {
			t.Fatalf("cfg %v logs %s", c.cfg, logs.String())
		}
	}
}
