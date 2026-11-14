package skills

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyrich"
	"github.com/nickheyer/icebath/pkg/tools"
)

func advisoryCLI(t *testing.T) (*HubCLI, *bytes.Buffer, *pyrich.Console) {
	x, _, _ := newTestCLI(t)
	sink := &bytes.Buffer{}
	return x, sink, &pyrich.Console{Out: sink, Width: 80}
}

func TestTier1AdvisoryNeverRaisesOnScannerError(t *testing.T) {
	x, sink, c := advisoryCLI(t)
	x.seams.tier1Enabled = func() bool { return true }
	x.seams.tier1Scan = func(context.Context, string, int) (Tier1Report, error) { return Tier1Report{}, errors.New("boom") }
	x.printTier1Advisory(context.Background(), t.TempDir(), c)
	if sink.Len() != 0 {
		t.Fatal(sink.String())
	}
}

func TestTier1AdvisorySilentWhenUnavailable(t *testing.T) {
	x, sink, c := advisoryCLI(t)
	x.seams.tier1Enabled = func() bool { return true }
	x.seams.tier1Scan = func(context.Context, string, int) (Tier1Report, error) { return Tier1Report{Available: false}, nil }
	x.printTier1Advisory(context.Background(), t.TempDir(), c)
	if sink.Len() != 0 {
		t.Fatal(sink.String())
	}
}

func TestTier1AdvisorySilentWhenDisabled(t *testing.T) {
	x, sink, c := advisoryCLI(t)
	x.seams.tier1Enabled = func() bool { return false }
	x.seams.tier1Scan = func(context.Context, string, int) (Tier1Report, error) {
		t.Fatal("scan ran with the advisory disabled")
		return Tier1Report{}, nil
	}
	x.printTier1Advisory(context.Background(), t.TempDir(), c)
	if sink.Len() != 0 {
		t.Fatal(sink.String())
	}
}

func TestTier1AdvisoryPrintsFindingsAndContinues(t *testing.T) {
	x, sink, c := advisoryCLI(t)
	report := mustTier1(t, tier1ReportJSON(t, []string{tier1Finding("emails", "high", "Non-placeholder email: a@b.com", 3)}))
	x.seams.tier1Enabled = func() bool { return true }
	x.seams.tier1Scan = func(context.Context, string, int) (Tier1Report, error) { return report, nil }
	x.printTier1Advisory(context.Background(), t.TempDir(), c)
	if !strings.Contains(sink.String(), "SkillEvaluator Tier 1 (advisory)") {
		t.Fatal(sink.String())
	}
}

// Hub CLI over home whose router holds only the URL source and whose URL guards admit the loopback server
func urlInstallCLI(t *testing.T, home string) (*HubCLI, *bytes.Buffer) {
	t.Setenv("ICEBATH_ALLOW_PRIVATE_URLS", "true")
	s := NewScope(home, nil)
	h := NewHub(s)
	h.io.isSafeURL = func(string) bool { return true }
	h.io.siteAccess = func(string) *tools.WebsiteBlock { return nil }
	x := NewHubCLI(s)
	x.Hub = h
	sink := &bytes.Buffer{}
	x.C, x.Stdout = &pyrich.Console{Out: sink, Width: 80}, &bytes.Buffer{}
	x.Input = func(string) (string, error) { t.Fatal("unexpected prompt"); return "", nil }
	x.LineInput = x.Input
	x.seams.recordInstall = func(context.Context, string, string, any, string, any, any, error) {}
	x.Sources = func() []SkillSource { return []SkillSource{NewUrlSource(h)} }
	return x, sink
}

func lockEntry(t *testing.T, skillsDir, name string) map[string]any {
	var lock struct {
		Installed map[string]map[string]any `json:"installed"`
	}
	if err := json.Unmarshal([]byte(mustRead(t, filepath.Join(skillsDir, ".hub", "lock.json"))), &lock); err != nil {
		t.Fatal(err)
	}
	return lock.Installed[name]
}

func TestRealTempRepoAndHomeInstallE2E(t *testing.T) {
	_, _, url := servedRepo(t)
	home := filepath.Join(t.TempDir(), "home")
	x, sink := urlInstallCLI(t, home)
	if _, err := x.DoInstall(context.Background(), url, HubInstallOptions{SkipConfirm: true}); err != nil {
		t.Fatal(err)
	}
	installed := filepath.Join(home, "skills", "demo-bundle")
	for rel, want := range map[string]string{"references/guide.md": "safe guide\n", "references/foo#bar.md": "encoded delimiter\n",
		"references/my guide.md": "encoded space\n", "assets/logo.png": "\x89PNG\r\n\x1a\n\x00\xff"} {
		if got := mustRead(t, filepath.Join(installed, filepath.FromSlash(rel))); got != want {
			t.Fatalf("%s = %q\n%s", rel, got, sink.String())
		}
	}
	for _, rel := range []string{"templates/report.md", "scripts/run.py", "examples/endpoint-inventory.md"} {
		if st, err := os.Stat(filepath.Join(installed, filepath.FromSlash(rel))); err != nil || !st.Mode().IsRegular() {
			t.Fatalf("%s missing %v", rel, err)
		}
	}
	if lexists(filepath.Join(installed, "examples", "not-installed.md")) {
		t.Fatal("unreferenced example installed")
	}
	prov, _ := lockEntry(t, filepath.Join(home, "skills"), "demo-bundle")["scan_provenance"].(map[string]any)
	if prov["source_url"] != url || prov["fresh"] != true {
		t.Fatalf("scan provenance %v", prov)
	}
}

func TestInstallWithJunctionedSkillsDir(t *testing.T) {
	_, _, url := servedRepo(t)
	tmp := t.TempDir()
	home, real := filepath.Join(tmp, "home"), filepath.Join(tmp, "real-skills")
	os.MkdirAll(home, 0o755)
	os.MkdirAll(real, 0o755)
	if err := os.Symlink(real, filepath.Join(home, "skills")); err != nil {
		t.Fatal(err)
	}
	x, sink := urlInstallCLI(t, home)
	if _, err := x.DoInstall(context.Background(), url, HubInstallOptions{SkipConfirm: true}); err != nil {
		t.Fatal(err)
	}
	installed := filepath.Join(real, "demo-bundle")
	if st, err := os.Stat(filepath.Join(installed, "SKILL.md")); err != nil || !st.Mode().IsRegular() ||
		mustRead(t, filepath.Join(installed, "references", "guide.md")) != "safe guide\n" {
		t.Fatalf("install did not land through the redirect %v\n%s", err, sink.String())
	}
	entry := lockEntry(t, filepath.Join(home, "skills"), "demo-bundle")
	if entry["install_path"] != "demo-bundle" || !strings.HasPrefix(entry["content_hash"].(string), "sha256:") {
		t.Fatalf("lock entry %v", entry)
	}
	if !strings.Contains(sink.String(), "Installed:") {
		t.Fatal(sink.String())
	}
}

func TestInstallSkipsUnreachableSupportFileE2E(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "upstream-missing")
	writeTestFile(t, filepath.Join(repo, "SKILL.md"), "---\nname: partial-bundle\ndescription: References a support file that is unreachable.\n---\n"+
		"# Partial\n\nRead [the guide](references/present.md#usage) and the appendix at\n`references/absent.md`, then run `scripts/run.py`.\n")
	writeTestFile(t, filepath.Join(repo, "references", "present.md"), "present guide\n")
	writeTestFile(t, filepath.Join(repo, "scripts", "run.py"), "print('ok')\n")
	srv := httptest.NewServer(http.FileServer(http.Dir(repo)))
	t.Cleanup(srv.Close)
	url := srv.URL + "/SKILL.md"
	home := filepath.Join(t.TempDir(), "home")
	x, sink := urlInstallCLI(t, home)
	if _, err := x.DoInstall(context.Background(), url, HubInstallOptions{SkipConfirm: true}); err != nil {
		t.Fatal(err)
	}
	installed := filepath.Join(home, "skills", "partial-bundle")
	if !lexists(filepath.Join(installed, "SKILL.md")) || mustRead(t, filepath.Join(installed, "references", "present.md")) != "present guide\n" ||
		!lexists(filepath.Join(installed, "scripts", "run.py")) || lexists(filepath.Join(installed, "references", "absent.md")) {
		t.Fatalf("installed tree wrong\n%s", sink.String())
	}
	entry := lockEntry(t, filepath.Join(home, "skills"), "partial-bundle")
	prov, _ := entry["scan_provenance"].(map[string]any)
	var files []string
	for _, f := range entry["files"].([]any) {
		files = append(files, f.(string))
	}
	if prov["source_url"] != url || !slices.Contains(files, "references/present.md") || slices.Contains(files, "references/absent.md") {
		t.Fatalf("lock entry %v", entry)
	}
}
