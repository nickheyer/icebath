package plugins

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/proc"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	hostStartTimeout    = 30 * time.Second
	hostShutdownGrace   = 3 * time.Second
	hostRestartBudget   = 3
	hostRestartWindow   = 600 * time.Second
	hostRestartWindowS  = 600.0
	hostExitWaitTimeout = 500 * time.Millisecond
)

// One host process for one manager, running the Python code of that profile plugins
type PluginHost struct {
	manager     *Manager
	home        string
	envOverride map[string]string
	mu          sync.Mutex
	proc        *hostProcess
	channel     *Channel
	stateMu     sync.Mutex
	contexts    map[string]*Context
	handles     map[int64]*Registration
	handleIDs   int64
	baseCtx     context.Context
	loading     string
	starting    *hostStart
	stopping    bool
	deaths      []time.Time
	generation  int
	releases    []int64
	info        map[string]any
	restartWait func(time.Duration)
	parents     map[int64]parentCallableSlot
	parentIDs   int64
}

// One started child with its exit status once known
type hostProcess struct {
	cmd  *exec.Cmd
	done chan struct{}
	code int
}

// Exit code once the process has exited, a signal death reported as the negative signal number
func (p *hostProcess) poll() (int, bool) {
	select {
	case <-p.done:
		return p.code, true
	default:
		return 0, false
	}
}

func newPluginHost(m *Manager) (*PluginHost, error) {
	h := &PluginHost{manager: m, home: m.home, contexts: map[string]*Context{}, handles: map[int64]*Registration{},
		info: map[string]any{}, restartWait: time.Sleep, parents: map[int64]parentCallableSlot{}}
	h.baseCtx = h.buildBaseContext()
	return h, nil
}

// Process id of the running child, zero when none
func (h *PluginHost) PID() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.proc == nil {
		return 0
	}
	return h.proc.cmd.Process.Pid
}

// Profile home this host serves
func (h *PluginHost) Home() string { return h.home }

// Hello reply of the running child
func (h *PluginHost) Info() map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.info
}

// Reports whether the channel is open and the child has not exited
func (h *PluginHost) Alive() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.aliveLocked()
}

func (h *PluginHost) aliveLocked() bool {
	if h.channel == nil || h.proc == nil {
		return false
	}
	if _, closed := h.channel.ClosedReason(); closed {
		return false
	}
	_, exited := h.proc.poll()
	return !exited
}

// Fresh context bound to this profile home and its secret scope, never a caller session
func (h *PluginHost) buildBaseContext() context.Context {
	ctx := home.WithOverride(context.Background(), h.home)
	if s := installedServices(); s != nil && s.BindProfileSecretScope != nil && tools.MultiplexActive() {
		ctx = s.BindProfileSecretScope(ctx, h.home)
	}
	return ctx
}

// Launcher, interpreter and module argv of the child
func (h *PluginHost) argv(ctx context.Context, extra ...string) ([]string, error) {
	launcher, err := hostLauncher(h.home)
	if err != nil {
		return nil, err
	}
	python, err := hostInterpreter(ctx)
	if err != nil {
		return nil, err
	}
	return hostChildArgv(launcher, python, extra...)
}

// Child environment acting for this profile with its hosted ambient credentials and the resource path
func (h *PluginHost) env() ([]string, error) {
	env, err := tools.ServedProfileChildEnv(h.baseCtx, nil, nil, h.home, true)
	if err != nil {
		return nil, err
	}
	ambient, err := hostedAmbientCredentials(h.baseCtx, h.home)
	if err != nil {
		return nil, err
	}
	for k, v := range ambient {
		env[k] = v
	}
	pythonPath, err := hostPythonPath(h.baseCtx, env["PYTHONPATH"])
	if err != nil {
		return nil, err
	}
	env["PYTHONPATH"] = pythonPath
	if _, ok := env[HostLogLevelEnv]; !ok {
		env[HostLogLevelEnv] = "WARNING"
	}
	env[HostProcessEnv] = "1"
	for k, v := range h.envOverride {
		env[k] = v
	}
	return envList(env), nil
}

func envList(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

// Starts the child when it is not running and returns its channel after the hello handshake
func (h *PluginHost) ensureStarted(ctx context.Context) (*Channel, error) {
	h.mu.Lock()
	if h.starting == nil && h.aliveLocked() {
		ch := h.channel
		h.mu.Unlock()
		return ch, nil
	}
	s := h.starting
	if s == nil {
		s = &hostStart{done: make(chan struct{})}
		h.starting, h.stopping = s, false
		go h.runStart(context.WithoutCancel(ctx), s)
	}
	h.mu.Unlock()
	select {
	case <-s.done:
	case <-ctx.Done():
		return nil, &UserInterrupt{Err: ctx.Err()}
	}
	if s.panicked != nil {
		panic(s.panicked)
	}
	return s.ch, s.err
}

// Start every concurrent caller waits on, resolved once by the starter or by a shutdown
type hostStart struct {
	done     chan struct{}
	once     sync.Once
	argv     []string
	ch       *Channel
	err      error
	panicked any
}

// Settles the start and releases its waiters, reporting whether this call settled it
func (s *hostStart) resolve(ch *Channel, err error, panicked any) bool {
	settled := false
	s.once.Do(func() {
		s.ch, s.err, s.panicked = ch, err, panicked
		settled = true
		close(s.done)
	})
	return settled
}

// Failure every waiter of the start reports, naming the argv once it is known
func (s *hostStart) failure(argv []string, err any) error {
	if argv == nil {
		return &PluginHostUnavailable{Reason: fmt.Sprintf("plugin host failed to start: %v", err)}
	}
	return &PluginHostUnavailable{Reason: fmt.Sprintf("plugin host failed to start (%s): %v", strings.Join(argv, " "), err)}
}

// Spawns the child and runs the hello handshake, settling the start with the channel or the failure
func (h *PluginHost) runStart(ctx context.Context, s *hostStart) {
	var spawned *hostProcess
	var ch *Channel
	defer func() {
		if p := recover(); p != nil {
			h.abandonStart(s, spawned, ch)
			s.resolve(nil, nil, p)
		}
	}()
	argv, err := h.argv(ctx)
	if err != nil {
		h.abandonStart(s, nil, nil)
		s.resolve(nil, s.failure(nil, err), nil)
		return
	}
	h.mu.Lock()
	s.argv = argv
	h.mu.Unlock()
	ch, spawned, err = h.spawn(argv, s)
	if err != nil {
		h.abandonStart(s, spawned, ch)
		s.resolve(nil, s.failure(argv, err), nil)
		return
	}
	info, err := ch.Call(ctx, "hello", map[string]any{}, hostStartTimeout)
	if err != nil {
		h.abandonStart(s, spawned, ch)
		s.resolve(nil, s.failure(argv, err), nil)
		return
	}
	h.mu.Lock()
	current := h.starting == s
	if current {
		h.info, _ = info.(map[string]any)
		h.starting = nil
	}
	h.mu.Unlock()
	if !current {
		return
	}
	logInfo(fmt.Sprintf("Plugin host started for %s (pid %d)", h.home, spawned.cmd.Process.Pid))
	s.resolve(ch, nil, nil)
}

// Drops a failed or shut down start and kills the child it spawned, the process group included
func (h *PluginHost) abandonStart(s *hostStart, spawned *hostProcess, ch *Channel) {
	h.mu.Lock()
	if h.starting == s {
		h.starting = nil
		h.stopping = true
	}
	if spawned != nil && h.proc == spawned {
		h.proc, h.channel = nil, nil
	}
	h.mu.Unlock()
	if ch != nil {
		ch.Close("shutdown")
	}
	if spawned != nil {
		spawned.kill()
	}
}

// Launches the child in its own process group and installs it unless a shutdown abandoned the start
func (h *PluginHost) spawn(argv []string, s *hostStart) (*Channel, *hostProcess, error) {
	env, err := h.env()
	if err != nil {
		return nil, nil, err
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env, cmd.Dir = env, h.home
	proc.Group(cmd)
	var parentEnds, childEnds []*os.File
	closeAll := func(files []*os.File) {
		for _, f := range files {
			_ = f.Close()
		}
	}
	for i := 0; i < 3; i++ {
		r, w, err := os.Pipe()
		if err != nil {
			closeAll(parentEnds)
			closeAll(childEnds)
			return nil, nil, err
		}
		if i == 0 {
			parentEnds, childEnds = append(parentEnds, w), append(childEnds, r)
		} else {
			parentEnds, childEnds = append(parentEnds, r), append(childEnds, w)
		}
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = childEnds[0], childEnds[1], childEnds[2]
	terminal, releaseTerminal, err := attachTerminal(cmd)
	if err != nil {
		closeAll(parentEnds)
		closeAll(childEnds)
		return nil, nil, err
	}
	cmd.Env = append(cmd.Env, HostTerminalEnv+"="+terminal)
	err = cmd.Start()
	releaseTerminal()
	closeAll(childEnds)
	if err != nil {
		closeAll(parentEnds)
		return nil, nil, err
	}
	stdin, stdout, stderr := parentEnds[0], parentEnds[1], parentEnds[2]
	child := &hostProcess{cmd: cmd, done: make(chan struct{})}
	go func() {
		pumpHostStderr(stderr, filepath.Base(h.home))
		_ = stderr.Close()
	}()
	go func() {
		_ = cmd.Wait()
		child.code = exitCode(cmd.ProcessState)
		close(child.done)
	}()
	h.mu.Lock()
	if h.starting != s {
		h.mu.Unlock()
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, child, fmt.Errorf("shutdown")
	}
	h.generation++
	generation := h.generation
	ch := newChannel(stdout, stdin, h.handle, fmt.Sprintf("plugin-host[%d]", cmd.Process.Pid),
		func(reason string) { h.onChannelClose(generation, reason) }, h.contextForOrigin)
	h.proc, h.channel = child, ch
	h.releases = nil
	h.mu.Unlock()
	ch.Start()
	return ch, child, nil
}

// Kills the child and every process of its group, then waits a grace period for the exit status
func (p *hostProcess) kill() {
	if _, exited := p.poll(); !exited {
		_ = proc.Kill(p.cmd)
	}
	select {
	case <-p.done:
	case <-time.After(hostShutdownGrace):
	}
}

// Python returncode of a finished process, negative for a signal death
func exitCode(state *os.ProcessState) int {
	if state == nil {
		return -1
	}
	if ws, ok := state.Sys().(interface {
		Signaled() bool
		Signal() syscall.Signal
	}); ok && ws.Signaled() {
		return -int(ws.Signal())
	}
	return state.ExitCode()
}

// Reason every proxy reports once the child is gone
func (h *PluginHost) exitReason() string {
	h.mu.Lock()
	proc := h.proc
	h.mu.Unlock()
	if proc == nil {
		return "the plugin host is not running"
	}
	code, exited := proc.poll()
	if !exited {
		select {
		case <-proc.done:
			code, exited = proc.code, true
		case <-time.After(hostExitWaitTimeout):
		}
	}
	if !exited {
		return "the plugin host is not running"
	}
	return fmt.Sprintf("the plugin host for this profile exited (code %d); Icebath restarts it and reloads its plugins automatically", code)
}

// Records an unexpected child death and reloads its plugins in the background, the loading culprit excluded
func (h *PluginHost) onChannelClose(generation int, reason string) {
	h.dropParentCallables(generation)
	h.mu.Lock()
	stale := h.stopping || generation != h.generation
	h.mu.Unlock()
	if stale {
		return
	}
	h.stateMu.Lock()
	culprit := h.loading
	keys := []string{}
	h.manager.mu.RLock()
	for key := range h.contexts {
		if _, loaded := h.manager.plugins[key]; key != culprit && loaded {
			keys = append(keys, key)
		}
	}
	h.manager.mu.RUnlock()
	sort.Strings(keys)
	now := time.Now()
	kept := []time.Time{}
	for _, t := range h.deaths {
		if now.Sub(t) < hostRestartWindow {
			kept = append(kept, t)
		}
	}
	h.deaths = append(kept, now)
	h.stateMu.Unlock()
	suffix := ""
	if culprit != "" {
		suffix = fmt.Sprintf(" while loading plugin '%s'", culprit)
	}
	logWarn(fmt.Sprintf("Plugin host for %s exited (%s)%s", h.home, h.exitReason(), suffix))
	if len(keys) > 0 {
		go h.restart(keys)
	}
}

// Reports and logs whether the restart budget of the window is spent
func (h *PluginHost) restartRefused(what string) bool {
	h.stateMu.Lock()
	deaths := len(h.deaths)
	h.stateMu.Unlock()
	if deaths <= hostRestartBudget {
		return false
	}
	logError(fmt.Sprintf("Plugin host for %s died %d times in %.0fs; not restarting it again (%s)", h.home, deaths, hostRestartWindowS, what))
	return true
}

// Unloads and reloads every enabled plugin the dead child served
func (h *PluginHost) restart(keys []string) {
	if h.restartRefused("plugins: " + strings.Join(keys, ", ")) {
		return
	}
	h.stateMu.Lock()
	deaths := len(h.deaths)
	h.stateMu.Unlock()
	h.restartWait(time.Duration(deaths) * 500 * time.Millisecond)
	m := h.manager
	for _, key := range keys {
		m.mu.RLock()
		loaded := m.plugins[key]
		m.mu.RUnlock()
		if loaded == nil || !loaded.Enabled {
			continue
		}
		m.Unload(key)
		if err := m.loadPlugin(h.baseCtx, loaded.Manifest); err != nil {
			logWarn(fmt.Sprintf("Failed to load plugin '%s': %s", loaded.Manifest.Name, loadErrorText(err)))
		}
	}
}

// Stops the child, asking it to run unload callbacks first and killing it after the grace period
func (h *PluginHost) Shutdown() {
	h.mu.Lock()
	h.stopping = true
	child, ch, pending := h.proc, h.channel, h.starting
	h.proc, h.channel, h.starting = nil, nil, nil
	var argv []string
	if pending != nil {
		argv = pending.argv
	}
	h.mu.Unlock()
	if pending != nil {
		pending.resolve(nil, pending.failure(argv, "shutdown"), nil)
		if ch != nil {
			ch.Close("shutdown")
		}
		if child != nil {
			child.kill()
		}
		return
	}
	if ch != nil {
		if _, closed := ch.ClosedReason(); !closed {
			asked := make(chan struct{})
			go func() {
				_, _ = ch.Call(context.Background(), "shutdown", map[string]any{}, hostShutdownGrace)
				close(asked)
			}()
			select {
			case <-asked:
			case <-time.After(hostShutdownGrace):
			}
			ch.Close("shutdown")
		}
	}
	if child != nil {
		select {
		case <-child.done:
		case <-time.After(hostShutdownGrace):
			child.kill()
		}
	}
}

// Public method names of the plugin context the child mirrors so attribute probes answer alike
func hostCtxMethods() []string {
	names := map[string]bool{"manifest": true, "plugin_id": true, "profile_name": true, "on_unload": true, "spawn_task": true,
		"apost_message": true, "post_message": true}
	for name := range hostCtxDispatch {
		names[name] = true
	}
	for name := range HostUnsupportedCtxMethods {
		names[name] = true
	}
	for name := range HostSkippedCtxMethods {
		names[name] = true
	}
	for name := range HostRemoteFacades {
		names[name] = true
	}
	return sortedKeys(names)
}

// Unsupported registrars the parent serves for a plugin loaded under in process isolation, none under host isolation
func hostInProcessCtxMethods(ctx context.Context) map[string]bool {
	served := map[string]bool{}
	if IsolationMode(ctx, nil) != IsolationInProcess {
		return served
	}
	for name := range HostInProcessObjectBases {
		served[name] = true
	}
	for name := range hostInProcessServed {
		served[name] = true
	}
	return served
}

// Imports a plugin in the child and replays its registrations onto the plugin context
func (h *PluginHost) Load(ctx context.Context, mf *Manifest, pctx *Context, moduleName string, entrypoint, predeclared bool) (string, error) {
	ch, err := h.ensureStarted(ctx)
	if err != nil {
		return "", err
	}
	key := mf.ID()
	served := hostInProcessCtxMethods(ctx)
	h.stateMu.Lock()
	h.contexts[key] = pctx
	pctx.hostInProcess = served
	h.loading = key
	h.stateMu.Unlock()
	params := map[string]any{
		"plugin_key": key, "plugin_id": pctx.PluginID(), "name": mf.Name, "path": mf.Path, "module_name": nilIfEmpty(moduleName),
		"entrypoint": entrypoint, "predeclared": predeclared, "profile_name": pctx.ProfileName(h.baseCtx), "manifest": hostManifestInfo(mf),
		"ctx_methods": anyList(hostCtxMethods()), "in_process_ctx_methods": anyList(sortedKeys(served)),
	}
	result, err := ch.Call(ctx, "load", params, 0)
	h.stateMu.Lock()
	h.loading = ""
	if err != nil {
		delete(h.contexts, key)
	}
	h.stateMu.Unlock()
	if err != nil {
		return "", h.unavailableAsExit(err)
	}
	_, err = pctx.OnUnload(func() error { return h.unload(key) })
	return replyModuleFile(result), err
}

// Module file the child reported for an imported plugin, empty for a module without one
func replyModuleFile(result any) string {
	reply, _ := result.(map[string]any)
	return wireStr(reply["file"])
}

// Imports a deferred platform plugin tools.py in the child and runs its register_tools(ctx)
func (h *PluginHost) LoadTools(ctx context.Context, mf *Manifest, pctx *Context, moduleName string) (string, error) {
	ch, err := h.ensureStarted(ctx)
	if err != nil {
		return "", err
	}
	key := mf.ID()
	served := hostInProcessCtxMethods(ctx)
	h.stateMu.Lock()
	h.contexts[key] = pctx
	pctx.hostInProcess = served
	h.loading = key
	h.stateMu.Unlock()
	params := map[string]any{
		"plugin_key": key, "plugin_id": pctx.PluginID(), "name": mf.Name, "path": mf.Path, "module_name": moduleName,
		"profile_name": pctx.ProfileName(h.baseCtx), "manifest": hostManifestInfo(mf), "ctx_methods": anyList(hostCtxMethods()),
		"in_process_ctx_methods": anyList(sortedKeys(served)),
	}
	result, err := ch.Call(ctx, "load_tools", params, 0)
	h.stateMu.Lock()
	h.loading = ""
	h.stateMu.Unlock()
	if err != nil {
		return "", h.unavailableAsExit(err)
	}
	if reply, _ := result.(map[string]any); reply != nil && reply["register_tools"] == false {
		logWarn(fmt.Sprintf("Plugin '%s' declares provides_tools %s but its tools.py has no register_tools(ctx); those tools will not be available in CLI/TUI sessions.",
			key, pyListRepr(mf.ProvidesTools)))
	}
	_, err = pctx.OnUnload(func() error { return h.unload(key) })
	return replyModuleFile(result), err
}

func pyListRepr(items []string) string { return config.PyRepr(anyList(items)) }

func hostManifestInfo(mf *Manifest) map[string]any {
	return map[string]any{"name": mf.Name, "version": mf.Version, "description": mf.Description, "author": mf.Author,
		"source": mf.Source, "path": mf.Path, "key": mf.Key, "kind": mf.Kind, "data_namespace": mf.DataNamespace}
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Loads a category plugin in the child and returns a proxy that reloads it after a host restart
func (h *PluginHost) LoadInstance(ctx context.Context, pluginDir, moduleName, baseRef, capture string, pctx *Context, beforeReload func(context.Context)) (*HostedObject, error) {
	return h.loadInstance(ctx, pluginDir, moduleName, baseRef, capture, pctx, beforeReload, nil)
}

// Category plugin load whose extra params ask the child for the in process loader path
func (h *PluginHost) loadInstance(ctx context.Context, pluginDir, moduleName, baseRef, capture string, pctx *Context, beforeReload func(context.Context), extra map[string]any) (*HostedObject, error) {
	name := filepath.Base(pluginDir)
	load := func(ctx context.Context) (map[string]any, error) {
		pluginID := name
		if pctx != nil {
			pluginID = pctx.PluginID()
		}
		params := map[string]any{
			"plugin_id": pluginID, "name": name, "path": pluginDir, "module_name": moduleName,
			"base": baseRef, "capture": capture, "forward": pctx != nil,
			"manifest": map[string]any{"name": name, "path": pluginDir, "source": "user"},
		}
		for k, v := range extra {
			params[k] = v
		}
		return h.instanceRef(ctx, capture+":"+name, "load_instance", params, pctx)
	}
	return h.reloadableInstance(ctx, capture, name, baseRef, load, beforeReload)
}

// Proxy over the object load answers, reloading it into a restarted child after beforeReload
func (h *PluginHost) reloadableInstance(ctx context.Context, capture, name, baseRef string, load func(context.Context) (map[string]any, error), beforeReload func(context.Context)) (*HostedObject, error) {
	ref, err := load(ctx)
	if err != nil || ref == nil {
		return nil, err
	}
	reload := func(ctx context.Context) (map[string]any, error) {
		if h.restartRefused(fmt.Sprintf("%s plugin %s", capture, name)) {
			return nil, &PluginHostUnavailable{Reason: h.exitReason()}
		}
		if beforeReload != nil {
			beforeReload(ctx)
		}
		fresh, err := load(ctx)
		if err != nil {
			return nil, err
		}
		if fresh == nil {
			return nil, &PluginHostUnavailable{Reason: fmt.Sprintf("plugin %s no longer loads in the plugin host", config.PyRepr(name))}
		}
		return fresh, nil
	}
	return h.objectProxy(ref, baseRef, reload), nil
}

// Object ref one instance loading request answers, pctx serving the registrations it forwards
func (h *PluginHost) instanceRef(ctx context.Context, key, method string, params map[string]any, pctx *Context) (map[string]any, error) {
	ch, err := h.ensureStarted(ctx)
	if err != nil {
		return nil, err
	}
	h.stateMu.Lock()
	if pctx != nil {
		h.contexts[key] = pctx
	}
	h.loading = key
	h.stateMu.Unlock()
	params["plugin_key"] = key
	if _, ok := params["ctx_methods"]; !ok {
		params["ctx_methods"] = anyList(hostCtxMethods())
	}
	raw, err := ch.Call(ctx, method, params, 0)
	h.stateMu.Lock()
	h.loading = ""
	h.stateMu.Unlock()
	if err != nil {
		return nil, h.unavailableAsExit(err)
	}
	ref, _ := raw.(map[string]any)
	if ref != nil {
		h.mu.Lock()
		ref["generation"] = int64(h.generation)
		h.mu.Unlock()
	}
	return ref, nil
}

// Runs a user memory provider config_schema.py in the child and returns its CONFIG_SCHEMA
func (h *PluginHost) ConfigSchema(ctx context.Context, path string) (any, error) {
	if _, err := h.ensureStarted(ctx); err != nil {
		return nil, err
	}
	return h.call(ctx, "config_schema", map[string]any{"path": path}, nil, 0, nil)
}

// Runs a model provider profile overridden method or callable field in the child
func (h *PluginHost) ProfileCall(ctx context.Context, pluginDir, moduleName, profile, attr string, args []any, kwargs map[string]any) (any, error) {
	if _, err := h.ensureStarted(ctx); err != nil {
		return nil, err
	}
	return h.call(ctx, "profile_call", map[string]any{"path": pluginDir, "module_name": moduleName, "profile": profile,
		"attr": attr, "args": h.encodeArgs(nonNilArgs(args)), "kwargs": h.encodeArgs(nonNilKwargs(kwargs))}, nil, 0, nil)
}

// Serves one dashboard plugin API request with the plugin router in the child, a TimeoutError after timeout
func (h *PluginHost) ASGIRequest(ctx context.Context, pluginName, dashboardDir, apiFile, method, path, query string,
	headers [][2]string, body []byte, timeout time.Duration, authenticated bool) (map[string]any, error) {
	if _, err := h.ensureStarted(ctx); err != nil {
		return nil, err
	}
	pairs := make([]any, len(headers))
	for i, hd := range headers {
		pairs[i] = []any{hd[0], hd[1]}
	}
	if body == nil {
		body = []byte{}
	}
	raw, err := h.call(ctx, "asgi", map[string]any{"plugin": pluginName, "dashboard_dir": dashboardDir, "api_file": apiFile,
		"method": method, "path": path, "query": query, "authenticated": authenticated, "headers": pairs,
		"body": wireEncode(body, nil)}, nil, timeout, nil)
	if err != nil {
		return nil, err
	}
	reply, _ := raw.(map[string]any)
	return reply, nil
}

// Drops a plugin from the child, logging each on_unload callback failure the child reports
func (h *PluginHost) unload(key string) error {
	h.stateMu.Lock()
	delete(h.contexts, key)
	h.stateMu.Unlock()
	h.mu.Lock()
	ch := h.channel
	h.mu.Unlock()
	if ch == nil {
		return nil
	}
	if _, closed := ch.ClosedReason(); closed {
		return nil
	}
	raw, err := ch.Call(h.baseCtx, "unload", map[string]any{"plugin_key": key}, 0)
	if err != nil {
		return err
	}
	reply, _ := raw.(map[string]any)
	errs, _ := reply["errors"].([]any)
	for _, e := range errs {
		logWarn(fmt.Sprintf("Plugin '%s' on_unload callback failed in the plugin host: %s", key, wireStr(e)))
	}
	return nil
}

// Drops a module and its submodules from a running child, failing when it does not answer in time
func (h *PluginHost) evictModules(moduleName string) error {
	h.mu.Lock()
	ch := h.channel
	h.mu.Unlock()
	if ch == nil {
		return nil
	}
	if _, closed := ch.ClosedReason(); closed {
		return nil
	}
	_, err := ch.Call(h.baseCtx, "evict_modules", map[string]any{"module_name": moduleName}, hostShutdownGrace)
	return err
}

func (h *PluginHost) unavailableAsExit(err error) error {
	var un *PluginHostUnavailable
	if errors.As(err, &un) {
		return &PluginHostUnavailable{Reason: h.exitReason()}
	}
	return err
}

// Calls the child, flushing queued releases first and rebuilding object refs of a declared provider return
func (h *PluginHost) call(ctx context.Context, method string, params map[string]any, generation *int, timeout time.Duration, returns *pyHint) (any, error) {
	return h.callIn(ctx, "", method, params, generation, timeout, returns)
}

// One child request whose object refs resolve under base, the base the return hint declares when empty
func (h *PluginHost) callIn(ctx context.Context, forcedBase, method string, params map[string]any, generation *int, timeout time.Duration, returns *pyHint) (any, error) {
	h.mu.Lock()
	ch := h.channel
	current := h.generation
	h.mu.Unlock()
	if ch == nil {
		return nil, h.unavailableAsExit(&PluginHostUnavailable{Reason: "not started"})
	}
	if generation != nil && *generation != current {
		return nil, h.unavailableAsExit(&PluginHostUnavailable{Reason: "this object belonged to a plugin host process that has exited"})
	}
	h.mu.Lock()
	released := h.releases
	h.releases = nil
	h.mu.Unlock()
	if len(released) > 0 {
		refs := make([]any, len(released))
		for i, r := range released {
			refs[i] = r
		}
		if _, err := ch.Call(ctx, "release", map[string]any{"refs": refs}, 0); err != nil {
			return nil, h.unavailableAsExit(err)
		}
	}
	raw, err := ch.Call(ctx, method, params, timeout)
	if err != nil {
		return nil, h.unavailableAsExit(err)
	}
	base := hostObjectBase(returns)
	if forcedBase != "" {
		base = forcedBase
	}
	value, err := wireDecode(raw, func(ref map[string]any) (any, error) { return h.resolveResultRef(ref, base) })
	if err != nil {
		return nil, err
	}
	return restoreHinted(value, returns)
}

// Queues an object release for the next call, dropped when the object belonged to an earlier child
func (h *PluginHost) release(generation int, ref int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if generation == h.generation {
		h.releases = append(h.releases, ref)
	}
}

func (h *PluginHost) invoke(ctx context.Context, ref int64, args []any, kwargs map[string]any, generation int) (any, error) {
	return h.call(ctx, "invoke", map[string]any{"ref": ref, "args": h.encodeArgs(nonNilArgs(args)),
		"kwargs": h.encodeArgs(nonNilKwargs(kwargs))}, &generation, 0, nil)
}

func nonNilArgs(args []any) []any {
	if args == nil {
		return []any{}
	}
	return args
}

func nonNilKwargs(kwargs map[string]any) map[string]any {
	if kwargs == nil {
		return map[string]any{}
	}
	return kwargs
}

// Caller context of the request of ours the child was serving, else the bare profile context
func (h *PluginHost) contextForOrigin(origin any) context.Context {
	h.mu.Lock()
	ch := h.channel
	h.mu.Unlock()
	if ch != nil {
		if caller := ch.contextOf(origin); caller != nil {
			return caller
		}
	}
	return h.baseCtx
}

// Serves one request the child sends to the parent
func (h *PluginHost) handle(ctx context.Context, method string, params map[string]any, _ any) (any, error) {
	switch method {
	case "ctx":
		return h.serveCtx(ctx, params)
	case "facade":
		return h.serveFacade(ctx, params)
	case "sdk":
		return h.serveSDK(ctx, params)
	case "parent_call":
		return h.serveParentCall(ctx, params)
	case "parent_event":
		return h.serveParentEvent(ctx, params)
	case "parent_object":
		return h.serveParentObject(ctx, params)
	case "release_parent":
		h.releaseParentCallables(listOrEmpty(params["ids"]))
		return nil, nil
	case "collect_source":
		return h.serveCollectSource(ctx, params)
	case "dispose":
		id, _ := wireInt(params["handle"])
		h.stateMu.Lock()
		reg := h.handles[id]
		delete(h.handles, id)
		h.stateMu.Unlock()
		if reg != nil {
			return nil, reg.Dispose()
		}
		return nil, nil
	}
	return nil, &HostBuiltinError{Type: "ValueError", Message: fmt.Sprintf("unknown plugin host request %s", config.PyRepr(method))}
}

func (h *PluginHost) pluginCtx(params map[string]any) (*Context, error) {
	h.stateMu.Lock()
	c := h.contexts[wireStr(params["plugin"])]
	h.stateMu.Unlock()
	if c == nil {
		return nil, &HostBuiltinError{Type: "LookupError", Message: fmt.Sprintf("plugin %s is not loaded in this host", config.PyRepr(params["plugin"]))}
	}
	return c, nil
}

// Replays one context call of a hosted plugin on its real plugin context
func (h *PluginHost) serveCtx(ctx context.Context, params map[string]any) (any, error) {
	c, err := h.pluginCtx(params)
	if err != nil {
		return nil, err
	}
	method := wireStr(params["method"])
	h.stateMu.Lock()
	served := c.hostInProcess[method]
	h.stateMu.Unlock()
	_, unsupported := HostUnsupportedCtxMethods[method]
	_, skipped := HostSkippedCtxMethods[method]
	if strings.HasPrefix(method, "_") || ((unsupported || skipped) && !served) || method == "on_unload" || method == "spawn_task" {
		return nil, &PluginHostUnsupported{Msg: fmt.Sprintf("ctx.%s() cannot be called across the plugin host", method)}
	}
	base := HostObjectBases[method]
	if served {
		base = HostInProcessObjectBases[method]
	}
	resolve := func(ref map[string]any) (any, error) { return h.resolveRef(ref, base) }
	rawArgs, err := wireDecode(listOrEmpty(params["args"]), resolve)
	if err != nil {
		return nil, err
	}
	rawKwargs, err := wireDecode(mapOrEmpty(params["kwargs"]), resolve)
	if err != nil {
		return nil, err
	}
	var result any
	if c.engineCollector != nil {
		result, err = c.engineCollector.serve(ctx, h, c, method, rawArgs.([]any), rawKwargs.(map[string]any))
	} else if c.collector != nil {
		result, err = c.collector.serve(ctx, h, c, method, rawArgs.([]any), rawKwargs.(map[string]any))
	} else {
		result, err = hostCtxCall(ctx, h, c, method, rawArgs.([]any), rawKwargs.(map[string]any))
	}
	if err != nil {
		return nil, err
	}
	return h.encodeForHost(result), nil
}

// Runs one facade attribute read or method call of a hosted plugin on the parent facade object
func (h *PluginHost) serveFacade(ctx context.Context, params map[string]any) (any, error) {
	c, err := h.pluginCtx(params)
	if err != nil {
		return nil, err
	}
	facade, method := wireStr(params["facade"]), wireStr(params["method"])
	refusal := &PluginHostUnsupported{Msg: fmt.Sprintf("ctx.%s.%s is not available in the plugin host", facade, method)}
	if !HostRemoteFacades[facade] || strings.HasPrefix(method, "_") {
		return nil, refusal
	}
	if c.collector != nil || c.engineCollector != nil {
		return nil, &HostBuiltinError{Type: "AttributeError", Message: facade}
	}
	obj, err := hostFacadeObject(ctx, c, facade)
	if err != nil {
		return nil, err
	}
	target, ok := obj.(HostFacade)
	if !ok {
		return nil, refusal
	}
	return h.serveFacadeAttr(ctx, target, method, params)
}

// Serves one parent object a hosted plugin holds, its repr, a plain attribute or a method call
func (h *PluginHost) serveParentObject(ctx context.Context, params map[string]any) (any, error) {
	id, _ := wireInt(params["id"])
	h.stateMu.Lock()
	slot, ok := h.parents[id]
	h.stateMu.Unlock()
	if !ok || slot.obj == nil {
		return nil, &HostBuiltinError{Type: "LookupError", Message: fmt.Sprintf("icebath object %d was released", id)}
	}
	if config.PyTruthy(params["repr"]) {
		return slot.obj.PyRepr(), nil
	}
	method := wireStr(params["method"])
	if strings.HasPrefix(method, "_") {
		return nil, &HostBuiltinError{Type: "AttributeError", Message: fmt.Sprintf("'%s' object has no attribute %s", slot.obj.PyTypeName(), config.PyRepr(method))}
	}
	return h.serveFacadeAttr(ctx, slot.obj, method, params)
}

// Reads one attribute of a parent facade, probing or calling it when it is a method
func (h *PluginHost) serveFacadeAttr(ctx context.Context, target HostFacade, method string, params map[string]any) (any, error) {
	value, isMethod, async, err := target.HostAttr(ctx, method)
	if err != nil {
		return nil, err
	}
	if !isMethod {
		return h.encodeForHost(value), nil
	}
	if config.PyTruthy(params["probe"]) {
		return map[string]any{"__method__": true, "async": async}, nil
	}
	resolve := func(ref map[string]any) (any, error) { return h.resolveRef(ref, "") }
	args, err := wireDecode(listOrEmpty(params["args"]), resolve)
	if err != nil {
		return nil, err
	}
	kwargs, err := wireDecode(mapOrEmpty(params["kwargs"]), resolve)
	if err != nil {
		return nil, err
	}
	result, err := target.HostCall(ctx, method, args.([]any), kwargs.(map[string]any))
	if err != nil {
		return nil, err
	}
	return h.encodeForHost(result), nil
}

// Facade object a plugin context exposes under a Python attribute name
func hostFacadeObject(ctx context.Context, c *Context, facade string) (any, error) {
	switch facade {
	case "state":
		return c.State(ctx), nil
	case "llm":
		return c.LLM(), nil
	case "platform_actions":
		return c.PlatformActions()
	case "subagent_lifecycle":
		return c.SubagentLifecycle(), nil
	case "delegation_backends":
		return c.DelegationBackends()
	}
	return nil, &PluginHostUnsupported{Msg: fmt.Sprintf("ctx.%s is not available in the plugin host", facade)}
}

// Parent object whose attributes and methods a hosted plugin reaches by Python name
type HostFacade interface {
	HostAttr(ctx context.Context, name string) (value any, isMethod bool, async bool, err error)
	HostCall(ctx context.Context, method string, args []any, kwargs map[string]any) (any, error)
}

// Parent object a ctx method returns that the plugin keeps by reference, with the Python type name and repr it shows
type HostObjectFacade interface {
	HostFacade
	PyTypeName() string
	PyRepr() string
}

func listOrEmpty(v any) []any {
	if l, ok := v.([]any); ok {
		return l
	}
	return []any{}
}

func mapOrEmpty(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

// Wire form of a parent result, registrations becoming handles the child can dispose
func (h *PluginHost) encodeForHost(value any) any {
	return wireEncode(value, func(obj any) map[string]any {
		reg, ok := obj.(*Registration)
		if !ok || reg == nil {
			return h.parentRef(obj)
		}
		h.stateMu.Lock()
		h.handleIDs++
		id := h.handleIDs
		h.handles[id] = reg
		h.stateMu.Unlock()
		return map[string]any{"__handle__": id, "kind": reg.Kind, "key": reg.Key}
	})
}

// Proxy for a callable or provider object ref the child sent as an argument
func (h *PluginHost) resolveRef(ref map[string]any, base string) (any, error) {
	if e, ok, err := h.resolveEventRef(ref); ok {
		return e, err
	}
	if _, ok := ref["__callable__"]; ok {
		return h.callableProxy(ref), nil
	}
	if _, ok := ref["__object__"]; ok {
		if base == "" {
			typ := wireStr(ref["type"])
			if typ == "" {
				typ = "plugin"
			}
			return nil, &PluginHostUnsupported{Msg: fmt.Sprintf("a %s object cannot be passed here across the plugin host", typ)}
		}
		return h.objectProxy(ref, base, nil), nil
	}
	return nil, &PluginHostUnsupported{Msg: "registration handles are owned by the plugin host"}
}

// Proxy for an object a hosted method returned when its contract declares a provider, else an Opaque
func (h *PluginHost) resolveResultRef(ref map[string]any, base string) (any, error) {
	if e, ok, err := h.resolveEventRef(ref); ok {
		return e, err
	}
	if base == hostedRawRefBase {
		return ref, nil
	}
	if raw, ok := ref["__object__"]; ok && base == "" {
		id, _ := wireInt(raw)
		h.mu.Lock()
		generation := h.generation
		h.mu.Unlock()
		h.release(generation, id)
		typ := wireStr(ref["type"])
		if typ == "" {
			typ = "object"
		}
		return &Opaque{TypeName: typ}, nil
	}
	return h.resolveRef(ref, base)
}

// Plugin callable living in the child, invoked over the wire with its declared signature
type HostedCallable struct {
	host       *PluginHost
	ref        int64
	generation int
	Async      bool
	Sig        []wireParam
	Name       string
	QualName   string
	Module     string
}

func (h *PluginHost) callableProxy(ref map[string]any) *HostedCallable {
	id, _ := wireInt(ref["__callable__"])
	h.mu.Lock()
	generation := h.generation
	h.mu.Unlock()
	f := &HostedCallable{host: h, ref: id, generation: generation, Async: config.PyTruthy(ref["async"]), Sig: signatureFrom(ref["sig"])}
	f.Name = wireStr(ref["name"])
	if f.Name == "" {
		f.Name = "callback"
	}
	f.QualName = wireStr(ref["qualname"])
	if f.QualName == "" {
		f.QualName = f.Name
	}
	f.Module = wireStr(ref["module"])
	if f.Module == "" {
		f.Module = "icebath_cli.plugin_host"
	}
	return f
}

// Calls the plugin callable in the child
func (f *HostedCallable) Call(ctx context.Context, args []any, kwargs map[string]any) (any, error) {
	return f.host.invoke(ctx, f.ref, args, kwargs, f.generation)
}

// Python identity used by hook attribution and failure reports
func (f *HostedCallable) callbackIdentity() (string, string) {
	return f.Module + "." + f.QualName, f.Name
}

// Plugin provider object living in the child, an instance of its provider base on the parent side
type HostedObject struct {
	host     *PluginHost
	slot     *objectSlot
	methods  map[string]hostedMethod
	mu       sync.Mutex
	live     map[string]bool
	local    map[string]any
	TypeName string
	Base     string
	// Identity the child judged from the class alone for an object an in process registrar received
	Identity map[string]any
}

// Async flag and signature of one hosted object method
type hostedMethod struct {
	Async bool
	Sig   []wireParam
}

// Object id and child generation a proxy addresses, reloaded after a restart when the proxy can reload
type objectSlot struct {
	mu         sync.Mutex
	host       *PluginHost
	reload     func(context.Context) (map[string]any, error)
	ref        int64
	generation int
	cleanup    runtime.Cleanup
	tracked    bool
}

type releaseRef struct {
	host       *PluginHost
	generation int
	ref        int64
}

func (h *PluginHost) objectProxy(ref map[string]any, base string, reload func(context.Context) (map[string]any, error)) *HostedObject {
	id, _ := wireInt(ref["__object__"])
	h.mu.Lock()
	generation := h.generation
	h.mu.Unlock()
	if g, ok := wireInt(ref["generation"]); ok && g != 0 {
		generation = int(g)
	}
	o := &HostedObject{host: h, slot: &objectSlot{host: h, reload: reload, ref: id, generation: generation},
		methods: map[string]hostedMethod{}, live: map[string]bool{}, local: map[string]any{}, TypeName: wireStr(ref["type"]), Base: base}
	if methods, ok := ref["methods"].(map[string]any); ok {
		for name, raw := range methods {
			meta, _ := raw.(map[string]any)
			o.methods[name] = hostedMethod{Async: config.PyTruthy(meta["async"]), Sig: signatureFrom(meta["sig"])}
		}
	}
	for _, name := range listOrEmpty(ref["live"]) {
		o.live[wireStr(name)] = true
	}
	if identity, ok := ref["identity"].(map[string]any); ok {
		o.Identity = identity
	}
	o.slot.track(o)
	return o
}

func (s *objectSlot) track(o *HostedObject) {
	if s.tracked {
		s.cleanup.Stop()
	}
	s.cleanup = runtime.AddCleanup(o, func(r releaseRef) { r.host.release(r.generation, r.ref) },
		releaseRef{host: s.host, generation: s.generation, ref: s.ref})
	s.tracked = true
}

// Current object id and generation, reloading the plugin into a fresh child when the old one is gone
func (o *HostedObject) addr(ctx context.Context) (int64, int, error) {
	s := o.slot
	s.mu.Lock()
	defer s.mu.Unlock()
	h := s.host
	h.mu.Lock()
	stale := s.generation != h.generation || !h.aliveLocked()
	h.mu.Unlock()
	if stale && s.reload != nil {
		fresh, err := s.reload(ctx)
		if err != nil {
			return 0, 0, err
		}
		s.ref, _ = wireInt(fresh["__object__"])
		g, _ := wireInt(fresh["generation"])
		s.generation = int(g)
		s.track(o)
	}
	return s.ref, s.generation, nil
}

// Reports whether the object declared a method of that name
func (o *HostedObject) HasMethod(name string) bool {
	_, ok := o.methods[name]
	return ok
}

// Async flag of a declared method
func (o *HostedObject) MethodIsAsync(name string) bool { return o.methods[name].Async }

// Whether the provider class replaces a base method, every declared method under host isolation
func (o *HostedObject) OverridesBase(ctx context.Context, method string) (bool, error) {
	if !strings.Contains(o.Base, ":") || IsolationMode(ctx, nil) == IsolationHost {
		_, declared := o.methods[method]
		return declared, nil
	}
	ref, generation, err := o.addr(ctx)
	if err != nil {
		return false, err
	}
	raw, err := o.host.call(ctx, "obj_overrides", map[string]any{"ref": ref, "base": o.Base, "method": method}, &generation, 0, nil)
	if err != nil {
		return false, err
	}
	overrides, _ := raw.(bool)
	return overrides, nil
}

// Calls one method of the plugin object in the child, a dotted name reading its attribute path first as Python does
func (o *HostedObject) Call(ctx context.Context, method string, args []any, kwargs map[string]any) (any, error) {
	ref, generation, err := o.addr(ctx)
	if err != nil {
		return nil, err
	}
	return o.host.callIn(ctx, o.liveClientBase(), "obj_invoke", map[string]any{"ref": ref, "method": method, "args": o.host.encodeArgs(nonNilArgs(args)),
		"kwargs": o.host.encodeArgs(nonNilKwargs(kwargs))}, &generation, 0, declaredReturn(o.Base, method))
}

// Runs one login backend operation on the object in the child, which type checks its result
func (o *HostedObject) LoginBackendCall(ctx context.Context, op string, params map[string]any) (any, error) {
	ref, generation, err := o.addr(ctx)
	if err != nil {
		return nil, err
	}
	payload := map[string]any{"ref": ref, "op": op}
	for k, v := range params {
		payload[k] = v
	}
	return o.host.call(ctx, "login_backend", payload, &generation, 0, nil)
}

// Takes the next item of a for loop over the object in the child, ok false once it raises StopIteration
func (o *HostedObject) Next(ctx context.Context) (item any, ok bool, err error) {
	ref, generation, err := o.addr(ctx)
	if err != nil {
		return nil, false, err
	}
	raw, err := o.host.callIn(ctx, o.liveClientBase(), "obj_next", map[string]any{"ref": ref}, &generation, 0, nil)
	if err != nil {
		return nil, false, err
	}
	step, isMap := raw.(map[string]any)
	if !isMap {
		return nil, false, fmt.Errorf("plugin host: obj_next answered %T instead of a step", raw)
	}
	if config.PyTruthy(step["done"]) {
		return nil, false, nil
	}
	return step["item"], true, nil
}

// Reads an attribute, live ones from the child and the rest locally first, an absent one an AttributeError
func (o *HostedObject) Get(ctx context.Context, name string) (any, error) {
	o.mu.Lock()
	live := o.live[name]
	local, isLocal := o.local[name]
	o.mu.Unlock()
	if !live && isLocal {
		return local, nil
	}
	if !live && strings.HasPrefix(name, "_") {
		return nil, &HostBuiltinError{Type: "AttributeError", Message: name}
	}
	value, err := o.remoteGet(ctx, name)
	var un *PluginHostUnavailable
	if !live && errors.As(err, &un) {
		return nil, &HostBuiltinError{Type: "AttributeError", Message: name}
	}
	return value, err
}

func (o *HostedObject) remoteGet(ctx context.Context, name string) (any, error) {
	ref, generation, err := o.addr(ctx)
	if err != nil {
		return nil, err
	}
	value, err := o.host.callIn(ctx, o.liveClientBase(), "obj_getattr", map[string]any{"ref": ref, "name": name}, &generation, 0, declaredReturn(o.Base, name))
	if err != nil {
		return nil, err
	}
	if nested, ok := value.(*HostedObject); ok && (o.Base == hostedClientBase || o.Base == hostedModuleBase) {
		nested.slot.reload = o.attributeReload(name)
	}
	if m, ok := value.(map[string]any); ok && len(m) == 1 {
		if _, missing := m["__missing__"]; missing {
			return nil, &HostBuiltinError{Type: "AttributeError", Message: name}
		}
	}
	return value, nil
}

// Assigns an attribute on the plugin object when the value can cross, else keeps it on the proxy
func (o *HostedObject) Set(ctx context.Context, name string, value any) error {
	if !strings.HasPrefix(name, "_") && !holdsLiveHandle(value) {
		ref, generation, err := o.addr(ctx)
		if err != nil {
			return err
		}
		if _, err := o.host.call(ctx, "obj_setattr", map[string]any{"ref": ref, "name": name, "value": wireEncode(value, nil)}, &generation, 0, nil); err != nil {
			return err
		}
		o.mu.Lock()
		o.live[name] = true
		o.mu.Unlock()
		return nil
	}
	o.mu.Lock()
	delete(o.live, name)
	o.local[name] = value
	o.mu.Unlock()
	return nil
}

func (o *HostedObject) String() string {
	return fmt.Sprintf("<plugin-host %s #%d>", o.TypeName, o.slot.ref)
}

// Reports whether a value would reach the child only as an Opaque
func holdsLiveHandle(value any) bool {
	found := false
	encoded := wireEncode(value, func(obj any) map[string]any {
		if _, isRecord := recordFields(reflect.ValueOf(obj)); !isRecord {
			found = true
		}
		return nil
	})
	if m, ok := encoded.(map[string]any); ok {
		if _, opaque := m["__opaque__"]; opaque {
			return true
		}
	}
	return found
}

// Reports whether obj is a provider object proxy whose methods run in the plugin host
func IsHostedObject(obj any) bool {
	o, ok := obj.(*HostedObject)
	return ok && o != nil
}

// Plugin output and child logs into the plugin log tagged with the profile
func pumpHostStderr(stream io.Reader, label string) {
	r := bufio.NewReader(stream)
	for {
		raw, err := r.ReadBytes('\n')
		if line := pystr.Rstrip(pystr.DecodeUTF8Replace(raw)); line != "" {
			logInfo(fmt.Sprintf("plugin host [%s]: %s", label, line))
		}
		if err != nil {
			return
		}
	}
}

// Env values of this home hosted ambient credential profiles, each read by get_secret under the home secret scope
func hostedAmbientCredentials(ctx context.Context, h string) (map[string]string, error) {
	names := hostedAmbientEnvNames()
	out := map[string]string{}
	if len(names) == 0 {
		return out, nil
	}
	s := installedServices()
	if s == nil || s.BindProfileSecretScope == nil {
		return nil, &ServiceMissingError{Name: "BindProfileSecretScope"}
	}
	if s.GetSecret == nil {
		return nil, &ServiceMissingError{Name: "GetSecret"}
	}
	scoped := s.BindProfileSecretScope(home.WithOverride(ctx, h), h)
	for _, name := range names {
		value, ok, err := s.GetSecret(scoped, name)
		if err != nil {
			return nil, err
		}
		if ok && value != "" {
			out[name] = value
		}
	}
	return out, nil
}
