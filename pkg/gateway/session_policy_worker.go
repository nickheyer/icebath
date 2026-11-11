package gateway

import (
	"context"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/plugins"
)

// Stored private policy of a bootstrap frame, the storage_unavailable refusal of an incomplete one
func RestorePolicy(raw any) (*LocalSessionPolicy, error) { return restorePolicy(raw) }

// Runs body under the frozen policy with no owner authority, the managed worker child's scope
func WorkerPolicyScope(ctx context.Context, policy *LocalSessionPolicy, body func(ctx context.Context) error) error {
	return policyScope(ctx, policy, nil, body)
}

// Committed attachment media urls and types of a bootstrap frame, nil for a text only frame
func RestoreAttachments(ctx context.Context, frame *pyjson.Object) ([]string, []string, error) {
	return restoreAttachments(ctx, frame)
}

// Registers the session's shell hooks and outbound webhooks from the frozen config in the worker, nothing for a bypass session
func RegisterWorkerHooks(ctx context.Context, policy *LocalSessionPolicy) error {
	if policy.IgnoreUserConfig {
		return nil
	}
	kanbanText := "null"
	if policy.KanbanJSON != nil && *policy.KanbanJSON != "" {
		kanbanText = *policy.KanbanJSON
	}
	kanbanValue, err := pyjson.Loads(kanbanText)
	if err != nil {
		return err
	}
	kanban := pyjson.NewObject()
	if pyrepr.Truthy(kanbanValue) {
		obj, isDict := kanbanValue.(*pyjson.Object)
		if !isDict {
			return &PyError{Kind: "AttributeError", Msg: "'" + pyTypeName(kanbanValue) + "' object has no attribute 'get'"}
		}
		kanban = obj
	}
	requestValue, err := pyjson.Loads(policy.RequestJSON)
	if err != nil {
		return err
	}
	request, isDict := requestValue.(*pyjson.Object)
	if !isDict {
		return &PyError{Kind: "AttributeError", Msg: "'" + pyTypeName(requestValue) + "' object has no attribute 'get'"}
	}
	accept := request.GetOr("accept_hooks", nil) == true || kanban.GetOr("accept_hooks", nil) == true
	cfg, err := policy.config(ctx, nil, true)
	if err != nil {
		return err
	}
	if _, err := plugins.RegisterShellHooksFromConfig(ctx, cfg, accept, false); err != nil {
		return err
	}
	_, err = plugins.RegisterOutboundWebhooksFromConfig(ctx, cfg)
	return err
}
