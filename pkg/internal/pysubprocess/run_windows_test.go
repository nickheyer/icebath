package pysubprocess

import (
	"errors"
	"os"
	"testing"
	"time"
)

func TestRunSleepHelper(t *testing.T) {
	if os.Getenv("PYSUBPROCESS_SLEEP_HELPER") != "1" {
		t.Skip("helper process")
	}
	time.Sleep(5 * time.Second)
}

func TestRunFailsLikeCheckTrueOnWindows(t *testing.T) {
	out, _, err := Run([]string{"cmd", "/c", "echo hi"}, 30, true)
	if err != nil || string(out) != "hi\r\n" {
		t.Fatalf("%q %v", out, err)
	}
	_, _, err = Run([]string{"cmd", "/c", "exit 3"}, 30, true)
	var cpe *CalledProcessError
	if !errors.As(err, &cpe) || err.Error() != "Command '['cmd', '/c', 'exit 3']' returned non-zero exit status 3." {
		t.Fatalf("%v", err)
	}
	_, _, err = Run([]string{"cmd", "/c", "exit -1073741819"}, 30, true)
	if !errors.As(err, &cpe) || cpe.Returncode != 3221225477 || err.Error() != "Command '['cmd', '/c', 'exit -1073741819']' returned non-zero exit status 3221225477." {
		t.Fatalf("%v", err)
	}
	if _, _, err = Run([]string{"cmd", "/c", "exit 3"}, 30, false); err != nil {
		t.Fatal(err)
	}
	if text := (&CalledProcessError{Cmd: []string{"x"}, Returncode: -22}).Error(); text != "Command '['x']' died with <Signals.SIGABRT: 22>." {
		t.Fatal(text)
	}
	if text := (&CalledProcessError{Cmd: []string{"x"}, Returncode: -9}).Error(); text != "Command '['x']' died with unknown signal 9." {
		t.Fatal(text)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PYSUBPROCESS_SLEEP_HELPER", "1")
	argv := []string{exe, "-test.run=^TestRunSleepHelper$"}
	_, _, err = Run(argv, 1, true)
	var te *TimeoutExpired
	if !errors.As(err, &te) || te.Timeout != 1 {
		t.Fatalf("%v", err)
	}
}
