package selfupdate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

type scInstall struct {
	root, linked, home, base, head string
	mu                             sync.Mutex
	responses                      map[string][2]any
	requests, auths                []string
	decide                         map[string]func(*http.Request) [2]any
	headers                        map[string]map[string]string
}

// Authorization header of each call to path in request order, empty when anonymous
func (f *scInstall) authorizations(path string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var seen []string
	for i, asked := range f.requests {
		if asked == path {
			seen = append(seen, f.auths[i])
		}
	}
	return seen
}

func (f *scInstall) respond(path string, code int, body any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.responses[path] = [2]any{code, body}
}

func (f *scInstall) asked() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...)
}

func (f *scInstall) count(path string) int {
	n := 0
	for _, asked := range f.asked() {
		if asked == path {
			n++
		}
	}
	return n
}

func (f *scInstall) git(t *testing.T, dir string, args ...string) string {
	return srGit(t, dir, append([]string{"-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "-c", "commit.gpgsign=false"}, args...)...)
}

// Fork checkout with a linked feature worktree, a profile home and a local GitHub
func scFixture(t *testing.T) *scInstall {
	tmp := t.TempDir()
	f := &scInstall{root: filepath.Join(tmp, "checkout"), linked: filepath.Join(tmp, "linked"), home: filepath.Join(tmp, "profile"),
		responses: map[string][2]any{}}
	os.MkdirAll(f.root, 0o755)
	os.MkdirAll(f.home, 0o755)
	t.Setenv("ICEBATH_HOME", f.home)
	t.Setenv("ICEBATH_REVISION", "")
	t.Setenv("GIT_ALLOW_PROTOCOL", "file")
	f.git(t, f.root, "init", "-b", "main")
	f.git(t, f.root, "commit", "--allow-empty", "-m", "base")
	f.base = f.git(t, f.root, "rev-parse", "HEAD")
	f.git(t, f.root, "commit", "--allow-empty", "-m", "local")
	f.head = f.git(t, f.root, "rev-parse", "HEAD")
	f.git(t, f.root, "remote", "add", "origin", "https://github.com/fixture/fork.git")
	f.git(t, f.root, "worktree", "add", "-b", "feature/gui", f.linked)
	srRoute(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.RequestURI
		f.mu.Lock()
		f.requests = append(f.requests, path)
		f.auths = append(f.auths, r.Header.Get("Authorization"))
		entry, ok := f.responses[path]
		decide := f.decide[path]
		headers := f.headers[path]
		f.mu.Unlock()
		if !ok {
			entry = [2]any{404, map[string]any{}}
		}
		if decide != nil {
			entry = decide(r)
		}
		for name, value := range headers {
			w.Header().Set(name, value)
		}
		w.WriteHeader(entry[0].(int))
		if text, ok := entry[1].(string); ok {
			w.Write([]byte(text))
			return
		}
		body, _ := json.Marshal(entry[1])
		w.Write(body)
	}))
	return f
}

func scGet(t *testing.T, status *pyjson.Object, key string) any {
	t.Helper()
	value, _ := status.Get(key)
	return value
}

func TestSourceCheckCountsAreHonestWithoutFetch(t *testing.T) {
	cases := []struct {
		tip      string
		compare  any
		expected int64
	}{{"head", nil, 0}, {"base", nil, 0}, {"unknown", map[string]any{"ahead_by": 61}, 61},
		{"unknown", map[string]any{"ahead_by": 0}, 0}, {"unknown", nil, -1}, {"unknown", map[string]any{"ahead_by": true}, -1}}
	for _, tc := range cases {
		f := scFixture(t)
		target := map[string]string{"head": f.head, "base": f.base, "unknown": strings.Repeat("a", 40)}[tc.tip]
		f.respond("/repos/fixture/fork/commits/main", 200, "\xef\xbb\xbf"+target)
		if tc.compare != nil {
			body, _ := json.Marshal(tc.compare)
			f.respond("/repos/fixture/fork/compare/"+f.head+"..."+target, 200, "\xef\xbb\xbf"+string(body))
		}
		status, err := CheckForUpdates(context.Background(), SourceCheckOptions{InstallRoot: f.root, Home: f.home})
		if err != nil {
			t.Fatal(err)
		}
		if got, _ := pyIntValue(scGet(t, status, "behind")); got != tc.expected || scGet(t, status, "updateAvailable") != (tc.expected != 0) {
			t.Fatalf("%s %v answered %v", tc.tip, tc.compare, status)
		}
		if asked := f.asked(); tc.tip != "unknown" && (len(asked) != 1 || asked[0] != "/repos/fixture/fork/commits/main") {
			t.Fatalf("%s asked %v", tc.tip, asked)
		}
	}
}

func TestSourceCheckCacheForceExpiryAndPassiveOptOut(t *testing.T) {
	f := scFixture(t)
	clock := 1000000.0
	previous := sourceCheckClock
	sourceCheckClock = func() float64 { return clock }
	t.Cleanup(func() { sourceCheckClock = previous })
	url := "/repos/fixture/fork/commits/main"
	f.respond(url, 200, f.head)
	check := func(opts SourceCheckOptions) *pyjson.Object {
		t.Helper()
		opts.InstallRoot, opts.Home = f.root, f.home
		status, err := CheckForUpdates(context.Background(), opts)
		if err != nil {
			t.Fatal(err)
		}
		return status
	}
	behindZero := func(status *pyjson.Object) bool { n, ok := pyIntValue(scGet(t, status, "behind")); return ok && n == 0 }
	if !behindZero(check(SourceCheckOptions{})) {
		t.Fatal("first check")
	}
	caches, _ := filepath.Glob(filepath.Join(f.home, "source-checks", "*.json"))
	raw, _ := os.ReadFile(caches[0])
	if bytes.HasPrefix(raw, []byte("\xef\xbb\xbf")) {
		t.Fatal("the cache must be written without a BOM")
	}
	os.WriteFile(caches[0], append([]byte("\xef\xbb\xbf"), raw...), 0o644)
	f.respond(url, 503, map[string]any{})
	os.WriteFile(filepath.Join(f.root, "dirty.txt"), []byte("carried work"), 0o644)
	if scGet(t, check(SourceCheckOptions{}), "dirty") != true || f.count(url) != 1 {
		t.Fatal("a cached status must carry the live dirty flag without a request")
	}
	if scGet(t, check(SourceCheckOptions{Force: true}), "error") != "fetch-failed" {
		t.Fatal("force must bypass the cache")
	}
	clock += 3599
	if scGet(t, check(SourceCheckOptions{}), "error") != "fetch-failed" || f.count(url) != 2 {
		t.Fatal("a failure stays cached for an hour")
	}
	clock += 2
	f.respond(url, 200, f.head)
	if !behindZero(check(SourceCheckOptions{})) || f.count(url) != 3 {
		t.Fatal("a failure expires after an hour")
	}
	clock += 86401
	if !behindZero(check(SourceCheckOptions{})) || f.count(url) != 4 {
		t.Fatal("a success expires after a day")
	}
	os.WriteFile(filepath.Join(f.home, "config.yaml"), []byte("updates: {check: false}"), 0o644)
	if scGet(t, check(SourceCheckOptions{Passive: true}), "behind") != nil || !behindZero(check(SourceCheckOptions{})) || f.count(url) != 4 {
		t.Fatal("passive opt out")
	}
	f.git(t, f.root, "commit", "--allow-empty", "-m", "moved")
	f.respond(url, 200, f.git(t, f.root, "rev-parse", "HEAD"))
	if !behindZero(check(SourceCheckOptions{})) || f.count(url) != 5 {
		t.Fatal("a moved HEAD is a new identity")
	}
}

func TestSourceCheckExplicitAndCurrentBranchHealOnlyAfterConfirmedAbsence(t *testing.T) {
	f := scFixture(t)
	f.git(t, f.root, "remote", "set-url", "origin", f.root)
	ctx := context.Background()
	branch := func(name *string, force bool) *pyjson.Object {
		status, err := CheckForUpdates(ctx, SourceCheckOptions{InstallRoot: f.linked, Home: f.home, Branch: name, Force: force})
		if err != nil {
			t.Fatal(err)
		}
		return status
	}
	mainName, deleted := "main", "deleted"
	if got := scGet(t, branch(nil, false), "branch"); got != "feature/gui" {
		t.Fatalf("current branch %v", got)
	}
	if got := scGet(t, branch(&mainName, false), "branch"); got != "main" {
		t.Fatalf("explicit main %v", got)
	}
	if got := scGet(t, branch(&deleted, false), "branch"); got != "main" {
		t.Fatalf("deleted branch %v", got)
	}
	f.git(t, f.root, "remote", "set-url", "origin", filepath.Join(f.home, "unreachable"))
	failed := branch(&deleted, true)
	if scGet(t, failed, "branch") != "deleted" || scGet(t, failed, "error") != "fetch-failed" {
		t.Fatalf("unreachable origin %v", failed)
	}
	if got := f.git(t, f.linked, "branch", "--show-current"); got != "feature/gui" || len(f.asked()) != 0 {
		t.Fatalf("branch %s requests %v", got, f.asked())
	}
}

func TestSourceCheckMainFollowsTheBranchWithoutReadingAChannelRecord(t *testing.T) {
	f := scFixture(t)
	branchPath := "/repos/fixture/fork/commits/desktop-choice"
	f.respond(branchPath, 200, f.head)
	branchFile := filepath.Join(f.home, "desktop-update.json")
	os.WriteFile(branchFile, []byte(`{"branch": "desktop-choice"}`), 0o644)
	channel := "main"
	status, err := CheckForUpdates(context.Background(), SourceCheckOptions{InstallRoot: f.linked, Home: f.home, Channel: &channel, BranchConfigPath: branchFile})
	if err != nil {
		t.Fatal(err)
	}
	if _, failed := status.Get("error"); failed || scGet(t, status, "branch") != "desktop-choice" || scGet(t, status, "targetSha") != f.head {
		t.Fatalf("status %v", status)
	}
	if asked := f.asked(); len(asked) != 1 || asked[0] != branchPath {
		t.Fatalf("requests %v", asked)
	}
	data, _ := os.ReadFile(branchFile)
	if !strings.Contains(string(data), `"desktop-choice"`) {
		t.Fatalf("the Desktop branch setting changed %s", data)
	}
}

func TestSourceCheckMainPrintsOneSelectableStatusLine(t *testing.T) {
	f := scFixture(t)
	f.respond("/repos/fixture/fork/commits/main", 200, f.head)
	var stdout, stderr bytes.Buffer
	code := SourceCheckMain(context.Background(), []string{"--install-root", f.root, "--home", f.home}, &stdout, &stderr)
	var status map[string]any
	if code != 0 || json.Unmarshal(stdout.Bytes(), &status) != nil || status["channelSelectable"] != true || status["behind"] != 0.0 {
		t.Fatalf("exit %d stdout %s stderr %s", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := SourceCheckMain(context.Background(), []string{"--home", f.home}, &stdout, &stderr); code != 2 ||
		!strings.Contains(stderr.String(), "the following arguments are required: --install-root") {
		t.Fatalf("missing root exit %d %s", code, stderr.String())
	}
	if code := SourceCheckMain(context.Background(), []string{"--install-root", f.root, "--home", f.home, "--channel", "Bad Name"}, &stdout, &stderr); code != 2 ||
		!strings.Contains(stderr.String(), "argument --channel: invalid validate_name value: 'Bad Name'") {
		t.Fatalf("invalid channel exit %d %s", code, stderr.String())
	}
}

func TestSourceCheckRuntimeLookupHasOneDeadlineWithBoundedParallelReads(t *testing.T) {
	for _, blocked := range []string{"tree", "modules"} {
		f := scFixture(t)
		sha := f.head
		listing := "/repos/fixture/fork/contents/pkg/state?ref=" + sha
		var entries []any
		for i := 0; i < 12; i++ {
			entries = append(entries, map[string]any{"path": "pkg/state/schema_" + string(rune('a'+i)) + ".go", "type": "file"})
		}
		release := make(chan struct{})
		overlapped := make(chan struct{})
		var mu sync.Mutex
		active, peak := 0, 0
		stalled := func(w http.ResponseWriter) {
			mu.Lock()
			active++
			if active > peak {
				peak = active
			}
			if active == 4 {
				close(overlapped)
			}
			mu.Unlock()
			select {
			case <-release:
			case <-time.After(20 * time.Second):
			}
			mu.Lock()
			active--
			mu.Unlock()
			w.Write([]byte("const schemaSQL = `old schema`"))
		}
		srRoute(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			path := r.URL.Path + "?" + r.URL.RawQuery
			if path == listing {
				if blocked == "tree" {
					stalled(w)
					return
				}
				body, _ := json.Marshal(entries)
				w.Write(body)
				return
			}
			if strings.HasPrefix(r.URL.Path, "/raw/fixture/fork/"+sha+"/pkg/state/") {
				stalled(w)
				return
			}
			w.WriteHeader(404)
		}))
		releaseOnce := sync.OnceFunc(func() { close(release) })
		t.Cleanup(releaseOnce)
		previous := runtimeLookupTimeout
		runtimeLookupTimeout = 3 * time.Second
		finished := make(chan *bool, 1)
		go func() {
			finished <- githubPredatesRuntime(context.Background(), "fixture/fork", sha, filepath.Join(f.home, "runtime.json"))
		}()
		if blocked == "modules" {
			select {
			case <-overlapped:
			case <-time.After(5 * time.Second):
				t.Fatal("independent schema reads must overlap")
			}
		}
		select {
		case verdict := <-finished:
			if verdict != nil {
				t.Fatalf("%s stalled lookup answered %v", blocked, *verdict)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("a slow response must not extend the lookup deadline")
		}
		mu.Lock()
		if peak > 4 {
			t.Fatalf("%d schema reads ran at once", peak)
		}
		mu.Unlock()
		runtimeLookupTimeout = previous
		releaseOnce()
	}
}

func TestSourceCheckUnknownRuntimeVerdictExpiresButDefiniteVerdictDoesNot(t *testing.T) {
	for _, failure := range []string{"tree", "module"} {
		for _, runtime := range []bool{true, false} {
			f := scFixture(t)
			clock := 1000000.0
			previous := sourceCheckClock
			sourceCheckClock = func() float64 { return clock }
			sha := f.head
			listing := "/repos/fixture/fork/contents/pkg/state?ref=" + sha
			module := "/raw/fixture/fork/" + sha + "/pkg/state/common_schema.go"
			schema := "const schemaSQL = `old schema`"
			if runtime {
				schema = "const schemaSQL = `" + runtimeSchemaMarker + " (seq INTEGER)`"
			}
			f.respond(listing, 200, []any{map[string]any{"path": "pkg/state/common_schema.go", "type": "file"}})
			f.respond(module, 200, schema)
			failed := listing
			if failure == "module" {
				failed = module
			}
			good := f.responses[failed]
			f.respond(failed, 503, map[string]any{})
			check := func() *bool {
				return githubPredatesRuntime(context.Background(), "fixture/fork", sha, filepath.Join(f.home, "runtime.json"))
			}
			if check() != nil {
				t.Fatalf("%s failure must be unknown", failure)
			}
			seen := len(f.asked())
			f.respond(failed, good[0].(int), good[1])
			if check() != nil || len(f.asked()) != seen {
				t.Fatal("cache hits must not retry an unreachable GitHub")
			}
			clock += updateCheckFailureCacheSeconds + 1
			if verdict := check(); verdict == nil || *verdict == runtime {
				t.Fatalf("runtime %v answered %v after the retry time", runtime, verdict)
			}
			seen = len(f.asked())
			clock += 10 * updateCheckCacheSeconds
			if verdict := check(); verdict == nil || *verdict == runtime || len(f.asked()) != seen {
				t.Fatal("a definite verdict never expires")
			}
			sourceCheckClock = previous
		}
	}
}

func TestSourceCheckInvalidConfigRefusesTheDesktopChannelProbe(t *testing.T) {
	root := newChSource(t).root
	for _, content := range [][]byte{[]byte("update: ["), []byte("- not-a-mapping"), {0xff}} {
		h := filepath.Join(t.TempDir(), "home")
		os.MkdirAll(h, 0o755)
		config := filepath.Join(h, "config.yaml")
		os.WriteFile(config, content, 0o644)
		t.Setenv("ICEBATH_HOME", h)
		t.Setenv("ICEBATH_IGNORE_USER_CONFIG", "0")
		var stdout, stderr strings.Builder
		code := SourceCheckMain(context.Background(), []string{"--install-root", root, "--home", h}, &stdout, &stderr)
		if code == 0 || strings.Contains(stdout.String(), `"channel": "main"`) || !strings.Contains(stderr.String(), "config") {
			t.Fatalf("%q exited %d with %q %q", content, code, stdout.String(), stderr.String())
		}
		if after, _ := os.ReadFile(config); string(after) != string(content) {
			t.Fatalf("config rewritten to %q", after)
		}
	}
}

func lfGit(t *testing.T, cwd string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

func TestSourceCheckAncestryProbeNeverFetchesFromThePromisor(t *testing.T) {
	tmp := t.TempDir()
	seed, up, clone := filepath.Join(tmp, "seed"), filepath.Join(tmp, "up.git"), filepath.Join(tmp, "clone")
	lfGit(t, tmp, "init", "-q", "-b", "main", seed)
	for i := 0; i < 2; i++ {
		os.WriteFile(filepath.Join(seed, fmt.Sprintf("f%d.txt", i)), []byte(strings.Repeat(fmt.Sprintf("v%d\n", i), 50)), 0o644)
		lfGit(t, seed, "add", "-A")
		lfGit(t, seed, "commit", "-qm", fmt.Sprintf("c%d", i))
	}
	lfGit(t, tmp, "clone", "-q", "--bare", seed, up)
	lfGit(t, up, "config", "uploadpack.allowFilter", "true")
	lfGit(t, up, "config", "uploadpack.allowAnySHA1InWant", "true")
	lfGit(t, tmp, "clone", "-q", "--filter=blob:none", "--no-checkout", "file://"+filepath.ToSlash(up), clone)
	os.WriteFile(filepath.Join(seed, "new.txt"), []byte("upstream moved\n"), 0o644)
	lfGit(t, seed, "add", "-A")
	lfGit(t, seed, "commit", "-qm", "upstream")
	lfGit(t, seed, "push", "-q", up, "main")
	head, tip := lfGit(t, clone, "rev-parse", "HEAD"), lfGit(t, up, "rev-parse", "main")
	fetched := func(sha string) bool {
		cmd := exec.Command("git", "cat-file", "-e", sha)
		cmd.Dir = clone
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1", "GIT_NO_LAZY_FETCH=1")
		return cmd.Run() == nil
	}
	ctx := context.Background()
	if checkGitOK(ctx, []string{"merge-base", "--is-ancestor", tip, head}, clone, "git") {
		t.Fatal("an unfetched upstream tip read as an ancestor")
	}
	if fetched(tip) {
		t.Fatal("the update check downloaded upstream history")
	}
	if parent := lfGit(t, clone, "rev-parse", "HEAD~1"); !checkGitOK(ctx, []string{"merge-base", "--is-ancestor", parent, head}, clone, "git") {
		t.Fatal("a local ancestor did not read as up to date")
	}
}

// Source branch channel record of a repository as the release publisher writes it
func scSourceChannel(name, repository, branch string) map[string]any {
	return map[string]any{"schema": 1, "name": name, "repository": repository, "policy": "source-branch",
		"state": "active", "revision": 1, "nextSequence": 1, "identity": nil, "head": nil,
		"delivery": map[string]any{"kind": "source-branch", "branch": branch}}
}

func scUnique(prefix string) string {
	return fmt.Sprintf("%s%012x", prefix, time.Now().UnixNano()&0xffffffffffff)
}

func scCheck(t *testing.T, opts SourceCheckOptions) *pyjson.Object {
	t.Helper()
	status, err := CheckForUpdates(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	return status
}

func scWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func scTree(t *testing.T, dir string) map[string]string {
	t.Helper()
	files := map[string]string{}
	filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			body, _ := os.ReadFile(path)
			files[path] = string(body)
		}
		return nil
	})
	return files
}

func TestSourceCheckTargetWorktreeOwnsAdmissionAndForkComparison(t *testing.T) {
	f := scFixture(t)
	scWrite(t, filepath.Join(f.root, "install-stamp.json"), `{"updateMechanism": "external"}`)
	scWrite(t, filepath.Join(f.linked, "install-stamp.json"), `{"updateMechanism": "self"}`)
	cache := filepath.Join(f.home, "shared-cache.json")
	target := strings.Repeat("a", 40)
	f.respond("/repos/fixture/fork/commits/feature%2Fgui", 200, target)
	f.respond("/repos/fixture/fork/compare/"+f.head+"..."+target, 200, map[string]any{"ahead_by": 3, "commits": []any{}})
	var mu sync.Mutex
	var commands [][]string
	saved, savedReleases := sourceGitRun, runSourceGit
	record := func(run func(context.Context, []string, string, time.Duration) (*sourceGitResult, error)) func(context.Context, []string, string, time.Duration) (*sourceGitResult, error) {
		return func(ctx context.Context, argv []string, cwd string, timeout time.Duration) (*sourceGitResult, error) {
			mu.Lock()
			commands = append(commands, append([]string(nil), argv...))
			mu.Unlock()
			return run(ctx, argv, cwd, timeout)
		}
	}
	sourceGitRun, runSourceGit = record(saved), record(savedReleases)
	t.Cleanup(func() { sourceGitRun, runSourceGit = saved, savedReleases })
	before := scTree(t, filepath.Join(f.root, ".git"))
	status := scCheck(t, SourceCheckOptions{InstallRoot: f.linked, Home: f.home, CachePath: cache})
	if scGet(t, status, "supported") != true || scGet(t, status, "branch") != "feature/gui" || scGet(t, status, "icebathRoot") != f.linked {
		t.Fatalf("linked status %v", status)
	}
	if behind, _ := pyIntValue(scGet(t, status, "behind")); behind != 3 {
		t.Fatalf("behind %v", status)
	}
	if got := scGet(t, scCheck(t, SourceCheckOptions{InstallRoot: f.root, Home: f.home, CachePath: cache}), "supported"); got != false {
		t.Fatalf("external stamp admitted %v", got)
	}
	if asked := f.asked(); len(asked) != 2 || asked[0] != "/repos/fixture/fork/commits/feature%2Fgui" || asked[1] != "/repos/fixture/fork/compare/"+f.head+"..."+target {
		t.Fatalf("asked %v", asked)
	}
	mu.Lock()
	for _, cmd := range commands {
		for _, arg := range cmd {
			if arg == "fetch" || arg == "checkout" || arg == "reset" || arg == "update-ref" || arg == "stash" {
				t.Fatalf("the check ran a mutating git command %v", cmd)
			}
		}
	}
	mu.Unlock()
	if got := f.git(t, f.linked, "rev-parse", "HEAD"); got != f.head {
		t.Fatalf("linked head moved to %s", got)
	}
	after := scTree(t, filepath.Join(f.root, ".git"))
	if len(after) != len(before) {
		t.Fatalf("git dir changed from %d to %d files", len(before), len(after))
	}
	for path, body := range before {
		if after[path] != body {
			t.Fatalf("the check rewrote %s", path)
		}
	}
}

func TestSourceCheckBareGitOffPathReadsThroughTheUpdatersGit(t *testing.T) {
	f := scFixture(t)
	f.respond("/repos/fixture/fork/commits/main", 200, f.head)
	storeGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(t.TempDir(), "no-git-on-path")
	os.MkdirAll(empty, 0o755)
	t.Setenv("PATH", empty)
	saved := checkGitExecutable
	checkGitExecutable = func(context.Context) string { return storeGit }
	t.Cleanup(func() { checkGitExecutable = saved })
	status := scCheck(t, SourceCheckOptions{InstallRoot: f.root, Home: f.home, Git: "git", Force: true})
	if scGet(t, status, "error") != nil || scGet(t, status, "currentSha") != f.head {
		t.Fatalf("status %v", status)
	}
	if behind, ok := pyIntValue(scGet(t, status, "behind")); !ok || behind != 0 {
		t.Fatalf("behind %v", status)
	}
}

func TestSourceCheckDynamicSourceChannelPreservesBranchPrecedence(t *testing.T) {
	for _, selection := range []string{"explicit", "configured", "current", "detached"} {
		f := scFixture(t)
		name := scUnique("branch-")
		channelPath := "/releases/channels/" + name + ".json"
		f.respond(channelPath, 200, scSourceChannel(name, "fixture/fork", "channel-default"))
		branchFile := filepath.Join(f.home, "desktop-update.json")
		if selection == "explicit" || selection == "configured" {
			scWrite(t, branchFile, `{"branch": "desktop-choice"}`)
		}
		if selection == "detached" {
			f.git(t, f.linked, "checkout", "--detach")
		}
		expected := map[string]string{"explicit": "explicit-choice", "configured": "desktop-choice", "current": "feature/gui", "detached": "channel-default"}[selection]
		branchPath := "/repos/fixture/fork/commits/" + strings.ReplaceAll(expected, "/", "%2F")
		f.respond(branchPath, 200, f.head)
		opts := SourceCheckOptions{InstallRoot: f.linked, Home: f.home, Channel: &name, BranchConfigPath: branchFile}
		if selection == "explicit" {
			explicit := "explicit-choice"
			opts.Branch = &explicit
		}
		status := scCheck(t, opts)
		if scGet(t, status, "branch") != expected || scGet(t, status, "targetSha") != f.head {
			t.Fatalf("%s status %v", selection, status)
		}
		if behind, ok := pyIntValue(scGet(t, status, "behind")); !ok || behind != 0 {
			t.Fatalf("%s behind %v", selection, status)
		}
		want := []string{channelPath, branchPath}
		if selection == "explicit" {
			want = want[1:]
		}
		if asked := f.asked(); strings.Join(asked, " ") != strings.Join(want, " ") {
			t.Fatalf("%s asked %v want %v", selection, asked, want)
		}
	}
}

func TestSourceCheckChannelFailureNeverProbesOrHealsABranch(t *testing.T) {
	for _, c := range []struct{ name, failure string }{{"canary", "missing"}, {"", "missing"}, {"", "malformed"}, {"", "foreign"}, {"", "unpublished"}} {
		f := scFixture(t)
		name := c.name
		if name == "" {
			name = scUnique("preview-")
		}
		channelPath := "/releases/channels/" + name + ".json"
		body := scSourceChannel(name, "fixture/fork", "main")
		var served any = body
		code := 200
		switch c.failure {
		case "missing":
			code = 404
		case "malformed":
			served = "not JSON"
		case "foreign":
			body["repository"] = OfficialRepository
		default:
			body["policy"] = "preview"
			delete(body, "delivery")
			body["identity"] = map[string]any{"token": strings.Repeat("a", 16), "displayName": "Fixture", "appNamePascal": "Fixture",
				"artifactNamePascal": "Fixture", "appId": "ai.fixture.preview", "msixAppIdWithOrg": "Fixture.Preview", "cliName": "fixture-preview",
				"windowsExecutableName": "Fixture"}
		}
		f.respond(channelPath, code, served)
		f.respond("/repos/fixture/fork/commits/main", 200, f.head)
		branchFile := filepath.Join(f.home, "desktop-update.json")
		scWrite(t, branchFile, `{"branch": "deleted"}`)
		status := scCheck(t, SourceCheckOptions{InstallRoot: f.linked, Home: f.home, Channel: &name, BranchConfigPath: branchFile})
		if _, has := status.Get("targetSha"); scGet(t, status, "error") != "release-unavailable" || has || scGet(t, status, "behind") != nil {
			t.Fatalf("%s %s status %v", c.name, c.failure, status)
		}
		if asked := f.asked(); len(asked) != 1 || asked[0] != channelPath {
			t.Fatalf("%s %s asked %v", c.name, c.failure, asked)
		}
		if raw, _ := os.ReadFile(branchFile); string(raw) != `{"branch": "deleted"}` {
			t.Fatalf("%s %s healed the branch file %s", c.name, c.failure, raw)
		}
	}
}

func TestSourceCheckRunningRevisionIsNotAppliedToAnExplicitTarget(t *testing.T) {
	f := scFixture(t)
	t.Setenv("ICEBATH_REVISION", strings.Repeat("e", 40))
	t.Setenv("ICEBATH_INSTALL_ROOT", f.root)
	f.respond("/repos/fixture/fork/commits/feature%2Fgui", 200, f.head)
	if got := scGet(t, scCheck(t, SourceCheckOptions{InstallRoot: f.linked, Home: f.home}), "currentSha"); got != f.head {
		t.Fatalf("explicit target read the running revision %v", got)
	}
	saved := checkProjectRoot
	checkProjectRoot = func() string { return f.home }
	t.Cleanup(func() { checkProjectRoot = saved })
	f.respond("/releases/channels/main.json", 200, scSourceChannel("main", OfficialRepository, "main"))
	f.respond("/repos/"+OfficialRepository+"/commits/main", 200, strings.Repeat("e", 40))
	status := scCheck(t, SourceCheckOptions{Home: f.home})
	if behind, ok := pyIntValue(scGet(t, status, "behind")); !ok || behind != 0 {
		t.Fatalf("default invocation %v", status)
	}
}

func TestSourceCheckDeletedDesktopBranchIsPersistedOnlyAfterDefinitiveProbe(t *testing.T) {
	f := scFixture(t)
	branchFile := filepath.Join(f.home, "desktop-update.json")
	scWrite(t, branchFile, "\xef\xbb\xbf"+`{"branch": "deleted", "other": "café"}`)
	f.git(t, f.root, "remote", "set-url", "origin", filepath.Join(f.home, "unreachable"))
	if got := scGet(t, scCheck(t, SourceCheckOptions{InstallRoot: f.linked, Home: f.home, BranchConfigPath: branchFile}), "branch"); got != "deleted" {
		t.Fatalf("unreachable origin branch %v", got)
	}
	if raw, _ := os.ReadFile(branchFile); !strings.Contains(string(raw), `"branch": "deleted"`) {
		t.Fatalf("an indefinite probe healed %s", raw)
	}
	f.git(t, f.root, "remote", "set-url", "origin", f.root)
	if got := scGet(t, scCheck(t, SourceCheckOptions{InstallRoot: f.linked, Home: f.home, BranchConfigPath: branchFile}), "branch"); got != "main" {
		t.Fatalf("definitive absence branch %v", got)
	}
	raw, _ := os.ReadFile(branchFile)
	decoded, err := pyjson.Loads(string(raw))
	if err != nil || bytes.HasPrefix(raw, []byte("\xef\xbb\xbf")) {
		t.Fatalf("healed file %q %v", raw, err)
	}
	obj, _ := decoded.(*pyjson.Object)
	if obj == nil || obj.Len() != 2 || scGet(t, obj, "branch") != "main" || scGet(t, obj, "other") != "café" {
		t.Fatalf("healed file %q", raw)
	}
}

// Points the checkout at a real bare remote holding main, with no GitHub repository involved
func (f *scInstall) bareOrigin(t *testing.T) {
	remote := filepath.Join(f.home, "remote.git")
	f.git(t, f.root, "init", "--bare", "-b", "main", remote)
	f.git(t, f.root, "remote", "set-url", "origin", remote)
	f.git(t, f.root, "push", "-q", "origin", "main")
	f.git(t, f.root, "fetch", "-q", "origin")
}

func (f *scInstall) commitOn(t *testing.T, branch, message string) string {
	f.git(t, f.root, "checkout", "-q", branch)
	f.git(t, f.root, "commit", "-q", "--allow-empty", "-m", message)
	sha := f.git(t, f.root, "rev-parse", "HEAD")
	f.git(t, f.root, "checkout", "-q", "main")
	return sha
}

func TestSourceCheckNeverPushedBranchKeepsItsPin(t *testing.T) {
	for _, pinnedBy := range []string{"desktop", "current"} {
		f := scFixture(t)
		f.bareOrigin(t)
		f.git(t, f.root, "branch", "local-work")
		f.commitOn(t, "local-work", "unpushed work")
		branchFile := filepath.Join(f.home, "desktop-update.json")
		if pinnedBy == "desktop" {
			scWrite(t, branchFile, `{"branch": "local-work"}`)
		} else {
			f.git(t, f.root, "checkout", "-q", "local-work")
		}
		status := scCheck(t, SourceCheckOptions{InstallRoot: f.root, Home: f.home, BranchConfigPath: branchFile})
		message, _ := scGet(t, status, "message").(string)
		if _, has := status.Get("targetSha"); scGet(t, status, "branch") != "local-work" || scGet(t, status, "error") != "branch-local-only" ||
			scGet(t, status, "localOnly") != true || !strings.Contains(message, "never been pushed") || has {
			t.Fatalf("%s status %v", pinnedBy, status)
		}
		raw, err := os.ReadFile(branchFile)
		if pinnedBy == "desktop" && string(raw) != `{"branch": "local-work"}` || pinnedBy == "current" && !os.IsNotExist(err) {
			t.Fatalf("%s branch file %q %v", pinnedBy, raw, err)
		}
		if asked := f.asked(); len(asked) != 0 {
			t.Fatalf("%s asked %v", pinnedBy, asked)
		}
	}
}

func TestSourceCheckDeletedRemoteBranchHealsOnlyWhenItsCommitsAreInMain(t *testing.T) {
	for _, merge := range []string{"fast-forward", "rebase", "unmerged"} {
		f := scFixture(t)
		f.bareOrigin(t)
		f.git(t, f.root, "branch", "pushed")
		work := f.commitOn(t, "pushed", "published work")
		f.git(t, f.root, "push", "-q", "-u", "origin", "pushed")
		switch merge {
		case "fast-forward":
			f.git(t, f.root, "merge", "-q", "--ff-only", "pushed")
		case "rebase":
			f.git(t, f.root, "commit", "-q", "--allow-empty", "-m", "upstream moved on")
			f.git(t, f.root, "cherry-pick", "--allow-empty", work)
		}
		if merge != "unmerged" {
			f.git(t, f.root, "push", "-q", "origin", "main")
		}
		f.git(t, f.root, "push", "-q", "origin", "--delete", "pushed")
		f.git(t, f.root, "fetch", "-q", "--prune", "origin")
		if refs := f.git(t, f.root, "for-each-ref", "refs/remotes/origin/pushed"); refs != "" {
			t.Fatalf("pruned ref survived %q", refs)
		}
		branchFile := filepath.Join(f.home, "desktop-update.json")
		scWrite(t, branchFile, `{"branch": "pushed"}`)
		status := scCheck(t, SourceCheckOptions{InstallRoot: f.root, Home: f.home, BranchConfigPath: branchFile})
		raw, _ := os.ReadFile(branchFile)
		if merge == "unmerged" {
			message, _ := scGet(t, status, "message").(string)
			if scGet(t, status, "branch") != "pushed" || scGet(t, status, "error") != "branch-local-only" || !strings.Contains(message, "not in main") || !strings.Contains(string(raw), `"pushed"`) {
				t.Fatalf("unmerged status %v file %s", status, raw)
			}
			continue
		}
		if _, has := status.Get("error"); has || scGet(t, status, "branch") != "main" || scGet(t, status, "targetSha") != f.git(t, f.root, "rev-parse", "origin/main") || !strings.Contains(string(raw), `"main"`) {
			t.Fatalf("%s status %v file %s", merge, status, raw)
		}
	}
}

func TestSourceCheckInheritedGitTargetCannotRedirectAnExplicitInstall(t *testing.T) {
	f := scFixture(t)
	f.respond("/repos/fixture/fork/commits/feature%2Fgui", 200, f.head)
	t.Setenv("GIT_DIR", filepath.Join(f.root, ".git"))
	t.Setenv("GIT_WORK_TREE", f.root)
	status := scCheck(t, SourceCheckOptions{InstallRoot: f.linked, Home: f.home})
	if behind, ok := pyIntValue(scGet(t, status, "behind")); scGet(t, status, "currentBranch") != "feature/gui" || !ok || behind != 0 {
		t.Fatalf("status %v", status)
	}
}

func TestSourceCheckSourceAdmissionIsStampOwnedNotPathOwned(t *testing.T) {
	for _, mechanism := range []any{"external", "electron-updater", "app-installer", "microsoft-store", "self", nil} {
		f := scFixture(t)
		distribution := "source"
		if mechanism == "external" {
			distribution = "nix"
		}
		stamp, _ := json.Marshal(map[string]any{"updateMechanism": mechanism, "distribution": distribution})
		if mechanism != nil {
			scWrite(t, filepath.Join(f.linked, "install-stamp.json"), string(stamp))
		}
		f.respond("/repos/fixture/fork/commits/feature%2Fgui", 200, f.head)
		status := scCheck(t, SourceCheckOptions{InstallRoot: f.linked, Home: f.home})
		supported := mechanism == "self" || mechanism == nil
		if scGet(t, status, "supported") != supported {
			t.Fatalf("%v status %v", mechanism, status)
		}
		if asked := f.asked(); supported && (len(asked) != 1 || asked[0] != "/repos/fixture/fork/commits/feature%2Fgui") || !supported && len(asked) != 0 {
			t.Fatalf("%v asked %v", mechanism, asked)
		}
		empty := filepath.Join(f.home, "no-source")
		os.MkdirAll(empty, 0o755)
		scWrite(t, filepath.Join(empty, "install-stamp.json"), string(stamp))
		if got := scGet(t, scCheck(t, SourceCheckOptions{InstallRoot: empty, Home: f.home}), "reason"); got != "not-a-git-checkout" {
			t.Fatalf("%v empty tree reason %v", mechanism, got)
		}
	}
}

func TestSourceCheckMalformedOptionalChangelogAndCacheDoNotHideTheUpdate(t *testing.T) {
	f := scFixture(t)
	cache := filepath.Join(f.home, "cache.json")
	target := strings.Repeat("a", 40)
	compare := "/repos/fixture/fork/compare/" + f.head + "..." + target
	f.respond("/repos/fixture/fork/commits/main", 200, target)
	f.respond(compare, 200, map[string]any{"ahead_by": 2, "commits": []any{map[string]any{"sha": strings.Repeat("b", 40), "commit": 42}}})
	status := scCheck(t, SourceCheckOptions{InstallRoot: f.root, Home: f.home, CachePath: cache})
	if behind, _ := pyIntValue(scGet(t, status, "behind")); behind != 2 || scGet(t, status, "updateAvailable") != true {
		t.Fatalf("status %v", status)
	}
	raw, _ := os.ReadFile(cache)
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatal(err)
	}
	data["status"] = nil
	rewritten, _ := json.Marshal(data)
	scWrite(t, cache, string(rewritten))
	if behind, _ := pyIntValue(scGet(t, scCheck(t, SourceCheckOptions{InstallRoot: f.root, Home: f.home, CachePath: cache}), "behind")); behind != 2 {
		t.Fatal("a malformed cache entry hid the update")
	}
	if asked := f.asked(); strings.Join(asked, " ") != strings.Join([]string{"/repos/fixture/fork/commits/main", compare, "/repos/fixture/fork/commits/main", compare}, " ") {
		t.Fatalf("asked %v", asked)
	}
}

func TestSourceCheckGithubCallsCarryTheConfiguredTokenAndRetryAnonymouslyOn401(t *testing.T) {
	f := scFixture(t)
	url := "/repos/fixture/fork/commits/main"
	f.respond(url, 200, f.head)
	t.Setenv("GITHUB_TOKEN", "  ghp_fixture  ")
	if behind, ok := pyIntValue(scGet(t, scCheck(t, SourceCheckOptions{InstallRoot: f.root, Home: f.home}), "behind")); !ok || behind != 0 {
		t.Fatal("token check is not current")
	}
	if seen := f.authorizations(url); len(seen) != 1 || seen[0] != "Bearer ghp_fixture" {
		t.Fatalf("authorizations %q", seen)
	}
	f.mu.Lock()
	f.decide = map[string]func(*http.Request) [2]any{url: func(r *http.Request) [2]any {
		if r.Header.Get("Authorization") != "" {
			return [2]any{401, map[string]any{}}
		}
		return [2]any{200, f.head}
	}}
	f.mu.Unlock()
	if behind, ok := pyIntValue(scGet(t, scCheck(t, SourceCheckOptions{InstallRoot: f.root, Home: f.home, Force: true}), "behind")); !ok || behind != 0 {
		t.Fatal("anonymous retry is not current")
	}
	if seen := f.authorizations(url); len(seen) < 2 || seen[len(seen)-2] != "Bearer ghp_fixture" || seen[len(seen)-1] != "" {
		t.Fatalf("authorizations %q", seen)
	}
}

func TestSourceCheckBranchTipFailureNamesTheCause(t *testing.T) {
	f := scFixture(t)
	url := "/repos/fixture/fork/commits/main"
	f.respond(url, 403, map[string]any{})
	f.mu.Lock()
	f.headers = map[string]map[string]string{url: {"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": "4102444800"}}
	f.mu.Unlock()
	status := scCheck(t, SourceCheckOptions{InstallRoot: f.root, Home: f.home})
	message, _ := scGet(t, status, "message").(string)
	if scGet(t, status, "error") != "fetch-failed" || !strings.Contains(message, "rate limit") || !strings.Contains(message, "GITHUB_TOKEN") {
		t.Fatalf("rate limit status %v", status)
	}
	if seen := f.authorizations(url); len(seen) != 1 || seen[0] != "" {
		t.Fatalf("authorizations %q", seen)
	}
	f.mu.Lock()
	f.headers = nil
	f.mu.Unlock()
	f.respond(url, 503, map[string]any{})
	status = scCheck(t, SourceCheckOptions{InstallRoot: f.root, Home: f.home, Force: true})
	message, _ = scGet(t, status, "message").(string)
	if scGet(t, status, "error") != "fetch-failed" || !strings.Contains(message, "HTTP 503") {
		t.Fatalf("503 status %v", status)
	}
}

func TestSourceCheckStableReleaseTheCheckoutAlreadyContainsIsARollback(t *testing.T) {
	for _, c := range []struct {
		on, release string
		rollback    bool
	}{{"head", "base", true}, {"base", "head", false}} {
		f := scFixture(t)
		shas := map[string]string{"base": f.base, "head": f.head}
		tmp := t.TempDir()
		bare := filepath.Join(tmp, "origin.git")
		f.git(t, tmp, "clone", "-q", "--bare", f.root, bare)
		f.git(t, bare, "tag", "v1.2.3", shas[c.release])
		f.git(t, f.root, "remote", "set-url", "origin", bare)
		f.git(t, f.root, "checkout", "-q", "--detach", shas[c.on])
		api := "/repos/" + OfficialRepository
		f.respond(api+"/releases/latest", 200, map[string]any{"draft": false, "prerelease": false, "tag_name": "v1.2.3"})
		f.respond(api+"/commits/v1.2.3", 200, map[string]any{"sha": shas[c.release]})
		stable := "stable"
		status := scCheck(t, SourceCheckOptions{InstallRoot: f.root, Home: f.home, Channel: &stable, Force: true})
		if scGet(t, status, "targetSha") != shas[c.release] || scGet(t, status, "updateAvailable") != true || scGet(t, status, "rollback") != c.rollback {
			t.Fatalf("on %s release %s status %v", c.on, c.release, status)
		}
	}
}

func TestSourceCheckEmbeddedRevisionKeepsHTTPSRefAdvertisementRecovery(t *testing.T) {
	f := scFixture(t)
	t.Setenv("ICEBATH_REVISION", f.head)
	savedRoot := checkProjectRoot
	checkProjectRoot = func() string { return f.home }
	f.respond("/releases/channels/main.json", 200, scSourceChannel("main", OfficialRepository, "main"))
	var mu sync.Mutex
	var probes [][]string
	saved, savedReleases := sourceGitRun, runSourceGit
	advertise := func(run func(context.Context, []string, string, time.Duration) (*sourceGitResult, error)) func(context.Context, []string, string, time.Duration) (*sourceGitResult, error) {
		return func(ctx context.Context, argv []string, cwd string, timeout time.Duration) (*sourceGitResult, error) {
			for _, arg := range argv {
				if arg == "ls-remote" {
					mu.Lock()
					probes = append(probes, append([]string(nil), argv...))
					mu.Unlock()
					return &sourceGitResult{ReturnCode: 0, Stdout: f.head + "\trefs/heads/main\n"}, nil
				}
			}
			return run(ctx, argv, cwd, timeout)
		}
	}
	sourceGitRun, runSourceGit = advertise(saved), advertise(savedReleases)
	t.Cleanup(func() { sourceGitRun, runSourceGit, checkProjectRoot = saved, savedReleases, savedRoot })
	if behind, ok := pyIntValue(scGet(t, scCheck(t, SourceCheckOptions{Home: f.home}), "behind")); !ok || behind != 0 {
		t.Fatal("the advertised tip did not recover the embedded revision check")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(probes) != 1 || !strings.Contains(strings.Join(probes[0], " "), "https://github.com/"+OfficialRepository+".git") {
		t.Fatalf("probes %v", probes)
	}
	prompt := false
	for _, kv := range sourceGitEnv() {
		prompt = prompt || kv == "GIT_TERMINAL_PROMPT=0"
	}
	if !prompt {
		t.Fatal("the advertisement probe may prompt for credentials")
	}
}

func TestSourceCheckOfficialSSHHealingUsesPublicHTTPSWithoutRetargetingForks(t *testing.T) {
	for _, c := range []struct {
		repository string
		heals      bool
	}{{OfficialRepository, true}, {"fixture/fork", false}} {
		f := scFixture(t)
		f.git(t, f.root, "remote", "set-url", "origin", "git@github.com:"+c.repository+".git")
		f.git(t, f.root, "config", "url."+(&url.URL{Scheme: "file", Path: filepath.ToSlash(f.root)}).String()+".insteadOf", "https://github.com/"+OfficialRepository+".git")
		if _, err := SetInstallChannel(context.Background(), "main", f.linked); err != nil {
			t.Fatal(err)
		}
		t.Setenv("GIT_SSH_COMMAND", "false")
		branchFile := filepath.Join(f.home, "desktop-update.json")
		scWrite(t, branchFile, `{"branch": "deleted"}`)
		f.respond("/repos/"+c.repository+"/commits/main", 200, f.head)
		status := scCheck(t, SourceCheckOptions{InstallRoot: f.linked, Home: f.home, BranchConfigPath: branchFile})
		want := "deleted"
		if c.heals {
			want = "main"
		}
		raw, _ := os.ReadFile(branchFile)
		if scGet(t, status, "branch") != want || !strings.Contains(string(raw), `"`+want+`"`) {
			t.Fatalf("%s status %v file %s", c.repository, status, raw)
		}
		if behind, ok := pyIntValue(scGet(t, status, "behind")); c.heals && (!ok || behind != 0) || !c.heals && scGet(t, status, "error") != "fetch-failed" {
			t.Fatalf("%s status %v", c.repository, status)
		}
	}
}

func TestSourceCheckStableReleaseTheUpdaterRefusesIsHeldNotOffered(t *testing.T) {
	for _, c := range []struct{ ledgered, divergent bool }{{true, true}, {false, true}, {true, false}, {false, false}} {
		f := scFixture(t)
		sibling := filepath.Join(f.home, "profiles", "sibling")
		schema := filepath.Join(f.root, dgSchemaPath)
		os.MkdirAll(filepath.Dir(schema), 0o755)
		f.git(t, f.root, "checkout", "-q", "-b", "release-line", f.base)
		scWrite(t, schema, "const schemaSQL = `CREATE TABLE IF NOT EXISTS sessions (id TEXT)`\n")
		f.git(t, f.root, "add", dgSchemaPath)
		f.git(t, f.root, "commit", "-q", "-m", "release from the older line")
		release := f.git(t, f.root, "rev-parse", "HEAD")
		f.git(t, f.root, "checkout", "-q", "main")
		if !c.divergent {
			f.git(t, f.root, "merge", "--no-ff", "-qm", "release on main's history", "release-line")
		}
		os.MkdirAll(filepath.Dir(schema), 0o755)
		scWrite(t, schema, "const schemaSQL = `CREATE TABLE IF NOT EXISTS session_admissions (seq INTEGER)`\n")
		f.git(t, f.root, "add", dgSchemaPath)
		f.git(t, f.root, "commit", "-q", "-m", "gateway runtime")
		tmp := t.TempDir()
		bare := filepath.Join(tmp, "origin.git")
		f.git(t, tmp, "clone", "-q", "--bare", f.root, bare)
		f.git(t, bare, "tag", "v1.2.3", release)
		f.git(t, f.root, "remote", "set-url", "origin", bare)
		f.git(t, f.root, "checkout", "-q", "main")
		if c.ledgered {
			dgLedgered(t, sibling)
		}
		api := "/repos/" + OfficialRepository
		f.respond(api+"/releases/latest", 200, map[string]any{"draft": false, "prerelease": false, "tag_name": "v1.2.3"})
		f.respond(api+"/commits/v1.2.3", 200, map[string]any{"sha": release})
		stable := "stable"
		status := scCheck(t, SourceCheckOptions{InstallRoot: f.root, Home: f.home, Channel: &stable, Force: true})
		wantHeld := any(nil)
		if c.ledgered {
			wantHeld = "downgrade"
		}
		if scGet(t, status, "targetSha") != release || scGet(t, status, "updateAvailable") != !c.ledgered || scGet(t, status, "held") != wantHeld {
			t.Fatalf("%+v status %v", c, status)
		}
		held := status
		if !c.ledgered {
			dgLedgered(t, sibling)
			held = scCheck(t, SourceCheckOptions{InstallRoot: f.root, Home: f.home, Channel: &stable})
			if scGet(t, held, "updateAvailable") != false || scGet(t, held, "held") != "downgrade" {
				t.Fatalf("%+v held %v", c, held)
			}
		}
		if scGet(t, held, "rollback") != false {
			t.Fatalf("%+v a held release promised a rollback %v", c, held)
		}
		os.RemoveAll(sibling)
		seen := f.asked()
		allowed := scCheck(t, SourceCheckOptions{InstallRoot: f.root, Home: f.home, Channel: &stable})
		behind, _ := pyIntValue(scGet(t, allowed, "behind"))
		if _, has := allowed.Get("held"); scGet(t, allowed, "updateAvailable") != true || behind != -1 || scGet(t, allowed, "rollback") != !c.divergent || has ||
			!pyjson.Equal(scGet(t, allowed, "fetchedAt"), scGet(t, status, "fetchedAt")) {
			t.Fatalf("%+v allowed %v", c, allowed)
		}
		if asked := f.asked(); len(asked) != len(seen) {
			t.Fatalf("%+v the cached verdict asked again %v", c, asked[len(seen):])
		}
	}
}

func TestSourceCheckAnUnfetchedReleaseIsJudgedFromGithub(t *testing.T) {
	for _, ledgered := range []bool{true, false} {
		f := scFixture(t)
		tmp := t.TempDir()
		bare, cut := filepath.Join(tmp, "origin.git"), filepath.Join(tmp, "cut")
		f.git(t, tmp, "clone", "-q", "--bare", f.root, bare)
		f.git(t, tmp, "clone", "-q", "--no-local", bare, cut)
		f.git(t, cut, "checkout", "-q", "-b", "release-line", f.base)
		f.git(t, cut, "commit", "-q", "--allow-empty", "-m", "release from the older line")
		release := f.git(t, cut, "rev-parse", "HEAD")
		f.git(t, cut, "tag", "v1.2.3", release)
		f.git(t, cut, "push", "-q", "origin", "v1.2.3")
		f.git(t, f.root, "remote", "set-url", "origin", bare)
		probe := exec.Command("git", "cat-file", "-e", release+"^{commit}")
		probe.Dir = f.root
		if probe.Run() == nil {
			t.Fatal("the release commit is already in the checkout")
		}
		api := "/repos/" + OfficialRepository
		f.respond(api+"/releases/latest", 200, map[string]any{"draft": false, "prerelease": false, "tag_name": "v1.2.3"})
		f.respond(api+"/commits/v1.2.3", 200, map[string]any{"sha": release})
		f.respond(api+"/compare/"+f.git(t, f.root, "rev-parse", "HEAD")+"..."+release, 200, map[string]any{"status": "diverged"})
		listing := api + "/contents/pkg/state?ref=" + release
		f.respond(listing, 200, []any{map[string]any{"path": "pkg/state/common_schema.go", "type": "file"}, map[string]any{"path": "pkg/state/store.go", "type": "file"}})
		f.respond("/raw/"+OfficialRepository+"/"+release+"/pkg/state/common_schema.go", 200, "const schemaSQL = `CREATE TABLE IF NOT EXISTS sessions (id TEXT)`\n")
		f.respond("/raw/"+OfficialRepository+"/"+release+"/pkg/state/store.go", 200, "package state\n")
		if ledgered {
			dgLedgered(t, f.home)
		}
		stable := "stable"
		status := scCheck(t, SourceCheckOptions{InstallRoot: f.root, Home: f.home, Channel: &stable, Force: true})
		wantHeld := any(nil)
		if ledgered {
			wantHeld = "downgrade"
		}
		if scGet(t, status, "targetSha") != release || scGet(t, status, "updateAvailable") != !ledgered || scGet(t, status, "held") != wantHeld {
			t.Fatalf("ledgered %v status %v", ledgered, status)
		}
		want := 0
		if ledgered {
			want = 1
		}
		if got := f.count(listing); got != want {
			t.Fatalf("ledgered %v read the release state listing %d times", ledgered, got)
		}
		if ledgered {
			scCheck(t, SourceCheckOptions{InstallRoot: f.root, Home: f.home, Channel: &stable, Force: true})
			if got := f.count(listing); got != 1 {
				t.Fatalf("a definite verdict was looked up again %d", got)
			}
		}
	}
}
