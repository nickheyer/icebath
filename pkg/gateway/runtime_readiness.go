package gateway

import (
	"context"
	"errors"
	"strings"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/settings"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Whether any inference provider is usable in the bound profile, assigned from cmd/icebath in wiring.go
var HasAnyProviderConfigured func(ctx context.Context, strictProfileScope bool) bool

// Runtime check answer for a resolve that raised, the error code riding along only for an unrepairable failure
func ReadinessFailure(err error) *jsonutil.Object {
	out := jsonutil.ObjectOf("ok", false, "error", err.Error())
	var local *provider.LocalRuntimeUnavailableError
	if errors.As(err, &local) {
		out.Set("error_code", local.ErrorCode())
	}
	return out
}

// Readiness verdict of the runtime a session would be built with, a failure naming the provider the user pinned
func CheckRuntimeReadiness(ctx context.Context, requested string, strictProfileScope bool) (*jsonutil.Object, error) {
	h := home.Dir(ctx)
	sctx := profileEnvScope(ctx, h)
	target := strings.TrimSpace(settings.Model.Get(sctx))
	var configured *string
	if target != "" {
		configured = &target
	}
	var runtime *provider.Runtime
	var model any
	if requested != "" {
		resolved, err := provider.ResolveRuntimeProvider(ctx, h, requested, target, "", "")
		if err != nil {
			return nil, err
		}
		runtime = resolved
		if configured != nil {
			model = *configured
		}
	} else {
		resolved, entry, err := provider.ResolveRuntimeWithFallback(ctx, h, "", target, "", "")
		if err != nil {
			return nil, err
		}
		runtime = resolved
		switch {
		case anyutil.IsTrue(entry["model"]):
			model = entry["model"]
		case configured != nil:
			model = *configured
		case runtime.Model != "":
			model = runtime.Model
		}
	}
	anyConfigured := HasAnyProviderConfigured(ctx, strictProfileScope)
	providerName := runtime.Provider
	if providerName == "" {
		providerName = "provider"
	}
	source := runtime.Source
	pinned := requested
	if pinned == "" {
		pinned = strings.TrimSpace(settings.ModelProvider.Get(sctx))
	}
	blamed := pinned
	if blamed == "" {
		blamed = providerName
	}
	fail := func(message string, src any) *jsonutil.Object {
		return jsonutil.ObjectOf("ok", false, "provider", blamed, "model", model, "source", src, "error", message)
	}
	if !anyConfigured && providerName == "bedrock" && (source == "iam-role" || source == "aws-sdk-default-chain") {
		return fail("No Icebath provider is configured.", source), nil
	}
	if !provider.RuntimeHasCredentials(runtime) {
		var src any
		if runtime.Source != "" {
			src = runtime.Source
		}
		return fail("No usable credentials found for "+blamed+"."+provider.MissingCredentialsSuffix(ctx, blamed), src), nil
	}
	var resolvedProvider, resolvedSource any
	if runtime.Provider != "" {
		resolvedProvider = runtime.Provider
	}
	if runtime.Source != "" {
		resolvedSource = runtime.Source
	}
	return jsonutil.ObjectOf("ok", true, "provider", resolvedProvider, "model", model, "source", resolvedSource), nil
}
