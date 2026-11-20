package updaterecord

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

func lkPyBool(value bool) string {
	if value {
		return "True"
	}
	return "False"
}

func lkTouch(path string) {
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		panic(err)
	}
}

func lkStdinLine() { _, _ = bufio.NewReader(os.Stdin).ReadString('\n') }

// Child stand-ins for the holders, readers and claimants the update lock tests race and pause
func lockChild(mode string, args []string) (int, bool) {
	ctx := context.Background()
	switch mode {
	case "lock_hold":
		lock := NewUpdateLock(args[0], args[1], true)
		if ok, err := lock.Acquire(ctx); !ok || err != nil {
			panic(fmt.Sprint("hold refused ", err))
		}
		pcPrint("held")
		time.Sleep(120 * time.Second)
	case "lock_middle":
		cmd := pcCommand("lock_leaf", args...)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		_ = cmd.Run()
		return cmd.ProcessState.ExitCode(), true
	case "lock_leaf":
		lock := NewUpdateLock(args[0], "", true)
		ok, err := lock.Acquire(ctx)
		if err != nil {
			panic(err)
		}
		if !ok {
			pcPrint(fmt.Sprintf("REFUSED %d", lock.Holder.PID))
			return 2, true
		}
		if lock.Acquired {
			pcPrint("OWNED")
			return 1, true
		}
		pcPrint("ADOPTED")
	case "lock_claimant":
		checkoutFirst, install := true, ""
		if len(args) > 2 {
			install, checkoutFirst = args[2], false
		}
		lock := NewUpdateLock(args[0], install, checkoutFirst)
		if ok, err := lock.Acquire(ctx); !ok || err != nil {
			panic(fmt.Sprint("claim refused ", err))
		}
		lkTouch(args[1])
		time.Sleep(60 * time.Second)
	case "lock_mutex_holder":
		_, release, err := MarkerMutex(ctx, args[0], MutexWaitSeconds*time.Second)
		if err != nil {
			panic(err)
		}
		lkTouch(args[1])
		time.Sleep(1500 * time.Millisecond)
		release()
	case "lock_orphan_update":
		base := args[0]
		pcWaitFile(filepath.Join(base, "delegate-written"))
		lock := NewUpdateLock(filepath.Join(base, "marker"), "", true)
		if ok, err := lock.Acquire(ctx); !ok || err != nil {
			panic(fmt.Sprintf("%v %+v", err, lock.Holder))
		}
		lkTouch(filepath.Join(base, "ready"))
		pcWaitFile(filepath.Join(base, "go"))
		lock.Release(ctx)
	case "lock_checkout_holder":
		if refused, err := acquireCheckout(args[0]); refused != nil || err != nil {
			panic(fmt.Sprintf("%v %+v", err, refused))
		}
		if len(args) > 1 {
			if err := os.WriteFile(args[1], []byte("held"), 0o644); err != nil {
				panic(err)
			}
			time.Sleep(120 * time.Second)
			return 0, true
		}
		pcPrint("held")
		lkStdinLine()
	case "lock_probe_paused":
		paused := false
		hook := func(point string) {
			if point == "shared-probe" && !paused {
				paused = true
				pcPrint("probing")
				lkStdinLine()
			}
		}
		pausePoint.Store(&hook)
		held, err := CheckoutLockHeld(ctx, args[0])
		if err != nil {
			panic(err)
		}
		if !paused {
			pcPrint("unprobed")
			return 3, true
		}
		pcPrint(lkPyBool(held))
	case "lock_contender":
		lock := NewUpdateLock(args[0], args[1], true)
		ok, err := lock.Acquire(ctx)
		if err != nil {
			panic(err)
		}
		out, _ := json.Marshal(map[string]bool{"acquired": ok})
		pcPrint(string(out))
		lock.Release(ctx)
	case "lock_exec_child":
		lock := NewUpdateLock(filepath.Join(args[0], "child-marker"), args[0], true)
		if ok, err := lock.Acquire(ctx); !ok || err != nil {
			panic(fmt.Sprintf("%v %+v", err, lock.Holder))
		}
		lock.Release(ctx)
		pcPrint("JOINED")
		lkStdinLine()
	case "lock_wait_file":
		pcWaitFile(args[0])
	default:
		return lockChildPOSIX(ctx, mode, args)
	}
	return 0, true
}
