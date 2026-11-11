package tools

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"path/filepath"
	"regexp"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
)

const (
	BotRelayDirName           = "bot_relay"
	botRelayLocksDir          = "locks"
	TurnWaitSecondsFallback   = 120
	botRelayTurnLockSleepMax  = 0.1
	botRelayTurnLockSleepMin  = 0.005
	botRelayTurnBusyReason    = "target_busy"
	botRelayTurnLockFileMode  = 0o600
	botRelayTurnLockSafeLimit = 64
)

var botRelayUnsafeProfileRE = regexp.MustCompile(`[^a-zA-Z0-9_-]`)

func botRelayLog() *slog.Logger { return slog.Default().With("logger", "tools.bot_relay") }

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
func (e *TurnBusyError) PyTypeName() string { return "TurnBusyError" }

// Structured refusal reason of the busy turn
func (e *TurnBusyError) Reason() string { return botRelayTurnBusyReason }

// Relay directory under the root
func BotRelayRoot(root string) string { return filepath.Join(root, BotRelayDirName) }

// Bot mode config value of the key, nil when absent or the config is unreadable
func botModeCfg(ctx context.Context, key string) any {
	h := home.Dir(ctx)
	cfg, err := config.LoadContext(ctx, h, config.NewSecrets(h).Lookup())
	if err != nil {
		botRelayLog().Debug(fmt.Sprintf("bot_mode.%s config read failed", key), "exc_info", err)
		return nil
	}
	botMode, _ := cfg["bot_mode"].(map[string]any)
	return botMode[key]
}

// Wait budget of a queued delivery turn from config
func TurnWaitSeconds(ctx context.Context) (float64, error) {
	value := botModeCfg(ctx, "turn_wait_seconds")
	if value == nil {
		return TurnWaitSecondsFallback, nil
	}
	f, err := pyint.Float(value)
	if err != nil {
		return 0, err
	}
	return math.Max(0, f), nil
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
