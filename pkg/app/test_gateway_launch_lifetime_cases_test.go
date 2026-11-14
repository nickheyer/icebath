package app

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Records an accepted turn the gateway still owes the session
func lifetimePendingTurn(t *testing.T, dbPath, sid string) {
	t.Helper()
	conn, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Exec("INSERT INTO session_admissions(admission_id,request_id,principal_id,target_session_id,lineage_json,"+
		"payload_json,payload_digest,intent,status,outcome,owner_epoch,generation) "+
		"VALUES(?,?,'p',?,'[]','{}','d','queue',?,NULL,1,1)", "adm-"+sid, "req-"+sid, sid, "queued"); err != nil {
		t.Fatal(err)
	}
}

// Settles every admitted turn of the store
func lifetimeFinishTurns(t *testing.T, dbPath string) {
	t.Helper()
	conn, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Exec("UPDATE session_admissions SET status='terminal', outcome='completed'"); err != nil {
		t.Fatal(err)
	}
}

// Ages a checkout past the stale prune window and runs the prune of the next launch
func lifetimeAgeAndPrune(t *testing.T, repo, checkout string) {
	t.Helper()
	lifetimeFinishTurns(t, filepath.Join(home.Dir(context.Background()), "state.db"))
	old := time.Now().Add(-25 * time.Hour)
	if err := os.Chtimes(checkout, old, old); err != nil {
		t.Fatal(err)
	}
	if err := tools.PruneStaleWorktrees(context.Background(), repo, 24); err != nil {
		t.Fatal(err)
	}
}

// Launch seams of a worktree launch whose terminal UI run reports the checkout it worked in
func lifetimeDeps(t *testing.T, ink func(env map[string]string) string, checkout *string) tuiLaunchDeps {
	t.Helper()
	var captured []map[string]string
	deps, _ := tuiFakeDeps(t, 0, &captured)
	deps.run = func(ctx context.Context, env map[string]string) (int, error) {
		attached := ink(env)
		if err := os.WriteFile(env["ICEBATH_TUI_ACTIVE_SESSION_FILE"], []byte(`{"session_id": "`+attached+`"}`), 0o644); err != nil {
			t.Fatal(err)
		}
		*checkout = env["ICEBATH_TUI_CWD"]
		return 0, nil
	}
	return deps
}

// Creates a tui session in the home store with an optional user message
func lifetimeSession(t *testing.T, sid, cwd string, withMessage bool) {
	t.Helper()
	ctx := context.Background()
	db, err := state.NewSessionDB(ctx, filepath.Join(home.Dir(ctx), "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.CreateSession(state.NewSession{ID: sid, Source: "tui", Cwd: cwd}); err != nil {
		t.Fatal(err)
	}
	if withMessage {
		oneshotAppend(t, db, sid, "user", "keep my checkout")
	}
}

func TestRetainedTuiWorktreeSurvivesStalePruneWhileItsSessionResumes(t *testing.T) {
	for _, resumable := range []bool{true, false} {
		t.Run(map[bool]string{true: "True", false: "False"}[resumable], func(t *testing.T) {
			tuiTestHome(t)
			repo := tuiRepo(t, t.TempDir())
			t.Chdir(repo)
			t.Setenv("ICEBATH_PYTHON", tuiSelfExecutable(t))
			ctx := context.Background()
			wtInfo := tools.SetupWorktree(ctx, repo, false, "", nil)
			if wtInfo == nil {
				t.Fatal("worktree setup failed")
			}
			tuiGit(t, "-C", repo, "worktree", "unlock", wtInfo["path"])
			tuiGit(t, "-C", repo, "worktree", "lock", "--reason", "icebath pid=999999999", wtInfo["path"])
			var checkout string
			deps := lifetimeDeps(t, func(env map[string]string) string {
				lifetimeSession(t, "tui-wt", env["ICEBATH_TUI_CWD"], resumable)
				lifetimePendingTurn(t, filepath.Join(home.Dir(ctx), "state.db"), "tui-wt")
				return "tui-wt"
			}, &checkout)
			deps.setupWorktree = func(context.Context) (map[string]any, error) {
				out := map[string]any{}
				for k, v := range wtInfo {
					out[k] = v
				}
				return out, nil
			}
			launchTUIWith(ctx, TUILaunch{Worktree: true}, deps)
			lifetimeAgeAndPrune(t, repo, checkout)
			st, err := os.Stat(checkout)
			if (err == nil && st.IsDir()) != resumable {
				t.Fatalf("checkout kept %v, want %v", err == nil, resumable)
			}
		})
	}
}

func TestRetainedTuiWorktreeKeptWhileAnySessionInItResumes(t *testing.T) {
	for _, tc := range []string{"quoted_home", "after_new", "terminal_closed"} {
		t.Run(tc, func(t *testing.T) {
			name := "home"
			if tc == "quoted_home" {
				name = "Zoë \"q\" \\ home"
			}
			h := filepath.Join(t.TempDir(), name)
			if err := os.MkdirAll(h, 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("ICEBATH_HOME", h)
			repo := tuiRepo(t, t.TempDir())
			t.Chdir(repo)
			t.Setenv("ICEBATH_PYTHON", tuiSelfExecutable(t))
			if tc == "terminal_closed" {
				swapSeam(t, &tools.RetainWorktreeForSession, func(string, string, string, string) bool { return false })
			}
			ctx := context.Background()
			var checkout string
			deps := lifetimeDeps(t, func(env map[string]string) string {
				lifetimeSession(t, "tui-a", env["ICEBATH_TUI_CWD"], true)
				attached := "tui-a"
				if tc == "after_new" {
					attached = "tui-b"
					lifetimeSession(t, attached, env["ICEBATH_TUI_CWD"], false)
				}
				lifetimePendingTurn(t, filepath.Join(home.Dir(ctx), "state.db"), attached)
				return attached
			}, &checkout)
			deps.setupWorktree = func(ctx context.Context) (map[string]any, error) { return setupTUIWorktree(ctx, &strings.Builder{}) }
			launchTUIWith(ctx, TUILaunch{Worktree: true}, deps)
			listing := tuiGit(t, "-C", repo, "worktree", "list", "--porcelain", "-z")
			reason := ""
			for _, field := range strings.Split(listing, "\x00") {
				if strings.HasPrefix(field, "locked ") {
					reason = field[len("locked "):]
					break
				}
			}
			tuiGit(t, "-C", repo, "worktree", "unlock", checkout)
			tuiGit(t, "-C", repo, "worktree", "lock", "--reason", regexp.MustCompile(`pid=\d+`).ReplaceAllString(reason, "pid=999999999"), checkout)
			lifetimeAgeAndPrune(t, repo, checkout)
			if st, err := os.Stat(checkout); err != nil || !st.IsDir() {
				t.Fatalf("checkout removed: %v", err)
			}
		})
	}
}
