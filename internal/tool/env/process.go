package env

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/internal/proc"
	"github.com/nickheyer/icebath/internal/tool"
)

type cmdProcess struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	out    *os.File
	done   chan struct{}
	code   int
	group  bool
	leader proc.Identity
	known  bool
	pgid   int
	guard  *proc.Guard

	mu    sync.Mutex
	owned bool
}

func startCommand(cmd *exec.Cmd, stdin *string, group bool, guard *proc.Guard) (*cmdProcess, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	cmd.Stdout, cmd.Stderr = w, w
	var in io.WriteCloser
	if stdin != nil {
		if in, err = cmd.StdinPipe(); err != nil {
			_ = r.Close()
			_ = w.Close()
			return nil, err
		}
	}
	if group {
		startGroup(cmd)
	} else {
		proc.Hide(cmd)
	}
	if err := cmd.Start(); err != nil {
		_ = r.Close()
		_ = w.Close()
		return nil, err
	}
	_ = w.Close()
	p := &cmdProcess{cmd: cmd, stdin: in, out: r, done: make(chan struct{}), group: group, guard: guard}
	if group {
		p.adopt()
	}
	go func() {
		_ = cmd.Wait()
		p.code = proc.ExitOf(cmd.ProcessState).Status()
		close(p.done)
	}()
	if stdin != nil {
		data := *stdin
		go func() {
			_, _ = io.WriteString(in, data)
			_ = in.Close()
		}()
	}
	return p, nil
}

func StartGroup(cmd *exec.Cmd, stdin *string) (tool.Process, error) {
	return startCommand(cmd, stdin, true, nil)
}

func (p *cmdProcess) PID() int { return p.cmd.Process.Pid }

func (p *cmdProcess) Stdin() io.WriteCloser { return p.stdin }

func (p *cmdProcess) Output() io.Reader { return p.out }

func (p *cmdProcess) Wait(ctx context.Context) (int, error) {
	select {
	case <-p.done:
		return p.code, nil
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

func (p *cmdProcess) Exited() (int, bool) {
	select {
	case <-p.done:
		return p.code, true
	default:
		return 0, false
	}
}

func (p *cmdProcess) Kill() error {
	if p.group {
		killGroup(p)
		return nil
	}
	if err := p.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	return nil
}

func (p *cmdProcess) ForceKill() error {
	if p.group {
		forceKillGroup(p)
		return nil
	}
	return p.Kill()
}

func (p *cmdProcess) Release() {
	p.mu.Lock()
	owned := p.owned
	p.owned = false
	p.mu.Unlock()
	if owned && p.guard != nil {
		_ = p.guard.Release(p.pgid)
	}
}

func (p *cmdProcess) Close() error {
	return p.out.Close()
}

type pumped struct {
	src  tool.Process
	ch   chan []byte
	quit chan struct{}
	once sync.Once
	rest []byte
}

func newPumped(src tool.Process) *pumped {
	p := &pumped{src: src, ch: make(chan []byte), quit: make(chan struct{})}
	r := src.Output()
	go func() {
		defer close(p.ch)
		if r == nil {
			return
		}
		for {
			buf := make([]byte, 4096)
			n, err := r.Read(buf)
			if n > 0 {
				select {
				case p.ch <- buf[:n]:
				case <-p.quit:
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	return p
}

func (p *pumped) abandon() {
	p.once.Do(func() { close(p.quit) })
	if c, ok := p.src.(io.Closer); ok {
		_ = c.Close()
	}
}

func (p *pumped) PID() int { return p.src.PID() }

func (p *pumped) Stdin() io.WriteCloser { return p.src.Stdin() }

func (p *pumped) Output() io.Reader { return pumpReader{p} }

func (p *pumped) Wait(ctx context.Context) (int, error) { return p.src.Wait(ctx) }

func (p *pumped) Exited() (int, bool) { return p.src.Exited() }

func (p *pumped) Kill() error { return p.src.Kill() }

func (p *pumped) ForceKill() error {
	if f, ok := p.src.(ForceKiller); ok {
		return f.ForceKill()
	}
	return p.src.Kill()
}

func (p *pumped) Release() {
	if r, ok := p.src.(Releaser); ok {
		r.Release()
	}
}

func (p *pumped) Close() error {
	p.abandon()
	return nil
}

type pumpReader struct{ p *pumped }

func (r pumpReader) Read(b []byte) (int, error) {
	if len(r.p.rest) == 0 {
		chunk, ok := <-r.p.ch
		if !ok {
			return 0, io.EOF
		}
		r.p.rest = chunk
	}
	n := copy(b, r.p.rest)
	r.p.rest = r.p.rest[n:]
	return n, nil
}

type threadedProcess struct {
	out    *io.PipeReader
	done   chan struct{}
	code   int
	cancel func()
	stop   context.CancelFunc
}

func NewThreadedProcess(run func(ctx context.Context) (string, int, error), cancel func()) tool.Process {
	ctx, stop := context.WithCancel(context.Background())
	r, w := io.Pipe()
	p := &threadedProcess{out: r, done: make(chan struct{}), cancel: cancel, stop: stop}
	go func() {
		output, code, err := run(ctx)
		if err != nil {
			code, output = 1, ""
		}
		p.code = code
		_, _ = io.WriteString(w, strings.TrimPrefix(output, string(rune(0xFEFF))))
		_ = w.Close()
		close(p.done)
	}()
	return p
}

func (p *threadedProcess) PID() int { return 0 }

func (p *threadedProcess) Stdin() io.WriteCloser { return nil }

func (p *threadedProcess) Output() io.Reader { return p.out }

func (p *threadedProcess) Wait(ctx context.Context) (int, error) {
	select {
	case <-p.done:
		return p.code, nil
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

func (p *threadedProcess) Exited() (int, bool) {
	select {
	case <-p.done:
		return p.code, true
	default:
		return 0, false
	}
}

func (p *threadedProcess) Kill() error {
	p.stop()
	if p.cancel != nil {
		p.cancel()
	}
	return nil
}

func (p *threadedProcess) Close() error {
	return p.out.Close()
}
