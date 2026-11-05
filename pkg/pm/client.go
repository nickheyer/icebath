package pm

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Argv starting this binary's isolated PM worker, swapped by tests for a re-executed test binary
var workerArgv = func() ([]string, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return append([]string{exe}, pmWorkerArgs()...), nil
}

// Missing packages of a closure, refusing a static registry gap or a lazy install the policy disallows
func missingOrRefuse(ctx context.Context, name string) ([]string, error) {
	chain, err := Walk([]string{name})
	if err != nil {
		return nil, err
	}
	var missing []string
	for _, p := range chain {
		installed, err := IsInstalled(ctx, p.Base().Name)
		if err != nil {
			return nil, err
		}
		if !installed {
			missing = append(missing, p.Base().Name)
		}
	}
	if len(missing) == 0 {
		return nil, nil
	}
	target, err := CurrentTarget()
	if err != nil {
		return nil, err
	}
	for _, gap := range missing {
		p, err := GetPackage(gap)
		if err != nil {
			return nil, err
		}
		if reason := p.MissingReason(target); reason != "" {
			return nil, NewInstallError(gap, fmt.Sprintf("unavailable on %s: %s", target, reason), "none")
		}
	}
	if !LazyInstallsAllowedFor(ctx) {
		return nil, RefuseLazyFor(ctx, name, strings.Join(missing, ", "))
	}
	return missing, nil
}

// Receives a worker's completed receipt, replaced by tests that publish a competing latest receipt
var acceptWorkerReceipt = AcceptWorkerReceipt

// Records a lazy sync refused because PM's engine is not ready and returns the error to raise
func refuseColdRuntime(ctx context.Context, cold *InstallError, extras []string) error {
	refusal := cold
	if len(extras) > 0 {
		hints := make([]string, len(extras))
		for i, extra := range extras {
			hints[i] = InstallHint(extra)
		}
		refusal = NewInstallError(cold.Package, fmt.Sprintf("%s while enabling %s", cold.Cause, pyListRepr(extras)),
			"run `"+strings.Join(hints, "`, `")+"`")
	}
	token := BeginReceipt(ctx, "sync")
	RecordRefusal(ctx, "lazy-install", refusal.Error())
	RecordStep(ctx, "dependency-sync", false, "InstallError: "+refusal.Error())
	FinalizeReceipt(ctx, "failed", 1, token)
	return refusal
}

// Reports PM's engine toolchain ready without installing anything, a resident payload runtime counting as ready
func engineReady(ctx context.Context) (*InstallError, error) {
	resident, err := FindResidentRuntime()
	if err != nil {
		return nil, err
	}
	if resident != nil {
		return nil, nil
	}
	_, _, found, err := Toolchain(ctx, false, false)
	if err != nil {
		return nil, err
	}
	if !found {
		return NewInstallError("pm-runtime", "not installed and lazy installs are disabled",
			"run `icebath pm install` to prepare the independent PM runtime"), nil
	}
	return nil, nil
}

// Request fields the prerequisite policy reads
type policyArgs struct {
	Name     string
	Explicit bool
	Repair   bool
	Extras   []string
}

// Decides whether an operation runs with lazy installs off, refusing a lazy state sync PM cannot run cold
func workerPolicy(ctx context.Context, spec *workerOperation, args policyArgs) (bool, error) {
	stateSync := spec.bootstrap == bootstrapPolicy
	if spec.bootstrap == bootstrapState {
		p, err := GetPackage(args.Name)
		if err != nil {
			return false, err
		}
		_, stateSync = p.(StatePackage)
	}
	if !stateSync || args.Explicit || args.Repair || LazyInstallsAllowedFor(ctx) {
		return false, nil
	}
	cold, err := engineReady(ctx)
	if err != nil {
		return false, err
	}
	if cold != nil {
		return false, refuseColdRuntime(ctx, cold, args.Extras)
	}
	return true, nil
}

// Ambient update correlation id of a context, nil when no update is open
func ambientUpdate(ctx context.Context) *string {
	s := scopeOf(ctx)
	s.mu.Lock()
	id := ambientUpdateID(ctx, s)
	s.mu.Unlock()
	if text, ok := id.(string); ok {
		return &text
	}
	return nil
}

// Runs an operation in process behind the worker boundary, its receipt crossing back by correlation
func workerCall(ctx context.Context, operation string, args policyArgs, run func(ctx context.Context) error) error {
	spec, err := lookupOperation(operation)
	if err != nil {
		return err
	}
	lazyDisabled, err := workerPolicy(ctx, spec, args)
	if err != nil {
		return err
	}
	updateID := ambientUpdate(ctx)
	workerCtx := context.WithValue(ctx, receiptScopeKey{}, &receiptScope{})
	if lazyDisabled {
		workerCtx = withLazyInstallsDisabled(workerCtx)
	}
	restore := WorkerContext(workerCtx, updateID)
	failure := run(workerCtx)
	completed := LastCompleted(workerCtx)
	restore()
	if err := acceptWorkerReceipt(ctx, completed, updateID); err != nil {
		return err
	}
	return failure
}

// Client side handlers of the worker's progress callbacks in the order the request offers them
func callbackHandlers(hooks workerHooks) (map[string]func(args []any) error, []any) {
	handlers := map[string]func(args []any) error{}
	var offered []any
	if hooks.progress != nil {
		offered = append(offered, "progress")
		handlers["progress"] = func(args []any) error {
			if len(args) != 4 {
				return &PyError{Class: "TypeError", Message: fmt.Sprintf("progress() takes 4 positional arguments but %d were given", len(args))}
			}
			done, _ := jsonNumber(args[1])
			total, _ := jsonNumber(args[2])
			return hooks.progress(pyStr(args[0]), int64(done), int64(total), pyStr(args[3]))
		}
	}
	if hooks.downloadProgress != nil {
		offered = append(offered, "download_progress")
		handlers["download_progress"] = func(args []any) error {
			if len(args) != 3 {
				return &PyError{Class: "TypeError", Message: fmt.Sprintf("download_progress() takes 3 positional arguments but %d were given", len(args))}
			}
			done, _ := jsonNumber(args[0])
			total, _ := jsonNumber(args[1])
			return hooks.downloadProgress(int64(done), int64(total), rangesFromPayload(args[2]))
		}
	}
	return handlers, offered
}

// Policy fields of a wire request's arguments
func policyFromArguments(arguments map[string]any) (policyArgs, error) {
	args := policyArgs{Name: pyStr(arguments["name"]), Explicit: truthy(arguments["explicit"]), Repair: truthy(arguments["repair"])}
	if raw, ok := arguments["extras"]; ok && truthy(raw) {
		extras, err := pyIterStrings(raw)
		if err != nil {
			return policyArgs{}, err
		}
		args.Extras = extras
	}
	return args, nil
}

// Sends one request to an isolated child of this binary and relays its callbacks, receipt and result
func request(ctx context.Context, operation string, arguments map[string]any, hooks workerHooks, projectRoot string) (any, error) {
	spec, err := lookupOperation(operation)
	if err != nil {
		return nil, err
	}
	requestID := randomHex32()
	updateID := ambientUpdate(ctx)
	names := spec.packages
	if names == nil {
		if raw, ok := arguments["names"]; ok {
			if names, err = pyIterStrings(raw); err != nil {
				return nil, err
			}
		} else {
			names = []string{pyStr(arguments["name"])}
		}
	}
	definitions, err := PackageDefinitions(names)
	if err != nil {
		return nil, err
	}
	encodedDefinitions := make([]any, len(definitions))
	for i, d := range definitions {
		encodedDefinitions[i] = map[string]any{"name": d.Name, "module": d.Module, "qualname": d.Qualname}
	}
	repo := RepoRoot()
	if projectRoot != "" {
		repo = absoluteUnresolved(projectRoot)
	}
	callbacks, offered := callbackHandlers(hooks)
	if offered == nil {
		offered = []any{}
	}
	updateValue := any(nil)
	if updateID != nil {
		updateValue = *updateID
	}
	message := map[string]any{
		"operation": operation, "arguments": arguments, "update_id": updateValue,
		"callbacks": offered, "packages": encodedDefinitions,
		"context": map[string]any{"repo": repo, "lockfile": LockfilePath()},
	}
	environment, err := RuntimeEnvironment(ctx)
	if err != nil {
		return nil, err
	}
	policy, err := policyFromArguments(arguments)
	if err != nil {
		return nil, err
	}
	lazyDisabled, err := workerPolicy(ctx, spec, policy)
	if err != nil {
		return nil, err
	}
	if lazyDisabled {
		environment[EnvDisableLazyInstalls] = "1"
	}
	argv, err := workerArgv()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = EnvList(environment)
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	session := &workerSession{id: requestID, stdin: stdin, stdout: stdout, pause: hooks.pause, cmd: cmd, stopped: make(chan struct{})}
	defer session.finish()
	return session.exchange(ctx, message, callbacks, updateID)
}

// One client side worker conversation over the child's pipes
type workerSession struct {
	id       string
	stdin    io.WriteCloser
	stdout   io.Reader
	pause    *PauseEvent
	cmd      *exec.Cmd
	writeMu  sync.Mutex
	stopped  chan struct{}
	stopOnce sync.Once
	monitor  sync.WaitGroup
	waited   chan error
	reaped   bool
}

// Writes one control line carrying the request id and the current cancel flag
func (s *workerSession) send(data map[string]any) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	message := NewOrderedObject()
	message.Set("id", s.id)
	message.Set("cancel", s.pause != nil && s.pause.IsSet())
	for _, key := range sortedKeys(data) {
		message.Set(key, data[key])
	}
	line, err := dumpsJSON(message, false)
	if err != nil {
		return err
	}
	_, err = s.stdin.Write(append(line, '\n'))
	return err
}

// Forwards a pause to the running worker once, polling every fifty milliseconds until the exchange ends
func (s *workerSession) watchPause() {
	defer s.monitor.Done()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-s.stopped:
			return
		case <-ticker.C:
			if s.pause.IsSet() {
				_ = s.send(map[string]any{"type": "cancel"})
				return
			}
		}
	}
}

// Ends the pause monitor and waits for it
func (s *workerSession) stop() {
	s.stopOnce.Do(func() { close(s.stopped) })
	s.monitor.Wait()
}

// Waits for the child's exit, starting the wait once
func (s *workerSession) wait() chan error {
	if s.waited == nil {
		s.waited = make(chan error, 1)
		go func() { s.waited <- s.cmd.Wait() }()
	}
	return s.waited
}

// Stops monitoring and terminates a child still running after the exchange
func (s *workerSession) finish() {
	s.stop()
	if s.reaped {
		return
	}
	select {
	case <-s.wait():
		return
	default:
	}
	terminateWorker(s.cmd.Process)
	select {
	case <-s.wait():
	case <-time.After(5 * time.Second):
	}
}

// Runs the request through to its result, relaying each callback and accepting the receipt by correlation
func (s *workerSession) exchange(ctx context.Context, message map[string]any, callbacks map[string]func(args []any) error, updateID *string) (any, error) {
	if err := s.send(message); err != nil {
		return nil, err
	}
	if s.pause != nil {
		s.monitor.Add(1)
		go s.watchPause()
	}
	reader := bufio.NewReader(s.stdout)
	var callbackErr error
	var response map[string]any
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) == 0 && err != nil {
			return nil, NewInstallError("pm", "worker exited without a result", "check the worker diagnostics on stderr")
		}
		value, err := loadJSON(line)
		if err != nil {
			return nil, &PyError{Class: "ValueError", Message: err.Error()}
		}
		parsed, ok := value.(map[string]any)
		if !ok {
			return nil, &PyError{Class: "TypeError", Message: fmt.Sprintf("'%s' object is not subscriptable", pyTypeName(value))}
		}
		if !jsonEquals(parsed["id"], s.id) {
			return nil, NewInstallError("pm", "worker returned a different request id", "")
		}
		if jsonEquals(parsed["type"], "result") {
			response = parsed
			break
		}
		args, _ := parsed["args"].([]any)
		if jsonEquals(parsed["callback"], "download_progress") {
			if ordered, err := loadOrderedJSON(line); err == nil {
				args = orderedCallbackArgs(ordered, args)
			}
		}
		handler, known := callbacks[pyStr(parsed["callback"])]
		var runErr error
		if !known {
			runErr = &PyError{Class: "KeyError", Message: pyRepr(pyStr(parsed["callback"]))}
		} else {
			runErr = handler(args)
		}
		if runErr != nil {
			if callbackErr == nil {
				callbackErr = runErr
			}
			if err := s.send(map[string]any{"type": "callback_result", "call": parsed["call"], "error": errorClass(runErr) + ": " + runErr.Error()}); err != nil {
				return nil, err
			}
			continue
		}
		if err := s.send(map[string]any{"type": "callback_result", "call": parsed["call"], "result": nil}); err != nil {
			return nil, err
		}
	}
	s.stop()
	s.stdin.Close()
	select {
	case err := <-s.wait():
		s.reaped = true
		if err != nil {
			return nil, NewInstallError("pm", "worker exited unsuccessfully", "")
		}
	case <-time.After(5 * time.Second):
		return nil, &PyError{Class: "TimeoutExpired", Message: fmt.Sprintf("Command '%s' timed out after 5 seconds", strings.Join(s.cmd.Args, " "))}
	}
	receipt, _ := response["receipt"].(map[string]any)
	if err := acceptWorkerReceipt(ctx, receipt, updateID); err != nil {
		return nil, err
	}
	if callbackErr != nil {
		return nil, callbackErr
	}
	if payload, failed := response["error"].(map[string]any); failed {
		return nil, workerError(payload)
	}
	return response["result"], nil
}

// Callback arguments with the coverage object rebuilt in the order the worker reported it
func orderedCallbackArgs(ordered any, args []any) []any {
	message, ok := ordered.(*OrderedObject)
	if !ok {
		return args
	}
	raw, _ := message.Get("args")
	list, ok := raw.([]any)
	if !ok || len(list) != 3 || len(args) != 3 {
		return args
	}
	return []any{args[0], args[1], list[2]}
}

// Download coverage of a relayed callback in the order the worker reported it
func rangesFromPayload(value any) []RangeSet {
	var out []RangeSet
	appendRows := func(key string, rows any) {
		list, _ := rows.([]any)
		set := RangeSet{Key: key}
		for _, row := range list {
			pair, _ := row.([]any)
			if len(pair) != 2 {
				continue
			}
			start, _ := jsonNumber(pair[0])
			end, _ := jsonNumber(pair[1])
			set.Ranges = append(set.Ranges, Range{Start: int64(start), End: int64(end)})
		}
		out = append(out, set)
	}
	switch v := value.(type) {
	case *OrderedObject:
		for _, key := range v.Keys {
			appendRows(key, v.Values[key])
		}
	case map[string]any:
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			appendRows(key, v[key])
		}
	}
	return out
}

// Caller observers whose first failure a facade returns in place of the operation's own error
type observedHooks struct {
	mu  sync.Mutex
	err error
}

// Remembers the first observer failure
func (o *observedHooks) note(err error) error {
	if err != nil {
		o.mu.Lock()
		if o.err == nil {
			o.err = err
		}
		o.mu.Unlock()
	}
	return err
}

// Install progress observer whose failure is remembered
func (o *observedHooks) progress(f InstallProgress) InstallProgress {
	if f == nil {
		return nil
	}
	return func(stage string, done, total int64, label string) error { return o.note(f(stage, done, total, label)) }
}

// Download coverage observer whose failure is remembered
func (o *observedHooks) download(f ProgressFunc) ProgressFunc {
	if f == nil {
		return nil
	}
	return func(done, total int64, ranges []RangeSet) error { return o.note(f(done, total, ranges)) }
}

// The observer's failure when one raised, else the operation's result
func (o *observedHooks) result(err error) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.err != nil {
		return o.err
	}
	return err
}

// Ensures a package for an application caller, installed closures answering without crossing the worker boundary
func Ensure(ctx context.Context, name string, opts EnsureOptions) (*Runner, error) {
	p, err := GetPackage(name)
	if err != nil {
		return nil, err
	}
	if _, state := p.(StatePackage); !opts.Explicit && !state {
		missing, err := missingOrRefuse(ctx, name)
		if err != nil {
			return nil, err
		}
		if len(missing) == 0 {
			env, err := envFor(ctx, []string{name}, opts.BaseEnv)
			if err != nil {
				return nil, err
			}
			return &Runner{Name: name, Env: env}, nil
		}
	}
	if opts.PauseEvent != nil && opts.PauseEvent.IsSet() {
		return nil, downloadError(DownloadWasPaused, "install paused")
	}
	observed := &observedHooks{}
	if err := observed.result(workerCall(ctx, "ensure", policyArgs{Name: name, Explicit: opts.Explicit}, func(workerCtx context.Context) error {
		_, err := ensure(workerCtx, name, EnsureOptions{Explicit: opts.Explicit, SkipVerify: opts.SkipVerify,
			Progress: observed.progress(opts.Progress), PauseEvent: opts.PauseEvent, DownloadProgress: observed.download(opts.DownloadProgress)})
		return err
	})); err != nil {
		return nil, err
	}
	env, err := envFor(ctx, []string{name}, opts.BaseEnv)
	if err != nil {
		return nil, err
	}
	return &Runner{Name: name, Env: env}, nil
}

// Syncs the dependency environment for an application caller, a foreign checkout going through its own worker
func SyncVenv(ctx context.Context, opts SyncVenvOptions) error {
	root := RepoRoot()
	if opts.ProjectRoot != "" {
		root = opts.ProjectRoot
	}
	if len(opts.Extras) > 0 && !opts.Explicit && !opts.Repair {
		running, prefix, err := runningFromSelectedEnvironment(ctx, root)
		if err != nil {
			return err
		}
		if !running {
			return RefuseLazyFor(ctx, "venv", fmt.Sprintf("%s: this process is not running from the install's dependency environment "+
				"(%s); only an explicit install may change what later processes boot into", pyListRepr(opts.Extras), prefix))
		}
	}
	plugins := opts.Plugins
	if selection, ok := plugins.(*Selection); ok {
		if _, has := selection.Data["expected_config"]; !has {
			digest, found, err := FileDigest(filepath.Join(pyStr(selection.Data["home"]), "config.yaml"))
			if err != nil {
				return err
			}
			if !found {
				digest = "missing"
			}
			data := copyJSONMap(selection.Data)
			data["expected_config"] = digest
			plugins = &Selection{Data: data}
		}
	}
	if opts.ProjectRoot != "" && resolvePath(opts.ProjectRoot) != resolvePath(RepoRoot()) {
		arguments := map[string]any{
			"extras": optionalList(opts.Extras), "explicit": opts.Explicit, "repair": opts.Repair,
			"plugins": EncodePluginInput(plugins), "evict_incompatible_plugins": opts.EvictIncompatiblePlugins,
			"automatic": optionalList(opts.Automatic), "carried": optionalList(opts.Carried),
		}
		_, err := request(ctx, "sync_venv", arguments, workerHooks{}, opts.ProjectRoot)
		return err
	}
	return workerCall(ctx, "sync_venv", policyArgs{Explicit: opts.Explicit, Repair: opts.Repair, Extras: opts.Extras}, func(workerCtx context.Context) error {
		return syncVenv(workerCtx, SyncVenvOptions{
			Extras: opts.Extras, Explicit: opts.Explicit, Repair: opts.Repair, Automatic: opts.Automatic, Carried: opts.Carried,
			EvictIncompatiblePlugins: opts.EvictIncompatiblePlugins, Plugins: plugins, Stdout: opts.Stdout,
		})
	})
}

// JSON list of an optional string list, null for Python None
func optionalList(list []string) any {
	if list == nil {
		return nil
	}
	out := make([]any, len(list))
	for i, s := range list {
		out[i] = s
	}
	return out
}

// Publishes every required tool of this tree's lockfile and puts them on PATH before a venv sync
func EnsureToolsForSync(ctx context.Context) error {
	InstallLibatomicBeforeLock(ctx)
	roots, err := ToolRoots(currentLockfile().Names())
	if err != nil {
		return err
	}
	for _, name := range roots {
		if _, err := Ensure(ctx, name, EnsureOptions{Explicit: true}); err != nil {
			return err
		}
	}
	problems, err := Activate(ctx, true)
	if err != nil {
		return err
	}
	if len(problems) > 0 {
		return &PyError{Class: "RuntimeError", Message: "tools not on PATH before venv sync: " + strings.Join(problems, "; ")}
	}
	return nil
}

// Stages a cross target pin through the worker boundary and returns its entry
func StageOnly(ctx context.Context, name, target string, opts StageOnlyOptions) (string, error) {
	var entry string
	observed := &observedHooks{}
	err := observed.result(workerCall(ctx, "stage_only", policyArgs{Name: name}, func(workerCtx context.Context) error {
		var err error
		entry, err = stageOnly(workerCtx, name, target, StageOnlyOptions{Progress: observed.progress(opts.Progress),
			PauseEvent: opts.PauseEvent, DownloadProgress: observed.download(opts.DownloadProgress)})
		return err
	}))
	return entry, err
}

// Runs a path returning Python environment operation through the worker boundary
func pathCall(ctx context.Context, operation string, args policyArgs, run func(context.Context) (string, error)) (string, error) {
	var path string
	err := workerCall(ctx, operation, args, func(workerCtx context.Context) error {
		var err error
		path, err = run(workerCtx)
		return err
	})
	return path, err
}

// Builds a validated Python environment without exposing install machinery
func BuildEnvironment(ctx context.Context, opts BuildEnvironmentOptions) (string, error) {
	return pathCall(ctx, "build_environment", policyArgs{Explicit: opts.Explicit, Extras: opts.Extras},
		func(c context.Context) (string, error) { return buildEnvironment(c, opts) })
}

// Resolves a project's lock without creating or selecting an environment
func LockProject(ctx context.Context, opts LockProjectOptions) error {
	return workerCall(ctx, "lock_project", policyArgs{Explicit: opts.Explicit},
		func(c context.Context) error { return lockProject(c, opts) })
}

// Stages PM's own offline runtime directly, its construction never dispatched through that runtime
func StageManagerRuntime(ctx context.Context, opts StageManagerRuntimeOptions) (string, error) {
	return stageManagerRuntime(ctx, opts)
}

// Selects a complete isolated dependency generation, retaining the previous one
func EnsureEnvironment(ctx context.Context, opts EnsureEnvironmentOptions) (string, error) {
	return pathCall(ctx, "ensure_environment", policyArgs{Name: opts.Name, Explicit: opts.Explicit},
		func(c context.Context) (string, error) { return ensureEnvironment(c, opts) })
}

// Selects an isolated environment of a project's locked dependencies
func EnsureProjectEnvironment(ctx context.Context, opts EnsureProjectEnvironmentOptions) (string, error) {
	return pathCall(ctx, "ensure_project_environment", policyArgs{Name: opts.Name, Explicit: opts.Explicit, Extras: opts.Extras},
		func(c context.Context) (string, error) { return ensureProjectEnvironment(c, opts) })
}

// Installs an isolated Python tool and returns its validated executable
func EnsurePythonTool(ctx context.Context, opts EnsurePythonToolOptions) (string, error) {
	return pathCall(ctx, "ensure_python_tool", policyArgs{Name: opts.Name, Explicit: opts.Explicit},
		func(c context.Context) (string, error) { return ensurePythonTool(c, opts) })
}

// VenvIsCurrentOptions name the checkout, the extras to add to the recorded union and the plugin members to probe
type VenvIsCurrentOptions struct {
	ProjectRoot string
	Extras      []string
	Plugins     PluginInput
}

// Probes dependency currency without bootstrapping, a cold foreign worker answering not current
func VenvIsCurrent(ctx context.Context, opts VenvIsCurrentOptions) (bool, error) {
	if opts.ProjectRoot == "" || resolvePath(opts.ProjectRoot) == resolvePath(RepoRoot()) {
		return venvIsCurrent(ctx, opts.Extras, opts.Plugins, opts.ProjectRoot)
	}
	result, err := request(ctx, "venv_is_current", map[string]any{"extras": optionalList(opts.Extras), "plugins": EncodePluginInput(opts.Plugins)},
		workerHooks{}, opts.ProjectRoot)
	var install *InstallError
	if errors.As(err, &install) && install.Package == "pm-runtime" {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return truthy(result), nil
}

// Checks a project lock is current
func CheckProjectLock(ctx context.Context, opts CheckProjectLockOptions) error {
	return workerCall(ctx, "check_project_lock", policyArgs{Explicit: opts.Explicit},
		func(c context.Context) error { return checkProjectLock(c, opts) })
}

// Exports a project's locked requirements
func ExportRequirements(ctx context.Context, opts ExportRequirementsOptions) error {
	return workerCall(ctx, "export_requirements", policyArgs{Explicit: opts.Explicit, Extras: opts.Extras},
		func(c context.Context) error {
			_, err := exportRequirements(c, opts)
			return err
		})
}

// Builds an environment from caller owned requirements
func BuildRequirementsEnvironment(ctx context.Context, opts BuildRequirementsEnvironmentOptions) (string, error) {
	return pathCall(ctx, "build_requirements_environment", policyArgs{Explicit: opts.Explicit},
		func(c context.Context) (string, error) { return buildRequirementsEnvironment(c, opts) })
}

// Realizes build tools without selecting application or profile state
func PrepareTools(ctx context.Context, opts PrepareToolsOptions) (string, error) {
	return pathCall(ctx, "prepare_tools", policyArgs{},
		func(c context.Context) (string, error) { return prepareTools(c, opts) })
}

// Copies verified pins independently, never bootstrapping
func StageTools(ctx context.Context, opts StageToolsOptions) (string, error) {
	return pathCall(ctx, "stage_tools", policyArgs{},
		func(c context.Context) (string, error) { return stageTools(c, opts) })
}

// Prunes a uv cache to what the lock needs
func PruneCache(ctx context.Context, opts PruneCacheOptions) error {
	return workerCall(ctx, "prune_cache", policyArgs{},
		func(c context.Context) error { return pruneCache(c, opts) })
}
