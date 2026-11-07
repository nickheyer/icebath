package selfupdate

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/nickheyer/icebath/pkg/backup"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
	"github.com/nickheyer/icebath/pkg/state"
)

const (
	runtimeSchemaMarker = "CREATE TABLE IF NOT EXISTS session_admissions"
	StateModules        = "pkg/state/*.go"
	ledgerProbe         = "SELECT EXISTS(SELECT 1 FROM session_admissions) OR EXISTS(SELECT 1 FROM worker_executions)"
	downgradeDocs       = "https://hermes-agent.nousresearch.com/docs/getting-started/updating#rolling-back-to-an-older-release"
)

// Git runner answering one command's exit code under the updater's custody, -1 when git cannot run
func updaterGit(ctx context.Context, gitCmd []string, root string) func(args ...string) int {
	return func(args ...string) int {
		result, err := RunGit(ctx, gitCmd, args, ChildOptions{Dir: root, Capture: true, Text: true, Timeout: 120 * time.Second})
		if err != nil {
			return -1
		}
		return result.ReturnCode
	}
}

// Reports whether any state schema source defines the runtime ledger
func definesRuntime(sources ...string) bool {
	for _, source := range sources {
		if strings.Contains(source, runtimeSchemaMarker) {
			return true
		}
	}
	return false
}

// True when targetRef does not descend from HEAD and no state schema source of it defines the runtime ledger
func targetPredatesRuntime(ctx context.Context, gitCmd []string, root, targetRef string, git func(args ...string) int, remote func(string) *bool) bool {
	if git == nil {
		git = updaterGit(ctx, gitCmd, root)
	}
	if git("merge-base", "--is-ancestor", "HEAD", targetRef) == 0 {
		return false
	}
	askRemote := func() bool {
		if remote == nil {
			return false
		}
		verdict := remote(targetRef)
		return verdict != nil && *verdict
	}
	if git("rev-list", "--objects", "--no-walk", "--sparse", "--missing=error", targetRef, "--", StateModules) != 0 {
		return askRemote()
	}
	result := git("grep", "-q", "-F", runtimeSchemaMarker, targetRef, "--", StateModules)
	if result == 0 || result == 1 {
		return result == 1
	}
	return askRemote()
}

// Opens a state store read only, nil when it cannot be opened
func openReadOnlyStore(dbPath string, timeout time.Duration) *sql.DB {
	uri, err := state.ReadOnlyDBURI(dbPath)
	if err != nil {
		return nil
	}
	db, err := sql.Open("sqlite", uri)
	if err != nil {
		return nil
	}
	db.SetMaxOpenConns(1)
	if timeout > 0 {
		if _, err := db.Exec("PRAGMA busy_timeout = " + strconv.Itoa(int(timeout/time.Millisecond))); err != nil {
			db.Close()
			return nil
		}
	}
	return db
}

// Reports whether a state store holds runtime ledger rows, false when it has none or cannot be read
func hasLedgerRows(dbPath string) bool {
	if info, err := os.Stat(dbPath); err != nil || !info.Mode().IsRegular() {
		return false
	}
	db := openReadOnlyStore(dbPath, 5*time.Second)
	if db == nil {
		return false
	}
	defer db.Close()
	var exists bool
	if err := db.QueryRow(ledgerProbe).Scan(&exists); err != nil {
		return false
	}
	return exists
}

// Newest quick snapshot of a home whose state store predates the runtime ledger, empty when none does
func preRuntimeSnapshot(h string) string {
	root := backup.QuickSnapshotRoot(h)
	entries, err := os.ReadDir(root)
	if err != nil {
		return ""
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	for _, name := range names {
		db := filepath.Join(root, name, "state.db")
		if info, err := os.Stat(db); strings.HasPrefix(name, ".") || err != nil || !info.Mode().IsRegular() {
			continue
		}
		conn := openReadOnlyStore(db, 0)
		if conn == nil {
			continue
		}
		var one int
		err := conn.QueryRow("SELECT 1 FROM sqlite_master WHERE name='session_admissions'").Scan(&one)
		conn.Close()
		if err == sql.ErrNoRows {
			return name
		}
	}
	return ""
}

func resolvedPath(path string) string {
	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real
	}
	abs, _ := filepath.Abs(path)
	return abs
}

// Profile name whose -p flag opens a home, empty for the default root or a custom home
func namedProfile(h string) string {
	parent := resolvedPath(profiles.ProfilesRoot())
	resolved := resolvedPath(h)
	name := filepath.Base(resolved)
	if filepath.Dir(resolved) == parent && home.ProfileIDRe.MatchString(name) && name != "default" {
		return name
	}
	return ""
}

// Profiles of this install whose state store holds runtime ledger rows, the given home first
func ledgeredProfiles(ctx context.Context, h string) [][2]string {
	if h == "" {
		h = home.Dir(ctx)
	}
	candidates := append([][2]string{{namedProfile(h), h}}, backup.SiblingProfileHomes(h)...)
	var out [][2]string
	for _, profile := range candidates {
		if hasLedgerRows(filepath.Join(profile[1], "state.db")) {
			out = append(out, profile)
		}
	}
	return out
}

// Whether the update would refuse targetRef here, the store scan first so remote is asked only when needed
func wouldRefuse(ctx context.Context, root, targetRef, h string, git func(args ...string) int, remote func(string) *bool) bool {
	return len(ledgeredProfiles(ctx, h)) > 0 && targetPredatesRuntime(ctx, nil, root, targetRef, git, remote)
}

// Why the update must not move to targetRef, nil unless a ledgered store would meet a pre runtime target
func downgradeRefusalFor(ctx context.Context, targetRef, h string, predates func() bool) *string {
	ledgered := ledgeredProfiles(ctx, h)
	if len(ledgered) == 0 || !predates() {
		return nil
	}
	short := targetRef
	if len(short) > 12 {
		short = short[:12]
	}
	lines := []string{"✗ Refusing to move this install to " + short + ": that release predates the gateway runtime " +
		"these session stores now use. Downgrading is unsupported."}
	for _, profile := range ledgered {
		lines = append(lines, "  "+filepath.Join(profile[1], "state.db")+" has gateway runtime history.")
	}
	lines = append(lines, "  The older release could not delete or prune those sessions.",
		"  To go back anyway: run `icebath gateway stop`, check out the older release with git, then put back "+
			"each store's copy from before the upgrade:")
	for _, profile := range ledgered {
		name, h := profile[0], profile[1]
		snapshot := preRuntimeSnapshot(h)
		icebath := "icebath"
		if name != "" {
			icebath = "icebath -p " + name
		}
		if snapshot != "" {
			lines = append(lines, "    "+h+": run `"+icebath+"` and enter `/snapshot restore "+snapshot+"`.")
		} else {
			lines = append(lines, "    "+h+": no pre-update snapshot predates the upgrade; restore a full backup taken before "+
				"it with `"+icebath+" import <backup.zip>`.")
		}
	}
	lines = append(lines, "  Sessions created since the upgrade are not in that copy.", "  Guide: "+downgradeDocs)
	text := strings.Join(lines, "\n")
	return &text
}

// Git path refusal of an install root holding a checkout, the updater's custody git deciding the target
func downgradeRefusalGit(ctx context.Context, gitCmd []string, root, targetRef, h string) *string {
	return downgradeRefusalFor(ctx, targetRef, h, func() bool {
		return targetPredatesRuntime(ctx, gitCmd, root, targetRef, nil, nil)
	})
}

// Reports whether an install root holds a git checkout, a directory or a worktree gitfile
func holdsGitCheckout(root string) bool {
	_, err := os.Stat(filepath.Join(root, ".git"))
	return err == nil
}

// Binary form of the schema question, a tag at or above the running build is a forward update
func releasePredatesRuntime(ctx context.Context, h, tag string) bool {
	if CompareVersions(tag, updaterecord.BuildVersion()) >= 0 {
		return false
	}
	verdict := githubPredatesRuntime(ctx, OfficialRepository, tag, filepath.Join(h, "source-checks", "release-runtime.json"))
	return verdict != nil && *verdict
}

// Read only probe git of the update check, 20 second bound, -1 when git cannot run
func checkProbeGit(ctx context.Context, root string) func(args ...string) int {
	return func(args ...string) int {
		result, err := sourceGitRun(ctx, gitArgs([]string{updaterecord.GitExecutable(ctx)}, args...), root, 20*time.Second)
		if err != nil {
			return -1
		}
		return result.ReturnCode
	}
}

// Whether the update would refuse rel here, the check's read only twin of downgradeRefusal
func downgradeWouldRefuse(ctx context.Context, root string, rel *Release) (bool, error) {
	h := home.Dir(ctx)
	if holdsGitCheckout(root) {
		repository, err := sourceRepository(ctx, []string{updaterecord.GitExecutable(ctx)}, root)
		if err != nil {
			return false, err
		}
		remote := func(sha string) *bool {
			return githubPredatesRuntime(ctx, repository, sha, filepath.Join(h, "source-checks", "release-runtime.json"))
		}
		return wouldRefuse(ctx, root, rel.Tag, h, checkProbeGit(ctx, root), remote), nil
	}
	return len(ledgeredProfiles(ctx, h)) > 0 && releasePredatesRuntime(ctx, h, rel.Tag), nil
}

// Why the update must not install rel, nil when no ledgered store would meet a pre runtime release
func downgradeRefusal(ctx context.Context, root string, rel *Release) (*string, error) {
	h := home.Dir(ctx)
	if holdsGitCheckout(root) {
		return downgradeRefusalGit(ctx, []string{updaterecord.GitExecutable(ctx)}, root, rel.Tag, h), nil
	}
	return downgradeRefusalFor(ctx, rel.Tag, h, func() bool { return releasePredatesRuntime(ctx, h, rel.Tag) }), nil
}
