package gatewayrun

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Hands the answer to the detached update process atomically, the OSError text on failure
func hmWriteUpdateResponse(ctx context.Context, responseText string) *string {
	h := home.Dir(ctx)
	responsePath := filepath.Join(h, ".update_response")
	failed := func(err error) *string {
		text := pypath.OSErrorText(err)
		return &text
	}
	tmp := responsePath + ".tmp"
	if err := os.WriteFile(tmp, []byte(responseText), 0o644); err != nil {
		return failed(err)
	}
	if err := os.Rename(tmp, responsePath); err != nil {
		return failed(err)
	}
	if err := os.Remove(filepath.Join(h, ".update_prompt.json")); err != nil && !os.IsNotExist(err) {
		return failed(err)
	}
	return nil
}

// Consumes a reply to a pending update prompt, nil when nothing was consumed
func (r *GatewayRunner) hmUpdatePromptReply(ctx context.Context, event *gateway.MessageEvent, quickKey string) *string {
	upState := r.peekSessionState(quickKey)
	if upState == nil {
		return nil
	}
	upState.mu.Lock()
	pendingNow := upState.persistent.updatePromptPending
	upState.mu.Unlock()
	if !pendingNow {
		return nil
	}
	cmd, _ := event.GetCommand()
	recognized := ""
	responseText := ""
	switch cmd {
	case "approve", "yes":
		responseText = "y"
	case "deny", "no":
		responseText = "n"
	default:
		if cmd != "" && ResolveCommand != nil {
			if name, ok := ResolveCommand(cmd); ok {
				recognized = name
			}
		}
		if recognized == "" {
			responseText = pystr.Strip(event.Text)
		}
	}
	if responseText == "" && recognized == "" {
		return nil
	}
	claimed := false
	upState.mu.Lock()
	if upState.persistent.updatePromptPending {
		upState.persistent.updatePromptPending, claimed = false, true
	}
	upState.mu.Unlock()
	if !claimed {
		return nil
	}
	if responseText != "" {
		if err := hmWriteUpdateResponse(ctx, responseText); err != nil {
			upState.mu.Lock()
			upState.persistent.updatePromptPending = true
			upState.mu.Unlock()
			runLog().Warn(fmt.Sprintf("Failed to write update response: %s", *err))
			reply := i18n.T(ctx, "gateway.update.reply_failed", "", map[string]any{"error": *err})
			return &reply
		}
		label := responseText
		if runes := []rune(responseText); len(runes) > 20 {
			label = string(runes[:20]) + "…"
		}
		reply := i18n.T(ctx, "gateway.update.reply_sent", "", map[string]any{"label": label})
		return &reply
	}
	if recognized != "" {
		if err := hmWriteUpdateResponse(ctx, ""); err == nil {
			runLog().Info(fmt.Sprintf("Recognized /%s during pending update prompt for %s; cancelled prompt with default and dispatching command", recognized, quickKey))
		} else {
			runLog().Warn(fmt.Sprintf("Failed to write cancel response for pending update prompt: %s", *err))
		}
	}
	return nil
}

// Intercepts a reply to a pending clarify prompt, nil when the message falls through, empty once handled
func (r *GatewayRunner) hmClarifyReply(ctx context.Context, event *gateway.MessageEvent, source *gateway.SessionSource, quickKey string) (*string, error) {
	pending := tools.GetPendingClarifyForSession(quickKey, true)
	if pending == nil {
		return nil, nil
	}
	hasAudio := len(pendingEventAudioPaths(event)) > 0
	raw := r.prepareClarifyReplyText(ctx, event)
	handled := ""
	retain := func(why string) *string {
		runLog().Info(fmt.Sprintf("Gateway retained pending clarify after %s (session=%s, id=%s)", why, quickKey, pending.ClarifyID))
		return &handled
	}
	if hasAudio && raw == "" {
		return retain("voice transcription produced no usable text"), nil
	}
	if raw == "" || strings.HasPrefix(raw, "/") {
		return nil, nil
	}
	switch tools.AttemptClarifyTextResponse(quickKey, raw) {
	case tools.ClarifyTextResolved:
		runLog().Info(fmt.Sprintf("Gateway intercepted clarify text response (session=%s, id=%s)", quickKey, pending.ClarifyID))
		adapter, err := r.deliveryAdapterFor(ctx, source)
		if err != nil {
			return nil, err
		}
		if adapter != nil {
			adapter.Base().ResumeTypingForChat(source.ChatID)
			if retirer, ok := adapter.(clarifyCardRetirer); ok {
				answer := raw
				if response := pending.Response(); response != nil && *response != tools.ClarifySkipped && *response != "" {
					answer = *response
				}
				if err := retirer.RetireClarifyCard(ctx, pending.ClarifyID, i18n.T(ctx, "gateway.clarify.answered", "", map[string]any{"response": answer})); err != nil {
					runLog().Debug("Failed to retire clarify card after typed answer", "error", err)
				}
			}
		}
		return &handled, nil
	case tools.ClarifyTextRejectedSelection:
		return retain("invalid selection attempt"), nil
	case tools.ClarifyTextRejectedProse:
		if tools.ResolveGatewayClarify(pending.ClarifyID, tools.ClarifyCancelled) {
			adapter, err := r.deliveryAdapterFor(ctx, source)
			if err != nil {
				return nil, err
			}
			if retirer, ok := adapter.(clarifyCardRetirer); ok {
				if err := retirer.RetireClarifyCard(ctx, pending.ClarifyID, i18n.T(ctx, "gateway.clarify.cancelled", "", nil)); err != nil {
					runLog().Debug("Failed to retire clarify card after prose cancellation", "error", err)
				}
			}
		}
	}
	return nil, nil
}

// Typed command spellings of a slash confirm answer, winning over the free text spellings
var slashConfirmCmdChoices = map[string]string{
	"approve": "once", "yes": "once", "ok": "once", "confirm": "once",
	"always": "always", "remember": "always",
	"cancel": "cancel", "no": "cancel", "deny": "cancel", "nevermind": "cancel",
}

// Free text spellings of each slash confirm answer, English and the active language
func slashConfirmTextChoices(ctx context.Context) map[string]string {
	choices := map[string]string{}
	for _, pair := range [][2]string{{"confirm_once", "once"}, {"confirm_always", "always"}, {"confirm_cancel", "cancel"}} {
		for _, word := range approvalInputWords(ctx, pair[0]) {
			if _, seen := choices[word]; !seen {
				choices[word] = pair[1]
			}
		}
	}
	return choices
}

// Resolves a reply to a pending slash confirm prompt, nil when it falls through
func (r *GatewayRunner) hmSlashConfirmReply(ctx context.Context, event *gateway.MessageEvent, quickKey string) *string {
	confirmID, pending := SlashConfirmGetPending(quickKey)
	if !pending {
		return nil
	}
	if tools.HasBlockingApproval(quickKey) {
		return nil
	}
	normReply := pystr.Lower(pystr.LstripChars(pystr.Strip(event.Text), "!/"))
	cmd, _ := event.GetCommand()
	choice, ok := slashConfirmCmdChoices[cmd]
	if !ok {
		choice, ok = slashConfirmTextChoices(ctx)[normReply]
	}
	if ok {
		resolved := SlashConfirmResolve(ctx, quickKey, confirmID, choice)
		return &resolved
	}
	SlashConfirmClearIfStale(quickKey)
	return nil
}

// Reply owned by in flight work, the update prompt then clarify then slash confirm, nil when none answers
func (r *GatewayRunner) hmPendingReplyIntercepts(ctx context.Context, event *gateway.MessageEvent, source *gateway.SessionSource, quickKey string) (*string, error) {
	if fifoOnly, _ := event.FIFOOnly(); !event.AllowGatewayControl || fifoOnly || gateway.ExecutingAdmission(ctx) {
		return nil, nil
	}
	reply := r.hmUpdatePromptReply(ctx, event, quickKey)
	if reply == nil {
		var err error
		if reply, err = r.hmClarifyReply(ctx, event, source, quickKey); err != nil {
			return nil, err
		}
	}
	if reply == nil {
		reply = r.hmSlashConfirmReply(ctx, event, quickKey)
	}
	return reply, nil
}
