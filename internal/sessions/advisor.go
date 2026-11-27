package sessions

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/nickheyer/icebath/internal/config"
	sessionsconfig "github.com/nickheyer/icebath/internal/sessions/config"
	"github.com/nickheyer/icebath/pkg/fsx"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
)

// How long a "later" answer holds the low disk question off
const snoozeFor = 7 * 24 * time.Hour

// File that holds when the low disk question may be asked again
func snoozeFile(cfg *config.Config) string {
	return filepath.Join(cfg.Dir(), ".auto_prune_prompt.json")
}

// Holds the low disk question off for a week
func Snooze(cfg *config.Config) error {
	data, err := json.Marshal(map[string]float64{"snoozed_until": float64(time.Now().Add(snoozeFor).UnixNano()) / 1e9})
	if err != nil {
		return err
	}
	path := snoozeFile(cfg)
	return fsx.Write(path, data, 0o600)
}

// Lets the low disk question be asked again
func Unsnooze(cfg *config.Config) {
	os.Remove(snoozeFile(cfg))
}

func snoozed(cfg *config.Config) bool {
	data, err := os.ReadFile(snoozeFile(cfg))
	if err != nil {
		return false
	}
	var record struct {
		Until float64 `json:"snoozed_until"`
	}
	data = []byte(strings.TrimPrefix(string(data), "\ufeff"))
	return json.Unmarshal(data, &record) == nil && float64(time.Now().UnixNano())/1e9 < record.Until
}

// Free and total bytes of the file system the path is on
func disk(path string) (int64, int64, bool) {
	var stat syscall.Statfs_t
	if syscall.Statfs(path, &stat) != nil {
		return 0, 0, false
	}
	return int64(stat.Bavail) * int64(stat.Bsize), int64(stat.Blocks) * int64(stat.Bsize), true
}

// Whether a disk is low: under 2 GiB free, or under a tenth of it free and under 20 GiB
func low(free, total int64) bool {
	return total > 0 && (free < 2<<30 || free*100/total < 10 && free < 20<<30)
}

// Size as advice shows it
func Size(n int64) string {
	if n >= 1<<30 {
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	}
	return fmt.Sprintf("%.0f MB", float64(n)/(1<<20))
}

func sessionCount(n int) string {
	if n == 1 {
		return "1 session"
	}
	return fmt.Sprintf("%d sessions", n)
}

// Where automatic pruning stands: on or off, and whether the person chose that or it is the default
func AutoPruneState(cfg *config.Config) (bool, string) {
	_, setting, _ := cfg.Setting(sessionsconfig.AutoPrune)
	source := "default"
	switch from, _ := cfg.Source(setting); from {
	case config.FromFile:
		source = "your setting"
	case config.FromEnvironment:
		source = "your environment"
	}
	return cfg.GetBool(sessionsconfig.AutoPrune), source
}

// Disk the store is on, as the status of automatic pruning tells it
func DiskLine(ctx context.Context, store Store) string {
	stats, err := store.Stats(ctx)
	if err != nil || stats.GetPath() == "" {
		return ""
	}
	free, total, ok := disk(filepath.Dir(stats.GetPath()))
	if !ok {
		return ""
	}
	line := fmt.Sprintf(" %s %s; %s free of %s", filepath.Base(stats.GetPath()), Size(stats.GetBytes()), Size(free), Size(total))
	if low(free, total) {
		line += " (low)"
	}
	return line + "."
}

// Lines of the low disk question, the commands written with the prefix, and whether it should be asked: the disk is low, automatic pruning
// was never chosen either way, and it was not put off
func Advice(ctx context.Context, store Store, cfg *config.Config, prefix string) ([]string, bool) {
	stats, err := store.Stats(ctx)
	if err != nil || stats.GetPath() == "" {
		return nil, false
	}
	free, total, ok := disk(filepath.Dir(stats.GetPath()))
	if _, source := AutoPruneState(cfg); !ok || !low(free, total) || source != "default" || snoozed(cfg) {
		return nil, false
	}
	days := cfg.GetInt(sessionsconfig.RetentionDays)
	if days < 0 {
		days = 90
	}
	lines := []string{
		fmt.Sprintf("Low disk space: %s free of %s. Your session history (%s) uses %s.", Size(free), Size(total), filepath.Base(stats.GetPath()), Size(stats.GetBytes())),
		fmt.Sprintf("Auto-prune is off. Turning it on deletes ended sessions inactive for more than %d days, once a day (pinned and open sessions are kept).", days),
	}
	cutoff := timestamppb.New(time.Now().Add(-time.Duration(days) * 24 * time.Hour))
	matches, err := store.Prune(ctx, &sessionsv1.PruneRequest{Filter: &sessionsv1.Filter{ActiveBefore: cutoff}, DryRun: true})
	switch {
	case err != nil:
		lines = append(lines, "Could not read the session database to estimate how much pruning would free.")
	default:
		var prunable int64
		for _, s := range matches.GetSessions() {
			prunable += s.GetMessages()
		}
		reclaim := int64(0)
		if stats.GetMessages() > 0 {
			reclaim = stats.GetBytes() * prunable / stats.GetMessages()
		}
		count := len(matches.GetSessions())
		switch {
		case stats.GetSessions() == 0:
			lines = append(lines, "Pruning will not help right now: there is no session history to delete. Something else is filling this disk.")
		case count == 0:
			lines = append(lines, fmt.Sprintf("Pruning will not help right now: none of your %s is older than %d days. Something else is filling this disk.",
				sessionCount(int(stats.GetSessions())), days))
		case reclaim < 1<<30 && low(free+reclaim, total):
			lines = append(lines, fmt.Sprintf("Pruning would remove only about %s of history (%s), which will not fix this disk on its own.", Size(reclaim),
				sessionCount(count)))
		default:
			lines = append(lines, fmt.Sprintf("Pruning would remove about %s of history (%d of %s).", Size(reclaim), count, sessionCount(int(stats.GetSessions()))))
		}
		if count > 0 {
			lines = append(lines, "Deleted history frees space inside the database; to give it back to the disk, run `icebath sessions optimize` with other "+
				"Icebath windows closed.")
		}
	}
	lines = append(lines, fmt.Sprintf("%sautoprune on to enable, %sautoprune off to keep everything (never asked again), %sautoprune later to be reminded in a week.",
		prefix, prefix, prefix))
	return lines, true
}
