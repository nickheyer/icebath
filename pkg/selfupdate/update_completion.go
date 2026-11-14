package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
	"github.com/nickheyer/icebath/pkg/tools"
)

// The one whole line the desktop hand off scripts match for a build the update still owes
const DesktopBuildOwed = "Desktop app build owed:"

// Argument of the completion entry that runs the libatomic pre install of the new tree
const completionInstallBeforeLock = "--install-before-lock"

// Steps of the completion process, the host behaviour unless a hermes test replaces them
var (
	completionBinary          = pm.ResolveInstallBinary
	completionStdoutTTY       = func() bool { return fileIsTTY(os.Stdout) }
	completionCompleteTail    = CompleteSourceCheckout
	completionSweep           = sweepBytecodeAfterUpdate
	completionSkipReason      = fleetRestartSkipReason
	completionPendingFleet    = func(ctx context.Context) (bool, error) { return pendingFleetRestartNeeded(ctx, nil, nil, false) }
	completionRestartFleet    = restartGatewayFleetAfterUpdate
	completionMergeResume     = resumeWindowsGatewaysAndMergeOutcome
	completionVerifyFleet     = verifyFleetAfterUpdate
	completionResume          = resumeWindowsGatewaysAfterUpdate
	completionSyncVenv        = pm.SyncVenv
	completionPlatform        = runtime.GOOS
	completionJoinCheckout    = updaterecord.AcquireCheckout
	completionArm             = ArmCompletion
	completionEnsureTools     = pm.EnsureToolsForSync
	completionCollect         = pm.CollectSupersededGenerations
	completionWorkerScope     = pm.WorkerContext
	completionLastReceipt     = pm.LastForUpdate
	completionActivation      = pm.ActivationEnvironment
	completionRunPrepared     = func(child *exec.Cmd) error { return child.Run() }
	completionTerminalReceipt = readTerminalReceipt
)

// Parent pipe that answers isatty with the parent terminal's answer and forwards every other lookup
type forwardedOutput struct {
	stream   io.Writer
	terminal bool
}

func (f *forwardedOutput) Write(p []byte) (int, error) { return f.stream.Write(p) }

func (f *forwardedOutput) IsATTY() bool { return f.terminal }

func (f *forwardedOutput) Unwrap() io.Writer { return f.stream }

// Writes JSON through a sibling with a tmp suffix and replaces the target
func writeCompletionJSON(path string, data any) error {
	text, err := pyjson.Dumps(data)
	if err != nil {
		return err
	}
	temporary := strings.TrimSuffix(path, filepath.Ext(path)) + ".tmp"
	if err := os.WriteFile(temporary, []byte(text), 0o666); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}

// Exit status a shell reports, a signal death becoming 128 plus the signal
func completionExitStatus(code int) int {
	if code >= 0 {
		return code
	}
	return 128 - code
}

// Update id of the receipt a completion request carries, the KeyError or TypeError of a missing one
func completionUpdateID(request map[string]any) (string, error) {
	receipt, present := request["receipt"]
	if !present {
		return "", errors.New(pyKeyErrorText("receipt"))
	}
	id, err := pyDictItem(receipt, "update_id")
	if err != nil {
		return "", err
	}
	return pyrepr.Str(id), nil
}

// Answer of the bootstrap child with the resume token left to the parent
func writeBootstrapResult(request map[string]any, resultPath string, code int, receipt any) (int, error) {
	updateID, err := completionUpdateID(request)
	if err != nil {
		return 0, err
	}
	err = writeCompletionJSON(resultPath, pyjson.ObjectOf("schema", 1, "update_id", updateID, "exit_code", code,
		"receipt", receipt, "windows_resume", nil, "pm_receipt", request["pm_receipt"]))
	return code, err
}

// Binds the suspended Windows completion child to the update job, recording a refusal, then resumes it
func bindAndResume(ctx context.Context, w io.Writer, process *os.Process, request map[string]any, requestPath string) error {
	if refusal := updaterecord.BindChildToUpdateTree(process.Pid); refusal != nil {
		detail := fmt.Sprintf("the update's job would not take the completion child (%s), so it runs outside the job, holding its own checkout lease", launchErrorText(refusal))
		fmt.Fprintf(w, "  ⚠ Update completion: %s\n", detail)
		updaterecord.RecordStep(ctx, "update_custody", false, detail)
		updateID, err := completionUpdateID(request)
		if err != nil {
			return err
		}
		if current := updaterecord.CurrentReceipt(ctx); current != nil && current.Data.GetOr("update_id", nil) == updateID {
			request["receipt"] = pyjson.Clone(current.Data)
			if err := writeCompletionJSON(requestPath, request); err != nil {
				return err
			}
		}
	}
	return updaterecord.ResumeSuspendedChild(process)
}

// Incremental utf-8 decoder with replacement that holds an incomplete trailing sequence for the next chunk
type replaceDecoder struct{ pending []byte }

func (d *replaceDecoder) decode(chunk []byte, final bool) string {
	data := append(d.pending, chunk...)
	d.pending = nil
	if !final {
		cut := len(data)
		for back := 1; back <= 3 && back <= len(data); back++ {
			start := len(data) - back
			if utf8.RuneStart(data[start]) {
				if !utf8.FullRune(data[start:]) {
					cut = start
				}
				break
			}
		}
		d.pending = append([]byte{}, data[cut:]...)
		data = data[:cut]
	}
	return pystr.DecodeUTF8Replace(data)
}

// Runs the completion of a moved checkout in a fresh process holding the checkout lock and checks its correlated result
func runCompletion(ctx context.Context, w io.Writer, request map[string]any) (map[string]any, error) {
	sourceValue, present := request["source"]
	if !present {
		return nil, errors.New(pyKeyErrorText("source"))
	}
	root := pyrepr.Str(sourceValue)
	binary := completionBinary(root)
	if runtime.GOOS == "linux" && binary != "" {
		pre := exec.CommandContext(ctx, binary, UpdateCompletionEntry, completionInstallBeforeLock)
		pre.Dir, pre.Stdout, pre.Stderr = root, w, nil
		_ = pre.Run()
	}
	homeValue, present := request["home"]
	if !present {
		return nil, errors.New(pyKeyErrorText("home"))
	}
	env := []string{}
	for _, kv := range os.Environ() {
		key, _, _ := strings.Cut(kv, "=")
		if key == home.EnvHome || key == "PYTHONUNBUFFERED" || key == "PYTHONPATH" || key == "PYTHONHOME" || key == "VIRTUAL_ENV" {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, home.EnvHome+"="+pyrepr.Str(homeValue), "PYTHONUNBUFFERED=1")
	directory, err := os.MkdirTemp("", "icebath-completion-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(directory)
	requestPath, resultPath := filepath.Join(directory, "request.json"), filepath.Join(directory, "result.json")
	request = maps.Clone(request)
	request["stdout_isatty"] = completionStdoutTTY()
	request["bytecode_cache"] = filepath.Join(directory, "bytecode")
	if err := writeCompletionJSON(requestPath, request); err != nil {
		return nil, err
	}
	if binary == "" {
		return nil, &exec.Error{Name: filepath.Join(root, "icebath"), Err: os.ErrNotExist}
	}
	child := exec.Command(binary, UpdateCompletionEntry, requestPath, resultPath)
	child.Dir, child.Env = root, env
	child.SysProcAttr = completionChildAttr()
	if runtime.GOOS != "windows" {
		files, err := updaterecord.CheckoutLockFiles(root)
		if err != nil {
			return nil, err
		}
		child.ExtraFiles = files
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	child.Stdout, child.Stderr = writer, writer
	if err := child.Start(); err != nil {
		reader.Close()
		writer.Close()
		return nil, err
	}
	writer.Close()
	done := make(chan error, 1)
	cleanup := func(cause error) error {
		var cleanupErr error
		if err := killCompletionTree(child.Process); err != nil {
			cleanupErr = err
		}
		_ = child.Process.Kill()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			if cleanupErr == nil {
				cleanupErr = &recoveryTimeout{argv: child.Args, seconds: 10}
			}
		}
		reader.Close()
		if cleanupErr != nil {
			return fmt.Errorf("%w\nCompletion cleanup failed; child processes may still be running.", cause)
		}
		return cause
	}
	started := false
	if runtime.GOOS == "windows" {
		if err := bindAndResume(ctx, w, child.Process, request, requestPath); err != nil {
			go func() { done <- child.Wait() }()
			return nil, cleanup(err)
		}
	}
	started = true
	chunks := make(chan []byte)
	go func() {
		buf := make([]byte, 8192)
		for {
			n, err := reader.Read(buf)
			if n > 0 {
				chunks <- append([]byte{}, buf[:n]...)
			}
			if err != nil {
				close(chunks)
				return
			}
		}
	}()
	decoder := &replaceDecoder{}
	for open := started; open; {
		select {
		case chunk, ok := <-chunks:
			if !ok {
				fmt.Fprint(w, decoder.decode(nil, true))
				open = false
				break
			}
			fmt.Fprint(w, decoder.decode(chunk, false))
		case <-ctx.Done():
			go func() { done <- child.Wait() }()
			return nil, cleanup(ctx.Err())
		}
	}
	waitErr := child.Wait()
	reader.Close()
	code := 0
	if waitErr != nil {
		var exit *exec.ExitError
		if !errors.As(waitErr, &exit) {
			return nil, waitErr
		}
		code = pysubprocess.Returncode(exit)
	}
	code = completionExitStatus(code)
	result, err := readCompletionResult(request, resultPath, code)
	if err != nil {
		var lost *completionLost
		if errors.As(err, &lost) {
			reported := code
			if reported == 0 {
				reported = 1
			}
			return map[string]any{"exit_code": reported, "receipt": nil, "windows_resume": nil,
				"error": fmt.Sprintf("%s (the completion process exited %d)", lost.text, code)}, nil
		}
		return nil, err
	}
	return result, nil
}

// A completion result that does not attest the run, the OSError, ValueError, KeyError or TypeError text
type completionLost struct{ text string }

func (e *completionLost) Error() string { return e.text }

// Reads and checks the correlated result of a completion process
func readCompletionResult(request map[string]any, resultPath string, code int) (map[string]any, error) {
	lost := func(text string) error { return &completionLost{text: text} }
	raw, err := os.ReadFile(resultPath)
	if err != nil {
		return nil, lost(launchErrorText(err))
	}
	text, err := pystr.DecodeUTF8Sig(raw)
	if err != nil {
		return nil, lost(err.Error())
	}
	decoded, err := pyjson.Loads(text)
	if err != nil {
		return nil, lost(err.Error())
	}
	updateID, err := completionUpdateID(request)
	if err != nil {
		return nil, lost(err.Error())
	}
	schema, err := pyDictItem(decoded, "schema")
	if err != nil {
		return nil, lost(err.Error())
	}
	result := decoded.(*pyjson.Object)
	if !pyjson.Equal(schema, 1) {
		return nil, lost("completion response identity mismatch")
	}
	resultID, err := pyDictItem(result, "update_id")
	if err != nil {
		return nil, lost(err.Error())
	}
	if !pyjson.Equal(resultID, updateID) {
		return nil, lost("completion response identity mismatch")
	}
	exitCode, err := pyDictItem(result, "exit_code")
	if err != nil {
		return nil, lost(err.Error())
	}
	if !pyjson.Equal(exitCode, code) {
		return nil, lost("completion response disagrees with process exit")
	}
	receipt := result.GetOr("receipt", nil)
	if receipt != nil {
		obj, ok := receipt.(*pyjson.Object)
		if !ok {
			return nil, fmt.Errorf("'%s' object has no attribute 'get'", pyrepr.TypeName(receipt))
		}
		if !pyjson.Equal(obj.GetOr("update_id", nil), updateID) || !pyrepr.Truthy(obj.GetOr("finished_at", nil)) ||
			(code == 0) != (obj.GetOr("outcome", nil) == "success") {
			return nil, lost("completion receipt does not attest this outcome")
		}
	}
	if code == 0 && receipt == nil {
		return nil, lost("completion did not publish a terminal receipt")
	}
	out := map[string]any{}
	for _, key := range result.Keys {
		out[key] = result.Vals[key]
	}
	if token, ok := out["windows_resume"].(*pyjson.Object); ok {
		out["windows_resume"] = pyjson.Plain(token)
	}
	return out, nil
}

// Terminal record of the request's run, the finalized one this process holds when the store refused it
func readTerminalReceipt(ctx context.Context, request map[string]any) (*pyjson.Object, error) {
	updateID, err := completionUpdateID(request)
	if err != nil {
		return nil, err
	}
	if _, found, ok := updaterecord.ReadRunRecord(ctx, updateID); ok && pyrepr.Truthy(found.GetOr("finished_at", nil)) {
		return found, nil
	}
	return updaterecord.FinalizedReceipt(updateID), nil
}

// Still running record of the request's run without its writer pid, nil otherwise
func runningRecord(ctx context.Context, request map[string]any) (*pyjson.Object, error) {
	updateID, err := completionUpdateID(request)
	if err != nil {
		return nil, err
	}
	_, found, ok := updaterecord.ReadRunRecord(ctx, updateID)
	if !ok || found.GetOr("outcome", nil) != "running" {
		return nil, nil
	}
	found.Delete("writer_pid")
	return found, nil
}

// The unsettled autostash notice when this run parked the user's local changes, empty otherwise
func owedUserAction(request map[string]any) string {
	message := ""
	if pyrepr.Truthy(request["completion_message"]) {
		message = pyrepr.Str(request["completion_message"])
	}
	if message != "" && !strings.HasPrefix(message, "✓") {
		return message
	}
	return ""
}

// Lands parked local changes on the receipt, true when nothing is owed to the user
func recordOwedUserAction(ctx context.Context, request map[string]any) bool {
	notice := owedUserAction(request)
	if notice != "" {
		updaterecord.RecordUserAction(ctx, "local_changes", notice)
	}
	return notice == ""
}

// Publishes exit 0 for the gateway update watcher of a gateway mode run, an unwritable home only warning
func publishGatewaySuccess(w io.Writer, request map[string]any) {
	if !pyrepr.Truthy(request["gateway_mode"]) {
		return
	}
	err := func() error {
		homeValue, present := request["home"]
		if !present {
			return errors.New(pyKeyErrorText("home"))
		}
		marker := filepath.Join(pyrepr.Str(homeValue), ".update_exit_code")
		temporary := marker + ".tmp"
		if err := os.WriteFile(temporary, []byte("0"), 0o666); err != nil {
			return err
		}
		return os.Rename(temporary, marker)
	}()
	if err != nil {
		fmt.Fprintf(w, "  ⚠ Could not publish the gateway's update status: %s\n", launchErrorText(err))
	}
}

// Names the owed desktop build when this run never reached the build stage
func reportDesktopBuildOwed(w io.Writer, request map[string]any, record *pyjson.Object, reason string) {
	if !pyrepr.Truthy(request["desktop"]) {
		return
	}
	if record != nil {
		if stages, ok := record.GetOr("stages", nil).([]any); ok {
			for _, mark := range stages {
				if obj, isObj := mark.(*pyjson.Object); isObj && obj.GetOr("name", nil) == "build" {
					return
				}
			}
		}
	}
	fmt.Fprintf(w, "  %s %s\n", DesktopBuildOwed, reason)
}

// Settles a committed run whose completion never answered as owed work, the result the caller adopts
func settleLostCompletion(ctx context.Context, w io.Writer, request map[string]any, reason string) (map[string]any, error) {
	fmt.Fprintf(w, "  ⚠ Source update completion did not finish: %s\n", reason)
	receipt, err := completionTerminalReceipt(ctx, request)
	if err != nil {
		return nil, err
	}
	if receipt == nil {
		if source, present := request["source"]; !present {
			fmt.Fprintf(w, "  ⚠ Could not record the owed source-update tail: %s\n", pyKeyErrorText("source"))
		} else if _, err := ArmCompletion(ctx, pyrepr.Str(source)); err != nil {
			if isInterrupt(err) {
				return nil, err
			}
			fmt.Fprintf(w, "  ⚠ Could not record the owed source-update tail: %s\n", launchErrorText(err))
		}
		record, err := runningRecord(ctx, request)
		if err != nil {
			return nil, err
		}
		reportDesktopBuildOwed(w, request, record, "the update completion did not finish")
		updaterecord.ResumeRunRecord(ctx)
		updaterecord.RecordFollowup(ctx, "completion", reason, "the next launch or `icebath update` finishes it")
		parked := !recordOwedUserAction(ctx, request)
		if parked {
			fmt.Fprintln(w, owedUserAction(request))
		}
		code := 0
		if parked {
			code = 1
		}
		updaterecord.FinalizePendingUpdateReceipt(ctx, &code, "completion owed after the code was updated")
		if receipt, err = completionTerminalReceipt(ctx, request); err != nil {
			return nil, err
		}
	}
	var outcome any
	if receipt != nil {
		outcome = receipt.GetOr("outcome", nil)
	}
	code := 0
	switch outcome {
	case "interrupted":
		code = 130
	case "partial":
		code = 1
	}
	if code == 0 {
		publishGatewaySuccess(w, request)
	}
	var receiptValue any
	if receipt != nil {
		receiptValue = receipt
	}
	return map[string]any{"exit_code": code, "receipt": receiptValue, "windows_resume": nil, "pm_receipt": nil}, nil
}

// Settles a failure after the tree moved as an owed follow up and writes the bootstrap result
func settleAfterCommit(ctx context.Context, w io.Writer, request map[string]any, resultPath, step, reason string) (int, error) {
	source, present := request["source"]
	if !present {
		return 0, errors.New(pyKeyErrorText("source"))
	}
	if _, err := completionArm(ctx, pyrepr.Str(source)); err != nil {
		if !pypath.IsOSError(err) {
			return 0, err
		}
		fmt.Fprintf(w, "  ⚠ Could not record the owed source-update tail: %s\n", launchErrorText(err))
	}
	receipt, err := completionTerminalReceipt(ctx, request)
	if err != nil {
		return 0, err
	}
	if receipt == nil {
		record, err := runningRecord(ctx, request)
		if err != nil {
			return 0, err
		}
		if record == nil {
			obj, ok := request["receipt"].(*pyjson.Object)
			if !ok {
				return 0, fmt.Errorf("'%s' object has no attribute 'get'", pyrepr.TypeName(request["receipt"]))
			}
			record = obj
		}
		reportDesktopBuildOwed(w, request, record, step+" did not finish")
		if _, err := pyDictItem(record, "update_id"); err != nil {
			return 0, err
		}
		updaterecord.ResumeReceipt(ctx, record)
		stage, retry := "build", "the next launch or `icebath update` retries it"
		if step == "dependencies" {
			stage, retry = "deps", "dependencies not installed yet — the next launch retries"
		}
		updaterecord.RecordStage(ctx, stage, "failed", nil)
		updaterecord.RecordFollowup(ctx, step, reason, retry)
		if !recordOwedUserAction(ctx, request) {
			fmt.Fprintln(w, owedUserAction(request))
		}
		zero := 0
		updaterecord.FinalizePendingUpdateReceipt(ctx, &zero, step+" owed after the code was updated")
		if receipt, err = completionTerminalReceipt(ctx, request); err != nil {
			return 0, err
		}
	}
	code := 1
	if receipt != nil && receipt.GetOr("outcome", nil) == "success" {
		code = 0
	}
	if code == 0 {
		publishGatewaySuccess(w, request)
	}
	var receiptValue any
	if receipt != nil {
		receiptValue = receipt
	}
	return writeBootstrapResult(request, resultPath, code, receiptValue)
}

// Syncs the new tree's dependencies under the request's correlation and runs the prepared completion child
func prepareCompletion(ctx context.Context, w io.Writer, request map[string]any, requestPath, resultPath string) (int, error) {
	source, present := request["source"]
	if !present {
		return 0, errors.New(pyKeyErrorText("source"))
	}
	root := pyrepr.Str(source)
	updateID, err := completionUpdateID(request)
	if err != nil {
		return 0, err
	}
	if completionPlatform == "windows" {
		refused, err := completionJoinCheckout(root)
		if err != nil {
			return 0, err
		}
		if refused != nil {
			why := refused.Reason
			if why == "" {
				why = fmt.Sprintf("held by process %d", refused.PID)
			}
			return 0, runtimeErr("could not join the update's checkout lock (" + why + ")")
		}
	}
	if _, err := completionArm(ctx, root); err != nil {
		return 0, err
	}
	tools.ConvertTreelessCheckoutFirst(ctx, root)
	err = func() error {
		leave := completionWorkerScope(ctx, &updateID)
		defer leave()
		defer func() {
			var last any
			if receipt := completionLastReceipt(ctx, updateID, false); receipt != nil {
				last = receipt
			}
			request["pm_receipt"] = last
			if werr := writeCompletionJSON(requestPath, request); werr != nil && err == nil {
				err = werr
			}
		}()
		if err := completionEnsureTools(ctx); err != nil {
			return err
		}
		if err := completionSyncVenv(ctx, pm.SyncVenvOptions{Explicit: true, ProjectRoot: root, EvictIncompatiblePlugins: true}); err != nil {
			return err
		}
		return completionCollect(ctx, root)
	}()
	if err != nil {
		return 0, err
	}
	binary := completionBinary(root)
	if binary == "" {
		return 0, &exec.Error{Name: filepath.Join(root, "icebath"), Err: os.ErrNotExist}
	}
	env, err := completionActivation(ctx, root)
	if err != nil {
		return 0, err
	}
	child := exec.Command(binary, UpdateCompletionEntry, requestPath, resultPath, "--prepared")
	child.Dir, child.Env = root, env
	child.Stdin, child.Stdout, child.Stderr = os.Stdin, os.Stdout, os.Stderr
	if completionPlatform != "windows" {
		files, err := updaterecord.CheckoutLockFiles(root)
		if err != nil {
			return 0, err
		}
		child.ExtraFiles = files
	}
	code := 0
	if err := completionRunPrepared(child); err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			return 0, err
		}
		code = pysubprocess.Returncode(exit)
	}
	code = completionExitStatus(code)
	if _, err := os.Stat(resultPath); err != nil {
		return settleAfterCommit(ctx, w, request, resultPath, "completion", fmt.Sprintf("the completion process exited %d without a result", code))
	}
	return code, nil
}

// Everything after the commit point, false only when the user's local changes stay parked
func completeSelected(ctx context.Context, w, errw io.Writer, request map[string]any) (bool, error) {
	source, present := request["source"]
	if !present {
		return false, errors.New(pyKeyErrorText("source"))
	}
	root := pyrepr.Str(source)
	fleetProjectRoot = func() string { return root }
	complete := recordOwedUserAction(ctx, request)
	item := func(key string) (any, error) {
		value, present := request[key]
		if !present {
			return nil, errors.New(pyKeyErrorText(key))
		}
		return value, nil
	}
	siblings, err := item("sibling_snapshots")
	if err != nil {
		return false, err
	}
	if obj, ok := pyjson.AsObject(siblings); ok {
		lastSiblingSnapshots = obj
	} else {
		lastSiblingSnapshots = pyjson.NewObject()
	}
	planData, err := item("plan")
	if err != nil {
		return false, err
	}
	var plan *UpdatePlan
	if planData != nil {
		if plan, err = UpdatePlanFromDict(planData); err != nil {
			return false, err
		}
	}
	branch, err := item("branch")
	if err != nil {
		return false, err
	}
	completionSweep(ctx, w, pyrepr.Str(branch))
	followups := [][2]string{}
	runtimeSafe := false
	values := map[string]any{}
	for _, key := range []string{"desktop", "assume_yes", "gateway_mode", "snapshot_id", "pre_update_version"} {
		if values[key], err = item(key); err != nil {
			return false, err
		}
	}
	announce := "\n✓ Code updated!"
	if pyrepr.Truthy(request["completion_message"]) {
		announce = ""
	}
	safe, err := completionCompleteTail(ctx, root, SourceCompletion{
		Desktop: pyrepr.Truthy(values["desktop"]), AssumeYes: pyrepr.Truthy(values["assume_yes"]),
		GatewayMode: pyrepr.Truthy(values["gateway_mode"]), PreUpdateSnapshotID: optionalText(values["snapshot_id"]),
		PreUpdateVersion: optionalText(values["pre_update_version"]), CompletionMessage: optionalText(request["completion_message"]),
		Announce: announce, Followups: &followups, BeforeBuild: func() { resumePausedBeforeBuild(ctx, w, request) },
		Stdout: w, Stderr: errw})
	if err != nil {
		if isInterrupt(err) {
			return false, err
		}
		reason := pyReason(err)
		reportDesktopBuildOwed(w, request, nil, "the source-update tail did not run")
		updaterecord.RecordFollowup(ctx, "completion", reason, updaterecord.FollowupRetryDefault)
		followups = append(followups, [2]string{"completion", reason})
	} else {
		runtimeSafe = safe
	}
	tailOwed := false
	for _, step := range followups {
		tailOwed = tailOwed || updaterecord.TailFollowups[step[0]]
	}
	updaterecord.RecordBuildStage(ctx, followups)
	if !tailOwed {
		if err := ClearCompletion(ctx, root); err != nil {
			return false, err
		}
	}
	gatewayMode := pyrepr.Truthy(values["gateway_mode"])
	if gatewayMode {
		updaterecord.WriteGatewayUpdateExitCode(ctx, complete)
	}
	if pyrepr.Truthy(request["no_gateway_restart"]) {
		updaterecord.RecordSkip(ctx, "gateway_restart", "--no-gateway-restart: deferred, marker kept")
		updaterecord.RecordStage(ctx, "restart", "skipped", nil)
		fmt.Fprintln(w, "→ Gateway restart deferred (--no-gateway-restart); restart gateways separately.")
		return complete, nil
	}
	skip, skipping := completionSkipReason(ctx, plan)
	if skipping {
		needed, err := completionPendingFleet(ctx)
		if err != nil {
			return false, err
		}
		if needed {
			fmt.Fprintf(w, "  → Gateway restart not skipped (%s): gateways are still off the checkout code.\n", skip)
			skipping = false
		}
	}
	if skipping {
		updaterecord.RecordSkip(ctx, "gateway_restart", skip)
		updaterecord.RecordStage(ctx, "restart", "skipped", nil)
		fmt.Fprintf(w, "  ✓ Gateway restart skipped: %s.\n", skip)
		return complete, nil
	}
	restart, err := completionRestartFleet(ctx, w, plan, gatewayMode)
	if err != nil {
		return false, err
	}
	outcome := "success"
	if restart != nil && restart.Incomplete {
		outcome = "failed"
	}
	updaterecord.RecordStage(ctx, "restart", outcome, nil)
	token, err := item("windows_resume")
	if err != nil {
		return false, err
	}
	resume, _ := token.(map[string]any)
	completionMergeResume(ctx, w, restart, resume, gatewayMode)
	if err := completionVerifyFleet(ctx, w, restart, plan, token, runtimeSafe && complete); err != nil {
		return false, err
	}
	return complete, nil
}

// Restarts the gateways the POSIX pause stopped before the product builds, a failure retried after the build
func resumePausedBeforeBuild(ctx context.Context, w io.Writer, request map[string]any) {
	token, _ := request["windows_resume"].(map[string]any)
	if len(token) == 0 || !pyrepr.Truthy(token["resume_needed"]) || token["platform"] != "posix" {
		return
	}
	if err := completionResume(ctx, w, token); err != nil {
		fmt.Fprintf(w, "  ⚠ Paused gateway restart incomplete (%s); retrying after the build\n", launchErrorText(err))
	}
}

// Names the owed desktop build when the tail raised before the build stage of an open run
func reportUnbuiltDesktop(ctx context.Context, w io.Writer, request map[string]any) {
	if current := updaterecord.CurrentReceipt(ctx); current != nil {
		reportDesktopBuildOwed(w, request, current.Data, "the source-update tail did not finish")
	}
}

// Runs the prepared completion in the selected tree and writes its correlated result
func finishCompletion(ctx context.Context, w, errw io.Writer, request map[string]any, resultPath string) (int, error) {
	receipt, ok := request["receipt"].(*pyjson.Object)
	if !ok {
		return 0, fmt.Errorf("'%s' object is not subscriptable", pyrepr.TypeName(request["receipt"]))
	}
	updateID, err := completionUpdateID(request)
	if err != nil {
		return 0, err
	}
	updaterecord.ResumeReceipt(ctx, receipt)
	if err := pm.AcceptWorkerReceipt(ctx, request["pm_receipt"], &updateID); err != nil {
		return 0, err
	}
	updaterecord.RecordStage(ctx, "deps", "success", nil)
	code, reason := 0, "source update completion"
	complete, err := completeSelected(ctx, w, errw, request)
	var exit *pm.SystemExit
	switch {
	case err == nil:
		if !complete {
			code, reason = 1, "local changes left in the stash; re-apply them by hand"
		}
	case isInterrupt(err):
		code, reason = 130, "KeyboardInterrupt: interrupted after the code was updated"
		updaterecord.FinalizeInterruptedUpdateReceipt(ctx, reason, code)
	case errors.As(err, &exit):
		if exit.Code != 0 {
			reportUnbuiltDesktop(ctx, w, request)
			updaterecord.RecordFollowup(ctx, "completion", fmt.Sprintf("completion exited %d", exit.Code), updaterecord.FollowupRetryDefault)
		}
	default:
		reportUnbuiltDesktop(ctx, w, request)
		updaterecord.RecordFollowup(ctx, "completion", launchErrorClass(err)+": "+launchErrorText(err), updaterecord.FollowupRetryDefault)
	}
	if code != 0 {
		if refusal := updaterecord.RefusalNotice("icebath update"); refusal != nil {
			fmt.Fprintln(w, *refusal)
		}
	}
	token, _ := request["windows_resume"].(map[string]any)
	if err := completionResume(ctx, w, token); err != nil {
		if err := updaterecord.OweFollowup(ctx, &updateID, "windows_resume", "Windows gateway recovery failed: "+launchErrorText(err), updaterecord.FollowupRetryDefault); err != nil {
			return 0, err
		}
	}
	updaterecord.FinalizePendingUpdateReceipt(ctx, &code, reason)
	terminal, err := completionTerminalReceipt(ctx, request)
	if err != nil {
		return 0, err
	}
	if terminal != nil && terminal.GetOr("outcome", nil) == "success" {
		code = 0
	}
	if code != 0 && pyrepr.Truthy(request["gateway_mode"]) {
		updaterecord.WriteGatewayUpdateExitCode(ctx, false)
	}
	if terminal == nil && code == 0 {
		code = 1
	}
	var terminalValue any
	if terminal != nil {
		terminalValue = terminal
	}
	if err := writeCompletionJSON(resultPath, pyjson.ObjectOf("schema", 1, "update_id", updateID, "exit_code", code,
		"receipt", terminalValue, "windows_resume", request["windows_resume"])); err != nil {
		return 0, err
	}
	return code, nil
}

// Dependency preparation in the bootstrap process, every failure after the commit an owed follow up
func bootstrapCompletion(ctx context.Context, w io.Writer, request map[string]any, requestPath, resultPath string) (int, error) {
	code, err := prepareCompletion(ctx, w, request, requestPath, resultPath)
	if err == nil {
		return code, nil
	}
	if isInterrupt(err) {
		receipt, ok := request["receipt"].(*pyjson.Object)
		if !ok {
			return 0, fmt.Errorf("'%s' object is not subscriptable", pyrepr.TypeName(request["receipt"]))
		}
		updaterecord.ResumeReceipt(ctx, receipt)
		updaterecord.FinalizeInterruptedUpdateReceipt(ctx, "KeyboardInterrupt: interrupted while installing dependencies", 130)
		terminal, err := completionTerminalReceipt(ctx, request)
		if err != nil {
			return 0, err
		}
		var terminalValue any
		if terminal != nil {
			terminalValue = terminal
		}
		return writeBootstrapResult(request, resultPath, 130, terminalValue)
	}
	return settleAfterCommit(ctx, w, request, resultPath, "dependencies", pyReason(err))
}

// Entry of the completion process the source update runs in the selected tree, its exit status
func UpdateCompletionMain(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if slices.Equal(args, []string{completionInstallBeforeLock}) {
		pm.InstallLibatomicBeforeLock(ctx)
		return 0
	}
	code, err := updateCompletion(ctx, args, stdout, stderr)
	if err != nil {
		if isInterrupt(err) {
			return 130
		}
		fmt.Fprintf(stderr, "%s: %s\n", launchErrorClass(err), launchErrorText(err))
		return 1
	}
	return code
}

// Body of the completion entry, the error an uncaught Python exception stands for
func updateCompletion(ctx context.Context, args []string, stdout, stderr io.Writer) (int, error) {
	if len(args) < 2 {
		return 0, &pm.PyError{Class: "ValueError", Message: fmt.Sprintf("not enough values to unpack (expected 2, got %d)", len(args))}
	}
	requestPath, resultPath := args[0], args[1]
	decoded, err := readRequestFile(requestPath)
	if err != nil {
		return 0, err
	}
	schema, err := pyDictItem(decoded, "schema")
	if err != nil {
		return 0, err
	}
	if !pyjson.Equal(schema, 1) {
		return 0, &pm.PyError{Class: "ValueError", Message: "unsupported source completion request"}
	}
	object := decoded.(*pyjson.Object)
	request := map[string]any{}
	for _, key := range object.Keys {
		request[key] = object.Vals[key]
	}
	if token, ok := request["windows_resume"].(*pyjson.Object); ok {
		request["windows_resume"] = pyjson.Plain(token)
	}
	exe, err := runningExecutable()
	if err != nil {
		return 0, err
	}
	resolvedExe, err := pypath.Resolve(exe)
	if err != nil {
		return 0, err
	}
	source, err := pyDictItem(object, "source")
	if err != nil {
		return 0, err
	}
	resolvedSource, err := pypath.Resolve(pyrepr.Str(source))
	if err != nil {
		return 0, err
	}
	if filepath.Dir(resolvedExe) != resolvedSource {
		return 0, &pm.PyError{Class: "ValueError", Message: "completion checkout does not match request"}
	}
	isatty := pyrepr.Truthy(request["stdout_isatty"])
	cliui.Default().StdoutTTY = func() bool { return isatty }
	stdout = &forwardedOutput{stream: stdout, terminal: isatty}
	if slices.Contains(args[2:], "--prepared") {
		if err := pm.ActivateDependencies(ctx, resolvedSource); err != nil {
			return 0, err
		}
		return finishCompletion(ctx, stdout, stderr, request, resultPath)
	}
	return bootstrapCompletion(ctx, stdout, request, requestPath, resultPath)
}
