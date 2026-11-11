package gateway

import (
	"context"
	"errors"
	"strings"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Whether any inference provider is usable in the bound profile, assigned from cmd/icebath in wiring.go
var HasAnyProviderConfigured func(ctx context.Context, strictProfileScope bool) bool

// Runtime check answer for a resolve that raised, the error code riding along only for an unrepairable failure
func ReadinessFailure(err error) *pyjson.Object {
	out := pyjson.ObjectOf("ok", false, "error", err.Error())
	var local *provider.LocalRuntimeUnavailableError
	if errors.As(err, &local) {
		out.Set("error_code", local.ErrorCode())
	}
	return out
}

// Model the profile config pins, nil when none
func configuredModelOf(cfgModel any) *string {
	if section, ok := cfgModel.(map[string]any); ok {
		value := section["default"]
		if !pyrepr.Truthy(value) {
			value = section["model"]
		}
		if !pyrepr.Truthy(value) {
			return nil
		}
		if text := pyrepr.Str(value); text != "" {
			return &text
		}
		return nil
	}
	if text, ok := cfgModel.(string); ok && text != "" {
		return &text
	}
	return nil
}

// Readiness verdict of the runtime a session would be built with, a failure naming the provider the user pinned
func CheckRuntimeReadiness(ctx context.Context, requested string, strictProfileScope bool) (*pyjson.Object, error) {
	h := home.Dir(ctx)
	cfg := loadProfileConfig(ctx, h)
	cfgModel := cfg["model"]
	configured := configuredModelOf(cfgModel)
	target := ""
	if configured != nil {
		target = *configured
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
		case pyrepr.Truthy(entry["model"]):
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
		if section, ok := cfgModel.(map[string]any); ok && pyrepr.Truthy(section["provider"]) {
			pinned = strings.TrimSpace(pyrepr.Str(section["provider"]))
		}
	}
	blamed := pinned
	if blamed == "" {
		blamed = providerName
	}
	fail := func(message string, src any) *pyjson.Object {
		return pyjson.ObjectOf("ok", false, "provider", blamed, "model", model, "source", src, "error", message)
	}
	if !anyConfigured && providerName == "bedrock" && (source == "iam-role" || source == "aws-sdk-default-chain") {
		return fail("No Icebath provider is configured.", source), nil
	}
	callable := runtime.TokenSource != nil
	apiKeyText := ""
	if !callable {
		apiKeyText = strings.TrimSpace(runtime.APIKey)
	}
	if !(callable || apiKeyText == "aws-sdk" || apiKeyText == "no-key-required" || provider.HasUsableSecret(apiKeyText, 4) || runtime.Command != "") {
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
	return pyjson.ObjectOf("ok", true, "provider", resolvedProvider, "model", model, "source", resolvedSource,
		"free_tier_route", providerName == "nous" && provider.RouteIsWelcomeHost(runtime.BaseURL)), nil
}
