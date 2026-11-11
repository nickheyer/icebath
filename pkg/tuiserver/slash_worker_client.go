package tuiserver

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Owned child spawner of the worker, the process identity ledger spawn
var spawnOwned = tools.SpawnOwned

// Seconds a slash command may run before the worker counts as timed out
var slashWorkerTimeoutS = max(5.0, envFloat("ICEBATH_TUI_SLASH_TIMEOUT_S", 45.0))

// Child PATH led by the managed bin, this binary dir and the user local bin, package stores first
func prependToolPaths(ctx context.Context, env map[string]string) map[string]string {
	managedBin := ""
	suppress(func() { managedBin = filepath.Join(home.Dir(ctx), "bin") })
	binDir := ""
	if exe, err := os.Executable(); err == nil {
		binDir = filepath.Dir(exe)
	}
	userLocal := ""
	if userHome, err := home.UserHome(); err == nil {
		userLocal = filepath.Join(userHome, ".local", "bin")
	}
	var parts []string
	for _, p := range []string{managedBin, binDir, userLocal, env["PATH"]} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	env["PATH"] = strings.Join(parts, string(os.PathListSeparator))
	if storeFirst, err := pm.StoreFirstPath(ctx, env["PATH"]); err == nil {
		env["PATH"] = storeFirst
	}
	return env
}

// Unbounded queue of decoded worker replies, nil marking a closed pipe
type replyQueue struct {
	mu     sync.Mutex
	items  []any
	notify chan struct{}
}

// Appends one reply and wakes a waiting reader
func (q *replyQueue) put(item any) {
	q.mu.Lock()
	q.items = append(q.items, item)
	q.mu.Unlock()
	select {
	case q.notify <- struct{}{}:
	default:
	}
}

// Oldest reply, false when none arrived within the timeout
func (q *replyQueue) get(timeout time.Duration) (any, bool) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		q.mu.Lock()
		if len(q.items) > 0 {
			item := q.items[0]
			q.items = q.items[1:]
			q.mu.Unlock()
			return item, true
		}
		q.mu.Unlock()
		select {
		case <-q.notify:
		case <-deadline.C:
			return nil, false
		}
	}
}

// Persistent CLI child of one TUI session that runs slash commands over JSON lines
type slashWorker struct {
	mu       sync.Mutex
	seq      int64
	tailMu   sync.Mutex
	tail     []string
	queue    *replyQueue
	closed   atomic.Bool
	seedMu   sync.Mutex
	lastSeed string
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	stdout   io.ReadCloser
	stderr   io.ReadCloser
	exited   chan struct{}
}

// Starts the worker child pinned to the session model and provider in the session profile's env
func newSlashWorker(ctx context.Context, sessionKey, model, profileHome, providerName string) (*slashWorker, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	argv := []string{tuigateway.SlashWorkerEntry, "--session-key", sessionKey}
	if model != "" {
		argv = append(argv, "--model", model)
	}
	if providerName != "" {
		argv = append(argv, "--provider", providerName)
	}
	target := profileHome
	if target == "" && provider.IsMultiplexActive(ctx) {
		target = hermesHome
	}
	childEnv, err := tools.ServedProfileChildEnv(ctx, configEnv(ctx), nil, target, true)
	if err != nil {
		return nil, err
	}
	childEnv = prependToolPaths(ctx, childEnv)
	cmd := exec.Command(exe, argv...)
	if cmd.Dir, err = os.Getwd(); err != nil {
		return nil, err
	}
	for key, value := range childEnv {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	hideSlashWorkerWindow(cmd)
	w := &slashWorker{queue: &replyQueue{notify: make(chan struct{}, 1)}, cmd: cmd, exited: make(chan struct{})}
	if w.stdin, err = cmd.StdinPipe(); err != nil {
		return nil, err
	}
	if w.stdout, err = cmd.StdoutPipe(); err != nil {
		return nil, err
	}
	if w.stderr, err = cmd.StderrPipe(); err != nil {
		return nil, err
	}
	if err := spawnOwned(ctx, cmd, "slash-worker"); err != nil {
		return nil, err
	}
	go w.drainStdout()
	go w.drainStderr()
	go func() {
		_, _ = cmd.Process.Wait()
		close(w.exited)
	}()
	return w, nil
}

// Decodes every stdout line into the reply queue, undecodable lines dropped, then marks the pipe closed
func (w *slashWorker) drainStdout() {
	reader := bufio.NewReader(w.stdout)
	for {
		raw, err := reader.ReadBytes('\n')
		if len(raw) > 0 {
			if msg, decodeErr := pyjson.Loads(pystr.DecodeUTF8Replace(raw)); decodeErr == nil {
				w.queue.put(msg)
			}
		}
		if err != nil {
			break
		}
	}
	w.queue.put(nil)
}

// Keeps the last 80 non empty stderr lines
func (w *slashWorker) drainStderr() {
	reader := bufio.NewReader(w.stderr)
	for {
		raw, err := reader.ReadBytes('\n')
		if text := strings.TrimRight(pystr.DecodeUTF8Replace(raw), "\n"); text != "" {
			w.tailMu.Lock()
			w.tail = append(w.tail, text)
			if len(w.tail) > 80 {
				w.tail = w.tail[len(w.tail)-80:]
			}
			w.tailMu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

// Whether the child process has exited
func (w *slashWorker) poll() bool {
	select {
	case <-w.exited:
		return true
	default:
		return false
	}
}

// Waits up to the timeout for the child to exit
func (w *slashWorker) wait(timeout time.Duration) bool {
	select {
	case <-w.exited:
		return true
	case <-time.After(timeout):
		return false
	}
}

// Runs one command and returns its output, a parked next turn seed kept for popSeed
func (w *slashWorker) run(command string) (string, error) {
	if w.poll() {
		return "", errors.New("slash worker exited")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.seq++
	rid := w.seq
	encoded, err := pyjson.Dumps(pyjson.ObjectOf("id", rid, "command", command))
	if err != nil {
		return "", err
	}
	if _, err := io.WriteString(w.stdin, encoded+"\n"); err != nil {
		return "", err
	}
	for {
		raw, arrived := w.queue.get(time.Duration(slashWorkerTimeoutS * float64(time.Second)))
		if !arrived {
			return "", errors.New("slash worker timed out")
		}
		if raw == nil {
			break
		}
		msg, isDict := pyjson.AsObject(raw)
		if !isDict {
			return "", fmt.Errorf("'%s' object has no attribute 'get'", pyrepr.TypeName(raw))
		}
		if !pyjson.Equal(msg.GetOr("id", nil), rid) {
			continue
		}
		if !truthy(msg.GetOr("ok", nil)) {
			return "", errors.New(pyrepr.Str(msg.GetOr("error", "slash worker failed")))
		}
		w.seedMu.Lock()
		w.lastSeed = strOr(msg.GetOr("seed", ""))
		w.seedMu.Unlock()
		return pystr.Rstrip(pyrepr.Str(msg.GetOr("output", ""))), nil
	}
	w.tailMu.Lock()
	tail := w.tail[max(0, len(w.tail)-8):]
	w.tailMu.Unlock()
	if len(tail) > 0 {
		return "", errors.New("slash worker closed pipe: " + strings.Join(tail, "\n"))
	}
	return "", errors.New("slash worker closed pipe")
}

// Returns and clears the seed of the last run
func (w *slashWorker) popSeed() string {
	w.seedMu.Lock()
	defer w.seedMu.Unlock()
	seed := w.lastSeed
	w.lastSeed = ""
	return seed
}

// Stops the worker group once and closes its pipes
func (w *slashWorker) Close() {
	if !w.closed.CompareAndSwap(false, true) {
		return
	}
	defer func() {
		for _, stream := range []io.Closer{w.stdin, w.stdout, w.stderr} {
			suppress(func() { _ = stream.Close() })
		}
	}()
	stopWorkerGroup(w)
}
