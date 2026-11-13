package codeexec

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// ChildPipe names the descriptors a child reads replies from and writes requests to, Windows handles when Handles is set
type ChildPipe struct {
	Replies  uintptr
	Requests uintptr
	Handles  bool
}

// ChildRun is one run of user code on an interpreter, its stdin fed and its pipe requests served by the caller
type ChildRun struct {
	Python  string
	Args    func(pipe ChildPipe) []string
	Stdin   string
	Env     map[string]string
	Timeout time.Duration
	Purpose string
	Serve   func(ctx context.Context, requests io.Reader, replies io.Writer)
}

// ChildResult is the return code and text mode output of one child run
type ChildResult struct {
	ReturnCode int
	Stdout     string
	Stderr     string
	TimedOut   bool
}

// Wait after the group is killed for any process outside it still holding an output pipe
const childDrainDelay = 10 * time.Second

// Runs the child in its own owned process group, killing the whole group at the timeout or on cancel
func RunChild(ctx context.Context, env *tools.Env, run ChildRun) (ChildResult, error) {
	reqR, reqW, err := os.Pipe()
	if err != nil {
		return ChildResult{}, err
	}
	repR, repW, err := os.Pipe()
	if err != nil {
		closeFiles(reqR, reqW)
		return ChildResult{}, err
	}
	cmd := exec.Command(run.Python)
	pipe, err := attachChildPipe(cmd, repR, reqW)
	if err != nil {
		closeFiles(reqR, reqW, repR, repW)
		return ChildResult{}, err
	}
	cmd.Args = append(cmd.Args, run.Args(pipe)...)
	cmd.Env = envList(run.Env)
	cmd.Stdin = strings.NewReader(run.Stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.WaitDelay = childDrainDelay
	hideConsoleWindow(cmd)
	err = spawnOwned(tools.WithEnvHome(ctx, env), cmd, run.Purpose)
	closeFiles(repR, reqW)
	if err != nil {
		closeFiles(reqR, repW)
		return ChildResult{}, err
	}
	pid := cmd.Process.Pid
	serveCtx, stopServe := context.WithCancel(ctx)
	defer stopServe()
	served := make(chan struct{})
	go func() {
		defer close(served)
		run.Serve(serveCtx, reqR, repW)
	}()
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()
	timer := time.NewTimer(run.Timeout)
	defer timer.Stop()
	timedOut := false
	select {
	case <-exited:
	case <-timer.C:
		timedOut = true
		signalTree(pid, cmd.Process.Kill, syscall.SIGKILL)
		<-exited
	case <-ctx.Done():
		signalTree(pid, cmd.Process.Kill, syscall.SIGKILL)
		<-exited
	}
	stopServe()
	closeFiles(reqR, repW)
	<-served
	if !isWindows {
		tools.ReleaseProcessGroupOnceEmpty(pid, nil, time.Second)
	}
	return ChildResult{ReturnCode: returnCode(cmd.ProcessState), Stdout: textMode(stdout.Bytes()),
		Stderr: textMode(stderr.Bytes()), TimedOut: timedOut}, nil
}

// Return code as Python reports it, the negated signal number for a child a signal ended
func returnCode(state *os.ProcessState) int {
	if status, ok := state.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return -int(status.Signal())
	}
	return state.ExitCode()
}

// UTF-8 text with invalid bytes replaced and every line ending turned into a newline
func textMode(b []byte) string {
	return strings.ReplaceAll(strings.ReplaceAll(pystr.DecodeUTF8Replace(b), "\r\n", "\n"), "\r", "\n")
}
