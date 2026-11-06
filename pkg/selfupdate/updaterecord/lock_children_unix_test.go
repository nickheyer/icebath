//go:build !windows

package updaterecord

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Stops this child at the named marker protocol step, a scheduling pause the test resumes
func lkStopAt(step string) {
	hook := func(point string) {
		if point == step {
			pausePoint.Store(nil)
			_ = syscall.Kill(os.Getpid(), syscall.SIGSTOP)
		}
	}
	pausePoint.Store(&hook)
}

// Child stand-ins that stop themselves, start new sessions or hand descriptors on
func lockChildPOSIX(ctx context.Context, mode string, args []string) (int, bool) {
	switch mode {
	case "lock_stale_reader":
		lkStopAt("unlink")
		ReadLiveUpdate(ctx, args[0], "")
	case "lock_reclaim_paused_reader":
		lkStopAt("reclaim")
		pcPrint(lkPyBool(ReadLiveUpdate(ctx, args[0], args[1]) != nil))
	case "lock_handoff_owner":
		base := args[0]
		marker := filepath.Join(base, "marker")
		ct := ownCreateTime()
		if err := os.WriteFile(marker, []byte(fmt.Sprintf("%d\n%d\nct:%.3f\n", os.Getpid(), time.Now().Unix(), *ct)), 0o644); err != nil {
			panic(err)
		}
		child := pcCommand("lock_orphan_update", base)
		child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := child.Start(); err != nil {
			panic(err)
		}
		f, err := os.OpenFile(marker, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			panic(err)
		}
		fmt.Fprintf(f, "delegate:%d ct:%.3f\n", child.Process.Pid, *ProcessCreateTime(child.Process.Pid))
		f.Close()
		if err := os.WriteFile(filepath.Join(base, "delegate-written"), []byte(strconv.Itoa(child.Process.Pid)), 0o644); err != nil {
			panic(err)
		}
		_ = child.Wait()
	case "lock_orphaning_holder":
		if refused, err := acquireCheckout(args[0]); refused != nil || err != nil {
			panic(fmt.Sprintf("%v %+v", err, refused))
		}
		release := filepath.Join(filepath.Dir(args[1]), "release")
		files, err := CheckoutLockFiles(args[0])
		if err != nil {
			panic(err)
		}
		child := pcCommand("lock_wait_file", release)
		child.ExtraFiles = files
		if err := child.Start(); err != nil {
			panic(err)
		}
		if err := os.WriteFile(args[1], []byte(strconv.Itoa(child.Process.Pid)), 0o644); err != nil {
			panic(err)
		}
	case "lock_prober":
		seconds, _ := strconv.ParseFloat(args[1], 64)
		end := time.Now().Add(time.Duration(seconds * float64(time.Second)))
		lkTouch(args[2])
		n := 0
		for time.Now().Before(end) {
			fd, err := unix.Open(args[0], unix.O_RDONLY|unix.O_CLOEXEC, 0)
			if err == nil {
				if unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB) == nil {
					time.Sleep(100 * time.Microsecond)
					_ = unix.Flock(fd, unix.LOCK_UN)
				}
				_ = unix.Close(fd)
			}
			n++
			time.Sleep(time.Millisecond)
		}
		pcPrint(strconv.Itoa(n))
	default:
		return 0, false
	}
	return 0, true
}
