package selfupdate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"encoding/base64"
	"github.com/nickheyer/icebath/pkg/internal/pyast"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyshutil"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
	"github.com/nickheyer/icebath/pkg/internal/pytoml"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
	"github.com/nickheyer/icebath/pkg/tools"
)

// One obligation record as a run found or left it, present false when the file was absent
type armedRecord struct {
	path    string
	data    []byte
	present bool
}

// Git command and the HEAD and branch a run started its checkout phase from
type commitRun struct {
	gitCmd       []string
	head, branch string
}

// One git tree move the interrupted pull marker names, empty Pre, Stash and Rollback standing for None
type treeMove struct {
	Pre, Target, Stash, Rollback string
}

// A startup file that does not compile with the compiler's verdict
type syntaxFault struct {
	Path, Error string
}

// A startup file's bytes, nil Data when the file is absent
type sourceFile struct {
	Rel  string
	Data []byte
}

var (
	armedSnapshot  []armedRecord
	armedBytes     = map[string]armedRecord{}
	commitRunStart *commitRun
	obligationRoot *string
	commitOwner    string

	commitStderr io.Writer = os.Stderr

	obligationReadFile  = os.ReadFile
	armCommitPoint      = armCommitPointNow
	admittedPythonProbe = admittedPython
)

const pythonVersionProbe = "import platform; print(platform.python_version())"

const compileAll = "import base64, json, sys\n" +
	"verdict = None\n" +
	"for rel, data in json.load(sys.stdin).items():\n" +
	"    try:\n" +
	"        compile(base64.b64decode(data), rel, 'exec', dont_inherit=True)\n" +
	"    except (SyntaxError, ValueError) as exc:\n" +
	"        verdict = [rel, f'{type(exc).__name__}: {exc}']\n" +
	"        break\n" +
	"print(json.dumps(verdict))\n"

// Whether a test run owns the live checkout at root, which no update step may touch
func ownsLiveCheckout(root string) bool { return testOwnsLiveCheckout(root) }

// Completion tail, profile fleet marker and host obligation records of the install at root
func obligationPaths(ctx context.Context, root string) []string {
	return []string{CompletionPendingPath(ctx, root), fleetRestartPendingMarkerPath(ctx), updaterecord.HostObligationPath()}
}

// Bytes of a record, present false when absent, every other read failure returned
func readOrNone(path string) ([]byte, bool, error) {
	data, err := obligationReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return data, true, nil
}

// Owes the completion tail and the fleet restart for expectedSHA before the tree moves
func armCommitObligations(ctx context.Context, root, expectedSHA string) error {
	if ownsLiveCheckout(root) {
		return nil
	}
	if armedSnapshot == nil {
		snapshot := make([]armedRecord, 0, 3)
		for _, path := range obligationPaths(ctx, root) {
			if slices.ContainsFunc(snapshot, func(r armedRecord) bool { return r.path == path }) {
				continue
			}
			data, present, err := readOrNone(path)
			if err != nil {
				return err
			}
			snapshot = append(snapshot, armedRecord{path: path, data: data, present: present})
		}
		armedSnapshot = snapshot
		commitOwner = fmt.Sprintf("%s|%d|%s", root, os.Getpid(), strings.ReplaceAll(uuid.NewString(), "-", ""))
	}
	owed := false
	_, armErr := ArmCompletion(ctx, root)
	if armErr == nil {
		owed, armErr = writeFleetRestartPendingMarker(ctx, expectedSHA, nil, commitOwner)
	}
	for _, rec := range armedSnapshot {
		data, present, err := readOrNone(rec.path)
		if err != nil {
			return err
		}
		armedBytes[rec.path] = armedRecord{path: rec.path, data: data, present: present}
	}
	if armErr != nil {
		return armErr
	}
	if !owed {
		return &pm.PyError{Class: "OSError", Message: "could not record the pending gateway-restart obligation"}
	}
	return nil
}

// Restores both obligations to what this run found, or owes them for the head a later move settled on
func disarmCommitObligations(ctx context.Context) error {
	if commitRunStart != nil {
		head := ""
		if obligationRoot != nil {
			found, _, err := headAndBranch(ctx, commitRunStart.gitCmd, *obligationRoot)
			if err != nil {
				return err
			}
			head = found
		}
		if obligationRoot == nil || commitRunStart.head == "" || head != commitRunStart.head {
			if obligationRoot != nil {
				return oweFor(ctx, *obligationRoot, head)
			}
			return nil
		}
	}
	snapshot := armedSnapshot
	armedSnapshot = nil
	armed := maps.Clone(armedBytes)
	clear(armedBytes)
	host := updaterecord.HostObligationPath()
	for _, rec := range snapshot {
		var err error
		if rec.path == host {
			err = updaterecord.ReleaseHostObligation(ctx, commitOwner)
		} else {
			err = restoreArmedRecord(rec, armed)
		}
		if err != nil && !isUpdateOSError(err) {
			return err
		}
	}
	return nil
}

// Puts one record back to what the run found when it still holds exactly what the run armed
func restoreArmedRecord(rec armedRecord, armed map[string]armedRecord) error {
	left, ok := armed[rec.path]
	if !ok {
		return nil
	}
	data, present, err := readOrNone(rec.path)
	if err != nil {
		return err
	}
	if present != left.present || !bytes.Equal(data, left.data) {
		return nil
	}
	if !rec.present {
		if err := pypath.Unlink(rec.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	return updaterecord.ReplaceBytes(rec.path, rec.data)
}

// Names pre in the obligations after a failed move left a torn tree under its marker
func oweRestoreOf(ctx context.Context, root, pre string) error {
	return oweFor(ctx, root, pre)
}

// Arms again for sha what this run armed, the commit the checkout is on or is restored to
func oweFor(ctx context.Context, root, sha string) error {
	if armedSnapshot == nil || !IsObjectID(sha) {
		return nil
	}
	err := armCommitObligations(ctx, root, sha)
	if err == nil {
		return nil
	}
	if !isUpdateOSError(err) {
		return err
	}
	updateLog.Warn(fmt.Sprintf("Could not re-arm the update obligations for %s: %s", short(sha), launchErrorText(err)))
	fmt.Fprintf(commitStderr, "  ⚠ Could not record the restart this update still owes for %s (%s); run `icebath gateway restart` once the update is done.\n", short(sha), launchErrorText(err))
	return nil
}

// Commit a fast forward from pre to target arms the obligations for
func debtSHAForMove(pre, target string) string {
	start := ""
	if commitRunStart != nil {
		start = commitRunStart.head
	}
	if start != "" && pre != start {
		return pre
	}
	return target
}

// Arms the obligations then the tree move marker, the refusal text when either is not durable
func armCommitPointNow(ctx context.Context, gitCmd []string, root, expectedSHA string, move treeMove) (string, error) {
	refused, err := pauseAtCommitPoint(ctx)
	if err != nil || refused != "" {
		return refused, err
	}
	err = armCommitObligations(ctx, root, expectedSHA)
	if err == nil {
		_, err = armTreeMove(ctx, gitCmd, root, move)
	}
	if err == nil {
		return "", nil
	}
	if !isUpdateOSError(err) {
		return "", err
	}
	if derr := disarmCommitObligations(ctx); derr != nil {
		return "", derr
	}
	head := ""
	if commitRunStart != nil {
		found, _, herr := headAndBranch(ctx, gitCmd, root)
		if herr != nil {
			return "", herr
		}
		head = found
	}
	if head != "" && head != commitRunStart.head {
		return fmt.Sprintf("could not arm the update (%s); this step changed nothing, but an earlier step of this update already moved the checkout to %s", launchErrorText(err), short(head)), nil
	}
	return fmt.Sprintf("could not arm the update (%s); the checkout was not changed", launchErrorText(err)), nil
}

// Writes the interrupted pull marker for one git tree move and returns its path
func armTreeMove(ctx context.Context, gitCmd []string, root string, move treeMove) (string, error) {
	marker, err := pm.InterruptedPullMarkerPath(root)
	if err != nil {
		return "", err
	}
	ref := ""
	if move.Rollback != "" {
		named, err := RunGit(ctx, gitCmd, []string{"symbolic-ref", "-q", "HEAD"}, ChildOptions{Dir: root, Stdin: DevNull, Capture: true, Text: true, Timeout: 60 * time.Second})
		if err != nil {
			return "", err
		}
		if named.ReturnCode == 0 {
			ref = pystr.Strip(named.Stdout)
		}
	}
	if move.Pre != "" {
		if _, err := publishRecoveryClosure(ctx, gitCmd, root, move.Pre); err != nil && !isUpdateOSError(err) && !isSubprocessError(err) {
			return "", err
		}
	}
	text := fmt.Sprintf("pid=%d\npre=%s\ntarget=%s\nstash=%s\n", os.Getpid(), move.Pre, move.Target, move.Stash) +
		"index_lock=" + lockIdentity(filepath.Join(filepath.Dir(marker), "index.lock")) + "\n"
	if move.Rollback != "" {
		text += "rollback=" + move.Rollback + "\nref=" + ref + "\n"
	}
	if git := absoluteGit(gitCmd); git != "" {
		text += "git=" + git + "\n"
	}
	if err := WriteDurableText(marker, text); err != nil {
		return "", err
	}
	return marker, nil
}

// Copies the launch repair's modules as committed at pre beside the marker and returns the closure directory
func publishRecoveryClosure(ctx context.Context, gitCmd []string, root, pre string) (string, error) {
	if !IsObjectID(pre) {
		return "", &pm.PyError{Class: "OSError", Message: "not a commit id: " + pyrepr.StrRepr(pre)}
	}
	dest, err := RecoveryClosureDirOf(root, pre)
	if err != nil {
		return "", err
	}
	if RecoveryClosureVerified(dest, pre) {
		return dest, nil
	}
	gitCmd = append(slices.Clone(gitCmd), "--no-replace-objects")
	listed, err := RunGit(ctx, gitCmd, append([]string{"ls-tree", "-z", "--full-tree", pre, "--"}, RecoveryClosure...),
		ChildOptions{Dir: root, Stdin: DevNull, Capture: true, Timeout: 120 * time.Second})
	if err != nil {
		return "", err
	}
	ids := map[string]string{}
	if listed.ReturnCode == 0 {
		for _, entry := range strings.Split(listed.Stdout, "\x00") {
			meta, rel, _ := strings.Cut(pystr.DecodeUTF8Replace([]byte(entry)), "\t")
			fields := strings.Fields(meta)
			if len(fields) < 2 || fields[1] != "blob" {
				continue
			}
			if len(fields) < 3 {
				return "", &pm.PyError{Class: "IndexError", Message: "list index out of range"}
			}
			ids[rel] = fields[2]
		}
	}
	shown, err := readTargetFiles(ctx, gitCmd, root, pre, RecoveryClosure)
	if err != nil {
		return "", err
	}
	blobs := []sourceFile{{Rel: RecoveryClosureInit, Data: []byte{}}}
	for _, rel := range RecoveryClosure {
		id, listedBlob := ids[rel]
		if !listedBlob || shown[rel] == nil || BlobID(shown[rel], pre) != id {
			return "", &pm.PyError{Class: "OSError", Message: rel + " is not in " + short(pre)}
		}
		blobs = append(blobs, sourceFile{Rel: rel, Data: shown[rel]})
	}
	var manifest strings.Builder
	for _, blob := range blobs {
		manifest.WriteString(BlobID(blob.Data, pre) + " " + blob.Rel + "\n")
	}
	blobs = append(blobs, sourceFile{Rel: RecoveryClosureManifest, Data: []byte(manifest.String())})
	staging := filepath.Join(filepath.Dir(dest), fmt.Sprintf(".%s.%d.staging", pre, os.Getpid()))
	_ = os.RemoveAll(staging)
	defer os.RemoveAll(staging)
	for _, blob := range blobs {
		if err := writeSyncedFile(filepath.Join(staging, filepath.FromSlash(blob.Rel)), blob.Data); err != nil {
			return "", err
		}
	}
	for _, directory := range []string{filepath.Join(staging, filepath.Dir(filepath.FromSlash(RecoveryClosureInit))), staging} {
		if err := fsyncDir(directory); err != nil {
			return "", err
		}
	}
	_ = os.RemoveAll(dest)
	if err := os.Rename(staging, dest); err != nil {
		return "", err
	}
	if err := fsyncDir(filepath.Dir(dest)); err != nil {
		return "", err
	}
	_ = os.RemoveAll(staging)
	entries, err := os.ReadDir(filepath.Dir(dest))
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		if stale := filepath.Join(filepath.Dir(dest), entry.Name()); stale != dest {
			_ = os.RemoveAll(stale)
		}
	}
	return dest, nil
}

// Writes data to a new file under freshly made parents and syncs it to disk
func writeSyncedFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		return err
	}
	handle, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o666)
	if err != nil {
		return err
	}
	if _, err := handle.Write(data); err != nil {
		handle.Close()
		return err
	}
	if err := handle.Sync(); err != nil {
		handle.Close()
		return err
	}
	return handle.Close()
}

// Makes a directory's entries durable on POSIX, Windows journaling the rename itself
func fsyncDir(directory string) error {
	if custodyWindows() {
		return nil
	}
	fd, err := os.OpenFile(directory, os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	defer fd.Close()
	return fd.Sync()
}

// Absolute path of the git this run moves the tree with, empty when it is not on PATH
func absoluteGit(gitCmd []string) string {
	head := "git"
	if len(gitCmd) > 0 {
		head = gitCmd[0]
	}
	found, ok := pyshutil.Which(head, nil)
	if !ok || found == "" {
		return ""
	}
	abs, err := pypath.Abspath(found)
	if err != nil {
		return ""
	}
	return abs
}

// Paths target adds over pre, which a rollback removes by name afterwards
func filesAddedBy(ctx context.Context, gitCmd []string, root, pre, target string) ([]string, error) {
	if target == "" || target == pre {
		return nil, nil
	}
	cp, err := RunGit(ctx, gitCmd, []string{"diff", "--name-only", "-z", "--no-renames", "--diff-filter=A", pre, target},
		ChildOptions{Dir: root, Stdin: DevNull, Capture: true, Text: true, Timeout: 120 * time.Second})
	if err != nil {
		return nil, err
	}
	var added []string
	if cp.ReturnCode == 0 {
		for _, path := range strings.Split(cp.Stdout, "\x00") {
			if path != "" {
				added = append(added, path)
			}
		}
	}
	return added, nil
}

// Removes the files a move added and their emptied directories, false while one is still there
func dropAddedFiles(root string, added []string) bool {
	gone := true
	for _, rel := range added {
		if err := pypath.Unlink(filepath.Join(root, filepath.FromSlash(rel))); err != nil && !errors.Is(err, fs.ErrNotExist) {
			gone = false
		}
	}
	seen := map[string]bool{}
	var parents []string
	for _, rel := range added {
		for _, parent := range pypath.PathParents(rel) {
			if pypath.PathStr(parent) != "." && !seen[parent] {
				seen[parent] = true
				parents = append(parents, parent)
			}
		}
	}
	depth := func(p string) int { return len(strings.Split(pypath.PathAsPosix(p), "/")) }
	sort.SliceStable(parents, func(i, j int) bool { return depth(parents[i]) > depth(parents[j]) })
	for _, parent := range parents {
		_ = syscall.Rmdir(filepath.Join(root, filepath.FromSlash(parent)))
	}
	for _, rel := range added {
		if lexists(filepath.Join(root, filepath.FromSlash(rel))) {
			return false
		}
	}
	return gone
}

// Whether HEAD is sha, no index lock is left and no tracked file differs from it
func treeWholeAt(ctx context.Context, gitCmd []string, root, sha string) (bool, error) {
	run := func(args ...string) (*CompletedProcess, error) {
		return RunGit(ctx, gitCmd, args, ChildOptions{Dir: root, Stdin: DevNull, Capture: true, Text: true, Timeout: 120 * time.Second})
	}
	head, err := run("rev-parse", "-q", "--verify", "HEAD")
	if err != nil {
		return false, err
	}
	marker, err := pm.InterruptedPullMarkerPath(root)
	if err != nil {
		return false, err
	}
	if head.ReturnCode != 0 || pystr.Strip(head.Stdout) != sha || statExists(filepath.Join(filepath.Dir(marker), "index.lock")) {
		return false, nil
	}
	status, err := run("status", "--porcelain", "-z", "--untracked-files=no")
	if err != nil {
		return false, err
	}
	return status.ReturnCode == 0 && strings.Trim(status.Stdout, "\x00") == "", nil
}

// Puts back what an unfinished tree move wrote, true when the tree is verified whole again
func settleFailedTreeMove(ctx context.Context, root string) (bool, error) {
	marker, err := pm.InterruptedPullMarkerPath(root)
	if err != nil {
		return false, err
	}
	if !isRegular(marker) {
		return false, nil
	}
	RestoreInterruptedPull(ctx, root, true)
	return !isRegular(marker), nil
}

// The requires-python specifier of a target's pyproject, false when absent or unreadable
func requiresPythonSpec(pyproject []byte) (string, bool) {
	if len(pyproject) == 0 || !utf8.Valid(pyproject) {
		return "", false
	}
	document, err := pytoml.Loads(string(pyproject))
	if err != nil {
		return "", false
	}
	project, ok := document["project"].(map[string]any)
	if !ok {
		return "", false
	}
	spec, ok := project["requires-python"].(string)
	return spec, ok
}

// Whether a requires-python specifier admits version, an unparsable specifier admitting everything
func specAdmits(spec, version string) (bool, error) {
	admits, invalid, err := pm.SpecifierSetAdmits(spec, version)
	if err != nil {
		return false, err
	}
	return invalid || admits, nil
}

// Whether a target's requires-python excludes the running interpreter
func requiresOtherPython(pyproject []byte) (bool, error) {
	spec, ok := requiresPythonSpec(pyproject)
	if !ok {
		return false, nil
	}
	running, err := pm.PinnedPythonFullVersion()
	if err != nil {
		return false, err
	}
	admits, err := specAdmits(spec, running)
	return !admits, err
}

// Stdout of a read only interpreter child, false when it could not run or exited nonzero
func runPython(ctx context.Context, argv []string, stdin string, timeout time.Duration) (string, bool) {
	done, err := pysubprocess.RunWith(ctx, argv, pysubprocess.Options{Input: []byte(stdin), Timeout: timeout, Attr: tools.WindowsHideAttr()})
	if err != nil || done == nil || done.Returncode != 0 {
		return "", false
	}
	return pystr.TranslateNewlines(pystr.DecodeUTF8Replace(done.Stdout)), true
}

// An installed python3.N the target's requires-python admits, newest first, false when the machine has none
func admittedPython(ctx context.Context, spec string) (string, bool, error) {
	_, minorText, err := pm.PinnedPythonVersion()
	if err != nil {
		return "", false, err
	}
	running, err := strconv.Atoi(minorText)
	if err != nil {
		return "", false, err
	}
	var candidates []string
	for minor := 40; minor > running && len(candidates) < 3; minor-- {
		if command := tools.LocateCommand(fmt.Sprintf("python3.%d", minor)); len(command) > 0 {
			candidates = append(candidates, command[0])
		}
	}
	for _, python := range candidates {
		out, _ := runPython(ctx, []string{python, "-I", "-c", pythonVersionProbe}, "", 30*time.Second)
		version := pystr.Strip(out)
		if version == "" {
			continue
		}
		admits, err := specAdmits(spec, version)
		if err != nil {
			return "", false, err
		}
		if admits {
			return python, true, nil
		}
	}
	return "", false, nil
}

// First source the running interpreter's compiler rejects, with its exception class and text
func compileHere(sources []sourceFile) (*syntaxFault, error) {
	for _, source := range sources {
		if err := pyast.Compile(source.Data, source.Rel); err != nil {
			var syntax *pyast.SyntaxError
			if errors.As(err, &syntax) {
				return &syntaxFault{Path: source.Rel, Error: syntax.Class + ": " + syntax.Error()}, nil
			}
			var other *pyast.LimitError
			if errors.As(err, &other) && other.Class == "ValueError" {
				return &syntaxFault{Path: source.Rel, Error: "ValueError: " + other.Msg}, nil
			}
			return nil, err
		}
	}
	return nil, nil
}

// First startup file that does not compile under the interpreter the target admits, nil when all do
func startupSyntaxError(ctx context.Context, sources []sourceFile, pyproject []byte) (*syntaxFault, error) {
	var present []sourceFile
	for _, source := range sources {
		if source.Data != nil {
			present = append(present, source)
		}
	}
	spec, has := requiresPythonSpec(pyproject)
	if !has {
		return compileHere(present)
	}
	running, err := pm.PinnedPythonFullVersion()
	if err != nil {
		return nil, err
	}
	admits, err := specAdmits(spec, running)
	if err != nil {
		return nil, err
	}
	if admits {
		return compileHere(present)
	}
	python, found, err := admittedPythonProbe(ctx, spec)
	if err != nil {
		return nil, err
	}
	if found {
		pairs := make([]any, 0, 2*len(present))
		for _, source := range present {
			pairs = append(pairs, source.Rel, base64.StdEncoding.EncodeToString(source.Data))
		}
		payload, err := pyjson.Dumps(pyjson.ObjectOf(pairs...))
		if err != nil {
			return nil, err
		}
		out, _ := runPython(ctx, []string{python, "-I", "-c", compileAll}, payload, 120*time.Second)
		if lines := pystr.Splitlines(pystr.Strip(out), false); len(lines) > 0 {
			if verdict, err := pyjson.Loads(lines[len(lines)-1]); err == nil {
				if verdict == nil {
					return nil, nil
				}
				if pair, ok := verdict.([]any); ok && len(pair) == 2 {
					return &syntaxFault{Path: pyrepr.Str(pair[0]), Error: pyrepr.Str(pair[1]) + " (judged by " + python + ")"}, nil
				}
			}
		}
	}
	broken, err := compileHere(present)
	if err != nil || broken == nil {
		return nil, err
	}
	return &syntaxFault{Path: broken.Path, Error: broken.Error + "\nJudged by Python " + running + ": the target requires Python " +
		spec + " and no installed interpreter satisfies it. Install one (`uv python install '" + spec +
		"'`) and re-run `icebath update` so the release is judged by its own Python."}, nil
}

// Bytes of each path as committed at targetRef from one cat-file batch, nil for an absent or unreadable path
func readTargetFiles(ctx context.Context, gitCmd []string, root, targetRef string, relpaths []string) (map[string][]byte, error) {
	var names []string
	found := map[string][]byte{}
	var request strings.Builder
	for _, rel := range relpaths {
		if _, seen := found[rel]; seen {
			continue
		}
		found[rel] = nil
		names = append(names, rel)
		request.WriteString(targetRef + ":" + rel + "\n")
	}
	input := request.String()
	cp, err := RunGit(ctx, gitCmd, []string{"cat-file", "--batch"}, ChildOptions{Dir: root, Input: &input, Capture: true, Timeout: 120 * time.Second})
	if err != nil {
		return nil, err
	}
	out := ""
	if cp.ReturnCode == 0 {
		out = cp.Stdout
	}
	pos := 0
	for _, rel := range names {
		if pos > len(out) {
			break
		}
		end := strings.IndexByte(out[pos:], '\n')
		if end < 0 {
			break
		}
		header := bytes.Fields([]byte(out[pos : pos+end]))
		pos += end + 1
		if len(header) != 3 || !asciiDigits(header[2]) {
			continue
		}
		size, err := strconv.Atoi(string(header[2]))
		if err != nil {
			size = len(out)
		}
		if string(header[1]) == "blob" {
			found[rel] = append([]byte{}, out[pos:min(pos+size, len(out))]...)
		}
		pos += size + 1
	}
	return found, nil
}

// Whether b is a nonempty run of ASCII digits
func asciiDigits(b []byte) bool {
	for _, c := range b {
		if c < '0' || c > '9' {
			return false
		}
	}
	return len(b) > 0
}

// First startup critical file that does not compile at targetRef, read from the object store before HEAD moves
func targetSyntaxError(ctx context.Context, w io.Writer, gitCmd []string, root, targetRef string, relpaths []string) (*syntaxFault, error) {
	wanted := append([]string{"pyproject.toml"}, relpaths...)
	files, err := readTargetFiles(ctx, gitCmd, root, targetRef, wanted)
	if err != nil {
		var timeout *updaterecord.GitTimeout
		if errors.As(err, &timeout) {
			fmt.Fprintln(w, "  ⚠ Syntax preflight skipped (slow object fetch); the post-update check still runs.")
			return nil, nil
		}
		return nil, err
	}
	var sources []sourceFile
	seen := map[string]bool{"pyproject.toml": true}
	for _, rel := range relpaths {
		if !seen[rel] {
			seen[rel] = true
			sources = append(sources, sourceFile{Rel: rel, Data: files[rel]})
		}
	}
	return startupSyntaxError(ctx, sources, files["pyproject.toml"])
}

// First startup critical file in the worktree at root that does not compile, the backstop after a move
func treeSyntaxError(ctx context.Context, root string, relpaths []string) (*syntaxFault, error) {
	var sources []sourceFile
	index := map[string]int{}
	for _, rel := range relpaths {
		var data []byte
		if path := filepath.Join(root, filepath.FromSlash(rel)); isRegular(path) {
			raw, err := os.ReadFile(path)
			if err != nil {
				return &syntaxFault{Path: rel, Error: "could not read: " + launchErrorText(err)}, nil
			}
			data = append([]byte{}, raw...)
		}
		if at, seen := index[rel]; seen {
			sources[at].Data = data
			continue
		}
		index[rel] = len(sources)
		sources = append(sources, sourceFile{Rel: rel, Data: data})
	}
	var pyproject []byte
	if path := filepath.Join(root, "pyproject.toml"); isRegular(path) {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		pyproject = raw
	}
	return startupSyntaxError(ctx, sources, pyproject)
}

// HEAD commit and abbreviated branch of the checkout, each empty when git cannot name it
func headAndBranch(ctx context.Context, gitCmd []string, root string) (string, string, error) {
	out := func(args ...string) (string, error) {
		cp, err := RunGit(ctx, gitCmd, args, ChildOptions{Dir: root, Stdin: DevNull, Capture: true, Text: true, Timeout: 60 * time.Second})
		if err != nil {
			return "", err
		}
		if cp.ReturnCode != 0 {
			return "", nil
		}
		return pystr.Strip(cp.Stdout), nil
	}
	head, err := out("rev-parse", "HEAD")
	if err != nil {
		return "", "", err
	}
	branch, err := out("rev-parse", "--abbrev-ref", "HEAD")
	return head, branch, err
}

// Whether the checkout is exactly where the run found it with no autostash taken and no tree move armed
func checkoutUntouched(ctx context.Context, gitCmd []string, root string, start *[2]string) (bool, error) {
	if start == nil || start[0] == "" {
		return false, nil
	}
	if _, pending := unrestoredAutostashNotice(); pending {
		return false, nil
	}
	marker, err := pm.InterruptedPullMarkerPath(root)
	if err != nil {
		return false, err
	}
	if isRegular(marker) {
		return false, nil
	}
	head, branch, err := headAndBranch(ctx, gitCmd, root)
	return err == nil && head == start[0] && branch == start[1], err
}

// Forgets the snapshot, stake and run start of an earlier update attempt in this process
func beginUpdateAttempt() {
	commitOwner = ""
	armedSnapshot = nil
	clear(armedBytes)
	commitRunStart = nil
	obligationRoot = nil
}

// Records the git command, HEAD and branch this run reached its checkout phase with
func recordRunStart(ctx context.Context, gitCmd []string, root string) error {
	head, branch, err := headAndBranch(ctx, gitCmd, root)
	if err != nil {
		return err
	}
	commitRunStart = &commitRun{gitCmd: slices.Clone(gitCmd), head: head, branch: branch}
	obligationRoot = &root
	return nil
}

// Whether this run's checkout is untouched, true before the checkout phase began
func runCheckoutUntouched(ctx context.Context, root string) (bool, error) {
	if commitRunStart == nil {
		return true, nil
	}
	return checkoutUntouched(ctx, commitRunStart.gitCmd, root, &[2]string{commitRunStart.head, commitRunStart.branch})
}

// Why this update must not start, checked before the first tree move, empty to proceed
func preflightRefusal(ctx context.Context, w io.Writer, gitCmd []string, root, targetRef string, criticalFiles []string) (string, error) {
	if !ownsLiveCheckout(root) {
		if err := RefuseForeignOwnedVenv(ctx, root); err != nil {
			recordStop(ctx, "venv_foreign_owner")
			return "✗ " + launchErrorText(err), nil
		}
	}
	broken, err := targetSyntaxError(ctx, w, gitCmd, root, targetRef, criticalFiles)
	if err != nil {
		return "", err
	}
	if broken != nil {
		recordStop(ctx, "target_syntax_error")
		lines := pystr.Splitlines(broken.Error, false)
		if len(lines) > 6 {
			lines = lines[:6]
		}
		return "✗ The update target has a syntax error in a critical file:\n  " + broken.Path + "\n    " + strings.Join(lines, "\n    "), nil
	}
	return "", nil
}
