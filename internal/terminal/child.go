package terminal

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/pkg/fsx"
	"github.com/nickheyer/icebath/pkg/proc"
)

// Descriptors a child reads replies from and writes requests to, Windows handles when Handles is set
type ChildPipe struct {
	Replies  uintptr
	Requests uintptr
	Handles  bool
}

// One run of a program of this machine that asks its caller over a pipe pair while it runs: its arguments made from the pipe it was
// given, its stdin fed, its requests served by Serve until it ends
type ChildRun struct {
	Program string
	Args    func(pipe ChildPipe) []string
	Stdin   string
	Env     map[string]string
	Dir     string
	Timeout time.Duration
	// What the ledger records it as
	Purpose string
	Serve   func(ctx context.Context, requests io.Reader, replies io.Writer)
}

// Exit code and output of a child run, its line endings made newlines
type ChildResult struct {
	Code     int
	Stdout   string
	Stderr   string
	TimedOut bool
}

// Longest wait, once the child is killed, for anything outside its tree still holding an output pipe
const childDrain = 10 * time.Second

// Runs the child in a process group of its own that dies with icebath, recorded in the profile's ledger; its whole tree killed at the
// timeout or when ctx ends
func (t *Terminal) RunChild(ctx context.Context, cfg *config.Config, run ChildRun) (ChildResult, error) {
	reqR, reqW, err := os.Pipe()
	if err != nil {
		return ChildResult{}, err
	}
	repR, repW, err := os.Pipe()
	if err != nil {
		fsx.CloseAll(reqR, reqW)
		return ChildResult{}, err
	}
	cmd := exec.Command(run.Program)
	pipe, err := attachPipe(cmd, repR, reqW)
	if err != nil {
		fsx.CloseAll(reqR, reqW, repR, repW)
		return ChildResult{}, err
	}
	cmd.Args = append(cmd.Args, run.Args(pipe)...)
	cmd.Env = Environ(run.Env)
	cmd.Dir = run.Dir
	cmd.Stdin = strings.NewReader(run.Stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.WaitDelay = childDrain
	NewSession(cmd)
	proc.HideWindow(cmd)
	err = cmd.Start()
	fsx.CloseAll(repR, reqW)
	if err != nil {
		fsx.CloseAll(reqR, repW)
		return ChildResult{}, err
	}
	p := Watch(cmd)
	p.Own(t.Supervisor)
	defer p.Release()
	RegisterChild(cfg, p.Pid(), run.Purpose)
	serveCtx, stopServe := context.WithCancel(ctx)
	defer stopServe()
	served := make(chan struct{})
	go func() {
		defer close(served)
		run.Serve(serveCtx, reqR, repW)
	}()
	timer := time.NewTimer(run.Timeout)
	defer timer.Stop()
	timedOut := false
	select {
	case <-p.done:
	case <-timer.C:
		timedOut = true
		p.ForceKill()
	case <-ctx.Done():
		p.ForceKill()
	}
	<-p.done
	stopServe()
	fsx.CloseAll(reqR, repW)
	<-served
	return ChildResult{Code: p.code, Stdout: textMode(stdout.Bytes()), Stderr: textMode(stderr.Bytes()), TimedOut: timedOut}, nil
}

// Valid UTF-8 with every line ending a newline
func textMode(b []byte) string {
	return strings.ReplaceAll(strings.ReplaceAll(strings.ToValidUTF8(string(b), "�"), "\r\n", "\n"), "\r", "\n")
}
