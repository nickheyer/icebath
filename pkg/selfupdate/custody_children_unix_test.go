//go:build !windows

package selfupdate

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

// Closes every inherited descriptor naming a filesystem path, as os.closerange does for the build writers
func cuCloseInheritedFiles() {
	for fd := 3; fd < 1024; fd++ {
		if link, err := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", fd)); err == nil && strings.HasPrefix(link, "/") {
			_ = syscall.Close(fd)
		}
	}
}

func cuSeconds(text string) time.Duration {
	f, _ := strconv.ParseFloat(text, 64)
	return time.Duration(f * float64(time.Second))
}

// Writes this process's pid to path
func cuTouchPID(path string) {
	if err := os.WriteFile(path+".tmp", []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		panic(err)
	}
	if err := os.Rename(path+".tmp", path); err != nil {
		panic(err)
	}
}

func cuTouch(path string) {
	if path != "" {
		_ = os.WriteFile(path, nil, 0o644)
	}
}

// Acquires the update lock of repo with marker, exiting the child when it cannot
func cuOwnerLock(repo, marker string) *updaterecord.UpdateLock {
	lock := updaterecord.NewUpdateLock(marker, repo, true)
	if ok, err := lock.Acquire(context.Background()); !ok || err != nil {
		fmt.Fprintln(os.Stderr, "could not acquire the update lock", err)
		os.Exit(3)
	}
	return lock
}

// Child stand-ins for the updaters, nodes and build writers the custody tests kill
func cuChild(mode string, args []string) int {
	ctx := context.Background()
	switch mode {
	case "fetch-owner":
		if os.Getenv("ICEBATH_CUSTODY_TEST_NO_DEATHSIGNAL") == "1" {
			custodyDeathSignal = func() *syscall.SysProcAttr { return nil }
		}
		cuOwnerLock(args[0], args[1])
		_, err := RunGit(ctx, []string{"git"}, []string{"fetch", "--upload-pack=" + args[2], "origin"}, ChildOptions{Dir: args[0]})
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return 0
	case "contained-owner":
		cuOwnerLock(args[0], args[1])
		contained, err := ContainedCommand(ctx, args[2:], true, args[0])
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		cmd := exec.Command(contained.Argv[0], contained.Argv[1:]...)
		cmd.Stdout, cmd.Stderr, cmd.ExtraFiles, cmd.SysProcAttr = os.Stdout, os.Stderr, contained.ExtraFiles, contained.Attr
		_ = cmd.Run()
		return 0
	case "build-owner":
		cuOwnerLock(args[0], args[1])
		contained, err := ContainedCommand(ctx, args[2:], true, args[0])
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		cmd := exec.Command(contained.Argv[0], contained.Argv[1:]...)
		cmd.ExtraFiles, cmd.SysProcAttr = contained.ExtraFiles, contained.Attr
		_, err = pm.RunContained(cmd, "probe build", pm.ContainOptions{})
		if cerr := contained.Close(); cerr != nil {
			err = cerr
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return 0
	case "writer":
		if args[2] == "1" {
			_, _ = syscall.Setsid()
		}
		cuCloseInheritedFiles()
		time.Sleep(cuSeconds(args[1]))
		cuTouch(args[0])
		return 0
	case "heartbeat":
		_, _ = syscall.Setsid()
		cuCloseInheritedFiles()
		tmp := args[0] + ".tmp"
		for i := 0; i < 300; i++ {
			_ = os.WriteFile(tmp, []byte(fmt.Sprintf("%d %d", os.Getpid(), i)), 0o644)
			_ = os.Rename(tmp, args[0])
			time.Sleep(100 * time.Millisecond)
		}
		return 0
	case "node":
		code, _ := strconv.Atoi(args[0])
		writer := exec.Command(args[3], args[4:]...)
		if err := writer.Start(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		time.Sleep(cuSeconds(args[1]))
		cuTouch(args[2])
		return code
	case "ctrl-c-build":
		cleanup := cuSeconds(args[0])
		interrupts := make(chan os.Signal, 1)
		signal.Notify(interrupts, syscall.SIGINT)
		go func() {
			<-interrupts
			time.Sleep(cleanup)
			os.Exit(130)
		}()
		writer := exec.Command(args[2], args[3:]...)
		if err := writer.Start(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		cuTouch(args[1])
		for {
			time.Sleep(100 * time.Millisecond)
		}
	case "claim-holder":
		lock := updaterecord.NewUpdateLock("", "", false)
		if ok, err := lock.Acquire(ctx); !ok || err != nil || !lock.Acquired {
			fmt.Fprintln(os.Stderr, "the holder could not acquire", err)
			return 3
		}
		host := &exec.Cmd{Path: os.Args[0], Args: args, Env: append(os.Environ(), "ICEBATH_SELFUPDATE_TEST_HOST=1"), Stderr: os.Stderr}
		out, err := host.Output()
		fmt.Println(strings.TrimSpace(string(out)))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
		return 0
	case "claim-host":
		exe, err := os.Executable()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		probe := exec.Command(exe, cuChildEntry, "claim-probe")
		probe.Stderr = os.Stderr
		for _, kv := range os.Environ() {
			if !strings.HasPrefix(kv, "ICEBATH_SELFUPDATE_TEST_HOST=") {
				probe.Env = append(probe.Env, kv)
			}
		}
		out, _ := probe.Output()
		fmt.Println(strings.TrimSpace(string(out)))
		return 0
	case "claim-probe":
		lock := updaterecord.NewUpdateLock("", "", false)
		ok, _ := lock.Acquire(ctx)
		fmt.Println(cuPyBool(ok), cuPyBool(lock.Holder != nil))
		return 0
	case "pause-draining":
		stops := make(chan os.Signal, 1)
		signal.Notify(stops, syscall.SIGTERM)
		fmt.Println("up")
		<-stops
		fmt.Println("stopping")
		for !cuExists(args[0]) {
			time.Sleep(10 * time.Millisecond)
		}
		return 0
	case "planned-stop-updater":
		pm.SetCommandArgv(func(argv []string) []string { return argv })
		pid, _ := strconv.Atoi(args[0])
		token, err := updaterecord.RecordPause(ctx, map[string]any{"resume_needed": true, "profiles": map[string]any{"default": pid},
			"identities": map[string]any{strconv.Itoa(pid): updaterecord.PauseIdentity(pid)["ct"]}}, nil, nil)
		if err == nil {
			err = updaterecord.MarkStopRequested(ctx, token, []int{pid}, map[int]string{pid: args[1]})
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		if ok, err := writeUpdatePlannedStopMarker(filepath.Dir(args[1]), pid); !ok || err != nil {
			fmt.Fprintln(os.Stderr, "the planned stop request was not written", err)
			return 1
		}
		fmt.Println("requested")
		time.Sleep(120 * time.Second)
		return 0
	case "pause-recover":
		pm.SetCommandArgv(func(argv []string) []string { return argv })
		updaterecord.SetPauseGatewayResumer(func(ctx context.Context, token map[string]any, stdout io.Writer) error {
			names := []any{}
			for name := range token["profiles"].(map[string]any) {
				names = append(names, name)
			}
			sort.Slice(names, func(i, j int) bool { return names[i].(string) < names[j].(string) })
			fmt.Println("resume " + pyrepr.Repr(names))
			token["resume_needed"] = false
			return nil
		})
		if err := updaterecord.RecoverPauses(ctx, []string{"status"}); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return 0
	case "git-add-owner":
		cuOwnerLock(args[0], filepath.Join(args[0], "owner-marker"))
		if _, err := RunGit(ctx, []string{"git"}, []string{"add", "tracked"}, ChildOptions{Dir: args[0], Check: true}); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return 0
	case "stall":
		cuTouchPID(filepath.Join(args[0], args[1]))
		for !cuExists(filepath.Join(args[0], "go")) {
			time.Sleep(20 * time.Millisecond)
		}
		if len(args) > 2 {
			_, _ = io.Copy(os.Stdout, os.Stdin)
		}
		return 0
	}
	fmt.Fprintln(os.Stderr, "unknown selfupdate test child mode "+mode)
	return 2
}

func cuPyBool(b bool) string {
	if b {
		return "True"
	}
	return "False"
}
