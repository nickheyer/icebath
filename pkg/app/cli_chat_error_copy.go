package app

import (
	"context"
	"errors"
	"fmt"
	"reflect"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
)

const summaryLimit = 120

// Failover reason to the catalog key suffix of its copy, reasons sharing copy pointing at one key
var reasonKeys = map[string]string{
	"auth": "auth", "auth_permanent": "auth", "billing": "billing", "model_not_found": "model_not_found",
	"rate_limit": "rate_limit", "upstream_rate_limit": "rate_limit", "upstream_blocked": "upstream_blocked",
	"overloaded": "overloaded", "server_error": "server_error", "timeout": "timeout"}

const unknownReasonKey = "unknown"

// Text of an error or string value as str would render it
func errorText(v any) string {
	switch e := v.(type) {
	case nil:
		return ""
	case error:
		return e.Error()
	case string:
		return e
	}
	return fmt.Sprint(v)
}

// The value as an error, a string wrapped like Exception(str(value))
func asError(v any) error {
	if e, ok := v.(error); ok {
		return e
	}
	return errors.New(errorText(v))
}

// First line of the text capped at limit cells with an ellipsis, a nil limit keeping the whole line
func shortText(text string, limit *int) string {
	stripped := pystr.Strip(text)
	first := ""
	if stripped != "" {
		first = pystr.Splitlines(stripped, false)[0]
	}
	if limit == nil || pystr.Len(first) <= *limit {
		return first
	}
	return pystr.Rstrip(pystr.Slice(first, 0, *limit-1)) + "…"
}

// Text of a truthy value, empty for None, empty strings, zero and false
func truthyText(v any) string {
	if !cliTruthy(v) {
		return ""
	}
	return cliStr(v, "")
}

// The refusal as the client shows it, the reason code plus the field and detail the owner named
func refusalText(ctx context.Context, err any) string {
	text := errorText(err)
	if text == "launch_credentials_unavailable" {
		return agent.LaunchCredentialsCopy(ctx, err)
	}
	var data map[string]any
	if e, ok := err.(error); ok {
		var rpc *gateway.GatewayRPCError
		if errors.As(e, &rpc) {
			data = rpc.Data
		}
	}
	field := pystr.Strip(truthyText(data["field"]))
	detail := pystr.Strip(truthyText(data["detail"]))
	if field == "" && detail == "" {
		return text
	}
	if field == "" {
		field = "unnamed field"
	}
	if detail != "" {
		return text + " (" + field + ": " + detail + ")"
	}
	return text + " (" + field + ")"
}

// Two line panel text, the plain sentence with the fix command then a Details line with the raw error
func chatErrorResponse(ctx context.Context, err any, providerName, model, failureReason string) string {
	reason := pystr.Strip(failureReason)
	if reason != "" {
		text := pystr.Strip(errorText(err))
		if agent.SiteFailureCodes[reason] && text != "" {
			return text
		}
	} else {
		reason = string(provider.ClassifyError(provider.ClassifyInput{Err: asError(err), Provider: providerName, Model: model}).Reason)
	}
	key, ok := reasonKeys[reason]
	if !ok {
		key = unknownReasonKey
	}
	providerLabel := providerName
	if providerLabel == "" {
		providerLabel = i18n.T(ctx, "cli.error.the_provider", "", nil)
	}
	modelLabel := model
	if modelLabel == "" {
		modelLabel = i18n.T(ctx, "cli.error.the_current_model", "", nil)
	}
	lead := i18n.T(ctx, "cli.error."+key, "", map[string]any{"provider": providerLabel, "model": modelLabel})
	limit := 300
	return i18n.T(ctx, "cli.error.with_details", "", map[string]any{"lead": lead, "details": shortText(refusalText(ctx, err), &limit)})
}

// Type name of the error value without its pointer
func errorTypeName(err error) string {
	t := reflect.TypeOf(err)
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t.Name()
}

// Copy for a failed agent build on the first message, an unreachable gateway keeping its whole first line
func agentInitFailureMessage(err error) string {
	ctx := context.Background()
	limit := new(int)
	*limit = summaryLimit
	var unavailable *gateway.GatewayUnavailableError
	if errors.As(err, &unavailable) {
		limit = nil
	}
	detail := shortText(refusalText(ctx, err), limit)
	if detail == "" {
		detail = errorTypeName(err)
	}
	return i18n.T(ctx, "cli.error.agent_init_failed", "", map[string]any{"error": detail})
}
