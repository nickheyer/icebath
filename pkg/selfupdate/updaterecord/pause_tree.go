package updaterecord

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/pm"
)

const (
	maxMoveTargets  = 16
	gitQueryTimeout = 30 * time.Second
)

var (
	pauseRunGit           = RunGitQuery
	pauseInstalledPackage = pm.Installed
	pauseReadInstallStamp = pm.ReadInstallStamp
	pauseVenvIsCurrent    = pm.VenvIsCurrent
	pauseDepsHoldResume   = depsHoldResume
)

func isRegular(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// PATH's git, else the git pm recorded or staged in its store, else a bare git
func gitExecutable(ctx context.Context) string {
	if found, err := exec.LookPath("git"); err == nil || (errors.Is(err, exec.ErrDot) && found != "") {
		return found
	}
	recorded, err := pauseInstalledPackage(ctx, "git", true)
	if err != nil {
		return "git"
	}
	if recorded != nil && recorded.Binary != "" && isRegular(recorded.Binary) {
		return recorded.Binary
	}
	pkg, err := pm.GetPackage("git")
	if err != nil {
		return "git"
	}
	target, err := pm.CurrentTarget()
	if err != nil {
		return "git"
	}
	version, ok := pm.NewLockfile(pm.LockfilePath()).Version("git")
	if !ok {
		return "git"
	}
	store, err := pm.StoreRoot(ctx)
	if err != nil {
		return "git"
	}
	if staged := pkg.Binary(filepath.Join(store, pkg.StoreEntry(version, target)), target); staged != "" && isRegular(staged) {
		return staged
	}
	return "git"
}

// Read only git query on root in the updater's custody, nil when git cannot run
func pauseGit(ctx context.Context, root string, args ...string) *GitResult {
	result, err := pauseRunGit(ctx, []string{gitExecutable(ctx), "-C", root}, args, gitQueryTimeout)
	if err != nil {
		return nil
	}
	return result
}

// Commit HEAD names, empty when git cannot read it
func HeadSHA(ctx context.Context, root string) string {
	result := pauseGit(ctx, root, "rev-parse", "--verify", "HEAD")
	if result == nil || result.ReturnCode != 0 {
		return ""
	}
	return pystr.Strip(result.Stdout)
}

// Sorted tracked paths differing from HEAD in index or worktree, false when git cannot say
func TrackedChanges(ctx context.Context, root string) ([]string, bool) {
	result := pauseGit(ctx, root, "status", "--porcelain=v1", "-z", "--untracked-files=no")
	if result == nil || result.ReturnCode != 0 {
		return nil, false
	}
	paths := map[string]bool{}
	entries := strings.Split(result.Stdout, "\x00")
	for index := 0; index < len(entries); {
		entry := []rune(entries[index])
		index++
		if len(entry) < 4 {
			continue
		}
		paths[string(entry[3:])] = true
		if entry[0] == 'R' || entry[0] == 'C' {
			index++
		}
	}
	return sortedKeys(paths), true
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Baselines of a token as mappings, raising as attribute access on a non mapping entry does
func baselineDicts(v any) ([]map[string]any, error) {
	if !truthy(v) {
		return nil, nil
	}
	list, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("'%s' object has no attribute 'get'", pyrepr.TypeName(v))
	}
	out := make([]map[string]any, 0, len(list))
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("'%s' object has no attribute 'get'", pyrepr.TypeName(item))
		}
		out = append(out, m)
	}
	return out, nil
}

// Adds this pause's tree baseline once per pause id beside the baselines of absorbed pauses
func StampTree(ctx context.Context, token map[string]any, root string) (map[string]any, error) {
	if root == "" {
		root = PauseInstallRoot()
	}
	if _, ok := token["pause_id"]; !ok {
		token["pause_id"] = uuid4Hex()
	}
	pauseID := token["pause_id"]
	if _, ok := token["baselines"]; !ok {
		token["baselines"] = []any{}
	}
	list, isList := token["baselines"].([]any)
	if !isList {
		return nil, fmt.Errorf("'%s' object is not iterable", pyrepr.TypeName(token["baselines"]))
	}
	baselines, err := baselineDicts(list)
	if err != nil {
		return nil, err
	}
	for _, b := range baselines {
		if pyrepr.Equal(b["pause_id"], pauseID) {
			return token, nil
		}
	}
	if state := TreeState(ctx, root); state != nil {
		entry := map[string]any{"pause_id": pauseID}
		for k, v := range state {
			entry[k] = v
		}
		token["baselines"] = append(list, entry)
	}
	return token, nil
}

// HEAD, the tracked changes and their digests now, nil when root is not a git checkout
func TreeState(ctx context.Context, root string) map[string]any {
	if !pathExists(filepath.Join(root, ".git")) {
		return nil
	}
	dirty, ok := TrackedChanges(ctx, root)
	var dirtyAt any
	digests := map[string]any{}
	if ok {
		names := make([]any, len(dirty))
		for i, path := range dirty {
			names[i] = path
			digests[path] = fileDigest(filepath.Join(root, path))
		}
		dirtyAt = names
	}
	var pre any
	if head := HeadSHA(ctx, root); head != "" {
		pre = head
	}
	return map[string]any{"pre_sha": pre, "dirty_at_pause": dirtyAt, "dirty_digests": digests}
}

// Sha256 hex of a file's bytes, nil when it is deleted or unreadable
func fileDigest(path string) any {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// Persists this pause's move target before git writes the tree, an error meaning the move must not run
func MarkMove(ctx context.Context, token map[string]any, target string) error {
	if token == nil || !truthy(token["pause_id"]) || target == "" {
		return nil
	}
	baselines, err := baselineDicts(token["baselines"])
	if err != nil {
		return err
	}
	for _, b := range baselines {
		if pyrepr.Equal(b["pause_id"], token["pause_id"]) {
			set := strSet(b["move_targets"])
			set[target] = true
			b["move_targets"] = sortedSet(set)
		}
	}
	copied := shallowCopy(token)
	copied["resume_needed"] = true
	return WritePause(ctx, copied, nil, "")
}

func stringItems(v any) []string {
	items := listItems(orEmptyList(v))
	out := make([]string, len(items))
	for i, item := range items {
		out[i] = pyrepr.Str(item)
	}
	return out
}

// Tracked paths differing between HEAD and any target, false when git cannot say
func pathsBetween(ctx context.Context, root string, targets []string) (map[string]bool, bool) {
	sorted := append([]string{}, targets...)
	sort.Strings(sorted)
	paths := map[string]bool{}
	for _, target := range sorted {
		diff := pauseGit(ctx, root, "diff", "--name-only", "-z", "--no-renames", "HEAD", target, "--")
		if diff == nil || diff.ReturnCode != 0 {
			return nil, false
		}
		for _, path := range strings.Split(diff.Stdout, "\x00") {
			if path != "" {
				paths[path] = true
			}
		}
	}
	return paths, true
}

// Paths between HEAD and the fetched or tracked commits, false when git cannot say or nothing was fetched
func pathsAMoveCouldWrite(ctx context.Context, root string) (map[string]bool, bool) {
	where := pauseGit(ctx, root, "rev-parse", "--git-path", "FETCH_HEAD")
	refs := pauseGit(ctx, root, "for-each-ref", "--format=%(objectname)", "refs/remotes")
	if where == nil || where.ReturnCode != 0 || refs == nil || refs.ReturnCode != 0 {
		return nil, false
	}
	fetchHead := pystr.Strip(where.Stdout)
	if !filepath.IsAbs(fetchHead) {
		fetchHead = filepath.Join(root, fetchHead)
	}
	var fetched []string
	raw, err := os.ReadFile(fetchHead)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, false
	default:
		raw = bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf"))
		fetched = strings.Split(pystr.TranslateNewlines(pystr.DecodeUTF8Replace(raw)), "\n")
	}
	targets := map[string]bool{}
	for _, line := range fetched {
		if fields := pystr.SplitWhitespace(line, -1); len(fields) > 0 {
			targets[fields[0]] = true
		}
	}
	for _, ref := range pystr.SplitWhitespace(refs.Stdout, -1) {
		targets[ref] = true
	}
	if len(targets) == 0 || len(targets) > maxMoveTargets {
		return nil, false
	}
	return pathsBetween(ctx, root, sortedKeys(targets))
}

// Whether path was dirty at the pause and still holds the bytes the pause saw
func seenAtPause(root string, baseline map[string]any, path string) (bool, error) {
	seen, err := pyContains(orEmptyList(baseline["dirty_at_pause"]), path)
	if err != nil || !seen {
		return false, err
	}
	digests := baseline["dirty_digests"]
	if digests == nil {
		return true, nil
	}
	m, ok := digests.(map[string]any)
	if !ok {
		return false, fmt.Errorf("'%s' object has no attribute 'get'", pyrepr.TypeName(digests))
	}
	return pyrepr.Equal(m[path], fileDigest(filepath.Join(root, path))), nil
}

// Changes at an unmoved HEAD the update's move may have left half written, for the first baseline with any
func halfWritten(ctx context.Context, root string, changes []string, atHead []map[string]any) ([]string, error) {
	for _, baseline := range atHead {
		var unexpected []string
		for _, path := range changes {
			seen, err := seenAtPause(root, baseline, path)
			if err != nil {
				return nil, err
			}
			if !seen {
				unexpected = append(unexpected, path)
			}
		}
		sort.Strings(unexpected)
		if len(unexpected) == 0 {
			continue
		}
		targets, paths := baseline["move_targets"], baseline["move_paths"]
		writable, known := map[string]bool{}, true
		switch {
		case truthy(targets):
			writable, known = pathsBetween(ctx, root, stringItems(targets))
		case truthy(paths):
		default:
			writable, known = pathsAMoveCouldWrite(ctx, root)
		}
		torn := unexpected
		if known {
			allowed := strSet(paths)
			for path := range writable {
				allowed[path] = true
			}
			torn = nil
			for _, path := range unexpected {
				if allowed[path] {
					torn = append(torn, path)
				}
			}
		}
		if len(torn) > 0 {
			return torn, nil
		}
	}
	return nil, nil
}

// Paths a move from found's HEAD wrote that lack the current commit's bytes, false when git cannot say
func TornByMove(ctx context.Context, root string, found map[string]any, changes []string, changesKnown bool) ([]string, bool, error) {
	var writable map[string]bool
	known := false
	if truthy(found["pre_sha"]) {
		writable, known = pathsBetween(ctx, root, []string{pyrepr.Str(found["pre_sha"])})
	}
	if !known || !changesKnown {
		return nil, false, nil
	}
	torn := []string{}
	for _, path := range changes {
		if !writable[path] {
			continue
		}
		seen, err := seenAtPause(root, found, path)
		if err != nil {
			return nil, false, err
		}
		if !seen {
			torn = append(torn, path)
		}
	}
	sort.Strings(torn)
	return torn, true, nil
}

// Changes at a HEAD a recorded move reached that are not that commit's bytes
func landedTorn(ctx context.Context, root, head string, changes []string, landed []map[string]any) ([]string, error) {
	for _, baseline := range landed {
		verdicts := baseline["landed"]
		if !truthy(verdicts) {
			verdicts = map[string]any{}
		}
		m, ok := verdicts.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("'%s' object has no attribute 'get'", pyrepr.TypeName(verdicts))
		}
		verdict, recorded := m[head]
		var paths []string
		known := true
		if !recorded || verdict == nil {
			var err error
			if paths, known, err = TornByMove(ctx, root, baseline, changes, true); err != nil {
				return nil, err
			}
		} else {
			paths = stringItems(verdict)
		}
		var torn []string
		if !known {
			torn = append(torn, changes...)
		} else {
			inChanges := map[string]bool{}
			for _, path := range changes {
				inChanges[path] = true
			}
			for _, path := range paths {
				if inChanges[path] {
					torn = append(torn, path)
				}
			}
		}
		if len(torn) > 0 {
			return torn, nil
		}
	}
	return nil, nil
}

// Whether stale dependencies at a moved HEAD hold the set, only where the next launch syncs them
func depsHoldResume(ctx context.Context, root string) (holds bool) {
	switch pystr.Lower(os.Getenv("ICEBATH_DISABLE_LAZY_INSTALLS")) {
	case "1", "true", "yes":
		return false
	}
	if !pathExists(filepath.Join(root, ".git")) || !isRegular(filepath.Join(root, "pyproject.toml")) {
		return false
	}
	defer func() {
		if recover() != nil {
			holds = false
		}
	}()
	mechanism, _ := pauseReadInstallStamp(root).Get("updateMechanism")
	if !pyrepr.Equal(mechanism, "self") {
		return false
	}
	current, err := pauseVenvIsCurrent(ctx, pm.VenvIsCurrentOptions{ProjectRoot: root})
	if err != nil {
		return false
	}
	return !current
}

// Whether every pause already found HEAD unreadable and none bound a tree move since
func pausedOnUnreadableHead(token map[string]any) (bool, error) {
	baselines, err := baselineDicts(token["baselines"])
	if err != nil || len(baselines) == 0 {
		return false, err
	}
	for _, b := range baselines {
		if b["pre_sha"] != nil || truthy(b["move_targets"]) || truthy(b["move_paths"]) || truthy(b["landed"]) {
			return false, nil
		}
	}
	return true, nil
}

// Whether paused gateways may start on this checkout now, with the reason when not
func TreeIsWhole(ctx context.Context, token map[string]any, root string) (bool, string, error) {
	if root == "" {
		root = PauseInstallRoot()
	}
	if pathExists(filepath.Join(root, ".git")) {
		marker, err := pm.InterruptedPullMarkerPath(root)
		if err != nil {
			return false, "", err
		}
		if pathExists(marker) {
			return false, "the checkout is mid-pull (interrupted-pull marker present)", nil
		}
		head := HeadSHA(ctx, root)
		if head == "" {
			unreadable, err := pausedOnUnreadableHead(token)
			if err != nil {
				return false, "", err
			}
			if unreadable {
				return true, "", nil
			}
			return false, "the checkout HEAD is unreadable", nil
		}
		baselines, err := baselineDicts(token["baselines"])
		if err != nil {
			return false, "", err
		}
		var atHead, landed []map[string]any
		for _, b := range baselines {
			if pyrepr.Equal(b["pre_sha"], head) {
				atHead = append(atHead, b)
				continue
			}
			inLanded, err := pyContains(orEmptyMapValue(b["landed"]), head)
			if err != nil {
				return false, "", err
			}
			inTargets, err := pyContains(orEmptyList(b["move_targets"]), head)
			if err != nil {
				return false, "", err
			}
			if inLanded || inTargets {
				landed = append(landed, b)
			}
		}
		if len(atHead) > 0 || len(landed) > 0 {
			changes, ok := TrackedChanges(ctx, root)
			if !ok {
				return false, "git cannot read the checkout state", nil
			}
			torn, err := halfWritten(ctx, root, changes, atHead)
			if err == nil && len(torn) == 0 {
				torn, err = landedTorn(ctx, root, head, changes, landed)
			}
			if err != nil {
				return false, "", err
			}
			if len(torn) > 0 {
				return false, fmt.Sprintf("the checkout has %d file(s) git left half-written (e.g. %s)", len(torn), torn[0]), nil
			}
			if len(atHead) > 0 {
				return true, "", nil
			}
		}
	}
	if pauseDepsHoldResume(ctx, root) {
		return false, "dependencies are not current for the updated code yet", nil
	}
	return true, "", nil
}

func orEmptyMapValue(v any) any {
	if !truthy(v) {
		return map[string]any{}
	}
	return v
}

// Merges the adopted set, stamps the tree and persists under this process before the first stop
func RecordPause(ctx context.Context, token, adopted map[string]any, claims []string) (map[string]any, error) {
	if adopted != nil {
		if _, err := MergePauseInto(token, adopted); err != nil {
			return nil, err
		}
	}
	if _, err := StampTree(ctx, token, ""); err != nil {
		return nil, err
	}
	if _, ok := token["stop_requested"]; !ok {
		token["stop_requested"] = []any{}
	}
	if err := WritePause(ctx, token, PauseIdentity(0), ""); err != nil {
		return nil, err
	}
	pauseTransferHook("record")
	if err := ReleasePauseClaims(ctx, claims); err != nil {
		return nil, err
	}
	return token, nil
}

// Git the updater runs, PATH's first and then the one pm recorded or staged
func GitExecutable(ctx context.Context) string { return gitExecutable(ctx) }
