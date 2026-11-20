package agent

import (
	"context"
	"strings"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

var thinkingTimeoutSubstrings = []string{
	"broken pipe", "errno 32", "remote protocol", "connection reset", "connection lost",
	"peer closed", "server disconnected",
}

// Reason value of a classified error, read from the provider verdict or a duck typed reason object
func classifiedReasonValue(classified any) any {
	if c, ok := classified.(*provider.Classified); ok {
		if c == nil {
			return nil
		}
		return string(c.Reason)
	}
	reason := state.ToolCallField(classified, "reason")
	if r, ok := reason.(provider.FailoverReason); ok {
		return string(r)
	}
	return state.ToolCallField(reason, "value")
}

// True when a reasoning model's thinking phase hit a transport kill before its first content token
func IsThinkingTimeout(classified any, model string, errorMsg *string) bool {
	if classifiedReasonValue(classified) != "timeout" {
		return false
	}
	if _, ok := provider.ReasoningStaleTimeoutFloor(model); !ok {
		return false
	}
	msg := ""
	if errorMsg != nil {
		msg = pystr.Lower(*errorMsg)
	}
	for _, p := range thinkingTimeoutSubstrings {
		if strings.Contains(msg, p) {
			return true
		}
	}
	return false
}

// User facing guidance naming the easiest fix first and the per model stale timeout knob last
func BuildThinkingTimeoutGuidance(ctx context.Context, providerName, model, modelLabel string) string {
	label := modelLabel
	if label == "" {
		label = model
	}
	return label + " was thinking for so long that the connection timed out before it wrote anything " +
		"(common for reasoning models behind cloud gateways such as NVIDIA NIM, OpenAI, Anthropic, " +
		"DeepSeek). Easiest fixes: `/reasoning low`, or switch to a faster model with /model. " +
		"Advanced: set `providers." + providerName + ".models." + model + ".stale_timeout_seconds: 900` in " +
		"`" + home.Display(home.Dir(ctx)) + "/config.yaml` to allow a longer wait."
}
