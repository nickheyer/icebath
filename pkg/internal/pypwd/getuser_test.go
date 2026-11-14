package pypwd

import (
	"os"
	"runtime"
	"testing"
)

func TestGetuserPrefersEnvironmentInPythonOrder(t *testing.T) {
	for _, name := range []string{"LOGNAME", "USER", "LNAME", "USERNAME"} {
		t.Setenv(name, "")
	}
	t.Setenv("USERNAME", "winuser")
	t.Setenv("LNAME", "lname")
	if got, _ := Getuser(); got != "lname" {
		t.Fatalf("LNAME should win over USERNAME, got %q", got)
	}
	t.Setenv("USER", "user")
	if got, _ := Getuser(); got != "user" {
		t.Fatalf("USER should win over LNAME, got %q", got)
	}
	t.Setenv("LOGNAME", "logname")
	if got, _ := Getuser(); got != "logname" {
		t.Fatalf("LOGNAME should win, got %q", got)
	}
}

func TestGetuserFallsBackToThePasswordDatabase(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no password database")
	}
	for _, name := range []string{"LOGNAME", "USER", "LNAME", "USERNAME"} {
		t.Setenv(name, "")
	}
	pw, err := Getpwuid(os.Getuid())
	got, gerr := Getuser()
	if err != nil {
		if gerr == nil || gerr.Error() != "No username set in the environment" {
			t.Fatalf("missing passwd entry must raise the OSError text, got %q %v", got, gerr)
		}
		return
	}
	if gerr != nil || got != pw.Name {
		t.Fatalf("Getuser() = %q, %v want %q", got, gerr, pw.Name)
	}
}
