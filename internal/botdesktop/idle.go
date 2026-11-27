package botdesktop

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"strconv"
	"time"

	bdconfig "github.com/nickheyer/icebath/internal/botdesktop/config"
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/terminal"
)

// First argument this binary takes to run one idle check of a screen, as its launcher does between sleeps
const IdleEntry = "__icebath_bot_desktop_idle_watch__"

// Shortest wait between two idle checks
const minCheck = 30 * time.Second

// Whole seconds until the screen could next be due, a default window when the idle stop is off
func nextCheck(limit, unused time.Duration) int {
	if limit <= 0 {
		return bdconfig.DefaultIdleStop * 60
	}
	next := limit
	if unused < limit {
		next = limit - unused
	}
	return int(math.Ceil(max(min(minCheck, limit), next).Seconds()))
}

// Command the launcher runs for one idle check of the profile's screen, the launcher's pid appended
func idleCommand(cfg *config.Config) ([]string, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return []string{exe, IdleEntry, cfg.Profile()}, nil
}

// One idle check of the profile and launcher the arguments name, in a session of its own: the screen stopped when due, else the seconds
// until the next check printed
func (d *Desktop) Idle(ctx context.Context, args []string, out io.Writer) error {
	detach()
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)).With("component", "idle-watch"))
	if len(args) < 1 {
		return errors.New("the idle check names no profile")
	}
	cfg, err := d.config.Of(args[0])
	if err != nil {
		return err
	}
	var of *launcher
	if len(args) > 1 {
		if pid, err := strconv.Atoi(args[1]); err == nil && pid > 0 {
			born, ok := terminal.StartTime(pid)
			if !ok {
				born = -1
			}
			of = &launcher{pid, born}
		}
	}
	stopped, err := d.StopIfIdle(ctx, cfg, of)
	if err != nil || stopped {
		return err
	}
	unused, _ := idle(cfg)
	delay := nextCheck(idleStop(cfg), unused)
	slog.Info(fmt.Sprintf("screen kept; next idle check in %ds", delay))
	fmt.Fprintln(out, delay)
	return nil
}
