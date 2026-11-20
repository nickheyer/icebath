package agent

import (
	"context"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Gateway form of the @ reference preprocessing within an allowed root, plugin kinds only when asked
func ExpandGatewayContextReferences(ctx context.Context, text, cwd string, contextLength int, allowedRoot string, plugins bool) (gateway.ContextExpansion, error) {
	root := allowedRoot
	result, err := PreprocessContextReferences(ctx, text, ContextReferenceOptions{Cwd: cwd, ContextLength: contextLength, AllowedRoot: &root, NoPlugins: !plugins})
	if err != nil {
		return gateway.ContextExpansion{}, err
	}
	return gateway.ContextExpansion{Message: result.Message, Warnings: result.Warnings, Expanded: result.Expanded, Blocked: result.Blocked}, nil
}
