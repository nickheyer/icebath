package gatewayrun

import (
	"context"
	"strings"
	"testing"
)

func TestFormatSessionInfoConfigContextLength(t *testing.T) {
	runtimeTestHome(t, "model:\n  default: test-model\n  context_length: 32768\n", nil)
	info := allocGatewayRunner().formatSessionInfo(context.Background())
	if !strings.Contains(info, "32K") || !strings.Contains(info, "config") {
		t.Fatalf("info %q", info)
	}
}

func TestFormatSessionInfoLocalEndpointShown(t *testing.T) {
	runtimeTestHome(t, "model:\n  default: qwen3:8b\n  provider: custom\n  base_url: http://localhost:11434/v1\n  context_length: 8192\n", nil)
	info := allocGatewayRunner().formatSessionInfo(context.Background())
	if !strings.Contains(info, "localhost:11434") || !strings.Contains(info, "8K") {
		t.Fatalf("info %q", info)
	}
}

func TestFormatSessionInfoMoaPresetNamesTheBilledAggregator(t *testing.T) {
	runtimeTestHome(t, "model:\n  default: review\n  provider: moa\nmoa:\n  presets:\n    review:\n      reference_models:\n        - provider: openai\n          model: gpt-5.5\n      aggregator:\n        provider: nous\n        model: claude-opus-4.8\n", nil)
	info := allocGatewayRunner().formatSessionInfo(context.Background())
	if !strings.Contains(info, "nous:claude-opus-4.8") {
		t.Fatalf("info %q", info)
	}
}
