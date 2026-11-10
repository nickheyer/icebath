package selfupdate

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/pm"
)

// One real launch through the decision chain the boot runs, only PM's sync and currency answers replaced
func vsLaunchChild(ctx context.Context, original, args, rest []string) int {
	root, scratch, syncMode, launch := rest[0], rest[1], rest[2], rest[3]
	var command []string
	if err := json.Unmarshal([]byte(os.Getenv("LAUNCH_COMMAND")), &command); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	generations := filepath.Join(scratch, "generations-"+launch)
	f, err := os.OpenFile(generations, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o666)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	fmt.Fprintf(f, "%d\n", os.Getpid())
	f.Close()
	limit, _ := strconv.Atoi(os.Getenv("GENERATION_CAP"))
	if data, _ := os.ReadFile(generations); len(strings.Fields(string(data))) > limit {
		return 99
	}
	state := filepath.Join(scratch, "state")
	syncVenv = func(context.Context, pm.SyncVenvOptions) error {
		f, err := os.OpenFile(filepath.Join(scratch, "syncs"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o666)
		if err == nil {
			fmt.Fprintf(f, "%s\n", launch)
			f.Close()
		}
		if syncMode == "fail" {
			return runtimeErr("sync failed")
		}
		return os.WriteFile(state, []byte("current"), 0o666)
	}
	venvIsCurrent = func(context.Context, string) (bool, error) {
		data, _ := os.ReadFile(state)
		return syncMode == "restore" && string(data) == "current", nil
	}
	ensureToolsForSync = func(context.Context) error { return nil }
	resolveStorePython = func(string) string { exe, _ := os.Executable(); return exe }
	publishLaunchers = func(context.Context, string) error { return nil }
	restored := filepath.Join(scratch, "restored")
	if _, err := os.Stat(restored); syncMode == "restore" && Relaunched() && err != nil {
		os.WriteFile(restored, []byte("1"), 0o666)
		os.WriteFile(state, []byte("stale"), 0o666)
		return RelaunchAfterRestore(original)
	}
	binary, err := PrepareLaunch(ctx, root, command)
	if err == nil && binary != "" {
		_, err = relaunchExec(binary, RelaunchCommand(binary, args))
	}
	if err != nil {
		if !runtimeErrorClasses[launchErrorClass(err)] {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		fmt.Fprintf(os.Stderr, "degraded: %s\n", err)
	}
	fmt.Println("BOOTED")
	return 0
}
