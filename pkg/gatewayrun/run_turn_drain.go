package gatewayrun

import (
	"context"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const (
	interruptReasonStop          = "Stop requested"
	interruptReasonReset         = "Session reset requested"
	interruptReasonEvicted       = "Session ended while the turn was running"
	interruptReasonSSEDisconnect = "SSE client disconnected"
)

// Lowercased interrupt reasons the gateway itself issues, never a user follow up
var controlInterruptMessages = map[string]bool{
	strings.ToLower(interruptReasonStop): true, strings.ToLower(interruptReasonReset): true,
	strings.ToLower(interruptReasonTimeout): true, strings.ToLower(interruptReasonSSEDisconnect): true,
	strings.ToLower(interruptReasonEvicted): true, strings.ToLower(interruptReasonGatewayShutdown): true,
	strings.ToLower(interruptReasonGatewayRestart): true,
}

// Consumes and returns the adapter's pending event for the session, media metadata kept for preprocessing
func dequeuePendingEvent(adapter gateway.PlatformAdapter, sessionKey string) *gateway.MessageEvent {
	return adapter.Base().GetPendingMessage(sessionKey)
}

// Whether an interrupt message is internal control flow rather than user text
func isControlInterruptMessage(message any) bool {
	if !pyrepr.Truthy(message) {
		return false
	}
	normalized := strings.Join(pystr.SplitWhitespace(pystr.Strip(pyrepr.Str(message)), -1), " ")
	return controlInterruptMessages[pystr.Lower(normalized)]
}

// Pending event and text of the follow up the finished turn leaves queued, interrupt or leftover steer included
func (r *GatewayRunner) runAgentDrainPending(ctx context.Context, result map[string]any, adapter gateway.PlatformAdapter,
	source *gateway.SessionSource, sessionKey string) (*gateway.MessageEvent, string) {
	var pendingEvent *gateway.MessageEvent
	pending := ""
	if len(result) > 0 && adapter != nil && sessionKey != "" {
		pendingEvent = dequeuePendingEvent(adapter, sessionKey)
		if pyrepr.Truthy(result["compression_exhausted"]) && pendingEvent != nil {
			r.deferPastExhaustionReset(result, adapter, source, sessionKey, pendingEvent)
			return nil, ""
		}
		pendingEvent = r.promoteQueuedEvent(sessionKey, adapter, pendingEvent)
		if pyrepr.Truthy(result["interrupted"]) && pendingEvent == nil && pyrepr.Truthy(result["interrupt_message"]) {
			interruptMessage := result["interrupt_message"]
			if isControlInterruptMessage(interruptMessage) {
				key := sessionKey
				if key == "" {
					key = "?"
				}
				runLog().Info(fmt.Sprintf("Ignoring control interrupt message for session %s: %s", key, pyrepr.Str(interruptMessage)))
			} else if !gateway.ExecutingAdmission(ctx) {
				pending = pyrepr.Str(interruptMessage)
			}
		} else if pendingEvent != nil {
			pendingText := pendingEvent.Text
			if len(pendingEventAudioPaths(pendingEvent)) > 0 {
				var metadata map[string]any
				if source.ThreadID != nil && *source.ThreadID != "" {
					metadata = map[string]any{"thread_id": *source.ThreadID}
				}
				pending, _ = r.transcribeAndEchoPendingVoice(ctx, pendingEvent, adapter, source, pendingText, "Voice-drain", metadata, true)
				if pending == "" {
					pending = mediaOnlyEventText(pendingEvent)
				}
			} else {
				pending = pendingText
				if pending == "" {
					pending = mediaOnlyEventText(pendingEvent)
				}
			}
			if pending != "" {
				runLog().Debug(fmt.Sprintf("Processing queued message after agent completion: '%s...'", headRunes(pending, 40)))
			}
		}
	}
	pendingEvent, pending = r.mergeLeftoverSteer(result, adapter, source, sessionKey, pendingEvent, pending)
	if pending != "" && strings.HasPrefix(pystr.Strip(pending), "/") {
		cmdWord := pystr.Lower(strings.TrimPrefix(pystr.SplitWhitespace(pystr.Strip(pending), 1)[0], "/"))
		if cmdWord != "" {
			if _, known := ResolveCommand(cmdWord); known {
				runLog().Info(fmt.Sprintf("Discarding command '/%s' from pending queue — commands must not be passed as agent input", cmdWord))
				pendingEvent = nil
				pending = ""
			}
		}
	}
	if r.draining && (pendingEvent == nil || !pendingEvent.AdmittedBeforeDrain()) {
		if pendingEvent != nil || pending != "" {
			key := sessionKey
			if key == "" {
				key = "?"
			}
			runLog().Info(fmt.Sprintf("Discarding pending follow-up for session %s during gateway %s", key, r.statusActionLabel()))
		}
		pendingEvent = nil
		if len(result) > 0 && adapter != nil && sessionKey != "" {
			pendingEvent = r.takeWakeAdmittedBeforeDrain(sessionKey, adapter)
		}
		pending = ""
		if pendingEvent != nil {
			pending = pendingEvent.Text
			if pending == "" {
				pending = mediaOnlyEventText(pendingEvent)
			}
		}
	}
	return pendingEvent, pending
}
