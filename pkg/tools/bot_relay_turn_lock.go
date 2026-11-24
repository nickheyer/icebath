package tools

import (
	"context"
	"fmt"
	"math"
	"path/filepath"
	"regexp"

	"github.com/nickheyer/icebath/pkg/settings"
)

const (
	BotRelayDirName           = "bot_relay"
	botRelayLocksDir          = "locks"
	botRelayTurnLockSleepMax  = 0.1
	botRelayTurnLockSleepMin  = 0.005
	botRelayTurnBusyReason    = "target_busy"
	botRelayTurnLockFileMode  = 0o600
	botRelayTurnLockSafeLimit = 64
)

var botRelayUnsafeProfileRE = regexp.MustCompile(`[^a-zA-Z0-9_-]`)

// A delivery turn already running for the target profile, waited roughly the given seconds
type TurnBusyError struct {
	Profile       string
	WaitedSeconds float64
}

func (e *TurnBusyError) Error() string {
	return fmt.Sprintf("target_busy: another delivery turn is already running for profile '%s' — "+
		"queued behind it for ~%ds without it finishing. The message was NOT delivered; retry shortly.", e.Profile, int64(math.RoundToEven(e.WaitedSeconds)))
}

// Python class name of the refusal
func (e *TurnBusyError) ErrorKind() string { return "TurnBusyError" }

// Structured refusal reason of the busy turn
func (e *TurnBusyError) Reason() string { return botRelayTurnBusyReason }

// Relay directory under the root
func BotRelayRoot(root string) string { return filepath.Join(root, BotRelayDirName) }

// Seconds a queued delivery turn waits for the busy one, never below zero
func TurnWaitSeconds(ctx context.Context) float64 {
	return math.Max(0, settings.BotModeTurnWaitSeconds.Get(ctx).Seconds())
}

// Per profile lockfile path, short enough for macOS temp roots
func TurnLockPath(root, profile string) string {
	safe := botRelayUnsafeProfileRE.ReplaceAllString(profile, "_")
	if runes := []rune(safe); len(runes) > botRelayTurnLockSafeLimit {
		safe = string(runes[:botRelayTurnLockSafeLimit])
	}
	if safe == "" {
		safe = "_"
	}
	return filepath.Join(BotRelayRoot(root), botRelayLocksDir, safe+".lock")
}

// Holds the profile's cross process turn lock until release, bounded by the configured or given wait
func AcquireTurnLock(ctx context.Context, root, profile string, timeoutSeconds *float64) (release func(), err error) {
	return acquireTurnLock(ctx, root, profile, timeoutSeconds)
}
