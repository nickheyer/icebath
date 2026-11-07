package selfupdate

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

const dgSchemaPath = "pkg/state/common_schema.go"

// Checkout whose old-release tag predates the runtime ledger and whose HEAD defines it
func dgRepo(t *testing.T) string {
	root := filepath.Join(t.TempDir(), "checkout")
	os.MkdirAll(filepath.Join(root, "pkg", "state"), 0o755)
	srGit(t, root, "init", "-b", "main")
	srGit(t, root, "config", "user.email", "t@example.invalid")
	srGit(t, root, "config", "user.name", "T")
	os.WriteFile(filepath.Join(root, dgSchemaPath), []byte("const schemaSQL = `CREATE TABLE IF NOT EXISTS sessions (id TEXT)`\n"), 0o644)
	srGit(t, root, "add", ".")
	srGit(t, root, "commit", "-m", "release before the cutover")
	srGit(t, root, "tag", "old-release")
	os.WriteFile(filepath.Join(root, dgSchemaPath), []byte("const schemaSQL = `CREATE TABLE IF NOT EXISTS session_admissions (seq INTEGER)`\n"), 0o644)
	srGit(t, root, "commit", "-am", "gateway runtime")
	return root
}

func dgStore(t *testing.T, dbPath string, ddl ...string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(dbPath), 0o755)
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, stmt := range ddl {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
}

// State store holding gateway runtime ledger rows
func dgLedgered(t *testing.T, h string) {
	dgStore(t, filepath.Join(h, "state.db"), "CREATE TABLE sessions (id TEXT PRIMARY KEY)",
		"CREATE TABLE session_admissions (seq INTEGER PRIMARY KEY, session_id TEXT)",
		"CREATE TABLE worker_executions (id TEXT PRIMARY KEY)", "INSERT INTO session_admissions (session_id) VALUES ('s')")
}

func dgSnapshot(t *testing.T, h, id string, runtime bool) {
	ddl := "CREATE TABLE sessions (id TEXT PRIMARY KEY)"
	if runtime {
		ddl = "CREATE TABLE session_admissions (seq INTEGER)"
	}
	dgStore(t, filepath.Join(h, "state-snapshots", id, "state.db"), ddl)
}

func TestDowngradeRefusesAPreRuntimeTargetNamingTheSnapshotThatPredatesIt(t *testing.T) {
	repo := dgRepo(t)
	h := filepath.Join(t.TempDir(), "home")
	t.Setenv("ICEBATH_HOME", h)
	dgLedgered(t, h)
	dgSnapshot(t, h, "20261001-000000-pre-update", false)
	dgSnapshot(t, h, "20261008-000000-pre-update", true)
	head := srGit(t, repo, "rev-parse", "HEAD")
	out := downgradeRefusalGit(context.Background(), []string{"git"}, repo, srGit(t, repo, "rev-parse", "old-release"), h)
	if out == nil {
		t.Fatal("a pre runtime target must be refused")
	}
	for _, want := range []string{"Downgrading is unsupported", filepath.Join(h, "state.db"), "/snapshot restore 20261001-000000-pre-update", "icebath gateway stop"} {
		if !strings.Contains(*out, want) {
			t.Fatalf("refusal lacks %q:\n%s", want, *out)
		}
	}
	if strings.Contains(*out, "20261008") || srGit(t, repo, "rev-parse", "HEAD") != head {
		t.Fatalf("refusal named a runtime snapshot or moved HEAD:\n%s", *out)
	}
}

func TestDowngradeForwardUpdatesAndStoresWithoutLedgerRowsAreNeverRefused(t *testing.T) {
	repo := dgRepo(t)
	t.Setenv("ICEBATH_HOME", filepath.Join(t.TempDir(), "unrelated"))
	old := srGit(t, repo, "rev-parse", "old-release")
	plain := filepath.Join(t.TempDir(), "plain")
	dgStore(t, filepath.Join(plain, "state.db"), "CREATE TABLE session_admissions (seq INTEGER)", "CREATE TABLE worker_executions (id TEXT)")
	ctx := context.Background()
	if out := downgradeRefusalGit(ctx, []string{"git"}, repo, old, plain); out != nil {
		t.Fatalf("an empty ledger refused: %s", *out)
	}
	ledgered := filepath.Join(t.TempDir(), "ledgered")
	dgLedgered(t, ledgered)
	head := srGit(t, repo, "rev-parse", "HEAD")
	if out := downgradeRefusalGit(ctx, []string{"git"}, repo, head, ledgered); out != nil {
		t.Fatalf("the running commit refused: %s", *out)
	}
	srGit(t, repo, "checkout", "-q", "--detach", old)
	if out := downgradeRefusalGit(ctx, []string{"git"}, repo, head, ledgered); out != nil {
		t.Fatalf("a forward target refused: %s", *out)
	}
}

func TestDowngradeRuntimeSchemaMarkerIsStillInThisTreesStateSchema(t *testing.T) {
	sources, err := filepath.Glob(filepath.Join("..", "state", "*.go"))
	if err != nil || len(sources) == 0 {
		t.Fatalf("no state schema sources %v", err)
	}
	var texts []string
	for _, path := range sources {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		texts = append(texts, string(data))
	}
	if !definesRuntime(texts...) {
		t.Fatal("no pkg/state source defines the runtime ledger")
	}
}

func dgTree(t *testing.T, dir string) map[string][]byte {
	files := map[string][]byte{}
	filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			data, _ := os.ReadFile(path)
			files[path] = data
		}
		return nil
	})
	return files
}

func TestDowngradePresentCommitWithMissingSchemaBlobsUsesRemoteWithoutGitWrites(t *testing.T) {
	yes, no := true, false
	for _, runtime := range []bool{false, true} {
		for _, remoteResult := range []string{"true", "false", "none", "no-reader"} {
			repo := dgRepo(t)
			start := "old-release"
			if runtime {
				start = "HEAD"
			}
			srGit(t, repo, "checkout", "-q", "-b", "release-line", start)
			os.WriteFile(filepath.Join(repo, "VERSION"), []byte("1.2.3\n"), 0o644)
			srGit(t, repo, "add", "VERSION")
			srGit(t, repo, "commit", "-qm", "release version bump")
			target := srGit(t, repo, "rev-parse", "HEAD")
			srGit(t, repo, "checkout", "-q", "main")
			os.WriteFile(filepath.Join(repo, dgSchemaPath), []byte("const schemaSQL = `CREATE TABLE IF NOT EXISTS session_admissions (seq INTEGER, extra TEXT)`\n"), 0o644)
			srGit(t, repo, "commit", "-qam", "newer runtime schema")
			srGit(t, repo, "config", "uploadpack.allowFilter", "true")
			tmp := t.TempDir()
			clone := filepath.Join(tmp, "partial")
			srGit(t, tmp, "clone", "--filter=blob:none", "--no-checkout", "file://"+filepath.ToSlash(repo), clone)
			trace := filepath.Join(tmp, "git-trace.jsonl")
			t.Setenv("GIT_TRACE2_EVENT", trace)
			probe := func(args ...string) *sourceGitResult {
				result, err := sourceGitRun(context.Background(), gitArgs([]string{"git"}, args...), clone, 10*time.Second)
				if err != nil {
					t.Fatal(err)
				}
				return result
			}
			git := func(args ...string) int { return probe(args...).ReturnCode }
			before := dgTree(t, filepath.Join(clone, ".git"))
			if git("cat-file", "-e", target+"^{commit}") != 0 || git("cat-file", "-e", target+":"+dgSchemaPath) == 0 {
				t.Fatal("the partial clone must hold the commit without its schema blob")
			}
			if grep := probe("grep", "-q", "-F", runtimeSchemaMarker, target, "--", StateModules); grep.ReturnCode == 0 || grep.Stderr == "" {
				t.Fatalf("grep over a missing blob answered %d %q", grep.ReturnCode, grep.Stderr)
			}
			var calls []string
			var remote func(string) *bool
			if remoteResult != "no-reader" {
				remote = func(ref string) *bool {
					calls = append(calls, ref)
					switch remoteResult {
					case "true":
						return &yes
					case "false":
						return &no
					}
					return nil
				}
			}
			if got := targetPredatesRuntime(context.Background(), nil, clone, target, git, remote); got != (remoteResult == "true") {
				t.Fatalf("runtime %v remote %s answered %v", runtime, remoteResult, got)
			}
			if remote != nil && (len(calls) != 1 || calls[0] != target) {
				t.Fatalf("remote calls %v", calls)
			}
			after := dgTree(t, filepath.Join(clone, ".git"))
			if len(after) != len(before) {
				t.Fatalf("git wrote into the clone: %d files became %d", len(before), len(after))
			}
			for path, data := range before {
				if !bytes.Equal(after[path], data) {
					t.Fatalf("git rewrote %s", path)
				}
			}
			data, _ := os.ReadFile(trace)
			for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
				var event map[string]any
				if json.Unmarshal([]byte(line), &event) == nil && event["event"] == "child_start" {
					t.Fatalf("a probe started a child: %s", line)
				}
			}
		}
	}
}

func TestDowngradeLocalSchemaDecidesWithoutRemoteUnlessTheProbeFails(t *testing.T) {
	for _, grepError := range []bool{false, true} {
		repo := dgRepo(t)
		runtime := srGit(t, repo, "rev-parse", "HEAD")
		os.WriteFile(filepath.Join(repo, "notes.txt"), []byte("later build\n"), 0o644)
		srGit(t, repo, "add", "notes.txt")
		srGit(t, repo, "commit", "-qm", "later build")
		calls := 0
		remote := func(ref string) *bool {
			calls++
			verdict := ref == "old-release"
			return &verdict
		}
		run := updaterGit(context.Background(), []string{"git"}, repo)
		git := func(args ...string) int {
			if grepError && args[0] == "grep" {
				return -1
			}
			return run(args...)
		}
		ctx := context.Background()
		if !targetPredatesRuntime(ctx, nil, repo, "old-release", git, remote) {
			t.Fatal("the pre runtime release must predate the runtime")
		}
		if targetPredatesRuntime(ctx, nil, repo, runtime, git, remote) || targetPredatesRuntime(ctx, nil, repo, "HEAD", git, remote) {
			t.Fatal("a runtime release must not predate the runtime")
		}
		want := 0
		if grepError {
			want = 2
		}
		if calls != want {
			t.Fatalf("grep error %v asked remote %d times", grepError, calls)
		}
	}
}

func TestDowngradeNamedProfileRefusalNamesItsOwnProfile(t *testing.T) {
	repo := dgRepo(t)
	old := srGit(t, repo, "rev-parse", "old-release")
	root := filepath.Join(t.TempDir(), "root")
	acme := filepath.Join(root, "profiles", "acme")
	t.Setenv("ICEBATH_HOME", acme)
	dgLedgered(t, acme)
	dgSnapshot(t, acme, "20261001-000000-pre-update", false)
	out := downgradeRefusalGit(context.Background(), []string{"git"}, repo, old, acme)
	if out == nil || !strings.Contains(*out, acme+": run `icebath -p acme` and enter `/snapshot restore 20261001-000000-pre-update`") ||
		strings.Contains(*out, "run `icebath` and") {
		t.Fatalf("named profile refusal:\n%v", out)
	}
	dgLedgered(t, root)
	dgSnapshot(t, root, "20261002-000000-pre-update", false)
	t.Setenv("ICEBATH_HOME", root)
	out = downgradeRefusalGit(context.Background(), []string{"git"}, repo, old, root)
	if out == nil || !strings.Contains(*out, root+": run `icebath` and enter `/snapshot restore 20261002-000000-pre-update`") {
		t.Fatalf("default root refusal:\n%v", out)
	}
}

func dgBuild(t *testing.T, version string) {
	previous := updaterecord.BuildVersion()
	updaterecord.SetBuildVersion(version)
	t.Cleanup(func() { updaterecord.SetBuildVersion(previous) })
}

func TestDowngradeBinaryReleaseAtOrAboveTheRunningBuildIsForward(t *testing.T) {
	dgBuild(t, "v1.5.0")
	var requests atomic.Int32
	srRoute(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.WriteHeader(500) }))
	h := filepath.Join(t.TempDir(), "home")
	t.Setenv("ICEBATH_HOME", h)
	dgLedgered(t, h)
	root := t.TempDir()
	for _, tag := range []string{"v1.5.0", "v1.6.0"} {
		refuses, err := downgradeWouldRefuse(context.Background(), root, &Release{Tag: tag})
		if err != nil || refuses {
			t.Fatalf("%s refused %v %v", tag, refuses, err)
		}
		if out, err := downgradeRefusal(context.Background(), root, &Release{Tag: tag}); err != nil || out != nil {
			t.Fatalf("%s refusal %v %v", tag, out, err)
		}
	}
	if requests.Load() != 0 {
		t.Fatalf("a forward release asked GitHub %d times", requests.Load())
	}
}

func TestDowngradeBinaryOlderReleaseIsJudgedFromItsStateSchemaSources(t *testing.T) {
	dgBuild(t, "v1.5.0")
	var mu sync.Mutex
	var seen []string
	paths := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
	sources := map[string]string{
		"/raw/" + OfficialRepository + "/v1.2.0/pkg/state/common_schema.go": "const schemaSQL = `CREATE TABLE IF NOT EXISTS sessions (id TEXT)`",
		"/raw/" + OfficialRepository + "/v1.2.0/pkg/state/store.go":         "package state",
		"/raw/" + OfficialRepository + "/v1.4.0/pkg/state/common_schema.go": "const schemaSQL = `" + runtimeSchemaMarker + " (seq INTEGER)`",
		"/raw/" + OfficialRepository + "/v1.4.0/pkg/state/store.go":         "package state",
	}
	listing := `[{"type": "file", "path": "pkg/state/common_schema.go"}, {"type": "file", "path": "pkg/state/store.go"},` +
		` {"type": "dir", "path": "pkg/state/sub"}, {"type": "file", "path": "pkg/state/README.md"}]`
	srRoute(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.URL.Path+"?"+r.URL.RawQuery)
		mu.Unlock()
		switch {
		case r.URL.Path == "/repos/"+OfficialRepository+"/contents/pkg/state" && r.URL.Query().Get("ref") != "v1.3.0":
			w.Write([]byte(listing))
		case sources[r.URL.Path] != "":
			w.Write([]byte(sources[r.URL.Path]))
		default:
			w.WriteHeader(500)
		}
	}))
	h := filepath.Join(t.TempDir(), "home")
	t.Setenv("ICEBATH_HOME", h)
	root := t.TempDir()
	ctx := context.Background()
	if refuses, _ := downgradeWouldRefuse(ctx, root, &Release{Tag: "v1.2.0"}); refuses || len(paths()) != 0 {
		t.Fatalf("no ledgered store must refuse nothing and ask nothing, asked %v", paths())
	}
	dgLedgered(t, h)
	if refuses, err := downgradeWouldRefuse(ctx, root, &Release{Tag: "v1.2.0"}); err != nil || !refuses {
		t.Fatalf("a pre runtime release was offered %v %v", refuses, err)
	}
	out, err := downgradeRefusal(ctx, root, &Release{Tag: "v1.2.0"})
	if err != nil || out == nil || !strings.Contains(*out, "Refusing to move this install to v1.2.0") {
		t.Fatalf("refusal %v %v", out, err)
	}
	if refuses, _ := downgradeWouldRefuse(ctx, root, &Release{Tag: "v1.4.0"}); refuses {
		t.Fatal("a release whose schema defines the runtime must not be refused")
	}
	asked := len(paths())
	if refuses, _ := downgradeWouldRefuse(ctx, root, &Release{Tag: "v1.3.0"}); refuses {
		t.Fatal("an unreadable schema proves nothing")
	}
	if refuses, _ := downgradeWouldRefuse(ctx, root, &Release{Tag: "v1.3.0"}); refuses || len(paths()) != asked+1 {
		t.Fatalf("an unknown verdict must wait for its retry time, asked %v", paths()[asked:])
	}
	if refuses, _ := downgradeWouldRefuse(ctx, root, &Release{Tag: "v1.2.0"}); !refuses || len(paths()) != asked+1 {
		t.Fatalf("a definite verdict must come from the cache, asked %v", paths()[asked:])
	}
	data, _ := os.ReadFile(filepath.Join(h, "source-checks", "release-runtime.json"))
	var cache map[string]any
	if json.Unmarshal(data, &cache) != nil || cache["v1.2.0"] != true || cache["v1.4.0"] != false {
		t.Fatalf("verdict cache %s", data)
	}
	if retry, ok := cache["v1.3.0"].(map[string]any); !ok || retry["retryAt"].(float64) < float64(time.Now().Unix()+3000) {
		t.Fatalf("unknown verdict entry %v", cache["v1.3.0"])
	}
}

func TestDowngradeCheckHoldsAReleaseThatUpdateWouldRefuse(t *testing.T) {
	for _, divergent := range []bool{true, false} {
		repo := dgRepo(t)
		h := filepath.Join(t.TempDir(), "home")
		os.MkdirAll(h, 0o755)
		t.Setenv("ICEBATH_HOME", h)
		dgBuild(t, "v1.2.0")
		tag := "v1.0.0"
		srGit(t, repo, "tag", tag, "old-release")
		if divergent {
			tag = "v1.3.0"
			srGit(t, repo, "checkout", "-q", "-b", "release-line", "old-release")
			os.WriteFile(filepath.Join(repo, "notes.txt"), []byte("release fix\n"), 0o644)
			srGit(t, repo, "add", "notes.txt")
			srGit(t, repo, "commit", "-qm", "release from the older line")
			srGit(t, repo, "tag", tag)
			srGit(t, repo, "checkout", "-q", "main")
		}
		saved := resolveCheckTarget
		resolveCheckTarget = func(context.Context, string, bool) (ReleaseTarget, error) {
			return ReleaseTarget{RequestedChannel: "stable", Channel: "stable", Repository: OfficialRepository,
				Release: &Release{Tag: tag}, Version: strings.TrimPrefix(tag, "v")}, nil
		}
		var out strings.Builder
		if err := CheckChannel(context.Background(), &out, "stable", repo); err != nil {
			t.Fatal(err)
		}
		want := "moves it back"
		if divergent {
			want = "Selected release available"
		}
		if !strings.Contains(out.String(), want) {
			t.Fatalf("divergent %v without runtime history printed %q", divergent, out.String())
		}
		dgLedgered(t, h)
		out.Reset()
		if err := CheckChannel(context.Background(), &out, "stable", repo); err != nil {
			t.Fatal(err)
		}
		if got := out.String(); !strings.Contains(got, "Holding at this build") || strings.Contains(got, "Selected release available") ||
			strings.Contains(got, "moves it back") {
			t.Fatalf("divergent %v with runtime history printed %q", divergent, got)
		}
		resolveCheckTarget = saved
	}
}
