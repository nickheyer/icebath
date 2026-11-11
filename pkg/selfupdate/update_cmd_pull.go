package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/internal/pyshlex"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

// Wall clock of the rescue tag and rescue ref names
var updateNow = time.Now

// Exit status 1 of a refused or failed update step
func updateExit(code int) error { return &pm.SystemExit{Code: code} }

// Whether a failure is the operator's interrupt, which leaves a torn tree's marker for the next launch
func isKeyboardInterrupt(err error) bool {
	return errors.Is(err, cliui.ErrInterrupt) || errors.Is(err, context.Canceled)
}

// Reconciles a proven diverged checkout, a merge on a custom branch, else a rescue ref and a hard reset
func reconcileDivergedCheckout(ctx context.Context, w io.Writer, gitCmd []string, branch, prePullSHA, targetRef string) error {
	root := fleetProjectRoot()
	mergeRef := targetRef
	if mergeRef == "" {
		mergeRef = "origin/" + branch
	}
	shown, err := updateGitRun(ctx, gitCmd, []string{"branch", "--show-current"}, "", false, false)
	if err != nil {
		return err
	}
	if curBranch := pystr.Strip(shown.Stdout); curBranch != "" && curBranch != branch {
		fmt.Fprintf(w, "  ⚠ Checkout is on custom branch '%s' — merging origin/%s instead of resetting so local commits survive...\n", curBranch, branch)
		if _, err := updateGitRun(ctx, gitCmd, []string{"tag", "pre-update-" + updateNow().Format("20060102-150405")}, "", false, false); err != nil {
			return err
		}
		merged, err := updateGitRun(ctx, gitCmd, []string{"merge", "--no-edit", mergeRef}, "", false, false)
		if err != nil {
			return err
		}
		if merged.ReturnCode != 0 {
			if _, err := updateGitRun(ctx, gitCmd, []string{"merge", "--abort"}, "", false, false); err != nil {
				return err
			}
			fmt.Fprintln(w, "✗ Merge conflict between local commits and upstream — update stopped, nothing was changed.")
			fmt.Fprintf(w, "  Resolve manually: cd %s && git merge origin/%s\n", root, branch)
			fmt.Fprintln(w, "  Then re-run the update. Local work is untouched.")
			recordStop(ctx, "merge_conflict")
			return updateExit(1)
		}
		return nil
	}
	mergeBase, err := updateGitRun(ctx, gitCmd, []string{"merge-base", "HEAD", mergeRef}, "", false, false)
	if err != nil {
		return err
	}
	hasCommonAncestor := mergeBase.ReturnCode == 0 && pystr.Strip(mergeBase.Stdout) != ""
	if prePullSHA != "" {
		kind := "orphan"
		head := fmt.Sprintf("  ⚠ Local history shares no common ancestor with origin/%s (orphan divergence) — ", branch)
		if hasCommonAncestor {
			kind = "diverged"
			head = fmt.Sprintf("  ⚠ Local history has diverged from origin/%s — ", branch)
		}
		rescueRef := fmt.Sprintf("refs/icebath-update-backups/%s-%s-%s-%s", kind, branch, updateNow().UTC().Format("20060102-150405"), clip(prePullSHA, 12))
		backup, err := updateGitRun(ctx, gitCmd, []string{"update-ref", rescueRef, prePullSHA}, "", false, false)
		if err != nil {
			return err
		}
		if backup.ReturnCode == 0 {
			fmt.Fprintf(w, "%sbacked up current HEAD to %s before resetting. This backup expires after %d days.\n", head, rescueRef, orphanRescueRefMaxAgeDays)
			if hasCommonAncestor {
				counted, err := updateGitRun(ctx, gitCmd, []string{"rev-list", "--count", fmt.Sprintf("origin/%s..%s", branch, prePullSHA)}, "", false, false)
				if err != nil {
					return err
				}
				dropped := pystr.Strip(counted.Stdout)
				if dropped == "" {
					dropped = "Some"
				}
				fmt.Fprintf(w, "    %s commit(s) not on origin/%s leave the branch; list them with: git log origin/%s..%s\n", dropped, branch, branch, rescueRef)
			}
		} else {
			fmt.Fprintf(w, "%sattempted to back up current HEAD to %s before resetting, but the backup write failed (pre-reset SHA was %s).\n", head, rescueRef, prePullSHA)
		}
		if err := pruneOrphanRescueRefs(ctx, gitCmd, root, branch, orphanRescueRefsToKeep, orphanRescueRefMaxAgeDays); err != nil {
			return err
		}
	}
	fmt.Fprintln(w, "  ⚠ Fast-forward not possible (history diverged), resetting to match remote...")
	reset, err := updateGitRun(ctx, gitCmd, []string{"reset", "--hard", mergeRef}, "", false, false)
	if err != nil {
		return err
	}
	if reset.ReturnCode != 0 {
		fmt.Fprintf(w, "✗ Failed to reset to origin/%s.\n", branch)
		if detail := pystr.Strip(reset.Stderr); detail != "" {
			fmt.Fprintf(w, "  %s\n", detail)
		}
		fmt.Fprintf(w, "  Try manually: git fetch origin && git reset --hard origin/%s\n", branch)
		recordStop(ctx, "checkout_move_failed")
		return updateExit(1)
	}
	return nil
}

// Rolls the checkout back and exits 1 when a startup file no longer compiles after the pull
func rollbackIfPulledSyntaxError(ctx context.Context, w io.Writer, gitCmd []string, prePullSHA string, rollbackBranch *string, token map[string]any) error {
	root := fleetProjectRoot()
	pyproject := filepath.Join(root, "pyproject.toml")
	other := false
	if info, err := os.Stat(pyproject); err == nil && info.Mode().IsRegular() {
		data, err := os.ReadFile(pyproject)
		if err != nil {
			return err
		}
		if other, err = requiresOtherPython(data); err != nil {
			return err
		}
	}
	var syntaxOK bool
	var failingPath, syntaxError string
	if other {
		broken, err := treeSyntaxError(ctx, root, updateCriticalFiles)
		if err != nil {
			return err
		}
		if syntaxOK = broken == nil; !syntaxOK {
			failingPath, syntaxError = broken.Path, broken.Error
		}
	} else {
		syntaxOK, failingPath, syntaxError = validateCriticalFilesSyntax(root)
	}
	if syntaxOK {
		return nil
	}
	return rollBackBrokenUpdate(ctx, w, gitCmd, prePullSHA, failingPath, syntaxError, rollbackOptions{RollbackBranch: rollbackBranch, Token: token})
}

// Keywords of a broken update rollback, the branch to return to, the header line and the stop token
type rollbackOptions struct {
	RollbackBranch *string
	Header         string
	Token          map[string]any
	StopClass      string
}

// Restores the checkout to the pre pull commit after a startup file failed the syntax check, then exits 1
func rollBackBrokenUpdate(ctx context.Context, w io.Writer, gitCmd []string, prePullSHA, failingPath, syntaxError string, o rollbackOptions) error {
	if o.Header == "" {
		o.Header = "✗ Pulled code has a syntax error in a critical file:"
	}
	if o.StopClass == "" {
		o.StopClass = "syntax_rollback"
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, o.Header)
	fmt.Fprintf(w, "  %s\n", failingPath)
	if syntaxError != "" {
		lines := pystr.Splitlines(syntaxError, false)
		if len(lines) > 6 {
			lines = lines[:6]
		}
		for _, line := range lines {
			fmt.Fprintf(w, "    %s\n", line)
		}
	}
	fmt.Fprintln(w)
	root := fleetProjectRoot()
	if prePullSHA == "" {
		fmt.Fprintln(w, "  Could not capture pre-pull SHA — recover manually with:")
		fmt.Fprintf(w, "    cd %s && git reflog && git reset --hard <prev-sha>\n", root)
		recordStop(ctx, o.StopClass)
		return updateExit(1)
	}
	parked := o.RollbackBranch != nil && *o.RollbackBranch != "HEAD"
	if parked {
		fmt.Fprintf(w, "→ Rolling back to %s...\n", *o.RollbackBranch)
	} else {
		fmt.Fprintf(w, "→ Rolling back to %s...\n", short(prePullSHA))
	}
	mode := "detach"
	if o.RollbackBranch == nil {
		mode = "branch"
	}
	targetSHA := captureHeadSHA(ctx, gitCmd, root)
	if _, err := armTreeMove(ctx, gitCmd, root, treeMove{Pre: prePullSHA, Target: targetSHA, Rollback: mode}); err != nil {
		if !isOSOrSubprocessError(err) {
			return err
		}
		fmt.Fprintf(w, "  ⚠ Could not write the interrupted-pull marker (%s); rolling back without it.\n", launchErrorText(err))
	}
	var rollbackArgs []string
	var rollbackResult *CompletedProcess
	run := func(args ...string) error {
		var err error
		rollbackArgs = args
		rollbackResult, err = updateGitRun(ctx, gitCmd, args, "", false, false)
		return err
	}
	fail := func(stderr string) {
		rollbackResult = &CompletedProcess{Args: rollbackArgs, ReturnCode: 1, Stderr: stderr}
	}
	err := withCheckoutMove(ctx, movesFor(o.Token), checkoutMoveBind{Targets: []string{prePullSHA}}, func(map[string]any) error {
		added, err := filesAddedBy(ctx, gitCmd, root, prePullSHA, targetSHA)
		if err != nil {
			return err
		}
		if mode == "detach" {
			if err := run("update-ref", "--no-deref", "HEAD", prePullSHA); err != nil {
				return err
			}
		}
		if mode == "branch" || rollbackResult.ReturnCode == 0 {
			if err := run("reset", "-q", prePullSHA); err != nil {
				return err
			}
		}
		if rollbackResult.ReturnCode == 0 {
			if err := run("reset", "--hard", prePullSHA); err != nil {
				return err
			}
			if rollbackResult.ReturnCode == 0 && !dropAddedFiles(root, added) {
				fail("could not remove the broken release's added files")
			}
		}
		if rollbackResult.ReturnCode == 0 {
			whole, err := treeWholeAt(ctx, gitCmd, root, prePullSHA)
			if err != nil {
				return err
			}
			if !whole {
				fail(fmt.Sprintf("the checkout is not whole at %s", short(prePullSHA)))
			}
		}
		if rollbackResult.ReturnCode != 0 {
			settled, err := settleFailedTreeMove(ctx, root)
			if err != nil {
				return err
			}
			if settled {
				rollbackResult = &CompletedProcess{Args: rollbackArgs}
			}
		}
		if rollbackResult.ReturnCode == 0 && parked {
			checkedOut, err := updateGitRun(ctx, gitCmd, []string{"checkout", *o.RollbackBranch}, "", false, false)
			if err != nil {
				return err
			}
			if checkedOut.ReturnCode != 0 {
				fmt.Fprintf(w, "  ✗ Could not check out %s; restoring its commit detached.\n", *o.RollbackBranch)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	marker, err := pm.InterruptedPullMarkerPath(root)
	if err != nil {
		return err
	}
	if rollbackResult.ReturnCode == 0 {
		if err := unlinkMissingOK(marker); err != nil {
			return err
		}
		if err := disarmCommitObligations(ctx); err != nil {
			return err
		}
		fmt.Fprintln(w, "  ✓ Rollback complete — your install is unchanged.")
		fmt.Fprintln(w, "  Try ``icebath update`` again later once a fix lands.")
	} else {
		fmt.Fprintln(w, "  ✗ Rollback failed. Recover manually with:")
		fmt.Fprintf(w, "    cd %s && git %s\n", root, pyshlex.Join(rollbackArgs))
		if detail := pystr.Strip(rollbackResult.Stderr); detail != "" {
			fmt.Fprintf(w, "    (%s)\n", pystr.Splitlines(detail, false)[0])
		}
		if info, err := os.Stat(marker); err == nil && info.Mode().IsRegular() {
			if err := oweRestoreOf(ctx, root, prePullSHA); err != nil {
				return err
			}
			fmt.Fprintf(w, "  The next `icebath` launch also finishes the rollback to %s on its own.\n", short(prePullSHA))
		}
	}
	recordStop(ctx, o.StopClass)
	return updateExit(1)
}

// The HEAD a git move landed on when it neither is nor contains the target, false when on target
func landedOffTarget(ctx context.Context, gitCmd []string, targetSHA string, merged bool) (string, bool, error) {
	head, err := updateGitRun(ctx, gitCmd, []string{"rev-parse", "-q", "--verify", "HEAD"}, "", false, false)
	if err != nil {
		return "", false, err
	}
	landed := pystr.Strip(head.Stdout)
	if landed == targetSHA {
		return "", false, nil
	}
	if merged && landed != "" {
		contains, err := updateGitRun(ctx, gitCmd, []string{"merge-base", "--is-ancestor", targetSHA, landed}, "", false, false)
		if err != nil {
			return "", false, err
		}
		if contains.ReturnCode == 0 {
			return "", false, nil
		}
	}
	if landed == "" {
		landed = "an unreadable HEAD"
	}
	return landed, true, nil
}

// Exits 1 when git landed on a revision other than the armed one, an unmoved HEAD left to the caller
func refuseUnselectedHead(ctx context.Context, w io.Writer, gitCmd []string, targetSHA, unmoved string) error {
	landed, off, err := landedOffTarget(ctx, gitCmd, targetSHA, true)
	if err != nil || !off || (unmoved != "" && landed == unmoved) {
		return err
	}
	fmt.Fprintf(w, "✗ The checkout landed on %s, not the selected commit %s (a ref moved during the update).\n", short(landed), short(targetSHA))
	recordStop(ctx, "head_moved")
	return updateExit(1)
}

// Header line of a rollback caused by an upstream target that fails the syntax check
func upstreamBrokenHeader(broken *UpstreamTargetBroken) string {
	return fmt.Sprintf("✗ upstream/main (%s) has a syntax error in a critical file; it was not merged:", short(broken.SHA))
}

// The sha HEAD must move off, a parked branch awaiting repair told apart from an applied early sync
func updateMovementBaseline(ctx context.Context, gitCmd []string, prePullSHA, preSyncSHA string, rollbackBranch *string, targetSHA string) (string, error) {
	if rollbackBranch != nil && prePullSHA != "" && targetSHA != "" {
		containsTarget, err := updateGitRun(ctx, gitCmd, []string{"merge-base", "--is-ancestor", targetSHA, prePullSHA}, "", false, false)
		if err != nil {
			return "", err
		}
		if containsTarget.ReturnCode != 0 {
			return prePullSHA, nil
		}
	}
	if preSyncSHA != "" {
		return preSyncSHA, nil
	}
	return prePullSHA, nil
}

// The commit a ref names now, empty when it names none
func resolvedCommit(ctx context.Context, gitCmd []string, ref string) (string, error) {
	result, err := updateGitRun(ctx, gitCmd, []string{"rev-parse", "-q", "--verify", ref + "^{commit}"}, "", false, false)
	if err != nil {
		return "", err
	}
	return pystr.Strip(result.Stdout), nil
}

// The principal pull's one checkout move onto the target commit, exiting when it cannot be made
func moveCheckoutTo(ctx context.Context, w io.Writer, gitCmd []string, branch, mergeRef, targetSHA, prePullSHA string) error {
	if mergeRef != "origin/"+branch {
		if err := parkDetachedHead(ctx, w, gitCmd, fleetProjectRoot(), branch); err != nil {
			return err
		}
		_, err := updateGitRun(ctx, gitCmd, []string{"checkout", "--detach", targetSHA}, "", true, false)
		return err
	}
	mergeResult, err := updateGitRun(ctx, gitCmd, []string{"merge", "--ff-only", targetSHA}, "", false, false)
	if err != nil || mergeResult.ReturnCode == 0 {
		return err
	}
	ancestry, err := updateGitRun(ctx, gitCmd, []string{"merge-base", "--is-ancestor", "HEAD", targetSHA}, "", false, false)
	if err != nil {
		return err
	}
	if ancestry.ReturnCode == 1 {
		return reconcileDivergedCheckout(ctx, w, gitCmd, branch, prePullSHA, targetSHA)
	}
	fmt.Fprintln(w, "✗ Fast-forward failed; refusing to reset because history divergence was not proven.")
	detail := pystr.Strip(mergeResult.Stderr)
	if mergeResult.Stderr == "" {
		detail = pystr.Strip(mergeResult.Stdout)
	}
	if detail != "" {
		fmt.Fprintf(w, "  %s\n", detail)
	}
	if ancestry.ReturnCode == 0 {
		fmt.Fprintf(w, "  HEAD is still an ancestor of %s.\n", mergeRef)
	} else {
		fmt.Fprintf(w, "  Could not verify whether HEAD is an ancestor of %s.\n", mergeRef)
		ancestryDetail := pystr.Strip(ancestry.Stderr)
		if ancestry.Stderr == "" {
			ancestryDetail = pystr.Strip(ancestry.Stdout)
		}
		if ancestryDetail != "" {
			fmt.Fprintf(w, "  %s\n", ancestryDetail)
		}
	}
	fmt.Fprintln(w, "  Resolve the Git error and re-run `icebath update`; no reset was attempted.")
	stop := "checkout_move_failed"
	if class := updaterecord.GitOutputStopClass(detail, []string{"merge"}, &mergeResult.ReturnCode); class != nil && *class != "" {
		stop = *class
	}
	recordStop(ctx, stop)
	return updateExit(1)
}

// Keywords of the pull phase, how the autostash settles and what the move is checked against
type pullOptions struct {
	PromptForRestore     bool
	GwInputFn            func(prompt, def string) (string, error)
	DiscardLocalChanges  bool
	KeepStash            bool
	TargetRef            string
	PreSyncSHA           string
	RollbackBranch       *string
	SyncUpstream         bool
	AssumeYes            bool
	InPlaceUpdate        bool
	WindowsGatewayResume map[string]any
	ExpectedSHA          string
}

// Fast forwards onto the target, rolls back a broken pull and settles the autostash, returning the sha HEAD moved off
func pullUpdates(ctx context.Context, w io.Writer, gitCmd []string, branch, autoStashRef string, o pullOptions) (string, error) {
	root := fleetProjectRoot()
	token := o.WindowsGatewayResume
	prePullSHA := captureHeadSHA(ctx, gitCmd, root)
	pullMarker, err := pm.InterruptedPullMarkerPath(root)
	if err != nil {
		return "", err
	}
	mergeRef := o.TargetRef
	if mergeRef == "" {
		mergeRef = "origin/" + branch
	}
	movementBaseline, err := func() (string, error) {
		targetSHA, err := resolvedCommit(ctx, gitCmd, mergeRef)
		if err != nil {
			return "", err
		}
		if !IsObjectID(targetSHA) {
			fmt.Fprintf(w, "✗ Could not resolve %s to a commit. No update was applied.\n", mergeRef)
			recordStop(ctx, "target_unresolved")
			return "", updateExit(1)
		}
		baseline, err := updateMovementBaseline(ctx, gitCmd, prePullSHA, o.PreSyncSHA, o.RollbackBranch, targetSHA)
		if err != nil {
			return "", err
		}
		if o.ExpectedSHA != "" && targetSHA != o.ExpectedSHA {
			fmt.Fprintf(w, "✗ %s resolves to %s, not the selected channel commit %s. No update was applied.\n", mergeRef, short(targetSHA), short(o.ExpectedSHA))
			recordStop(ctx, "head_moved")
			return "", updateExit(1)
		}
		refused, err := armCommitPoint(ctx, gitCmd, root, targetSHA, treeMove{Pre: prePullSHA, Target: targetSHA, Stash: autoStashRef})
		if err != nil {
			return "", err
		}
		if refused != "" {
			fmt.Fprintf(w, "✗ %s.\n", refused)
			recordStop(ctx, "commit_point_refused")
			return "", updateExit(1)
		}
		moveErr := withCheckoutMove(ctx, movesFor(token), checkoutMoveBind{Targets: []string{targetSHA}}, func(map[string]any) error {
			return moveCheckoutTo(ctx, w, gitCmd, branch, mergeRef, targetSHA, prePullSHA)
		})
		if moveErr == nil {
			moveErr = refuseUnselectedHead(ctx, w, gitCmd, targetSHA, prePullSHA)
		}
		if moveErr != nil {
			if isKeyboardInterrupt(moveErr) {
				return "", moveErr
			}
			settled, err := settleFailedTreeMove(ctx, root)
			if err != nil {
				return "", err
			}
			if settled {
				err = disarmCommitObligations(ctx)
			} else {
				err = oweRestoreOf(ctx, root, prePullSHA)
			}
			if err != nil {
				return "", err
			}
			return "", moveErr
		}
		if err := unlinkMissingOK(pullMarker); err != nil {
			return "", err
		}
		rollbackTo := o.PreSyncSHA
		if rollbackTo == "" {
			rollbackTo = prePullSHA
		}
		if o.SyncUpstream {
			if _, err := verifyHeadAfterPull(ctx, w, gitCmd, branch, baseline, o.InPlaceUpdate, token); err != nil {
				return "", err
			}
			_, err := syncWithUpstreamIfNeeded(ctx, w, gitCmd, root, o.AssumeYes, o.GwInputFn, movesFor(token))
			var broken *UpstreamTargetBroken
			if errors.As(err, &broken) {
				if _, err := verifyHeadAfterPull(ctx, w, gitCmd, branch, baseline, o.InPlaceUpdate, token); err != nil {
					return "", err
				}
				return "", rollBackBrokenUpdate(ctx, w, gitCmd, rollbackTo, broken.Path, broken.Detail,
					rollbackOptions{RollbackBranch: o.RollbackBranch, Header: upstreamBrokenHeader(broken), Token: token})
			}
			if err != nil {
				return "", err
			}
		}
		if _, err := verifyHeadAfterPull(ctx, w, gitCmd, branch, baseline, o.InPlaceUpdate, token); err != nil {
			return "", err
		}
		if err := rollbackIfPulledSyntaxError(ctx, w, gitCmd, rollbackTo, o.RollbackBranch, token); err != nil {
			return "", err
		}
		if o.SyncUpstream {
			pushSyncedFork(ctx, w, gitCmd, root)
		}
		return baseline, nil
	}()
	if autoStashRef != "" {
		var settleErr error
		switch {
		case err != nil:
			fmt.Fprintf(w, "  ℹ️  Local changes preserved in stash (ref: %s)\n", autoStashRef)
			fmt.Fprintln(w, "  Restore manually with: git stash apply")
			clearPendingAutostash()
		case o.DiscardLocalChanges:
			_, settleErr = discardStashedChanges(ctx, w, gitCmd, root, autoStashRef)
		case o.KeepStash:
			parkStashedChanges(ctx, w, autoStashRef)
		default:
			_, settleErr = restoreStashedChanges(ctx, w, gitCmd, root, autoStashRef, o.PromptForRestore, o.GwInputFn, movesFor(token))
		}
		if settleErr != nil {
			return "", settleErr
		}
	}
	return movementBaseline, err
}
