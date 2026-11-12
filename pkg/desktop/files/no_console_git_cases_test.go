package files

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
	"time"
)

var gitArgs = []string{"-c", "windows.appendAtomically=false", "merge-base", "--is-ancestor", "origin/main", "HEAD"}

const windowsGitBin = `C:\Program Files\Git\cmd\git.exe`

// Helper modes of the test binary standing in for git, a quick answer or a hanging tree owning a listener
func runHelper(mode string) int {
	switch mode {
	case "ok":
		fmt.Print("ok")
		return 0
	case "hang":
		child := exec.Command(os.Args[0], os.Args[1], strconv.Itoa(os.Getpid()))
		child.Env = append(os.Environ(), "ICEBATH_FILES_HELPER=listen")
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		if err := child.Start(); err != nil {
			return 3
		}
		for {
			time.Sleep(time.Hour)
		}
	case "listen":
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return 4
		}
		parent, _ := strconv.Atoi(os.Args[2])
		body, _ := json.Marshal(map[string]any{"pids": []int{parent, os.Getpid()}, "port": ln.Addr().(*net.TCPAddr).Port})
		os.WriteFile(os.Args[1]+".tmp", body, 0o644)
		os.Rename(os.Args[1]+".tmp", os.Args[1])
		for {
			c, err := ln.Accept()
			if err == nil {
				c.Close()
			}
		}
	}
	return 2
}

type hangTree struct {
	pids string
}

func newHangTree(t *testing.T) hangTree {
	tree := hangTree{pids: filepath.Join(t.TempDir(), "pids.json")}
	t.Cleanup(func() {
		raw, err := os.ReadFile(tree.pids)
		if err != nil {
			return
		}
		var rec struct{ Pids []int }
		json.Unmarshal(raw, &rec)
		for _, pid := range rec.Pids {
			if p, err := os.FindProcess(pid); err == nil {
				p.Kill()
			}
		}
	})
	return tree
}

func (h hangTree) port(t *testing.T) int {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if raw, err := os.ReadFile(h.pids); err == nil {
			var rec struct{ Port int }
			if json.Unmarshal(raw, &rec) == nil {
				return rec.Port
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("tree never listened")
	return 0
}

func listening(port int) bool {
	c, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), 2*time.Second)
	if err != nil {
		var ne net.Error
		return errors.As(err, &ne) && ne.Timeout()
	}
	c.Close()
	return true
}

func waitClosed(t *testing.T, port int) {
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !listening(port) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("tree still listening")
}

func helperEnv(mode string) []string { return append(os.Environ(), "ICEBATH_FILES_HELPER="+mode) }

func TestATimedOutGitCommandReapsDescendantsIncludingTheWindowsPythonHost(t *testing.T) {
	tree := newHangTree(t)
	ok, err := ExecGit(os.Args[0], nil, "", helperEnv("ok"), 5*time.Second)
	if err != nil || ok.Code != 0 || ok.Stdout != "ok" {
		t.Fatal(ok, err)
	}
	_, err = ExecGit(os.Args[0], []string{tree.pids}, "", helperEnv("hang"), 5*time.Second)
	port := tree.port(t)
	waitClosed(t, port)
	var timeout *GitTimeoutError
	if !errors.As(err, &timeout) || timeout.Code() != "ETIMEDOUT" {
		t.Fatal(err)
	}
}

func TestAppQuitKillsATimedGitCommandStillRunningDescendantsIncluded(t *testing.T) {
	tree := newHangTree(t)
	done := make(chan struct{})
	go func() {
		ExecGit(os.Args[0], []string{tree.pids}, "", helperEnv("hang"), 60*time.Second)
		close(done)
	}()
	port := tree.port(t)
	if !listening(port) {
		t.Fatal("tree not listening")
	}
	KillTimedGitChildren()
	waitClosed(t, port)
	<-done
}

func TestWindowsGitSpawnUsesACREATENOWINDOWHostAndDoesNotRewriteGitArgv(t *testing.T) {
	plan := PlanNoConsoleGitSpawn(windowsGitBin, gitArgs, true, []string{"GIT_TERMINAL_PROMPT=0", `PATH=C:\Windows`})
	if plan.Command != windowsGitBin || !reflect.DeepEqual(plan.Args, gitArgs) || !plan.WindowsHide ||
		plan.CreationFlags != CreateNoWindow || CreateNoWindow != 0x08000000 ||
		!reflect.DeepEqual(plan.Env, []string{"GIT_TERMINAL_PROMPT=0", `PATH=C:\Windows`}) {
		t.Fatal(plan)
	}
}

func TestNonWindowsGitSpawnKeepsTheGitBinaryAndArgv(t *testing.T) {
	plan := PlanNoConsoleGitSpawn("/usr/bin/git", []string{"status", "--porcelain"}, false, nil)
	if plan.Command != "/usr/bin/git" || !reflect.DeepEqual(plan.Args, []string{"status", "--porcelain"}) || plan.CreationFlags != 0 {
		t.Fatal(plan)
	}
}

func TestMissingPythonDoesNotRewriteGitArgv(t *testing.T) {
	plan := PlanNoConsoleGitSpawn("git.exe", gitArgs, true, nil)
	if plan.Command != "git.exe" || !reflect.DeepEqual(plan.Args, gitArgs) {
		t.Fatal(plan)
	}
}

func TestHostScriptForwardsGitArgvUnchangedAndSetsCREATENOWINDOW(t *testing.T) {
	plan := PlanNoConsoleGitSpawn(windowsGitBin, gitArgs, true, nil)
	cmd := exec.Command(plan.Command, plan.Args...)
	if !reflect.DeepEqual(cmd.Args, append([]string{windowsGitBin}, gitArgs...)) || plan.CreationFlags != CreateNoWindow {
		t.Fatal(cmd.Args, plan.CreationFlags)
	}
}
