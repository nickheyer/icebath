package pm

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

var (
	commandArgvMu        sync.Mutex
	commandArgvHook      func([]string) []string
	updateRetryRecovered bool
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
		text = strings.TrimSpace(text)
		if strings.HasPrefix(text, "gitdir:") {
			target := strings.TrimSpace(text[len("gitdir:"):])
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
