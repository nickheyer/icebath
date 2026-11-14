//go:build linux

package botdesktop

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestXvncNeverSendsTheHoldersClipboardToWatchers(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	tmp := t.TempDir()
	bindir := filepath.Join(tmp, "bin")
	if err := os.Mkdir(bindir, 0o755); err != nil {
		t.Fatal(err)
	}
	argvLog := filepath.Join(tmp, "xvnc-argv")
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(bindir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write("Xvnc", "#!/bin/sh\nprintf \"%s\\n\" \"$@\" > \""+argvLog+"\"\nexec sleep 3\n")
	for _, stub := range []string{"xdpyinfo", "setxkbmap", "xsetroot", "xset", "dbus-run-session", "xauth"} {
		write(stub, "#!/bin/sh\nexit 0\n")
	}
	for _, tool := range []string{"mkdir", "sed", "cat", "printf", "dirname", "bash", "sh", "rm", "ln", "touch", "chmod", "xauth", "od", "tr", "awk", "grep", "seq", "sleep", "kill"} {
		real, err := exec.LookPath(tool)
		if err != nil {
			continue
		}
		if _, err := os.Lstat(filepath.Join(bindir, tool)); err == nil {
			continue
		}
		if err := os.Symlink(real, filepath.Join(bindir, tool)); err != nil {
			t.Fatal(err)
		}
	}
	launcher := filepath.Join(tmp, "launcher.sh")
	if err := os.WriteFile(launcher, launcherScript, 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(filepath.Join(bindir, "bash"), launcher)
	cmd.Env = []string{
		"PATH=" + bindir, "HOME=" + tmp, "ICEBATH_BD_PROFILE=t", "ICEBATH_BD_DISPLAY_NUM=99",
		"ICEBATH_BD_SOCKET=" + filepath.Join(tmp, "rfb.sock"), "ICEBATH_BD_XAUTH=" + filepath.Join(tmp, "Xauthority"),
		"ICEBATH_BD_ENV_FILE=" + filepath.Join(tmp, "env"), "ICEBATH_BD_CONFIG_HOME=" + filepath.Join(tmp, "xdg"),
	}
	done := make(chan error, 1)
	var out []byte
	go func() {
		var err error
		out, err = cmd.CombinedOutput()
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("launcher failed: %v\n%s", err, out)
		}
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("launcher timed out")
	}
	raw, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatal(err)
	}
	argv := strings.Split(strings.TrimPrefix(string(raw), "\ufeff"), "\n")
	if !slices.Contains(argv, "-SendCutText=0") {
		t.Fatalf("argv %v lacks -SendCutText=0", argv)
	}
	for _, a := range argv {
		if strings.HasPrefix(a, "-AcceptCutText") {
			t.Fatalf("paste into the screen must keep working: %v", argv)
		}
	}
	i := slices.Index(argv, "-MaxCutText")
	if i < 0 || i+1 >= len(argv) {
		t.Fatalf("argv %v lacks -MaxCutText", argv)
	}
	if n, err := strconv.Atoi(argv[i+1]); err != nil || n != rfbMaxCutText {
		t.Fatalf("Xvnc cut text cap %q, filter cap %d", argv[i+1], rfbMaxCutText)
	}
	if _, err := os.Stat(filepath.Join(tmp, "rfb.sock")); err == nil {
		t.Fatal("stub never bound the socket yet it exists")
	}
}
