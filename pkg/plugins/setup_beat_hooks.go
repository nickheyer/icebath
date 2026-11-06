package plugins

import (
	"context"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Installs the onboarding catalog hooks the setup beat tool reads
func init() {
	tools.OnboardingPluginNames = func(env *tools.Env) []string { return onboardingNames(envContext(env)) }
	tools.CatalogInstalledDir = func(env *tools.Env, name string) string {
		return catalogInstalledDir(home.Dir(envContext(env)), name)
	}
	tools.CatalogPluginSkill = func(env *tools.Env, written string) string {
		name, ok, err := CatalogPluginSkill(envContext(env), written)
		if err != nil {
			panic(err)
		}
		if !ok {
			return ""
		}
		return name
	}
}

// Context bound to the tool env home, the process home when the env names none
func envContext(env *tools.Env) context.Context {
	ctx := context.Background()
	if env != nil && env.Home != "" {
		ctx = home.WithOverride(ctx, env.Home)
	}
	return ctx
}
