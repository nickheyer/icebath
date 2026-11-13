package selfupdate

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"golang.org/x/net/http/httpproxy"

	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyshutil"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyzip"
	"github.com/nickheyer/icebath/pkg/pm"
)

var zipStagingArtifactSuffixes = [2]string{zipStagingSuffix, zipOldSuffix}

var zipIcebathRootArtifacts = map[string]bool{
	ZipSwapJournal: true, ZipSwapJournal + ".lock": true, ".update-incomplete": true, ".lazy-refresh-incomplete": true,
}

var zipJournalTemp = regexp.MustCompile(`^\.icebath-update-zip-swap\.[0-9a-f]{12}\.tmp$`)

var zipPreservedTopLevel = map[string]bool{"venv": true, ".venv": true, "node_modules": true, ".git": true, ".env": true}

var zipPreservedNested = map[string][]string{
	"apps":    {"desktop/release", "desktop/dist", "desktop/dist-webapp", "desktop/node_modules", "desktop/build"},
	"cmd":     {"icebath/web_dist"},
	"scripts": {"whatsapp-bridge/node_modules"},
	"web":     {"node_modules"},
}

const stashHint = "  Stash or commit your changes, then rerun `icebath update`."

var (
	fullCommitRE    = regexp.MustCompile(`^[0-9a-f]{40}$`)
	zipRepositoryRE = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
)

// The source completion tail a finished ZIP swap hands its request to
var zipCompleteSourceUpdate = completeSourceUpdate

// The rename, replace, hardlink and tree copy the two phase swap is built from
var (
	zipRename   = os.Rename
	zipReplace  = os.Rename
	zipLink     = os.Link
	zipCopyTree = pyshutil.CopyTree
)

// A staged copy and the live path it replaces
type stagedEntry struct {
	staging, dst string
}

// Copies a URL to a file the way urllib.request.urlretrieve does, an error status or a short body failing
var zipURLRetrieve = func(ctx context.Context, source, path string) error {
	parsed, err := url.Parse(source)
	if err != nil {
		return &pm.PyError{Class: "ValueError", Message: "unknown url type: " + pyrepr.StrRepr(source)}
	}
	switch parsed.Scheme {
	case "file":
		return retrieveLocalFile(parsed, path)
	case "http", "https":
	default:
		return &pm.PyError{Class: "URLError", Message: "<urlopen error unknown url type: " + parsed.Scheme + ">"}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
	if err != nil {
		return err
	}
	proxy := httpproxy.FromEnvironment().ProxyFunc()
	client := &http.Client{Transport: &http.Transport{Proxy: func(r *http.Request) (*url.URL, error) { return proxy(r.URL) }}}
	resp, err := client.Do(req)
	if err != nil {
		return &pm.PyError{Class: "URLError", Message: "<urlopen error " + err.Error() + ">"}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		reason := strings.TrimSpace(strings.TrimPrefix(resp.Status, strconv.Itoa(resp.StatusCode)))
		return &pm.PyError{Class: "HTTPError", Message: fmt.Sprintf("HTTP Error %d: %s", resp.StatusCode, reason)}
	}
	out, err := os.Create(path)
	if err != nil {
		return err
	}
	read, err := io.CopyBuffer(out, resp.Body, make([]byte, 8192))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if resp.ContentLength >= 0 && read < resp.ContentLength {
		return &pm.PyError{Class: "ContentTooShortError", Message: fmt.Sprintf("<urlopen error retrieval incomplete: got only %d out of %d bytes>", read, resp.ContentLength)}
	}
	return nil
}

// Copies the local file a file URL names to path, the urlopen error of a missing or remote one returned
func retrieveLocalFile(source *url.URL, path string) error {
	if source.Host != "" && source.Host != "localhost" {
		return &pm.PyError{Class: "URLError", Message: "<urlopen error file:// scheme is supported only on localhost>"}
	}
	local := source.Path
	if custodyWindows() {
		local = filepath.FromSlash(strings.TrimLeft(local, "/"))
	}
	in, err := os.Open(local)
	if err != nil {
		return &pm.PyError{Class: "URLError", Message: "<urlopen error " + launchErrorText(err) + ">"}
	}
	defer in.Close()
	out, err := os.Create(path)
	if err != nil {
		return err
	}
	_, err = io.CopyBuffer(out, in, make([]byte, 8192))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return err
}

// Removes a directory or file, a missing path changing nothing
func removePath(path string, ignoreErrors bool) error {
	if isDirFollow(path) {
		_ = pyshutil.Rmtree(path)
		return nil
	}
	if statExists(path) {
		if err := pypath.Unlink(path); err != nil && !ignoreErrors {
			return err
		}
	}
	return nil
}

// Replaces dst with src through the two phase stage and swap, the old tree never half deleted
func atomicReplaceDir(w io.Writer, src, dst string) error {
	staging, err := stageReplacement(w, src, dst, nil)
	if err != nil {
		return err
	}
	return commitStagedReplacements([]stagedEntry{{staging, dst}}, nil, "", nil)
}

// Copies src to the sibling staging path of dst and returns it, nothing live touched
func stageReplacement(w io.Writer, src, dst string, onCreated func(staging string) error) (string, error) {
	staging, backup := dst+zipStagingSuffix, dst+zipOldSuffix
	if !lexists(dst) && lexists(backup) {
		if err := zipRename(backup, dst); err != nil {
			return "", err
		}
	}
	for _, leftover := range []string{staging, backup} {
		if lexists(leftover) {
			aside, err := keepAside(leftover)
			if err != nil {
				return "", err
			}
			fmt.Fprintf(w, "  ⚠ Kept %s aside as %s: nothing proves it is this update's.\n", leftover, filepath.Base(aside))
		}
	}
	srcIsDir := isDirFollow(src)
	if srcIsDir {
		if err := os.Mkdir(staging, 0o777); err != nil {
			return "", err
		}
	} else if err := copyFileExclusive(src, staging, false, nil); err != nil {
		return "", err
	}
	made := ZipEntryIdentity(staging, false)
	err := func() error {
		if onCreated != nil {
			if err := onCreated(staging); err != nil {
				return err
			}
		}
		if ZipEntryIdentity(staging, false) != made {
			return &pm.PyError{Class: "OSError", Message: "the staging entry " + staging + " was replaced while it was being created"}
		}
		if srcIsDir {
			return zipCopyTree(src, staging, false, true)
		}
		return nil
	}()
	if err != nil {
		if ZipEntryIdentity(staging, false) == made {
			_ = removePath(staging, false)
		}
		return "", err
	}
	return staging, nil
}

// Copies src into a dst this call alone creates, bytes, mode and times going through the held handle
func copyFileExclusive(src, dst string, sync bool, onCreated func(dst string) error) error {
	st, err := os.Stat(src)
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL|openNoFollow, 0o600)
	if err != nil {
		return err
	}
	err = func() error {
		if onCreated != nil {
			if err := onCreated(dst); err != nil {
				return err
			}
		}
		source, err := os.Open(src)
		if err != nil {
			return err
		}
		defer source.Close()
		if _, err := io.CopyBuffer(out, source, make([]byte, 1<<20)); err != nil {
			return err
		}
		if err := chmodHandle(out, st); err != nil {
			return err
		}
		if err := utimeHandle(out, st); err != nil {
			return err
		}
		if sync {
			return out.Sync()
		}
		return nil
	}()
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = pypath.Unlink(dst)
	}
	return err
}

// Removes the staging paths of entries that were never committed, a failure logged and passed over
func discardStaged(staged []stagedEntry) {
	for _, entry := range staged {
		if err := removePath(entry.staging, false); err != nil {
			updateLog.Warn(fmt.Sprintf("could not remove staging path %s: %s", entry.staging, launchErrorText(err)))
		}
	}
}

// Hardlinks path to backup, false where links are unsupported
func hardlinkBackup(path, backup string) bool {
	return zipLink(path, backup) == nil
}

// Backs up a regular file as a complete backup while it stays in place, a hardlink or a synced copy
func fileBackup(path, backup, tag string, onTemp func(tmp string) error) error {
	if hardlinkBackup(path, backup) {
		return nil
	}
	if tag == "" {
		tag = randomHex(6)
	}
	tmp := backup + "." + tag + ".tmp"
	err := copyFileExclusive(path, tmp, true, onTemp)
	if err == nil {
		err = zipReplace(tmp, backup)
	}
	if err != nil && isUpdateOSError(err) {
		_ = pypath.Unlink(tmp)
	}
	return err
}

// Lowercase hex of n random bytes
func randomHex(n int) string {
	raw := make([]byte, n)
	if _, err := rand.Read(raw); err != nil {
		panic(err)
	}
	return hex.EncodeToString(raw)
}

// Swaps every staged entry into place, rolling back all of them when one swap fails
func commitStagedReplacements(staged []stagedEntry, onCommitted func() error, tag string, onTemp func(tmp string) error) error {
	type swap struct{ dst, backup string }
	var swapped []swap
	err := func() error {
		for _, entry := range staged {
			backup := entry.dst + zipOldSuffix
			if lexists(backup) {
				return &pm.PyError{Class: "FileExistsError", Message: backup + " appeared during the update; it was left in place"}
			}
			if isRegularNoFollow(entry.dst) {
				if err := fileBackup(entry.dst, backup, tag, onTemp); err != nil {
					return err
				}
				swapped = append(swapped, swap{entry.dst, backup})
				if err := zipReplace(entry.staging, entry.dst); err != nil {
					return err
				}
				continue
			}
			if lexists(entry.dst) {
				if err := zipRename(entry.dst, backup); err != nil {
					return err
				}
				swapped = append(swapped, swap{entry.dst, backup})
			} else {
				swapped = append(swapped, swap{entry.dst, ""})
			}
			if err := zipRename(entry.staging, entry.dst); err != nil {
				return err
			}
		}
		return nil
	}()
	if err != nil {
		if !isUpdateOSError(err) {
			return err
		}
		for i := len(swapped) - 1; i >= 0; i-- {
			if undo := rollBackSwap(swapped[i].dst, swapped[i].backup); undo != nil {
				if !isUpdateOSError(undo) {
					return undo
				}
				updateLog.Warn(fmt.Sprintf("rollback failed for %s: %s", swapped[i].dst, launchErrorText(undo)))
			}
		}
		return err
	}
	if onCommitted != nil {
		if err := onCommitted(); err != nil {
			return err
		}
	}
	for _, done := range swapped {
		if done.backup != "" {
			_ = removePath(done.backup, true)
		}
	}
	return nil
}

// Puts one swapped entry back from its backup, an entry that had none removed
func rollBackSwap(dst, backup string) error {
	if backup != "" && isRegularNoFollow(backup) {
		return zipReplace(backup, dst)
	}
	if err := removePath(dst, false); err != nil {
		return err
	}
	if backup != "" && lexists(backup) {
		return zipRename(backup, dst)
	}
	return nil
}

// Why overlaying a ZIP onto root would destroy work, blocked false when it is safe
func zipOverlayBlockReason(ctx context.Context, root string, staged []string, shipped map[string]bool) (reason string, blocked bool, err error) {
	if !statExists(filepath.Join(root, ".git")) {
		return "", false, nil
	}
	gitCmd := []string{"git"}
	if custodyWindows() {
		gitCmd = []string{"git", "-c", "windows.appendAtomically=false"}
	}
	result, err := RunGit(ctx, gitCmd, []string{"status", "--porcelain", "--untracked-files=all", "--ignored=matching"},
		ChildOptions{Dir: root, Capture: true, Text: true})
	if err != nil {
		return "", false, err
	}
	code := result.ReturnCode
	if code == 0 && shipped == nil {
		tracked, err := RunGit(ctx, gitCmd, []string{"ls-tree", "--name-only", "HEAD"}, ChildOptions{Dir: root, Capture: true, Text: true})
		if err != nil {
			return "", false, err
		}
		if tracked.ReturnCode == 0 {
			shipped = map[string]bool{}
			for _, name := range pystr.Splitlines(tracked.Stdout, false) {
				shipped[name] = true
			}
		}
		code = tracked.ReturnCode
	}
	if code != 0 {
		text := result.Stderr
		if text == "" {
			text = result.Stdout
		}
		detail := pystr.Splitlines(pystr.Strip(text), false)
		if len(detail) > 0 {
			return "could not check the working tree (" + detail[0] + ")", true, nil
		}
		return "could not check the working tree", true, nil
	}
	for _, line := range pystr.Splitlines(result.Stdout, false) {
		if pystr.Strip(line) != "" && !isZipPreservedEntryStatusLine(line, shipped) && !isZipStagingArtifactStatusLine(line, staged) {
			return "the working tree has uncommitted changes or untracked files", true, nil
		}
	}
	return "", false, nil
}

// Top level entry a porcelain path sits under
func statusTopLevel(path string) string {
	cleaned := strings.TrimRight(strings.ReplaceAll(strings.Trim(pystr.Strip(path), `"`), `\`, "/"), "/")
	top, _, _ := strings.Cut(cleaned, "/")
	return top
}

// Status code and payload of a porcelain line, the whole line as payload when it is shorter than three characters
func splitStatusLine(line string) (status, payload string) {
	if utf8.RuneCountInString(line) < 3 {
		return "", line
	}
	rest := dropRunes(line, 2)
	return line[:len(line)-len(rest)], dropRunes(rest, 1)
}

// True when the swap would not destroy what a porcelain status line names
func isZipPreservedEntryStatusLine(line string, shipped map[string]bool) bool {
	status, payload := splitStatusLine(line)
	paths := []string{payload}
	if strings.ContainsAny(status, "RC") {
		paths = strings.Split(payload, " -> ")
	}
	allPreserved := true
	for _, path := range paths {
		if !zipPreservedTopLevel[statusTopLevel(path)] {
			allPreserved = false
			break
		}
	}
	if allPreserved {
		return true
	}
	if status != "!!" {
		return false
	}
	path := strings.TrimRight(strings.ReplaceAll(strings.Trim(pystr.Strip(payload), `"`), `\`, "/"), "/")
	top, nested, _ := strings.Cut(path, "/")
	if shipped != nil && !shipped[top] {
		return true
	}
	if last := path[strings.LastIndex(path, "/")+1:]; last == "__pycache__" || last == "node_modules" {
		return true
	}
	for _, keep := range zipPreservedNested[top] {
		if nested == keep || strings.HasPrefix(nested, keep+"/") {
			return true
		}
	}
	return false
}

// True when a porcelain status line names the update's own breadcrumb or a staging copy this run made
func isZipStagingArtifactStatusLine(line string, staged []string) bool {
	_, payload := splitStatusLine(line)
	top := statusTopLevel(payload)
	if zipIcebathRootArtifacts[top] || zipJournalTemp.MatchString(top) {
		return true
	}
	for _, item := range staged {
		if top == item+zipStagingArtifactSuffixes[0] {
			return true
		}
	}
	return false
}

// Refuses with exit status 1 to overlay a ZIP onto a dirty git checkout
func abortZipUpdateIfDirtyTree(ctx context.Context, w io.Writer) error {
	root := fleetProjectRoot()
	RestoreInterruptedZipSwap(root, nil)
	reason, blocked, err := zipOverlayBlockReason(ctx, root, nil, nil)
	if err != nil || !blocked {
		return err
	}
	fmt.Fprintf(w, "✗ ZIP fallback refused: %s.\n", reason)
	fmt.Fprintln(w, "  Overlaying the ZIP would overwrite uncommitted edits and permanently delete untracked files.")
	fmt.Fprintln(w, stashHint)
	fmt.Fprintln(w, "  To inspect: git status --porcelain")
	recordStop(ctx, "local_changes_blocked")
	return &pm.SystemExit{Code: 1}
}

// Extracts the archive, rejecting a member that escapes the directory and any symlink member
func extractZipSafely(zipPath, tmpDir string) error {
	archive, err := pyzip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer archive.Close()
	tmpDirReal, err := pypath.Realpath(tmpDir, pypath.Lax)
	if err != nil {
		return err
	}
	for _, member := range archive.Filelist {
		memberPath, err := pypath.Realpath(pypath.Join(tmpDir, member.Filename), pypath.Lax)
		if err != nil {
			return err
		}
		if !strings.HasPrefix(memberPath, tmpDirReal+string(os.PathSeparator)) && memberPath != tmpDirReal {
			return &pm.PyError{Class: "ValueError", Message: "Zip-slip detected: " + member.Filename + " escapes extraction directory"}
		}
		if (member.ExternalAttr>>16)&0o170000 == 0o120000 {
			return &pm.PyError{Class: "ValueError", Message: "ZIP contains unsupported symlink member: " + member.Filename}
		}
	}
	return archive.ExtractAll(&tmpDir, nil)
}

// Commit a GitHub archive was made from, read from the ZIP comment, false when it names none
func archiveCommit(zipPath string) (string, bool, error) {
	archive, err := pyzip.OpenReader(zipPath)
	if err != nil {
		return "", false, err
	}
	raw, err := archive.Comment()
	archive.Close()
	if err != nil {
		return "", false, err
	}
	var decoded strings.Builder
	for _, b := range raw {
		if b < 0x80 {
			decoded.WriteByte(b)
		} else {
			decoded.WriteRune(utf8.RuneError)
		}
	}
	comment := pystr.Strip(decoded.String())
	return comment, fullCommitRE.MatchString(comment), nil
}

// Names of a directory in the order the operating system lists them
func listDir(dir string) ([]string, error) {
	handle, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	defer handle.Close()
	return handle.Readdirnames(-1)
}

// Directory a GitHub ZIP extracted to, the first directory that is not __MACOSX when the named one is absent
func extractedRoot(tmpDir, branch string) (string, error) {
	extracted := filepath.Join(tmpDir, "icebath-agent-"+branch)
	if !isDirFollow(extracted) {
		names, err := listDir(tmpDir)
		if err != nil {
			return "", err
		}
		for _, name := range names {
			if candidate := filepath.Join(tmpDir, name); isDirFollow(candidate) && name != "__MACOSX" {
				return candidate, nil
			}
		}
	}
	return extracted, nil
}

// Fails unless the project root's disk has room for one staged tree copy plus a fifth of headroom
func requireStagingSpace(extracted string, entries []string, projectRoot string) error {
	var need int64
	var walk func(dir string) error
	walk = func(dir string) error {
		children, err := os.ReadDir(dir)
		if err != nil {
			return nil
		}
		var dirs []string
		for _, child := range children {
			path := filepath.Join(dir, child.Name())
			followed, err := os.Stat(path)
			if err == nil && followed.IsDir() {
				if child.Type()&os.ModeSymlink == 0 {
					dirs = append(dirs, path)
				}
				continue
			}
			if err != nil {
				return err
			}
			need += followed.Size()
		}
		for _, sub := range dirs {
			if err := walk(sub); err != nil {
				return err
			}
		}
		return nil
	}
	for _, entry := range entries {
		if path := filepath.Join(extracted, entry); isDirFollow(path) {
			if err := walk(path); err != nil {
				return err
			}
		}
	}
	for _, entry := range entries {
		if path := filepath.Join(extracted, entry); isRegular(path) {
			info, err := os.Stat(path)
			if err != nil {
				return err
			}
			need += info.Size()
		}
	}
	required := int64(float64(need) * 1.2)
	_, _, free, err := pyfs.DiskUsage(projectRoot)
	if err != nil {
		return err
	}
	if free < uint64(required) {
		return &pm.PyError{Class: "RuntimeError", Message: fmt.Sprintf(
			"not enough free disk space to stage the update safely (need ~%d MB, have %d MB)", required/(1024*1024), free/(1024*1024))}
	}
	return nil
}

// Hardlinks source to destination where the filesystem allows, a byte copy otherwise
func linkOrCopyArtifact(source, destination string) error {
	if err := os.Link(source, destination); err != nil {
		return pyshutil.Copy2(source, destination)
	}
	return nil
}

// Clones the live build outputs under item into its staged copy so the swap keeps them
func graftNestedArtifacts(item, live, staging string) error {
	for _, nested := range zipPreservedNested[item] {
		parts := strings.Split(nested, "/")
		source := filepath.Join(append([]string{live}, parts...)...)
		destination := filepath.Join(append([]string{staging}, parts...)...)
		if lexists(destination) || !isDirFollow(source) {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(destination), 0o777); err != nil {
			return err
		}
		if err := pyshutil.CopyTreeFunc(source, destination, true, false, linkOrCopyArtifact); err != nil {
			return err
		}
	}
	return nil
}

// Stages every entry beside its target, dropping the copies it made when one of them fails
func stageEntries(w io.Writer, extracted string, entries []string, projectRoot string, onCreated func(index int, staging string) error) ([]stagedEntry, error) {
	var staged []stagedEntry
	for index, item := range entries {
		dst := filepath.Join(projectRoot, item)
		var created func(string) error
		if onCreated != nil {
			created = func(path string) error { return onCreated(index, path) }
		}
		staging, err := stageReplacement(w, filepath.Join(extracted, item), dst, created)
		if err == nil {
			staged = append(staged, stagedEntry{staging, dst})
			err = graftNestedArtifacts(item, dst, staging)
		}
		if err != nil {
			if !isUpdateExit(err) {
				discardStaged(staged)
			}
			return nil, err
		}
	}
	return staged, nil
}

// Removes the swap journal once none of the swap's own siblings is left on disk
func dropJournalIfClean(w io.Writer, root string, entries []string, suffixes ...string) error {
	if len(suffixes) == 0 {
		suffixes = []string{zipStagingSuffix}
	}
	var left []string
	for _, item := range entries {
		for _, suffix := range suffixes {
			if lexists(filepath.Join(root, item+suffix)) {
				left = append(left, item)
				break
			}
		}
	}
	if len(left) > 0 {
		fmt.Fprintf(w, "  ⚠ Could not remove the update's own copies of %s; the next `icebath` launch removes them.\n", strings.Join(left, ", "))
		return nil
	}
	if err := pypath.Unlink(filepath.Join(root, ZipSwapJournal)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// Stages and swaps under the swap journal and its owner lock so any kill is finished or rolled back
func journaledStageAndSwap(ctx context.Context, w io.Writer, extracted string, entries []string, root, targetSHA string) ([]stagedEntry, error) {
	RestoreInterruptedZipSwap(root, nil)
	if statExists(filepath.Join(root, ZipSwapJournal)) {
		return nil, &pm.PyError{Class: "RuntimeError", Message: "an interrupted ZIP update could not be settled; see the warning above"}
	}
	owned, release := zipSwapOwnerLock(root, 10*time.Second)
	if release != nil {
		defer release()
	}
	if !owned.Owned {
		if owned.Reason == "busy" {
			return nil, &pm.PyError{Class: "RuntimeError", Message: "another `icebath update` is swapping this install right now"}
		}
		return nil, &pm.PyError{Class: "RuntimeError", Message: "no ZIP swap lock, no swap (" + owned.Reason + ")"}
	}
	records := make([][]any, len(entries))
	journalEntries := make([]any, len(entries))
	for i, item := range entries {
		records[i] = []any{item, lexists(filepath.Join(root, item)) || lexists(filepath.Join(root, item+zipOldSuffix)), "", ""}
		journalEntries[i] = records[i]
	}
	gen := randomHex(6)
	if err := WriteZipSwapJournal(root, "staging", journalEntries, gen, nil); err != nil {
		return nil, err
	}
	recordStaged := func(index int, staging string) error {
		records[index][2] = ZipEntryIdentity(staging, false)
		if index == len(entries)-1 || isDirFollow(filepath.Join(extracted, entries[index])) {
			return WriteZipSwapJournal(root, "staging", journalEntries, gen, nil)
		}
		return nil
	}
	staged, err := stageEntries(w, extracted, entries, root, recordStaged)
	if err != nil {
		if dropErr := dropJournalIfClean(w, root, entries); dropErr != nil {
			return nil, dropErr
		}
		return nil, err
	}
	shipped := make(map[string]bool, len(entries))
	for _, item := range entries {
		shipped[item] = true
	}
	recheckReason, blocked, err := zipOverlayBlockReason(ctx, root, entries, shipped)
	if err != nil {
		return nil, err
	}
	if blocked {
		discardStaged(staged)
		if err := dropJournalIfClean(w, root, entries); err != nil {
			return nil, err
		}
		fmt.Fprintf(w, "✗ ZIP fallback aborted before the swap: %s.\n", recheckReason)
		fmt.Fprintln(w, "  Files appeared in the checkout while the update was downloading; committing the swap would delete them.")
		fmt.Fprintln(w, stashHint)
		recordStop(ctx, "local_changes_blocked")
		return nil, &pm.SystemExit{Code: 1}
	}
	broken, err := treeSyntaxError(ctx, extracted, updateCriticalFiles)
	if err != nil {
		return nil, err
	}
	if broken != nil {
		return nil, &pm.PyError{Class: "SyntaxError", Message: broken.Path + ": " + broken.Error}
	}
	if err := armCommitObligations(ctx, root, targetSHA); err != nil {
		return nil, err
	}
	for _, record := range records {
		record[3] = ZipEntryIdentity(filepath.Join(root, record[0].(string)), false)
	}
	if err := WriteZipSwapJournal(root, "swapping", journalEntries, gen, nil); err != nil {
		return nil, err
	}
	temps := map[string]string{}
	recordTemp := func(tmp string) error {
		temps[filepath.Base(tmp)] = ZipEntryIdentity(tmp, true)
		return WriteZipSwapJournal(root, "swapping", journalEntries, gen, temps)
	}
	committed := func() error { return WriteZipSwapJournal(root, "committed", journalEntries, gen, nil) }
	if err := commitStagedReplacements(staged, committed, gen, recordTemp); err != nil {
		return nil, err
	}
	if err := dropJournalIfClean(w, root, entries, zipStagingSuffix, zipOldSuffix, zipOldSuffix+"."+gen+".tmp"); err != nil && !isUpdateOSError(err) {
		return nil, err
	}
	return staged, nil
}

// Closed stop token for a failed ZIP update, by exception class and errno
func zipStopClass(err error, downloaded bool) string {
	class := pm.ErrorClass(err)
	if errors.Is(err, syscall.ENOSPC) || (class == "RuntimeError" && strings.HasPrefix(err.Error(), "not enough free disk space")) {
		return "disk_full"
	}
	if class == "SyntaxError" {
		return "target_syntax_error"
	}
	if downloaded {
		return "zip_failed"
	}
	return "download_failed"
}

// Downloads the source ZIP and two phase swaps it into the checkout, returning the commit it installed
func downloadAndSwapZip(ctx context.Context, w io.Writer, branch, zipURL, targetSHA string) (string, error) {
	fmt.Fprintln(w, "→ Downloading latest version...")
	tmpDir, err := os.MkdirTemp("", "icebath-update-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmpDir)
	downloaded := false
	installed, staged, err := func() (string, int, error) {
		zipPath := filepath.Join(tmpDir, "icebath-agent-"+branch+".zip")
		if err := zipURLRetrieve(ctx, zipURL, zipPath); err != nil {
			return "", 0, err
		}
		downloaded = true
		fmt.Fprintln(w, "→ Extracting...")
		if err := extractZipSafely(zipPath, tmpDir); err != nil {
			return "", 0, err
		}
		archived, named, err := archiveCommit(zipPath)
		if err != nil {
			return "", 0, err
		}
		if !named || (targetSHA != "" && archived != targetSHA) {
			found, wanted := "none", "a full commit SHA"
			if named {
				found = archived
			}
			if targetSHA != "" {
				wanted = targetSHA
			}
			return "", 0, &pm.PyError{Class: "ValueError", Message: "the archive names commit " + found + ", not " + wanted + "; refusing to install unidentified bytes"}
		}
		extracted, err := extractedRoot(tmpDir, branch)
		if err != nil {
			return "", 0, err
		}
		names, err := listDir(extracted)
		if err != nil {
			return "", 0, err
		}
		var entries []string
		for _, name := range names {
			if !zipPreservedTopLevel[name] {
				entries = append(entries, name)
			}
		}
		projectRoot := fleetProjectRoot()
		if err := requireStagingSpace(extracted, entries, projectRoot); err != nil {
			return "", 0, err
		}
		done, err := journaledStageAndSwap(ctx, w, extracted, entries, projectRoot, archived)
		return archived, len(done), err
	}()
	if err == nil {
		fmt.Fprintf(w, "✓ Updated %d items from ZIP\n", staged)
		return installed, nil
	}
	if isUpdateExit(err) {
		return "", err
	}
	RestoreInterruptedZipSwap(fleetProjectRoot(), nil)
	if !statExists(filepath.Join(fleetProjectRoot(), ZipSwapJournal)) {
		if derr := disarmCommitObligations(ctx); derr != nil && isUpdateExit(derr) {
			return "", derr
		}
	}
	fmt.Fprintf(w, "✗ ZIP update failed: %s\n", launchErrorText(err))
	fmt.Fprintln(w, "  Your existing install was left in place.")
	fmt.Fprintln(w, "  Re-run `icebath update` to retry; if the agent won't start, reinstall from https://hermes-agent.nousresearch.com")
	recordStop(ctx, zipStopClass(err, downloaded))
	return "", &pm.SystemExit{Code: 1}
}

// Updates through the source ZIP when git file I/O fails, dependency and build failures returned
func updateViaZip(ctx context.Context, w io.Writer, args map[string]any, _ bool, targetSHA, targetRepository string, completionRequest map[string]any) (bool, error) {
	branch := resolveUpdateBranch(args)
	if branch != "main" {
		fmt.Fprintf(w, "✗ --branch=%s is not supported on the Windows ZIP-fallback update path.\n", branch)
		fmt.Fprintf(w, "  This path runs when git file I/O is broken on the system. Either resolve the git-side breakage (typically an antivirus "+
			"or NTFS filter holding files open) and rerun `icebath update --branch %s`, or update against main with `icebath update`.\n", branch)
		recordStop(ctx, "branch_unsupported")
		return false, &pm.SystemExit{Code: 1}
	}
	if err := abortZipUpdateIfDirtyTree(ctx, w); err != nil {
		return false, err
	}
	if completionRequest == nil {
		return false, pm.StopForRelaunchIncomplete(ctx)
	}
	if targetSHA != "" && !fullCommitRE.MatchString(targetSHA) {
		return false, &pm.PyError{Class: "ValueError", Message: "ZIP update requires an exact full commit SHA"}
	}
	ref := "refs/heads/" + branch
	if targetSHA != "" {
		ref = targetSHA
	}
	repository := targetRepository
	if repository == "" {
		repository = OfficialRepository
	}
	owner, name, _ := strings.Cut(repository, "/")
	if !zipRepositoryRE.MatchString(repository) || owner == "." || owner == ".." || name == "." || name == ".." {
		return false, &pm.PyError{Class: "ValueError", Message: "ZIP update requires a GitHub owner/repository"}
	}
	installed, err := downloadAndSwapZip(ctx, w, branch, "https://github.com/"+repository+"/archive/"+ref+".zip", targetSHA)
	if err != nil {
		return false, err
	}
	switch {
	case installed != "":
		completionRequest["expected_sha"] = installed
	case targetSHA != "":
		completionRequest["expected_sha"] = targetSHA
	default:
		completionRequest["expected_sha"] = nil
	}
	completionRequest["apply_mode"] = "zip"
	if err := zipCompleteSourceUpdate(ctx, w, completionRequest); err != nil {
		return false, err
	}
	return true, nil
}
