//go:build windows

package tools

import (
	"context"
	"io"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/windows"
)

// Reports whether a ConPTY bridge can be spawned on this host
func WinPtyBridgeAvailable() bool {
	return windows.NewLazySystemDLL("kernel32.dll").NewProc("CreatePseudoConsole").Find() == nil
}

// Spawns argv on a fresh ConPTY with the agent subprocess environment when Env is nil
func SpawnWinPtyBridge(ctx context.Context, env *Env, argv []string, o PtySpawnOptions) (TerminalBridge, error) {
	if !WinPtyBridgeAvailable() {
		return nil, &PtyUnavailableError{Msg: "ConPTY is unavailable on this platform."}
	}
	var spawnEnv map[string]string
	if o.Env == nil {
		built, err := BuildSubprocessEnv(ctx, env, SubprocessEnvOptions{ScrubSecrets: false, InheritProfileHome: false})
		if err != nil {
			return nil, err
		}
		spawnEnv = built
	} else {
		spawnEnv = make(map[string]string, len(o.Env)+1)
		for k, v := range o.Env {
			spawnEnv[k] = v
		}
	}
	if spawnEnv["TERM"] == "" {
		spawnEnv["TERM"] = "xterm-256color"
	}
	cols, rows := o.Cols, o.Rows
	if cols == 0 {
		cols = 80
	}
	if rows == 0 {
		rows = 24
	}
	p, err := spawnPty(argv, o.Cwd, spawnEnv, rows, cols)
	if err != nil {
		return nil, err
	}
	return newWinPtyBridge(newConptyChild(p)), nil
}

// Spawns the dashboard terminal bridge of this platform, ConPTY here
func SpawnTerminalBridge(ctx context.Context, env *Env, argv []string, o PtySpawnOptions) (TerminalBridge, error) {
	return SpawnWinPtyBridge(ctx, env, argv, o)
}

// Reports whether this platform's dashboard terminal bridge can spawn
func TerminalBridgeAvailable() bool { return WinPtyBridgeAvailable() }

type conptyChunk struct {
	data []byte
	err  error
}

// ConPTY child with a pump goroutine so reads return within their timeout like pywinpty's reader thread
type conptyChild struct {
	p      *ptyProcess
	chunks chan conptyChunk
}

func newConptyChild(p *ptyProcess) *conptyChild {
	c := &conptyChild{p: p, chunks: make(chan conptyChunk, 64)}
	go func() {
		var carry []byte
		for {
			buf := make([]byte, 65536)
			n, err := p.Read(buf)
			data := append(carry, buf[:n]...)
			cut := utf8CompletePrefix(data)
			if err != nil {
				cut = len(data)
			}
			carry = append([]byte(nil), data[cut:]...)
			if cut > 0 {
				c.chunks <- conptyChunk{data: data[:cut]}
			}
			if err != nil {
				c.chunks <- conptyChunk{err: err}
				close(c.chunks)
				return
			}
		}
	}()
	return c
}

func (c *conptyChild) Pid() int { return c.p.Pid() }

func (c *conptyChild) IsAlive() bool { return c.p.IsAlive() }

func (c *conptyChild) Read(timeout time.Duration) ([]byte, error) {
	t := time.NewTimer(max(timeout, 0))
	defer t.Stop()
	select {
	case ch, ok := <-c.chunks:
		if !ok {
			return nil, io.EOF
		}
		if ch.err != nil {
			return nil, ch.err
		}
		return ch.data, nil
	case <-t.C:
		if !c.p.IsAlive() && len(c.chunks) == 0 {
			return nil, io.EOF
		}
		return nil, nil
	}
}

func (c *conptyChild) WriteText(text string) error { return c.p.Write([]byte(text)) }

func (c *conptyChild) SetWinSize(rows, cols int) error { return c.p.Resize(rows, cols) }

func (c *conptyChild) Terminate(force bool) error { return c.p.Close() }

// Length of data without a trailing incomplete UTF-8 sequence, the incremental decoder hold back
func utf8CompletePrefix(data []byte) int {
	for back := 1; back <= 3 && back <= len(data); back++ {
		i := len(data) - back
		if !utf8.RuneStart(data[i]) {
			continue
		}
		if !utf8.FullRune(data[i:]) {
			return i
		}
		break
	}
	return len(data)
}
