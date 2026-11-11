package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/mcp"
	"github.com/nickheyer/icebath/pkg/webhooksubs"
)

const (
	mcpEventMaxChallengeChars     = 512
	mcpEventMaxNoticeDetailChars  = 200
	mcpEventMaxEventDataChars     = 32_000
	mcpEventTruncatedNoteTemplate = "%s\n[truncated: the first %s of %s characters]"
)

// Logger of the MCP event route module
func webhookMCPEventsLog() *slog.Logger {
	return slog.Default().With("logger", "gateway.platforms.webhook_mcp_events")
}

// JSON response of an MCP event control envelope
func mcpEventJSON(body *pyjson.Object, status int) (*WebResponse, error) {
	return WebJSONResponse(body, status, nil)
}

// The mcp_event block of a route created by the MCP events tool, nil for any other route
func mcpEventBlock(routeConfig *pyjson.Object) *pyjson.Object {
	event, ok := routeConfig.GetOr("mcp_event", nil).(*pyjson.Object)
	if !ok {
		return nil
	}
	return event
}

// Decimal text of n with comma thousands separators
func mcpEventGrouped(n int) string {
	text := strconv.Itoa(n)
	var b strings.Builder
	for i, r := range text {
		if i > 0 && (len(text)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// Prompt of an MCP event occurrence whose template names the data field, false for any other route
func RenderMCPEventPrompt(adapter *WebhookAdapter, routeConfig *pyjson.Object, payload, eventType any, routeName string) (string, bool, error) {
	obj, isDict := payload.(*pyjson.Object)
	if mcpEventBlock(routeConfig) == nil || !isDict {
		return "", false, nil
	}
	template, ok := routeConfig.GetOr("prompt", nil).(string)
	if !ok || !strings.Contains(template, "{data}") {
		return "", false, nil
	}
	data, err := pyjson.Dumps(obj.GetOr("data", nil), pyjson.Indent(2), pyjson.EnsureASCII(false))
	if err != nil {
		return "", false, err
	}
	if length := utf8.RuneCountInString(data); length > mcpEventMaxEventDataChars {
		data = fmt.Sprintf(mcpEventTruncatedNoteTemplate, pySliceRunes(data, mcpEventMaxEventDataChars),
			mcpEventGrouped(mcpEventMaxEventDataChars), mcpEventGrouped(length))
	}
	parts := strings.Split(template, "{data}")
	rendered := make([]string, len(parts))
	for i, part := range parts {
		if part == "" {
			continue
		}
		text, err := adapter.RenderPrompt(part, payload, eventType, routeName)
		if err != nil {
			return "", false, err
		}
		rendered[i] = text
	}
	return strings.Join(rendered, data), true, nil
}

// Delivery id of an MCP event occurrence keyed by its event id, false when the payload names none
func MCPEventDeliveryID(routeConfig *pyjson.Object, payload any) (string, bool) {
	obj, isDict := payload.(*pyjson.Object)
	if mcpEventBlock(routeConfig) == nil || !isDict {
		return "", false
	}
	eventID, ok := obj.GetOr("eventId", nil).(string)
	if !ok || eventID == "" {
		return "", false
	}
	return "mcp-event:" + eventID, true
}

// Persists the replay cursor beside the route, never inside it
func mcpEventStoreCursor(ctx context.Context, routeName, cursor string) error {
	return mcp.StoreEventCursor(home.Dir(ctx), routeName, cursor)
}

// Removes an ended subscription route and its stored state
func mcpEventRemoveRoute(ctx context.Context, routeName string) error {
	h := home.Dir(ctx)
	if err := webhooksubs.Mutate(h, func(subs *pyjson.Object) error {
		subs.Pop(routeName)
		return nil
	}); err != nil {
		return err
	}
	return mcp.DropEventState(h, routeName)
}

// Echoes the server verification challenge, 400 for a missing or oversized one
func mcpEventVerification(ctx context.Context, adapter *WebhookAdapter, routeName string, routeConfig, payload *pyjson.Object, profile *string) (*WebResponse, error) {
	challenge, ok := payload.GetOr("challenge", nil).(string)
	if !ok || challenge == "" || utf8.RuneCountInString(challenge) > mcpEventMaxChallengeChars {
		return mcpEventJSON(pyjson.ObjectOf("error", "Invalid verification challenge"), http.StatusBadRequest)
	}
	webhookMCPEventsLog().Info(fmt.Sprintf("[webhook] MCP event route %s answered the server's verification challenge", routeName))
	return mcpEventJSON(pyjson.ObjectOf("challenge", challenge), http.StatusOK)
}

// Drops a route the server ended and notifies its delivery target when it has one
func mcpEventTerminated(ctx context.Context, adapter *WebhookAdapter, routeName string, routeConfig, payload *pyjson.Object, profile *string) (*WebResponse, error) {
	event := mcpEventBlock(routeConfig)
	if event == nil {
		event = pyjson.NewObject()
	}
	errorInfo, ok := payload.GetOr("error", nil).(*pyjson.Object)
	if !ok {
		errorInfo = pyjson.NewObject()
	}
	if err := mcpEventRemoveRoute(ctx, routeName); err != nil {
		return nil, err
	}
	webhookMCPEventsLog().Warn(fmt.Sprintf("[webhook] MCP server '%s' ended event subscription %s (%s): %s",
		pyrepr.Str(event.GetOr("server", nil)), routeName, pyrepr.Str(event.GetOr("name", nil)), pyrepr.Str(errorInfo)))
	if deliver := routeConfig.GetOr("deliver", "log"); !pyrepr.Equal(deliver, "log") {
		reason := errorInfo.GetOr("message", nil)
		if !pyrepr.Truthy(reason) {
			reason = "no reason given"
		}
		detail := pySliceRunes(pyrepr.Str(reason), mcpEventMaxNoticeDetailChars)
		notice := fmt.Sprintf("MCP server '%s' ended the subscription to '%s': %s (code %s). Subscribe again to keep receiving these events.",
			pyrepr.Str(event.GetOr("server", nil)), pyrepr.Str(event.GetOr("name", nil)), detail, pyrepr.Str(errorInfo.GetOr("code", nil)))
		if _, err := adapter.handleDeliverOnly(ctx, notice, payload, routeConfig, routeName, "terminated", "mcp-terminated:"+routeName, profile); err != nil {
			return nil, err
		}
	}
	return mcpEventJSON(pyjson.ObjectOf("status", "terminated"), http.StatusOK)
}

// Records the fresh cursor a gap envelope carries and acknowledges the skipped events
func mcpEventGap(ctx context.Context, adapter *WebhookAdapter, routeName string, routeConfig, payload *pyjson.Object, profile *string) (*WebResponse, error) {
	if cursor, ok := payload.GetOr("cursor", nil).(string); ok && cursor != "" {
		if err := mcpEventStoreCursor(ctx, routeName, cursor); err != nil {
			return nil, err
		}
	}
	webhookMCPEventsLog().Warn(fmt.Sprintf("[webhook] MCP event route %s: the server skipped events (gap); resuming from a fresh cursor", routeName))
	return mcpEventJSON(pyjson.ObjectOf("status", "gap"), http.StatusOK)
}

// Handlers of the control envelopes by their type
var mcpEventControlHandlers = map[string]func(context.Context, *WebhookAdapter, string, *pyjson.Object, *pyjson.Object, *string) (*WebResponse, error){
	"verification": mcpEventVerification,
	"terminated":   mcpEventTerminated,
	"gap":          mcpEventGap,
}

// Response of a control envelope on an MCP event route, nil letting the delivery continue as an occurrence
func HandleMCPEventControl(ctx context.Context, adapter *WebhookAdapter, routeName string, routeConfig *pyjson.Object, payload any, profile *string) (*WebResponse, error) {
	obj, isDict := payload.(*pyjson.Object)
	if mcpEventBlock(routeConfig) == nil || !isDict {
		return nil, nil
	}
	kind, found := obj.Get("type")
	kindText, isText := kind.(string)
	if isText {
		if handler, ok := mcpEventControlHandlers[kindText]; ok {
			return handler(ctx, adapter, routeName, routeConfig, obj, profile)
		}
	}
	if found {
		webhookMCPEventsLog().Info(fmt.Sprintf("[webhook] MCP event route %s: ignoring a control envelope of unknown type %s", routeName, pyrepr.Repr(kind)))
		var reported any
		if isText {
			reported = kindText
		}
		return mcpEventJSON(pyjson.ObjectOf("status", "ignored", "type", reported), http.StatusOK)
	}
	if cursor, ok := obj.GetOr("cursor", nil).(string); ok && cursor != "" {
		if err := mcpEventStoreCursor(ctx, routeName, cursor); err != nil {
			return nil, err
		}
	}
	return nil, nil
}
