package selfupdate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

const (
	autostashNamePrefix  = "icebath-update-autostash-"
	autostashWarnAgeDays = 7
	stashLeftInPlace     = "  The stash was left in place. You can remove it manually after checking the result."
)

// Stash commit and stashed path count of a run's autostash until it is settled
type pendingStash struct {
	ref   string
	count int
}

var pendingAutostash *pendingStash

var stashSelectorRE = regexp.MustCompile(`^stash@\{(\p{Nd}+)\}$`)

var nonRuntimeSuffixes = map[string]bool{
	".md": true, ".mdx": true, ".rst": true, ".txt": true, ".adoc": true,
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".svg": true, ".webp": true, ".ico": true,
}

// Runs a git command with captured output, nil when git cannot run
func gitQuiet(ctx context.Context, gitCmd, args []string, cwd string) (*CompletedProcess, error) {
	done, err := RunGit(ctx, gitCmd, args, ChildOptions{Dir: cwd, Capture: true})
	if err != nil {
		if isUpdateOSError(err) || isSubprocessError(err) {
			return nil, nil
		}
		return nil, err
	}
	return done, nil
}

// Paths recorded with git add -N, read from NUL separated porcelain output
func intentToAddPaths(porcelainZ string) []string {
	var paths []string
	for _, record := range strings.Split(porcelainZ, "\x00") {
		if utf8.RuneCountInString(record) > 3 && strings.HasPrefix(record, " A") {
			paths = append(paths, dropRunes(record, 3))
		}
	}
	return paths
}

// s without its first n code points
func dropRunes(s string, n int) string {
	for i := range s {
		if n == 0 {
			return s[i:]
		}
		n--
	}
	return ""
}

// File system bytes of a path decoded with surrogateescape
func fsEncode(path string) string {
	raw, err := pystr.SurrogateEscapeEncode(path)
	if err != nil {
		return path
	}
	return string(raw)
}

// NUL separated path listing as a set of surrogateescape decoded names, false when git failed
func gitPathsZ(ctx context.Context, gitCmd, args []string, cwd string) (map[string]bool, bool, error) {
	result, err := gitQuiet(ctx, gitCmd, args, cwd)
	if err != nil {
		return nil, false, err
	}
	if result == nil || result.ReturnCode != 0 {
		return nil, false, nil
	}
	paths := map[string]bool{}
	for _, path := range strings.Split(pystr.TranslateNewlines(pystr.SurrogateEscapeDecode([]byte(result.Stdout))), "\x00") {
		if path != "" {
			paths[path] = true
		}
	}
	return paths, true, nil
}

// Resets every tracked file of the checkout to HEAD
func resetHard(ctx context.Context, gitCmd []string, cwd string) error {
	_, err := RunGit(ctx, gitCmd, []string{"reset", "--hard", "HEAD"}, ChildOptions{Dir: cwd, Capture: true})
	return err
}

// Prints stripped text behind prefix unless it is blank
func printNonempty(w io.Writer, text, prefix string) {
	if stripped := pystr.Strip(text); stripped != "" {
		fmt.Fprintln(w, prefix+stripped)
	}
}

// Prints the first line of stripped text indented unless it is blank
func printFirstLine(w io.Writer, text string) {
	if stripped := pystr.Strip(text); stripped != "" {
		fmt.Fprintln(w, "  "+pystr.Splitlines(stripped, false)[0])
	}
}

// Records of status --porcelain -z, the second field of a rename or copy skipped
func porcelainRecords(porcelainZ string) []string {
	fields := strings.Split(porcelainZ, "\x00")
	var records []string
	for i := 0; i < len(fields); {
		if utf8.RuneCountInString(fields[i]) > 3 {
			records = append(records, fields[i])
			if fields[i][0] == 'R' || fields[i][0] == 'C' {
				i += 2
				continue
			}
		}
		i++
	}
	return records
}

// Casefolded files and directories the update can write, unioned over every ref that resolves, false when unknown
func targetTreePaths(ctx context.Context, gitCmd []string, cwd string, refs []string) (files, dirs map[string]bool, known bool, err error) {
	files = map[string]bool{}
	readAny := false
	seen := map[string]bool{}
	for _, ref := range refs {
		if seen[ref] {
			continue
		}
		seen[ref] = true
		resolved, err := gitQuiet(ctx, gitCmd, []string{"rev-parse", "-q", "--verify", ref + "^{tree}"}, cwd)
		if err != nil {
			return nil, nil, false, err
		}
		if resolved == nil {
			return nil, nil, false, nil
		}
		if resolved.ReturnCode != 0 {
			continue
		}
		paths, ok, err := gitPathsZ(ctx, gitCmd, []string{"ls-tree", "-r", "--name-only", "-z", ref}, cwd)
		if err != nil || !ok {
			return nil, nil, false, err
		}
		for path := range paths {
			files[pystr.Casefold(path)] = true
		}
		readAny = true
	}
	if !readAny {
		return nil, nil, false, nil
	}
	dirs = map[string]bool{}
	for path := range files {
		for i := 0; i < len(path); i++ {
			if path[i] == '/' {
				dirs[path[:i]] = true
			}
		}
	}
	return files, dirs, true, nil
}

// Whether the update could write where an untracked directory record or file sits
func untrackedCollides(entry string, files, dirs map[string]bool) bool {
	path := pystr.Casefold(strings.TrimRight(entry, "/"))
	if files[path] || dirs[path] {
		return true
	}
	parts := strings.Split(path, "/")
	for i := 1; i < len(parts); i++ {
		if files[strings.Join(parts[:i], "/")] {
			return true
		}
	}
	return false
}

// What git status shows, split by what the update has to move out of its way
type localChanges struct {
	porcelain      string
	tracked        bool
	stashUntracked []string
	leaveUntracked []string
}

// Whether a tracked change or a colliding untracked entry has to be moved
func (c *localChanges) needsMoving() bool { return c.tracked || len(c.stashUntracked) > 0 }

// Local changes of the checkout, nil when the tree is clean, untracked entries scoped to the target trees
func scanLocalChanges(ctx context.Context, gitCmd []string, cwd string, untrackedTargets []string) (*localChanges, error) {
	status, err := gitRunCaptured(ctx, gitCmd, []string{"status", "--porcelain", "-z", "--untracked-files=normal"}, cwd, true, nil)
	if err != nil {
		return nil, err
	}
	if pystr.Strip(status.Stdout) == "" {
		return nil, nil
	}
	records := porcelainRecords(status.Stdout)
	var untracked []string
	for _, record := range records {
		if strings.HasPrefix(record, "?? ") {
			untracked = append(untracked, record[3:])
		}
	}
	var files, dirs map[string]bool
	scoped := false
	if len(untrackedTargets) > 0 && len(untracked) > 0 {
		if files, dirs, scoped, err = targetTreePaths(ctx, gitCmd, cwd, untrackedTargets); err != nil {
			return nil, err
		}
	}
	changes := &localChanges{porcelain: status.Stdout, tracked: len(untracked) < len(records)}
	colliding := map[string]bool{}
	for _, entry := range untracked {
		if !scoped || untrackedCollides(entry, files, dirs) {
			changes.stashUntracked = append(changes.stashUntracked, entry)
			colliding[entry] = true
		}
	}
	for _, entry := range untracked {
		if !colliding[entry] {
			changes.leaveUntracked = append(changes.leaveUntracked, entry)
		}
	}
	return changes, nil
}

// True unless targetRef is HEAD or an ancestor of it, an unresolvable target answering true
func targetWouldMoveHead(ctx context.Context, gitCmd []string, cwd, targetRef string) (bool, error) {
	probe, err := gitQuiet(ctx, gitCmd, []string{"merge-base", "--is-ancestor", targetRef + "^{commit}", "HEAD"}, cwd)
	if err != nil {
		return false, err
	}
	return probe == nil || probe.ReturnCode != 0, nil
}

// Pathspec matching path byte for byte relative to the checkout root
func literal(path string, magic ...string) string {
	return ":(" + strings.Join(append([]string{"literal"}, magic...), ",") + ")" + path
}

// Asks before anything moves whether to keep the local changes, closed stdin keeping them
func askKeepLocalChanges(w io.Writer, changes *localChanges) (bool, error) {
	fmt.Fprintln(w, "→ Local changes detected:")
	tracked := "none"
	if changes.tracked {
		tracked = "yes"
	}
	fmt.Fprintf(w, "  • tracked file edits: %s\n", tracked)
	fmt.Fprintf(w, "  • untracked files the update would overwrite: %d\n", len(changes.stashUntracked))
	if n := len(changes.leaveUntracked); n > 0 {
		noun := "ies stay"
		if n == 1 {
			noun = "y stays"
		}
		fmt.Fprintf(w, "  (%d other untracked entr%s in place either way.)\n", n, noun)
	}
	fmt.Fprintln(w, "Keep these changes after updating? [Y/n]  (n deletes them now)")
	response, err := updateInput("")
	if err != nil {
		if errors.Is(err, cliui.ErrInterrupt) {
			return false, err
		}
		response = ""
	}
	response = pystr.Lower(pystr.Strip(response))
	return response != "n" && response != "no", nil
}

// Deletes exactly what would have been stashed, tracked edits and the colliding untracked entries
func discardLocalChangesBeforeUpdate(ctx context.Context, w io.Writer, gitCmd []string, cwd string, changes *localChanges, move checkoutMoveFunc) error {
	err := withCheckoutMove(ctx, move, checkoutMoveBind{Revert: true}, func(map[string]any) error {
		if changes.tracked {
			if _, err := gitRunCaptured(ctx, gitCmd, []string{"reset", "--hard", "HEAD"}, cwd, true, nil); err != nil {
				return err
			}
		}
		if len(changes.stashUntracked) > 0 {
			args := []string{"clean", "-f", "-d", "--"}
			for _, path := range changes.stashUntracked {
				args = append(args, literal(path))
			}
			if _, err := gitRunCaptured(ctx, gitCmd, args, cwd, true, nil); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	fmt.Fprintln(w, "→ Discarded local changes (you chose not to keep them).")
	updaterecord.RecordStep(ctx, "local_changes_stash", true, "discarded before update (user declined keeping them)")
	return nil
}

// Stashes local changes before the update moves the checkout, the stash commit or nil when nothing needs moving
func stashLocalChangesIfNeeded(ctx context.Context, w io.Writer, gitCmd []string, cwd string, move checkoutMoveFunc,
	untrackedTargets []string, askFirst bool) (*string, error) {
	pendingAutostash = nil
	changes, err := scanLocalChanges(ctx, gitCmd, cwd, untrackedTargets)
	if err != nil || changes == nil {
		return nil, err
	}
	if n := len(changes.leaveUntracked); n > 0 {
		noun := "ies"
		if n == 1 {
			noun = "y"
		}
		fmt.Fprintf(w, "→ Leaving %d untracked entr%s in place (the update does not touch them).\n", n, noun)
	}
	if !changes.needsMoving() {
		return nil, nil
	}
	if askFirst {
		keep, err := askKeepLocalChanges(w, changes)
		if err != nil {
			return nil, err
		}
		if !keep {
			return nil, discardLocalChangesBeforeUpdate(ctx, w, gitCmd, cwd, changes, move)
		}
	}
	unmerged, err := gitRunCaptured(ctx, gitCmd, []string{"ls-files", "--unmerged"}, cwd, false, nil)
	if err != nil {
		return nil, err
	}
	if pystr.Strip(unmerged.Stdout) != "" {
		fmt.Fprintln(w, "→ Clearing unmerged index entries from a previous conflict...")
		if _, err := gitQuiet(ctx, gitCmd, []string{"reset"}, cwd); err != nil {
			return nil, err
		}
	}
	if intentToAdd := intentToAddPaths(changes.porcelain); len(intentToAdd) > 0 {
		fmt.Fprintf(w, "→ Making %d intent-to-add file(s) stashable...\n", len(intentToAdd))
		add, err := gitRunCaptured(ctx, gitCmd, append([]string{"add", "--"}, intentToAdd...), cwd, false, nil)
		if err != nil {
			return nil, err
		}
		if add.ReturnCode != 0 {
			printNonempty(w, add.Stderr, "")
		}
	}
	stashName := autostashNamePrefix + time.Now().UTC().Format("20060102-150405")
	fmt.Fprintln(w, "→ Local changes detected — stashing before update...")
	prev, err := gitRunCaptured(ctx, gitCmd, []string{"rev-parse", "--verify", "refs/stash"}, cwd, false, nil)
	if err != nil {
		return nil, err
	}
	prevStash := pystr.Strip(prev.Stdout)
	stashRef := ""
	err = withCheckoutMove(ctx, move, checkoutMoveBind{Revert: true}, func(map[string]any) error {
		ref, err := pushStash(ctx, w, gitCmd, cwd, stashName, prevStash, changes.leaveUntracked)
		stashRef = ref
		return err
	})
	if err != nil {
		return nil, err
	}
	pendingAutostash = &pendingStash{ref: stashRef, count: len(porcelainRecords(changes.porcelain)) - len(changes.leaveUntracked)}
	return &stashRef, nil
}

// Pushes the stash and returns its commit, failing when nothing was saved and resetting tracked leftovers
func pushStash(ctx context.Context, w io.Writer, gitCmd []string, cwd, stashName, prevStash string, leave []string) (string, error) {
	args := []string{"stash", "push", "--include-untracked", "-m", stashName}
	if len(leave) > 0 {
		args = append(args, "--", ".")
		for _, path := range leave {
			args = append(args, literal(path, "exclude"))
		}
	}
	push, err := gitRunCaptured(ctx, gitCmd, args, cwd, false, nil)
	if err != nil {
		return "", err
	}
	printNonempty(w, push.Stdout, "")
	probe, err := gitRunCaptured(ctx, gitCmd, []string{"rev-parse", "--verify", "refs/stash"}, cwd, false, nil)
	if err != nil {
		return "", err
	}
	stashRef := pystr.Strip(probe.Stdout)
	created := probe.ReturnCode == 0 && stashRef != "" && stashRef != prevStash
	if push.ReturnCode != 0 {
		if !created {
			fmt.Fprintln(w, "✗ Could not stash local changes — update aborted.")
			printFirstLine(w, push.Stderr)
			fmt.Fprintln(w, "  Commit, stash, or clean up your local changes manually, then re-run `icebath update`.")
			fmt.Fprintln(w, "  (An index entry from `git add -N` is the usual cause of this error; `git add` the")
			fmt.Fprintln(w, "   paths it names, or `git reset` them, and the update will proceed.)")
			return "", &CalledProcessError{ReturnCode: push.ReturnCode, Cmd: push.Args, Output: push.Stdout, Stderr: push.Stderr}
		}
		printNonempty(w, push.Stderr, "")
		fmt.Fprintln(w, "  ⚠ Some untracked files could not be removed from the working tree (permission denied).")
		fmt.Fprintln(w, "    They were still saved to the stash and were left in place — the update will continue.")
		if err := resetHard(ctx, gitCmd, cwd); err != nil {
			return "", err
		}
	}
	return stashRef, nil
}

// Bare index selector of the stash entry whose commit is stashRef, false when no entry holds it
func resolveStashSelector(ctx context.Context, gitCmd []string, cwd, stashRef string) (string, bool, error) {
	listed, err := gitRunCaptured(ctx, gitCmd, []string{"stash", "list", "--format=%gd %H"}, cwd, true, nil)
	if err != nil {
		return "", false, err
	}
	for _, line := range pystr.Splitlines(listed.Stdout, false) {
		selector, commit, _ := strings.Cut(line, " ")
		if pystr.Strip(commit) != stashRef {
			continue
		}
		selector = pystr.Strip(selector)
		if match := stashSelectorRE.FindStringSubmatch(selector); match != nil {
			return match[1], true, nil
		}
		return selector, true, nil
	}
	return "", false, nil
}

// Forgets this run's unsettled autostash after it was already reported
func clearPendingAutostash() { pendingAutostash = nil }

// What to tell the user while this run's autostash is unsettled, false when it is settled
func unrestoredAutostashNotice() (string, bool) {
	if pendingAutostash == nil {
		return "", false
	}
	ref := pendingAutostash.ref
	return fmt.Sprintf("⚠ icebath update stashed %d local modification(s) and did NOT restore them.\n"+
		"  Stash ref: %s\n  Review with: git stash show --stat %s\n  Re-apply with: git stash show -p %s | git apply --3way",
		pendingAutostash.count, ref, ref, ref), true
}

// Prints a notice for update autostashes older than the warn threshold and returns their count
func warnOrphanedUpdateAutostashes(ctx context.Context, w io.Writer, gitCmd []string, cwd string) int {
	listed, err := gitRunCaptured(ctx, gitCmd, []string{"stash", "list", "--format=%gd %s"}, cwd, false, nil)
	if err != nil {
		updateLog.Debug(fmt.Sprintf("Autostash age check failed: %s", launchErrorText(err)))
		return 0
	}
	if listed.ReturnCode != 0 {
		return 0
	}
	cutoff := time.Now().UTC().Add(-autostashWarnAgeDays * 24 * time.Hour)
	var stale [][2]string
	for _, line := range pystr.Splitlines(listed.Stdout, false) {
		selector, subject, _ := strings.Cut(pystr.Strip(line), " ")
		pos := strings.Index(subject, autostashNamePrefix)
		if pos < 0 {
			continue
		}
		stamp := clipRunes(subject[pos+len(autostashNamePrefix):], 15)
		stashTime, _, err := pydatetime.Strptime(stamp, "%Y%m%d-%H%M%S")
		if err != nil {
			continue
		}
		if utcWall(stashTime).Before(cutoff) {
			stale = append(stale, [2]string{selector, stamp})
		}
	}
	if len(stale) == 0 {
		return 0
	}
	noun := "ies are"
	if len(stale) == 1 {
		noun = "y is"
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "⚠ %d leftover update autostash entr%s more than %d days old:\n", len(stale), noun, autostashWarnAgeDays)
	for _, entry := range stale {
		fmt.Fprintf(w, "    %s  (%s%s)\n", entry[0], autostashNamePrefix, entry[1])
	}
	fmt.Fprintln(w, "  These hold local changes stashed by earlier updates and never")
	fmt.Fprintln(w, "  restored. Review with: git stash show -p <entry>")
	fmt.Fprintln(w, "  Restore with: git stash apply <entry>   Discard with: git stash drop <entry>")
	return len(stale)
}

// Notes the autostash disposition in the update receipt, a park the user did not choose staying unsettled
func recordStashDisposition(ctx context.Context, outcome, stashRef, detail string, chosen bool) {
	if outcome != "parked" || chosen {
		pendingAutostash = nil
	}
	text := outcome + ": " + stashRef
	if detail != "" {
		text += " (" + detail + ")"
	}
	updaterecord.RecordStep(ctx, "local_changes_stash", outcome != "parked", text)
}

// Prints how to find and drop a stash entry left in place
func printStashCleanupGuidance(w io.Writer, stashRef, stashSelector string) {
	fmt.Fprintln(w, "  Check `git status` first so you don't accidentally reapply the same change twice.")
	io.WriteString(w, "  Find the saved entry with: git stash list --format='%gd %H %s'\n")
	if stashSelector != "" {
		fmt.Fprintf(w, "  Remove it with: git stash drop %s\n", stashSelector)
	} else {
		fmt.Fprintf(w, "  Look for commit %s, then drop it by index with: git stash drop <N>\n", stashRef)
	}
}

// True when a stash apply failure is only about untracked files that already exist in the tree
func stashApplyFailedOnlyOnExistingUntracked(stderr string) bool {
	sawUntrackedError := false
	for _, raw := range pystr.Splitlines(stderr, false) {
		line := pystr.Strip(raw)
		if line == "" {
			continue
		}
		if strings.Contains(line, "already exists, no checkout") || strings.Contains(line, "could not restore untracked files from stash") {
			sawUntrackedError = true
		} else if !strings.HasPrefix(line, "warning:") && !strings.HasPrefix(line, "hint:") {
			return false
		}
	}
	return sawUntrackedError
}

// Paths stash apply refused because the update now tracks its own file where the stash holds an untracked one
func untrackedFilesReplacedByUpdate(ctx context.Context, gitCmd []string, cwd, stashRef, stderr string) ([]string, error) {
	var replaced []string
	const suffix = " already exists, no checkout"
	for _, raw := range pystr.Splitlines(stderr, false) {
		line := pystr.Strip(raw)
		if !strings.HasSuffix(line, suffix) {
			continue
		}
		rel := strings.TrimSuffix(line, suffix)
		tracked, err := RunGit(ctx, gitCmd, []string{"cat-file", "-e", "HEAD:" + rel}, ChildOptions{Dir: cwd, Capture: true})
		if err != nil {
			return nil, err
		}
		if tracked.ReturnCode != 0 {
			continue
		}
		stashed, err := RunGit(ctx, gitCmd, []string{"show", stashRef + "^3:" + rel}, ChildOptions{Dir: cwd, Capture: true})
		if err != nil {
			return nil, err
		}
		current, readErr := os.ReadFile(pypath.Join(cwd, rel))
		if stashed.ReturnCode != 0 || readErr != nil || !bytes.Equal(current, []byte(stashed.Stdout)) {
			replaced = append(replaced, rel)
		}
	}
	return replaced, nil
}

// Leaves a pre update autostash parked for keep-stash runs and says how to restore it
func parkStashedChanges(ctx context.Context, w io.Writer, stashRef string) {
	fmt.Fprintln(w)
	fmt.Fprintln(w, "ℹ️  Local changes were stashed before updating and were NOT re-applied (--keep-stash).")
	fmt.Fprintf(w, "  Stash ref: %s\n", stashRef)
	fmt.Fprintf(w, "  Restore manually with: git stash apply %s\n", stashRef)
	recordStashDisposition(ctx, "parked", stashRef, "--keep-stash", true)
}

// Untracked paths of the checkout, false with a printed warning when git cannot enumerate them
func gitUntrackedPaths(ctx context.Context, w io.Writer, gitCmd []string, cwd string) (map[string]bool, bool, error) {
	paths, ok, err := gitPathsZ(ctx, gitCmd, []string{"ls-files", "--others", "--exclude-standard", "-z"}, cwd)
	if err != nil {
		return nil, false, err
	}
	if !ok {
		fmt.Fprintln(w, "  ⚠ Could not enumerate untracked files while validating the restored stash.")
	}
	return paths, ok, nil
}

// Restored Python paths changed from HEAD in sorted order, false when git cannot enumerate them
func restoredPythonPaths(ctx context.Context, w io.Writer, gitCmd []string, cwd string) ([]string, bool, error) {
	paths, ok, err := gitPathsZ(ctx, gitCmd, []string{"diff", "--name-only", "-z", "HEAD", "--", "*.py"}, cwd)
	if err != nil {
		return nil, false, err
	}
	if !ok {
		fmt.Fprintln(w, "  ⚠ Could not enumerate tracked Python files restored from the stash.")
		return nil, false, nil
	}
	untracked, ok, err := gitUntrackedPaths(ctx, w, gitCmd, cwd)
	if err != nil || !ok {
		return nil, false, err
	}
	for path := range untracked {
		if strings.HasSuffix(path, ".py") {
			paths[path] = true
		}
	}
	return sortedKeys(paths), true, nil
}

// Keys of a set in code point order
func sortedKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// True only when every path the stash restores is demonstrably unable to change an import
func restoreIsNonRuntime(ctx context.Context, gitCmd []string, cwd, stashRef string) (bool, error) {
	restored := map[string]bool{}
	for _, side := range []string{stashRef, stashRef + "^2"} {
		changed, ok, err := gitPathsZ(ctx, gitCmd, []string{"diff", "--name-only", "--no-renames", "-z", stashRef + "^1", side, "--"}, cwd)
		if err != nil || !ok {
			return false, err
		}
		for path := range changed {
			restored[path] = true
		}
	}
	hasUntracked, err := gitQuiet(ctx, gitCmd, []string{"rev-parse", "-q", "--verify", stashRef + "^3"}, cwd)
	if err != nil || hasUntracked == nil {
		return false, err
	}
	if hasUntracked.ReturnCode == 0 {
		untracked, ok, err := gitPathsZ(ctx, gitCmd, []string{"ls-tree", "-r", "--name-only", "-z", stashRef + "^3"}, cwd)
		if err != nil || !ok {
			return false, err
		}
		for path := range untracked {
			restored[path] = true
		}
	}
	if len(restored) == 0 {
		return true, nil
	}
	headDirs, ok, err := gitPathsZ(ctx, gitCmd, []string{"ls-tree", "-r", "-d", "--name-only", "-z", "HEAD"}, cwd)
	if err != nil || !ok {
		return false, err
	}
	for path := range restored {
		parent := pypath.PathAsPosix(pypath.PathParent(path))
		if !nonRuntimeSuffixes[pystr.Lower(pypath.PathSuffix(path))] || isSymlink(pypath.Join(cwd, fsEncode(path))) ||
			(parent != "." && !headDirs[parent]) {
			return false, nil
		}
	}
	return true, nil
}

// Whether path itself is a symbolic link
func isSymlink(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode()&os.ModeSymlink != 0
}

// Restores the clean updated tree, keeps the stash and stops the update with exit status 1
func rejectUnsafeStashRestore(ctx context.Context, w io.Writer, gitCmd []string, cwd, stashRef string, preexistingUntracked map[string]bool,
	failingTarget, detail string, move checkoutMoveFunc) error {
	fmt.Fprintln(w)
	fmt.Fprintln(w, "✗ Restored local changes made the Icebath agent unexecutable.")
	fmt.Fprintf(w, "  Health check failed: %s\n", failingTarget)
	if detail != "" {
		lines := pystr.Splitlines(detail, false)
		for _, line := range lines[:min(len(lines), 6)] {
			fmt.Fprintf(w, "    %s\n", line)
		}
	}
	succeeded := func(result *CompletedProcess) bool { return result != nil && result.ReturnCode == 0 }
	currentUntracked, enumerated, err := gitUntrackedPaths(ctx, w, gitCmd, cwd)
	if err != nil {
		return err
	}
	var restoredUntracked []string
	for _, path := range sortedKeys(currentUntracked) {
		if !preexistingUntracked[path] {
			restoredUntracked = append(restoredUntracked, fsEncode(path))
		}
	}
	var reset, clean *CompletedProcess
	err = withCheckoutMove(ctx, move, checkoutMoveBind{Revert: true}, func(map[string]any) error {
		var err error
		if reset, err = gitQuiet(ctx, gitCmd, []string{"reset", "--hard", "HEAD"}, cwd); err != nil {
			return err
		}
		if len(restoredUntracked) > 0 {
			clean, err = gitQuiet(ctx, gitCmd, append([]string{"clean", "-fd", "--"}, restoredUntracked...), cwd)
		}
		return err
	})
	if err != nil {
		return err
	}
	cleanupOK := enumerated && succeeded(reset) && (len(restoredUntracked) == 0 || succeeded(clean))
	if cleanupOK {
		diff, err := gitQuiet(ctx, gitCmd, []string{"diff", "--quiet", "HEAD", "--"}, cwd)
		if err != nil {
			return err
		}
		cleanupOK = succeeded(diff)
	}
	if cleanupOK {
		fmt.Fprintln(w, "  The clean updated tree has been restored; the gateway was not restarted.")
	} else {
		fmt.Fprintln(w, "  ⚠ The clean updated tree could not be fully restored automatically.")
		fmt.Fprintln(w, "    Inspect `git status` and run `git reset --hard HEAD` before retrying.")
	}
	fmt.Fprintln(w, "  Platform connectivity alone does not mean the agent can execute turns.")
	fmt.Fprintf(w, "  Your local changes remain preserved in stash: %s\n", stashRef)
	fmt.Fprintf(w, "  Inspect them with: git stash show --stat %s\n", stashRef)
	fmt.Fprintf(w, "  Restore manually after fixing them: git stash apply %s\n", stashRef)
	clearPendingAutostash()
	recordStop(ctx, "stash_restore_rejected")
	return &pm.SystemExit{Code: 1}
}

// Asks whether to restore the stash, a remote prompt defaulting to no and the local prompt to yes
func confirmRestore(w io.Writer, stashRef string, inputFn func(prompt, def string) (string, error)) (bool, error) {
	remotePrompt := inputFn != nil
	promptSuffix := "[Y/n]"
	if remotePrompt {
		promptSuffix = "[y/N]"
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "⚠ Local changes were stashed before updating.")
	fmt.Fprintln(w, "  Restoring them may reapply local customizations onto the updated codebase.")
	fmt.Fprintln(w, "  Review the result afterward if Icebath behaves unexpectedly.")
	fmt.Fprintf(w, "Restore local changes now? %s\n", promptSuffix)
	var response string
	if remotePrompt {
		answer, err := inputFn("Restore local changes now? "+promptSuffix, "n")
		if err != nil {
			return false, err
		}
		response = answer
	} else {
		answer, err := updateInput("")
		switch {
		case errors.Is(err, cliui.ErrInterrupt):
			return false, err
		case err != nil:
			response = "n"
		default:
			response = pystr.Lower(pystr.Strip(answer))
		}
	}
	if response == "y" || response == "yes" || (!remotePrompt && response == "") {
		return true, nil
	}
	fmt.Fprintln(w, "Skipped restoring local changes.")
	fmt.Fprintln(w, "Your changes are still preserved in git stash.")
	fmt.Fprintf(w, "Restore manually with: git stash apply %s\n", stashRef)
	return false, nil
}

// Applies the stash, returning the untracked paths the update replaced, applied false on conflicts or failure
func applyStash(ctx context.Context, w io.Writer, gitCmd []string, cwd, stashRef string, move checkoutMoveFunc) (replaced []string, applied bool, err error) {
	fmt.Fprintln(w, "→ Restoring local changes...")
	var restore *CompletedProcess
	var conflictedFiles string
	whole := false
	err = withCheckoutMove(ctx, move, checkoutMoveBind{Targets: []string{stashRef}}, func(step map[string]any) error {
		var err error
		if restore, conflictedFiles, err = applyStashStep(ctx, gitCmd, cwd, stashRef); err != nil {
			return err
		}
		whole = conflictedFiles == "" && (restore.ReturnCode == 0 || stashApplyFailedOnlyOnExistingUntracked(restore.Stderr))
		step["whole"] = whole
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	if whole && restore.ReturnCode == 0 {
		return []string{}, true, nil
	}
	if whole {
		replaced, err := untrackedFilesReplacedByUpdate(ctx, gitCmd, cwd, stashRef, restore.Stderr)
		if err != nil {
			return nil, false, err
		}
		if len(replaced) == 0 {
			fmt.Fprintln(w, "  ⚠ Some stashed untracked files already exist in the working tree and were kept as-is.")
		}
		return replaced, true, nil
	}
	fmt.Fprintln(w, "✗ Update pulled new code, but restoring local changes hit conflicts.")
	printNonempty(w, restore.Stdout, "")
	printNonempty(w, restore.Stderr, "")
	if conflictedFiles != "" {
		fmt.Fprintln(w, "\nConflicted files:")
		for _, file := range pystr.Splitlines(conflictedFiles, false) {
			fmt.Fprintf(w, "  • %s\n", file)
		}
	}
	fmt.Fprintln(w, "\nYour stashed changes are preserved — nothing is lost.")
	fmt.Fprintf(w, "  Stash ref: %s\n", stashRef)
	fmt.Fprintln(w, "Working tree reset to clean state.")
	fmt.Fprintf(w, "Restore your changes later with: git stash apply %s\n", stashRef)
	recordStashDisposition(ctx, "parked", stashRef, "restore hit conflicts", false)
	return nil, false, nil
}

// Runs stash apply and reports its conflicted paths, resetting to the clean tree on any real failure
func applyStashStep(ctx context.Context, gitCmd []string, cwd, stashRef string) (*CompletedProcess, string, error) {
	restore, err := gitRunCaptured(ctx, gitCmd, []string{"stash", "apply", stashRef}, cwd, false, nil)
	if err != nil {
		return nil, "", err
	}
	unmerged, err := gitRunCaptured(ctx, gitCmd, []string{"diff", "--name-only", "--diff-filter=U"}, cwd, false, nil)
	if err != nil {
		return nil, "", err
	}
	conflictedFiles := pystr.Strip(unmerged.Stdout)
	if conflictedFiles != "" || (restore.ReturnCode != 0 && !stashApplyFailedOnlyOnExistingUntracked(restore.Stderr)) {
		if err := resetHard(ctx, gitCmd, cwd); err != nil {
			return nil, "", err
		}
	}
	return restore, conflictedFiles, nil
}

// Drops the stash entry of restored changes, printing cleanup guidance when it cannot
func dropRestoredStash(ctx context.Context, w io.Writer, gitCmd []string, cwd, stashRef string) error {
	stashSelector, found, err := resolveStashSelector(ctx, gitCmd, cwd, stashRef)
	if err != nil {
		return err
	}
	if !found {
		fmt.Fprintln(w, "⚠ Local changes were restored, but Icebath couldn't find the stash entry to drop.")
		fmt.Fprintln(w, stashLeftInPlace)
		printStashCleanupGuidance(w, stashRef, "")
		return nil
	}
	drop, err := gitRunCaptured(ctx, gitCmd, []string{"stash", "drop", stashSelector}, cwd, false, nil)
	if err != nil {
		return err
	}
	if drop.ReturnCode != 0 {
		fmt.Fprintln(w, "⚠ Local changes were restored, but Icebath couldn't drop the saved stash entry.")
		printNonempty(w, drop.Stdout, "")
		printNonempty(w, drop.Stderr, "")
		fmt.Fprintln(w, stashLeftInPlace)
		printStashCleanupGuidance(w, stashRef, stashSelector)
	}
	return nil
}

// Restores the autostash onto the updated tree after health checks, true when it was applied and dropped
func restoreStashedChanges(ctx context.Context, w io.Writer, gitCmd []string, cwd, stashRef string, promptUser bool,
	inputFn func(prompt, def string) (string, error), move checkoutMoveFunc) (bool, error) {
	if promptUser {
		confirmed, err := confirmRestore(w, stashRef, inputFn)
		if err != nil {
			return false, err
		}
		if !confirmed {
			recordStashDisposition(ctx, "parked", stashRef, "restore declined", true)
			return false, nil
		}
	}
	preexistingUntracked, known, err := gitUntrackedPaths(ctx, w, gitCmd, cwd)
	if err != nil {
		return false, err
	}
	if !known {
		fmt.Fprintln(w, "  The stash was not restored because its cleanup baseline is unknown.")
		fmt.Fprintf(w, "  Restore manually with: git stash apply %s\n", stashRef)
		recordStashDisposition(ctx, "parked", stashRef, "untracked baseline unknown", false)
		return false, nil
	}
	_, cleanImportFailures, err := criticalModuleImportFailures(ctx, cwd, true)
	if err != nil {
		return false, err
	}
	replaced, applied, err := applyStash(ctx, w, gitCmd, cwd, stashRef, move)
	if err != nil || !applied {
		return false, err
	}
	reject := func(failingTarget, detail string) error {
		return rejectUnsafeStashRestore(ctx, w, gitCmd, cwd, stashRef, preexistingUntracked, failingTarget, detail, move)
	}
	restoredPython, known, err := restoredPythonPaths(ctx, w, gitCmd, cwd)
	if err != nil {
		return false, err
	}
	if !known {
		return false, reject("restored Python source discovery", "could not determine which restored Python files require validation")
	}
	onDisk := make([]string, len(restoredPython))
	for i, path := range restoredPython {
		onDisk[i] = fsEncode(path)
	}
	if syntaxOK, failingPath, syntaxError := validatePythonFilesSyntax(cwd, onDisk); !syntaxOK {
		if failingPath == "" {
			failingPath = "restored Python source"
		}
		return false, reject(failingPath, syntaxError)
	}
	nonRuntime, err := restoreIsNonRuntime(ctx, gitCmd, cwd, stashRef)
	if err != nil {
		return false, err
	}
	if !nonRuntime {
		order, failures, err := criticalModuleImportFailures(ctx, cwd, true)
		if err != nil {
			return false, err
		}
		for _, module := range order {
			failure := failures[module]
			if before, had := cleanImportFailures[module]; had && before == failure {
				continue
			}
			name := module
			if name == "" {
				name = "unknown"
			}
			return false, reject("agent import "+name, failure[1])
		}
	}
	if len(replaced) > 0 {
		shown := replaced[:min(len(replaced), 10)]
		fmt.Fprintf(w, "⚠ The update added %d file(s) where you had untracked files of the same name:\n", len(replaced))
		for _, path := range shown {
			fmt.Fprintf(w, "    %s\n", path)
		}
		fmt.Fprintln(w, "  The updated files are in place; your versions are kept in the stash, which was NOT dropped.")
		fmt.Fprintf(w, "  Stash ref: %s\n", stashRef)
		fmt.Fprintf(w, "  Recover a file with: git show %s^3:<path> > <path>.mine\n", stashRef)
		recordStashDisposition(ctx, "parked", stashRef, "untracked files replaced by the update: "+strings.Join(shown, ", "), false)
		return false, nil
	}
	if err := dropRestoredStash(ctx, w, gitCmd, cwd, stashRef); err != nil {
		return false, err
	}
	recordStashDisposition(ctx, "restored", stashRef, "", false)
	fmt.Fprintln(w, "⚠ Local changes were restored on top of the updated codebase.")
	fmt.Fprintln(w, "  Review `git diff` / `git status` if Icebath behaves unexpectedly.")
	return true, nil
}

// Drops a pre update stash without applying it, true when dropped and false when the stash is left in place
func discardStashedChanges(ctx context.Context, w io.Writer, gitCmd []string, cwd, stashRef string) (bool, error) {
	stashSelector, found, err := resolveStashSelector(ctx, gitCmd, cwd, stashRef)
	if err != nil {
		return false, err
	}
	if !found {
		fmt.Fprintln(w, "⚠ Configured to discard local changes on non-interactive update, but Icebath couldn't find the stash entry to drop.")
		printStashCleanupGuidance(w, stashRef, "")
		return false, nil
	}
	drop, err := gitRunCaptured(ctx, gitCmd, []string{"stash", "drop", stashSelector}, cwd, false, nil)
	if err != nil {
		return false, err
	}
	if drop.ReturnCode != 0 {
		fmt.Fprintln(w, "⚠ Configured to discard local changes, but Icebath couldn't drop the saved stash entry.")
		printFirstLine(w, drop.Stderr)
		printStashCleanupGuidance(w, stashRef, stashSelector)
		recordStashDisposition(ctx, "parked", stashRef, "configured discard failed", false)
		return false, nil
	}
	fmt.Fprintln(w, "→ Discarded local source changes (updates.non_interactive_local_changes=discard).")
	recordStashDisposition(ctx, "discarded", stashRef, "", false)
	return true, nil
}
