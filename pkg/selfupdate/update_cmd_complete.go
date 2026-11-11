package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

// Name the Windows gateway resume is registered under among the command's exit hooks
const resumeWindowsGatewaysHook = "resume_windows_gateways_after_update"

// Exit hooks of the update command, run newest first once the command boundary has returned
var updateExitHooks struct {
	sync.Mutex
	hooks []updateExitHook
}

// One registered exit hook and the name it is unregistered by
type updateExitHook struct {
	name string
	run  func() error
}

// Registers a hook the command runs after its boundary, the atexit net of a paused gateway resume
func registerUpdateExitHook(name string, run func() error) {
	updateExitHooks.Lock()
	defer updateExitHooks.Unlock()
	updateExitHooks.hooks = append(updateExitHooks.hooks, updateExitHook{name: name, run: run})
}

// Drops every exit hook registered under name
func unregisterUpdateExitHook(name string) {
	updateExitHooks.Lock()
	defer updateExitHooks.Unlock()
	kept := updateExitHooks.hooks[:0]
	for _, hook := range updateExitHooks.hooks {
		if hook.name != name {
			kept = append(kept, hook)
		}
	}
	updateExitHooks.hooks = kept
}

// Runs and clears the exit hooks newest first, a failing hook reported on stderr and never raised
func runUpdateExitHooks(stderr io.Writer) {
	updateExitHooks.Lock()
	hooks := updateExitHooks.hooks
	updateExitHooks.hooks = nil
	updateExitHooks.Unlock()
	for i := len(hooks) - 1; i >= 0; i-- {
		if err := hooks[i].run(); err != nil {
			fmt.Fprintf(stderr, "Exception ignored in atexit callback %s: %s: %s\n", hooks[i].name, launchErrorClass(err), launchErrorText(err))
		}
	}
}

// The admission gate and the GitHub distance count, the two calls the check tests replace
var (
	updateAdmission     = evaluateUpdateAdmission
	updateCompareBehind = githubCompareBehind
)

// Channel this run updates from, main for an explicit branch, then the transient flag, then the install record
func sourceUpdateChannel(ctx context.Context, args map[string]any, channel *string, branchExplicit bool) (string, error) {
	transient := channel
	if value := args["channel"]; transient == nil && value != nil {
		text := pyrepr.Str(value)
		transient = &text
	}
	return SourceUpdateChannel(ctx, branchExplicit || pyrepr.Truthy(args["branch"]), transient, fleetProjectRoot())
}

// Implements the update check, fetching and reporting behind-ness without installing
func cmdUpdateCheck(ctx context.Context, w io.Writer, branch string, branchExplicit bool, channel *string) error {
	root := fleetProjectRoot()
	refusal, err := updateAdmission(ctx, root)
	if err != nil {
		return err
	}
	if refusal != nil {
		fmt.Fprintln(w, refusal.Message)
		recordRefusalReceipt(ctx, refusal)
		return &pm.SystemExit{Code: 2}
	}
	if !existsFollow(filepath.Join(root, ".git")) {
		fmt.Fprintln(w, "✗ Not a git repository — cannot check for updates.")
		return &pm.SystemExit{Code: 1}
	}
	gitCmd := baseGitCmd()
	if err := mapSSLCertFileForGit(ctx, gitCmd); err != nil {
		return err
	}
	clearGitDebris(ctx, w, root)
	selectedChannel, err := sourceUpdateChannel(ctx, nil, channel, branchExplicit)
	if err != nil {
		return err
	}
	if !branchExplicit {
		compare, isBranch, err := channelCompareBranch(ctx, w, selectedChannel, gitCmd, root)
		if err != nil || !isBranch {
			return err
		}
		branch = compare
	}
	isShallow, err := isShallowRepository(ctx, gitCmd, root)
	if err != nil {
		return err
	}
	depthArgs := []string{}
	if isShallow {
		depthArgs = []string{"--depth", "1"}
	}
	fetchResult, compareBranch, err := fetchCompareBranch(ctx, w, gitCmd, root, branch, depthArgs)
	if err != nil {
		return err
	}
	if fetchResult.ReturnCode != 0 {
		if err := printFetchFailure(ctx, w, fetchResult.Stderr, gitCmd, root); err != nil {
			return err
		}
		return &pm.SystemExit{Code: 1}
	}
	if isShallow {
		repairShallowGrafts(ctx, w, root)
	}
	exists, err := compareRefExists(ctx, gitCmd, root, compareBranch)
	if err != nil {
		return err
	}
	if !exists {
		fmt.Fprintf(w, "✗ Branch '%s' not found on %s.\n", branch, strings.SplitN(compareBranch, "/", 2)[0])
		return &pm.SystemExit{Code: 1}
	}
	if isShallow {
		return reportShallowVerdict(ctx, w, gitCmd, root, compareBranch)
	}
	return reportRevListVerdict(ctx, w, gitCmd, root, compareBranch)
}

// The git argv, Windows adding the non atomic append override its loose object writes need
func baseGitCmd() []string {
	if sysPlatform() == "win32" {
		return []string{"git", "-c", "windows.appendAtomically=false"}
	}
	return []string{"git"}
}

// Whether git reports the update's checkout as shallow
func isShallowCheckout(ctx context.Context, gitCmd []string) (bool, error) {
	result, err := updateGitRun(ctx, gitCmd, []string{"rev-parse", "--is-shallow-repository"}, "", false, false)
	if err != nil {
		return false, err
	}
	return pystr.Strip(result.Stdout) == "true", nil
}

// The shas rev-parse prints for base and the target ref, empty when unresolvable
func tipShas(ctx context.Context, gitCmd []string, targetRef, base string) (string, string, error) {
	var shas [2]string
	for i, ref := range []string{base, targetRef} {
		result, err := updateGitRun(ctx, gitCmd, []string{"rev-parse", ref}, "", false, false)
		if err != nil {
			return "", "", err
		}
		shas[i] = pystr.Strip(result.Stdout)
	}
	return shas[0], shas[1], nil
}

// Reports the check's verdict, up to date, a counted distance, or behind by an unknown count
func printUpdateCheckResult(ctx context.Context, w io.Writer, behind *int, compareBranch string) error {
	if behind != nil && *behind == 0 {
		fmt.Fprintln(w, "✓ Already up to date.")
		return nil
	}
	if behind != nil {
		fmt.Fprintf(w, "☤ Update available: %d %s behind %s.\n", *behind, commitsWord(*behind), compareBranch)
	} else {
		fmt.Fprintf(w, "☤ Update available (behind %s).\n", compareBranch)
	}
	command, err := config.RecommendedUpdateCommand(ctx, home.Dir(ctx))
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "  Run '%s' to install.\n", command)
	return nil
}

// Freezes the data the completion process needs before the checkout moves
func sourceCompletionRequest(ctx context.Context, opts *updateOptions, plan *UpdatePlan, snapshotID *string, windowsResume map[string]any, desktop, gatewayMode bool) (map[string]any, error) {
	current := updaterecord.CurrentReceipt(ctx)
	if current == nil {
		updaterecord.BeginUpdateReceipt(ctx, nil, "")
		current = updaterecord.CurrentReceipt(ctx)
	}
	source, err := pypath.Resolve(fleetProjectRoot())
	if err != nil {
		return nil, err
	}
	request := map[string]any{
		"schema": 1, "source": source, "home": home.Dir(ctx), "branch": "main", "desktop": desktop,
		"assume_yes": opts.AssumeYes, "gateway_mode": gatewayMode, "no_gateway_restart": opts.NoGatewayRestart,
		"pre_update_version": optionalValue(opts.PreUpdateVersion), "snapshot_id": optionalValue(snapshotID),
		"sibling_snapshots": pyjson.Clone(lastSiblingSnapshots), "plan": nil,
		"receipt": pyjson.Clone(current.Data), "windows_resume": nil,
	}
	if plan != nil {
		request["plan"] = plan.ToDict()
	}
	if windowsResume != nil {
		request["windows_resume"] = windowsResume
	}
	return request, nil
}

// Marks this run's post commit resume attempt over so neither the boundary nor the exit hook replays it
func settleWindowsResume(request map[string]any) {
	unregisterUpdateExitHook(resumeWindowsGatewaysHook)
	request["windows_resume_settled"] = true
}

// Update id of the receipt frozen on a completion request, nil when it has none
func requestUpdateID(request map[string]any) *string {
	receipt, ok := pyjson.AsObject(request["receipt"])
	if !ok {
		return nil
	}
	id, ok := receipt.Vals["update_id"].(string)
	if !ok {
		return nil
	}
	return &id
}

// The command's last resume of the gateways it paused, a failure owed as a followup and never raised
func resumePausedGatewaysAtExit(ctx context.Context, w io.Writer, token, request map[string]any) error {
	if len(token) == 0 || !pyrepr.Truthy(token["resume_needed"]) || pyrepr.Truthy(request["windows_resume_settled"]) {
		return nil
	}
	err := resumeWindowsGatewaysAfterUpdate(ctx, w, token)
	if err == nil || isUpdateExit(err) {
		return err
	}
	var updateID *string
	if request != nil {
		updateID = requestUpdateID(request)
	}
	return updaterecord.OweFollowup(ctx, updateID, "windows_resume", "Windows gateway recovery failed: "+launchErrorText(err), updaterecord.FollowupRetryDefault)
}

// The completion process runner and the hand off every selection route ends in, both replaced by hermes tests
var (
	updateRunCompletion  = runCompletion
	updateCompleteSource = handOffSourceUpdate
)

// Exit status a completion result carries, a missing or non integer value failing like the lookup would
func completionExitCode(result map[string]any) (int64, error) {
	value, present := result["exit_code"]
	if !present {
		return 0, &KeyError{Key: "exit_code"}
	}
	switch n := value.(type) {
	case nil:
		return 0, nil
	case bool:
		if n {
			return 1, nil
		}
		return 0, nil
	case float64:
		return int64(n), nil
	}
	if n, ok := pyIntValue(value); ok {
		return n, nil
	}
	return 0, &pm.PyError{Class: "TypeError", Message: "the completion exit code is " + pyrepr.TypeName(value) + ", not an integer"}
}

// Hands a moved or current checkout to its completion, the one exit of every source selection route
func completeSourceUpdate(ctx context.Context, w io.Writer, request map[string]any) error {
	return updateCompleteSource(ctx, w, request)
}

// Runs the completion process for a moved or current checkout and settles its receipt, resume and channel
func handOffSourceUpdate(ctx context.Context, w io.Writer, request map[string]any) error {
	unrestored, _ := unrestoredAutostashNotice()
	if request == nil {
		if unrestored != "" {
			fmt.Fprintln(w, unrestored)
		}
		return pm.StopForRelaunchIncomplete(ctx)
	}
	if unrestored != "" {
		request["completion_message"] = unrestored
	}
	outcome := "success"
	if pyrepr.Truthy(request["completion_message"]) {
		outcome = "skipped"
	}
	mode, ok := request["apply_mode"]
	if !ok {
		mode = "git"
	}
	updaterecord.RecordStage(ctx, "apply", outcome, pyjson.ObjectOf("mode", mode))
	PublishStage("Updating dependencies")
	if current := updaterecord.CurrentReceipt(ctx); current != nil {
		request["receipt"] = pyjson.Clone(current.Data)
	}
	var expectedSHA string
	if pyrepr.Truthy(request["expected_sha"]) {
		expectedSHA = pyrepr.Str(request["expected_sha"])
	} else {
		expectedSHA = currentCheckoutSHA(ctx)
	}
	if _, err := writeFleetRestartPendingMarker(ctx, expectedSHA, nil, ""); err != nil {
		return err
	}
	lost := false
	result, err := updateRunCompletion(ctx, w, request)
	if err != nil {
		if errors.Is(err, cliui.ErrInterrupt) || errors.Is(err, context.Canceled) {
			updaterecord.FinalizeInterruptedUpdateReceipt(ctx, "KeyboardInterrupt: interrupted after the code was updated", 130)
			return &pm.SystemExit{Code: 130}
		}
		var exit *pm.SystemExit
		if errors.As(err, &exit) {
			return err
		}
		result, err = settleLostCompletion(ctx, w, request, launchErrorClass(err)+": "+launchErrorText(err))
		if err != nil {
			return err
		}
		lost = true
	}
	exitCode, err := completionExitCode(result)
	if err != nil {
		return err
	}
	if !lost && result["receipt"] == nil && exitCode == 130 {
		updaterecord.FinalizeInterruptedUpdateReceipt(ctx, "the completion was interrupted after the code was updated", 130)
	} else if !lost && result["receipt"] == nil && exitCode != 0 {
		reason := fmt.Sprintf("the completion process exited %d", exitCode)
		if pyrepr.Truthy(result["error"]) {
			reason = pyrepr.Str(result["error"])
		}
		result, err = settleLostCompletion(ctx, w, request, reason)
		if err != nil {
			return err
		}
		lost = true
		if exitCode, err = completionExitCode(result); err != nil {
			return err
		}
	}
	updateID := requestUpdateID(request)
	if updateID == nil {
		return &KeyError{Key: "update_id"}
	}
	if err := pm.AcceptWorkerReceipt(ctx, result["pm_receipt"], updateID); err != nil {
		return err
	}
	if result["receipt"] != nil {
		updaterecord.AdoptTerminalReceipt(ctx, result["receipt"])
		updaterecord.ResetCurrentReceipt(ctx)
	}
	token, hasToken := request["windows_resume"].(map[string]any)
	resumed, hasResumed := result["windows_resume"].(map[string]any)
	if hasToken && hasResumed {
		for key := range token {
			delete(token, key)
		}
		for key, value := range resumed {
			token[key] = value
		}
		settleWindowsResume(request)
	} else if len(token) > 0 && pyrepr.Truthy(token["resume_needed"]) && exitCode == 0 {
		if err := resumeWindowsGatewaysAfterUpdate(ctx, w, token); err != nil {
			if isUpdateExit(err) {
				return err
			}
			if err := updaterecord.OweFollowup(ctx, updateID, "windows_resume", "Windows gateway recovery failed: "+launchErrorText(err), updaterecord.FollowupRetryDefault); err != nil {
				return err
			}
		}
		settleWindowsResume(request)
	}
	if exitCode != 0 {
		return &pm.SystemExit{Code: int(exitCode)}
	}
	if lost {
		return nil
	}
	adopted, err := AdoptRetiredChannel(ctx, request)
	if err != nil {
		if isUpdateExit(err) {
			return err
		}
		reason := launchErrorText(err)
		if reason == "" {
			reason = launchErrorClass(err)
		}
		return updaterecord.OweFollowup(ctx, updateID, "channel_adoption", reason, "the next `icebath update` adopts it again")
	}
	if adopted {
		retirement, _ := request["channel_retirement"].(map[string]any)
		fmt.Fprintf(w, "→ Source subscription moved to %s\n", pyrepr.Str(retirement["destination"]))
	}
	return nil
}
