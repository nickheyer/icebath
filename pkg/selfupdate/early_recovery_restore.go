package selfupdate

import (
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/proc"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

// Marker a killed update's git move leaves beside the git dir
const InterruptedPullMarker = pm.InterruptedPullMarker

const interruptedPullMaxAge = 10 * time.Minute

var gitOperationInProgress = []string{"MERGE_HEAD", "CHERRY_PICK_HEAD", "REVERT_HEAD", "rebase-merge", "rebase-apply"}

var regularFileModes = map[string]bool{"100644": true, "100755": true}

var worktreeBlobModes = map[string]bool{"100644": true, "100755": true, "120000": true}

// Directory under the git dir holding the published repair closure of a commit
const RecoveryClosureDir = "icebath-update-recovery"

// Files of the repair closure an updater publishes beside the marker
var RecoveryClosure = []string{"icebath_cli/_early_recovery.py", "icebath_cli/update_lock.py", "icebath_cli/update_custody.py"}

const (
	RecoveryClosureManifest = "MANIFEST"
	RecoveryClosureInit     = "icebath_cli/__init__.py"
)

const restoreClaimName = "icebath-update-pull.claim"

// Seconds a launch waits for another launch's restore claim before leaving the tree to it
var restoreClaimWaitSeconds = 10.0

var objectIDRE = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

// A full object name, SHA-1 or SHA-256, what rev-parse --verify HEAD must print
var fullOIDRE = regexp.MustCompile(`^[0-9a-f]{40}(?:[0-9a-f]{24})?$`)

var (
	mergeAdviceMu    sync.Mutex
	mergeAdviceShown bool
)

// Exit of a launch refused while another finishes an interrupted update, swapped by tests
var restoreExit = os.Exit

// Stream the repair reports on, swapped by tests
var restoreStderr io.Writer = os.Stderr

// The active Git operation controlling root, empty when none
func GitOperationInProgress(root string) (string, error) {
	gitDir, err := pm.CheckoutGitDir(root)
	if err != nil {
		return "", err
	}
	for _, marker := range gitOperationInProgress {
		state := filepath.Join(gitDir, marker)
		if !lexists(state) {
			continue
		}
		if marker == "rebase-apply" {
			if lexists(filepath.Join(state, "applying")) {
				return "am", nil
			}
			return "rebase", nil
		}
		if marker == "rebase-merge" {
			return "rebase", nil
		}
		return strings.ReplaceAll(strings.ToLower(strings.TrimSuffix(marker, "_HEAD")), "_", "-"), nil
	}
	return "", nil
}

// Whether a value is a commit id and never a revision or a path
func IsObjectID(value string) bool { return objectIDRE.MatchString(value) }

// Git's object id of a blob holding data in the object format of like
func BlobID(data []byte, like string) string {
	header := []byte(fmt.Sprintf("blob %d\x00", len(data)))
	if len(like) == 40 {
		sum := sha1.Sum(append(header, data...))
		return hex.EncodeToString(sum[:])
	}
	sum := sha256.Sum256(append(header, data...))
	return hex.EncodeToString(sum[:])
}

// Closure directory of a commit under the git dir, a ValueError for anything but a commit id
func RecoveryClosureDirOf(root, pre string) (string, error) {
	if !IsObjectID(pre) {
		return "", fmt.Errorf("not a commit id: %s", pyrepr.StrRepr(pre))
	}
	gitDir, err := pm.CheckoutGitDir(root)
	if err != nil {
		return "", err
	}
	return filepath.Join(gitDir, RecoveryClosureDir, pre), nil
}

// Every closure file present with exactly the bytes its manifest names and nothing missing
func RecoveryClosureVerified(closure, pre string) bool {
	raw, err := os.ReadFile(filepath.Join(closure, RecoveryClosureManifest))
	if err != nil {
		return false
	}
	text, err := pystr.DecodeUTF8Sig(raw)
	if err != nil {
		return false
	}
	listed := map[string]string{}
	for _, line := range pystr.Splitlines(text, false) {
		oid, rel, found := strings.Cut(line, " ")
		if !found {
			return false
		}
		listed[rel] = oid
	}
	want := map[string]bool{RecoveryClosureInit: true}
	for _, rel := range RecoveryClosure {
		want[rel] = true
	}
	if len(listed) != len(want) {
		return false
	}
	for rel := range want {
		if _, ok := listed[rel]; !ok {
			return false
		}
	}
	if listed[RecoveryClosureInit] != BlobID(nil, pre) {
		return false
	}
	for rel, oid := range listed {
		data, err := os.ReadFile(filepath.Join(closure, rel))
		if err != nil || BlobID(data, pre) != oid {
			return false
		}
	}
	return true
}

// Whether path still runs as git, --version exiting 0 and saying git version
func isGit(path string) bool {
	out, err := pysubprocess.RunWith(context.Background(), []string{path, "--version"}, pysubprocess.Options{Timeout: 30 * time.Second})
	if err != nil {
		return false
	}
	return out.Returncode == 0 && strings.HasPrefix(pystr.DecodeUTF8Replace(out.Stdout), "git version")
}

// Package with a staged binary under a store entry
type stagedBinaryPackage interface {
	StoreEntry(version, target string) string
	Binary(entry, target string) string
}

// The git icebath update runs, the updater's recorded one, then PATH, then PM's store, then a bare git
func gitExecutable(ctx context.Context, recordedByUpdater string) string {
	if recordedByUpdater != "" && isRegularNoFollow(recordedByUpdater) && isGit(recordedByUpdater) {
		return recordedByUpdater
	}
	if found, err := exec.LookPath("git"); err == nil && found != "" {
		return found
	}
	if recorded, err := pm.Installed(ctx, "git", true); err == nil && recorded != nil && recorded.Binary != "" && isRegularNoFollow(recorded.Binary) {
		return recorded.Binary
	}
	pkg, err := pm.GetPackage("git")
	if err == nil {
		if staged, ok := pkg.(stagedBinaryPackage); ok {
			target, terr := pm.CurrentTarget()
			version, hasVersion := pm.NewLockfile(pm.LockfilePath()).Version("git")
			store, serr := pm.StoreRoot(ctx)
			if terr == nil && hasVersion && serr == nil {
				binary := staged.Binary(filepath.Join(store, staged.StoreEntry(version, target)), target)
				if binary != "" && isRegularNoFollow(binary) {
					return binary
				}
			}
		}
	}
	return "git"
}

// Finished git of the restore with its exit status and both streams
type gitRun struct {
	returncode int
	stdout     string
	stderr     string
}

// The restore's git runner under custody over one checkout
type restoreGit struct {
	ctx        context.Context
	root       string
	executable string
}

// Runs git with text streams, a nil stdin reading from the null device
func (g restoreGit) run(stdin *string, args ...string) (*gitRun, error) {
	return g.runWith(stdin, true, args...)
}

// Runs git with raw byte streams kept as written
func (g restoreGit) runBytes(args ...string) (*gitRun, error) {
	return g.runWith(nil, false, args...)
}

func (g restoreGit) runWith(stdin *string, text bool, args ...string) (*gitRun, error) {
	base := []string{g.executable, "--literal-pathspecs", "-C", g.root}
	opts := ChildOptions{Dir: g.root, Capture: true, Text: text, Timeout: 120 * time.Second, Input: stdin}
	if stdin == nil {
		opts.Stdin = DevNull
	}
	done, err := RunGit(g.ctx, base, args, opts)
	if err != nil {
		return nil, err
	}
	return &gitRun{returncode: done.ReturnCode, stdout: done.Stdout, stderr: done.Stderr}, nil
}

// Last line of a failed git's stderr, the fallback when it printed nothing
func lastStderrLine(run *gitRun, fallback string) string {
	lines := pystr.Splitlines(strings.TrimSpace(run.stderr), false)
	if len(lines) == 0 {
		return fallback
	}
	return lines[len(lines)-1]
}

// Python subprocess error carrying a failed git's stderr
func gitError(run *gitRun) error { return errors.New(strings.TrimSpace(run.stderr)) }

// The trees the killed git was moving the checkout to and the paths whose new content is unknowable
func treesGitCouldWrite(git restoreGit, pre, target string) ([]string, map[string]bool, error) {
	mergeBase, err := git.run(nil, "merge-base", pre, target)
	if err != nil {
		return nil, nil, err
	}
	base := strings.TrimSpace(mergeBase.stdout)
	if base == "" || base == pre {
		return []string{target}, map[string]bool{}, nil
	}
	merged, err := git.run(nil, "merge-tree", "--write-tree", "-z", "--name-only", "--no-messages", pre, target)
	if err != nil {
		return nil, nil, err
	}
	if merged.returncode == 0 || merged.returncode == 1 {
		parts := strings.Split(merged.stdout, "\x00")
		conflicted := map[string]bool{}
		for _, p := range parts[1:] {
			if p != "" {
				conflicted[p] = true
			}
		}
		return []string{target, parts[0]}, conflicted, nil
	}
	changed := make([]map[string]bool, 0, 2)
	for _, side := range []string{pre, target} {
		diff, err := git.run(nil, "diff", "--name-only", "-z", "--no-renames", base, side)
		if err != nil {
			return nil, nil, err
		}
		set := map[string]bool{}
		for _, p := range strings.Split(diff.stdout, "\x00") {
			if p != "" {
				set[p] = true
			}
		}
		changed = append(changed, set)
	}
	both := map[string]bool{}
	for p := range changed[0] {
		if changed[1][p] {
			both[p] = true
		}
	}
	return []string{target}, both, nil
}

// Blob ids of the checkout's files through the repo's clean filters, as the index stores them
func hashWorktree(git restoreGit, paths []string) (map[string]string, error) {
	blobs := map[string]string{}
	listed := []string{}
	for _, p := range paths {
		if !strings.Contains(p, "\n") {
			listed = append(listed, p)
		}
	}
	if len(listed) > 0 {
		stdin := strings.Join(listed, "\n") + "\n"
		hashed, err := git.run(&stdin, "hash-object", "--stdin-paths")
		if err != nil {
			return nil, err
		}
		if hashed.returncode != 0 {
			return nil, gitError(hashed)
		}
		ids := strings.Fields(hashed.stdout)
		for i, p := range listed {
			if i < len(ids) {
				blobs[p] = ids[i]
			}
		}
	}
	listedSet := map[string]bool{}
	for _, p := range listed {
		listedSet[p] = true
	}
	for _, p := range paths {
		if listedSet[p] {
			continue
		}
		hashed, err := git.run(nil, "hash-object", "--", p)
		if err != nil {
			return nil, err
		}
		blobs[p] = strings.TrimSpace(hashed.stdout)
	}
	return blobs, nil
}

// One new tree entry of a changed path
type newEntry struct {
	mode string
	blob *string
}

// One changed path across the trees the killed git could write
type diffEntry struct {
	oldMode string
	oldBlob *string
	new     []newEntry
}

// Paths whose new entry keeps the pre blob under another mode and already show that mode
func modesGitWrote(git restoreGit, root string, entries map[string]*diffEntry, links map[string]bool) (map[string]bool, error) {
	wanted := map[string]map[string]bool{}
	for p, e := range entries {
		if e.oldBlob == nil {
			continue
		}
		modes := map[string]bool{}
		for _, n := range e.new {
			if n.blob != nil && *n.blob == *e.oldBlob && n.mode != e.oldMode {
				modes[n.mode] = true
			}
		}
		if len(modes) > 0 {
			wanted[p] = modes
		}
	}
	if len(wanted) == 0 {
		return map[string]bool{}, nil
	}
	symlinksOut, err := git.run(nil, "config", "--type=bool", "core.symlinks")
	if err != nil {
		return nil, err
	}
	symlinks := strings.TrimSpace(symlinksOut.stdout) != "false"
	paths := make([]string, 0, len(wanted))
	for p := range wanted {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	staged, err := git.run(nil, append([]string{"ls-files", "-s", "-z", "--"}, paths...)...)
	if err != nil {
		return nil, err
	}
	if staged.returncode != 0 {
		return nil, gitError(staged)
	}
	index := map[string]string{}
	for _, record := range strings.Split(staged.stdout, "\x00") {
		meta, p, found := strings.Cut(record, "\t")
		if !found || p == "" {
			continue
		}
		if fields := strings.Fields(meta); len(fields) > 0 {
			index[p] = fields[0]
		}
	}
	written := map[string]bool{}
	for p, modes := range wanted {
		shown := map[string]bool{}
		full := filepath.Join(root, p)
		if links[p] {
			shown["120000"] = true
		} else if info, err := os.Stat(full); err == nil && info.Mode().IsRegular() {
			if runtime.GOOS == "windows" {
				for m := range regularFileModes {
					shown[m] = true
				}
			} else if info.Mode().Perm()&0o100 != 0 {
				shown["100755"] = true
			} else {
				shown["100644"] = true
			}
			if !symlinks {
				shown["120000"] = true
			}
		}
		intersects := false
		for m := range shown {
			if modes[m] {
				intersects = true
			}
		}
		if (!shown[entries[p].oldMode] && intersects) || modes[index[p]] {
			written[p] = true
		}
	}
	return written, nil
}

// Paths the killed git already touched on the way to target
type writtenPaths struct {
	restore []string
	added   []string
	newDirs map[string]bool
	kept    map[string]bool
}

// Paths the killed git already wrote, false when git no longer knows target
func pathsGitWrote(git restoreGit, root, pre, target string) (*writtenPaths, bool, error) {
	verify, err := git.run(nil, "rev-parse", "-q", "--verify", target+"^{commit}")
	if err != nil {
		return nil, false, err
	}
	if verify.returncode != 0 {
		return nil, false, nil
	}
	trees, unknown, err := treesGitCouldWrite(git, pre, target)
	if err != nil {
		return nil, false, err
	}
	entries := map[string]*diffEntry{}
	order := []string{}
	for _, tree := range trees {
		diff, err := git.run(nil, "diff", "--raw", "-z", "--no-renames", "--no-abbrev", pre, tree)
		if err != nil {
			return nil, false, err
		}
		if diff.returncode != 0 {
			return nil, false, gitError(diff)
		}
		parts := strings.Split(diff.stdout, "\x00")
		for i := 0; i+1 < len(parts); i += 2 {
			meta, p := parts[i], parts[i+1]
			fields := strings.Fields(strings.TrimLeft(meta, ":"))
			if len(fields) < 5 {
				continue
			}
			oldMode, newMode, oldBlob, newBlob, status := fields[0], fields[1], fields[2], fields[3], fields[4]
			relevant := newMode
			if status == "D" {
				relevant = oldMode
			}
			if !worktreeBlobModes[relevant] {
				continue
			}
			entry, ok := entries[p]
			if !ok {
				entry = &diffEntry{oldMode: oldMode}
				if status != "A" {
					blob := oldBlob
					entry.oldBlob = &blob
				}
				entries[p] = entry
				order = append(order, p)
			}
			item := newEntry{mode: newMode}
			if status != "D" {
				blob := newBlob
				item.blob = &blob
			}
			entry.new = append(entry.new, item)
		}
	}
	links := map[string]bool{}
	var toHash []string
	for _, p := range order {
		full := filepath.Join(root, p)
		if info, err := os.Lstat(full); err == nil && info.Mode()&os.ModeSymlink != 0 {
			links[p] = true
			continue
		}
		if info, err := os.Stat(full); err == nil && info.Mode().IsRegular() {
			toHash = append(toHash, p)
		}
	}
	worktreeBlob, err := hashWorktree(git, toHash)
	if err != nil {
		return nil, false, err
	}
	for p := range links {
		targetText, err := os.Readlink(filepath.Join(root, p))
		if err != nil {
			return nil, false, err
		}
		worktreeBlob[p] = BlobID([]byte(targetText), pre)
	}
	retyped, err := modesGitWrote(git, root, entries, links)
	if err != nil {
		return nil, false, err
	}
	result := &writtenPaths{newDirs: map[string]bool{}, kept: map[string]bool{}}
	for _, p := range order {
		entry := entries[p]
		blobs := map[string]bool{}
		for _, n := range entry.new {
			if n.blob != nil {
				blobs[*n.blob] = true
			}
		}
		current, present := worktreeBlob[p]
		var written bool
		switch {
		case !present:
			written = entry.oldBlob != nil
		case entry.oldBlob != nil && current == *entry.oldBlob:
			written = retyped[p]
		case blobs[current] || unknown[p]:
			written = true
		case links[p]:
			written = false
		default:
			content, err := os.ReadFile(filepath.Join(root, p))
			if err != nil {
				return nil, false, err
			}
			written = false
			for blob := range blobs {
				shown, err := git.runBytes("cat-file", "--filters", "--path="+p, blob)
				if err != nil {
					return nil, false, err
				}
				if shown.returncode != 0 {
					return nil, false, errors.New(strings.TrimSpace(pystr.DecodeUTF8Replace([]byte(shown.stderr))))
				}
				if strings.HasPrefix(shown.stdout, string(content)) {
					written = true
				}
			}
		}
		if !written {
			continue
		}
		if entry.oldBlob == nil {
			result.added = append(result.added, p)
			if !blobs[current] {
				result.kept[p] = true
			}
		} else {
			result.restore = append(result.restore, p)
		}
	}
	for _, p := range order {
		if entries[p].oldBlob != nil {
			continue
		}
		for parent := path.Dir(p); parent != "." && parent != "/" && parent != ""; parent = path.Dir(parent) {
			result.newDirs[parent] = true
		}
	}
	if len(result.newDirs) > 0 {
		existing, err := git.run(nil, "ls-tree", "-r", "-d", "--name-only", "-z", pre)
		if err != nil {
			return nil, false, err
		}
		for _, d := range strings.Split(existing.stdout, "\x00") {
			delete(result.newDirs, d)
		}
	}
	return result, true, nil
}

// Holds the restore claim of a git dir, claimed false when another launch held it past the wait
func restoreClaim(gitDir string) (claimed bool, release func()) {
	f, err := os.OpenFile(filepath.Join(gitDir, restoreClaimName), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return true, func() {}
	}
	deadline := time.Now().Add(time.Duration(restoreClaimWaitSeconds * float64(time.Second)))
	for !lockClaimFile(f, true) {
		if time.Now().After(deadline) {
			f.Close()
			return false, func() {}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := f.Truncate(0); err == nil {
		f.WriteAt([]byte(fmt.Sprintf("pid=%d\n", os.Getpid())), 0)
	}
	return true, func() {
		lockClaimFile(f, false)
		f.Close()
	}
}

// Writes text at path as one durable record through an exclusively created random sibling
func WriteDurableText(path, text string) error {
	for {
		var buf [6]byte
		rand.Read(buf[:])
		tmp := filepath.Join(filepath.Dir(path), fmt.Sprintf("%s.%s.tmp", filepath.Base(path), hex.EncodeToString(buf[:])))
		f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|noFollowFlag, 0o644)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return err
		}
		if _, err := f.WriteString(text); err != nil {
			f.Close()
			os.Remove(tmp)
			return err
		}
		if err := f.Sync(); err != nil {
			f.Close()
			os.Remove(tmp)
			return err
		}
		if err := f.Close(); err != nil {
			os.Remove(tmp)
			return err
		}
		if err := os.Rename(tmp, path); err != nil {
			os.Remove(tmp)
			return err
		}
		return nil
	}
}

// Renames file to a free <name>.icebath-update-kept[-N] beside it
func keepAside(file string) (string, error) {
	for n := 1; ; n++ {
		aside := file + ".icebath-update-kept"
		if n > 1 {
			aside += fmt.Sprintf("-%d", n)
		}
		if lexists(aside) {
			continue
		}
		return aside, os.Rename(file, aside)
	}
}

// Whether go test or a pytest case drives this process against its own checkout, which no repair may touch
func testOwnsLiveCheckout(root string) bool {
	resolved, err := pypath.Resolve(root)
	return err == nil && pm.LiveCheckoutUnderTest(resolved)
}

// Key value fields of the interrupted pull marker
func readMarkerFields(marker string) (map[string]string, error) {
	raw, err := os.ReadFile(marker)
	if err != nil {
		return nil, err
	}
	text, err := pystr.DecodeUTF8Sig(raw)
	if err != nil {
		return nil, err
	}
	fields := map[string]string{}
	for _, line := range pystr.Splitlines(text, false) {
		key, value, _ := strings.Cut(line, "=")
		fields[key] = value
	}
	return fields, nil
}

// Holds the checkout kernel lock for a repair like an updater, the busy reason when a live update owns it
func checkoutCustody(ctx context.Context, root string) (busy string, release func(), err error) {
	holder, err := updaterecord.AcquireCheckout(root)
	if err != nil {
		return "", nil, err
	}
	if holder != nil {
		held, herr := updaterecord.CheckoutLockHeld(ctx, root)
		if herr != nil {
			return "", nil, herr
		}
		if !held {
			if holder, err = updaterecord.AcquireCheckout(root); err != nil {
				return "", nil, err
			}
		}
	}
	if holder != nil {
		held, herr := updaterecord.CheckoutLockHeld(ctx, root)
		if herr != nil {
			return "", nil, herr
		}
		if !held {
			return "", func() {}, nil
		}
		reason := updaterecord.DescribeHolder(holder)
		if reason == "" {
			reason = "another update holds the checkout lock"
		}
		return reason, func() {}, nil
	}
	return "", updaterecord.ReleaseCheckout, nil
}

// Puts back the files a killed icebath update had half moved to the new commit, true when the tree changed
func RestoreInterruptedPull(ctx context.Context, projectRoot string, afterFailure bool) bool {
	changed, err := restoreInterruptedPull(ctx, projectRoot, afterFailure)
	if err != nil {
		fmt.Fprintf(restoreStderr, "⚠ Could not check for an interrupted `icebath update`: %s\n", pypath.OSErrorText(err))
		return false
	}
	return changed
}

func restoreInterruptedPull(ctx context.Context, root string, afterFailure bool) (bool, error) {
	if root == "" {
		root = pm.RepoRoot()
	}
	marker, err := pm.InterruptedPullMarkerPath(root)
	if err != nil {
		return false, err
	}
	journal := filepath.Join(root, ZipSwapJournal)
	if !isRegularNoFollow(marker) && !isRegularNoFollow(journal) {
		return false, nil
	}
	if testOwnsLiveCheckout(root) {
		return false, nil
	}
	const busyNote = "⚠ Not repairing the checkout now: %s. Launch again once it finishes.\n"
	if isRegularNoFollow(journal) {
		busy, release, err := checkoutCustody(ctx, root)
		if err != nil {
			return false, err
		}
		changed := false
		func() {
			defer release()
			if busy != "" {
				return
			}
			changed = RestoreInterruptedZipSwap(root, restoreStderr)
		}()
		if busy != "" {
			fmt.Fprintf(restoreStderr, busyNote, busy)
			return false, nil
		}
		if changed {
			return true, nil
		}
	}
	if !isRegularNoFollow(marker) {
		return false, nil
	}
	claimed, releaseClaim := restoreClaim(filepath.Dir(marker))
	defer releaseClaim()
	if !claimed {
		if afterFailure {
			return false, nil
		}
		fmt.Fprintln(restoreStderr, "icebath: another Icebath launch is finishing an interrupted `icebath update`; launch again in a moment.")
		restoreExit(1)
		return false, nil
	}
	if !isRegularNoFollow(marker) {
		return true, nil
	}
	busy, release, err := checkoutCustody(ctx, root)
	if err != nil {
		return false, err
	}
	defer release()
	if busy != "" {
		fmt.Fprintf(restoreStderr, busyNote, busy)
		return false, nil
	}
	return restoreHoldingClaim(ctx, root, marker, afterFailure)
}

// Whether the updater that wrote the marker still runs and the marker is fresh
func claimOwnerAlive(fields map[string]string, marker string) bool {
	owner, err := strconv.Atoi(strings.TrimSpace(fields["pid"]))
	if err != nil {
		owner = -1
	}
	info, statErr := os.Stat(marker)
	if statErr != nil {
		return false
	}
	return owner != os.Getpid() && owner > 0 && proc.Exists(owner) && time.Since(info.ModTime()) < interruptedPullMaxAge
}

// Full object name HEAD names, empty with a report when git cannot answer it
func readHead(git restoreGit) (string, error) {
	head, err := git.run(nil, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	oid := ""
	if head.returncode == 0 {
		oid = strings.TrimSpace(head.stdout)
	}
	if fullOIDRE.MatchString(oid) {
		return oid, nil
	}
	detail := strings.TrimSpace(head.stderr)
	if detail == "" {
		detail = fmt.Sprintf("git printed %s, exit %d", pyrepr.StrRepr(oid), head.returncode)
	}
	fmt.Fprintf(restoreStderr, "⚠ Could not read HEAD to repair an interrupted `icebath update` (%s); the next launch retries.\n", detail)
	return "", nil
}

// Short form of a commit for messages
func short(oid string) string {
	if len(oid) > 10 {
		return oid[:10]
	}
	return oid
}

// Redoes a killed syntax rollback's HEAD move to pre, the new HEAD or empty with the marker kept
func resumeKilledRollback(git restoreGit, fields map[string]string, gitDir, root, pre, rollback string, afterFailure bool) (string, error) {
	named, err := git.run(nil, "symbolic-ref", "-q", "HEAD")
	if err != nil {
		return "", err
	}
	current := ""
	if named.returncode == 0 {
		current = strings.TrimSpace(named.stdout)
	}
	recorded, hasRef := fields["ref"]
	if !hasRef || current != strings.TrimSpace(recorded) {
		ref := strings.TrimPrefix(strings.TrimSpace(recorded), "refs/heads/")
		back := fmt.Sprintf("`git -C %s checkout --detach %s`", root, firstNonEmpty(short(strings.TrimSpace(fields["target"])), short(pre)))
		if ref != "" {
			back = fmt.Sprintf("`git -C %s checkout %s`", root, ref)
		}
		fmt.Fprintf(restoreStderr, "⚠ An interrupted `icebath update` rollback to %s was not resumed: HEAD now names "+
			"another branch. Switch back with %s, then launch `icebath` again to finish it.\n", short(pre), back)
		return "", nil
	}
	reason, err := redoRollbackHead(git, gitDir, root, pre, rollback, afterFailure)
	if err != nil {
		return "", err
	}
	if reason != "" {
		fmt.Fprintf(restoreStderr, "⚠ An interrupted `icebath update` rollback to %s cannot resume yet (%s); "+
			"the next launch retries.\n", short(pre), reason)
		return "", nil
	}
	return readHead(git)
}

// Once per process, tells the user to abort the killed updater's own unfinished merge
func adviseKilledMerge(gitDir, root, target, stash string) {
	mergeAdviceMu.Lock()
	defer mergeAdviceMu.Unlock()
	mergeHead := filepath.Join(gitDir, "MERGE_HEAD")
	if mergeAdviceShown || !isRegularNoFollow(mergeHead) {
		return
	}
	raw, err := os.ReadFile(mergeHead)
	if err != nil {
		return
	}
	text, _ := pystr.DecodeUTF8Sig(raw)
	if strings.TrimSpace(text) != target {
		return
	}
	mergeAdviceShown = true
	note := fmt.Sprintf("⚠ A killed `icebath update` left its merge unfinished. Run `git -C %s merge --abort`, then launch again.", root)
	if stash != "" {
		note += fmt.Sprintf(" Your local changes are in its stash (%s).", stash)
	}
	fmt.Fprintln(restoreStderr, note)
}

// The index.lock verdicts before any git runs, foreign, known foreign and whether the index is free
func judgeIndexLock(fields map[string]string, marker, root string, afterFailure bool) (foreignLock, knownForeign, indexFree bool, err error) {
	gitDir := filepath.Dir(marker)
	lock := filepath.Join(gitDir, "index.lock")
	foreignLock = afterFailure && lexists(lock)
	if foreignLock && lockPredatesMove(fields, lock) {
		if err := rememberForeignLock(marker, lock); err != nil {
			return false, false, false, err
		}
	}
	recorded := strings.TrimSpace(fields["foreign_lock"])
	knownForeign = recorded != "" && recorded == lockIdentity(lock)
	indexFree = foreignLock
	if !indexFree && !knownForeign {
		released, rerr := pm.ReleaseDeadLock(lock, root, false)
		if rerr != nil {
			return false, false, false, rerr
		}
		indexFree = released
	}
	return foreignLock, knownForeign, indexFree, nil
}

// Retires a marker whose target commit is gone once the tracked tree is clean at pre
func retireUnattributable(git restoreGit, gitDir, root, marker, pre, target string) error {
	verified, err := rollbackVerified(git, gitDir, pre, nil)
	if err != nil {
		return err
	}
	if !verified {
		fmt.Fprintf(restoreStderr, "⚠ An interrupted `icebath update` left tracked files that differ from %s and commit "+
			"%s is gone; the marker was kept. Inspect `git -C %s status`, then "+
			"`git -C %s checkout %s -- <file>` for each file the update wrote.\n", short(pre), short(target), root, root, short(pre))
		return nil
	}
	if err := os.Remove(marker); err != nil {
		return err
	}
	fmt.Fprintf(restoreStderr, "⚠ Ignoring a stale interrupted-update marker: commit %s is gone.\n", short(target))
	return nil
}

// The restore proper, run while this launch holds the claim and the checkout
func restoreHoldingClaim(ctx context.Context, root, marker string, afterFailure bool) (bool, error) {
	gitDir := filepath.Dir(marker)
	fields, err := readMarkerFields(marker)
	if err != nil {
		return false, err
	}
	if claimOwnerAlive(fields, marker) {
		return false, nil
	}
	pre, target := strings.TrimSpace(fields["pre"]), strings.TrimSpace(fields["target"])
	stash := strings.TrimSpace(fields["stash"])
	git := restoreGit{ctx: ctx, root: root, executable: gitExecutable(ctx, strings.TrimSpace(fields["git"]))}
	foreignLock, knownForeign, indexFree, err := judgeIndexLock(fields, marker, root, afterFailure)
	if err != nil {
		return false, err
	}
	rollback := strings.TrimSpace(fields["rollback"])
	if rollback != "branch" && rollback != "detach" {
		rollback = ""
	}
	head, err := readHead(git)
	if err != nil || head == "" {
		return false, err
	}
	if rollback != "" && pre != "" && target != "" && head == target {
		head, err = resumeKilledRollback(git, fields, gitDir, root, pre, rollback, afterFailure || knownForeign)
		if err != nil || head == "" {
			return false, err
		}
	}
	if pre == "" || target == "" || head != pre {
		return false, os.Remove(marker)
	}
	for _, name := range gitOperationInProgress {
		if lexists(filepath.Join(gitDir, name)) {
			adviseKilledMerge(gitDir, root, target, stash)
			return false, nil
		}
	}
	if !indexFree {
		fmt.Fprintf(restoreStderr, "⚠ %s holds the index after an interrupted `icebath update`; the next launch finishes the restore.\n",
			pm.IndexLockHolder(gitDir, root))
		return false, nil
	}
	written, known, err := pathsGitWrote(git, root, pre, target)
	if err != nil {
		return false, err
	}
	if !known {
		return false, retireUnattributable(git, gitDir, root, marker, pre, target)
	}
	return putBackWritten(git, root, marker, written, pre, stash, rollback, foreignLock, afterFailure)
}

// Returns every path git wrote to pre and retires the marker once verified
func putBackWritten(git restoreGit, root, marker string, written *writtenPaths, pre, stash, rollback string, foreignLock, afterFailure bool) (bool, error) {
	touched := len(written.restore) > 0 || len(written.added) > 0
	if touched && foreignLock {
		fmt.Fprintln(restoreStderr, "⚠ Another git holds the index, so the files the update already wrote cannot be put back "+
			"now; the next launch restores them.")
		return false, nil
	}
	if touched {
		lead := "⚠ A previous `icebath update` was killed while git was writing the new code — "
		if afterFailure {
			lead = "⚠ git stopped partway through writing the new code — "
		}
		fmt.Fprintf(restoreStderr, "%srestoring the checkout to %s...\n", lead, short(pre))
		failed, err := putBackPaths(git, root, written.restore, written.added, written.kept)
		if err != nil {
			return false, err
		}
		if failed != nil {
			fmt.Fprintf(restoreStderr, "  ✗ Could not restore it automatically (%s); the next launch retries.\n", lastStderrLine(failed, "git failed"))
			return false, nil
		}
	}
	dirs := make([]string, 0, len(written.newDirs))
	for d := range written.newDirs {
		dirs = append(dirs, d)
	}
	sort.SliceStable(dirs, func(i, j int) bool { return strings.Count(dirs[i], "/") > strings.Count(dirs[j], "/") })
	for _, rel := range dirs {
		os.Remove(filepath.Join(root, rel))
	}
	if rollback != "" {
		owned := map[string]bool{}
		for _, p := range written.restore {
			owned[p] = true
		}
		verified, err := rollbackVerified(git, filepath.Dir(marker), pre, owned)
		if err != nil {
			return false, err
		}
		if !verified {
			fmt.Fprintf(restoreStderr, "  ✗ The rollback to %s is not verified yet (files the update wrote still differ); "+
				"the next launch retries. Inspect `git -C %s status`.\n", short(pre), root)
			return touched, nil
		}
	}
	if err := os.Remove(marker); err != nil {
		return false, err
	}
	if !touched {
		return false, nil
	}
	fmt.Fprintln(restoreStderr, "  ✓ Checkout restored; `icebath update` updates it again.")
	if stash != "" {
		fmt.Fprintf(restoreStderr, "  Your local changes are still in the update's stash (%s).\n", stash)
	}
	return true, nil
}

// inode and mtime of a lock file, empty when there is none
func lockIdentity(lock string) string {
	info, err := os.Lstat(lock)
	if err != nil {
		return ""
	}
	_, ino, _ := fileIdentity(lock, info)
	return fmt.Sprintf("%d:%d", ino, mtimeNanos(info))
}

// Whether the lock is the generation the move saw before its git ran, every lock foreign without the record
func lockPredatesMove(fields map[string]string, lock string) bool {
	before, ok := fields["index_lock"]
	if !ok {
		return true
	}
	before = strings.TrimSpace(before)
	return before != "" && before == lockIdentity(lock)
}

// Records the lock's identity in the marker atomically
func rememberForeignLock(marker, lock string) error {
	identity := lockIdentity(lock)
	if identity == "" {
		return nil
	}
	raw, err := os.ReadFile(marker)
	if err != nil {
		return err
	}
	text, err := pystr.DecodeUTF8Sig(raw)
	if err != nil {
		return err
	}
	lines := []string{}
	for _, line := range pystr.Splitlines(text, false) {
		if !strings.HasPrefix(line, "foreign_lock=") {
			lines = append(lines, line)
		}
	}
	lines = append(lines, "foreign_lock="+identity)
	return WriteDurableText(marker, strings.Join(lines, "\n")+"\n")
}

// Moves HEAD and the index back to pre for a killed syntax rollback, empty or why not yet
func redoRollbackHead(git restoreGit, gitDir, root, pre, mode string, afterFailure bool) (string, error) {
	lock := filepath.Join(gitDir, "index.lock")
	if lexists(lock) {
		released := false
		if !afterFailure {
			var err error
			released, err = pm.ReleaseDeadLock(lock, root, false)
			if err != nil {
				return "", err
			}
		}
		if !released {
			return pm.IndexLockHolder(gitDir, root) + " may still hold .git/index.lock", nil
		}
	}
	if mode == "detach" {
		moved, err := git.run(nil, "update-ref", "--no-deref", "HEAD", pre)
		if err != nil {
			return "", err
		}
		if moved.returncode != 0 {
			return "git update-ref failed: " + lastStderrLine(moved, "?"), nil
		}
	}
	reset, err := git.run(nil, "reset", "-q", pre)
	if err != nil {
		return "", err
	}
	if reset.returncode != 0 {
		return "git reset failed: " + lastStderrLine(reset, "?"), nil
	}
	head, err := git.run(nil, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	if head.returncode != 0 || strings.TrimSpace(head.stdout) != pre {
		return "HEAD did not move back", nil
	}
	return "", nil
}

// Whether the rollback landed, HEAD at pre, no index lock and no tracked change among owned or anywhere
func rollbackVerified(git restoreGit, gitDir, pre string, owned map[string]bool) (bool, error) {
	head, err := git.run(nil, "rev-parse", "HEAD")
	if err != nil {
		return false, err
	}
	if head.returncode != 0 || strings.TrimSpace(head.stdout) != pre || lexists(filepath.Join(gitDir, "index.lock")) {
		return false, nil
	}
	status, err := git.run(nil, "status", "--porcelain", "-z", "--untracked-files=no", "--no-renames")
	if err != nil {
		return false, err
	}
	if status.returncode != 0 {
		return false, nil
	}
	for _, entry := range strings.Split(status.stdout, "\x00") {
		if len(entry) < 3 {
			continue
		}
		changed := entry[3:]
		if owned == nil || owned[changed] {
			return false, nil
		}
	}
	return true, nil
}

// Returns restore to HEAD and drops added, those in kept renamed aside, the failed git run or nil
func putBackPaths(git restoreGit, root string, restore, added []string, kept map[string]bool) (*gitRun, error) {
	if len(restore) > 0 {
		stdin := strings.Join(restore, "\x00")
		run, err := git.run(&stdin, "restore", "--source=HEAD", "--staged", "--worktree", "--pathspec-from-file=-", "--pathspec-file-nul")
		if err != nil {
			return nil, err
		}
		if run.returncode != 0 {
			return run, nil
		}
	}
	if len(added) > 0 {
		stdin := strings.Join(added, "\x00")
		run, err := git.run(&stdin, "rm", "-q", "--cached", "--ignore-unmatch", "--pathspec-from-file=-", "--pathspec-file-nul")
		if err != nil {
			return nil, err
		}
		for _, rel := range added {
			full := filepath.Join(root, rel)
			if kept[rel] {
				aside, err := keepAside(full)
				if err != nil {
					return nil, err
				}
				fmt.Fprintf(restoreStderr, "  Kept %s as %s: it may be your own file.\n", rel, filepath.Base(aside))
				continue
			}
			if err := os.Remove(full); err != nil && !errors.Is(err, os.ErrNotExist) {
				return nil, err
			}
		}
		if run.returncode != 0 {
			return run, nil
		}
	}
	return nil, nil
}

// Re runs this command from the restored tree, the exit code of the child on Windows and never returning elsewhere
func RelaunchAfterRestore(argv []string) int {
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	os.Stdout.Sync()
	os.Stderr.Sync()
	args := append([]string{}, argv[1:]...)
	if runtime.GOOS == "windows" {
		cmd := exec.Command(exe, args...)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			var exit *exec.ExitError
			if errors.As(err, &exit) {
				return exit.ExitCode()
			}
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return 0
	}
	if err := execSelf(exe, append([]string{exe}, args...)); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}
