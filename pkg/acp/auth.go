package acp

import (
	"context"
	"strings"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/provider"
	acputil "github.com/nickheyer/icebath/pkg/utils/acp"
)

const TerminalSetupAuthMethodID = "icebath-setup"

var resolveRuntimeProvider = func(ctx context.Context) (*provider.Runtime, error) {
	return provider.ResolveRuntimeProvider(ctx, home.Dir(ctx), "", "", "", "")
}

// Lowercased active runtime provider when it has a key or token source, empty otherwise
func DetectProvider(ctx context.Context) string {
	rt, err := resolveRuntimeProvider(ctx)
	if err != nil || rt == nil {
		return ""
	}
	p := strings.TrimSpace(rt.Provider)
	if p != "" && (strings.TrimSpace(rt.APIKey) != "" || rt.TokenSource != nil) {
		return strings.ToLower(p)
	}
	return ""
}

// Registry compatible auth methods, the detected provider first and terminal setup always
func BuildAuthMethods(ctx context.Context) []acputil.AuthMethod {
	var methods []acputil.AuthMethod
	if p := DetectProvider(ctx); p != "" {
		methods = append(methods, &acputil.AuthMethodAgent{
			ID: p, Name: p + " runtime credentials",
			Description: new("Authenticate Icebath using the currently configured " + p + " runtime credentials."),
		})
	}
	methods = append(methods, &acputil.TerminalAuthMethod{
		ID: TerminalSetupAuthMethodID, Name: "Configure Icebath provider", Args: []string{"--setup"},
		Description: new("Open Icebath's interactive model/provider setup in a terminal. " +
			"Use this when Icebath has not been configured on this machine yet."),
	})
	return methods
}
