package process

import (
	"context"
	"errors"
	"os"
	"sync"
	"time"

	"github.com/UserExistsError/conpty"

	"github.com/nickheyer/icebath/internal/shell"
)

const ptyTerminateDelay = 100 * time.Millisecond

const ptyUnavailableReason = "this version of Windows has no pseudo-console (ConPTY) support"

func ptyAvailable() bool { return conpty.IsConPtyAvailable() }

func ptyMissing(err error) bool { return errors.Is(err, conpty.ErrConPtyUnsupported) }

type winPTY struct {
	c         *conpty.ConPty
	done      chan struct{}
	code      *int
	closeOnce sync.Once
}

func startPTY(argv []string, cwd string, env []string) (ptyJob, error) {
	c, err := conpty.Start(shell.WindowsCommandLine(argv),
		conpty.ConPtyDimensions(ptyCols, ptyRows), conpty.ConPtyEnv(env), conpty.ConPtyWorkDir(cwd))
	if err != nil {
		return nil, err
	}
	j := &winPTY{c: c, done: make(chan struct{})}
	go func() {
		if code, err := c.Wait(context.Background()); err == nil {
			v := int(code)
			j.code = &v
		}
		close(j.done)
		_ = j.close()
	}()
	return j, nil
}

func (j *winPTY) pid() int { return j.c.Pid() }

func (j *winPTY) read(p []byte) (int, error) { return j.c.Read(p) }

func (j *winPTY) write(p []byte) (int, error) { return j.c.Write(p) }

func (j *winPTY) sendEOF() error {
	_, err := j.c.Write([]byte{4})
	return err
}

func (j *winPTY) terminate(force bool) (bool, error) {
	if !j.alive() {
		return true, nil
	}
	steps := 1
	if force {
		steps = 2
	}
	for range steps {
		if err := signalTerminate(j.pid()); err != nil && j.alive() {
			return false, err
		}
		time.Sleep(ptyTerminateDelay)
		if !j.alive() {
			return true, nil
		}
	}
	return false, nil
}

func signalTerminate(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	defer func() { _ = p.Release() }()
	if err := p.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	return nil
}

func (j *winPTY) alive() bool {
	select {
	case <-j.done:
		return false
	default:
		return true
	}
}

func (j *winPTY) wait(ctx context.Context) (*int, error) {
	select {
	case <-j.done:
		return j.code, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (j *winPTY) exitCode() (*int, bool) {
	select {
	case <-j.done:
		return j.code, true
	default:
		return nil, false
	}
}

func (j *winPTY) close() error {
	var err error
	j.closeOnce.Do(func() { err = j.c.Close() })
	return err
}

func (j *winPTY) answersQueries() bool { return false }
