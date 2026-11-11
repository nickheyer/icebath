package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/pm"
)

// Argv sentinels of a branch switch that never ran git, the refused arm and the moved ref
var (
	armRefusedArgs = []string{"arm_commit_point"}
	refMovedArgs   = []string{"ref_moved"}
)

// Settles a git tree move that did not finish, disarming when the tree is whole and owing the restore otherwise
func settleOrOweTreeMove(ctx context.Context, root, pre string) error {
	settled, err := settleFailedTreeMove(ctx, root)
	if err != nil {
		return err
	}
	if settled {
		return disarmCommitObligations(ctx)
	}
	return oweRestoreOf(ctx, root, pre)
}

// Switches the parked checkout to branch under the pull marker and armed obligations, returning git's result
func switchBranchAtCommitPoint(ctx context.Context, w io.Writer, gitCmd []string, branch, targetRef, pre, stash string) (*CompletedProcess, error) {
	root := fleetProjectRoot()
	local, err := resolvedCommit(ctx, gitCmd, "refs/heads/"+branch)
	if err != nil {
		return nil, err
	}
	remote, err := resolvedCommit(ctx, gitCmd, "origin/"+branch)
	if err != nil {
		return nil, err
	}
	startPoint := remote
	if startPoint == "" {
		startPoint = "origin/" + branch
	}
	attempts := []struct {
		args   []string
		target string
	}{
		{[]string{"checkout", "--no-guess", branch}, local},
		{[]string{"checkout", "-B", branch, startPoint}, remote},
	}
	var result *CompletedProcess
	for i, attempt := range attempts {
		if attempt.target == "" {
			if i == 0 {
				continue
			}
			if err := disarmCommitObligations(ctx); err != nil {
				return nil, err
			}
			return updateGitRun(ctx, gitCmd, attempt.args, "", false, false)
		}
		refused, err := armCommitPoint(ctx, gitCmd, root, attempt.target, treeMove{Pre: pre, Target: attempt.target, Stash: stash})
		if err != nil {
			return nil, err
		}
		if refused != "" {
			return &CompletedProcess{Args: armRefusedArgs, ReturnCode: 1, Stderr: refused}, nil
		}
		result, err = updateGitRun(ctx, gitCmd, attempt.args, "", false, false)
		if err != nil {
			if isKeyboardInterrupt(err) {
				return nil, err
			}
			if settleErr := settleOrOweTreeMove(ctx, root, pre); settleErr != nil {
				return nil, settleErr
			}
			return nil, err
		}
		if result.ReturnCode == 0 {
			landed, off, err := landedOffTarget(ctx, gitCmd, attempt.target, false)
			if err != nil {
				return nil, err
			}
			if off {
				fmt.Fprintf(w, "  ✗ The branch moved to %s after %s was selected.\n", short(landed), short(attempt.target))
				if err := settleOrOweTreeMove(ctx, root, pre); err != nil {
					return nil, err
				}
				return &CompletedProcess{Args: refMovedArgs, ReturnCode: 1, Stdout: result.Stdout,
					Stderr: fmt.Sprintf("'%s' moved to %s during the switch; re-run `icebath update`", branch, short(landed))}, nil
			}
			marker, err := pm.InterruptedPullMarkerPath(root)
			if err != nil {
				return nil, err
			}
			if err := unlinkMissingOK(marker); err != nil {
				return nil, err
			}
			return result, nil
		}
		settled, err := settleFailedTreeMove(ctx, root)
		if err != nil {
			return nil, err
		}
		if !settled {
			if err := oweRestoreOf(ctx, root, pre); err != nil {
				return nil, err
			}
			return result, nil
		}
	}
	if err := disarmCommitObligations(ctx); err != nil {
		return nil, err
	}
	return result, nil
}

// Refuses with the install untouched when a precondition that used to fail after the swap does not hold
func refuseBeforeCommitPoint(ctx context.Context, w io.Writer, gitCmd []string, targetRef string, token map[string]any) error {
	root := fleetProjectRoot()
	reason, err := preflightRefusal(ctx, w, gitCmd, root, targetRef, updateCriticalFiles)
	if err != nil {
		return err
	}
	if reason == "" {
		if refusal := downgradeRefusalGit(ctx, gitCmd, root, targetRef, home.Dir(ctx)); refusal != nil {
			reason = *refusal
			recordStop(ctx, "downgrade_refused")
		}
	}
	if reason == "" {
		return nil
	}
	fmt.Fprintln(w, reason)
	fmt.Fprintln(w, "  No update was applied; your install is unchanged.")
	if err := resumeWindowsGatewaysAfterUpdate(ctx, w, token); err != nil {
		return err
	}
	return updateExit(1)
}

// Says whether a branch switch was refused, torn or aimed at a missing branch, then exits 1
func exitAfterFailedBranchSwitch(ctx context.Context, w io.Writer, gitCmd []string, branch string, trackResult *CompletedProcess, autoStashRef string,
	gwInputFn func(prompt, def string) (string, error), token map[string]any) error {
	root := fleetProjectRoot()
	detail := pystr.Strip(trackResult.Stderr)
	marker, err := pm.InterruptedPullMarkerPath(root)
	if err != nil {
		return err
	}
	if info, err := os.Stat(marker); err == nil && info.Mode().IsRegular() {
		fmt.Fprintf(w, "✗ Switching to '%s' stopped part-way; the next `icebath` launch puts the checkout back.\n", branch)
		if detail != "" {
			fmt.Fprintf(w, "  %s\n", pystr.Splitlines(detail, false)[0])
		}
		if autoStashRef != "" {
			fmt.Fprintf(w, "  ℹ️  Local changes preserved in stash (ref: %s)\n", autoStashRef)
			fmt.Fprintln(w, "  Restore manually with: git stash apply")
			clearPendingAutostash()
		}
		recordStop(ctx, "checkout_move_failed")
		return updateExit(1)
	}
	if autoStashRef != "" {
		if _, err := restoreStashedChanges(ctx, w, gitCmd, root, autoStashRef, false, gwInputFn, movesFor(token)); err != nil {
			return err
		}
	}
	refused, moved := slices.Equal(trackResult.Args, armRefusedArgs), slices.Equal(trackResult.Args, refMovedArgs)
	if refused || moved {
		fmt.Fprintf(w, "✗ %s.\n", detail)
		if refused {
			recordStop(ctx, "commit_point_refused")
		} else {
			recordStop(ctx, "head_moved")
		}
	} else {
		fmt.Fprintf(w, "✗ Branch '%s' does not exist locally or on origin.\n", branch)
		if detail != "" {
			fmt.Fprintf(w, "  %s\n", pystr.Splitlines(detail, false)[0])
		}
		recordStop(ctx, missingBranchStop(ctx, gitCmd, branch))
	}
	return updateExit(1)
}

// Stop token of a failed switch, branch_missing unless the branch resolves, never failing
func missingBranchStop(ctx context.Context, gitCmd []string, branch string) (stop string) {
	defer func() {
		if recover() != nil {
			stop = "checkout_move_failed"
		}
	}()
	for _, ref := range []string{"refs/heads/" + branch, "origin/" + branch} {
		commit, err := resolvedCommit(ctx, gitCmd, ref)
		if err != nil || commit != "" {
			return "checkout_move_failed"
		}
	}
	return "branch_missing"
}

// What the pre pull checkout phase decided
type checkoutPlan struct {
	AutoStashRef              string
	CommitCount               int
	InPlaceUpdate             bool
	ParkedBranchSwitched      bool
	PromptForRestore          bool
	SwitchBlockReason         string
	UpstreamChecked           bool
	PreSyncSHA                string
	RollbackBranch            *string
	SwitchedWithoutNewCommits bool
}

// Decides how a checkout parked on another branch reaches the target, switched, in place and why
func applyParkedBranchGuard(ctx context.Context, w io.Writer, gitCmd []string, branch, currentBranch string, switchBranch bool, token map[string]any) (bool, bool, string, error) {
	if currentBranch == branch || currentBranch == "HEAD" {
		return false, false, "", nil
	}
	root := fleetProjectRoot()
	switchSafe, switchBlockReason, err := assessParkedBranchSwitch(ctx, gitCmd, root, currentBranch, branch)
	if err != nil {
		return false, false, "", err
	}
	if !switchSafe {
		printParkedBranchSkipWarning(ctx, w, gitCmd, root, currentBranch, branch, switchBlockReason)
		fmt.Fprintln(w)
		fmt.Fprintf(w, "⚠ Update finished — code update SKIPPED%s\n", branchHeadSuffix(ctx, gitCmd, root))
		if err := resumeWindowsGatewaysAfterUpdate(ctx, w, token); err != nil {
			return false, false, "", err
		}
		recordStop(ctx, "parked_branch_blocked")
		return false, false, "", updateExit(1)
	}
	if !strings.HasPrefix(switchBlockReason, "unmerged:") {
		fmt.Fprintf(w, "  ⚠ Checkout was parked on '%s' (fully merged) — switching back to %s...\n", currentBranch, branch)
		return true, false, switchBlockReason, nil
	}
	inPlaceConfigured := false
	if err := bestEffort(ctx, "Could not read updates.parked_branch_strategy: %s", func() error {
		section, err := updatesConfig(ctx)
		if err != nil {
			return err
		}
		strategy, ok := section["parked_branch_strategy"]
		if !ok {
			strategy = "switch"
		}
		inPlaceConfigured = pyrepr.Equal(strategy, "update_in_place")
		return nil
	}); err != nil {
		return false, false, "", err
	}
	if !inPlaceConfigured || switchBranch {
		printParkedBranchKeptNotice(w, currentBranch, branch, strings.SplitN(switchBlockReason, ":", 2)[1])
		return true, false, switchBlockReason, nil
	}
	exists, err := updateGitRun(ctx, gitCmd, []string{"rev-parse", "--verify", "--quiet", "origin/" + branch}, "", false, false)
	if err != nil {
		return false, false, "", err
	}
	if exists.ReturnCode != 0 {
		fmt.Fprintf(w, "✗ Branch '%s' does not exist locally or on origin.\n", branch)
		recordStop(ctx, "branch_missing")
		return false, false, "", updateExit(1)
	}
	fmt.Fprintf(w, "  ℹ On branch '%s' — updating it in place from origin/%s (no branch switch; local commits preserved).\n", currentBranch, branch)
	return false, true, switchBlockReason, nil
}

// Keywords of the pre pull checkout phase
type prepareOptions struct {
	IsFork               bool
	AssumeYes            bool
	GatewayMode          bool
	GwInputFn            func(prompt, def string) (string, error)
	SwitchBranch         bool
	TargetRef            string
	WindowsGatewayResume map[string]any
}

// Runs the parked branch guard, lands on the target branch, stashes local changes and counts new commits
func prepareCheckoutForUpdate(ctx context.Context, w io.Writer, gitCmd []string, branch, currentBranch string, o prepareOptions) (*checkoutPlan, error) {
	root := fleetProjectRoot()
	token := o.WindowsGatewayResume
	targetRef := o.TargetRef
	if targetRef == "" {
		targetRef = "origin/" + branch
	}
	releaseTag := targetRef != "origin/"+branch
	parkedBranchSwitched, inPlaceUpdate, switchBlockReason := false, true, ""
	if !releaseTag {
		var err error
		parkedBranchSwitched, inPlaceUpdate, switchBlockReason, err = applyParkedBranchGuard(ctx, w, gitCmd, branch, currentBranch, o.SwitchBranch, token)
		if err != nil {
			return nil, err
		}
	}
	if !releaseTag && !inPlaceUpdate && currentBranch == "HEAD" && branch != "HEAD" {
		fmt.Fprintf(w, "  ⚠ Currently on detached HEAD — switching to %s for update...\n", branch)
		if err := parkDetachedHead(ctx, w, gitCmd, root, branch); err != nil {
			return nil, err
		}
	}
	switching := !releaseTag && !inPlaceUpdate && currentBranch != branch
	promptForRestore := !o.AssumeYes && (o.GatewayMode || (updateStdinIsTTY() && updateStdoutIsTTY(w)))
	askFirst := promptForRestore && !o.GatewayMode && switching
	if promptForRestore && !o.GatewayMode && !switching {
		var err error
		if askFirst, err = targetWouldMoveHead(ctx, gitCmd, root, targetRef); err != nil {
			return nil, err
		}
	}
	untrackedTargets := []string{}
	if !o.IsFork {
		untrackedTargets = []string{targetRef, "origin/" + branch, "refs/heads/" + branch}
	}
	stashed, err := stashLocalChangesIfNeeded(ctx, w, gitCmd, root, movesFor(token), untrackedTargets, askFirst)
	if err != nil {
		return nil, err
	}
	autoStashRef := ""
	if stashed != nil {
		autoStashRef = *stashed
	}
	movedFromSHA := ""
	var rollbackBranch *string
	if switching {
		movedFromSHA = captureHeadSHA(ctx, gitCmd, root)
		rollbackBranch = &currentBranch
		if _, err := pauseAtCommitPoint(ctx); err != nil {
			return nil, err
		}
		var targets []string
		for _, ref := range []string{"refs/heads/" + branch, "origin/" + branch} {
			commit, err := resolvedCommit(ctx, gitCmd, ref)
			if err != nil {
				return nil, err
			}
			targets = append(targets, commit)
		}
		var trackResult *CompletedProcess
		if err := withCheckoutMove(ctx, movesFor(token), checkoutMoveBind{Targets: targets}, func(map[string]any) error {
			var err error
			trackResult, err = switchBranchAtCommitPoint(ctx, w, gitCmd, branch, targetRef, movedFromSHA, autoStashRef)
			return err
		}); err != nil {
			return nil, err
		}
		if trackResult.ReturnCode != 0 {
			return nil, exitAfterFailedBranchSwitch(ctx, w, gitCmd, branch, trackResult, autoStashRef, o.GwInputFn, token)
		}
	}
	promptForRestore = autoStashRef != "" && promptForRestore && !askFirst
	if releaseTag {
		commitCount := -1
		if captureHeadSHA(ctx, gitCmd, root) == targetRef {
			commitCount = 0
		}
		return &checkoutPlan{AutoStashRef: autoStashRef, CommitCount: commitCount, InPlaceUpdate: true,
			PromptForRestore: promptForRestore, UpstreamChecked: true}, nil
	}
	base := movedFromSHA
	if base == "" {
		base = "HEAD"
	}
	result, err := updateGitRun(ctx, gitCmd, []string{"rev-list", base + ".." + targetRef, "--count"}, "", true, false)
	if err != nil {
		return nil, err
	}
	commitCount, err := strconv.Atoi(pystr.Strip(result.Stdout))
	if err != nil {
		return nil, &ValueError{Msg: "invalid literal for int() with base 10: " + pyrepr.Repr(pystr.Strip(result.Stdout))}
	}
	applyIsShallow, err := isShallowCheckout(ctx, gitCmd)
	if err != nil {
		return nil, err
	}
	if commitCount > 0 && applyIsShallow {
		baseSHA, targetSHA, err := tipShas(ctx, gitCmd, targetRef, base)
		if err != nil {
			return nil, err
		}
		commitCount = updateCompareBehind(ctx, baseSHA, targetSHA, OfficialRepository)
	}
	switchedWithoutNewCommits := false
	if commitCount == 0 && movedFromSHA != "" {
		if landedSHA := captureHeadSHA(ctx, gitCmd, root); landedSHA != "" && landedSHA != movedFromSHA {
			commitCount = -1
			switchedWithoutNewCommits = true
		}
	}
	upstreamChecked := true
	if commitCount == 0 && o.IsFork && branch == "main" && !releaseTag {
		preSyncSHA := captureHeadSHA(ctx, gitCmd, root)
		upstreamChecked, err = syncWithUpstreamIfNeeded(ctx, w, gitCmd, root, o.AssumeYes, o.GwInputFn, movesFor(token))
		var broken *UpstreamTargetBroken
		if errors.As(err, &broken) {
			if autoStashRef != "" {
				fmt.Fprintf(w, "  ℹ️  Local changes preserved in stash (ref: %s)\n", autoStashRef)
				fmt.Fprintln(w, "  Restore manually with: git stash apply")
				clearPendingAutostash()
			}
			return nil, rollBackBrokenUpdate(ctx, w, gitCmd, preSyncSHA, broken.Path, broken.Detail,
				rollbackOptions{Header: upstreamBrokenHeader(broken), Token: token, StopClass: "target_syntax_error"})
		}
		if err != nil {
			return nil, err
		}
		postSyncSHA := captureHeadSHA(ctx, gitCmd, root)
		if preSyncSHA != "" && postSyncSHA != "" && preSyncSHA != postSyncSHA {
			commitCount = max(1, countCommitsBetween(ctx, gitCmd, root, preSyncSHA, postSyncSHA))
			if movedFromSHA == "" {
				movedFromSHA = preSyncSHA
			}
		}
	}
	return &checkoutPlan{AutoStashRef: autoStashRef, CommitCount: commitCount, InPlaceUpdate: inPlaceUpdate,
		ParkedBranchSwitched: parkedBranchSwitched, PromptForRestore: promptForRestore, SwitchBlockReason: switchBlockReason,
		UpstreamChecked: upstreamChecked, PreSyncSHA: movedFromSHA, RollbackBranch: rollbackBranch,
		SwitchedWithoutNewCommits: switchedWithoutNewCommits}, nil
}

// The HEAD after the pull, exiting 1 when the pull moved nothing or landed off the target branch
func verifyHeadAfterPull(ctx context.Context, w io.Writer, gitCmd []string, branch, prePullSHA string, inPlaceUpdate bool, token map[string]any) (string, error) {
	root := fleetProjectRoot()
	postPullSHA := captureHeadSHA(ctx, gitCmd, root)
	if prePullSHA != "" && postPullSHA == prePullSHA {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "✗ Code did not move — update was a no-op.")
		fmt.Fprintf(w, "  HEAD is pinned to %s (detached checkout); origin/%s advanced but the working tree stayed put.\n", short(prePullSHA), branch)
		fmt.Fprintf(w, "  Reattach to the branch and retry: git -C %s checkout %s && icebath update\n", root, branch)
		if err := disarmCommitObligations(ctx); err != nil {
			return "", err
		}
		if err := resumeWindowsGatewaysAfterUpdate(ctx, w, token); err != nil {
			return "", err
		}
		recordStop(ctx, "detached_head")
		return "", updateExit(1)
	}
	postPullBranch, err := currentBranchName(ctx, gitCmd, false)
	if err != nil {
		return "", err
	}
	if !inPlaceUpdate && postPullBranch != "" && postPullBranch != branch && postPullBranch != "HEAD" {
		fmt.Fprintln(w)
		fmt.Fprintf(w, "✗ Update pulled origin/%s, but the checkout is on '%s' — not claiming success.\n", branch, postPullBranch)
		fmt.Fprintf(w, "  Switch to the target branch and retry: git -C %s checkout %s && icebath update\n", root, branch)
		if err := resumeWindowsGatewaysAfterUpdate(ctx, w, token); err != nil {
			return "", err
		}
		recordStop(ctx, "unexpected_branch")
		return "", updateExit(1)
	}
	return postPullSHA, nil
}

// The branch name rev-parse abbreviates HEAD to, the literal HEAD when detached
func currentBranchName(ctx context.Context, gitCmd []string, check bool) (string, error) {
	result, err := updateGitRun(ctx, gitCmd, []string{"rev-parse", "--abbrev-ref", "HEAD"}, "", check, false)
	if err != nil {
		return "", err
	}
	return pystr.Strip(result.Stdout), nil
}
