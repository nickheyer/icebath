package gateway

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
)

// Thread created for a handoff, false when the platform has none and the watcher uses the parent chat
func (b *BasePlatformAdapter) CreateHandoffThread(ctx context.Context, parentChatID, name string) (string, bool, error) {
	return "", false, nil
}

// Edit of a sent message, unsupported by default so callers send anew
func (b *BasePlatformAdapter) EditMessage(ctx context.Context, chatID, messageID, content string, finalize bool) (SendResult, error) {
	return SendResult{Success: false, Error: "Not supported"}, nil
}

// Adapter that can delete a sent message, the base reporting false
type MessageDeleter interface {
	DeleteMessage(ctx context.Context, chatID, messageID string) (bool, error)
}

// Deletion of a sent message, false on platforms without a deletion API so callers leave it
func (b *BasePlatformAdapter) DeleteMessage(ctx context.Context, chatID, messageID string) (bool, error) {
	return false, nil
}

// Default seconds before a system notice is auto deleted from display ephemeral system ttl, zero keeping it
func (b *BasePlatformAdapter) ephemeralSystemTTLDefault(ctx context.Context) int {
	return orDefault(func() (int, error) {
		n, err := pyint.Of(configSection(ctx, "display")["ephemeral_system_ttl"])
		if err != nil {
			return 0, err
		}
		if !n.IsInt64() {
			return 0, fmt.Errorf("OverflowError: int too large")
		}
		return int(n.Int64()), nil
	}, 0)
}

// Waits out an ephemeral reply's ttl, swapped by tests the way hermes tests patch asyncio.sleep
var ephemeralDeleteWait = sleepSeconds

// Deletes a sent message after its ttl in the background, failures logged at debug
func (b *BasePlatformAdapter) ScheduleEphemeralDelete(ctx context.Context, chatID, messageID string, ttlSeconds int) {
	self := b.Self()
	spawnAdapterTask(ctx, func(tctx context.Context) error {
		if ephemeralDeleteWait(tctx, float64(max(1, ttlSeconds))) != nil {
			return nil
		}
		if _, err := self.DeleteMessage(tctx, chatID, messageID); err != nil {
			slog.Debug(fmt.Sprintf("[%s] Ephemeral delete failed for %s/%s: %v", self.Name(), chatID, messageID, err))
		}
		return nil
	})
}
