package agent

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"

	"github.com/nickheyer/icebath/pkg/state"
)

// Promotes a GMI served anthropic model to the native wire once the concurrency probe is clean
var gmiNativeWireCleared = false

var nousAnthropicWireMode = nousAnthropicWire

var switchAgentModel = (*AIAgent).switchModel

// Hook the usage recorder calls on the first response, a seam for the wiring test
var maybeSwitchWireAfterFirstResponse = (*AIAgent).maybeSwitchWireAfterFirstResponse

var openRouterGenerationID = regexp.MustCompile(`^gen-\d{9,}-[A-Za-z0-9_-]{8,}$`)

// Wire switch the first response scheduled for the next iteration start
type nousWireSwitch struct {
	want     string
	upstream string
}

func nousWireLog() *slog.Logger { return slog.Default().With("logger", "agent.nous_wire") }

// Upstream a Portal response came from, openrouter or gmi, empty when unknown
func classifyUpstream(response any) string {
	if response == nil {
		return ""
	}
	if p, ok := state.ToolCallField(response, "provider").(string); ok && p != "" {
		return "openrouter"
	}
	if rid, ok := state.ToolCallField(response, "id").(string); ok {
		if openRouterGenerationID.MatchString(rid) {
			return "openrouter"
		}
		if strings.HasPrefix(rid, "msg_") {
			return "gmi"
		}
	}
	return ""
}

// Api mode auto wants once the upstream is known, chat unless GMI and cleared
func wireForUpstream(upstream string) string {
	if upstream == "gmi" && gmiNativeWireCleared {
		return "anthropic_messages"
	}
	return "chat_completions"
}

// Display form of an upstream the way Python formats None
func upstreamLabel(upstream string) string {
	if upstream == "" {
		return "None"
	}
	return upstream
}

// Schedules the session wire from its first Nous anthropic response when the wire is auto
func (a *AIAgent) maybeSwitchWireAfterFirstResponse(ctx context.Context, response any, apiCallCount int) bool {
	if apiCallCount != 1 || a.nousWireDecided {
		return false
	}
	if strings.ToLower(a.provider) != "nous" {
		return false
	}
	if !strings.HasPrefix(strings.ToLower(a.model), "anthropic/") {
		return false
	}
	if nousAnthropicWireMode(ctx) != "auto" {
		return false
	}
	a.nousWireDecided = true
	upstream := classifyUpstream(response)
	want := wireForUpstream(upstream)
	if want == a.apiMode {
		nousWireLog().Debug(fmt.Sprintf("nous wire auto: upstream=%s, staying on %s", upstreamLabel(upstream), want))
		return false
	}
	a.nousWirePending = &nousWireSwitch{want: want, upstream: upstream}
	return true
}

// Performs the scheduled wire switch at iteration start, staying on the current wire when it fails
func applyPendingWireSwitch(ctx context.Context, a *AIAgent) bool {
	pending, _ := a.nousWirePending.(*nousWireSwitch)
	if pending == nil {
		return false
	}
	a.nousWirePending = nil
	if err := switchAgentModel(a, ctx, a.model, "nous", a.apiKey, a.baseURL, pending.want, nil); err != nil {
		nousWireLog().Warn(fmt.Sprintf("nous wire auto: switch to %s failed (%v); staying on %s", pending.want, err, a.apiMode))
		return false
	}
	sessionID := a.sessionID
	if sessionID == "" {
		sessionID = "?"
	}
	nousWireLog().Info(fmt.Sprintf("nous wire auto: upstream=%s -> %s for the rest of session %s", upstreamLabel(pending.upstream), pending.want, sessionID))
	return true
}
