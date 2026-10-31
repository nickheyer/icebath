package vsc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/harness"
)

// Waits between attempts to reach the editor, and how long a window gets to advertise before installing
const (
	dialMin      = time.Second
	dialMax      = 10 * time.Second
	installGrace = 20 * time.Second
)

// Target standing for the window the terminal belongs to, found through its advertisement instead of the variable
const discoverTarget = "*"

// Reaches the editor with backoff, by the socket the variable names or the advertised window, and serves the agent on every connection
type bridge struct {
	app       *harness.App
	agent     *Agent
	log       *traceLog
	mu        sync.Mutex
	target    string
	cancel    context.CancelFunc
	done      chan struct{}
	since     time.Time
	installed bool
}

func newBridge(app *harness.App) *bridge {
	b := &bridge{app: app, log: &traceLog{}}
	b.agent = NewAgent(app, Options{Version: Version(), Trace: b.log.trace})
	return b
}

// Starts reaching the target, restarting when a loop runs on another target and leaving one on the same target alone
func (b *bridge) start(target string) {
	b.mu.Lock()
	running, same := b.cancel != nil, b.target == target
	b.mu.Unlock()
	if running && same {
		return
	}
	if running {
		b.stop()
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	b.mu.Lock()
	b.target, b.cancel, b.done, b.since = target, cancel, done, time.Now()
	b.mu.Unlock()
	go b.loop(ctx, target, done)
}

// Ends the loop and waits for its connection to close
func (b *bridge) stop() {
	b.mu.Lock()
	cancel, done := b.cancel, b.done
	b.cancel, b.done, b.target = nil, nil, ""
	b.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	<-done
}

// Connects, serves, waits with growing backoff after each failure or disconnect and repeats until the context ends
func (b *bridge) loop(ctx context.Context, target string, done chan struct{}) {
	defer close(done)
	wait, last := dialMin, ""
	for {
		conn, err := b.connect(ctx, target)
		if err == nil {
			wait, last = dialMin, ""
			b.log.infof("connected to %s", target)
			if err := b.agent.Serve(ctx, conn); err != nil {
				b.log.infof("connection to %s ended, %v", target, err)
			} else {
				b.log.infof("connection to %s closed", target)
			}
		} else if err.Error() != last {
			last = err.Error()
			b.log.infof("reach %s, %v", target, err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = min(wait*2, dialMax)
	}
}

// Connection for the target, the socket the variable names or the one the window of this terminal advertises
func (b *bridge) connect(ctx context.Context, target string) (io.ReadWriteCloser, error) {
	if target != discoverTarget {
		return dial(ctx, target)
	}
	return b.discover(ctx)
}

// Dials the advertised window of this terminal, forgetting dead advertisements and installing the extension once the grace period passes with none
func (b *bridge) discover(ctx context.Context) (io.ReadWriteCloser, error) {
	windows, err := Windows()
	if err != nil {
		return nil, err
	}
	for _, w := range ordered(windows, b.app.Root) {
		conn, err := dial(ctx, w.Socket)
		if err != nil {
			w.forget()
			continue
		}
		if _, version, ok := bundledVSIX(); ok && newerVersion(version, w.Version) {
			b.install(ctx, fmt.Sprintf("updating the VS Code extension from %s to %s", w.Version, version))
		}
		return conn, nil
	}
	b.mu.Lock()
	since := b.since
	b.mu.Unlock()
	if time.Since(since) >= installGrace {
		b.install(ctx, "installing the VS Code extension")
	}
	return nil, errors.New("no editor window advertises a listener")
}

// Installs the bundled extension when auto install is on, once per process and once per version on this machine
func (b *bridge) install(ctx context.Context, why string) {
	if !Current().AutoInstall {
		return
	}
	b.mu.Lock()
	first := !b.installed
	b.installed = true
	b.mu.Unlock()
	if !first {
		return
	}
	data, version, ok := bundledVSIX()
	if !ok {
		b.notify(harness.Error, "VS Code extension: this build carries no VS Code extension, build it with make vsix")
		return
	}
	if installed, err := InstalledVersion(); err == nil && installed == version {
		b.log.infof("VS Code extension %s installed on this machine already, leaving the editor alone", version)
		return
	}
	b.notify(harness.Info, why)
	cli, err := installExtension(ctx, data)
	if err != nil {
		b.notify(harness.Error, "VS Code extension: "+err.Error())
		return
	}
	if err := markInstalled(version); err != nil {
		b.notify(harness.Warn, "VS Code extension: "+err.Error())
	}
	b.notify(harness.Success, "VS Code extension "+version+" installed with "+filepath.Base(cli)+", connecting once it starts")
}

// Shows the person a notice in the TUI and writes it to the log
func (b *bridge) notify(level harness.Level, text string) {
	b.log.infof("%s", text)
	b.app.Announce(harness.NoticeMsg{Text: text, Level: level})
}

// Appends protocol lines and notes to the file the settings name, nothing while none is named
type traceLog struct {
	mu   sync.Mutex
	path string
	f    *os.File
}

// Points the log at another file, closing the one open, a tilde standing for the home directory
func (l *traceLog) setPath(path string) {
	if strings.HasPrefix(path, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(home, strings.TrimPrefix(path, "~"))
		}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if path == l.path {
		return
	}
	if l.f != nil {
		l.f.Close()
		l.f = nil
	}
	l.path = path
}

// Writes one stamped line, opening the file on first use and dropping the line when it cannot be opened
func (l *traceLog) write(kind, text string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.path == "" {
		return
	}
	if l.f == nil {
		f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return
		}
		l.f = f
	}
	fmt.Fprintf(l.f, "%s %s %s\n", time.Now().Format(time.RFC3339Nano), kind, strings.TrimRight(text, "\n"))
}

func (l *traceLog) trace(dir string, line []byte) { l.write(dir, string(line)) }

func (l *traceLog) infof(format string, args ...any) { l.write("info", fmt.Sprintf(format, args...)) }
