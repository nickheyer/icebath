package agent

import (
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

var _ tools.DelegateRouteAgent = (*AIAgent)(nil)

// Wires the delegate speed tier gates to the provider fast mode resolvers
func init() {
	tools.DelegateServiceTierHooks.ParseExactServiceTier = provider.ParseExactServiceTier
	tools.DelegateServiceTierHooks.IsStaticTier = func(tier string) bool { return provider.StaticTiers[tier] }
	tools.DelegateServiceTierHooks.ResolveFastModeOverrides = provider.ResolveFastModeOverrides
	tools.DelegateServiceTierHooks.UltrafastOffered = provider.UltrafastOffered
}

func (a *AIAgent) RequestOverrides() map[string]any     { return a.requestOverrides }
func (a *AIAgent) SetRequestOverrides(v map[string]any) { a.requestOverrides = v }
