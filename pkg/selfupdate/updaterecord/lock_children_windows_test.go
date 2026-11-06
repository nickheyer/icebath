package updaterecord

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"
	"syscall"
	"time"
)

const lkCreateNoWindow = 0x08000000

// Windows stand-ins for an owner that binds its child into the job and one whose child joins by lease
func lockChildPOSIX(ctx context.Context, mode string, args []string) (int, bool) {
	switch mode {
	case "lock_win_owner":
		lock := NewUpdateLock(args[1], args[0], true)
		if ok, err := lock.Acquire(ctx); !ok || err != nil {
			panic(fmt.Sprintf("%v %+v", err, lock.Holder))
		}
		child := pcCommand("lock_wait_file", args[1]+".never")
		child.SysProcAttr = &syscall.SysProcAttr{CreationFlags: lkCreateNoWindow}
		if err := child.Start(); err != nil {
			panic(err)
		}
		if err := BindChildToUpdateTree(child.Process.Pid); err != nil {
			panic(err)
		}
		pcPrint(fmt.Sprint(child.Process.Pid))
		time.Sleep(300 * time.Second)
	case "lock_win_unbound_owner":
		lock := NewUpdateLock(args[1], args[0], true)
		if ok, err := lock.Acquire(ctx); !ok || err != nil {
			panic(fmt.Sprintf("%v %+v", err, lock.Holder))
		}
		child := pcCommand("lock_win_joining_child", args...)
		stdout, _ := child.StdoutPipe()
		if err := child.Start(); err != nil {
			panic(err)
		}
		if line, _ := bufio.NewReader(stdout).ReadString('\n'); strings.TrimSpace(line) != "joined" {
			panic("the child never joined " + line)
		}
		pcPrint(fmt.Sprint(child.Process.Pid))
		_ = child.Wait()
	case "lock_win_joining_child":
		if ok, err := NewUpdateLock(args[1], args[0], false).AcquireCheckout(args[0]); !ok || err != nil {
			panic(fmt.Sprint("join refused ", err))
		}
		pcPrint("joined")
		time.Sleep(300 * time.Second)
	default:
		return 0, false
	}
	_ = os.Stdout.Sync()
	return 0, true
}

// Child stand-ins this package's tests start on Windows, the update lock holders and claimants
func pcChild(mode string, args []string) int {
	if code, ok := lockChild(mode, args); ok {
		return code
	}
	panic("unknown pause test child " + mode)
}
