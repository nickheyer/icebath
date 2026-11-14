package gatewayrun

import (
	"testing"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gateway"
)

func TestWarningSurvivesNoiseFilter(t *testing.T) {
	emitted := func(reason string) string { return agent.ContextOverflowBlockedWarning(85000, 72000, reason) }
	t.Run("test_cooldown_warning_not_matched_by_noise_regex", func(t *testing.T) {
		if matched, err := telegramNoisyStatusRe.MatchString(emitted("cooldown:30")); err != nil || matched {
			t.Fatalf("matched %v err %v", matched, err)
		}
	})
	t.Run("test_warning_delivered_on_chat_platform", func(t *testing.T) {
		message := emitted("cooldown:30")
		if got := prepareGatewayStatusMessage(statusCtx(t), gateway.PlatformTelegram, "warn", message); got == nil || *got != message {
			t.Fatalf("got %v", got)
		}
	})
}
