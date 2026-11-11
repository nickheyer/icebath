package gatewayrun

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/nickheyer/icebath/pkg/botmode"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/media"
	"github.com/nickheyer/icebath/pkg/mediacache"
	"github.com/nickheyer/icebath/pkg/tools"
)

func housekeepingDiskLog() *slog.Logger {
	return slog.Default().With("logger", "gateway.run_housekeeping_disk")
}

// Hourly prune of every media cache past 24 hours, spillover kept for the session retention window
func housekeepingMediaCaches(ctx context.Context) {
	h := home.Dir(ctx)
	env := &tools.Env{Home: h, TerminalScope: tools.GetTerminalScope(ctx)}
	maxAge := float64(media.MediaCacheMaxAgeHours)
	cache := func(fn func(context.Context, string, float64) (int, error)) func() (int, error) {
		return func() (int, error) { return fn(ctx, h, maxAge) }
	}
	caches := []struct {
		name    string
		cleanup func() (int, error)
	}{
		{"Image", cache(mediacache.CleanupImageCache)},
		{"Document", cache(mediacache.CleanupDocumentCache)},
		{"Audio", cache(mediacache.CleanupAudioCache)},
		{"Video", cache(mediacache.CleanupVideoCache)},
		{"Screenshot", cache(mediacache.CleanupScreenshotCache)},
		{"Spillover", func() (int, error) { return tools.CleanupSpilloverCache(env, -1), nil }},
		{"Terminal temp", func() (int, error) { return tools.CleanupTerminalTempCache(env, maxAge), nil }},
		{"Bot DM", func() (int, error) { return botmode.CleanupBotDMCache(maxAge, nil), nil }},
		{"Bot relay", func() (int, error) { return botmode.CleanupBotRelayArtifacts(), nil }},
	}
	for _, c := range caches {
		housekeepingChore(c.name+" cache cleanup", func() error {
			removed, err := c.cleanup()
			if err != nil {
				return err
			}
			if removed > 0 {
				housekeepingDiskLog().Info(fmt.Sprintf("%s cache cleanup: removed %d stale file(s)", c.name, removed))
			}
			return nil
		})
	}
}
