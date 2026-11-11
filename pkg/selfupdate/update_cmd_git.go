package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyshlex"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	orphanRescueRefsToKeep    = 10
	orphanRescueRefMaxAgeDays = 30
	officialRepoURL           = "https://github.com/" + OfficialRepository + ".git"
	upstreamAddCmd            = "git remote add upstream " + officialRepoURL
	skipUpstreamPromptFile    = ".skip_upstream_prompt"
)

var updateBar = strings.Repeat("=", 68)

var officialRepoURLs = []string{
	"https://github.com/" + OfficialRepository + ".git",
	"git@github.com:" + OfficialRepository + ".git",
	"https://github.com/" + OfficialRepository,
	"git@github.com:" + OfficialRepository,
}

var parkedSkipWhy = map[string]string{
	"dirty":    "the working tree has uncommitted changes",
	"disabled": "updates.auto_switch_parked_branch is set to false in config.yaml",
}

// Reads one line the way input does, the prompt written first, end of input and Ctrl-C as cliui errors
var updateInput = func(prompt string) (string, error) { return cliui.Default().Input(prompt) }

// What one checkout writing git step binds the paused gateways tree gate to before git writes a file
type checkoutMoveBind struct {
	Targets []string
	Paths   []string
	Revert  bool
}

// Opens one bound checkout step, yielding its step record and the settle call that closes it
type checkoutMoveFunc func(ctx context.Context, bind checkoutMoveBind) (step map[string]any, settle func() error, err error)

// Whether a failure stands for a Python OSError, a custody refusal included
func isUpdateOSError(err error) bool {
	var refused *updaterecord.CustodyRefused
	return err != nil && (errors.As(err, &refused) || pypath.IsOSError(err) || osErrorClasses[pm.ErrorClass(err)])
}

// Whether a failure stands for a subprocess.SubprocessError, a failed check or a timeout
func isSubprocessError(err error) bool {
	if _, called := asCalledProcessError(err); called {
		return true
	}
	var gitTimeout *updaterecord.GitTimeout
	var expired *pysubprocess.TimeoutExpired
	return errors.As(err, &gitTimeout) || errors.As(err, &expired)
}

// Whether path exists, following symlinks
func statExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// Runs one step inside the caller's checkout move, the settle failure outranking the step's own
func withCheckoutMove(ctx context.Context, move checkoutMoveFunc, bind checkoutMoveBind, body func(step map[string]any) error) error {
	if move == nil {
		move = noMove
	}
	step, settle, err := move(ctx, bind)
	if err != nil {
		return err
	}
	bodyErr := body(step)
	if settleErr := settle(); settleErr != nil {
		return settleErr
	}
	return bodyErr
}

// True when the updater's git exits 0, any failure counting as false
func gitOK(ctx context.Context, gitCmd, args []string, cwd string, network bool) bool {
	_, ok := gitStdout(ctx, gitCmd, args, cwd, network)
	return ok
}

// Runs the updater's git capturing all output decoded as UTF-8, env replacing the child's environment
func gitRunCaptured(ctx context.Context, gitCmd, args []string, cwd string, check bool, env []string) (*CompletedProcess, error) {
	return RunGit(ctx, gitCmd, slices.Clone(args), ChildOptions{Dir: cwd, Env: env, Capture: true, Text: true, Check: check, Attr: tools.WindowsHideAttr()})
}

// Stripped stdout of a successful updater git, false on a nonzero exit or any failure
func gitStdout(ctx context.Context, gitCmd, args []string, cwd string, network bool) (string, bool) {
	result, err := updateGitRun(ctx, gitCmd, args, cwd, false, network)
	if err != nil || result == nil || result.ReturnCode != 0 {
		return "", false
	}
	return pystr.Strip(result.Stdout), true
}

// Expires old rescue refs, keeping the newest of each kind and dropping any past the age limit
func pruneOrphanRescueRefs(ctx context.Context, gitCmd []string, cwd, branch string, keep, maxAgeDays int) error {
	stale := map[string]bool{}
	for _, kind := range []string{"orphan", "diverged", "detached"} {
		prefix := "refs/icebath-update-backups/" + kind + "-" + branch + "-"
		listed, err := gitRunCaptured(ctx, gitCmd, []string{"for-each-ref", "--format=%(refname)", "--sort=refname", prefix + "*"}, cwd, false, nil)
		if err != nil {
			if isUpdateOSError(err) {
				return nil
			}
			return err
		}
		if listed.ReturnCode != 0 {
			continue
		}
		var refs []string
		for _, line := range pystr.Splitlines(listed.Stdout, false) {
			if ref := pystr.Strip(line); ref != "" {
				refs = append(refs, ref)
			}
		}
		if keep > 0 {
			for _, ref := range refs[:max(len(refs)-keep, 0)] {
				stale[ref] = true
			}
		} else {
			for _, ref := range refs {
				stale[ref] = true
			}
		}
		if maxAgeDays > 0 {
			cutoff := time.Now().UTC().Add(-time.Duration(maxAgeDays) * 24 * time.Hour)
			for _, ref := range refs {
				stamp, _, err := pydatetime.Strptime(clipRunes(ref[len(prefix):], 15), "%Y%m%d-%H%M%S")
				if err != nil {
					continue
				}
				if utcWall(stamp).Before(cutoff) {
					stale[ref] = true
				}
			}
		}
	}
	names := make([]string, 0, len(stale))
	for ref := range stale {
		names = append(names, ref)
	}
	sort.Strings(names)
	for _, ref := range names {
		if _, err := gitRunCaptured(ctx, gitCmd, []string{"update-ref", "-d", ref}, cwd, false, nil); err != nil {
			if isUpdateOSError(err) {
				return nil
			}
			return err
		}
	}
	return nil
}

// First n code points of s
func clipRunes(s string, n int) string {
	runes := []rune(s)
	if len(runes) > n {
		return string(runes[:n])
	}
	return s
}

// The wall clock fields of a parsed naive stamp read as UTC
func utcWall(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), time.UTC)
}

// Backs up commits made on a detached HEAD no ref contains before the update moves HEAD off it
func parkDetachedHead(ctx context.Context, w io.Writer, gitCmd []string, cwd, branch string) error {
	attached, err := updateGitRun(ctx, gitCmd, []string{"symbolic-ref", "-q", "HEAD"}, cwd, false, false)
	if err != nil {
		return err
	}
	if attached.ReturnCode == 0 {
		return nil
	}
	head, err := updateGitRun(ctx, gitCmd, []string{"rev-parse", "--verify", "--quiet", "HEAD^{commit}"}, cwd, false, false)
	if err != nil {
		return err
	}
	sha := pystr.Strip(head.Stdout)
	if head.ReturnCode != 0 || sha == "" {
		return nil
	}
	contains, err := updateGitRun(ctx, gitCmd, []string{"for-each-ref", "--contains", sha, "--format=%(refname)"}, cwd, false, false)
	if err != nil {
		return err
	}
	holders := 0
	for _, ref := range strings.Fields(contains.Stdout) {
		if ref != "refs/stash" {
			holders++
		}
	}
	if contains.ReturnCode == 0 && holders > 0 {
		return nil
	}
	stamp := time.Now().UTC().Format("20060102-150405")
	sha12 := clipRunes(sha, 12)
	rescueRef := "refs/icebath-update-backups/detached-" + branch + "-" + stamp + "-" + sha12
	written, err := updateGitRun(ctx, gitCmd, []string{"update-ref", rescueRef, sha}, cwd, false, false)
	if err != nil {
		return err
	}
	if written.ReturnCode != 0 {
		fmt.Fprintf(w, "✗ HEAD is detached at %s, which no branch or tag contains, and backing it up to %s failed.\n", sha12, rescueRef)
		fmt.Fprintf(w, "  Update stopped so those commits are not orphaned. Keep them with: git -C %s branch <name> %s\n", cwd, sha12)
		recordStop(ctx, "detached_head")
		return &pm.SystemExit{Code: 1}
	}
	counted, err := updateGitRun(ctx, gitCmd, []string{"rev-list", "--count", sha, "--not", "--branches", "--tags", "--remotes"}, cwd, false, false)
	if err != nil {
		return err
	}
	count := pystr.Strip(counted.Stdout)
	if count == "" {
		count = "Some"
	}
	fmt.Fprintf(w, "  ⚠ %s commit(s) made on the detached HEAD are on no branch — backed up to %s before moving HEAD. This backup expires after %d days.\n",
		count, rescueRef, orphanRescueRefMaxAgeDays)
	fmt.Fprintf(w, "    List them with: git log %s --not --branches --tags --remotes\n", rescueRef)
	return pruneOrphanRescueRefs(ctx, gitCmd, cwd, branch, orphanRescueRefsToKeep, orphanRescueRefMaxAgeDays)
}

// Branch and short commit of the checkout as one label, detached when on no branch, false when unknown
func branchHeadLabel(ctx context.Context, gitCmd []string, cwd string) (string, bool) {
	cmd := gitCmd
	if len(cmd) == 0 {
		cmd = []string{"git"}
	}
	root := cwd
	if root == "" {
		root = fleetProjectRoot()
	}
	revParse := func(args ...string) (*CompletedProcess, error) {
		return RunGit(ctx, cmd, append([]string{"rev-parse"}, args...), ChildOptions{Dir: root, Capture: true, Text: true, Attr: tools.WindowsHideAttr()})
	}
	branch, err := revParse("--abbrev-ref", "HEAD")
	if err != nil {
		return "", false
	}
	sha, err := revParse("--short", "HEAD")
	if err != nil {
		return "", false
	}
	branchName, shaText := pystr.Strip(branch.Stdout), pystr.Strip(sha.Stdout)
	if branch.ReturnCode != 0 || sha.ReturnCode != 0 || shaText == "" || branchName == "" {
		return "", false
	}
	if branchName == "HEAD" {
		branchName = "detached"
	}
	return branchName + " @ " + shaText, true
}

// Bracketed branch and commit suffix for summary lines, empty when unknown
func branchHeadSuffix(ctx context.Context, gitCmd []string, cwd string) string {
	if label, ok := branchHeadLabel(ctx, gitCmd, cwd); ok && label != "" {
		return " [" + label + "]"
	}
	return ""
}

// Whether a parked feature branch may be switched back to the update target, with the reason or unmerged count
func assessParkedBranchSwitch(ctx context.Context, gitCmd []string, cwd, currentBranch, targetBranch string) (bool, string, error) {
	if cfg, err := updatesConfig(ctx); err != nil {
		updateLog.Debug(fmt.Sprintf("Could not read updates.auto_switch_parked_branch: %s", launchErrorText(err)))
	} else if value, set := cfg["auto_switch_parked_branch"]; set && !pyrepr.Truthy(value) {
		return false, "disabled", nil
	}
	status, err := gitRunCaptured(ctx, gitCmd, []string{"status", "--porcelain"}, cwd, false, nil)
	if err != nil {
		return false, "", err
	}
	if status.ReturnCode != 0 {
		return false, "unverifiable", nil
	}
	if pystr.Strip(status.Stdout) != "" {
		return false, "dirty", nil
	}
	ahead, err := gitRunCaptured(ctx, gitCmd, []string{"rev-list", "--count", "origin/" + targetBranch + "..HEAD"}, cwd, false, nil)
	if err != nil {
		return false, "", err
	}
	aheadText := pystr.Strip(ahead.Stdout)
	if ahead.ReturnCode != 0 || !pystr.StrIsDigit(aheadText) {
		return false, "unverifiable", nil
	}
	aheadCount, ok := pyint.Parse(aheadText)
	if !ok {
		return false, "unverifiable", nil
	}
	if aheadCount.Sign() == 0 {
		return true, "", nil
	}
	promisor, err := gitRunCaptured(ctx, gitCmd, []string{"config", "--get-regexp", `^(remote\..*\.promisor|extensions\.partialclone)$`}, cwd, false, nil)
	if err != nil {
		return false, "", err
	}
	for _, line := range pystr.Splitlines(promisor.Stdout, false) {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			return false, "", &pm.PyError{Class: "IndexError", Message: "list index out of range"}
		}
		if pystr.Lower(fields[len(fields)-1]) != "false" {
			return true, "unmerged:" + aheadCount.String(), nil
		}
	}
	cherry, err := gitRunCaptured(ctx, gitCmd, []string{"cherry", "origin/" + targetBranch}, cwd, false, noLazyFetchGitEnv())
	if err != nil {
		return false, "", err
	}
	if cherry.ReturnCode != 0 {
		return true, "unmerged:" + aheadCount.String(), nil
	}
	unmerged := 0
	for _, line := range pystr.Splitlines(cherry.Stdout, false) {
		if strings.HasPrefix(line, "+") {
			unmerged++
		}
	}
	if unmerged > 0 {
		return true, "unmerged:" + strconv.Itoa(unmerged), nil
	}
	return true, "", nil
}

// Environment of an internal git that never prompts and never lazily fetches a missing object
func noLazyFetchGitEnv() []string {
	env := tools.NoninteractiveGitEnv(nil)
	env["GIT_NO_LAZY_FETCH"] = "1"
	out := make([]string, 0, len(env))
	for key, value := range env {
		out = append(out, key+"="+value)
	}
	sort.Strings(out)
	return out
}

// Prints why the update was skipped on a parked branch, how far behind it is and the fix commands
func printParkedBranchSkipWarning(ctx context.Context, w io.Writer, gitCmd []string, cwd, currentBranch, targetBranch, reason string) {
	behind, known := 0, false
	if text, ok := gitStdout(ctx, gitCmd, []string{"rev-list", "HEAD..origin/" + targetBranch, "--count"}, cwd, false); ok && text != "" {
		if n, parsed := pyint.Parse(text); parsed && n.IsInt64() {
			behind, known = int(n.Int64()), true
		}
	}
	why, ok := parkedSkipWhy[reason]
	if !ok {
		why = "the branch state could not be verified against origin/" + targetBranch
	}
	fmt.Fprintf(w, "\n%s\n⚠ CODE UPDATE SKIPPED — checkout is parked on '%s'\n", updateBar, currentBranch)
	fmt.Fprintf(w, "  Not auto-switching to %s: %s.\n", targetBranch, why)
	if known && behind > 0 {
		fmt.Fprintf(w, "  This checkout is %d commit(s) BEHIND origin/%s — the code you are running is stale.\n", behind, targetBranch)
	}
	fmt.Fprintf(w, "\n  To resolve, inspect the branch and switch back yourself:\n    git -C %s status\n    git -C %s checkout %s && icebath update\n"+
		"  (commit or stash your work on the branch first if you want to keep it)\n%s\n", cwd, cwd, targetBranch, updateBar)
}

// Prints the notice for a clean parked branch with unmerged commits that is switched to the target anyway
func printParkedBranchKeptNotice(w io.Writer, currentBranch, targetBranch, unmergedCount string) {
	fmt.Fprintf(w, "\n%s\n⚠ Checkout was parked on '%s' with %s commit(s) not merged into origin/%s.\n"+
		"  Switching to %s so the update can proceed — your commit(s) are safe on '%s'.\n\n"+
		"  To pick the work back up later:\n    git checkout %s\n%s\n",
		updateBar, currentBranch, unmergedCount, targetBranch, targetBranch, currentBranch, currentBranch, updateBar)
}

// URL of the origin remote, false when it is not set
func getOriginURL(ctx context.Context, gitCmd []string, cwd string) (string, bool) {
	return gitStdout(ctx, gitCmd, []string{"remote", "get-url", "origin"}, cwd, false)
}

// Whether the origin remote points to a fork instead of the official repository
func isFork(originURL string) bool {
	if originURL == "" {
		return false
	}
	norm := func(url string) string { return strings.TrimSuffix(strings.TrimRight(url, "/"), ".git") }
	for _, official := range officialRepoURLs {
		if norm(originURL) == norm(official) {
			return false
		}
	}
	return true
}

// Whether an upstream remote already exists
func hasUpstreamRemote(ctx context.Context, gitCmd []string, cwd string) bool {
	return gitOK(ctx, gitCmd, []string{"remote", "get-url", "upstream"}, cwd, false)
}

// Adds the official repository as the upstream remote, true on success
func addUpstreamRemote(ctx context.Context, gitCmd []string, cwd string) bool {
	return gitOK(ctx, gitCmd, []string{"remote", "add", "upstream", officialRepoURL}, cwd, false)
}

// Count of commits on head that are not on base, -1 on any failure
func countCommitsBetween(ctx context.Context, gitCmd []string, cwd, base, head string) int {
	text, ok := gitStdout(ctx, gitCmd, []string{"rev-list", "--count", base + ".." + head}, cwd, false)
	if !ok {
		return -1
	}
	n, parsed := pyint.Parse(text)
	if !parsed || !n.IsInt64() {
		return -1
	}
	return int(n.Int64())
}

// Whether the user previously declined to add the upstream remote
func shouldSkipUpstreamPrompt(ctx context.Context) bool {
	return statExists(filepath.Join(home.Dir(ctx), skipUpstreamPromptFile))
}

// Creates the marker file that skips future upstream prompts
func markSkipUpstreamPrompt(ctx context.Context) {
	path := filepath.Join(home.Dir(ctx), skipUpstreamPromptFile)
	handle, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE, 0o666)
	if err != nil {
		return
	}
	handle.Close()
	now := time.Now()
	_ = os.Chtimes(path, now, now)
}

// Pushes the updated main to origin to sync the fork, true on success
func syncForkWithUpstream(ctx context.Context, gitCmd []string, cwd string) bool {
	return gitOK(ctx, gitCmd, []string{"push", "origin", "main", "--force-with-lease"}, cwd, true)
}

// Offers to add the upstream remote and adds it, false when declined, noninteractive or the add failed
func offerUpstreamRemote(ctx context.Context, w io.Writer, gitCmd []string, cwd string, assumeYes bool, inputFn func(prompt, def string) (string, error)) (bool, error) {
	fmt.Fprint(w, "\nℹ Your fork is not tracking the official Icebath repository.\n  This means you may miss updates from "+OfficialRepository+".\n\n")
	if assumeYes || (inputFn == nil && !(updateStdinIsTTY() && updateStdoutIsTTY(w))) {
		fmt.Fprintf(w, "  Skipping upstream setup (non-interactive run).\n  Add it later with: %s\n", upstreamAddCmd)
		return false, nil
	}
	response := ""
	if inputFn != nil {
		answer, err := inputFn("Add official repo as 'upstream' remote? [y/N]", "n")
		if err != nil {
			return false, err
		}
		response = pystr.Lower(pystr.Strip(answer))
	} else {
		fmt.Fprint(w, "Add official repo as 'upstream' remote? [Y/n]: ")
		answer, err := updateInput("")
		if err != nil {
			fmt.Fprintln(w)
			response = "n"
		} else {
			response = pystr.Lower(pystr.Strip(answer))
		}
	}
	if response != "" && response != "y" && response != "yes" {
		fmt.Fprintf(w, "  Skipped. Run '%s' to add later.\n", upstreamAddCmd)
		markSkipUpstreamPrompt(ctx)
		return false, nil
	}
	fmt.Fprintln(w, "→ Adding upstream remote...")
	if !addUpstreamRemote(ctx, gitCmd, cwd) {
		fmt.Fprintln(w, "  ✗ Failed to add upstream remote. Skipping upstream sync.")
		return false, nil
	}
	fmt.Fprintln(w, "  ✓ Added upstream: "+officialRepoURL)
	return true, nil
}

// The fork sync's upstream target fails the startup syntax check and is refused before its move
type UpstreamTargetBroken struct {
	SHA, Path, Detail string
}

func (e *UpstreamTargetBroken) Error() string {
	return "upstream/main (" + short(e.SHA) + ") has a syntax error in " + e.Path
}

// Compares origin/main with upstream/main and fast forwards when strictly behind, true when the compare happened
func syncWithUpstreamIfNeeded(ctx context.Context, w io.Writer, gitCmd []string, cwd string, assumeYes bool,
	inputFn func(prompt, def string) (string, error), move checkoutMoveFunc) (bool, error) {
	if !hasUpstreamRemote(ctx, gitCmd, cwd) {
		if shouldSkipUpstreamPrompt(ctx) {
			return false, nil
		}
		if added, err := offerUpstreamRemote(ctx, w, gitCmd, cwd, assumeYes, inputFn); err != nil || !added {
			return false, err
		}
	}
	fmt.Fprintln(w, "\n→ Fetching upstream...")
	fetch := noPromptGitKwargs()
	fetch.Dir, fetch.Capture, fetch.Check = cwd, true, true
	if _, err := RunGit(ctx, gitCmd, []string{"fetch", "upstream", trackingRefspec("upstream", "main"), "--quiet"}, fetch); err != nil {
		if _, called := asCalledProcessError(err); called {
			fmt.Fprintln(w, "  ✗ Failed to fetch upstream. Skipping upstream sync.")
			return false, nil
		}
		return false, err
	}
	upstream, _ := gitStdout(ctx, gitCmd, []string{"rev-parse", "-q", "--verify", "refs/remotes/upstream/main^{commit}"}, cwd, false)
	originAhead, upstreamAhead := -1, -1
	if upstream != "" {
		originAhead = countCommitsBetween(ctx, gitCmd, cwd, upstream, "refs/remotes/origin/main")
		upstreamAhead = countCommitsBetween(ctx, gitCmd, cwd, "refs/remotes/origin/main", upstream)
	}
	if originAhead < 0 || upstreamAhead < 0 {
		fmt.Fprintln(w, "  ✗ Could not compare branches. Skipping upstream sync.")
		return false, nil
	}
	if originAhead > 0 {
		fmt.Fprintf(w, "\nℹ Your fork has %d commit(s) not on upstream.\n  Skipping upstream sync to preserve your changes.\n"+
			"  If you want to merge upstream changes, run:\n    git pull upstream main\n", originAhead)
		return true, nil
	}
	if upstreamAhead == 0 {
		fmt.Fprintln(w, "  ✓ Fork is up to date with upstream")
		return true, nil
	}
	fmt.Fprintf(w, "\n→ Fork is %d commit(s) behind upstream\n→ Pulling from upstream...\n", upstreamAhead)
	pre, _ := gitStdout(ctx, gitCmd, []string{"rev-parse", "HEAD"}, cwd, false)
	if !IsObjectID(pre) {
		fmt.Fprintln(w, "  ✗ Could not resolve HEAD. Skipping upstream sync.")
		return false, nil
	}
	target := upstream
	broken, err := targetSyntaxError(ctx, w, gitCmd, cwd, target, updateCriticalFiles)
	if err != nil {
		return false, err
	}
	if broken != nil {
		return false, &UpstreamTargetBroken{SHA: target, Path: broken.Path, Detail: broken.Error}
	}
	refused, err := armCommitPoint(ctx, gitCmd, cwd, debtSHAForMove(pre, target), treeMove{Pre: pre, Target: target})
	if err != nil {
		return false, err
	}
	var moveErr error
	if refused != "" {
		moveErr = &CalledProcessError{ReturnCode: 1, Cmd: []string{"merge"}, Stderr: refused}
	} else {
		moveErr = withCheckoutMove(ctx, move, checkoutMoveBind{Targets: []string{upstream}}, func(map[string]any) error {
			merge := noPromptGitKwargs()
			merge.Dir, merge.Check = cwd, true
			_, err := RunGit(ctx, gitCmd, []string{"merge", "--ff-only", upstream}, merge)
			return err
		})
	}
	if moveErr != nil {
		if !isUpdateOSError(moveErr) && !isSubprocessError(moveErr) {
			return false, moveErr
		}
		settled := refused != ""
		if !settled {
			if settled, err = settleFailedTreeMove(ctx, cwd); err != nil {
				return false, err
			}
		}
		if settled {
			err = disarmCommitObligations(ctx)
		} else {
			err = oweRestoreOf(ctx, cwd, pre)
		}
		if err != nil {
			return false, err
		}
		fmt.Fprintln(w, "  ✗ Failed to pull from upstream. You may need to resolve conflicts manually.")
		detail := subprocessStderr(moveErr)
		if _, called := asCalledProcessError(moveErr); detail == "" && !called {
			detail = launchErrorText(moveErr)
		}
		if detail != "" {
			fmt.Fprintf(w, "    (%s)\n", detail)
		}
		return false, nil
	}
	marker, err := pm.InterruptedPullMarkerPath(cwd)
	if err != nil {
		return false, err
	}
	if err := unlinkMissingOK(marker); err != nil {
		return false, err
	}
	landed, _ := gitStdout(ctx, gitCmd, []string{"rev-parse", "-q", "--verify", "HEAD"}, cwd, false)
	if landed != upstream {
		if err := disarmCommitObligations(ctx); err != nil {
			return false, err
		}
		shown := landed
		if shown == "" {
			shown = "?"
		}
		fmt.Fprintf(w, "  ✗ HEAD is on %s after the upstream merge, not %s.\n", short(shown), short(upstream))
		return false, nil
	}
	fmt.Fprintln(w, "  ✓ Updated from upstream")
	return true, nil
}

// Publishes an upstream sync to the fork's main once the update is validated
func pushSyncedFork(ctx context.Context, w io.Writer, gitCmd []string, cwd string) {
	head, _ := gitStdout(ctx, gitCmd, []string{"rev-parse", "HEAD"}, cwd, false)
	upstream, upstreamOK := gitStdout(ctx, gitCmd, []string{"rev-parse", "-q", "--verify", "refs/remotes/upstream/main^{commit}"}, cwd, false)
	origin, originOK := gitStdout(ctx, gitCmd, []string{"rev-parse", "-q", "--verify", "refs/remotes/origin/main^{commit}"}, cwd, false)
	if head == "" || !upstreamOK || head != upstream || (originOK && head == origin) {
		return
	}
	if origin != "" && !gitOK(ctx, gitCmd, []string{"merge-base", "--is-ancestor", origin, head}, cwd, false) {
		return
	}
	fmt.Fprintln(w, "→ Syncing fork...")
	if syncForkWithUpstream(ctx, gitCmd, cwd) {
		fmt.Fprintln(w, "  ✓ Fork synced with upstream")
		return
	}
	fmt.Fprint(w, "  ℹ Got updates from upstream but couldn't push to fork (no write access?)\n"+
		"    Your local repo is updated, but your fork on GitHub may be behind.\n")
}

// Repair text for a checkout git rejects, the installer pointed at this very checkout
func damagedCheckoutMessage(root string) string {
	var repair string
	if custodyWindows() {
		quoted := "'" + strings.ReplaceAll(root, "'", "''") + "'"
		repair = "& ([scriptblock]::Create((irm https://hermes-agent.nousresearch.com/install.ps1))) -InstallDir " + quoted
	} else {
		repair = "curl -fsSL https://hermes-agent.nousresearch.com/install.sh | bash -s -- --dir " + pyshlex.Quote(root)
	}
	return "✗ This Icebath checkout's git data is damaged (git no longer recognizes " + pypath.PathJoin(root, ".git") + ")," +
		" so it cannot be updated in place.\n" +
		"  Run the installer again to repair it; it moves the damaged checkout aside and keeps" +
		" your config and chats:\n    " + repair
}

// Whether a fetch failed because git cannot open root itself instead of its remote
func checkoutRejectedByGit(ctx context.Context, stderr string, gitCmd []string, root string) (bool, error) {
	rejected := false
	for _, line := range pystr.Splitlines(stderr, false) {
		if strings.HasPrefix(line, "fatal: not a git repository") {
			rejected = true
			break
		}
	}
	if !rejected {
		return false, nil
	}
	top, err := gitRunCaptured(ctx, gitCmd, []string{"rev-parse", "--show-toplevel"}, root, false, nil)
	if err != nil {
		return false, err
	}
	if top.ReturnCode != 0 {
		return true, nil
	}
	seen, err := pypath.Realpath(pystr.Strip(top.Stdout), pypath.Lax)
	if err != nil {
		return false, err
	}
	own, err := pypath.Realpath(root, pypath.Lax)
	if err != nil {
		return false, err
	}
	return pypath.Normcase(seen) != pypath.Normcase(own), nil
}

// Prints the classified fetch diagnosis and bounded raw stderr, the installer repair for a rejected checkout
func printFetchFailure(ctx context.Context, w io.Writer, stderr string, gitCmd []string, root string) error {
	stderr = pystr.Strip(stderr)
	damaged := false
	if root != "" {
		rejected, err := checkoutRejectedByGit(ctx, stderr, gitCmd, pypath.PathStr(root))
		if err != nil {
			return err
		}
		damaged = rejected
	}
	if damaged {
		fmt.Fprintln(w, damagedCheckoutMessage(pypath.PathStr(root)))
	} else {
		fmt.Fprintln(w, ClassifyFetchFailure(stderr))
	}
	if stderr != "" {
		lines := pystr.Splitlines(stderr, false)
		for _, line := range lines[:min(len(lines), 15)] {
			fmt.Fprintf(w, "  %s\n", line)
		}
		if len(lines) > 15 {
			fmt.Fprintf(w, "  ... (%d more lines omitted)\n", len(lines)-15)
		}
	}
	return nil
}

// Runs argv --version, guard true when the trampoline message is seen, ok false when the probe itself failed
func probeForkBomb(ctx context.Context, argv []string) (guard, ok bool) {
	result, err := RunGit(ctx, argv, []string{"--version"}, ChildOptions{Capture: true, Text: true, Timeout: 15 * time.Second, Attr: tools.WindowsHideAttr()})
	if err != nil || result == nil {
		return false, false
	}
	return strings.Contains(pystr.Lower(result.Stdout+result.Stderr), "fork bomb"), true
}

// Whether gitCmd is a broken Git for Windows trampoline shim, unknown states reporting false
func gitIsTrampoline(ctx context.Context, gitCmd []string) bool {
	guard, ok := probeForkBomb(ctx, gitCmd)
	return ok && guard
}

// PortableGit candidates under the shared root first, then the profile home
func portableGitCandidates(ctx context.Context) []string {
	var candidates []string
	for _, root := range []string{home.Root(), home.Dir(ctx)} {
		candidates = append(candidates, filepath.Join(root, "git", "mingw64", "libexec", "git-core", "git.exe"))
	}
	return candidates
}

// A real Git for Windows git-core binary that runs without the trampoline guard, false when none suits
func locateRealGit(ctx context.Context) (string, bool) {
	var candidates []string
	for _, base := range []string{`C:\Program Files`, `C:\Program Files (x86)`} {
		for _, arch := range []string{"clangarm64", "mingw64", "mingw32"} {
			candidates = append(candidates, filepath.Join(base, "Git", arch, "libexec", "git-core", "git.exe"))
		}
	}
	candidates = append(candidates, portableGitCandidates(ctx)...)
	for _, candidate := range candidates {
		if !statExists(candidate) {
			continue
		}
		if guard, ok := probeForkBomb(ctx, []string{candidate}); ok && !guard {
			return candidate, true
		}
	}
	return "", false
}

// Swaps a broken Git for Windows trampoline for a real git binary, the command untouched when none is found
func ensureNonTrampolineGit(ctx context.Context, w io.Writer, gitCmd []string) []string {
	if !custodyWindows() || !gitIsTrampoline(ctx, gitCmd) {
		return gitCmd
	}
	realGit, found := locateRealGit(ctx)
	if !found {
		fmt.Fprintln(w, "⚠ Detected a broken git trampoline and could not locate a real git binary — the update will fall back to the ZIP path.")
		return gitCmd
	}
	fmt.Fprintf(w, "⚠ Detected a broken git trampoline; switching to real git at %s\n", realGit)
	return append([]string{realGit}, gitCmd[min(1, len(gitCmd)):]...)
}

// Manifest directories whose specs the root package-lock.json records, the root and every workspace
func npmLockfileOwners(repoRoot string) (map[string]bool, error) {
	owners := map[string]bool{manifestDirKey("."): true}
	raw, err := os.ReadFile(filepath.Join(repoRoot, "package.json"))
	if err != nil {
		if pypath.IsOSError(err) {
			return owners, nil
		}
		return nil, err
	}
	text, err := pystr.DecodeUTF8Sig(raw)
	if err != nil {
		return owners, nil
	}
	parsed, err := pyjson.Loads(text)
	if err != nil {
		return owners, nil
	}
	pkg, ok := pyjson.AsObject(parsed)
	if !ok {
		return nil, &pm.PyError{Class: "AttributeError", Message: "'" + pyrepr.TypeName(parsed) + "' object has no attribute 'get'"}
	}
	var workspaces any = []any{}
	if value, set := pkg.Vals["workspaces"]; set {
		workspaces = value
	}
	if nested, isObject := pyjson.AsObject(workspaces); isObject {
		workspaces = []any{}
		if value, set := nested.Vals["packages"]; set {
			workspaces = value
		}
	}
	patterns, ok := workspaces.([]any)
	if !ok {
		return owners, nil
	}
	for _, pattern := range patterns {
		matches, err := pypath.Glob(repoRoot, pyrepr.Str(pattern))
		if err != nil {
			continue
		}
		for _, directory := range matches {
			if !isRegular(filepath.Join(directory, "package.json")) {
				continue
			}
			rel, err := filepath.Rel(repoRoot, directory)
			if err != nil {
				break
			}
			owners[manifestDirKey(rel)] = true
		}
	}
	return owners, nil
}

// Comparison key of a manifest directory the way two pathlib paths compare equal
func manifestDirKey(dir string) string {
	return pypath.Normcase(pypath.PathStr(dir))
}

// The checkout move of a run with no paused gateways, the step running unbound
func noMove(context.Context, checkoutMoveBind) (map[string]any, func() error, error) {
	return map[string]any{}, func() error { return nil }, nil
}

// Restores package-lock.json files npm rewrote unless a manifest the lockfile records is dirty
func discardLockfileChurn(ctx context.Context, w io.Writer, gitCmd []string, repoRoot string, move checkoutMoveFunc) {
	diff, err := gitRunCaptured(ctx, gitCmd, []string{"diff", "--name-only"}, repoRoot, false, nil)
	if err != nil || diff.ReturnCode != 0 {
		return
	}
	var changed []string
	dirtyManifests := map[string]bool{}
	for _, line := range pystr.Splitlines(diff.Stdout, false) {
		path := pystr.Strip(line)
		changed = append(changed, path)
		if strings.HasSuffix(path, "package.json") {
			dirtyManifests[manifestDirKey(pypath.PathParent(path))] = true
		}
	}
	rootOwners, err := npmLockfileOwners(repoRoot)
	if err != nil {
		return
	}
	rootDirty := false
	for owner := range rootOwners {
		if dirtyManifests[owner] {
			rootDirty = true
		}
	}
	var dirty []string
	for _, path := range changed {
		if !strings.HasSuffix(path, "package-lock.json") {
			continue
		}
		lockDir := manifestDirKey(pypath.PathParent(path))
		atRoot := lockDir == manifestDirKey(".")
		if (atRoot && rootDirty) || (!atRoot && dirtyManifests[lockDir]) {
			continue
		}
		dirty = append(dirty, path)
	}
	if len(dirty) == 0 {
		return
	}
	err = withCheckoutMove(ctx, move, checkoutMoveBind{Paths: dirty}, func(map[string]any) error {
		_, err := gitRunCaptured(ctx, gitCmd, append([]string{"checkout", "--"}, dirty...), repoRoot, false, nil)
		return err
	})
	if err != nil {
		return
	}
	fmt.Fprintf(w, "→ Discarded npm lockfile churn (%d file(s))\n", len(dirty))
}

// Takes a managed checkout off core.autocrlf=true once its line ending churn is cleared, leaving it clean
func normalizeManagedEOL(ctx context.Context, w io.Writer, gitCmd []string, repoRoot string, move checkoutMoveFunc) {
	probe := append(slices.Clone(gitCmd), "-c", "core.autocrlf=false")
	probeRun := func(input *string, args ...string) (*CompletedProcess, error) {
		return RunGit(ctx, probe, args, ChildOptions{Dir: repoRoot, Input: input, Capture: true, Text: true, Attr: tools.WindowsHideAttr()})
	}
	eolOnly := func() (paths []string, known bool, err error) {
		allDirty, err := probeRun(nil, "diff", "-z", "--name-only")
		if err != nil {
			return nil, false, err
		}
		realDirty, err := probeRun(nil, "-c", "core.quotepath=false", "diff", "--numstat", "--ignore-cr-at-eol")
		if err != nil {
			return nil, false, err
		}
		if allDirty.ReturnCode != 0 || realDirty.ReturnCode != 0 {
			return nil, false, nil
		}
		content := map[string]bool{}
		for _, line := range pystr.Splitlines(realDirty.Stdout, false) {
			if pystr.Strip(line) == "" {
				continue
			}
			if parts := strings.SplitN(line, "\t", 3); len(parts) == 3 && parts[2] != "" {
				content[parts[2]] = true
			}
		}
		seen := map[string]bool{}
		for _, path := range strings.Split(allDirty.Stdout, "\x00") {
			if path != "" && !content[path] && !seen[path] {
				seen[path] = true
				paths = append(paths, path)
			}
		}
		sort.Strings(paths)
		return paths, true, nil
	}
	autocrlf, err := gitRunCaptured(ctx, gitCmd, []string{"config", "--get", "core.autocrlf"}, repoRoot, false, nil)
	if err != nil || pystr.Lower(pystr.Strip(autocrlf.Stdout)) != "true" {
		return
	}
	churn, known, err := eolOnly()
	if err != nil || !known {
		return
	}
	if len(churn) > 0 {
		err := withCheckoutMove(ctx, move, checkoutMoveBind{Paths: churn}, func(map[string]any) error {
			pathspec := strings.Join(churn, "\x00")
			_, err := probeRun(&pathspec, "checkout", "--pathspec-from-file=-", "--pathspec-file-nul", "--")
			return err
		})
		if err != nil {
			return
		}
		left, _, err := eolOnly()
		if err != nil || len(left) > 0 {
			return
		}
		fmt.Fprintf(w, "→ Normalized line-ending churn (%d file(s))\n", len(churn))
	}
	_, _ = RunGit(ctx, gitCmd, []string{"config", "core.autocrlf", "false"}, ChildOptions{Dir: repoRoot, Capture: true, Attr: tools.WindowsHideAttr()})
}
