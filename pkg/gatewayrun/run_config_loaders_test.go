package gatewayrun

import (
	"context"
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

func TestParseReasoningCommandArgsAcceptsASCIIAndSmartGlobalFlags(t *testing.T) {
	if value, global := parseReasoningCommandArgs("high --global"); value != "high" || !global {
		t.Fatalf("got %q %v", value, global)
	}
	if value, global := parseReasoningCommandArgs("—global xhigh"); value != "xhigh" || !global {
		t.Fatalf("got %q %v", value, global)
	}
}
