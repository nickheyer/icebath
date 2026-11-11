package botdesktop

import (
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestTimeoutKillsThePackageManagersWholeProcessGroup(t *testing.T) {
	isolatedInstallHost(t)
	swapInstall(t, &sudoNopasswd, func() bool { return true })
	swapInstall(t, &installExec, fakeSudoExec("bash", "-c", "sleep 30 >/dev/null 2>&1 & echo child $!; wait"))
	var lines []string
	code, err := runInstall("sudo apt-get install -y x", func() (string, error) { return "", nil }, func(l string) { lines = append(lines, l) }, 0.5)
	if err != nil || code == 0 {
		t.Fatalf("code %d err %v", code, err)
	}
	child := installChildPid(t, lines)
	gone := func() bool {
		raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", child))
		if err != nil {
			return true
		}
		fields := strings.Fields(string(raw)[strings.LastIndex(string(raw), ")")+1:])
		return len(fields) == 0 || strings.Contains(fields[0], "Z")
	}
	for range 50 {
		if gone() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	exec.Command("kill", "-9", strconv.Itoa(child)).Run()
	t.Fatal("grandchild survived the install timeout")
}

func TestTimeoutReturnsAndFreesTheSlotEvenWhenADescendantSurvives(t *testing.T) {
	env := isolatedInstallHost(t)
	swapInstall(t, &sudoNopasswd, func() bool { return true })
	swapInstall(t, &termGraceSeconds, 0.2)
	swapInstall(t, &installExec, fakeSudoExec("sh", "-c", "setsid sleep 30 & echo child $!; wait"))
	lines := &installLines{}
	started := time.Now()
	done := make(chan struct{})
	go func() {
		defer close(done)
		code, err := InstallPackagesWith(env, func() (string, error) { return "", nil }, lines.add, 0.5, false)
		if err != nil {
			lines.add("error " + err.Error())
			return
		}
		lines.add("code " + strconv.Itoa(code))
	}()
	finished := false
	select {
	case <-done:
		finished = true
	case <-time.After(3 * time.Second):
	}
	got := lines.snapshot()
	for _, line := range got {
		if strings.HasPrefix(line, "child ") {
			exec.Command("kill", "-9", strings.Fields(line)[1]).Run()
			break
		}
	}
	if !finished {
		t.Fatalf("InstallPackages hung %.1fs on a surviving descendant", time.Since(started).Seconds())
	}
	if !slices.ContainsFunc(got, func(l string) bool { return strings.HasPrefix(l, "code ") && l != "code 0" }) {
		t.Fatalf("lines %q", got)
	}
	if !slices.ContainsFunc(got, func(l string) bool { return strings.Contains(l, "timed out") }) {
		t.Fatalf("lines %q", got)
	}
	key, err := Claim(env)
	if err != nil {
		t.Fatalf("slot not free: %v", err)
	}
	Release(key)
}
