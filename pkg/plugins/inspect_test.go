package plugins

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
)

// Git repository with an allow any sha upload pack and one commit of the files
func inspectRepo(t *testing.T, root string, files map[string]string, name string) string {
	t.Helper()
	repo := filepath.Join(root, name)
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	c6Git(t, repo, "init", "-q", "-b", "main")
	c6Git(t, repo, "config", "uploadpack.allowAnySHA1InWant", "true")
	inspectCommit(t, repo, files)
	return repo
}

func inspectCommit(t *testing.T, repo string, files map[string]string) string {
	for rel, text := range files {
		c6Write(t, filepath.Join(repo, rel), text)
	}
	return c6Commit(t, repo, "c")
}

func fileURI(path string) string {
	return (&url.URL{Scheme: "file", Path: filepath.ToSlash(path)}).String()
}

// Fresh inspection table, an isolated home and an agent package whose import would leave a sentinel
func inspectWorld(t *testing.T) (context.Context, string, map[string]string, string) {
	t.Helper()
	tmp := t.TempDir()
	h := filepath.Join(tmp, "home")
	_ = os.MkdirAll(h, 0o755)
	t.Setenv("ICEBATH_HOME", h)
	inspectionsMu.Lock()
	clear(inspections)
	inspectionsMu.Unlock()
	t.Cleanup(func() {
		inspectionsMu.Lock()
		clear(inspections)
		inspectionsMu.Unlock()
	})
	sentinel := filepath.Join(tmp, "imported.flag")
	files := map[string]string{"plugin.yaml": "name: demo-agent\nversion: 1.0.0\n", "__init__.py": "open(" + pyQuote(sentinel) + ", 'w').write('ran')\n"}
	return home.WithOverride(context.Background(), h), tmp, files, sentinel
}

func pyQuote(s string) string { b, _ := json.Marshal(s); return string(b) }

type cloneCounter struct {
	count   atomic.Int32
	started chan struct{}
	once    sync.Once
	gate    chan struct{}
}

// Counts real clones, holding each until the gate opens when one is set
func countClones(t *testing.T) *cloneCounter {
	c := &cloneCounter{started: make(chan struct{})}
	saved := inspectClone
	inspectClone = func(ctx context.Context, h, tmpClone, gitURL, revision, subdir string) (string, error) {
		c.count.Add(1)
		c.once.Do(func() { close(c.started) })
		if c.gate != nil {
			select {
			case <-c.gate:
			case <-time.After(30 * time.Second):
				t.Error("gate never opened")
			}
		}
		return saved(ctx, h, tmpClone, gitURL, revision, subdir)
	}
	t.Cleanup(func() { inspectClone = saved })
	return c
}

func mustInspect(t *testing.T, ctx context.Context, identifier, catalogName string) map[string]any {
	t.Helper()
	result, err := InspectPluginSource(ctx, identifier, catalogName)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func setInspectTTL(t *testing.T, d time.Duration) {
	saved := inspectResultTTL
	inspectResultTTL = d
	t.Cleanup(func() { inspectResultTTL = saved })
}

func TestAgentOnlyPackageIsReadWithoutRunningItsCode(t *testing.T) {
	ctx, tmp, files, sentinel := inspectWorld(t)
	repo := inspectRepo(t, tmp, files, "source")
	result := mustInspect(t, ctx, fileURI(repo), "")
	warnings := result["warnings"].([]string)
	want := map[string]any{"ok": true, "agent": true, "desktop": false, "agent_name": "demo-agent", "desktop_name": nil,
		"sha": c6Git(t, repo, "rev-parse", "HEAD"), "warnings": warnings, "insecure": true}
	if !reflect.DeepEqual(result, want) || len(warnings) != 1 || !strings.Contains(warnings[0], "insecure or local scheme") {
		t.Fatalf("%#v", result)
	}
	if _, err := os.Stat(sentinel); err == nil {
		t.Fatal("package code ran")
	}
}

func TestMixedPackageInASubdirectoryReportsBothHalves(t *testing.T) {
	ctx, tmp, files, sentinel := inspectWorld(t)
	nested := map[string]string{"pkgs/demo/desktop/plugin.js": "export default {}\n", "other/huge.txt": "x"}
	for k, v := range files {
		nested["pkgs/demo/"+k] = v
	}
	repo := inspectRepo(t, tmp, nested, "source")
	result := mustInspect(t, ctx, fileURI(repo)+"#pkgs/demo", "")
	if result["ok"] != true || result["agent"] != true || result["desktop"] != true || result["agent_name"] != "demo-agent" || result["desktop_name"] != "demo" {
		t.Fatalf("%#v", result)
	}
	if _, err := os.Stat(sentinel); err == nil {
		t.Fatal("package code ran")
	}
}

func TestDesktopOnlyAndPortablePackages(t *testing.T) {
	ctx, tmp, _, _ := inspectWorld(t)
	desktop := inspectRepo(t, tmp, map[string]string{"plugin.js": "export default {}\n"}, "ui-plugin")
	portable := inspectRepo(t, tmp, map[string]string{"plugin.json": `{"name": "portable-one"}`}, "portable")
	result := mustInspect(t, ctx, fileURI(desktop), "")
	result["warnings"] = []string{}
	want := map[string]any{"ok": true, "agent": false, "desktop": true, "agent_name": nil, "desktop_name": "ui-plugin",
		"sha": c6Git(t, desktop, "rev-parse", "HEAD"), "warnings": []string{}, "insecure": true}
	if !reflect.DeepEqual(result, want) {
		t.Fatalf("%#v", result)
	}
	if got := mustInspect(t, ctx, fileURI(portable), "")["agent_name"]; got != "portable-one" {
		t.Fatalf("%v", got)
	}
}

func TestRepositoryWithoutPluginArtifactsIsAFailedRead(t *testing.T) {
	ctx, tmp, _, _ := inspectWorld(t)
	repo := inspectRepo(t, tmp, map[string]string{"README.md": "nothing here\n"}, "source")
	result := mustInspect(t, ctx, fileURI(repo), "")
	if result["ok"] != false || result["error"] != "No agent or desktop plugin artifacts found in this repository." {
		t.Fatalf("%#v", result)
	}
}

func TestUnreachableSourceAndBadIdentifierFailWithoutRaising(t *testing.T) {
	ctx, tmp, _, _ := inspectWorld(t)
	missing := mustInspect(t, ctx, fileURI(filepath.Join(tmp, "nope")), "")
	bad := mustInspect(t, ctx, "not-a-repo", "")
	if missing["ok"] != false || missing["error"] == "" || bad["ok"] != false || bad["error"] == "" {
		t.Fatalf("%#v %#v", missing, bad)
	}
}

func TestSubdirectoryEscapeIsRefused(t *testing.T) {
	ctx, tmp, files, _ := inspectWorld(t)
	repo := inspectRepo(t, tmp, files, "source")
	if result := mustInspect(t, ctx, fileURI(repo)+"#../..", ""); result["ok"] != false {
		t.Fatalf("%#v", result)
	}
}

func TestKillListedSourceIsRefusedBeforeCloning(t *testing.T) {
	ctx, tmp, files, _ := inspectWorld(t)
	repo := inspectRepo(t, tmp, files, "source")
	var seen [][]string
	savedRemoved, savedClone := inspectRaiseIfRemoved, inspectClone
	inspectRaiseIfRemoved = func(ctx context.Context, candidates ...string) error {
		seen = append(seen, candidates)
		return &PluginOperationError{Msg: "removed from the catalog"}
	}
	inspectClone = func(context.Context, string, string, string, string, string) (string, error) {
		t.Fatal("cloned")
		return "", nil
	}
	t.Cleanup(func() { inspectRaiseIfRemoved, inspectClone = savedRemoved, savedClone })
	result := mustInspect(t, ctx, fileURI(repo), "")
	want := map[string]any{"ok": false, "agent": false, "desktop": false, "warnings": result["warnings"], "insecure": true, "error": "removed from the catalog"}
	if !reflect.DeepEqual(result, want) || len(seen) == 0 {
		t.Fatalf("%#v %v", result, seen)
	}
}

func TestCatalogEntryIsInspectedAtItsReviewedPin(t *testing.T) {
	ctx, tmp, files, _ := inspectWorld(t)
	repo := inspectRepo(t, tmp, files, "source")
	pin := c6Git(t, repo, "rev-parse", "HEAD")
	inspectCommit(t, repo, map[string]string{"desktop/plugin.js": "export default {}\n"})
	saved := inspectCatalogEntry
	inspectCatalogEntry = func(_ context.Context, name string) *PluginCatalogEntry {
		if name == "demo" {
			return &PluginCatalogEntry{Name: "demo", Repo: fileURI(repo), Sha: pin}
		}
		return nil
	}
	t.Cleanup(func() { inspectCatalogEntry = saved })
	pinned := mustInspect(t, ctx, "ignored/identifier", "demo")
	unknown := mustInspect(t, ctx, "", "missing")
	if pinned["ok"] != true || pinned["agent"] != true || pinned["desktop"] != false || pinned["sha"] != pin {
		t.Fatalf("%#v", pinned)
	}
	want := map[string]any{"ok": false, "agent": false, "desktop": false, "warnings": []string{}, "insecure": false,
		"error": "'missing' is not in the Icebath plugin catalog."}
	if !reflect.DeepEqual(unknown, want) {
		t.Fatalf("%#v", unknown)
	}
}

func TestTheReportedShaIsTheCommitReadEvenAfterTheSourceMoves(t *testing.T) {
	ctx, tmp, files, _ := inspectWorld(t)
	repo := inspectRepo(t, tmp, files, "source")
	reviewed := c6Git(t, repo, "rev-parse", "HEAD")
	if got := mustInspect(t, ctx, fileURI(repo), "")["sha"]; got != reviewed {
		t.Fatalf("%v", got)
	}
	moved := inspectCommit(t, repo, map[string]string{"desktop/plugin.js": "export default {}\n"})
	if got := mustInspect(t, ctx, fileURI(repo), "")["sha"]; got != reviewed {
		t.Fatalf("repeat %v", got)
	}
	setInspectTTL(t, -time.Second)
	if got := mustInspect(t, ctx, fileURI(repo), "")["sha"]; got != moved {
		t.Fatalf("expired %v", got)
	}
}

func TestInspectionWritesNothingIntoTheProfile(t *testing.T) {
	ctx, tmp, files, _ := inspectWorld(t)
	repo := inspectRepo(t, tmp, files, "source")
	if mustInspect(t, ctx, fileURI(repo), "")["ok"] != true {
		t.Fatal("not ok")
	}
	if entries, err := os.ReadDir(filepath.Join(home.Dir(ctx), "plugins")); err == nil && len(entries) > 0 {
		t.Fatalf("%v", entries)
	}
}

func TestRetryDuringASlowCloneJoinsItInsteadOfCloningAgain(t *testing.T) {
	ctx, tmp, files, _ := inspectWorld(t)
	repo := inspectRepo(t, tmp, files, "source")
	clones := countClones(t)
	clones.gate = make(chan struct{})
	answers := make(chan map[string]any, 2)
	go func() { answers <- mustInspectAsync(ctx, fileURI(repo)) }()
	<-clones.started
	retryDone := make(chan struct{})
	go func() { answers <- mustInspectAsync(ctx, fileURI(repo)); close(retryDone) }()
	select {
	case <-retryDone:
		t.Fatal("retry did not join the running clone")
	case <-time.After(500 * time.Millisecond):
	}
	if clones.count.Load() != 1 {
		t.Fatalf("%d", clones.count.Load())
	}
	close(clones.gate)
	a, b := <-answers, <-answers
	head := c6Git(t, repo, "rev-parse", "HEAD")
	if clones.count.Load() != 1 || a["agent_name"] != "demo-agent" || b["agent_name"] != "demo-agent" || a["sha"] != head || b["sha"] != head {
		t.Fatalf("%#v %#v", a, b)
	}
	wa, wb := a["warnings"].([]string), b["warnings"].([]string)
	if reflect.ValueOf(a).Pointer() == reflect.ValueOf(b).Pointer() || (len(wa) > 0 && &wa[0] == &wb[0]) {
		t.Fatal("joined requests share one answer")
	}
}

func mustInspectAsync(ctx context.Context, identifier string) map[string]any {
	result, err := InspectPluginSource(ctx, identifier, "")
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	return result
}

func TestARetryAfterTheCloneFinishedReadsItsAnswerUntilItExpires(t *testing.T) {
	ctx, tmp, files, _ := inspectWorld(t)
	repo := inspectRepo(t, tmp, files, "source")
	clones := countClones(t)
	for range 2 {
		if mustInspect(t, ctx, fileURI(repo), "")["ok"] != true {
			t.Fatal("not ok")
		}
	}
	if clones.count.Load() != 1 {
		t.Fatalf("%d", clones.count.Load())
	}
	setInspectTTL(t, -time.Second)
	if mustInspect(t, ctx, fileURI(repo), "")["ok"] != true || clones.count.Load() != 2 {
		t.Fatalf("%d", clones.count.Load())
	}
}

func TestAFailedReadIsNotKept(t *testing.T) {
	ctx, tmp, _, _ := inspectWorld(t)
	clones := countClones(t)
	missing := fileURI(filepath.Join(tmp, "nope"))
	for range 2 {
		if mustInspect(t, ctx, missing, "")["ok"] != false {
			t.Fatal("ok")
		}
	}
	inspectionsMu.Lock()
	kept := len(inspections)
	inspectionsMu.Unlock()
	if clones.count.Load() != 2 || kept != 0 {
		t.Fatalf("%d %d", clones.count.Load(), kept)
	}
}

func TestAnotherProfileNeverReadsThisProfilesAnswer(t *testing.T) {
	_, tmp, files, _ := inspectWorld(t)
	repo := inspectRepo(t, tmp, files, "source")
	clones := countClones(t)
	for _, name := range []string{"a", "b"} {
		h := filepath.Join(tmp, name)
		_ = os.MkdirAll(h, 0o755)
		if mustInspect(t, home.WithOverride(context.Background(), h), fileURI(repo), "")["ok"] != true {
			t.Fatal("not ok")
		}
	}
	if clones.count.Load() != 2 {
		t.Fatalf("%d", clones.count.Load())
	}
}

func TestForgetInspectionDropsOnlyAFinishedAnswer(t *testing.T) {
	ctx, tmp, files, _ := inspectWorld(t)
	repo := inspectRepo(t, tmp, files, "source")
	clones := countClones(t)
	mustInspect(t, ctx, fileURI(repo), "")
	forgetInspection(ctx, fileURI(repo), "")
	mustInspect(t, ctx, fileURI(repo), "")
	if clones.count.Load() != 2 {
		t.Fatalf("%d", clones.count.Load())
	}
}
