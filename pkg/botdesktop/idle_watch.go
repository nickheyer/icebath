package botdesktop

import (
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/tools"
)

const minDelayS = 30.0

// IdleWatchEntry is the argv entry the launcher loop re-executes this binary with for one idle check
const IdleWatchEntry = "__icebath_bot_desktop_idle_watch__"

// Whole seconds until the screen could next be due, a default window when the auto stop is off
func NextCheckIn(limit float64, idle *float64) int {
	if limit <= 0 {
		return DefaultIdleStopMinutes * 60
	}
	age := 0.0
	if idle != nil {
		age = *idle
	}
	next := limit
	if age < limit {
		next = limit - age
	}
	return int(math.Ceil(max(min(minDelayS, limit), next)))
}

// Launcher identity the check judges, a born time of minus one for a launcher already gone
func ownLauncher(argv []string) *launcherID {
	pidS := ""
	if len(argv) > 0 {
		pidS = argv[0]
	}
	if pidS == "" || strings.Trim(pidS, "0123456789") != "" {
		return nil
	}
	pid, err := strconv.Atoi(pidS)
	if err != nil {
		return nil
	}
	born, ok := createTime(pid)
	if !ok {
		born = -1.0
	}
	return &launcherID{pid, born}
}

// One idle check, stopping the screen when due and printing the next delay otherwise
func IdleWatchMain(env *tools.Env, argv []string, stdout io.Writer) error {
	stopped, err := StopIfIdle(env, ownLauncher(argv))
	if err != nil || stopped {
		return err
	}
	limit, err := IdleStopSeconds(env)
	if err != nil {
		return err
	}
	delay := NextCheckIn(limit, IdleSeconds(env))
	slog.Info(fmt.Sprintf("screen kept; next idle check in %ds", delay))
	fmt.Fprintln(stdout, delay)
	return nil
}

// Runs one idle check and exits when argv carries the entry, the check in a session of its own
func DispatchIdleWatchEntry() {
	args := os.Args[1:]
	if len(args) > 0 && strings.HasPrefix(args[0], pm.PinDefaultHomeFlag) {
		args = args[1:]
	}
	if len(args) == 0 || args[0] != IdleWatchEntry {
		return
	}
	detachSession()
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)).With("component", "idle-watch"))
	if err := IdleWatchMain(&tools.Env{}, args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

// Command the launcher loop runs for one idle check, bound to this installation
func idleCommand() []string {
	return pm.InstallationCommand(pm.RepoRoot(), nil, pm.LaunchCommandOptions{Subcommand: []string{IdleWatchEntry}})
}
