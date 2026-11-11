package gatewayrun

import (
	"sync"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
)

// Attributes only the adapter lifecycle, shutdown, watcher, topic and voice modules set
type runnerAdapterState struct {
	sessionAuthorityMu sync.Mutex
	sessionAuthority   *gateway.SessionAuthority

	botLoopGuard                   *gateway.BotLoopGuard
	warnedTelegramGroupUsersLegacy bool

	adapterClaimsMu            sync.Mutex
	secondaryHeldClaims        *pydict.Dict[secondaryKey, heldClaims]
	unservedSecondaryPlatforms *pydict.Dict[secondaryKey, struct{}]

	taskSetsMu         sync.Mutex
	supervisedWatchers map[*gatewayTask]struct{}

	maxSupervisedRestarts         int
	supervisedHealthySecs         int
	reconnectWatcherSlowRetrySecs float64
	maxSlowWatcherRespawns        int

	pluginRewireMu          sync.Mutex
	pluginRewireUnsubscribe map[string]func()

	lastSessionStorePruneTs float64
}

// Profile name and platform of one secondary adapter slot
type secondaryKey struct {
	profile  string
	platform gateway.Platform
}
