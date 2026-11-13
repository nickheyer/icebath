package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Resolved inputs of one update run, the flags, config and pre update version
type updateOptions struct {
	PreUpdateVersion    *string
	GwInputFn           func(prompt, def string) (string, error)
	AssumeYes           bool
	KeepStash           bool
	SwitchBranch        bool
	DiscardLocalChanges bool
	NoGatewayRestart    bool
}

// Snapshots the pre update version and resolves the flags and config the update runs on
func resolveUpdateOptions(ctx context.Context, w io.Writer, args map[string]any, gatewayMode bool) (*updateOptions, error) {
	opts := &updateOptions{
		PreUpdateVersion: checkoutVersion(ctx),
		AssumeYes:        pyrepr.Truthy(args["yes"]),
		KeepStash:        pyrepr.Truthy(args["keep_stash"]),
		SwitchBranch:     pyrepr.Truthy(args["switch_branch"]),
		NoGatewayRestart: pyrepr.Truthy(args["no_gateway_restart"]),
	}
	if gatewayMode {
		opts.GwInputFn = func(prompt, def string) (string, error) { return updateGatewayPrompt(ctx, w, prompt, def, 300.0) }
	}
	if gatewayMode || opts.AssumeYes || !(updateStdinIsTTY() && updateStdoutIsTTY(w)) {
		if err := bestEffort(ctx, "Could not read updates.non_interactive_local_changes: %s", func() error {
			section, err := updatesConfig(ctx)
			if err != nil {
				return err
			}
			mode, ok := section["non_interactive_local_changes"]
			if !ok {
				mode = "stash"
			}
			opts.DiscardLocalChanges = strings.ToLower(pyrepr.Str(mode)) == "discard"
			return nil
		}); err != nil {
			return nil, err
		}
	}
	return opts, nil
}

// Opens the receipt and snapshots the fleet before the checkout changes, nil when the inventory failed
func beginUpdateReceiptAndPlan(ctx context.Context, w io.Writer, args map[string]any) (*UpdatePlan, error) {
	if err := bestEffort(ctx, "Update receipt unavailable: %s", func() error {
		updaterecord.BeginUpdateReceipt(ctx, nil, "")
		return recordUpdateInitiator(ctx)
	}); err != nil {
		return nil, err
	}
	var plan *UpdatePlan
	if err := bestEffort(ctx, "Update plan phase failed: %s", func() error {
		plan = CollectRuntimeInventory(ctx)
		recordPlanInReceipt(ctx, plan)
		if len(plan.Runtimes) > 0 {
			distinct := map[string]bool{}
			for _, runtime := range plan.Runtimes {
				distinct[runtime.Profile] = true
			}
			profiles := make([]string, 0, len(distinct))
			for profile := range distinct {
				profiles = append(profiles, profile)
			}
			sort.Strings(profiles)
			fmt.Fprintf(w, "→ Fleet: %d running service(s) across profiles: %s\n", len(plan.Runtimes), strings.Join(profiles, ", "))
		}
		return nil
	}); err != nil {
		return nil, err
	}
	outcome := "success"
	if plan == nil {
		outcome = "failed"
	}
	updaterecord.RecordStage(ctx, "plan", outcome, nil)
	return plan, nil
}

// Records the desktop as initiator when the update marker names another pid than this lock holder
func recordUpdateInitiator(ctx context.Context) error {
	return bestEffort(ctx, "Update initiator unavailable: %s", func() error {
		raw, err := os.ReadFile(updaterecord.UpdateMarkerPath())
		if err != nil {
			return err
		}
		text, err := pystr.DecodeUTF8Sig(raw)
		if err != nil {
			return err
		}
		first, _, _ := strings.Cut(text, "\n")
		first = pystr.Strip(first)
		if first == "" {
			return nil
		}
		for _, r := range first {
			if !pystr.IsDigit(r) {
				return nil
			}
		}
		pid, err := strconv.Atoi(first)
		if err != nil {
			return &ValueError{Msg: "invalid literal for int() with base 10: " + pyrepr.Repr(first)}
		}
		if pid != os.Getpid() {
			updaterecord.RecordFact(ctx, "initiator", "desktop")
		}
		return nil
	})
}

// Resolves whether the update uses the ZIP path, the git argv and whether origin is a fork
func prepareGitCommand(ctx context.Context, w io.Writer, move checkoutMoveFunc) (bool, []string, bool, error) {
	root := fleetProjectRoot()
	gitDir := filepath.Join(root, ".git")
	useZipUpdate := !existsFollow(gitDir)
	if useZipUpdate && sysPlatform() != "win32" {
		fmt.Fprintln(w, "✗ Not a git repository. Please reinstall:")
		fmt.Fprintln(w, "  curl -fsSL https://hermes-agent.nousresearch.com/install.sh | bash")
		recordStop(ctx, "not_git_checkout")
		return false, nil, false, updateExit(1)
	}
	if err := ExposePMGit(ctx, root); err != nil {
		return false, nil, false, err
	}
	gitCmd := baseGitCmd()
	if sysPlatform() == "win32" && existsFollow(gitDir) {
		if _, err := updateGitRun(ctx, gitCmd, []string{"config", "windows.appendAtomically", "false"}, "", false, false); err != nil {
			return false, nil, false, err
		}
	}
	if err := mapSSLCertFileForGit(ctx, gitCmd); err != nil {
		return false, nil, false, err
	}
	gitCmd = ensureNonTrampolineGit(ctx, w, gitCmd)
	if err := discardLockfileChurn(ctx, w, gitCmd, root, move); err != nil {
		return false, nil, false, err
	}
	normalizeManagedEOL(ctx, w, gitCmd, root, move)
	originURL, _ := getOriginURL(ctx, gitCmd, root)
	fork := isFork(originURL)
	if fork {
		fmt.Fprintln(w, "⚠ Updating from fork:")
		fmt.Fprintf(w, "  %s\n", originURL)
		fmt.Fprintln(w)
	}
	return useZipUpdate, gitCmd, fork, nil
}

// Handles a failed git or installer subprocess, the ZIP download when safe, else the report and exit 1
func handleUpdateCalledProcessError(ctx context.Context, w io.Writer, e error, args map[string]any, gatewayMode, hadDesktopAppBeforeUpdate bool,
	targetSHA, targetRepository string, completionRequest map[string]any) error {
	if unrestored, _ := unrestoredAutostashNotice(); unrestored != "" {
		fmt.Fprintln(w, unrestored)
	}
	stage := formatUpdateFailureStage(e)
	fallback, err := shouldZipFallbackOnUpdateError(ctx, e)
	if err != nil {
		return err
	}
	if fallback {
		fmt.Fprintf(w, "⚠ %s: %s\n", stage, e.Error())
		fmt.Fprintln(w, "→ Falling back to ZIP download...")
		fmt.Fprintln(w)
		_, err := updateViaZip(ctx, w, args, hadDesktopAppBeforeUpdate, targetSHA, targetRepository, completionRequest)
		return err
	}
	if calledProcessErrorIsPythonDepInstall(e) {
		fmt.Fprintf(w, "✗ %s (the code update itself succeeded).\n", stage)
		printCalledProcessErrorTail(w, e, 12)
		fmt.Fprintln(w)
		fmt.Fprintln(w, "  Icebath may not start until the dependencies are installed. Fix the error above")
		fmt.Fprintln(w, "  (usually network or disk space), then run `icebath update` again.")
		if installRepairIsWindows() {
			fmt.Fprintln(w, "  If `icebath update` itself will not start, retry through the venv interpreter:")
			fmt.Fprintln(w, "    venv\\Scripts\\python.exe -c \"from icebath_cli.main import main; main()\" update --yes")
		}
	} else {
		fmt.Fprintf(w, "✗ %s.\n", stage)
		fmt.Fprintf(w, "  Details: %s\n", e.Error())
		printCalledProcessErrorTail(w, e, 12)
	}
	if stop := updaterecord.GitErrorStopClass(e); stop != nil && *stop != "" {
		recordStop(ctx, *stop)
	}
	if err := finalizeReceipt(ctx, "failed", "Update receipt finalize failed: %s"); err != nil {
		return err
	}
	return updateExit(1)
}

// Finalizes the open receipt with a status, never failing the update
func finalizeReceipt(ctx context.Context, status, debugMessage string) error {
	return bestEffort(ctx, debugMessage, func() (err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("%v", r)
			}
		}()
		updaterecord.FinalizeUpdateReceipt(ctx, status, nil, "")
		return nil
	})
}

// Settles an update with no new commits, restoring the stash and running the completion on the current checkout
func finishAlreadyUpToDate(ctx context.Context, w io.Writer, gitCmd []string, branch, currentBranch string, plan *checkoutPlan,
	gwInputFn func(prompt, def string) (string, error), completionRequest map[string]any, token map[string]any, heldMessage string) error {
	root := fleetProjectRoot()
	if plan.AutoStashRef != "" {
		if _, err := restoreStashedChanges(ctx, w, gitCmd, root, plan.AutoStashRef, plan.PromptForRestore, gwInputFn, movesFor(token)); err != nil {
			return err
		}
	}
	if plan.ParkedBranchSwitched {
		if strings.HasPrefix(plan.SwitchBlockReason, "unmerged:") {
			count := strings.SplitN(plan.SwitchBlockReason, ":", 2)[1]
			fmt.Fprintf(w, "  ✓ Checkout was parked on '%s' — switched back to %s; %s unmerged commit(s) kept on '%s'.\n", currentBranch, branch, count, currentBranch)
		} else {
			fmt.Fprintf(w, "  ✓ Checkout was parked on '%s' (fully merged) — switched back to %s.\n", currentBranch, branch)
		}
	}
	if completionRequest != nil {
		completionRequest["expected_sha"] = captureHeadSHA(ctx, gitCmd, root)
		message := heldMessage
		if message == "" {
			message = "✓ Up to date with your fork (official repo not checked)."
			if plan.UpstreamChecked {
				message = "✓ Already up to date!"
			}
		}
		completionRequest["completion_message"] = message
	}
	return completeSourceUpdate(ctx, w, completionRequest)
}

// Verifies HEAD after the pull, pins the completion to the observed commit and runs the completion
func applyPulledUpdate(ctx context.Context, w io.Writer, gitCmd []string, branch, movementBaseline string, plan *checkoutPlan, token, completionRequest map[string]any) error {
	postPullSHA, err := verifyHeadAfterPull(ctx, w, gitCmd, branch, movementBaseline, plan.InPlaceUpdate, token)
	if err != nil {
		return err
	}
	if completionRequest != nil {
		observed := captureHeadSHA(ctx, gitCmd, fleetProjectRoot())
		if observed == "" {
			observed = postPullSHA
		}
		pinned := completionRequest["expected_sha"]
		if pinned != nil && !pyrepr.Equal(pinned, observed) {
			fmt.Fprintln(w, "✗ Checkout no longer matches the selected channel commit. No completion was applied.")
			if err := resumeWindowsGatewaysAfterUpdate(ctx, w, token); err != nil {
				return err
			}
			recordStop(ctx, "head_moved")
			return updateExit(1)
		}
		if pyrepr.Truthy(pinned) {
			completionRequest["expected_sha"] = pinned
		} else {
			completionRequest["expected_sha"] = observed
		}
	}
	return completeSourceUpdate(ctx, w, completionRequest)
}

// Pauses Windows gateways now, or arms the pause the commit point performs elsewhere
func pauseGatewaysForUpdate(ctx context.Context, w io.Writer, opts *updateOptions) (map[string]any, error) {
	token, err := pauseWindowsGatewaysForUpdate(ctx, w)
	if err == nil && len(token) == 0 {
		token, err = armPause(ctx, w, opts.NoGatewayRestart)
	}
	if err != nil && runtimeErrorClasses[launchErrorClass(err)] {
		recordStop(ctx, "gateway_pause_failed")
	}
	return token, err
}

// Channel of this run, naming the exit when the install's configured channel is not a valid name
func updateRunChannel(ctx context.Context, args map[string]any) (string, error) {
	channel, err := sourceUpdateChannel(ctx, args, nil, false)
	if err != nil && (IsValueError(err) || pm.ErrorClass(err) == "ValueError") {
		recordStop(ctx, "channel_unresolved")
	}
	return channel, err
}

// Target branch of the update, main when the flag is absent or blank
func resolveUpdateBranch(args map[string]any) string {
	branch := "main"
	if pyrepr.Truthy(args["branch"]) {
		branch = pyrepr.Str(args["branch"])
	}
	if branch = pystr.Strip(branch); branch == "" {
		return "main"
	}
	return branch
}

// The probe of a user owned git operation in the checkout, replaced by hermes tests
var updateGitOperation = GitOperationInProgress

// Applies the update, the command boundary owning errors, receipts and output streams
func cmdUpdateImpl(ctx context.Context, w io.Writer, args map[string]any, gatewayMode bool) error {
	beginUpdateAttempt()
	PreferDotenvProxyEnv(nil)
	root := fleetProjectRoot()
	gitOperation, err := updateGitOperation(root)
	if err != nil {
		return err
	}
	if gitOperation != "" {
		fmt.Fprintf(w, "✗ Cannot update while a Git %s is in progress in %s.\n", gitOperation, root)
		fmt.Fprintf(w, "  Finish it or run `git %s --abort`, then re-run `icebath update`.\n", gitOperation)
		recordStopWithoutReceipt(ctx, "git_in_progress", "failed")
		return updateExit(1)
	}
	released, err := tools.ReleaseDeadIndexLock(ctx, root)
	if err != nil {
		return err
	}
	if released {
		fmt.Fprintln(w, "  (removed .git/index.lock left by a git that was killed)")
	}
	locks, err := tools.ReleaseDeadFetchLocks(ctx, root)
	if err != nil {
		return err
	}
	for _, lock := range locks {
		fmt.Fprintf(w, "  (removed %s left by a git that was killed)\n", lock)
	}
	clearGitDebris(ctx, w, root)
	opts, err := resolveUpdateOptions(ctx, w, args, gatewayMode)
	if err != nil {
		return err
	}
	fmt.Fprintln(w, "☤ Updating Icebath Agent...")
	fmt.Fprintln(w)
	preUpdatePlan, err := beginUpdateReceiptAndPlan(ctx, w, args)
	if err != nil {
		return err
	}
	preUpdateSnapshotID := runPreUpdateBackup(ctx, w, args)
	recordPreUpdateBackupOutcome(ctx, args, preUpdateSnapshotID)
	recordSnapshotStage(ctx, args, preUpdateSnapshotID)
	PublishStage("Downloading updates")
	token, err := pauseGatewaysForUpdate(ctx, w, opts)
	if err != nil {
		return err
	}
	resume := func() error { return resumeWindowsGatewaysAfterUpdate(ctx, w, token) }
	if len(token) > 0 {
		registerUpdateExitHook(resumeWindowsGatewaysHook, resume)
	}
	hadDesktopAppBeforeUpdate := desktopAppPresent(root)
	releaseBinary, err := releaseBinaryInstall(ctx, root)
	if err != nil {
		return err
	}
	if releaseBinary {
		return applyReleaseUpdate(ctx, w, args, opts, preUpdatePlan, preUpdateSnapshotID, token, hadDesktopAppBeforeUpdate, gatewayMode, resume)
	}
	useZipUpdate, gitCmd, fork, err := prepareGitCommand(ctx, w, movesFor(token))
	if err != nil {
		return err
	}
	completionRequest, err := sourceCompletionRequest(ctx, opts, preUpdatePlan, preUpdateSnapshotID, token, hadDesktopAppBeforeUpdate, gatewayMode)
	if err != nil {
		return err
	}
	branch := resolveUpdateBranch(args)
	completionRequest["branch"] = branch
	selectGit := gitCmd
	if useZipUpdate {
		selectGit = nil
	}
	target, err := selectApplyTarget(ctx, w, args, branch, completionRequest, selectGit, resume)
	if err != nil {
		return err
	}
	branch = pyrepr.Str(completionRequest["branch"])
	if useZipUpdate {
		_, zipErr := updateViaZip(ctx, w, args, hadDesktopAppBeforeUpdate, target.ReleaseSHA, target.Repository, completionRequest)
		if err := resumePausedGatewaysAtExit(ctx, w, token, completionRequest); err != nil {
			return err
		}
		return zipErr
	}
	err = applyGitUpdate(ctx, w, args, gatewayMode, opts, gitCmd, fork, branch, target, token, completionRequest)
	if _, called := asCalledProcessError(err); !called {
		return err
	}
	handled := handleUpdateCalledProcessError(ctx, w, err, args, gatewayMode, hadDesktopAppBeforeUpdate, target.ReleaseSHA, target.Repository, completionRequest)
	if err := resumePausedGatewaysAtExit(ctx, w, token, completionRequest); err != nil {
		return err
	}
	return handled
}

// The release path of a binary install, channel selection, download and swap, then the completion
func applyReleaseUpdate(ctx context.Context, w io.Writer, args map[string]any, opts *updateOptions, preUpdatePlan *UpdatePlan, preUpdateSnapshotID *string,
	token map[string]any, hadDesktopAppBeforeUpdate, gatewayMode bool, resume func() error) error {
	completionRequest, err := sourceCompletionRequest(ctx, opts, preUpdatePlan, preUpdateSnapshotID, token, hadDesktopAppBeforeUpdate, gatewayMode)
	if err != nil {
		return err
	}
	branch := resolveUpdateBranch(args)
	completionRequest["branch"] = branch
	target, err := selectApplyTarget(ctx, w, args, branch, completionRequest, nil, resume)
	if err != nil {
		return err
	}
	applyErr := updateViaRelease(ctx, w, target, completionRequest)
	if err := resumePausedGatewaysAtExit(ctx, w, token, completionRequest); err != nil {
		return err
	}
	return applyErr
}

// The git path of the update, fetch, preflight, checkout phase, pull and completion
func applyGitUpdate(ctx context.Context, w io.Writer, args map[string]any, gatewayMode bool, opts *updateOptions, gitCmd []string, fork bool, branch string,
	target applyTarget, token, completionRequest map[string]any) error {
	root := fleetProjectRoot()
	targetRef, releaseSHA := target.Ref, target.ReleaseSHA
	resume := func() error { return resumeWindowsGatewaysAfterUpdate(ctx, w, token) }
	reportPackTidy(ctx, w, root)
	repairShallowGrafts(ctx, w, root)
	warnOrphanedUpdateAutostashes(ctx, w, gitCmd, root)
	if err := healStaleShallowCheckout(ctx, w, root, branch); err != nil {
		return err
	}
	var fetchArgs []string
	switch {
	case releaseSHA != "" && target.IsHead:
	case releaseSHA != "":
		fetchArgs = []string{"fetch", "--no-tags", "origin", targetRef}
	default:
		fetchArgs = []string{"fetch", "origin", trackingRefspec("origin", branch)}
	}
	fetchResult := &CompletedProcess{Args: []string{}}
	if fetchArgs != nil {
		fmt.Fprintln(w, "→ Fetching updates...")
		runner := func(gc, a []string) (*pysubprocess.Completed, error) {
			done, err := updateGitRun(ctx, gc, a, "", false, true)
			return plainCompleted(done), err
		}
		fetched, err := tools.FetchWithPartialCloneRecovery(runner, gitCmd, fetchArgs, root)
		if err != nil {
			return err
		}
		fetchResult = custodyCompleted(fetched)
	}
	if fetchResult.ReturnCode != 0 {
		if tools.IsPartialClonePackObjectsCrash(fetchResult.Stderr) {
			fmt.Fprintln(w, "✗ git still crashed after marking this checkout's packs. See 'Fetch fails with"+
				" should_include_obj' in https://hermes-agent.nousresearch.com/docs/getting-started/updating")
		}
		if err := printFetchFailure(ctx, w, fetchResult.Stderr, gitCmd, root); err != nil {
			return err
		}
		if err := resume(); err != nil {
			return err
		}
		switch {
		case fetchResult.ReturnCode == 124:
			recordStop(ctx, "git_timeout")
		case strings.Contains(fetchResult.Stderr, "No space left on device"):
			recordStop(ctx, "disk_full")
		default:
			recordStop(ctx, "fetch_failed")
		}
		return updateExit(1)
	}
	if err := fetchReleaseTag(ctx, w, gitCmd, root, completionRequest); err != nil {
		return err
	}
	currentBranch, err := currentBranchName(ctx, gitCmd, true)
	if err != nil {
		return err
	}
	if err := recordRunStart(ctx, gitCmd, root); err != nil {
		return err
	}
	heldMessage := ""
	if releaseSHA != "" && !target.IsHead {
		channel, err := updateRunChannel(ctx, args)
		if err != nil {
			return err
		}
		if held := holdChannelReleaseBehindRuntime(ctx, w, gitCmd, targetRef, channel); held != "" {
			targetRef = held
			heldMessage = fmt.Sprintf("✓ Holding at this build until a newer %s release ships.", channel)
		}
	}
	if err := refuseBeforeCommitPoint(ctx, w, gitCmd, targetRef, token); err != nil {
		return err
	}
	plan, err := prepareCheckoutForUpdate(ctx, w, gitCmd, branch, currentBranch, prepareOptions{IsFork: fork, AssumeYes: opts.AssumeYes,
		GatewayMode: gatewayMode, GwInputFn: opts.GwInputFn, SwitchBranch: opts.SwitchBranch, TargetRef: targetRef, WindowsGatewayResume: token})
	if err != nil {
		return err
	}
	commitCount := plan.CommitCount
	if commitCount == 0 {
		return finishAlreadyUpToDate(ctx, w, gitCmd, branch, currentBranch, plan, opts.GwInputFn, completionRequest, token, heldMessage)
	}
	switch {
	case releaseSHA != "":
		fmt.Fprintf(w, "→ Switching to source commit %s\n", short(releaseSHA))
	case commitCount > 0:
		fmt.Fprintf(w, "→ Found %d new commit(s)\n", commitCount)
	case plan.SwitchedWithoutNewCommits:
		fmt.Fprintf(w, "→ Switched the running checkout to %s (no new commits to count)\n", branch)
	default:
		fmt.Fprintln(w, "→ Updates available (commit count unknown on this shallow checkout)")
	}
	fmt.Fprintln(w, "→ Pulling updates...")
	expectedSHA := ""
	if pinned := completionRequest["expected_sha"]; pinned != nil {
		expectedSHA = pyrepr.Str(pinned)
	}
	movementBaseline, err := pullUpdates(ctx, w, gitCmd, branch, plan.AutoStashRef, pullOptions{PromptForRestore: plan.PromptForRestore,
		GwInputFn: opts.GwInputFn, DiscardLocalChanges: opts.DiscardLocalChanges, KeepStash: opts.KeepStash, TargetRef: targetRef,
		PreSyncSHA: plan.PreSyncSHA, RollbackBranch: plan.RollbackBranch, SyncUpstream: fork && branch == "main" && releaseSHA == "",
		AssumeYes: opts.AssumeYes, InPlaceUpdate: plan.InPlaceUpdate, WindowsGatewayResume: token, ExpectedSHA: expectedSHA})
	if err != nil {
		return err
	}
	return applyPulledUpdate(ctx, w, gitCmd, branch, movementBaseline, plan, token, completionRequest)
}

// Whether a failure is a custody refusal or OS error the command boundary reports as a failed update
func isUpdateBoundaryError(err error) bool {
	var install *pm.InstallError
	return errors.As(err, &install) || isOSOrSubprocessError(err)
}
