package files

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"time"
)

// CREATE_NO_WINDOW keeps a console child of the GUI process from opening a console window
const CreateNoWindow = 0x08000000

// GitSpawnPlan is how a hidden git command is started
type GitSpawnPlan struct {
	Command       string
	Args          []string
	Env           []string
	WindowsHide   bool
	CreationFlags uint32
}

// Hidden git spawn, git itself created with CREATE_NO_WINDOW on Windows and argv kept unchanged
func PlanNoConsoleGitSpawn(gitBin string, args []string, isWindows bool, env []string) GitSpawnPlan {
	command := gitBin
	if command == "" {
		command = "git"
	}
	plan := GitSpawnPlan{Command: command, Args: append([]string{}, args...), Env: append([]string{}, env...), WindowsHide: isWindows}
	if isWindows {
		plan.CreationFlags = CreateNoWindow
	}
	return plan
}

// GitResult is the exit code and captured output of one git command
type GitResult struct {
	Code   int
	Stdout string
	Stderr string
}

// GitTimeoutError is a git command killed at its deadline
type GitTimeoutError struct{ Stderr string }

func (e *GitTimeoutError) Error() string { return "git timed out" }

// Code reads as ETIMEDOUT like the Node error
func (e *GitTimeoutError) Code() string { return "ETIMEDOUT" }

// Timed git commands still running, killed with their descendants at quit
var timedGit = struct {
	sync.Mutex
	cmds map[*exec.Cmd]bool
}{cmds: map[*exec.Cmd]bool{}}

// Runs git hidden, a timed command leading its own process group so the deadline kills its whole tree
func ExecGit(gitBin string, args []string, cwd string, env []string, timeout time.Duration) (GitResult, error) {
	if env == nil {
		env = os.Environ()
	}
	plan := PlanNoConsoleGitSpawn(gitBin, args, runtime.GOOS == "windows", env)
	cmd := exec.Command(plan.Command, plan.Args...)
	cmd.Dir = cwd
	cmd.Env = plan.Env
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	configureGitProcess(cmd, plan, timeout > 0)
	if err := cmd.Start(); err != nil {
		return GitResult{}, err
	}
	if timeout > 0 {
		timedGit.Lock()
		timedGit.cmds[cmd] = true
		timedGit.Unlock()
		defer func() {
			timedGit.Lock()
			delete(timedGit.cmds, cmd)
			timedGit.Unlock()
		}()
	}
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	var deadline <-chan time.Time
	if timeout > 0 {
		t := time.NewTimer(timeout)
		defer t.Stop()
		deadline = t.C
	}
	select {
	case err := <-waited:
		return gitExit(cmd, err, stdout.String(), stderr.String())
	case <-deadline:
		killTree(cmd)
		select {
		case <-waited:
		case <-time.After(5 * time.Second):
		}
		return GitResult{}, &GitTimeoutError{Stderr: stderr.String()}
	}
}

func gitExit(cmd *exec.Cmd, err error, stdout, stderr string) (GitResult, error) {
	if err == nil {
		return GitResult{Code: 0, Stdout: stdout, Stderr: stderr}, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		code := exitErr.ExitCode()
		if code < 0 {
			code = 1
		}
		return GitResult{Code: code, Stdout: stdout, Stderr: stderr}, nil
	}
	return GitResult{}, err
}

// Kills every timed git command still running with its descendants, for app quit
func KillTimedGitChildren() {
	timedGit.Lock()
	live := make([]*exec.Cmd, 0, len(timedGit.cmds))
	for cmd := range timedGit.cmds {
		live = append(live, cmd)
	}
	timedGit.cmds = map[*exec.Cmd]bool{}
	timedGit.Unlock()
	killTrees(live)
}
