package gateway

import (
	"context"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Whether a platform has the credentials its core checker or plugin entry requires
func (c *GatewayConfig) IsPlatformConnected(ctx context.Context, platform Platform, cfg *PlatformConfig) bool {
	return c.isPlatformConnected(ctx, platform, cfg)
}

// Points the status command line reader at fn until the returned restore runs, for other packages' tests
func SwapProcessCmdlineReader(fn func(pid int) (string, bool)) (restore func()) {
	previous := statusReadProcessCmdline
	statusReadProcessCmdline = fn
	return func() { statusReadProcessCmdline = previous }
}

// Points the status pid existence probe at fn until the returned restore runs, for other packages' tests
func SwapPidExists(fn func(pid int) bool) (restore func()) {
	previous := statusPidExists
	statusPidExists = fn
	return func() { statusPidExists = previous }
}

// Points the multiplexer liveness probe at fn until the returned restore runs, for other packages' tests
func SwapMultiplexerLivenessForProfile(fn func(profileDir string) (int, *pyjson.Object, bool, error)) (restore func()) {
	previous := statusMultiplexerLivenessForProfile
	statusMultiplexerLivenessForProfile = fn
	return func() { statusMultiplexerLivenessForProfile = previous }
}

// Drops the cached pseudo member of a plugin platform value, the enum map pops of hermes test teardown
func ForgetPseudoPlatform(value string) {
	platformMembers.Lock()
	defer platformMembers.Unlock()
	delete(platformMembers.pseudo, value)
	if platformMembers.memberMap != nil {
		platformMembers.memberMap.Delete(Platform(value).Name())
	}
}

var bundledDashboardCards = struct {
	sync.Mutex
	byDir map[string]func(*PlatformEntry)
}{byDir: map[string]func(*PlatformEntry){}}

// Records the channels page card a bundled platform directory declares, called from the adapter package init
func RegisterBundledDashboardCard(directory string, apply func(*PlatformEntry)) {
	bundledDashboardCards.Lock()
	defer bundledDashboardCards.Unlock()
	bundledDashboardCards.byDir[directory] = apply
}

// Env value rules the dashboard card of a bundled platform declares, empty for no bundled plugin or no card
func BundledDashboardCardRules(directory string) map[string]EnvValueRule {
	out := map[string]EnvValueRule{}
	if names, _ := bundledPlatformScanCached(); !names[directory] {
		return out
	}
	bundledDashboardCards.Lock()
	apply := bundledDashboardCards.byDir[directory]
	bundledDashboardCards.Unlock()
	if apply == nil {
		return out
	}
	var card PlatformEntry
	apply(&card)
	for key, rule := range card.EnvValueRules {
		out[key] = rule
	}
	return out
}
