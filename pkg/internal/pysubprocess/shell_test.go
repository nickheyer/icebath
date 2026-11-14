//go:build !windows

package pysubprocess

import (
	"errors"
	"os/exec"
	"testing"
)

func TestShellCommandRunsThroughBinSh(t *testing.T) {
	cmd, err := ShellCommand("echo $((1+2)); exit 7")
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Args[0] != "/bin/sh" || cmd.Args[1] != "-c" {
		t.Fatalf("argv %q", cmd.Args)
	}
	out, err := cmd.Output()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || Returncode(ee) != 7 || string(out) != "3\n" {
		t.Fatalf("out %q err %v", out, err)
	}
	cmd, _ = ShellCommand("kill -TERM $$")
	err = cmd.Run()
	if !errors.As(err, &ee) || Returncode(ee) != -15 {
		t.Fatalf("signal returncode from %v", err)
	}
}
