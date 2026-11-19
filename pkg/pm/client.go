//go:build !icebath_core && !icebath_noextras

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

	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Argv starting this binary's isolated PM worker, swapped by tests for a re-executed test binary
var workerArgv = func() ([]string, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return append([]string{exe}, pmWorkerArgs()...), nil
}

// Worker exchanges the facades start, replaced by tests the way Python patches the client _request
var (
	clientRequest func(ctx context.Context, operation string, arguments map[string]any, hooks workerHooks, projectRoot string) (any, error)
)

// Binds the foreign checkout worker request and the engine readiness check of a lazy state sync
func init() { clientRequest, coldEngine = request, engineReady }

// Relay environment provisioning an explicit sync of the relay extra runs, replaced by tests
var syncRuntimeEnvironment = EnsureRuntimeEnvironment

// Reports PM's engine toolchain ready without installing anything
func engineReady(ctx context.Context) (*InstallError, error) {
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

// Client side handlers of the worker's progress callbacks in the order the request offers them
func callbackHandlers(hooks workerHooks) (map[string]func(args []any) error, []any) {
	handlers := map[string]func(args []any) error{}
	var offered []any
	if hooks.progress != nil {
		offered = append(offered, "progress")
		handlers["progress"] = func(args []any) error {
			if len(args) != 4 {
				return &PyError{Class: "TypeError", Message: fmt.Sprintf("the progress callback takes 4 values, got %d", len(args))}
			}
			done, _ := jsonNumber(args[1])
			total, _ := jsonNumber(args[2])
			return hooks.progress(pyrepr.Str(args[0]), int64(done), int64(total), pyrepr.Str(args[3]))
		}
	}
	if hooks.downloadProgress != nil {
		offered = append(offered, "download_progress")
		handlers["download_progress"] = func(args []any) error {
			if len(args) != 3 {
				return &PyError{Class: "TypeError", Message: fmt.Sprintf("the download_progress callback takes 3 values, got %d", len(args))}
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
	args := policyArgs{Name: pyrepr.Str(arguments["name"]), Explicit: pyrepr.Truthy(arguments["explicit"]), Repair: pyrepr.Truthy(arguments["repair"])}
	if raw, ok := arguments["extras"]; ok && pyrepr.Truthy(raw) {
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
			names = []string{pyrepr.Str(arguments["name"])}
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
	if err := s.send(message); err != nil && !brokenPipe(err) {
		return nil, err
	}
	if s.pause != nil {
		s.monitor.Add(1)
		go s.watchPause()
	}
	reader := bufio.NewReader(s.stdout)
	var callbackErr error
	var response map[string]any
	var resultLine []byte
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
			return nil, wireSubscriptError(value, "id")
		}
		id, ok := parsed["id"]
		if !ok {
			return nil, wireKeyError("id")
		}
		if !jsonEquals(id, s.id) {
			return nil, NewInstallError("pm", "worker returned a different request id", "")
		}
		kind, ok := parsed["type"]
		if !ok {
			return nil, wireKeyError("type")
		}
		if jsonEquals(kind, "result") {
			response, resultLine = parsed, line
			break
		}
		name, ok := parsed["callback"]
		if !ok {
			return nil, wireKeyError("callback")
		}
		handler, known := callbacks[pyrepr.Str(name)]
		if _, isText := name.(string); !isText {
			known = false
		}
		var runErr error
		if !known {
			runErr = &PyError{Class: "KeyError", Message: "unknown callback " + errchain.Show(name)}
		} else if args, err := wireCallbackArgs(pyrepr.Str(name), parsed, line); err != nil {
			runErr = err
		} else {
			if jsonEquals(name, "download_progress") {
				if ordered, err := loadOrderedJSON(line); err == nil {
					args = orderedCallbackArgs(ordered, args)
				}
			}
			runErr = handler(args)
		}
		if _, hasCall := parsed["call"]; !hasCall {
			return nil, wireKeyError("call")
		}
		if runErr != nil {
			if callbackErr == nil {
				callbackErr = runErr
			}
			if err := s.send(map[string]any{"type": "callback_result", "call": parsed["call"], "error": runErr.Error()}); err != nil {
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
	if err := acceptWorkerReceipt(ctx, orderedWorkerReceipt(resultLine), updateID); err != nil {
		return nil, err
	}
	if callbackErr != nil {
		return nil, callbackErr
	}
	if raw, failed := response["error"]; failed {
		payload, ok := raw.(map[string]any)
		if !ok {
			return nil, wireErrorShape(raw)
		}
		return nil, workerError(payload)
	}
	result, ok := response["result"]
	if !ok {
		return nil, wireKeyError("result")
	}
	return result, nil
}

// KeyError subscripting a worker message raises for a missing key
func wireKeyError(key string) error {
	return &PyError{Class: "KeyError", Message: "the worker message has no " + key}
}

// Error subscripting a decoded worker value raises when it is no object
func wireSubscriptError(v any, key string) error {
	if _, ok := v.(map[string]any); ok {
		return wireKeyError(key)
	}
	return &PyError{Class: "TypeError", Message: fmt.Sprintf("the worker message must be a mapping holding %s, got %s", key, errchain.Kind(v))}
}

// Error for a worker error value that is no mapping
func wireErrorShape(v any) error {
	return &PyError{Class: "TypeError", Message: fmt.Sprintf("the worker error must be a mapping, got %s", errchain.Kind(v))}
}

// Positional arguments of a relayed callback the way star unpacking reads the message args, an empty list when absent
func wireCallbackArgs(name string, message map[string]any, line []byte) ([]any, error) {
	raw, ok := message["args"]
	if !ok {
		return []any{}, nil
	}
	switch v := raw.(type) {
	case []any:
		return v, nil
	case string:
		out := []any{}
		for _, r := range v {
			out = append(out, string(r))
		}
		return out, nil
	case map[string]any:
		ordered, err := loadOrderedJSON(line)
		if err != nil {
			return nil, &PyError{Class: "ValueError", Message: err.Error()}
		}
		doc, _ := ordered.(*OrderedObject)
		var inner any
		if doc != nil {
			inner, _ = doc.Get("args")
		}
		obj, isObject := inner.(*OrderedObject)
		if !isObject || len(obj.Keys) != len(v) {
			return nil, &PyError{Class: "ValueError", Message: "worker message args decoded differently in key order"}
		}
		out := make([]any, len(obj.Keys))
		for i, key := range obj.Keys {
			out[i] = key
		}
		return out, nil
	}
	return nil, &PyError{Class: "TypeError", Message: fmt.Sprintf("the %s callback arguments must be a list, got %s", name, errchain.Kind(raw))}
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

// Syncs an application caller's dependency environment, a foreign checkout through its worker, a projectless tree to its runtime set
func managedSyncVenv(ctx context.Context, opts SyncVenvOptions) error {
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
				"(%s); only an explicit install may change what later processes boot into", errchain.Show(opts.Extras), prefix))
		}
	}
	plugins := opts.Plugins
	if selection, ok := plugins.(*Selection); ok {
		if _, has := selection.Data["expected_config"]; !has {
			homeValue, ok := selection.Data["home"]
			if !ok {
				return &PyError{Class: "KeyError", Message: "the plugin selection has no home"}
			}
			homeDir, ok := homeValue.(string)
			if !ok {
				return &PyError{Class: "TypeError", Message: fmt.Sprintf("the plugin selection home must be text, got %s", errchain.Kind(homeValue))}
			}
			digest, found, err := FileDigest(filepath.Join(homeDir, "config.yaml"))
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
		_, err := clientRequest(ctx, "sync_venv", arguments, workerHooks{}, opts.ProjectRoot)
		return err
	}
	if runtimeOnlyProject(root) && (len(opts.Extras) > 0 || opts.Plugins == nil) {
		if err := validateSyncVenv(opts); err != nil || len(opts.Extras) == 0 {
			return err
		}
		if !opts.Explicit && !opts.Repair {
			hints := make([]string, len(opts.Extras))
			for i, extra := range opts.Extras {
				hints[i] = InstallHint(extra)
			}
			return NewInstallError("venv", "extras are provisioned only when the user switches their feature on, never by a lazy sync",
				"run `"+strings.Join(hints, "`, `")+"`")
		}
		out := opts.Stdout
		if out == nil {
			out = SyncOutput(ctx)
		}
		for _, extra := range opts.Extras {
			canonical, _, known := ResolveExtra(extra)
			if !known {
				return UnknownExtraError(extra)
			}
			var err error
			if canonical == relayExtra {
				_, err = syncRuntimeEnvironment(ctx, RuntimeEnvironmentOptions{Output: out})
			} else {
				_, err = EnsureExtra(ctx, canonical, ExtraOptions{Output: out})
			}
			if err != nil {
				return err
			}
		}
		if opts.Plugins == nil {
			return nil
		}
		opts.Extras, opts.Carried, opts.Automatic = nil, nil, nil
	}
	return clientWorkerCall(ctx, "sync_venv", policyArgs{Explicit: opts.Explicit, Repair: opts.Repair, Extras: opts.Extras}, func(workerCtx context.Context) error {
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

// Runs a path returning Python environment operation through the worker boundary
func pathCall(ctx context.Context, operation string, args policyArgs, run func(context.Context) (string, error)) (string, error) {
	var path string
	err := clientWorkerCall(ctx, operation, args, func(workerCtx context.Context) error {
		var err error
		path, err = run(workerCtx)
		return err
	})
	return path, err
}

// Resolves a project's lock without creating or selecting an environment
func LockProject(ctx context.Context, opts LockProjectOptions) error {
	return clientWorkerCall(ctx, "lock_project", policyArgs{Explicit: opts.Explicit},
		func(c context.Context) error { return lockProject(c, opts) })
}

// Selects a complete isolated dependency generation, retaining the previous one
func EnsureEnvironment(ctx context.Context, opts EnsureEnvironmentOptions) (string, error) {
	return pathCall(ctx, "ensure_environment", policyArgs{Name: opts.Name, Explicit: opts.Explicit},
		func(c context.Context) (string, error) { return ensureEnvironment(c, opts) })
}

// Installs an isolated Python tool and returns its validated executable
func managedEnsurePythonTool(ctx context.Context, opts EnsurePythonToolOptions) (string, error) {
	return pathCall(ctx, "ensure_python_tool", policyArgs{Name: opts.Name, Explicit: opts.Explicit},
		func(c context.Context) (string, error) { return ensurePythonTool(c, opts) })
}

// Probes dependency currency without bootstrapping, a cold foreign worker answering not current
func managedVenvIsCurrent(ctx context.Context, opts VenvIsCurrentOptions) (bool, error) {
	if opts.ProjectRoot == "" || resolvePath(opts.ProjectRoot) == resolvePath(RepoRoot()) {
		return venvIsCurrent(ctx, opts.Extras, opts.Plugins, opts.ProjectRoot)
	}
	result, err := clientRequest(ctx, "venv_is_current", map[string]any{"extras": optionalList(opts.Extras), "plugins": EncodePluginInput(opts.Plugins)},
		workerHooks{}, opts.ProjectRoot)
	var install *InstallError
	if errors.As(err, &install) && install.Package == "pm-runtime" {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return pyrepr.Truthy(result), nil
}

// Checks a project lock is current
func CheckProjectLock(ctx context.Context, opts CheckProjectLockOptions) error {
	return clientWorkerCall(ctx, "check_project_lock", policyArgs{Explicit: opts.Explicit},
		func(c context.Context) error { return checkProjectLock(c, opts) })
}

// Receipt of a worker result line in the key order the worker wrote it, nil when absent or not an object
func orderedWorkerReceipt(line []byte) any {
	root, err := loadOrderedJSON(line)
	if err != nil {
		return nil
	}
	doc, ok := root.(*OrderedObject)
	if !ok {
		return nil
	}
	receipt, _ := doc.Get("receipt")
	if obj, ok := receipt.(*OrderedObject); ok && obj != nil {
		return obj
	}
	return nil
}
