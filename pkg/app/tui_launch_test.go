package app

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	_ "github.com/nickheyer/icebath/pkg/cron"
	"github.com/nickheyer/icebath/pkg/goals"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Fresh profile home under the test temp dir
func tuiTestHome(t *testing.T) string {
	t.Helper()
	h := filepath.Join(t.TempDir(), ".icebath")
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	return h
}

// Session with one exchange and the given token counts in the store of h
func tuiSeed(t *testing.T, h, sid string, input, output int64) {
	t.Helper()
	ctx := context.Background()
	db, err := state.NewSessionDB(ctx, filepath.Join(h, "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.CreateSession(state.NewSession{ID: sid, Source: "tui"}); err != nil {
		t.Fatal(err)
	}
	for _, m := range []map[string]any{{"role": "user", "content": "q"}, {"role": "assistant", "content": "a"}} {
		if _, err := db.AppendMessage(ctx, sid, m, state.AppendGuards{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.UpdateTokenCounts(sid, state.TokenCountDelta{InputTokens: input, OutputTokens: output}); err != nil {
		t.Fatal(err)
	}
}

// Epilogue text for a session id
func tuiSummary(sid string) string {
	var out bytes.Buffer
	printTUIExitSummary(context.Background(), &out, sid, "")
	return out.String()
}

func TestEpiloguePrintsShortResumeIDForCanonicalSession(t *testing.T) {
	h := tuiTestHome(t)
	sid := "local-" + "ab12cd34ef56a7b8" + strings.Repeat("0", 48)
	tuiSeed(t, h, sid, 300, 60)
	out := tuiSummary(sid)
	if !strings.Contains(out, "icebath --tui --resume "+sid[:22]+"\n") {
		t.Fatalf("short id missing: %q", out)
	}
	if strings.Contains(out, sid) {
		t.Fatalf("full id printed: %q", out)
	}
	if !strings.Contains(out, "Tokens:         360 (in 300, out 60") {
		t.Fatalf("tokens: %q", out)
	}
}

func TestAmbiguousPrefixFallsBackToFullID(t *testing.T) {
	h := tuiTestHome(t)
	a := "local-" + "ab12cd34ef56a7b8" + strings.Repeat("0", 48)
	b := "local-" + "ab12cd34ef56a7b8" + strings.Repeat("1", 48)
	tuiSeed(t, h, a, 300, 60)
	tuiSeed(t, h, b, 300, 60)
	if out := tuiSummary(a); !strings.Contains(out, "icebath --tui --resume "+a+"\n") {
		t.Fatalf("ambiguous prefix shortened: %q", out)
	}
}

func TestStoredIDsPrintUnchanged(t *testing.T) {
	h := tuiTestHome(t)
	tuiSeed(t, h, "20261012_185208_a67483", 300, 60)
	if out := tuiSummary("20261012_185208_a67483"); !strings.Contains(out, "icebath --tui --resume 20261012_185208_a67483\n") {
		t.Fatalf("stored id: %q", out)
	}
}

func TestBranchChildEpilogueCountsTheBranchedConversation(t *testing.T) {
	h := tuiTestHome(t)
	parent := "local-" + strings.Repeat("1", 64)
	child := "local-" + strings.Repeat("2", 64)
	tuiSeed(t, h, parent, 300, 60)
	ctx := context.Background()
	db, err := state.NewSessionDB(ctx, filepath.Join(h, "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CreateSession(state.NewSession{ID: child, Source: "tui", ModelConfig: map[string]any{"_branched_from": parent}, ParentSessionID: parent}); err != nil {
		t.Fatal(err)
	}
	for _, m := range []map[string]any{{"role": "user", "content": "q"}, {"role": "assistant", "content": "a"}} {
		if _, err := db.AppendMessage(ctx, child, m, state.AppendGuards{}); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	out := tuiSummary(child)
	if !strings.Contains(out, "--resume "+child[:22]) || !strings.Contains(out, "Tokens:         360 (in 300, out 60") {
		t.Fatalf("branch epilogue: %q", out)
	}
}

// Session with three messages titled for the profile hint cases
func tuiTitledSession(t *testing.T, h, sid string) {
	t.Helper()
	tuiSeed(t, h, sid, 0, 0)
	ctx := context.Background()
	db, err := state.NewSessionDB(ctx, filepath.Join(h, "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.AppendMessage(ctx, sid, map[string]any{"role": "user", "content": "more"}, state.AppendGuards{}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SetSessionTitle(ctx, sid, "My TUI Session"); err != nil {
		t.Fatal(err)
	}
}

func TestTuiExitSummaryResumeHintTuiHintsIncludeProfileFlagForNamedProfile(t *testing.T) {
	root := t.TempDir()
	h := filepath.Join(root, ".icebath", "profiles", "dev")
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	tuiTitledSession(t, h, "20260524_000001_abc123")
	out := tuiSummary("20260524_000001_abc123")
	if !strings.Contains(out, "icebath --tui --resume 20260524_000001_abc123 -p dev") || !strings.Contains(out, `icebath --tui -c "My TUI Session" -p dev`) {
		t.Fatalf("profile hints: %q", out)
	}
}

func TestTuiExitSummaryResumeHintTuiHintsNoProfileFlagOnDefault(t *testing.T) {
	h := tuiTestHome(t)
	tuiTitledSession(t, h, "20260524_000001_abc123")
	out := tuiSummary("20260524_000001_abc123")
	if !strings.Contains(out, "icebath --tui --resume 20260524_000001_abc123") || strings.Contains(out, " -p ") {
		t.Fatalf("default hints: %q", out)
	}
}

// Launch seams that record the child environment and answer code
func tuiFakeDeps(t *testing.T, code int, captured *[]map[string]string) (tuiLaunchDeps, *bytes.Buffer) {
	t.Helper()
	var errOut bytes.Buffer
	return tuiLaunchDeps{
		setupWorktree: func(context.Context) (map[string]any, error) {
			t.Fatal("worktree setup without --worktree")
			return nil, nil
		},
		run: func(_ context.Context, env map[string]string) (int, error) {
			copied := map[string]string{}
			for k, v := range env {
				copied[k] = v
			}
			*captured = append(*captured, copied)
			return code, nil
		},
		printSummary: func(context.Context, string, string) {},
		relaunch:     func([]string, bool) { t.Fatal("relaunch") },
		stdout:       &bytes.Buffer{},
		stderr:       &errOut,
	}, &errOut
}

// Executable path of this test binary, the interpreter the launcher keeps
func tuiSelfExecutable(t *testing.T) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return self
}

func TestLaunchTuiExportsModelProviderAndToolsets(t *testing.T) {
	tuiTestHome(t)
	t.Setenv("ICEBATH_PYTHON", tuiSelfExecutable(t))
	var captured []map[string]string
	deps, _ := tuiFakeDeps(t, 1, &captured)
	activeDuringCall := ""
	run := deps.run
	deps.run = func(ctx context.Context, env map[string]string) (int, error) {
		activeDuringCall = env["ICEBATH_TUI_ACTIVE_SESSION_FILE"]
		if _, err := os.Stat(activeDuringCall); err != nil {
			t.Fatalf("active session file missing during call: %v", err)
		}
		return run(ctx, env)
	}
	code := launchTUIWith(context.Background(), TUILaunch{Model: "nous/icebath-test", Provider: "nous", Toolsets: "web, terminal"}, deps)
	if code != 1 {
		t.Fatalf("exit %d", code)
	}
	env := captured[0]
	for key, want := range map[string]string{"ICEBATH_MODEL": "nous/icebath-test", "ICEBATH_INFERENCE_MODEL": "nous/icebath-test",
		"ICEBATH_TUI_PROVIDER": "nous", "ICEBATH_INFERENCE_PROVIDER": "nous", "ICEBATH_TUI_TOOLSETS": "web,terminal"} {
		if env[key] != want {
			t.Fatalf("%s = %q, want %q", key, env[key], want)
		}
	}
	active := env["ICEBATH_TUI_ACTIVE_SESSION_FILE"]
	if !strings.HasPrefix(filepath.Base(active), "icebath-tui-active-session-") || filepath.Ext(active) != ".json" {
		t.Fatalf("active session file name %q", active)
	}
	if activeDuringCall != active {
		t.Fatalf("active file changed: %q != %q", activeDuringCall, active)
	}
	if _, err := os.Stat(active); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("active session file left behind: %v", err)
	}
}

// Resolved spelling of a path for comparisons
func tuiResolved(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestLaunchTuiPrefersLaunchCwdOverInheritedHermesCwd(t *testing.T) {
	tuiTestHome(t)
	base := t.TempDir()
	stale, launch := filepath.Join(base, "stale-project"), filepath.Join(base, "launch-project")
	for _, d := range []string{stale, launch} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("ICEBATH_PYTHON", tuiSelfExecutable(t))
	t.Setenv("ICEBATH_CWD", stale)
	t.Chdir(launch)
	var captured []map[string]string
	deps, _ := tuiFakeDeps(t, 1, &captured)
	launchTUIWith(context.Background(), TUILaunch{}, deps)
	handed := tuiResolved(t, captured[0]["ICEBATH_CWD"])
	if handed != tuiResolved(t, launch) || handed == tuiResolved(t, stale) {
		t.Fatalf("cwd handed to the TUI %q", handed)
	}
}

func TestLaunchTuiWorktreeStillOutranksTheLaunchCwd(t *testing.T) {
	tuiTestHome(t)
	base := t.TempDir()
	worktree, launch := filepath.Join(base, "worktree"), filepath.Join(base, "launch-project")
	for _, d := range []string{worktree, launch} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("ICEBATH_PYTHON", tuiSelfExecutable(t))
	t.Setenv("ICEBATH_CWD", base)
	t.Chdir(launch)
	var captured []map[string]string
	deps, _ := tuiFakeDeps(t, 1, &captured)
	deps.setupWorktree = func(context.Context) (map[string]any, error) { return map[string]any{"path": worktree}, nil }
	launchTUIWith(context.Background(), TUILaunch{Worktree: true}, deps)
	env := captured[0]
	if env["ICEBATH_CWD"] != worktree || env["TERMINAL_CWD"] != worktree {
		t.Fatalf("worktree cwd %q terminal %q", env["ICEBATH_CWD"], env["TERMINAL_CWD"])
	}
	if tuiResolved(t, env["ICEBATH_CWD"]) == tuiResolved(t, launch) {
		t.Fatal("launch cwd outranked the worktree")
	}
}

func TestLaunchTuiLocalSessionStartsInLaunchDirNotTerminalCwd(t *testing.T) {
	for _, backend := range []string{"local", "docker"} {
		t.Run(backend, func(t *testing.T) {
			h := tuiTestHome(t)
			base := t.TempDir()
			configured, launch := filepath.Join(base, "configured-home"), filepath.Join(base, "launch-project")
			for _, d := range []string{configured, launch} {
				if err := os.MkdirAll(d, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte("terminal:\n  backend: "+backend+"\n  cwd: "+configured+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			t.Setenv("TERMINAL_ENV", "")
			os.Unsetenv("TERMINAL_ENV")
			t.Setenv("TERMINAL_CWD", "")
			os.Unsetenv("TERMINAL_CWD")
			t.Setenv("ICEBATH_PYTHON", tuiSelfExecutable(t))
			t.Setenv("ICEBATH_TUI_CWD", base)
			t.Chdir(launch)
			var captured []map[string]string
			deps, _ := tuiFakeDeps(t, 1, &captured)
			launchTUIWith(context.Background(), TUILaunch{}, deps)
			env := captured[0]
			if backend == "local" {
				if tuiResolved(t, env["ICEBATH_TUI_CWD"]) != tuiResolved(t, launch) || tuiResolved(t, env["TERMINAL_CWD"]) != tuiResolved(t, launch) {
					t.Fatalf("local cwd %q terminal %q", env["ICEBATH_TUI_CWD"], env["TERMINAL_CWD"])
				}
				return
			}
			if _, ok := env["ICEBATH_TUI_CWD"]; ok {
				t.Fatal("remote backend kept ICEBATH_TUI_CWD")
			}
			if tuiResolved(t, env["TERMINAL_CWD"]) != tuiResolved(t, configured) {
				t.Fatalf("remote terminal cwd %q", env["TERMINAL_CWD"])
			}
		})
	}
}

func TestLaunchTuiBypassWithoutModelRefusesBeforeTheTuiStarts(t *testing.T) {
	for _, flag := range []string{"ICEBATH_SAFE_MODE", "ICEBATH_IGNORE_USER_CONFIG"} {
		t.Run(flag, func(t *testing.T) {
			tuiTestHome(t)
			t.Setenv(flag, "1")
			t.Setenv("ICEBATH_MODEL", "")
			os.Unsetenv("ICEBATH_MODEL")
			t.Setenv("ICEBATH_PYTHON", tuiSelfExecutable(t))
			var spawned []map[string]string
			deps, errOut := tuiFakeDeps(t, 0, &spawned)
			if code := launchTUIWith(context.Background(), TUILaunch{}, deps); code != 1 || len(spawned) != 0 {
				t.Fatalf("exit %d spawned %d", code, len(spawned))
			}
			if !strings.Contains(errOut.String(), "pass --model explicitly") {
				t.Fatalf("stderr %q", errOut.String())
			}
			launchTUIWith(context.Background(), TUILaunch{Model: "explicit-model"}, deps)
			if len(spawned) == 0 || spawned[len(spawned)-1][flag] != "1" || spawned[len(spawned)-1]["ICEBATH_MODEL"] != "explicit-model" {
				t.Fatalf("spawned %v", spawned)
			}
		})
	}
}

func TestTuiLauncherHandsTheShellEnvToTheTerminalUI(t *testing.T) {
	tuiTestHome(t)
	t.Setenv("PATH", "/client/venv/bin:/usr/bin")
	t.Setenv("VIRTUAL_ENV", "/client/venv")
	t.Setenv("ICEBATH_EPHEMERAL_SYSTEM_PROMPT", "mine")
	t.Setenv("NODE_AUTH_TOKEN", "npm-secret")
	t.Setenv("OPENAI_API_KEY", "sk-secret")
	t.Setenv("ICEBATH_PYTHON", tuiSelfExecutable(t))
	var captured []map[string]string
	deps, _ := tuiFakeDeps(t, 1, &captured)
	launchTUIWith(context.Background(), TUILaunch{}, deps)
	var env map[string]string
	if err := json.Unmarshal([]byte(captured[0]["ICEBATH_TUI_CLIENT_ENV"]), &env); err != nil {
		t.Fatal(err)
	}
	if env["PATH"] != "/client/venv/bin:/usr/bin" || env["VIRTUAL_ENV"] != "/client/venv" || env["ICEBATH_EPHEMERAL_SYSTEM_PROMPT"] != "mine" {
		t.Fatalf("client env %v", env)
	}
	if _, ok := env["NODE_AUTH_TOKEN"]; ok {
		t.Fatal("NODE_AUTH_TOKEN forwarded")
	}
	if _, ok := env["OPENAI_API_KEY"]; ok {
		t.Fatal("OPENAI_API_KEY forwarded")
	}
}

// Git invocation with a fixed test identity
func tuiGit(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-c", "user.name=Test", "-c", "user.email=test@example.invalid"}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// Repository with one commit ignoring the worktrees directory
func tuiRepo(t *testing.T, base string) string {
	t.Helper()
	repo := filepath.Join(base, "repo")
	tuiGit(t, "init", "-q", "-b", "main", repo)
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte(".worktrees/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tuiGit(t, "-C", repo, "add", ".gitignore")
	tuiGit(t, "-C", repo, "commit", "-qm", "fixture")
	return repo
}

// Branch names of a repository
func tuiBranches(t *testing.T, repo string) []string {
	return strings.Fields(tuiGit(t, "-C", repo, "branch", "--format=%(refname:short)"))
}

// True when name is among the branches
func tuiHasBranch(branches []string, name string) bool {
	for _, b := range branches {
		if b == name {
			return true
		}
	}
	return false
}

// Runs a worktree TUI launch whose Ink session prepare shapes, returning the tree, repo and info
func tuiQuitWith(t *testing.T, prepare func(db *state.SessionDB, cwd string)) (string, string, map[string]string) {
	t.Helper()
	h := tuiTestHome(t)
	repo := tuiRepo(t, t.TempDir())
	t.Chdir(repo)
	t.Setenv("ICEBATH_PYTHON", tuiSelfExecutable(t))
	ctx := context.Background()
	retain := filepath.Join(h, "state.db")
	wtInfo := tools.SetupWorktree(ctx, repo, false, "", &retain)
	if wtInfo == nil {
		t.Fatal("worktree setup failed")
	}
	var captured []map[string]string
	deps, _ := tuiFakeDeps(t, 0, &captured)
	deps.setupWorktree = func(context.Context) (map[string]any, error) {
		out := map[string]any{}
		for k, v := range wtInfo {
			out[k] = v
		}
		return out, nil
	}
	deps.run = func(ctx context.Context, env map[string]string) (int, error) {
		db, err := state.NewSessionDB(ctx, retain, false)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.CreateSession(state.NewSession{ID: "tui-wt", Source: "tui", Cwd: env["ICEBATH_TUI_CWD"]}); err != nil {
			t.Fatal(err)
		}
		prepare(db, env["ICEBATH_TUI_CWD"])
		db.Close()
		if err := os.WriteFile(env["ICEBATH_TUI_ACTIVE_SESSION_FILE"], []byte(`{"session_id": "tui-wt"}`), 0o644); err != nil {
			t.Fatal(err)
		}
		return 0, nil
	}
	if code := launchTUIWith(ctx, TUILaunch{Worktree: true}, deps); code != 0 {
		t.Fatalf("exit %d", code)
	}
	return wtInfo["path"], repo, wtInfo
}

func TestTuiWorktreeQuitRemovesOnlyACleanIdleTree(t *testing.T) {
	for _, tc := range []string{"clean", "clean_with_history", "dirty", "pending_turn"} {
		t.Run(tc, func(t *testing.T) {
			tree, repo, info := tuiQuitWith(t, func(db *state.SessionDB, cwd string) {
				if tc != "clean" {
					if _, err := db.AppendMessage(context.Background(), "tui-wt", map[string]any{"role": "user", "content": "hello"}, state.AppendGuards{}); err != nil {
						t.Fatal(err)
					}
				}
				if tc != "pending_turn" {
					if err := db.EndSession("tui-wt", "tui_shutdown"); err != nil {
						t.Fatal(err)
					}
				}
				if tc == "pending_turn" {
					conn, err := sql.Open("sqlite", "file:"+filepath.Join(home.Dir(context.Background()), "state.db"))
					if err != nil {
						t.Fatal(err)
					}
					if _, err := conn.Exec("INSERT INTO session_admissions(admission_id,request_id,principal_id,target_session_id," +
						"lineage_json,payload_json,payload_digest,intent,status,outcome,owner_epoch,generation) " +
						"VALUES('adm-1','req-1','p','tui-wt','[]','{}','d','queue','queued',NULL,1,1)"); err != nil {
						t.Fatal(err)
					}
					conn.Close()
				}
				if tc == "dirty" {
					if err := os.WriteFile(filepath.Join(cwd, "work.txt"), []byte("uncommitted"), 0o644); err != nil {
						t.Fatal(err)
					}
				}
			})
			branches := tuiBranches(t, repo)
			if tc == "clean" || tc == "clean_with_history" {
				if _, err := os.Stat(tree); !errors.Is(err, fs.ErrNotExist) || tuiHasBranch(branches, info["branch"]) {
					t.Fatalf("clean tree kept: %v %v", err, branches)
				}
				return
			}
			if st, err := os.Stat(tree); err != nil || !st.IsDir() || !tuiHasBranch(branches, info["branch"]) {
				t.Fatalf("tree removed: %v %v", err, branches)
			}
			if tc == "dirty" {
				if data, _ := os.ReadFile(filepath.Join(tree, "work.txt")); string(data) != "uncommitted" {
					t.Fatalf("work lost: %q", data)
				}
			}
		})
	}
}

// State meta JSON of the gateway's own loop, heartbeat or goal automation row
func tuiScheduled(t *testing.T, kind, status string) string {
	t.Helper()
	var raw string
	var err error
	switch kind {
	case "loop":
		s := agent.NewSessionLoopState("run the tests")
		s.IntervalSeconds, s.Status = 600, status
		s.NextDueAt = float64(time.Now().UnixNano())/1e9 + 600
		s.Route = pyjson.ObjectOf("platform", "local", "chat_id", "tui-wt")
		raw, err = s.ToJSON()
	case "heartbeat":
		s := agent.NewHeartbeatState("check in", 600)
		s.Status = status
		raw, err = s.ToJSON()
	default:
		s := goals.NewGoalState("ship it")
		s.Status = status
		raw, err = s.ToJSON()
	}
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestQuitKeepsACleanTreeWhileTheGatewayHasScheduledWorkForItsSession(t *testing.T) {
	for _, kind := range []string{"loop", "heartbeat", "goal"} {
		t.Run(kind, func(t *testing.T) {
			tree, repo, info := tuiQuitWith(t, func(db *state.SessionDB, cwd string) {
				tuiAppendLoopPrompt(t, db)
				if err := db.SetMeta(context.Background(), kind+":tui-wt", tuiScheduled(t, kind, "active")); err != nil {
					t.Fatal(err)
				}
				if err := db.EndSession("tui-wt", "tui_shutdown"); err != nil {
					t.Fatal(err)
				}
			})
			if st, err := os.Stat(tree); err != nil || !st.IsDir() || !tuiHasBranch(tuiBranches(t, repo), info["branch"]) {
				t.Fatalf("%s: tree removed while the gateway still schedules work in it", kind)
			}
		})
	}
}

// Appends the scheduled prompt message the quit fixture session carries
func tuiAppendLoopPrompt(t *testing.T, db *state.SessionDB) {
	t.Helper()
	if _, err := db.AppendMessage(context.Background(), "tui-wt", map[string]any{"role": "user", "content": "every 10m: run the tests"}, state.AppendGuards{}); err != nil {
		t.Fatal(err)
	}
}

func TestFinishedScheduledWorkDoesNotKeepTheTree(t *testing.T) {
	for _, kind := range []string{"loop", "heartbeat", "goal"} {
		t.Run(kind, func(t *testing.T) {
			tree, repo, info := tuiQuitWith(t, func(db *state.SessionDB, cwd string) {
				tuiAppendLoopPrompt(t, db)
				if err := db.SetMeta(context.Background(), kind+":tui-wt", tuiScheduled(t, kind, "cleared")); err != nil {
					t.Fatal(err)
				}
				if err := db.EndSession("tui-wt", "tui_shutdown"); err != nil {
					t.Fatal(err)
				}
			})
			if _, err := os.Stat(tree); !errors.Is(err, fs.ErrNotExist) || tuiHasBranch(tuiBranches(t, repo), info["branch"]) {
				t.Fatalf("%s: finished work kept the tree", kind)
			}
		})
	}
}

func TestQuitKeepsTheTreeWhileAnotherViewerStillHoldsTheSession(t *testing.T) {
	tree, repo, info := tuiQuitWith(t, func(db *state.SessionDB, cwd string) { tuiAppendLoopPrompt(t, db) })
	if st, err := os.Stat(tree); err != nil || !st.IsDir() || !tuiHasBranch(tuiBranches(t, repo), info["branch"]) {
		t.Fatal("tree removed while another viewer holds the session")
	}
}

func TestQuitKeepsTheTreeWhileACronJobIsBoundToIt(t *testing.T) {
	for _, bound := range []string{"workdir", "origin"} {
		t.Run(bound, func(t *testing.T) {
			tree, repo, info := tuiQuitWith(t, func(db *state.SessionDB, cwd string) {
				tuiAppendLoopPrompt(t, db)
				if err := db.EndSession("tui-wt", "tui_shutdown"); err != nil {
					t.Fatal(err)
				}
				conn, err := sql.Open("sqlite", "file:"+filepath.Join(home.Dir(context.Background()), "state.db"))
				if err != nil {
					t.Fatal(err)
				}
				if _, err := conn.Exec("UPDATE sessions SET chat_id='local-chat' WHERE id='tui-wt'"); err != nil {
					t.Fatal(err)
				}
				conn.Close()
				tuiCronJob(t, bound, cwd)
			})
			if st, err := os.Stat(tree); err != nil || !st.IsDir() || !tuiHasBranch(tuiBranches(t, repo), info["branch"]) {
				t.Fatalf("cron %s: tree removed while a job still runs there", bound)
			}
		})
	}
}

// Writes an enabled every 10m cron job bound to the tree by workdir or to the local chat by origin
func tuiCronJob(t *testing.T, bound, cwd string) {
	t.Helper()
	job := pyjson.ObjectOf("id", "abc123def456", "name", "run the tests", "prompt", "run the tests",
		"schedule", pyjson.ObjectOf("kind", "interval", "minutes", int64(10), "display", "every 10m"),
		"schedule_display", "every 10m", "enabled", true, "state", "scheduled")
	if bound == "workdir" {
		job.Set("workdir", cwd)
	} else {
		job.Set("origin", pyjson.ObjectOf("platform", "local", "chat_id", "local-chat"))
	}
	text, err := pyjson.Dumps(pyjson.ObjectOf("jobs", []any{job}), pyjson.Indent(2))
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home.Dir(context.Background()), "cron")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "jobs.json"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}
