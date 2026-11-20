package acpclient

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/provider"
)

// Installs the Copilot CLI client builder the built in copilot-acp provider creates its clients through
func init() {
	provider.NewCopilotACPClient = func(ctx context.Context, clientKwargs map[string]any) (provider.CopilotACPChatClient, error) {
		return New(ctx, copilotProviderVendor(), optionsFromKwargs(clientKwargs))
	}
}

// Copilot CLI vendor of the copilot-acp provider, the alias vendor plus the CLI sign in hint
func copilotProviderVendor() Vendor {
	v := copilotVendor()
	hint := "Run `copilot login` to sign in to the GitHub Copilot CLI."
	v.AuthRequiredHint = &hint
	return v
}

// Constructor keywords of a client kwargs map, unset and empty values left for the vendor defaults
func optionsFromKwargs(kw map[string]any) Options {
	return Options{
		APIKey: kwargString(kw["api_key"]), BaseURL: kwargString(kw["base_url"]), DefaultHeaders: kwargHeaders(kw["default_headers"]),
		ACPCommand: kwargString(kw["acp_command"]), ACPArgs: kwargStrings(kw["acp_args"]), ACPCwd: kwargString(kw["acp_cwd"]),
		Command: kwargString(kw["command"]), Args: kwargStrings(kw["args"]),
	}
}

// Python str of a truthy kwarg, empty for None and empty values
func kwargString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	}
	return fmt.Sprint(v)
}

// String items of a list kwarg
func kwargStrings(v any) []string {
	switch t := v.(type) {
	case []string:
		return append([]string(nil), t...)
	case []any:
		out := make([]string, 0, len(t))
		for _, item := range t {
			out = append(out, kwargString(item))
		}
		return out
	}
	return nil
}

// String header map of a headers kwarg
func kwargHeaders(v any) map[string]string {
	switch t := v.(type) {
	case map[string]string:
		return t
	case map[string]any:
		out := make(map[string]string, len(t))
		for k, item := range t {
			out[k] = kwargString(item)
		}
		return out
	}
	return nil
}
