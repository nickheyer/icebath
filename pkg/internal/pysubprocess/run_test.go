package pysubprocess

import (
	"errors"
	"runtime"
	"testing"
)

func TestRunFailsLikeCheckTrue(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix shell")
	}
	out, _, err := Run([]string{"sh", "-c", "printf hi"}, 30, true)
	if err != nil || string(out) != "hi" {
		t.Fatalf("%q %v", out, err)
	}
	_, _, err = Run([]string{"sh", "-c", "printf oops >&2; exit 3"}, 30, true)
	var cpe *CalledProcessError
	if !errors.As(err, &cpe) || string(cpe.Stderr) != "oops" || err.Error() != "Command '['sh', '-c', 'printf oops >&2; exit 3']' returned non-zero exit status 3." {
		t.Fatalf("%v", err)
	}
	if _, _, err = Run([]string{"sh", "-c", "exit 3"}, 30, false); err != nil {
		t.Fatal(err)
	}
	_, _, err = Run([]string{"sh", "-c", "kill -9 $$"}, 30, true)
	if err == nil || err.Error() != "Command '['sh', '-c', 'kill -9 $$']' died with <Signals.SIGKILL: 9>." {
		t.Fatalf("%v", err)
	}
	_, _, err = Run([]string{"sleep", "5"}, 1, true)
	var te *TimeoutExpired
	if !errors.As(err, &te) || err.Error() != "Command '['sleep', '5']' timed out after 1 seconds" {
		t.Fatalf("%v", err)
	}
}
