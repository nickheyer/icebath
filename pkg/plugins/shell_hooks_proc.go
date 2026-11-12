package plugins

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"sort"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/tools"
)

// One started hook process with its captured streams
type shellHookRun struct {
	cmd            *exec.Cmd
	child          *tools.CmdChild
	stdout, stderr bytes.Buffer
	readers        []*os.File
	done           chan struct{}
}

// Starts argv with piped stdio in its own process group, the payload written then stdin closed
func startShellHookProcess(argv []string, env map[string]string, stdin []byte) (*shellHookRun, error) {
	path, err := shellHookExecutable(argv[0], env)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(path, argv[1:]...)
	cmd.Args[0] = argv[0]
	cmd.Env = shellHookEnviron(env)
	shellHookProcAttr(cmd)
	inR, inW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		inR.Close()
		inW.Close()
		return nil, err
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		for _, f := range []*os.File{inR, inW, outR, outW} {
			f.Close()
		}
		return nil, err
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = inR, outW, errW
	err = cmd.Start()
	inR.Close()
	outW.Close()
	errW.Close()
	if err != nil {
		inW.Close()
		outR.Close()
		errR.Close()
		return nil, err
	}
	run := &shellHookRun{cmd: cmd, child: tools.WatchCmd(cmd), readers: []*os.File{outR, errR}, done: make(chan struct{})}
	go func() {
		_, _ = inW.Write(stdin)
		inW.Close()
	}()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = io.Copy(&run.stdout, outR) }()
	go func() { defer wg.Done(); _, _ = io.Copy(&run.stderr, errR) }()
	go func() {
		wg.Wait()
		<-run.child.Done()
		outR.Close()
		errR.Close()
		close(run.done)
	}()
	return run, nil
}

// Kills the hook's whole tree, then drains for at most a second before abandoning held pipes
func (r *shellHookRun) reap() {
	tools.KillChildTree(r.child, 0)
	select {
	case <-r.done:
	case <-time.After(time.Second):
		for _, f := range r.readers {
			f.Close()
		}
	}
}

// Exit status the way Popen.returncode reports it, minus the signal number for a signalled child
func (r *shellHookRun) returnCode() int { return shellHookReturnCode(r.cmd.ProcessState) }

// Child environment as a sorted KEY=VALUE list
func shellHookEnviron(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

// Whether a start error is Python's FileNotFoundError
func isNotFoundError(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, exec.ErrNotFound)
}

// Whether a start error is Python's PermissionError
func isPermissionError(err error) bool { return errors.Is(err, fs.ErrPermission) }
