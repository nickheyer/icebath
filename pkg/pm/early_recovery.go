package pm

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Automatic repairs stop after this many failed attempts so a broken network cannot retry every launch
const earlyCoreInstallMaxAttempts = 3

var (
	commandArgvMu        sync.Mutex
	commandArgvHook      func([]string) []string
	updateRetryRecovered bool
	repairDependencies   = RepairDependencies
	pidIsRunning         = pidRunning
)

// Installs the CLI parser's split of top level flags from the command a launch runs
func SetCommandArgv(split func([]string) []string) {
	commandArgvMu.Lock()
	defer commandArgvMu.Unlock()
	commandArgvHook = split
}

// Reports whether the last startup repair ran for an update command
func UpdateRetryRecovered() bool {
	commandArgvMu.Lock()
	defer commandArgvMu.Unlock()
	return updateRetryRecovered
}

func commandArgv(argv []string) ([]string, error) {
	commandArgvMu.Lock()
	split := commandArgvHook
	commandArgvMu.Unlock()
	if split == nil {
		return nil, errors.New("command line parser not registered with pm")
	}
	return split(argv), nil
}

// Command words of argv as the registered CLI parser splits them, global options removed
func CommandArgv(argv []string) ([]string, error) { return commandArgv(argv) }

// Attempt counter of a marker from its JSON body or attempts line, zero when corrupt or missing
func readMarkerAttempts(marker string) *big.Int {
	data, err := os.ReadFile(marker)
	if err != nil {
		return new(big.Int)
	}
	raw := pystr.Strip(strings.TrimPrefix(pystr.DecodeUTF8Replace(data), "\ufeff"))
	if raw == "" {
		return new(big.Int)
	}
	if doc, err := pyjson.Loads(raw); err == nil {
		if obj, ok := doc.(*pyjson.Object); ok {
			value, present := obj.Get("attempts")
			if !present {
				return new(big.Int)
			}
			switch n := value.(type) {
			case int64:
				return nonNegative(big.NewInt(n))
			case *big.Int:
				return nonNegative(new(big.Int).Set(n))
			}
		}
	}
	lines := pystr.Splitlines(raw, false)
	for i := len(lines) - 1; i >= 0; i-- {
		key, value, found := strings.Cut(lines[i], "=")
		if found && pystr.Strip(key) == "attempts" {
			n, ok := pyint.Parse(value)
			if !ok {
				return new(big.Int)
			}
			return nonNegative(n)
		}
	}
	return new(big.Int)
}

func nonNegative(n *big.Int) *big.Int {
	if n.Sign() < 0 {
		return new(big.Int)
	}
	return n
}

// Live process probe where a zombie counts as dead and a foreign owned process counts as live
func pidRunning(pid *big.Int) (bool, error) {
	if pid.Sign() <= 0 {
		return false, nil
	}
	if runtime.GOOS == "windows" {
		if !pid.IsInt64() || pid.Int64() > math.MaxUint32 {
			return true, nil
		}
		return pidAlive(int(pid.Int64())), nil
	}
	if !pid.IsInt64() || pid.Int64() > math.MaxInt32 {
		return false, &PyError{Class: "OverflowError", Message: "Python int too large to convert to C int"}
	}
	return pidAlive(int(pid.Int64())), nil
}

// Reports a legacy marker whose recorded pid names a running process
func markerOwnerIsLive(marker string) (bool, error) {
	data, err := os.ReadFile(marker)
	if err != nil {
		return false, nil
	}
	body := strings.TrimPrefix(pystr.DecodeUTF8Replace(data), "\ufeff")
	for _, line := range pystr.Splitlines(body, false) {
		key, value, found := strings.Cut(line, "=")
		if found && pystr.Strip(key) == "pid" {
			pid, ok := pyint.Parse(pystr.Strip(value))
			if !ok {
				return false, nil
			}
			return pidIsRunning(pid)
		}
	}
	return false, nil
}

// MarkerOwnerIsLive exports markerOwnerIsLive
func MarkerOwnerIsLive(marker string) (bool, error) { return markerOwnerIsLive(marker) }

// Reports a recorded dependency environment whose site packages directory is gone
func missingEnvironment(ctx context.Context, root string) (bool, error) {
	if !isRegularFile(EnvironmentRuntimeFactsPath(ctx, root)) {
		return false, nil
	}
	venv, err := SelectedVenv(ctx, root)
	if err == nil {
		var site string
		if site, err = SitePackages(venv); err == nil {
			return !isDirectory(site), nil
		}
	}
	var pyErr *PyError
	if errors.As(err, &pyErr) && pyErr.Class == "RuntimeError" {
		return true, nil
	}
	return false, err
}

// Bumps a marker's attempt count in the marker's own format so the retry limit can trip
func countFailedAttempt(marker string) {
	attempts := new(big.Int).Add(readMarkerAttempts(marker), big.NewInt(1))
	data, err := os.ReadFile(marker)
	if err != nil {
		return
	}
	body := strings.TrimPrefix(pystr.DecodeUTF8Replace(data), "\ufeff")
	lines := pystr.Splitlines(body, false)
	legacy := false
	for _, line := range lines {
		if strings.HasPrefix(line, "pid=") {
			legacy = true
			break
		}
	}
	if legacy {
		kept := make([]string, 0, len(lines)+1)
		for _, line := range lines {
			if !strings.HasPrefix(line, "attempts=") {
				kept = append(kept, line)
			}
		}
		body = strings.Join(append(kept, "attempts="+attempts.String()), "\n") + "\n"
	} else {
		body = fmt.Sprintf(`{"attempts": %s}`, attempts.String())
	}
	_ = os.WriteFile(marker, []byte(body), 0o666)
}

// Holds the non blocking recovery lock in writable install state, nil when another launch holds it
func claimRecoveryLock(ctx context.Context, root string) (*os.File, error) {
	state := InstallStateDir(ctx, root)
	if err := os.MkdirAll(state, 0o777); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(state, ".recovery.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	held, err := tryLockFile(f)
	if err != nil {
		f.Close()
		return nil, err
	}
	if !held {
		f.Close()
		return nil, nil
	}
	return f, nil
}

func existingMarkers(paths []string) []string {
	var found []string
	for _, path := range paths {
		if isRegularFile(path) {
			found = append(found, path)
		}
	}
	return found
}

func reprError(err error) string {
	if pypath.IsOSError(err) {
		return pypath.OSErrorText(err)
	}
	return err.Error()
}

// Asks pm to restore the recorded dependency environment before activation, failed requests stay retryable
func recoverIfNeeded(ctx context.Context, projectRoot string, argv []string, explicit bool) (bool, error) {
	root := RepoRoot()
	if projectRoot != "" {
		root = resolvePath(projectRoot)
	}
	if !explicit && LiveCheckoutUnderTest(root) {
		return false, nil
	}
	if argv == nil {
		argv = os.Args[1:]
	}
	args, err := commandArgv(argv)
	if err != nil {
		return false, err
	}
	if !explicit && len(args) > 0 && args[0] == "pm" {
		return false, nil
	}
	missingMarker := filepath.Join(InstallStateDir(ctx, root), ".repair-incomplete")
	markerPaths := []string{filepath.Join(root, ".update-incomplete"), filepath.Join(root, ".lazy-refresh-incomplete"), missingMarker}
	markers := existingMarkers(markerPaths)
	if !isRegularFile(filepath.Join(root, "pyproject.toml")) {
		return false, nil
	}
	if !explicit && len(markers) == 0 {
		missing, err := missingEnvironment(ctx, root)
		if err != nil || !missing {
			return false, err
		}
	}
	lock, err := claimRecoveryLock(ctx, root)
	if lock == nil || err != nil {
		return false, err
	}
	defer lock.Close()
	stderr := cliStderr(ctx)
	recovered, err := runRecovery(ctx, root, args, explicit, markerPaths, missingMarker, &markers)
	if err == nil {
		return recovered, nil
	}
	if isAccessError(err) && InstallStatePermissionMessage(ctx, root, err) != "" {
		return false, err
	}
	for _, marker := range markers {
		countFailedAttempt(marker)
	}
	fmt.Fprintf(stderr, "icebath: dependency repair failed: %s; run `icebath pm repair`\n", reprError(err))
	return false, nil
}

// RecoverIfNeeded exports recoverIfNeeded
func RecoverIfNeeded(ctx context.Context, projectRoot string, argv []string, explicit bool) (bool, error) {
	return recoverIfNeeded(ctx, projectRoot, argv, explicit)
}

// Claimed half of the recovery, its errors become counted attempts in the caller
func runRecovery(ctx context.Context, root string, args []string, explicit bool, markerPaths []string, missingMarker string, markers *[]string) (bool, error) {
	stderr := cliStderr(ctx)
	*markers = existingMarkers(markerPaths)
	if len(*markers) == 0 {
		if !explicit {
			missing, err := missingEnvironment(ctx, root)
			if err != nil || !missing {
				return false, err
			}
		}
		if err := os.WriteFile(missingMarker, []byte(`{"attempts": 0}`), 0o666); err != nil {
			return false, err
		}
		*markers = []string{missingMarker}
	}
	for _, marker := range *markers {
		live, err := markerOwnerIsLive(marker)
		if err != nil || live {
			return false, err
		}
	}
	if !explicit {
		for _, marker := range *markers {
			if readMarkerAttempts(marker).Cmp(big.NewInt(earlyCoreInstallMaxAttempts)) >= 0 {
				fmt.Fprintln(stderr, "icebath: automatic dependency repair retry limit reached; run `icebath pm repair`")
				return false, nil
			}
		}
	}
	fmt.Fprintln(stderr, "icebath: repairing the recorded dependency environment...")
	if err := repairDependencies(ctx, root); err != nil {
		return false, err
	}
	for _, marker := range *markers {
		if err := os.Remove(marker); err != nil && !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
	}
	commandArgvMu.Lock()
	updateRetryRecovered = len(args) > 0 && args[0] == "update"
	commandArgvMu.Unlock()
	fmt.Fprintln(stderr, "icebath: dependency environment repaired")
	return true, nil
}

// Reports an update process, whose dotenv loads must never resolve external secret sources
func ShouldSkipExternalSecretSources() bool {
	if UpdateRetryRecovered() {
		return true
	}
	argv := os.Args[1:]
	commandArgvMu.Lock()
	split := commandArgvHook
	commandArgvMu.Unlock()
	if split != nil {
		argv = split(argv)
	}
	return len(argv) > 0 && (argv[0] == "update" || argv[0] == ImportHealthEntry)
}

// File an interrupted checkout fast forward leaves in the git dir until its move is repaired
const InterruptedPullMarker = "icebath-update-pull"

// Git dir of root, the .git directory or the gitdir a linked worktree's .git file names
func checkoutGitDir(root string) (string, error) {
	dotGit := filepath.Join(root, ".git")
	if info, err := os.Stat(dotGit); err == nil && info.Mode().IsRegular() {
		raw, err := os.ReadFile(dotGit)
		if err != nil {
			return "", err
		}
		text, err := pystr.DecodeUTF8Sig(raw)
		if err != nil {
			return "", err
		}
		text = pystr.Strip(text)
		if strings.HasPrefix(text, "gitdir:") {
			target := pystr.Strip(text[len("gitdir:"):])
			if filepath.IsAbs(target) {
				return target, nil
			}
			return filepath.Join(root, target), nil
		}
	}
	return dotGit, nil
}

// Path of the interrupted pull marker inside root's git dir
func InterruptedPullMarkerPath(root string) (string, error) {
	gitDir, err := checkoutGitDir(root)
	if err != nil {
		return "", err
	}
	return filepath.Join(gitDir, InterruptedPullMarker), nil
}
