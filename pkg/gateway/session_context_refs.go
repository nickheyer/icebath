package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Expanded text of one message and the verdict of its @ reference injection
type ContextExpansion struct {
	Message  string
	Warnings []string
	Expanded bool
	Blocked  bool
}

// Agent side @ reference preprocessing within a root, assigned by the process wiring
var ExpandContextReferences func(ctx context.Context, text, cwd string, contextLength int, allowedRoot string, plugins bool) (ContextExpansion, error)

// Text with its @ references expanded in cwd, or the typed text and the refusal of a refused injection
func expandReferences(ctx context.Context, text, cwd string, contextLength int, plugins bool) (string, *string) {
	if ExpandContextReferences == nil {
		panic("gateway.ExpandContextReferences unassigned: the process wiring installs the agent context reference preprocessor")
	}
	result, err := ExpandContextReferences(ctx, text, cwd, contextLength, cwd, plugins)
	if err != nil {
		slog.Warn(fmt.Sprintf("@ context reference expansion failed: %s", err))
		return text, nil
	}
	if result.Blocked {
		refusal := strings.Join(result.Warnings, "\n")
		if refusal == "" {
			refusal = i18n.T(ctx, "gateway.notify.context_injection_refused", "", nil)
		}
		return text, &refusal
	}
	if result.Expanded {
		return result.Message, nil
	}
	return text, nil
}

// Text of a frozen policy model field, empty when unset or falsy
func policyFieldText(value any) string {
	if !pyrepr.Truthy(value) {
		return ""
	}
	return pyrepr.Str(value)
}

// Managed worker's admitted text expanded in the session workspace with the frozen context length, nil text keeps the committed one
func managedTurnText(ctx context.Context, a *SessionAuthority, row *pyjson.Object, policy *LocalSessionPolicy) (*string, *string) {
	text, isText := rowPayload(row).GetOr("text", nil).(string)
	if !isText || !strings.Contains(text, "@") {
		return nil, nil
	}
	contextLength, err := managedContextLength(ctx, a, policy)
	if err != nil {
		slog.Warn(fmt.Sprintf("@ context reference expansion failed: %s", err))
		return &text, nil
	}
	expanded, refusal := expandReferences(ctx, text, policy.Cwd, contextLength, !policy.IgnoreUserConfig)
	return &expanded, refusal
}

// Context window of the frozen policy model, its pinned config length winning
func managedContextLength(ctx context.Context, a *SessionAuthority, policy *LocalSessionPolicy) (int, error) {
	cfg, err := policy.config(ctx, a, true)
	if err != nil {
		return 0, err
	}
	modelConfig := cfg["model"]
	if !pyrepr.Truthy(modelConfig) {
		modelConfig = map[string]any{}
	}
	pinned := 0
	if section, isDict := modelConfig.(map[string]any); isDict {
		if raw := section["context_length"]; raw != nil {
			if value, err := pyint.Int64(raw); err == nil {
				pinned = int(value)
			}
		}
	}
	model, err := policy.currentModel(ctx, a)
	if err != nil {
		return 0, err
	}
	name := ""
	if model != nil {
		name = *model
	}
	return provider.GetModelContextLength(ctx, provider.ContextLengthRequest{Model: name,
		BaseURL: policyFieldText(policy.BaseURL()), Provider: policyFieldText(policy.Provider()), ConfigContextLength: pinned}), nil
}

// Result of a turn refused at its references, a finite prompt failing with the refusal
func refusedReferencesResult(row *pyjson.Object, refusal string) *pyjson.Object {
	if finite, _ := rowPayload(row).Get("finite"); finite == true {
		return pyjson.ObjectOf("final_response", "", "messages", []any{}, "failed", true, "completed", false, "error", refusal)
	}
	return pyjson.ObjectOf("final_response", refusal, "messages", []any{})
}

// Failed never run turn naming the session's vanished working folder
func workspaceUnavailableResult(ctx context.Context, policy *LocalSessionPolicy) *pyjson.Object {
	text := i18n.T(ctx, "gateway.errors.workspace_unavailable", "", map[string]any{"path": policy.Cwd})
	result := pyjson.ObjectOf("final_response", text, "messages", []any{}, "api_calls", 0, "tools", []any{},
		"failed", true, "completed", false, "error", text)
	stampTurnFailure(result, "workspace_unavailable", false)
	return result
}
