package botmode

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyshlex"
	"github.com/nickheyer/icebath/pkg/internal/pyshutil"
	"github.com/nickheyer/icebath/pkg/pm"
)

// Argv word that runs this binary as the Bot Mode DM delivery runner and relay reply waiter
const DMRunnerEntry = "__icebath_bot_mode_dm__"

// Shell command that blocks until the envelope's reply file appears and prints it, argv quoted so roster text stays data
func WaiterCommand(root string, envelope *pyjson.Object) (string, error) {
	id, ok := envelope.Get("id")
	if !ok {
		return "", &pyrepr.FormatError{Class: "KeyError", Msg: pyrepr.StrRepr("id")}
	}
	replyPath := pypath.PathJoin(RelayRoot(root), RepliesDir, pyrepr.Str(id)+".json")
	label := fmt.Sprintf("@%s on %s", pyrepr.Str(envelope.GetOr("target_handle", "")), pyrepr.Str(envelope.GetOr("target_connection", "")))
	executable, err := os.Executable()
	if err != nil || executable == "" {
		executable = "icebath"
	}
	argv := []string{executable, DMRunnerEntry, "--wait-reply", replyPath, label, strconv.Itoa(ReplyWaitSeconds)}
	if runtime.GOOS == "windows" {
		for i, part := range argv {
			argv[i] = strings.ReplaceAll(part, "\\", "/")
		}
	}
	return pyshlex.Join(argv), nil
}

// Command a delivery runs the CLI through, the published launcher, then the binary's sibling, then PATH
func IcebathCLI() string {
	name := "icebath"
	if runtime.GOOS == "windows" {
		name = "icebath.exe"
	}
	candidates := []string{filepath.Join(pm.RepoRoot(), ".icebath", "bin", name)}
	if executable, err := os.Executable(); err == nil && executable != "" {
		candidates = append(candidates, filepath.Join(filepath.Dir(executable), name))
	}
	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() {
			return candidate
		}
	}
	if found, ok := pyshutil.Which("icebath", nil); ok {
		return found
	}
	return "icebath"
}
