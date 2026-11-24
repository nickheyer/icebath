package gateway

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/mcp"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	"github.com/nickheyer/icebath/pkg/webhooksubs"
)

const (
	mcpEventMaxChallengeChars     = 512
	mcpEventMaxNoticeDetailChars  = 200
	mcpEventMaxEventDataChars     = 32_000
	mcpEventTruncatedNoteTemplate = "%s\n[truncated: the first %s of %s characters]"
)

// JSON response of an MCP event control envelope
func mcpEventJSON(body *jsonutil.Object, status int) (*WebResponse, error) {
	return WebJSONResponse(body, status, nil)
}

// The mcp_event block of a route created by the MCP events tool, nil for any other route
func mcpEventBlock(routeConfig *jsonutil.Object) *jsonutil.Object {
	event, ok := routeConfig.ValueOr("mcp_event", nil).(*jsonutil.Object)
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
func RenderMCPEventPrompt(adapter *WebhookAdapter, routeConfig *jsonutil.Object, payload, eventType any, routeName string) (string, bool, error) {
	obj, isDict := payload.(*jsonutil.Object)
	if mcpEventBlock(routeConfig) == nil || !isDict {
		return "", false, nil
	}
	template, ok := routeConfig.ValueOr("prompt", nil).(string)
	if !ok || !strings.Contains(template, "{data}") {
		return "", false, nil
	}
	data, err := jsonutil.Encode(obj.ValueOr("data", nil), jsonutil.Indent(2))
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
func MCPEventDeliveryID(routeConfig *jsonutil.Object, payload any) (string, bool) {
	obj, isDict := payload.(*jsonutil.Object)
	if mcpEventBlock(routeConfig) == nil || !isDict {
		return "", false
	}
	eventID, ok := obj.ValueOr("eventId", nil).(string)
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
	if err := webhooksubs.Mutate(h, func(subs *jsonutil.Object) error {
		subs.Take(routeName)
		return nil
	}); err != nil {
		return err
	}
	return mcp.DropEventState(h, routeName)
}

// Echoes the server verification challenge, 400 for a missing or oversized one
func mcpEventVerification(ctx context.Context, adapter *WebhookAdapter, routeName string, routeConfig, payload *jsonutil.Object, profile *string) (*WebResponse, error) {
	challenge, ok := payload.ValueOr("challenge", nil).(string)
	if !ok || challenge == "" || utf8.RuneCountInString(challenge) > mcpEventMaxChallengeChars {
		return mcpEventJSON(jsonutil.ObjectOf("error", "Invalid verification challenge"), http.StatusBadRequest)
	}
	pkgLog().Info(fmt.Sprintf("[webhook] MCP event route %s answered the server's verification challenge", routeName))
	return mcpEventJSON(jsonutil.ObjectOf("challenge", challenge), http.StatusOK)
}

// Drops a route the server ended and notifies its delivery target when it has one
func mcpEventTerminated(ctx context.Context, adapter *WebhookAdapter, routeName string, routeConfig, payload *jsonutil.Object, profile *string) (*WebResponse, error) {
	event := mcpEventBlock(routeConfig)
	if event == nil {
		event = jsonutil.NewObject()
	}
	errorInfo, ok := payload.ValueOr("error", nil).(*jsonutil.Object)
	if !ok {
		errorInfo = jsonutil.NewObject()
	}
	if err := mcpEventRemoveRoute(ctx, routeName); err != nil {
		return nil, err
	}
	pkgLog().Warn(fmt.Sprintf("[webhook] MCP server '%s' ended event subscription %s (%s): %s",
		anyutil.String(event.ValueOr("server", nil)), routeName, anyutil.String(event.ValueOr("name", nil)), anyutil.String(errorInfo)))
	if deliver := routeConfig.ValueOr("deliver", "log"); !anyutil.Equal(deliver, "log") {
		reason := errorInfo.ValueOr("message", nil)
		if !anyutil.IsTrue(reason) {
			reason = "no reason given"
		}
		detail := pySliceRunes(anyutil.String(reason), mcpEventMaxNoticeDetailChars)
		notice := fmt.Sprintf("MCP server '%s' ended the subscription to '%s': %s (code %s). Subscribe again to keep receiving these events.",
			anyutil.String(event.ValueOr("server", nil)), anyutil.String(event.ValueOr("name", nil)), detail, anyutil.String(errorInfo.ValueOr("code", nil)))
		if _, err := adapter.handleDeliverOnly(ctx, notice, payload, routeConfig, routeName, "terminated", "mcp-terminated:"+routeName, profile); err != nil {
			return nil, err
		}
	}
	return mcpEventJSON(jsonutil.ObjectOf("status", "terminated"), http.StatusOK)
}

// Records the fresh cursor a gap envelope carries and acknowledges the skipped events
func mcpEventGap(ctx context.Context, adapter *WebhookAdapter, routeName string, routeConfig, payload *jsonutil.Object, profile *string) (*WebResponse, error) {
	if cursor, ok := payload.ValueOr("cursor", nil).(string); ok && cursor != "" {
		if err := mcpEventStoreCursor(ctx, routeName, cursor); err != nil {
			return nil, err
		}
	}
	pkgLog().Warn(fmt.Sprintf("[webhook] MCP event route %s: the server skipped events (gap); resuming from a fresh cursor", routeName))
	return mcpEventJSON(jsonutil.ObjectOf("status", "gap"), http.StatusOK)
}

// Handlers of the control envelopes by their type
var mcpEventControlHandlers = map[string]func(context.Context, *WebhookAdapter, string, *jsonutil.Object, *jsonutil.Object, *string) (*WebResponse, error){
	"verification": mcpEventVerification,
	"terminated":   mcpEventTerminated,
	"gap":          mcpEventGap,
}

// Response of a control envelope on an MCP event route, nil letting the delivery continue as an occurrence
func HandleMCPEventControl(ctx context.Context, adapter *WebhookAdapter, routeName string, routeConfig *jsonutil.Object, payload any, profile *string) (*WebResponse, error) {
	obj, isDict := payload.(*jsonutil.Object)
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
		pkgLog().Info(fmt.Sprintf("[webhook] MCP event route %s: ignoring a control envelope of unknown type %s", routeName, anyutil.Quote(kind)))
		var reported any
		if isText {
			reported = kindText
		}
		return mcpEventJSON(jsonutil.ObjectOf("status", "ignored", "type", reported), http.StatusOK)
	}
	if cursor, ok := obj.ValueOr("cursor", nil).(string); ok && cursor != "" {
		if err := mcpEventStoreCursor(ctx, routeName, cursor); err != nil {
			return nil, err
		}
	}
	return nil, nil
}
