package tools

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
)

// Resolved fresh temp dir so tracker paths compare equal to test paths
func hintTempDir(t *testing.T) string {
	t.Helper()
	dir, err := pypath.Resolve(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// Writes content to path, creating its parent directories
func writeHintFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Project tree with hint files in backend, frontend and a deep nested path
func hintProject(t *testing.T) string {
	t.Helper()
	root := hintTempDir(t)
	writeHintFile(t, filepath.Join(root, "AGENTS.md"), "Root project instructions")
	writeHintFile(t, filepath.Join(root, "backend", "AGENTS.md"), "Backend-specific instructions:\n- Use FastAPI\n- Always add type hints")
	writeHintFile(t, filepath.Join(root, "backend", "src", "main.py"), "print('hello')")
	writeHintFile(t, filepath.Join(root, "frontend", "CLAUDE.md"), "Frontend rules:\n- Use TypeScript\n- No any types")
	writeHintFile(t, filepath.Join(root, "docs", "README.md"), "Documentation")
	writeHintFile(t, filepath.Join(root, "deep", "nested", "path", ".cursorrules"), "Cursor rules for nested path")
	return root
}

// Hint text of one tool call, empty with false when nothing was discovered
func hintCall(tr *SubdirectoryHintTracker, tool string, args map[string]any) (string, bool) {
	return tr.CheckToolCall(tool, args)
}

func TestSubdirectoryHintsDiscoversClaudeMD(t *testing.T) {
	p := hintProject(t)
	got, ok := hintCall(NewSubdirectoryHintTracker(p, true), "read_file", map[string]any{"path": filepath.Join(p, "frontend", "index.ts")})
	if !ok || !strings.Contains(got, "Frontend rules") {
		t.Fatalf("got %q %v", got, ok)
	}
}

func TestSubdirectoryHintsDisabledTrackerNeverInjects(t *testing.T) {
	p := hintProject(t)
	if got, ok := hintCall(NewSubdirectoryHintTracker(p, false), "read_file", map[string]any{"path": filepath.Join(p, "frontend", "index.ts")}); ok {
		t.Fatalf("disabled tracker injected %q", got)
	}
}

func TestSubdirectoryHintsNoDuplicateLoading(t *testing.T) {
	p := hintProject(t)
	tr := NewSubdirectoryHintTracker(p, true)
	if _, ok := hintCall(tr, "read_file", map[string]any{"path": filepath.Join(p, "frontend", "a.ts")}); !ok {
		t.Fatal("first visit loaded nothing")
	}
	if got, ok := hintCall(tr, "read_file", map[string]any{"path": filepath.Join(p, "frontend", "b.ts")}); ok {
		t.Fatalf("second visit reloaded %q", got)
	}
}

func TestSubdirectoryHintsBareDirectoryAfterNavigationCommandIsAPath(t *testing.T) {
	for _, command := range []string{"cd backend && ls", "pushd backend", "echo start; cd backend; ls", "cd backend;ls"} {
		p := hintProject(t)
		got, ok := hintCall(NewSubdirectoryHintTracker(p, true), "terminal", map[string]any{"command": command})
		if !ok || !strings.Contains(got, "Backend-specific instructions") {
			t.Fatalf("%q: got %q %v", command, got, ok)
		}
	}
}

func TestSubdirectoryHintsCdAsAnArgumentOrAQuotedOtherNameIsNotNavigation(t *testing.T) {
	for _, command := range []string{"echo cd backend", "printf '%s %s' cd backend", "cd 'backend;'"} {
		p := hintProject(t)
		if got, ok := hintCall(NewSubdirectoryHintTracker(p, true), "terminal", map[string]any{"command": command}); ok {
			t.Fatalf("%q injected %q", command, got)
		}
	}
}

func TestSubdirectoryHintsRelativePath(t *testing.T) {
	p := hintProject(t)
	got, ok := hintCall(NewSubdirectoryHintTracker(p, true), "read_file", map[string]any{"path": "frontend/index.ts"})
	if !ok || !strings.Contains(got, "Frontend rules") {
		t.Fatalf("got %q %v", got, ok)
	}
}

func TestSubdirectoryHintsWorkdirArg(t *testing.T) {
	p := hintProject(t)
	got, ok := hintCall(NewSubdirectoryHintTracker(p, true), "terminal", map[string]any{"command": "ls", "workdir": filepath.Join(p, "frontend")})
	if !ok || !strings.Contains(got, "Frontend rules") {
		t.Fatalf("got %q %v", got, ok)
	}
}

func TestSubdirectoryHintsTruncationOfLargeHints(t *testing.T) {
	tmp := hintTempDir(t)
	sub := filepath.Join(tmp, "bigdir")
	body := "HEAD-MARKER " + strings.Repeat("x", maxHintChars+5_000) + " TAIL-MARKER"
	writeHintFile(t, filepath.Join(sub, "AGENTS.md"), body)
	tr := NewSubdirectoryHintTracker(tmp, true)
	DrainTruncationWarnings(context.Background())
	logs := captureRegistryLog(t)
	got, ok := hintCall(tr, "read_file", map[string]any{"path": filepath.Join(sub, "file.py")})
	if !ok || !strings.Contains(got, "HEAD-MARKER") || !strings.Contains(got, "TAIL-MARKER") {
		t.Fatalf("head and tail not kept %v", ok)
	}
	if !strings.Contains(got, "truncated AGENTS.md") || !strings.Contains(got, filepath.Join("bigdir", "AGENTS.md")) {
		t.Fatalf("marker missing the file to read %q", got[len(got)/2-200:len(got)/2+200])
	}
	if len(got) >= len(body) {
		t.Fatalf("result %d not shorter than body %d", len(got), len(body))
	}
	text := logs.String()
	if !strings.Contains(text, "TRUNCATED") || !strings.Contains(text, "AGENTS.md") {
		t.Fatalf("truncation not logged %q", text)
	}
	if queued := DrainTruncationWarnings(context.Background()); len(queued) != 0 {
		t.Fatalf("status warning queued %q", queued)
	}
	if strings.Contains(text, "context_file_max_chars") {
		t.Fatalf("log points at a knob that cannot raise the cap %q", text)
	}
}

func TestSubdirectoryHintsAreaFileUnderCeilingIsDeliveredWhole(t *testing.T) {
	tmp := hintTempDir(t)
	sub := filepath.Join(tmp, "gateway")
	body := "# Gateway rules\n" + strings.Repeat("- rule\n", 1500)
	writeHintFile(t, filepath.Join(sub, "AGENTS.md"), body)
	got, ok := hintCall(NewSubdirectoryHintTracker(tmp, true), "read_file", map[string]any{"path": filepath.Join(sub, "run.py")})
	if !ok || strings.Contains(strings.ToLower(got), "truncated") || !strings.HasSuffix(got, strings.TrimSpace(body)) {
		t.Fatalf("area file not delivered whole %v", ok)
	}
}

func TestSubdirectoryHintsEmptyArgs(t *testing.T) {
	p := hintProject(t)
	tr := NewSubdirectoryHintTracker(p, true)
	if _, ok := hintCall(tr, "read_file", map[string]any{}); ok {
		t.Fatal("empty args injected")
	}
	if _, ok := hintCall(tr, "terminal", map[string]any{"command": ""}); ok {
		t.Fatal("empty command injected")
	}
}

func TestSubdirectoryHintsTimeoutSkipsSlowHintFiles(t *testing.T) {
	p := hintProject(t)
	backend := filepath.Join(p, "backend")
	writeHintFile(t, filepath.Join(backend, "AGENTS.md"), "Backend-specific instructions")
	h := t.TempDir()
	writeHintFile(t, filepath.Join(h, "config.yaml"), "context_file_read_timeout: 0.05\n")
	env := &Env{Home: h, Values: &sync.Map{}}
	slowDone := make(chan struct{})
	var slowOnce sync.Once
	prev := ReadContextFileText
	ReadContextFileText = func(path string) (string, error) {
		if strings.EqualFold(filepath.Base(path), "agents.md") && filepath.Dir(path) == backend {
			defer slowOnce.Do(func() { close(slowDone) })
			time.Sleep(600 * time.Millisecond)
		}
		return readTextFile(path)
	}
	t.Cleanup(func() {
		<-slowDone
		ReadContextFileText = prev
	})
	tr := NewSubdirectoryHintTrackerFor(env, p, true)
	logs := captureRegistryLog(t)
	start := time.Now()
	got, ok := hintCall(tr, "read_file", map[string]any{"path": filepath.Join(p, "backend", "src", "main.py")})
	if elapsed := time.Since(start); elapsed >= 400*time.Millisecond {
		t.Fatalf("hint load blocked for %s", elapsed)
	}
	if ok {
		t.Fatalf("slow hint injected %q", got)
	}
	if !strings.Contains(strings.ToLower(logs.String()), "timed out") {
		t.Fatalf("timeout not logged %q", logs.String())
	}
}

// Stat hook raising a permission error for the paths deny selects
func denyHintStat(t *testing.T, deny func(string) bool) {
	t.Helper()
	prev := hintPathStat
	hintPathStat = func(p string) (os.FileInfo, error) {
		if deny(p) {
			return nil, &fs.PathError{Op: "stat", Path: p, Err: syscall.EACCES}
		}
		return prev(p)
	}
	t.Cleanup(func() { hintPathStat = prev })
}

func TestSubdirectoryHintsLoadHintsPermissionErrorOnIsFile(t *testing.T) {
	tmp := hintTempDir(t)
	tr := NewSubdirectoryHintTracker(tmp, true)
	restricted := filepath.Join(tmp, "restricted")
	writeHintFile(t, filepath.Join(restricted, "AGENTS.md"), "Restricted rules")
	denyHintStat(t, func(p string) bool { return strings.Contains(p, "restricted") })
	if got, ok := tr.loadHintsForDirectory(restricted); ok {
		t.Fatalf("unreadable hint loaded %q", got)
	}
}

func TestSubdirectoryHintsCheckToolCallSurvivesInaccessiblePath(t *testing.T) {
	p := hintProject(t)
	tr := NewSubdirectoryHintTracker(p, true)
	denyHintStat(t, func(s string) bool { return strings.Contains(s, "backend") && !strings.Contains(s, "src") })
	if got, ok := hintCall(tr, "read_file", map[string]any{"path": filepath.Join(p, "backend", "src", "main.py")}); ok {
		t.Fatalf("inaccessible backend injected %q", got)
	}
}

func TestSubdirectoryHintsIsValidSubdirRejectsSiblingDir(t *testing.T) {
	p := hintProject(t)
	outside := filepath.Join(filepath.Dir(p), ".test-codex")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if NewSubdirectoryHintTracker(p, true).isValidSubdir(outside) {
		t.Fatal("sibling directory accepted")
	}
}

func TestSubdirectoryHintsSymlinkedDuplicateNotReinjected(t *testing.T) {
	tmp := hintTempDir(t)
	real, mirror := filepath.Join(tmp, "real"), filepath.Join(tmp, "mirror")
	writeHintFile(t, filepath.Join(real, "AGENTS.md"), "Shared workspace instructions")
	if err := os.MkdirAll(mirror, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(real, "AGENTS.md"), filepath.Join(mirror, "AGENTS.md")); err != nil {
		t.Fatal(err)
	}
	tr := NewSubdirectoryHintTracker(tmp, true)
	first, ok := hintCall(tr, "read_file", map[string]any{"path": filepath.Join(real, "x.py")})
	if !ok || !strings.Contains(first, "Shared workspace instructions") {
		t.Fatalf("first %q %v", first, ok)
	}
	if second, ok := hintCall(tr, "read_file", map[string]any{"path": filepath.Join(mirror, "y.py")}); ok {
		t.Fatalf("symlinked duplicate reinjected %q", second)
	}
}

func TestSubdirectoryHintsIdenticalCopyNotReinjected(t *testing.T) {
	tmp := hintTempDir(t)
	writeHintFile(t, filepath.Join(tmp, "a", "AGENTS.md"), "Same content")
	writeHintFile(t, filepath.Join(tmp, "b", "AGENTS.md"), "Same content")
	tr := NewSubdirectoryHintTracker(tmp, true)
	if _, ok := hintCall(tr, "read_file", map[string]any{"path": filepath.Join(tmp, "a", "f.py")}); !ok {
		t.Fatal("first copy not injected")
	}
	if got, ok := hintCall(tr, "read_file", map[string]any{"path": filepath.Join(tmp, "b", "f.py")}); ok {
		t.Fatalf("identical copy reinjected %q", got)
	}
}

func TestSubdirectoryHintsDifferingContentStillInjected(t *testing.T) {
	tmp := hintTempDir(t)
	writeHintFile(t, filepath.Join(tmp, "a", "AGENTS.md"), "Alpha rules")
	writeHintFile(t, filepath.Join(tmp, "b", "AGENTS.md"), "Beta rules")
	tr := NewSubdirectoryHintTracker(tmp, true)
	first, ok1 := hintCall(tr, "read_file", map[string]any{"path": filepath.Join(tmp, "a", "f.py")})
	second, ok2 := hintCall(tr, "read_file", map[string]any{"path": filepath.Join(tmp, "b", "f.py")})
	if !ok1 || !strings.Contains(first, "Alpha rules") || !ok2 || !strings.Contains(second, "Beta rules") {
		t.Fatalf("first %q second %q", first, second)
	}
}

func TestSubdirectoryHintsWorkingDirContentSeeded(t *testing.T) {
	tmp := hintTempDir(t)
	writeHintFile(t, filepath.Join(tmp, "AGENTS.md"), "Root instructions")
	writeHintFile(t, filepath.Join(tmp, "elsewhere", "AGENTS.md"), "Root instructions")
	if got, ok := hintCall(NewSubdirectoryHintTracker(tmp, true), "read_file", map[string]any{"path": filepath.Join(tmp, "elsewhere", "f.py")}); ok {
		t.Fatalf("copy of the cwd hint reinjected %q", got)
	}
}

func TestSubdirectoryHintsExcludedDirectorySkipped(t *testing.T) {
	names := make([]string, 0, len(SearchPruneDirNames))
	for name := range SearchPruneDirNames {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, excluded := range names {
		tmp := hintTempDir(t)
		target := filepath.Join(tmp, excluded, "snapshot")
		writeHintFile(t, filepath.Join(target, "AGENTS.md"), "Stale archived instructions")
		if got, ok := hintCall(NewSubdirectoryHintTracker(tmp, true), "read_file", map[string]any{"path": filepath.Join(target, "f.py")}); ok {
			t.Fatalf("%s: excluded copy injected %q", excluded, got)
		}
	}
}

func TestSubdirectoryHintsExcludedAncestorBlocksDescendant(t *testing.T) {
	tmp := hintTempDir(t)
	deep := filepath.Join(tmp, "backups", "2026", "proj")
	writeHintFile(t, filepath.Join(deep, "AGENTS.md"), "Archived")
	if got, ok := hintCall(NewSubdirectoryHintTracker(tmp, true), "read_file", map[string]any{"path": filepath.Join(deep, "f.py")}); ok {
		t.Fatalf("hint under an excluded ancestor injected %q", got)
	}
}

func TestSubdirectoryHintsWorkingDirInsideExcludedNameStillWorks(t *testing.T) {
	root := filepath.Join(hintTempDir(t), "vendor", "myproject")
	writeHintFile(t, filepath.Join(root, "pkg", "AGENTS.md"), "Package rules")
	got, ok := hintCall(NewSubdirectoryHintTracker(root, true), "read_file", map[string]any{"path": filepath.Join(root, "pkg", "f.py")})
	if !ok || !strings.Contains(got, "Package rules") {
		t.Fatalf("got %q %v", got, ok)
	}
}

func TestSubdirectoryHintsNormalDirectoryUnaffected(t *testing.T) {
	tmp := hintTempDir(t)
	writeHintFile(t, filepath.Join(tmp, "backend", "AGENTS.md"), "Backend rules")
	got, ok := hintCall(NewSubdirectoryHintTracker(tmp, true), "read_file", map[string]any{"path": filepath.Join(tmp, "backend", "f.py")})
	if !ok || !strings.Contains(got, "Backend rules") {
		t.Fatalf("got %q %v", got, ok)
	}
}

func TestSubdirectoryHintsAgentsOverrideWinsInSubdirectory(t *testing.T) {
	tmp := hintTempDir(t)
	sub := filepath.Join(tmp, "backend")
	writeHintFile(t, filepath.Join(sub, "AGENTS.md"), "Committed backend rules")
	writeHintFile(t, filepath.Join(sub, "AGENTS.override.md"), "Personal backend override")
	got, ok := hintCall(NewSubdirectoryHintTracker(tmp, true), "read_file", map[string]any{"path": filepath.Join(sub, "f.py")})
	if !ok || !strings.Contains(got, "Personal backend override") || strings.Contains(got, "Committed backend rules") {
		t.Fatalf("got %q %v", got, ok)
	}
}

func TestSubdirectoryHintsEscapingOrDeniedSymlinkIsNeverInjected(t *testing.T) {
	tmp := hintTempDir(t)
	outside, workspace := filepath.Join(tmp, "outside"), filepath.Join(tmp, "workspace")
	writeHintFile(t, filepath.Join(outside, "secret.md"), "OUTSIDE-MARKER-9f3a")
	writeHintFile(t, filepath.Join(workspace, ".env"), "API_KEY=SECRET-VALUE-7b2d")
	sub, envlink := filepath.Join(workspace, "sub"), filepath.Join(workspace, "envlink")
	for _, link := range [][2]string{
		{filepath.Join(outside, "secret.md"), filepath.Join(workspace, "AGENTS.md")},
		{filepath.Join(outside, "secret.md"), filepath.Join(sub, "AGENTS.md")},
		{filepath.Join(workspace, ".env"), filepath.Join(envlink, "AGENTS.md")},
	} {
		if err := os.MkdirAll(filepath.Dir(link[1]), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(link[0], link[1]); err != nil {
			t.Fatal(err)
		}
	}
	tr := NewSubdirectoryHintTracker(workspace, true)
	if len(tr.loadedDigests) != 0 {
		t.Fatalf("escaping cwd link seeded %v", tr.loadedDigests)
	}
	if got, ok := hintCall(tr, "read_file", map[string]any{"path": filepath.Join(sub, "f.py")}); ok {
		t.Fatalf("escaping link injected %q", got)
	}
	if got, ok := hintCall(tr, "read_file", map[string]any{"path": filepath.Join(envlink, "f.py")}); ok {
		t.Fatalf("denied link injected %q", got)
	}
	if got, ok := hintCall(NewSubdirectoryHintTracker(workspace, true), "terminal", map[string]any{"command": "cd " + sub}); ok {
		t.Fatalf("cd into escaping link injected %q", got)
	}
}

func TestSubdirectoryHintsInTreeHintFilesStillLoad(t *testing.T) {
	workspace := filepath.Join(hintTempDir(t), "workspace")
	docs, linked, plain := filepath.Join(workspace, "docs"), filepath.Join(workspace, "linked"), filepath.Join(workspace, "plain")
	writeHintFile(t, filepath.Join(docs, "AGENTS.md"), "Shared in-tree instructions")
	writeHintFile(t, filepath.Join(plain, "AGENTS.md"), "Legit subdirectory rules")
	if err := os.MkdirAll(linked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(docs, "AGENTS.md"), filepath.Join(linked, "AGENTS.md")); err != nil {
		t.Fatal(err)
	}
	tr := NewSubdirectoryHintTracker(workspace, true)
	got, ok := hintCall(tr, "read_file", map[string]any{"path": filepath.Join(linked, "f.py")})
	if !ok || !strings.Contains(got, "Shared in-tree instructions") {
		t.Fatalf("in tree link %q %v", got, ok)
	}
	got, ok = hintCall(tr, "read_file", map[string]any{"path": filepath.Join(plain, "f.py")})
	if !ok || !strings.Contains(got, "Legit subdirectory rules") {
		t.Fatalf("plain hint %q %v", got, ok)
	}
}

func TestSubdirectoryHintsDenyListFollowsTheEnvHome(t *testing.T) {
	workspace := hintTempDir(t)
	profileHome := filepath.Join(workspace, "profilehome")
	writeHintFile(t, filepath.Join(profileHome, "auth.json"), `{"token": "PROFILE-SECRET-41c"}`)
	sub := filepath.Join(workspace, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(profileHome, "auth.json"), filepath.Join(sub, "AGENTS.md")); err != nil {
		t.Fatal(err)
	}
	env := &Env{Home: profileHome, Values: &sync.Map{}}
	if got, ok := hintCall(NewSubdirectoryHintTrackerFor(env, workspace, true), "read_file", map[string]any{"path": filepath.Join(sub, "f.py")}); ok {
		t.Fatalf("profile credential store injected %q", got)
	}
}

// Fake home holding unrelated hint files across its subtree
func hintHomeTree(t *testing.T) string {
	t.Helper()
	h := hintTempDir(t)
	t.Setenv("HOME", h)
	for _, rel := range []string{"notes/AGENTS.md", ".icebath/AGENTS.md", ".icebath/skills/s1/AGENTS.md", "projects/whatever/CLAUDE.md"} {
		writeHintFile(t, filepath.Join(h, filepath.FromSlash(rel)), "HOME-SUBTREE-HINT "+rel)
	}
	return h
}

func TestSubdirectoryHintsHomeCwdNeverInjectsSubtreeHints(t *testing.T) {
	h := hintHomeTree(t)
	tr := NewSubdirectoryHintTracker(h, true)
	if got, ok := hintCall(tr, "read_file", map[string]any{"path": filepath.Join(h, ".icebath", "skills", "s1", "x.py")}); ok {
		t.Fatalf("home subtree read injected %q", got)
	}
	if got, ok := hintCall(tr, "terminal", map[string]any{"command": "cat " + filepath.Join(h, "notes", "AGENTS.md")}); ok {
		t.Fatalf("home subtree command injected %q", got)
	}
}

func TestSubdirectoryHintsHomeGuardBlocksDirectLoadPath(t *testing.T) {
	h := hintHomeTree(t)
	if got, ok := NewSubdirectoryHintTracker(h, true).loadHintsForDirectory(filepath.Join(h, "notes")); ok {
		t.Fatalf("direct home load injected %q", got)
	}
}

func TestSubdirectoryHintsSessionWorkspaceAdoptionRebindsDiscovery(t *testing.T) {
	h := hintHomeTree(t)
	project := filepath.Join(h, "projects", "app")
	writeHintFile(t, filepath.Join(project, "src", "AGENTS.md"), "App src rules")
	tr := NewSubdirectoryHintTracker(h, true)
	if got, ok := hintCall(tr, "read_file", map[string]any{"path": filepath.Join(project, "src", "f.py")}); ok {
		t.Fatalf("home pinned tracker injected %q", got)
	}
	tr.RebindWorkingDir(project)
	if tr.workingDir != project {
		t.Fatalf("working dir %q want %q", tr.workingDir, project)
	}
	got, ok := hintCall(tr, "read_file", map[string]any{"path": filepath.Join(project, "src", "f.py")})
	if !ok || !strings.Contains(got, "App src rules") {
		t.Fatalf("rebound tracker %q %v", got, ok)
	}
}

func TestSubdirectoryHintsReboundTrackerStaysInsideTheProject(t *testing.T) {
	h := hintHomeTree(t)
	project := filepath.Join(h, "projects", "app2")
	writeHintFile(t, filepath.Join(project, "AGENTS.md"), "App2 rules")
	tr := NewSubdirectoryHintTracker(h, true)
	tr.RebindWorkingDir(project)
	if got, ok := hintCall(tr, "read_file", map[string]any{"path": filepath.Join(h, ".icebath", "skills", "s1", "x.py")}); ok {
		t.Fatalf("stray home read injected %q", got)
	}
}

func TestSubdirectoryHintsRealProjectCwdKeepsDiscovery(t *testing.T) {
	h := hintHomeTree(t)
	project := filepath.Join(h, "projects", "app3")
	writeHintFile(t, filepath.Join(project, "pkg", "AGENTS.md"), "Package rules")
	got, ok := hintCall(NewSubdirectoryHintTracker(project, true), "read_file", map[string]any{"path": filepath.Join(project, "pkg", "f.py")})
	if !ok || !strings.Contains(got, "Package rules") {
		t.Fatalf("got %q %v", got, ok)
	}
}

func TestSubdirectoryHintsTildeApproximatelyInCommandDoesNotCrash(t *testing.T) {
	tr := NewSubdirectoryHintTracker(hintTempDir(t), true)
	cmd := "cat > out.md <<EOF\nSegment size signal: ~500-700 agencies in DACH region.\nCVE volume: ~45,000 disclosed in 2025.\nFounder blended rate: ~80/hr.\nEOF"
	if got, ok := hintCall(tr, "terminal", map[string]any{"command": cmd}); ok {
		t.Fatalf("approximate tilde injected %q", got)
	}
	if _, err := pypath.PathExpanduser("~500-700"); err == nil {
		t.Fatal("unknown user tilde expanded")
	}
}

func TestSubdirectoryHintsSafeWorkerReadsNoHintFiles(t *testing.T) {
	p := hintProject(t)
	withSafeWorker(t, true)
	var mu sync.Mutex
	var reads []string
	prev := ReadContextFileText
	ReadContextFileText = func(path string) (string, error) {
		mu.Lock()
		reads = append(reads, path)
		mu.Unlock()
		return readTextFile(path)
	}
	t.Cleanup(func() { ReadContextFileText = prev })
	tr := NewSubdirectoryHintTracker(p, true)
	if len(tr.loadedDigests) != 0 {
		t.Fatalf("safe worker seeded the cwd hint %v", tr.loadedDigests)
	}
	if got, ok := hintCall(tr, "read_file", map[string]any{"path": filepath.Join(p, "backend", "src", "file.py")}); ok {
		t.Fatalf("safe worker injected %q", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(reads) != 0 {
		t.Fatalf("safe worker read %v", reads)
	}
}

func TestSubdirectoryHintsNavTargets(t *testing.T) {
	for _, c := range []struct {
		cmd  string
		want []string
	}{
		{"cd -", nil},
		{"cd", nil},
		{"cd -P /srv && pushd ../x", []string{"/srv", "../x"}},
		{"cd -- ; ls", nil},
		{"(cd sub) | cd ''", []string{"sub"}},
		{"cd 'unterminated", nil},
	} {
		got := navTargets(c.cmd)
		if strings.Join(got, "\x00") != strings.Join(c.want, "\x00") {
			t.Fatalf("%q got %q want %q", c.cmd, got, c.want)
		}
	}
}
