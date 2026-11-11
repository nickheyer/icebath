package dashboard

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tools"
)

type fakeSSHFsCall struct {
	command string
	opts    tools.ExecOptions
}

type fakeSSHFsEnv struct {
	cwd, remoteHome, host string
	timeout               int
	opts                  tools.SSHOptions
	results               []*tools.ExecResult
	calls                 []fakeSSHFsCall
	prefix                []string
	cleanup               func() error
}

func (e *fakeSSHFsEnv) Execute(_ context.Context, command string, opts tools.ExecOptions) (*tools.ExecResult, error) {
	e.calls = append(e.calls, fakeSSHFsCall{command, opts})
	r := e.results[0]
	e.results = e.results[1:]
	return r, nil
}
func (e *fakeSSHFsEnv) Cwd() string                               { return e.cwd }
func (e *fakeSSHFsEnv) Timeout() int                              { return e.timeout }
func (e *fakeSSHFsEnv) RemoteHome() (string, bool)                { return e.remoteHome, true }
func (e *fakeSSHFsEnv) ExecPrefix(string, bool) ([]string, error) { return e.prefix, nil }
func (e *fakeSSHFsEnv) Cleanup(bool) error {
	if e.cleanup != nil {
		return e.cleanup()
	}
	return nil
}

func newFakeSSHFsEnv(results ...*tools.ExecResult) *fakeSSHFsEnv {
	return &fakeSSHFsEnv{cwd: "/srv/repos", timeout: 30, remoteHome: "/home/dev", results: results}
}

func sshFsEntry(name, path, kind string) string {
	return base64.StdEncoding.EncodeToString([]byte(name)) + "\t" + base64.StdEncoding.EncodeToString([]byte(path)) + "\t" + kind
}

// Restores the factory seams and cache a test replaces
func isolateSSHFsFactory(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	prevEnv, prevPopen, prevDial, prevEndpoint := newSSHWorkspaceEnv, sshFsPopen, sshFsDialLocks, sshFsEndpointLocks
	sshFsCacheMu.Lock()
	prevBackends := sshFsBackends
	sshFsBackends = map[string]sshFsCached{}
	sshFsCacheMu.Unlock()
	t.Cleanup(func() {
		newSSHWorkspaceEnv, sshFsPopen, sshFsDialLocks, sshFsEndpointLocks = prevEnv, prevPopen, prevDial, prevEndpoint
		sshFsCacheMu.Lock()
		sshFsBackends = prevBackends
		sshFsCacheMu.Unlock()
	})
}

func TestSshWorkspaceFsListsAndReadsRemotePaths(t *testing.T) {
	isolateSSHFsFactory(t)
	env := newFakeSSHFsEnv(
		&tools.ExecResult{Output: strings.Join([]string{
			sshFsEntry("z.txt", "/srv/repos/z.txt", "f"),
			sshFsEntry("src", "/srv/repos/src", "d"),
			sshFsEntry("node_modules", "/srv/repos/node_modules", "d"),
		}, "\n")},
		&tools.ExecResult{Output: "__HERMES_FS_SIZE__:5\naGVsbG8=\n"},
	)
	backend := newSshWorkspaceFs(env)
	listing, err := backend.ListDir("/srv/repos", map[string]bool{"node_modules": true})
	if err != nil {
		t.Fatal(err)
	}
	limit := int64(50)
	data, size, path, err := backend.ReadBytes("README.md", 100, &limit)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := pyjson.Dumps(listing)
	want := `{"entries": [{"name": "src", "path": "/srv/repos/src", "isDirectory": true}, {"name": "z.txt", "path": "/srv/repos/z.txt", "isDirectory": false}]}`
	if got != want {
		t.Fatalf("listing %s, want %s", got, want)
	}
	if string(data) != "hello" || size != 5 || path != "/srv/repos/README.md" {
		t.Fatalf("read %q %d %q", data, size, path)
	}
	for _, check := range []struct {
		call int
		text string
	}{
		{0, "p='/srv/repos'"}, {1, "p='/srv/repos/README.md'"}, {1, `if [ "$size" -gt 100 ]`},
		{1, `set -o pipefail; head -c 50 < "$p" | base64`},
	} {
		command := env.calls[check.call].command
		if !strings.Contains(command, check.text) && !strings.Contains(command, strings.ReplaceAll(check.text, "'", "")) {
			t.Fatalf("call %d lacks %q:\n%s", check.call, check.text, command)
		}
	}
}

func TestSshWorkspaceFsMapsRemoteSizeFailure(t *testing.T) {
	isolateSSHFsFactory(t)
	env := newFakeSSHFsEnv(&tools.ExecResult{ReturnCode: 47, Output: "__HERMES_FS_ERROR__:EFBIG\n"})
	_, _, _, err := newSshWorkspaceFs(env).ReadBytes("/srv/repos/large.bin", 3, nil)
	var fsErr *SshWorkspaceFsError
	if !errors.As(err, &fsErr) || fsErr.Code != "EFBIG" || !strings.Contains(fsErr.Error(), "EFBIG") {
		t.Fatalf("err %v, want EFBIG", err)
	}
}

type fakeSSHFsProcess struct{ stdout io.Reader }

func (p *fakeSSHFsProcess) Stdout() io.Reader { return p.stdout }
func (p *fakeSSHFsProcess) Running() bool     { return false }
func (p *fakeSSHFsProcess) Terminate()        {}
func (p *fakeSSHFsProcess) Wait() error       { return nil }

func TestSshWorkspaceFsInspectsResolvedFileBeforeStreaming(t *testing.T) {
	isolateSSHFsFactory(t)
	resolved := "/srv/repos/.env"
	encoded := base64.StdEncoding.EncodeToString([]byte(resolved))
	env := newFakeSSHFsEnv(&tools.ExecResult{Output: "__HERMES_FS_TARGET__:" + encoded + "\n__HERMES_FS_SIZE__:6\n"})
	backend := newSshWorkspaceFs(env)
	target, size, err := backend.InspectFile("safe-link")
	if err != nil || target != resolved || size != 6 {
		t.Fatalf("inspect %q %d %v", target, size, err)
	}
	if !strings.Contains(env.calls[0].command, `realpath "$p"`) {
		t.Fatalf("inspect command %s", env.calls[0].command)
	}
	env.prefix = []string{"ssh", "dev@example.com"}
	sshFsPopen = func([]string) (sshFsProcess, error) {
		return &fakeSSHFsProcess{stdout: bytes.NewReader([]byte("streamed"))}, nil
	}
	var out []byte
	err = backend.StreamFile(target, 3, 0, nil)(context.Background(), func(chunk []byte) error {
		out = append(out, chunk...)
		return nil
	})
	if err != nil || string(out) != "streamed" {
		t.Fatalf("stream %q %v", out, err)
	}
}

func TestSshWorkspaceFactoryDisablesAgentFileSync(t *testing.T) {
	isolateSSHFsFactory(t)
	var created []tools.SSHOptions
	newSSHWorkspaceEnv = func(_ context.Context, opts tools.SSHOptions) (sshFsEnv, error) {
		created = append(created, opts)
		return &fakeSSHFsEnv{cwd: opts.Cwd, timeout: opts.Timeout, remoteHome: "/home/dev"}, nil
	}
	backend, err := getSshWorkspaceFs(context.Background(), "remote-dev", map[string]any{
		"backend": "ssh", "cwd": "/srv/repos", "ssh_host": "ssh.example", "ssh_user": "dev",
		"ssh_port": int64(2222), "ssh_key": "/keys/id_ed25519", "timeout": int64(45),
	})
	if err != nil || backend == nil {
		t.Fatalf("backend %v err %v", backend, err)
	}
	if cwd, _ := backend.Cwd(); cwd != "/srv/repos" {
		t.Fatalf("cwd %q", cwd)
	}
	want := []tools.SSHOptions{{Host: "ssh.example", User: "dev", Cwd: "/srv/repos", Timeout: 45, Port: 2222,
		KeyPath: "/keys/id_ed25519", SyncFiles: false, Persistent: true}}
	if !reflect.DeepEqual(created, want) {
		t.Fatalf("created %+v, want %+v", created, want)
	}
}

func sshTerminal(host string) map[string]any {
	return map[string]any{"backend": "ssh", "cwd": "/srv/repos", "ssh_host": host, "ssh_user": "dev"}
}

// Shared progress of the concurrency cases, polled under its mutex
type sshFsProgress struct{ mu sync.Mutex }

func (p *sshFsProgress) waitFor(pred func() bool, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		p.mu.Lock()
		ok := pred()
		p.mu.Unlock()
		if ok {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(time.Millisecond)
	}
}

type recordingSSHFsLock struct {
	key      any
	mu       sync.Mutex
	progress *sshFsProgress
	waits    *[]any
}

func (l *recordingSSHFsLock) Lock() {
	if l.mu.TryLock() {
		return
	}
	l.progress.mu.Lock()
	*l.waits = append(*l.waits, l.key)
	l.progress.mu.Unlock()
	l.mu.Lock()
}

func (l *recordingSSHFsLock) Unlock() { l.mu.Unlock() }

type recordingSSHFsLocks struct {
	mu       sync.Mutex
	locks    map[any]*recordingSSHFsLock
	progress *sshFsProgress
	waits    *[]any
}

func (r *recordingSSHFsLocks) lockFor(key any) sync.Locker {
	r.mu.Lock()
	defer r.mu.Unlock()
	if l, ok := r.locks[key]; ok {
		return l
	}
	l := &recordingSSHFsLock{key: key, progress: r.progress, waits: r.waits}
	r.locks[key] = l
	return l
}

// Swaps the factory lock tables for recording ones and returns the keys some caller had to wait on
func recordLockWaits(progress *sshFsProgress) *[]any {
	waits := &[]any{}
	sshFsDialLocks = &recordingSSHFsLocks{locks: map[any]*recordingSSHFsLock{}, progress: progress, waits: waits}
	sshFsEndpointLocks = &recordingSSHFsLocks{locks: map[any]*recordingSSHFsLock{}, progress: progress, waits: waits}
	return waits
}

type gatedSSH struct {
	progress            *sshFsProgress
	created, cleaned    []string
	hanging, failing    map[string]bool
	dialing, release    chan struct{}
	dialOnce, releaseMu sync.Once
}

func (g *gatedSSH) releaseAll() { g.releaseMu.Do(func() { close(g.release) }) }

// Fake dialer recording each dial, a hanging host blocking until release and a failing one refusing
func installGatedSSH(t *testing.T, progress *sshFsProgress) *gatedSSH {
	g := &gatedSSH{progress: progress, hanging: map[string]bool{}, failing: map[string]bool{},
		dialing: make(chan struct{}), release: make(chan struct{})}
	newSSHWorkspaceEnv = func(_ context.Context, opts tools.SSHOptions) (sshFsEnv, error) {
		progress.mu.Lock()
		g.created = append(g.created, opts.Host)
		hanging, failing := g.hanging[opts.Host], g.failing[opts.Host]
		progress.mu.Unlock()
		if hanging {
			g.dialOnce.Do(func() { close(g.dialing) })
			select {
			case <-g.release:
			case <-time.After(5 * time.Second):
			}
		}
		if failing {
			return nil, errors.New("connection timed out")
		}
		env := &fakeSSHFsEnv{host: opts.Host, cwd: opts.Cwd, timeout: opts.Timeout, remoteHome: "/home/dev"}
		env.cleanup = func() error {
			progress.mu.Lock()
			g.cleaned = append(g.cleaned, env.host)
			progress.mu.Unlock()
			return nil
		}
		return env, nil
	}
	t.Cleanup(g.releaseAll)
	return g
}

func waitChan(ch <-chan struct{}, timeout time.Duration) bool {
	select {
	case <-ch:
		return true
	case <-time.After(timeout):
		return false
	}
}

func TestSshWorkspaceFactoryServesOtherProfilesWhileOneHostHangs(t *testing.T) {
	isolateSSHFsFactory(t)
	progress := &sshFsProgress{}
	g := installGatedSSH(t, progress)
	g.hanging["down.example"] = true
	var mu sync.Mutex
	results := map[string]*SshWorkspaceFs{}
	open := func(key, host string, done chan struct{}) {
		defer close(done)
		b, _ := getSshWorkspaceFs(context.Background(), key, sshTerminal(host))
		mu.Lock()
		results[key] = b
		mu.Unlock()
	}
	downDone, upDone := make(chan struct{}), make(chan struct{})
	go open("down", "down.example", downDone)
	if !waitChan(g.dialing, 5*time.Second) {
		t.Fatal("down dial never started")
	}
	go open("up", "up.example", upDone)
	if !waitChan(upDone, 2*time.Second) {
		g.releaseAll()
		t.Fatal("a hanging dial on one profile blocked another profile")
	}
	mu.Lock()
	cwd, _ := results["up"].Cwd()
	_, downIn := results["down"]
	mu.Unlock()
	if cwd != "/srv/repos" || downIn {
		t.Fatalf("up cwd %q, down present %v", cwd, downIn)
	}
	g.releaseAll()
	waitChan(downDone, 5*time.Second)
	if results["down"] == nil {
		t.Fatal("down backend missing after release")
	}
}

func TestSshWorkspaceFactoryDialsOneProfileOnceForConcurrentRequests(t *testing.T) {
	isolateSSHFsFactory(t)
	progress := &sshFsProgress{}
	g := installGatedSSH(t, progress)
	waits := recordLockWaits(progress)
	g.hanging["ssh.example"] = true
	var mu sync.Mutex
	var backends []*SshWorkspaceFs
	dones := []chan struct{}{make(chan struct{}), make(chan struct{})}
	open := func(done chan struct{}) {
		defer close(done)
		b, _ := getSshWorkspaceFs(context.Background(), "remote-dev", sshTerminal("ssh.example"))
		mu.Lock()
		backends = append(backends, b)
		mu.Unlock()
	}
	go open(dones[0])
	if !waitChan(g.dialing, 5*time.Second) {
		t.Fatal("first dial never started")
	}
	go open(dones[1])
	if !progress.waitFor(func() bool {
		for _, key := range *waits {
			if key == "remote-dev" {
				return true
			}
		}
		return len(g.created) > 1
	}, 5*time.Second) {
		t.Fatal("second caller never contended")
	}
	g.releaseAll()
	for _, done := range dones {
		if !waitChan(done, 5*time.Second) {
			t.Fatal("caller still running")
		}
	}
	if !reflect.DeepEqual(g.created, []string{"ssh.example"}) {
		t.Fatalf("created %v", g.created)
	}
	if len(backends) != 2 || backends[0] != backends[1] {
		t.Fatalf("backends %v", backends)
	}
}

func TestSshWorkspaceFactoryReplacesChangedConfigAndDropsFailedRedial(t *testing.T) {
	isolateSSHFsFactory(t)
	g := installGatedSSH(t, &sshFsProgress{})
	ctx := context.Background()
	first, _ := getSshWorkspaceFs(ctx, "remote-dev", sshTerminal("old.example"))
	if again, _ := getSshWorkspaceFs(ctx, "remote-dev", sshTerminal("old.example")); again != first {
		t.Fatal("same config redialed")
	}
	second, _ := getSshWorkspaceFs(ctx, "remote-dev", sshTerminal("new.example"))
	if second == first || !reflect.DeepEqual(g.cleaned, []string{"old.example"}) {
		t.Fatalf("second %p first %p cleaned %v", second, first, g.cleaned)
	}
	g.failing["down.example"] = true
	_, err := getSshWorkspaceFs(ctx, "remote-dev", sshTerminal("down.example"))
	var fsErr *SshWorkspaceFsError
	if !errors.As(err, &fsErr) || fsErr.Code != "ECONN" || !strings.Contains(fsErr.Error(), "Could not connect") {
		t.Fatalf("err %v", err)
	}
	if !reflect.DeepEqual(g.cleaned, []string{"old.example", "new.example"}) {
		t.Fatalf("cleaned %v", g.cleaned)
	}
	if _, ok := sshFsBackends["remote-dev"]; ok {
		t.Fatal("failed redial left a cached backend")
	}
}

type sharedSocketSSH struct {
	keyPath, socket string
	events          *[]any
	onCleanup       func(*sharedSocketSSH)
	fakeSSHFsEnv
}

func (s *sharedSocketSSH) Cleanup(bool) error {
	s.onCleanup(s)
	return nil
}

func sshKeyTerminal(key string) map[string]any {
	terminal := sshTerminal("ssh.example")
	terminal["ssh_key"] = key
	return terminal
}

func TestSshWorkspaceFactoryClosesReplacedConnectionBeforeDialingNewKey(t *testing.T) {
	isolateSSHFsFactory(t)
	var events [][2]string
	openMasters := map[string]bool{}
	newSSHWorkspaceEnv = func(_ context.Context, opts tools.SSHOptions) (sshFsEnv, error) {
		socket := opts.User + "@" + opts.Host + ":" + strconv.Itoa(opts.Port)
		events = append(events, [2]string{"dial", opts.KeyPath})
		if !openMasters[socket] {
			if opts.KeyPath == "/keys/revoked" {
				return nil, errors.New("Permission denied (publickey)")
			}
			openMasters[socket] = true
		}
		env := &sharedSocketSSH{keyPath: opts.KeyPath, socket: socket,
			fakeSSHFsEnv: fakeSSHFsEnv{cwd: opts.Cwd, timeout: opts.Timeout, remoteHome: "/home/dev"}}
		env.onCleanup = func(s *sharedSocketSSH) {
			events = append(events, [2]string{"cleanup", s.keyPath})
			delete(openMasters, s.socket)
		}
		return env, nil
	}
	ctx := context.Background()
	if _, err := getSshWorkspaceFs(ctx, "remote-dev", sshKeyTerminal("/keys/old")); err != nil {
		t.Fatal(err)
	}
	replacement, err := getSshWorkspaceFs(ctx, "remote-dev", sshKeyTerminal("/keys/new"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(events, [][2]string{{"dial", "/keys/old"}, {"cleanup", "/keys/old"}, {"dial", "/keys/new"}}) {
		t.Fatalf("events %v", events)
	}
	if !reflect.DeepEqual(openMasters, map[string]bool{"dev@ssh.example:22": true}) || sshFsBackends["remote-dev"].backend != replacement {
		t.Fatalf("masters %v", openMasters)
	}
	_, err = getSshWorkspaceFs(ctx, "remote-dev", sshKeyTerminal("/keys/revoked"))
	var fsErr *SshWorkspaceFsError
	if !errors.As(err, &fsErr) || fsErr.Code != "ECONN" || !strings.Contains(fsErr.Error(), "Could not connect") {
		t.Fatalf("err %v", err)
	}
	if !reflect.DeepEqual(events[3:], [][2]string{{"cleanup", "/keys/new"}, {"dial", "/keys/revoked"}}) {
		t.Fatalf("events %v", events)
	}
	if _, ok := sshFsBackends["remote-dev"]; ok {
		t.Fatal("failed dial left a cached backend")
	}
}

func TestSshWorkspaceFactoryKeepsSameEndpointCleanupAndDialApart(t *testing.T) {
	isolateSSHFsFactory(t)
	progress := &sshFsProgress{}
	waits := recordLockWaits(progress)
	var events []any
	sockets := map[string]string{}
	cleaning, release := make(chan struct{}), make(chan struct{})
	var cleaningOnce, releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	newSSHWorkspaceEnv = func(_ context.Context, opts tools.SSHOptions) (sshFsEnv, error) {
		socket := opts.User + "@" + opts.Host + ":" + strconv.Itoa(opts.Port)
		progress.mu.Lock()
		events = append(events, [2]string{"dial", opts.KeyPath})
		if _, ok := sockets[socket]; !ok {
			sockets[socket] = opts.KeyPath
		}
		progress.mu.Unlock()
		env := &sharedSocketSSH{keyPath: opts.KeyPath, socket: socket,
			fakeSSHFsEnv: fakeSSHFsEnv{cwd: opts.Cwd, timeout: opts.Timeout, remoteHome: "/home/dev"}}
		env.onCleanup = func(s *sharedSocketSSH) {
			progress.mu.Lock()
			events = append(events, [2]string{"exit", s.keyPath})
			delete(sockets, s.socket)
			progress.mu.Unlock()
			cleaningOnce.Do(func() { close(cleaning) })
			waitChan(release, 5*time.Second)
			progress.mu.Lock()
			popped, ok := sockets[s.socket]
			delete(sockets, s.socket)
			var poppedValue any
			if ok {
				poppedValue = popped
			}
			events = append(events, [3]any{"unlink", s.keyPath, poppedValue})
			progress.mu.Unlock()
		}
		return env, nil
	}
	ctx := context.Background()
	if _, err := getSshWorkspaceFs(ctx, "alpha", sshKeyTerminal("/keys/alpha-old")); err != nil {
		t.Fatal(err)
	}
	betaDone, alphaDone, betaExit := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(alphaDone)
		_, _ = getSshWorkspaceFs(ctx, "alpha", sshKeyTerminal("/keys/alpha-new"))
	}()
	if !waitChan(cleaning, 5*time.Second) {
		t.Fatal("alpha cleanup never started")
	}
	betaEndpoint := sshFsEndpoint(sshFsConfig{user: "dev", host: "ssh.example", port: 22})
	go func() {
		defer close(betaExit)
		_, _ = getSshWorkspaceFs(ctx, "beta", sshKeyTerminal("/keys/beta"))
		close(betaDone)
	}()
	contended := progress.waitFor(func() bool {
		select {
		case <-betaDone:
			return true
		default:
		}
		for _, key := range *waits {
			if key == betaEndpoint {
				return true
			}
		}
		return false
	}, 5*time.Second)
	releaseOnce.Do(func() { close(release) })
	if !contended {
		t.Fatal("beta neither finished nor waited")
	}
	if !waitChan(alphaDone, 5*time.Second) || !waitChan(betaExit, 5*time.Second) {
		t.Fatal("a caller is still running")
	}
	wantHead := []any{[2]string{"dial", "/keys/alpha-old"}, [2]string{"exit", "/keys/alpha-old"}, [3]any{"unlink", "/keys/alpha-old", nil}}
	if !reflect.DeepEqual(events[:3], wantHead) {
		t.Fatalf("events %v", events)
	}
	tail := map[[2]string]bool{}
	for _, e := range events[3:] {
		tail[e.([2]string)] = true
	}
	if len(events[3:]) != 2 || !tail[[2]string{"dial", "/keys/alpha-new"}] || !tail[[2]string{"dial", "/keys/beta"}] {
		t.Fatalf("events %v", events)
	}
	if len(sockets) != 1 || sockets["dev@ssh.example:22"] == "" {
		t.Fatalf("sockets %v", sockets)
	}
}

func TestSshWorkspaceFactoryDoesNotHoldUnrelatedEndpointLocks(t *testing.T) {
	isolateSSHFsFactory(t)
	progress := &sshFsProgress{}
	g := installGatedSSH(t, progress)
	waits := recordLockWaits(progress)
	ctx := context.Background()
	first, _ := getSshWorkspaceFs(ctx, "remote-dev", sshTerminal("old.example"))
	progress.mu.Lock()
	g.hanging["new.example"] = true
	progress.mu.Unlock()
	var moved, other *SshWorkspaceFs
	moverDone, openerDone := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(moverDone)
		moved, _ = getSshWorkspaceFs(ctx, "remote-dev", sshTerminal("new.example"))
	}()
	if !waitChan(g.dialing, 5*time.Second) {
		t.Fatal("mover dial never started")
	}
	progress.mu.Lock()
	cleaned := append([]string(nil), g.cleaned...)
	progress.mu.Unlock()
	if !reflect.DeepEqual(cleaned, []string{"old.example"}) {
		t.Fatalf("cleaned %v", cleaned)
	}
	go func() {
		defer close(openerDone)
		other, _ = getSshWorkspaceFs(ctx, "other", sshTerminal("old.example"))
	}()
	if !waitChan(openerDone, 2*time.Second) {
		g.releaseAll()
		t.Fatal("a hanging dial on one endpoint blocked a dial on another")
	}
	if other == first {
		t.Fatal("other profile reused the replaced backend")
	}
	g.releaseAll()
	waitChan(moverDone, 5*time.Second)
	if moved == nil || moved == first {
		t.Fatal("mover did not get a new backend")
	}
	if len(*waits) != 0 {
		t.Fatalf("lock waits %v", *waits)
	}
}

func TestSshWorkspaceFactoryFailedCleanupDoesNotWedgeTheProfile(t *testing.T) {
	isolateSSHFsFactory(t)
	g := installGatedSSH(t, &sshFsProgress{})
	ctx := context.Background()
	first, _ := getSshWorkspaceFs(ctx, "remote-dev", sshTerminal("old.example"))
	if first == nil {
		t.Fatal("no first backend")
	}
	first.env.(*fakeSSHFsEnv).cleanup = func() error { return errors.New("cleanup failed") }
	if _, err := getSshWorkspaceFs(ctx, "remote-dev", sshTerminal("new.example")); err == nil || !strings.Contains(err.Error(), "cleanup failed") {
		t.Fatalf("err %v", err)
	}
	dial := sshFsDialLocks.lockFor("remote-dev").(*sync.Mutex)
	if !dial.TryLock() {
		t.Fatal("dial lock still held")
	}
	dial.Unlock()
	if _, ok := sshFsBackends["remote-dev"]; ok {
		t.Fatal("failed cleanup left a cached backend")
	}
	second, _ := getSshWorkspaceFs(ctx, "remote-dev", sshTerminal("new.example"))
	if second == first || !reflect.DeepEqual(g.created, []string{"old.example", "new.example"}) {
		t.Fatalf("second %p created %v", second, g.created)
	}
}
