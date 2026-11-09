package provider

import (
	"context"

	"github.com/nickheyer/icebath/pkg/tools"
)

// agent/secret_scope.py surface the tools package reads when it builds child environments and redacts output
type profileSecretSource struct{}

func (profileSecretSource) ManagedDotenvKeys() []string   { return ManagedDotenvKeys() }
func (profileSecretSource) SourceSuppliedNames() []string { return SourceSuppliedNames() }
func (profileSecretSource) BuildProfileSecretScope(profileHome string) map[string]string {
	return BuildProfileSecretScope(profileHome)
}
func (profileSecretSource) CurrentSecretScope(ctx context.Context) map[string]string {
	return CurrentSecretScope(ctx)
}

// ProfileSecretSource is the secret scope reader cmd/icebath installs as tools.ProfileSecrets
func ProfileSecretSource() tools.ProfileSecretSource { return profileSecretSource{} }

// BindProfileSecretScope binds the built secret scope of a profile home on ctx the way plugin_host bind sets it
func BindProfileSecretScope(ctx context.Context, h string) context.Context {
	return WithSecretScope(ctx, BuildProfileSecretScope(h), h)
}

// ToolRuntimeCredentials resolves the live base URL and api key of a requested provider for a tool model lookup
func ToolRuntimeCredentials(ctx context.Context, env *tools.Env, requested, targetModel string) (string, string, error) {
	h := ""
	if env != nil {
		h = env.Home
	}
	rt, err := ResolveRuntimeProvider(ctx, h, requested, targetModel, "", "")
	if err != nil {
		return "", "", err
	}
	if rt == nil {
		return "", "", nil
	}
	return rt.BaseURL, rt.APIKey, nil
}
