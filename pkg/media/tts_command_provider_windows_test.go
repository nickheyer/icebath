package media

import (
	"context"
	"os/exec"
	"testing"
)

func TestTTSCommandWindowsConsoleHidden(t *testing.T) {
	t.Run("test_popen_kwargs_hide_console_window_windows", func(t *testing.T) {
		got := captureCommandSpawn(t)
		res, err := runCommandProvider(context.Background(), nil, "echo hi", 5, nil)
		if err != nil || res.Returncode != 0 {
			t.Fatalf("run = %v, %v", res, err)
		}
		flags := got.cmd.SysProcAttr.CreationFlags
		if flags&0x08000000 == 0 || flags&0x00000200 == 0 {
			t.Fatalf("creationflags = %#x", flags)
		}
	})
	t.Run("test_idle_kill_taskkill_hides_console_windows", func(t *testing.T) {
		child := exec.Command("cmd", "/c", "ping -n 30 127.0.0.1 >NUL")
		if err := child.Start(); err != nil {
			t.Fatal(err)
		}
		p := startCommandWaiter(child)
		var flags []uint32
		saved := runTaskkill
		runTaskkill = func(cmd *exec.Cmd) error {
			flags = append(flags, cmd.SysProcAttr.CreationFlags)
			return saved(cmd)
		}
		t.Cleanup(func() { runTaskkill = saved })
		terminateCommandProcessTree(p)
		if len(flags) == 0 {
			t.Fatal("taskkill was not invoked")
		}
		if flags[0]&0x08000000 == 0 {
			t.Fatal("CREATE_NO_WINDOW missing on taskkill")
		}
	})
}
