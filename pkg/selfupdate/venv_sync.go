package selfupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

// Relaunch driven completion retries back off after this many consecutive failures
const CompletionRetryBackoffAttempts = 2

// Seconds a backed off completion tail waits before it runs again
const CompletionRetryBackoffSeconds = 15 * 60

// Consecutive completion failures after which a launch leaves the tail to an explicit update
const CompletionRetryMaxAttempts = 6

// Hidden leading argument marking the one child a relaunch starts, never the processes it spawns
const RelaunchedFlag = "--icebath-relaunched"

// Hidden entries that run the completion tails, each a tail process identity
const (
	SourceCompletionEntry = "source-completion"
	UpdateCompletionEntry = "update-completion"
)

// Metadata queries answered from the tree alone, never waiting on a source update completion
var metadataFlags = map[string]bool{"-h": true, "--help": true, "-V": true, "--version": true}

// Update flags that report and exit without changing anything
var reportOnlyUpdateFlags = map[string]bool{"--check": true, "--plan": true, "--list-venv-holders": true}

// Completion tails that run the CLI while their pending marker is armed
var tailScripts = map[string]bool{SourceCompletionEntry: true, UpdateCompletionEntry: true}

// Inputs a launch time dependency sync consumes, a failed sync recorded against their digest
var syncInputs = []string{"pyproject.toml", "uv.lock", "pkg/pm/lock.json"}

// Legacy dependency markers the early recovery reads
var legacyMarkers = []string{".update-incomplete", ".lazy-refresh-incomplete"}

var relaunched atomic.Bool

// Seams for the module attributes the hermes tests replace
var (
	venvSyncProjectRoot = pm.RepoRoot
	venvIsCurrent       = func(ctx context.Context, root string) (bool, error) {
		return pm.VenvIsCurrent(ctx, pm.VenvIsCurrentOptions{ProjectRoot: root})
	}
	syncVenv                            = pm.SyncVenv
	ensureToolsForSync                  = pm.EnsureToolsForSync
	publishLaunchers                    = func(ctx context.Context, root string) error { return pm.PublishLaunchers(ctx, root, true) }
	resolveStorePython                  = pm.ResolveInstallBinary
	runningExecutable                   = os.Executable
	completionPendingPath               = defaultCompletionPendingPath
	treeMatchesCompletedStamp           = defaultTreeMatchesCompletedStamp
	finishSourceUpdate                  = defaultFinishSourceUpdate
	activationEnvironment               = pm.ActivationEnvironment
	venvSelectedVenv                    = pm.SelectedVenv
	lstatOwner                          = defaultLstatOwner
	completionCustodyRun                = Run
	launchStderr              io.Writer = os.Stderr
)

// Outcome of one dependency report or sync, detail present only on failure
type VenvSyncResult struct {
	State  string
	OK     bool
	Detail string
}

// Ordered JSON object of the result, the dict post update and the installers read
func (r VenvSyncResult) object() *pyjson.Object {
	object := pyjson.ObjectOf("state", r.State, "ok", r.OK)
	if r.Detail != "" {
		object.Set("detail", r.Detail)
	}
	return object
}

func existsFollow(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func isRegular(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Mode().IsRegular()
}

func isDirectoryPath(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}

func runtimeErr(message string) error { return &pm.PyError{Class: "RuntimeError", Message: message} }

// st_mtime of a stat result as CPython computes it, seconds plus nanoseconds in double precision
func statMtime(st os.FileInfo) float64 {
	t := st.ModTime()
	return float64(t.Unix()) + float64(t.Nanosecond())*1e-9
}

// Writes one line to the launch's stderr
func sayLaunch(format string, args ...any) { fmt.Fprintf(launchStderr, format+"\n", args...) }

// Reports or syncs this checkout's dependencies, a malformed install stamp failing as a build error
func SyncTree(ctx context.Context, projectRoot string, check bool) (VenvSyncResult, error) {
	root := projectRoot
	if root == "" {
		root = venvSyncProjectRoot()
	}
	sealed, err := pm.IsSealedTree(root)
	if err != nil {
		return VenvSyncResult{}, err
	}
	if sealed {
		return VenvSyncResult{State: "sealed", OK: true}, nil
	}
	if !isRegular(filepath.Join(root, "pyproject.toml")) {
		return VenvSyncResult{State: "failed", Detail: "no pyproject.toml under " + root}, nil
	}
	result, err := syncChecked(ctx, root, check)
	if err != nil {
		return VenvSyncResult{State: "failed", Detail: err.Error()}, nil
	}
	return result, nil
}

// Sync body whose every failure becomes a failed result
func syncChecked(ctx context.Context, root string, check bool) (VenvSyncResult, error) {
	current, err := venvIsCurrent(ctx, root)
	if err != nil {
		return VenvSyncResult{}, err
	}
	if current {
		if !check {
			if err := publishLaunchers(ctx, root); err != nil {
				return VenvSyncResult{}, err
			}
		}
		return VenvSyncResult{State: "current", OK: true}, nil
	}
	if check {
		return VenvSyncResult{State: "would-sync", OK: true}, nil
	}
	PublishStage("Updating Python dependencies")
	if err := RefuseForeignOwnedVenv(ctx, root); err != nil {
		return VenvSyncResult{}, err
	}
	if err := syncVenv(ctx, pm.SyncVenvOptions{Explicit: true, ProjectRoot: root, EvictIncompatiblePlugins: true}); err != nil {
		return VenvSyncResult{}, err
	}
	if err := pm.CollectSupersededGenerations(ctx, root); err != nil {
		return VenvSyncResult{}, err
	}
	if err := publishLaunchers(ctx, root); err != nil {
		return VenvSyncResult{}, err
	}
	return VenvSyncResult{State: "synced", OK: true}, nil
}

// Reports the recovery diagnostic that must never run an update tail
func isReadOnlyGatewayStatus(command []string) bool {
	return len(command) >= 2 && command[0] == "gateway" && command[1] == "status"
}

func defaultCompletionPendingPath(ctx context.Context, projectRoot string) string {
	return filepath.Join(pm.InstallStateDir(ctx, projectRoot), "source-completion-pending")
}

// Marker of a source update whose dependency sync committed but whose tail has not finished
func CompletionPendingPath(ctx context.Context, projectRoot string) string {
	return completionPendingPath(ctx, projectRoot)
}

// Persists the tail obligation before a new dependency generation is selected
func ArmCompletion(ctx context.Context, projectRoot string) (string, error) {
	pending := CompletionPendingPath(ctx, projectRoot)
	if err := os.MkdirAll(filepath.Dir(pending), 0o777); err != nil {
		return "", err
	}
	if err := os.WriteFile(pending, []byte("source update tail not finished\n"), 0o666); err != nil {
		return "", err
	}
	return pending, nil
}

// Attempt record of the owed tail beside its pending marker
func completionAttemptsPath(ctx context.Context, projectRoot string) string {
	return filepath.Join(filepath.Dir(CompletionPendingPath(ctx, projectRoot)), "source-completion-attempts")
}

// Digest of the sync inputs, a missing or unreadable input counting as a dash
func syncInputsDigest(root string) string {
	digest := sha256.New()
	for _, name := range syncInputs {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
		if err != nil {
			digest.Write([]byte("-"))
		} else {
			digest.Write(data)
		}
		digest.Write([]byte{0})
	}
	return hex.EncodeToString(digest.Sum(nil))
}

// Attempt count and inputs digest of a record, an error when it is unreadable or its count is no integer
func readCompletionAttempts(record string) (*big.Int, string, bool, error) {
	data, err := os.ReadFile(record)
	if err != nil {
		return nil, "", false, err
	}
	text, err := pystr.DecodeUTF8Sig(data)
	if err != nil {
		return nil, "", false, err
	}
	words := pystr.SplitWhitespace(text, -1)
	attempts := new(big.Int)
	if len(words) > 0 {
		n, ok := pyint.Parse(words[0])
		if !ok {
			return nil, "", false, &pm.PyError{Class: "ValueError", Message: "invalid literal for int() with base 10: " + pyrepr.StrRepr(words[0])}
		}
		attempts = n
	}
	if len(words) > 1 {
		return attempts, words[1], true, nil
	}
	return attempts, "", false, nil
}

// Tracks consecutive tail failures, a failure for other inputs restarting the count and success clearing it
func recordCompletionAttempt(ctx context.Context, root string, failed bool, inputs string) error {
	record := completionAttemptsPath(ctx, root)
	if !failed {
		if err := os.Remove(record); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	attempts, recorded, hasRecorded, err := readCompletionAttempts(record)
	if err != nil {
		attempts, hasRecorded = new(big.Int), false
	}
	if inputs != "" && hasRecorded && recorded != inputs {
		attempts = new(big.Int)
	}
	body := new(big.Int).Add(attempts, big.NewInt(1)).String() + "\n"
	if inputs != "" {
		body += inputs + "\n"
	}
	_ = os.WriteFile(record, []byte(body), 0o666)
	return nil
}

// Whether the owed tail may run now, its attempts and the backoff seconds, never failing
func CompletionRetryState(ctx context.Context, projectRoot string, now func() float64) (bool, *big.Int, int64) {
	record := completionAttemptsPath(ctx, projectRoot)
	attempts, inputs, hasInputs, err := readCompletionAttempts(record)
	if err != nil {
		return true, new(big.Int), 0
	}
	if attempts.Sign() <= 0 || (hasInputs && inputs != syncInputsDigest(projectRoot)) {
		return true, new(big.Int), 0
	}
	if attempts.Cmp(big.NewInt(CompletionRetryMaxAttempts)) >= 0 {
		return false, attempts, 0
	}
	if attempts.Cmp(big.NewInt(CompletionRetryBackoffAttempts)) < 0 {
		return true, attempts, 0
	}
	st, err := os.Stat(record)
	if err != nil {
		return true, attempts, 0
	}
	mtime := statMtime(st)
	if now == nil {
		now = func() float64 { return float64(time.Now().UnixNano()) / 1e9 }
	}
	age := max(0.0, now()-mtime)
	if age >= CompletionRetryBackoffSeconds {
		return true, attempts, CompletionRetryBackoffSeconds
	}
	return false, attempts, int64(CompletionRetryBackoffSeconds - age)
}

// Drops the pending marker and its attempt record
func ClearCompletion(ctx context.Context, root string) error {
	for _, path := range []string{CompletionPendingPath(ctx, root), completionAttemptsPath(ctx, root)} {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

// Refuses cross user mutation before PM changes the selected environment
func RefuseForeignOwnedVenv(ctx context.Context, projectRoot string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	uid := os.Geteuid()
	root := projectRoot
	selected, err := venvSelectedVenv(ctx, root)
	if err != nil {
		return err
	}
	candidates := []string{root, filepath.Join(root, "venv"), filepath.Join(root, ".venv"), filepath.Join(root, ".icebath"), selected}
	for _, venv := range []string{filepath.Join(root, "venv"), filepath.Join(root, ".venv"), selected} {
		directories := []string{filepath.Join(venv, "bin")}
		sites, err := globDirOrder(filepath.Join(venv, "lib"), "python*", "site-packages")
		if err != nil {
			return err
		}
		directories = append(directories, sites...)
		for _, directory := range directories {
			if !isDirectoryPath(directory) {
				continue
			}
			candidates = append(candidates, directory)
			entries, err := scandirNames(directory)
			if err != nil {
				return err
			}
			for _, name := range entries[:min(len(entries), 2000)] {
				entry := filepath.Join(directory, name)
				candidates = append(candidates, entry)
				if strings.HasSuffix(name, ".dist-info") && isDirectoryPath(entry) {
					inner, err := scandirNames(entry)
					if err != nil {
						return err
					}
					for _, file := range inner[:min(len(inner), 100)] {
						candidates = append(candidates, filepath.Join(entry, file))
					}
				}
			}
		}
	}
	for _, path := range candidates {
		owner, err := lstatOwner(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if owner != uid {
			return runtimeErr(fmt.Sprintf("refusing to update %s: %s is owned by uid %d, not the current uid %d; repair ownership before retrying", root, path, owner, uid))
		}
	}
	return nil
}

// Names of a directory's entries in the order the directory yields them, as iterdir lists them
func scandirNames(directory string) ([]string, error) {
	f, err := os.Open(directory)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return f.Readdirnames(-1)
}

// Matches of parent glob pattern then a fixed child, in directory order the way Path.glob walks
func globDirOrder(parent, pattern, child string) ([]string, error) {
	names, err := scandirNames(parent)
	if err != nil {
		return nil, nil
	}
	var out []string
	for _, name := range names {
		if ok, _ := filepath.Match(pattern, name); !ok {
			continue
		}
		candidate := filepath.Join(parent, name, child)
		if _, err := os.Lstat(candidate); err == nil {
			out = append(out, candidate)
		}
	}
	return out, nil
}

// Reports a launcher marked child booted by a manager, an explicit false never suppressing the tail
func supervisedChild() bool {
	for _, name := range []string{"ICEBATH_SUPERVISED_CHILD", "ICEBATH_S6_SUPERVISED_CHILD"} {
		switch pystr.Lower(pystr.Strip(os.Getenv(name))) {
		case "1", "true", "yes", "on":
			return true
		}
	}
	return false
}

// Exact own entry identity of a completion tail of this tree
func isTailScript(root, script string) bool {
	if !tailScripts[script] {
		return false
	}
	exe, err := runningExecutable()
	if err != nil {
		return false
	}
	resolved, err := pypath.Resolve(exe)
	if err != nil {
		return false
	}
	return filepath.Dir(resolved) == root
}

// Reports the checkout as the exact clean tree the last install or update completed
func defaultTreeMatchesCompletedStamp(ctx context.Context, root string) (bool, error) {
	stamp := pm.ReadInstallStamp(root)
	commitValue, _ := stamp.Get("commit")
	commit, ok := commitValue.(string)
	_, adopted := stamp.Get("adoptedAt")
	dirty, hasDirty := stamp.Get("dirty")
	if !ok || commit == "" || adopted || !hasDirty || dirty != false {
		return false, nil
	}
	pending := CompletionPendingPath(ctx, root)
	if isRegular(pending) {
		pendingStat, err := os.Stat(pending)
		if err != nil {
			return false, err
		}
		stampStat, err := os.Stat(filepath.Join(root, "install-stamp.json"))
		if err != nil {
			return false, err
		}
		if statMtime(pendingStat) >= statMtime(stampStat) {
			return false, nil
		}
	}
	info := updaterecord.GitVersionInfo(root, true)
	return info.Commit != nil && *info.Commit == commit && !info.Dirty, nil
}

// Whether this launch runs the owed repair now, a suppressed one saying why when it matters
func mayRetryCompletion(ctx context.Context, root string, command []string, current bool) bool {
	if !current && len(command) > 0 && command[0] == "update" {
		reportOnly := false
		for _, word := range command {
			reportOnly = reportOnly || reportOnlyUpdateFlags[word]
		}
		if !reportOnly {
			return true
		}
	}
	mayRetry, attempts, backoff := CompletionRetryState(ctx, root, nil)
	switch {
	case !mayRetry && attempts.Cmp(big.NewInt(CompletionRetryMaxAttempts)) >= 0:
		sayLaunch("icebath: a source update could not be finished automatically (%s attempts); run `icebath update` from a shell to finish it", attempts.String())
	case !mayRetry && !current:
		sayLaunch("icebath: running with the previous dependencies after %s failed syncs; next automatic retry in %d min, or run `icebath update` to finish it",
			attempts.String(), max(1, floorDiv(backoff, 60)))
	}
	return mayRetry
}

func floorDiv(a, b int64) int64 {
	q := a / b
	if (a%b != 0) && ((a < 0) != (b < 0)) {
		q--
	}
	return q
}

// The binary to relaunch into unless this process is a still stale relaunch into that same binary
func relaunchTarget(ctx context.Context, root, binary string, same bool) (string, error) {
	if same && relaunched.Load() {
		current, err := venvIsCurrent(ctx, root)
		if err != nil {
			return "", err
		}
		if !current {
			sayLaunch("icebath: relaunching did not leave this install current; running with the dependencies it has — run `icebath update` to finish it")
			return "", nil
		}
	}
	return binary, nil
}

// os.path.abspath of a path, joined to the working directory and normalized lexically
func abspath(path string) string {
	if runtime.GOOS == "windows" {
		abs, err := filepath.Abs(path)
		if err != nil {
			return path
		}
		return abs
	}
	if !strings.HasPrefix(path, "/") {
		if wd, err := os.Getwd(); err == nil {
			path = pypath.PosixJoin(wd, path)
		}
	}
	return pypath.PosixNormpath(path)
}

// Reports the binary as this process's own executable by lexical identity, never following links
func sameExecutable(binary string) (bool, error) {
	exe, err := runningExecutable()
	if err != nil {
		return false, err
	}
	return pypath.Normcase(abspath(binary)) == pypath.Normcase(abspath(exe)), nil
}

// Finishes a self managed source update before the app runs, the install binary to relaunch into or empty
func PrepareLaunch(ctx context.Context, projectRoot string, argv []string) (string, error) {
	root, err := pypath.Resolve(projectRoot)
	if err != nil {
		return "", err
	}
	script := ""
	if len(os.Args) > 1 {
		script = os.Args[1]
	}
	if isTailScript(root, script) {
		return "", nil
	}
	command, err := pm.CommandArgv(argv)
	if err != nil {
		return "", err
	}
	metadata := false
	for _, arg := range argv {
		metadata = metadata || metadataFlags[arg]
	}
	switch pystr.Lower(os.Getenv(pm.EnvDisableLazyInstalls)) {
	case "1", "true", "yes":
		return "", nil
	}
	if (len(command) > 0 && command[0] == "pm") || metadata || isReadOnlyGatewayStatus(command) ||
		!existsFollow(filepath.Join(root, ".git")) || !isRegular(filepath.Join(root, "pyproject.toml")) {
		return "", nil
	}
	stamp := pm.ReadInstallStamp(root)
	if stamp.Len() == 0 {
		if _, err := StepAdoptBlessedCheckout(ctx, root); err != nil {
			return "", err
		}
		stamp = pm.ReadInstallStamp(root)
	}
	if mechanism, _ := stamp.Get("updateMechanism"); mechanism != "self" {
		return "", nil
	}
	current, err := venvIsCurrent(ctx, root)
	if err != nil {
		return "", err
	}
	if owner := pm.OwningHomeRoot(ctx, root); owner != "" {
		return prepareBorrowedLaunch(ctx, root, owner, current)
	}
	pending := CompletionPendingPath(ctx, root)
	owedToCLI := current && isRegular(pending) && supervisedChild()
	if !mayRetryCompletion(ctx, root, command, current) {
		if !current {
			return "", nil
		}
	} else if !owedToCLI && (!current || isRegular(pending)) {
		done, err := runOwedRepair(ctx, root, current, pending)
		if err != nil || done {
			return "", err
		}
	}
	binary := resolveStorePython(root)
	if binary == "" {
		return "", runtimeErr("source update has no managed Python; run `icebath pm install`")
	}
	same, err := sameExecutable(binary)
	if err != nil {
		return "", err
	}
	if !current || !same {
		if err := publishLaunchers(ctx, root); err != nil {
			return "", err
		}
		return relaunchTarget(ctx, root, binary, same)
	}
	if owedToCLI {
		sayLaunch("icebath: a source update is unfinished; run `icebath update` from a shell to finish it")
	}
	return "", nil
}

// Runs the owed repair under the update marker, done when a current child of a live update owes nothing
func runOwedRepair(ctx context.Context, root string, current bool, pending string) (bool, error) {
	lock := updaterecord.NewUpdateLock("", root, false)
	acquired, err := lock.Acquire(ctx)
	if err != nil {
		return false, err
	}
	if !acquired {
		return false, runtimeErr("an update is still running; wait for it to exit, then relaunch icebath")
	}
	defer lock.Release(ctx)
	if !lock.Acquired && updaterecord.ReadLiveUpdate(ctx, "", root) != nil {
		if current {
			return true, nil
		}
		if err := syncSourceDependencies(ctx, root, false, ""); err != nil {
			return false, err
		}
		now, err := venvIsCurrent(ctx, root)
		if err != nil {
			return false, err
		}
		if !now {
			return false, runtimeErr("dependency sync left this install out of date")
		}
		return false, nil
	}
	checkout, err := lock.AcquireCheckout(root)
	if err != nil {
		return false, err
	}
	if !checkout {
		return false, runtimeErr("an update is still running; wait for it to exit, then relaunch icebath")
	}
	return false, finishSourceUpdate(ctx, root, current, pending)
}

// Launches a checkout another data root owns, syncing only this root's own environment
func prepareBorrowedLaunch(ctx context.Context, root, owner string, current bool) (string, error) {
	if !current {
		lock := updaterecord.NewUpdateLock("", root, false)
		acquired, err := lock.Acquire(ctx)
		if err != nil {
			return "", err
		}
		if !acquired {
			return "", runtimeErr("an update is still running; wait for it to exit, then relaunch icebath")
		}
		err = syncSourceDependencies(ctx, root, false, owner)
		lock.Release(ctx)
		if err != nil {
			return "", err
		}
		now, err := venvIsCurrent(ctx, root)
		if err != nil {
			return "", err
		}
		if !now {
			return "", runtimeErr("dependency sync left this install out of date")
		}
	}
	binary := resolveStorePython(root)
	if binary == "" {
		return "", runtimeErr("source update has no managed Python; run `icebath pm install`")
	}
	same, err := sameExecutable(binary)
	if err != nil {
		return "", err
	}
	if !current || !same {
		return binary, nil
	}
	return "", nil
}

// Syncs stale dependencies, then runs the completion tail the marker still owes
func defaultFinishSourceUpdate(ctx context.Context, root string, current bool, pending string) error {
	if !current {
		for _, name := range legacyMarkers {
			live, err := pm.MarkerOwnerIsLive(filepath.Join(root, name))
			if err != nil {
				return err
			}
			if live {
				return runtimeErr("an update is still running; wait for it to exit, then relaunch icebath")
			}
		}
		sayLaunch("icebath: completing source-update dependencies...")
		completed, err := treeMatchesCompletedStamp(ctx, root)
		if err != nil {
			return err
		}
		if err := syncSourceDependencies(ctx, root, !completed, ""); err != nil {
			if !completed {
				if recordErr := recordCompletionAttempt(ctx, root, true, syncInputsDigest(root)); recordErr != nil {
					return recordErr
				}
			}
			return err
		}
		if completed {
			return ClearCompletion(ctx, root)
		}
	} else {
		completed, err := treeMatchesCompletedStamp(ctx, root)
		if err != nil {
			return err
		}
		if completed {
			return ClearCompletion(ctx, root)
		}
		sayLaunch("icebath: finishing an interrupted source update...")
	}
	desktopApp := filepath.Join(root, "apps", "desktop")
	releases, _ := scandirNames(filepath.Join(desktopApp, "release"))
	desktop := isRegular(filepath.Join(desktopApp, "dist", "index.html")) || len(releases) > 0
	exe, err := runningExecutable()
	if err != nil {
		return err
	}
	argv := []string{exe, SourceCompletionEntry, "--source", root, "--finish-update"}
	if desktop {
		argv = append(argv, "--desktop")
	}
	env, err := activationEnvironment(ctx, root)
	if err != nil {
		return err
	}
	code := 0
	result, err := completionCustodyRun(ctx, argv, true, ChildOptions{Dir: root, Env: env, Stdout: os.Stderr})
	var refused *updaterecord.CustodyRefused
	switch {
	case errors.As(err, &refused):
		sayLaunch("icebath: %s", refused.Reason)
		code = 1
	case err != nil:
		return err
	default:
		code = result.ReturnCode
	}
	if code != 0 {
		if err := recordCompletionAttempt(ctx, root, true, ""); err != nil {
			return err
		}
		return runtimeErr("source update completion failed; run `icebath update` to finish it")
	}
	return ClearCompletion(ctx, root)
}

// Commits the tree's dependency generation, arm also owing the tail and a borrower leaving the owner's markers
func syncSourceDependencies(ctx context.Context, root string, arm bool, borrowedFrom string) error {
	if borrowedFrom != "" {
		sayLaunch("icebath: preparing dependencies for this data root (the checkout's updates belong to %s)...", borrowedFrom)
	} else if !arm {
		sayLaunch("icebath: preparing dependencies for this update...")
	}
	if err := RefuseForeignOwnedVenv(ctx, root); err != nil {
		return err
	}
	if arm {
		if _, err := ArmCompletion(ctx, root); err != nil {
			return err
		}
	}
	var extras []string
	if !isRegular(pm.EnvironmentRuntimeFactsPath(ctx, root)) {
		legacy, err := pm.LegacySelection(ctx, root)
		if err != nil {
			return err
		}
		extras = append([]string{}, legacy...)
	}
	if err := ensureToolsForSync(ctx); err != nil {
		return err
	}
	var carried []string
	if len(extras) > 1 {
		carried = extras[1:]
	}
	if err := syncVenv(ctx, pm.SyncVenvOptions{Extras: extras, Explicit: true, ProjectRoot: root, EvictIncompatiblePlugins: true, Carried: carried}); err != nil {
		return err
	}
	if err := pm.CollectSupersededGenerations(ctx, root); err != nil {
		return err
	}
	if borrowedFrom != "" {
		return nil
	}
	for _, name := range legacyMarkers {
		if err := os.Remove(filepath.Join(root, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

// Argv re-entering this program through the install binary, its arguments kept and the relaunch marked
func RelaunchCommand(binary string, argv []string) []string {
	command := []string{binary, RelaunchedFlag}
	if len(argv) > 1 {
		command = append(command, argv[1:]...)
	}
	return command
}

// Records a relaunch marker leading the arguments and returns the arguments without it
func TakeRelaunchMarker(args []string) []string {
	if len(args) > 1 && args[1] == RelaunchedFlag {
		relaunched.Store(true)
		return append([]string{args[0]}, args[2:]...)
	}
	return args
}

// Reports this process as the child a relaunch started
func Relaunched() bool { return relaunched.Load() }
