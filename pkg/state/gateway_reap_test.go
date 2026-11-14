package state

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func TestIsEphemeralPortZeroBackend(t *testing.T) {
	cases := []struct {
		argv []string
		want bool
	}{
		{[]string{"icebath", "dashboard", "--port", "0"}, true},
		{[]string{"icebath", "-p", "work", "serve", "--port= 0 "}, true},
		{[]string{"icebath", "serve", "--port", "8080"}, false},
		{[]string{"icebath", "--port", "0"}, false},
		{[]string{"icebath", "serve", "--port"}, false},
	}
	for _, c := range cases {
		if got := isEphemeralPortZeroBackend(c.argv); got != c.want {
			t.Errorf("%v: got %v want %v", c.argv, got, c.want)
		}
	}
}

func TestReapSparesHoldersThatAreNotOrphanDesktopBackends(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("holder reaping exists only on Linux")
	}
	db := filepath.Join(t.TempDir(), "state.db")
	os.WriteFile(db, nil, 0o644)
	child := exec.Command("sleep", "30")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer child.Process.Kill()
	holders := []Holder{{PID: child.Process.Pid, Target: db}, {PID: os.Getpid(), Target: db + "-wal"}}
	reaped, err := reapInactiveOrphanDesktopHolders(db, holders, 0)
	if err != nil || len(reaped) != 0 {
		t.Fatalf("a parented non dashboard holder must be kept, reaped %v err %v", reaped, err)
	}
	if child.ProcessState != nil {
		t.Fatal("the holder was signalled")
	}
}
