package mcp

import (
	"context"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Terminal UI that writes to stdout and never prompts
func HeadlessUI() TerminalUI { return headlessUI{} }

// One server probe filling the prompt, resource and schema size details
func ProbeSingleServerDetails(ctx context.Context, env *tools.Env, name string, cfg map[string]any, details *ProbeDetails) ([]ToolSummary, error) {
	return probeServer(ctx, env, name, cfg, nil, details)
}

// Whether a usable OAuth token for the server is stored in the home of ctx
func OAuthTokensPresent(ctx context.Context, name string) bool { return oauthTokensPresent(ctx, name) }

// Probe or display text with every credential header value and auth scheme span replaced
func RedactProbeText(text string) string { return redactProbeText(text) }
