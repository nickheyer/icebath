package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Runner whose inbound turns expand @ references in the session workspace with the turn model context
type ContextRefsRunner interface {
	AuthorityRunner
	InboundModelContextLength(ctx context.Context, source *SessionSource, sessionKey string) (int, error)
	ExpandInboundContextReferences(ctx context.Context, source *SessionSource, sessionKey, text string) (*string, error)
}

// Workspace an inbound turn resolves references in, a local session policy cwd before the terminal cwd
func inboundCwd(ctx context.Context, runner AuthorityRunner, source *SessionSource) (string, error) {
	policy, err := policyForSource(ctx, runner, source)
	if err != nil {
		return "", err
	}
	if policy != nil {
		return policy.Cwd, nil
	}
	return tenv(ctx, "TERMINAL_CWD", pypath.Expanduser("~"))
}

// Inbound turn text with its references expanded in the session workspace, or the typed text and the refusal
func InboundReferences(ctx context.Context, runner ContextRefsRunner, source *SessionSource, sessionKey, text string) (string, *string) {
	cwd, err := inboundCwd(ctx, runner, source)
	contextLength := 0
	if err == nil {
		contextLength, err = runner.InboundModelContextLength(ctx, source, sessionKey)
	}
	if err != nil {
		slog.Warn(fmt.Sprintf("@ context reference expansion failed: %s", err))
		return text, nil
	}
	return expandReferences(ctx, text, cwd, contextLength, true)
}

// Turn text with the model read references expanded, nil when refused with the local refusal left on the event
func TurnReferences(ctx context.Context, runner ContextRefsRunner, event *MessageEvent, source *SessionSource, sessionKey, text string) (*string, error) {
	var messageID any
	if event.MessageID != nil {
		messageID = *event.MessageID
	}
	rawAPIText, _ := cliTurn(ctx, messageID).Get("api_text")
	apiText, hasAPIText := rawAPIText.(string)
	if rawAPIText == nil {
		hasAPIText = false
	}
	target := text
	if hasAPIText {
		target = apiText
	}
	if !strings.Contains(target, "@") {
		return &text, nil
	}
	var expanded string
	if source.Platform == PlatformLocal {
		out, refusal := InboundReferences(ctx, runner, source, sessionKey, target)
		if refusal != nil {
			event.SetGatewayTurnRefusal(*refusal)
			return nil, nil
		}
		expanded = out
	} else {
		out, err := runner.ExpandInboundContextReferences(ctx, source, sessionKey, target)
		if err != nil || out == nil {
			return nil, err
		}
		expanded = *out
	}
	if !hasAPIText {
		return &expanded, nil
	}
	rebindAPIText(ctx, messageID, expanded)
	head := pystr.Lstrip(apiText)
	tail := ""
	if strings.HasPrefix(expanded, head) {
		tail = expanded[len(head):]
	}
	if tail == "" {
		return &text, nil
	}
	stored := pystr.Strip(text + tail)
	return &stored, nil
}
