package selfupdate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"syscall"
	"testing"

	"github.com/nickheyer/icebath/pkg/cron"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/profiles/manage"
	"github.com/nickheyer/icebath/pkg/state"
)

// Doctor over a home whose config.yaml carries the given memory section, with optional stale memory files
func memoryDoctor(t *testing.T, memoryYAML string, staleBuiltinFiles bool) (*Doctor, *bytes.Buffer) {
	t.Helper()
	d, out := toolsDoctor(t)
	writeTestFile(t, filepath.Join(d.Home, "config.yaml"), memoryYAML, 0o644)
	if staleBuiltinFiles {
		writeTestFile(t, filepath.Join(d.Home, "memories", "MEMORY.md"), "stale memory", 0o644)
		writeTestFile(t, filepath.Join(d.Home, "memories", "USER.md"), "stale user", 0o644)
	}
	toolsSeam(t, &doctorWhich, func(cmd string) string { return "" })
	return d, out
}

// Runs the directory and memory provider checks and returns everything they printed
func memorySectionOutput(t *testing.T, d *Doctor, out *bytes.Buffer) string {
	t.Helper()
	for _, check := range []doctorCheckFunc{checkDirectoryStructure, checkMemoryProvider} {
		if _, err := check(context.Background(), d, false); err != nil {
			t.Fatal(err)
		}
	}
	return out.String()
}

func TestDoctorMemoryProviderSectionNoProviderShowsBuiltinOk(t *testing.T) {
	d, buf := memoryDoctor(t, "memory: {}\n", false)
	out := memorySectionOutput(t, d, buf)
	if !strings.Contains(out, "Built-in memory active") || strings.Contains(out, "Honcho API key") || strings.Contains(out, "Mem0") {
		t.Fatalf("output:\n%s", out)
	}
}

func TestDoctorMemoryProviderSectionCatalogProviderNotInstalledNamesInstallCommand(t *testing.T) {
	d, buf := memoryDoctor(t, "memory:\n  provider: mem0\n", false)
	out := memorySectionOutput(t, d, buf)
	if strings.Contains(out, "Built-in memory active") || !strings.Contains(out, "mem0 plugin not found") || !strings.Contains(out, "plugins install mem0") {
		t.Fatalf("output:\n%s", out)
	}
}

func TestDoctorMemoryProviderSectionStaleBuiltinFilesReportedOnlyWhenStoreEnabled(t *testing.T) {
	for _, memoryEnabled := range []bool{false, true} {
		t.Run(fmt.Sprint(memoryEnabled), func(t *testing.T) {
			d, buf := memoryDoctor(t, fmt.Sprintf("memory:\n  provider: mnemosyne\n  memory_enabled: %v\n  user_profile_enabled: false\n", memoryEnabled), true)
			out := memorySectionOutput(t, d, buf)
			if strings.Contains(out, "MEMORY.md exists") != memoryEnabled || strings.Contains(out, "USER.md exists") ||
				strings.Contains(out, "Built-in memory files disabled by config") == memoryEnabled {
				t.Fatalf("output:\n%s", out)
			}
		})
	}
}

func TestDoctorTreatsCoreSentinelAsBuiltinMemory(t *testing.T) {
	for _, sentinel := range []string{"builtin", "Default", "none"} {
		t.Run(sentinel, func(t *testing.T) {
			d, out := toolsDoctor(t)
			writeTestFile(t, filepath.Join(d.Home, "config.yaml"), "memory:\n  provider: "+sentinel+"\n", 0o644)
			f, err := checkMemoryProvider(context.Background(), d, false)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), "Built-in memory active") || strings.Contains(out.String(), "plugin not found") || len(f.Issues) != 0 {
				t.Fatalf("issues = %q, output:\n%s", f.Issues, out.String())
			}
		})
	}
}

func TestGitHubTokenCheckGhAuthenticatedOnGhWithoutAuthenticatedJsonField(t *testing.T) {
	toolsSeam(t, &doctorWhich, func(cmd string) string {
		if cmd == "gh" {
			return "/usr/bin/gh"
		}
		return ""
	})
	toolsSeam(t, &doctorSubprocessRun, func(_ context.Context, cmd []string, _ pysubprocess.Options) (*pysubprocess.Completed, error) {
		if len(cmd) < 3 || !reflect.DeepEqual(cmd[:3], []string{"gh", "auth", "status"}) {
			t.Fatalf("argv = %q", cmd)
		}
		if containsString(cmd, "--json") && containsString(cmd, "authenticated") {
			return &pysubprocess.Completed{Args: cmd, Returncode: 1, Stderr: []byte("unknown JSON field")}, nil
		}
		return &pysubprocess.Completed{Args: cmd, Returncode: 0, Stderr: []byte("Logged in to github.com")}, nil
	})
	if !doctorGhAuthenticated(context.Background()) {
		t.Fatal("a logged in gh reads as not authenticated")
	}
}

func TestCronStoreCheckReportsWritabilityAndLowSpace(t *testing.T) {
	for _, tc := range []struct {
		name       string
		probeError error
		free       uint64
		expected   string
		issues     []string
	}{
		{"writable", nil, 50 << 30, "store is writable", nil},
		{"enospc", syscall.ENOSPC, 50 << 30, "NOT writable (ENOSPC", []string{"is not writable"}},
		{"low-space", nil, 10 << 20, "free — cron jobs stop when it fills", []string{"Free disk space"}},
		{"root-reserved-blocks", nil, (1 << 20) * 4096, "store is writable", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, out := toolsDoctor(t)
			if err := os.Mkdir(filepath.Join(d.Home, "cron"), 0o755); err != nil {
				t.Fatal(err)
			}
			previous := cron.SwapStoreProbe(func(string) error { return tc.probeError })
			t.Cleanup(func() { cron.SwapStoreProbe(previous) })
			toolsSeam(t, &doctorCronFreeBytes, func(string) (uint64, bool) { return tc.free, true })
			f, err := checkCronStore(context.Background(), d, false)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), tc.expected) {
				t.Fatalf("output lacks %q:\n%s", tc.expected, out.String())
			}
			if len(f.Issues) != len(tc.issues) {
				t.Fatalf("issues = %q", f.Issues)
			}
			for _, want := range tc.issues {
				found := false
				for _, issue := range f.Issues {
					found = found || strings.Contains(issue, want)
				}
				if !found {
					t.Fatalf("issues %q lack %q", f.Issues, want)
				}
			}
		})
	}
}

func TestProfileAliasStatusFollowsTheWrapperTheProfileReallyHas(t *testing.T) {
	tmp := t.TempDir()
	t.Cleanup(home.PatchUserHome(tmp))
	t.Setenv("HOME", tmp)
	icebathHome := filepath.Join(tmp, ".icebath")
	t.Setenv(home.EnvHome, icebathHome)
	profiles.ResetFileCache()
	profiles.ResetStandaloneMemo()
	home.ResetKeyCache()
	savedHooks := profiles.GatewayStatusHooks
	t.Cleanup(func() { profiles.GatewayStatusHooks = savedHooks })
	profiles.GatewayStatusHooks.ProfileRunning = func(string) bool { return false }
	profiles.GatewayStatusHooks.ServesNamedProfile = func(string) bool { return false }
	toolsSeam(t, &doctorDuplicateCredentialFindings, func(context.Context) ([]string, error) { return nil, nil })
	for _, name := range []string{"dev", "ops", "work", "old"} {
		writeTestFile(t, filepath.Join(icebathHome, "profiles", name, "config.yaml"), "model: m\n", 0o644)
	}
	if _, err := profiles.CreateWrapperScript("dev", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := profiles.CreateWrapperScript("w", "work"); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(tmp, ".local", "bin")
	writeTestFile(t, filepath.Join(bin, "old"), "#!/bin/sh\nexec icebath -p old \"$@\"\n", 0o755)
	for _, foreign := range []string{"ops.bat", "old.bat"} {
		writeTestFile(t, filepath.Join(bin, foreign), "@echo off\r\nnotepad %*\r\n", 0o644)
	}
	var out bytes.Buffer
	d := &Doctor{W: &out, Home: icebathHome, ProjectRoot: t.TempDir(), DisplayHome: icebathHome}
	if _, err := checkProfiles(context.Background(), d, false); err != nil {
		t.Fatal(err)
	}
	status := map[string]string{}
	for _, name := range []string{"dev", "ops", "work", "old"} {
		status[name] = toolsLineWith(t, out.String(), " "+name+":")
	}
	for _, name := range []string{"dev", "work", "old"} {
		if strings.Contains(status[name], "no alias") {
			t.Fatalf("%s line = %q", name, status[name])
		}
	}
	if !strings.Contains(status["ops"], "no alias") {
		t.Fatalf("ops line = %q", status["ops"])
	}
	listed, err := manage.ListProfiles(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range listed {
		if p.Name == "old" && p.AliasPath != filepath.Join(bin, "old") {
			t.Fatalf("old alias path = %q", p.AliasPath)
		}
	}
}

// Runs git in a directory and fails the test unless it exits zero
func checkoutGit(t *testing.T, cwd string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", cwd}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_SYSTEM="+os.DevNull)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %q failed: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// Sealed desktop bundle as the running tree with a committed legacy checkout under the home
func embeddedContext(t *testing.T) (*Doctor, *bytes.Buffer, string) {
	t.Helper()
	root := t.TempDir()
	bundle := filepath.Join(root, "bundle", "repo")
	stamp, _ := json.Marshal(map[string]any{"commit": strings.Repeat("a", 40), "distribution": "desktop-app", "updateMechanism": "electron-updater"})
	writeTestFile(t, filepath.Join(bundle, "install-stamp.json"), string(stamp), 0o644)
	icebathHome := filepath.Join(root, ".icebath")
	checkout := filepath.Join(icebathHome, "icebath-agent")
	if err := os.MkdirAll(checkout, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	checkoutGit(t, checkout, "init", "-b", "main")
	checkoutGit(t, checkout, "config", "user.email", "test@example.com")
	checkoutGit(t, checkout, "config", "user.name", "test")
	writeTestFile(t, filepath.Join(checkout, "f.txt"), "x\n", 0o644)
	checkoutGit(t, checkout, "add", ".")
	checkoutGit(t, checkout, "commit", "-m", "initial")
	out := &bytes.Buffer{}
	return &Doctor{W: out, Home: icebathHome, ProjectRoot: bundle, DisplayHome: icebathHome}, out, checkout
}

func TestLegacyDesktopCheckoutPristineCheckoutGetsReportWithoutDeletionCommand(t *testing.T) {
	d, out, _ := embeddedContext(t)
	doctorCheckLegacyDesktopCheckout(context.Background(), d)
	text := out.String()
	if !strings.Contains(text, "Unused checkout") || strings.Contains(text, "rm -rf") {
		t.Fatalf("output:\n%s", text)
	}
	if lower := strings.ToLower(text); strings.Contains(lower, "delete") && !strings.Contains(lower, "does not delete") {
		t.Fatalf("output:\n%s", text)
	}
}

func TestLegacyDesktopCheckoutDirtyCheckoutWarnsAndSuggestsNothing(t *testing.T) {
	d, out, checkout := embeddedContext(t)
	writeTestFile(t, filepath.Join(checkout, "f.txt"), "local work\n", 0o644)
	doctorCheckLegacyDesktopCheckout(context.Background(), d)
	if text := out.String(); !strings.Contains(text, "holds local work") || strings.Contains(text, "rm -rf") {
		t.Fatalf("output:\n%s", text)
	}
	if raw, _ := os.ReadFile(filepath.Join(checkout, "f.txt")); string(raw) != "local work\n" {
		t.Fatalf("tree changed: %q", raw)
	}
}

func TestLegacyDesktopCheckoutBranchedCheckoutCountsAsLocalWork(t *testing.T) {
	d, out, checkout := embeddedContext(t)
	checkoutGit(t, checkout, "checkout", "-b", "feature/y")
	doctorCheckLegacyDesktopCheckout(context.Background(), d)
	if text := out.String(); !strings.Contains(text, "holds local work") || strings.Contains(text, "rm -rf") {
		t.Fatalf("output:\n%s", text)
	}
}

func TestLegacyDesktopCheckoutStashedCheckoutCountsAsLocalWork(t *testing.T) {
	d, out, checkout := embeddedContext(t)
	writeTestFile(t, filepath.Join(checkout, "f.txt"), "stash me\n", 0o644)
	checkoutGit(t, checkout, "stash")
	doctorCheckLegacyDesktopCheckout(context.Background(), d)
	if text := out.String(); !strings.Contains(text, "holds local work") || strings.Contains(text, "rm -rf") {
		t.Fatalf("output:\n%s", text)
	}
}

func TestLegacyDesktopCheckoutSilentWithoutACheckout(t *testing.T) {
	d, out, checkout := embeddedContext(t)
	if err := os.RemoveAll(checkout); err != nil {
		t.Fatal(err)
	}
	doctorCheckLegacyDesktopCheckout(context.Background(), d)
	if out.String() != "" {
		t.Fatalf("output:\n%s", out.String())
	}
}

func TestLegacyDesktopCheckoutSilentWhenRunningFromAGitCheckout(t *testing.T) {
	d, out, checkout := embeddedContext(t)
	d.ProjectRoot = checkout
	doctorCheckLegacyDesktopCheckout(context.Background(), d)
	if out.String() != "" {
		t.Fatalf("output:\n%s", out.String())
	}
}

func TestPluginInBothListsIsAnIssueWithTheCommandThatSettlesIt(t *testing.T) {
	d, _ := toolsDoctor(t)
	for _, plugin := range [][2]string{{"obs/intake", "intake-probe"}, {"kept", "kept"}, {"parked", "parked"}} {
		dir := filepath.Join(d.Home, "plugins", filepath.FromSlash(plugin[0]))
		writeTestFile(t, filepath.Join(dir, "plugin.yaml"), "name: "+plugin[1]+"\nversion: '1.0'\n", 0o644)
		writeTestFile(t, filepath.Join(dir, "__init__.py"), "def register(ctx):\n    pass\n", 0o644)
	}
	writeTestFile(t, filepath.Join(d.Home, "config.yaml"), "plugins:\n  enabled: [kept, intake-probe]\n  disabled: [obs/intake, parked]\n", 0o644)
	f, err := checkPluginsInBothLists(context.Background(), d, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.ManualIssues) != 1 || !strings.Contains(f.ManualIssues[0], "icebath plugins enable obs/intake") {
		t.Fatalf("manual issues = %q", f.ManualIssues)
	}
}

func TestUnprunedCacheHogsSkipsPrunedDirsAndSmallEntries(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"scratch", "terminal", "campaign-x", "web"} {
		writeTestFile(t, filepath.Join(root, "cache", name, "blob"), strings.Repeat("x", 2048), 0o644)
	}
	hogs := doctorUnprunedCacheHogs(context.Background(), root, 1024)
	names := []string{}
	for _, hog := range hogs {
		names = append(names, hog.Name)
		if hog.Bytes < 2048 {
			t.Fatalf("hog = %+v", hog)
		}
	}
	sort.Strings(names)
	if !reflect.DeepEqual(names, []string{"campaign-x", "web"}) {
		t.Fatalf("hogs = %q", names)
	}
	if rest := doctorUnprunedCacheHogs(context.Background(), root, 1<<20); len(rest) != 0 {
		t.Fatalf("hogs above a megabyte = %+v", rest)
	}
}

// Writes one plugin dir with an optional sidecar row, git checkout and manifest update url
func provenancePlugin(t *testing.T, pluginsDir, name string, sidecar map[string]any, git bool, manifestUpdateURL string) {
	t.Helper()
	pdir := filepath.Join(pluginsDir, name)
	if err := os.MkdirAll(pdir, 0o755); err != nil {
		t.Fatal(err)
	}
	if sidecar != nil {
		side := filepath.Join(pluginsDir, ".install-metadata.json")
		rows := map[string]any{}
		if raw, err := os.ReadFile(side); err == nil {
			if err := json.Unmarshal(raw, &rows); err != nil {
				t.Fatal(err)
			}
		}
		rows[name] = sidecar
		raw, _ := json.Marshal(rows)
		writeTestFile(t, side, string(raw), 0o644)
	}
	if git {
		writeTestFile(t, filepath.Join(pdir, ".git", "config"), "[remote \"origin\"]\n\turl = https://example.com/x.git\n", 0o644)
	}
	if manifestUpdateURL != "" {
		writeTestFile(t, filepath.Join(pdir, "plugin.yaml"), "name: "+name+"\nupdate_url: "+manifestUpdateURL+"\n", 0o644)
	}
}

// Bytes of every regular file under a directory keyed by path
func treeBytes(t *testing.T, root string) map[string]string {
	t.Helper()
	files := map[string]string{}
	filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err == nil && entry.Type().IsRegular() {
			raw, _ := os.ReadFile(path)
			files[path] = string(raw)
		}
		return nil
	})
	return files
}

func TestNoPluginsDirIsInfo(t *testing.T) {
	rows := doctorPluginProvenanceRows(context.Background(), filepath.Join(t.TempDir(), "missing"))
	if !reflect.DeepEqual(rows, []doctorStatRow{{"info", "No plugins directory yet (nothing to check provenance for)", ""}}) {
		t.Fatalf("rows = %+v", rows)
	}
}

func TestMixedProvenanceDiagnosticIsReadOnly(t *testing.T) {
	d, out := toolsDoctor(t)
	pluginsDir := filepath.Join(d.Home, "plugins")
	url := "https://example.com/x"
	cases := []struct {
		name     string
		sidecar  map[string]any
		git      bool
		manifest string
		severity string
		remedy   string
	}{
		{"drifty", map[string]any{"source": "git", "update_url": url}, false, "", "warn", "reinstall"},
		{"dropped", nil, false, "", "info", "manually"},
		{"cloned", nil, true, "", "info", "adopt"},
		{"fine", map[string]any{"source": "git", "update_url": url}, true, url, "ok", "good standing"},
		{"sneaky", map[string]any{"source": "git"}, true, "https://evil.example/x", "warn", "no url was saved"},
		{"swapped", map[string]any{"source": "git", "update_url": url}, true, "https://evil.example/x", "warn", "update_url mismatch"},
		{"removed", map[string]any{"source": "git", "update_url": url}, true, "", "warn", "update_url mismatch"},
	}
	for _, c := range cases {
		provenancePlugin(t, pluginsDir, c.name, c.sidecar, c.git, c.manifest)
	}
	before := treeBytes(t, pluginsDir)
	rows := doctorPluginProvenanceRows(context.Background(), pluginsDir)
	for _, c := range cases {
		found := false
		for _, row := range rows {
			found = found || (row.Kind == c.severity && strings.Contains(row.Text, c.name) && strings.Contains(row.Text+row.Detail, c.remedy))
		}
		if !found {
			t.Fatalf("no %s row for %s naming %q in %+v", c.severity, c.name, c.remedy, rows)
		}
	}
	if _, err := checkUpdateProvenance(context.Background(), d, false); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		if !strings.Contains(out.String(), c.name) {
			t.Fatalf("output lacks %s:\n%s", c.name, out.String())
		}
	}
	if after := treeBytes(t, pluginsDir); !reflect.DeepEqual(before, after) {
		t.Fatal("provenance check changed the plugins tree")
	}
}

func TestCheckIsReadOnly(t *testing.T) {
	d, out := toolsDoctor(t)
	if _, err := checkUpdateProvenance(context.Background(), d, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "No plugins directory yet") || strings.Contains(out.String(), "⚠") {
		t.Fatalf("output:\n%s", out.String())
	}
	if _, err := os.Lstat(filepath.Join(d.Home, "plugins")); err == nil {
		t.Fatal("check created the plugins dir")
	}
}

func TestCheckSwallowsProvenanceReadFailure(t *testing.T) {
	d, out := toolsDoctor(t)
	if err := os.Mkdir(filepath.Join(d.Home, "plugins"), 0o755); err != nil {
		t.Fatal(err)
	}
	toolsSeam(t, &doctorPluginsProvenance, func(context.Context, string) ([]plugins.Provenance, error) {
		return nil, errors.New("disk gone")
	})
	if _, err := checkUpdateProvenance(context.Background(), d, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "could not be read") || !strings.Contains(out.String(), "disk gone") {
		t.Fatalf("output:\n%s", out.String())
	}
}

// Session store seeded with one session of rows messages, checkpointed, with its page size and the sessions root page
func structuralSeed(t *testing.T, dbPath string, rows int) (int64, int64) {
	t.Helper()
	ctx := context.Background()
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := state.NewSessionDB(ctx, dbPath, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CreateSession(state.NewSession{ID: "s1", Source: "cli"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < rows; i++ {
		if _, err := db.AppendMessage(ctx, "s1", map[string]any{"role": "user", "content": fmt.Sprintf("hello %d alpha beta ", i) + strings.Repeat("lorem ", 30)}, state.AppendGuards{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := state.ConnectSQLite(dbPath, state.SQLiteConfig{Timeout: 5.0})
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if checkpoint, err := raw.QueryContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err == nil {
		checkpoint.Close()
	} else {
		t.Fatal(err)
	}
	var pageSize, root int64
	if err := raw.QueryRowContext(ctx, "PRAGMA page_size").Scan(&pageSize); err != nil {
		t.Fatal(err)
	}
	if err := raw.QueryRowContext(ctx, "SELECT rootpage FROM sqlite_master WHERE name='sessions'").Scan(&root); err != nil {
		t.Fatal(err)
	}
	return pageSize, root
}

// Runs the state.db health probe against a database and returns what it found
func structuralRunDoctor(t *testing.T, dbPath string, shouldFix bool) *Finding {
	t.Helper()
	d := &Doctor{W: &bytes.Buffer{}, Home: filepath.Dir(dbPath), ProjectRoot: t.TempDir(), DisplayHome: "~/x"}
	f := &Finding{}
	if err := doctorStateDBHealth(context.Background(), d, f, shouldFix, dbPath); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestDoctorRoutesStructuralDamageToRecoverNotFtsRebuild(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("ICEBATH_HOME", tmp)
	dbPath := filepath.Join(tmp, "state.db")
	pageSize, root := structuralSeed(t, dbPath, 120)
	fh, err := os.OpenFile(dbPath, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fh.WriteAt(bytes.Repeat([]byte{0xff, 0xff}, 8), (root-1)*pageSize+8); err != nil {
		t.Fatal(err)
	}
	fh.Close()
	if damaged, err := state.StateDBHasStructuralDamage(dbPath); err != nil || !damaged {
		t.Fatalf("damaged = %v, err = %v", damaged, err)
	}
	finding := structuralRunDoctor(t, dbPath, true)
	if finding.Fixed != 0 || len(finding.Issues) != 0 || len(finding.ManualIssues) != 1 {
		t.Fatalf("finding = %+v", finding)
	}
	issue := finding.ManualIssues[0]
	if !strings.Contains(issue, "structural") || !strings.Contains(issue, "sessions recover") || !strings.Contains(issue, dbPath) ||
		strings.Contains(issue, "FTS write corruption") || strings.Contains(issue, "restore from the backup") {
		t.Fatalf("issue = %q", issue)
	}
	if backups, _ := filepath.Glob(filepath.Join(tmp, "state.db.malformed-backup-*")); len(backups) != 0 {
		t.Fatalf("backups = %q", backups)
	}
	ftsPath := filepath.Join(tmp, "fts", "state.db")
	structuralSeed(t, ftsPath, 40)
	raw, err := state.ConnectSQLite(ftsPath, state.SQLiteConfig{Timeout: 5.0})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(context.Background(), "UPDATE messages_fts_data SET block = X'DEADBEEFDEADBEEFDEADBEEFDEADBEEF'"); err != nil {
		t.Fatal(err)
	}
	raw.Close()
	if damaged, err := state.StateDBHasStructuralDamage(ftsPath); err != nil || damaged {
		t.Fatalf("fts damaged = %v, err = %v", damaged, err)
	}
	ftsFinding := structuralRunDoctor(t, ftsPath, false)
	if len(ftsFinding.ManualIssues) != 0 {
		t.Fatalf("manual issues = %q", ftsFinding.ManualIssues)
	}
	found := false
	for _, issue := range ftsFinding.Issues {
		found = found || strings.Contains(issue, "FTS")
	}
	if !found {
		t.Fatalf("issues = %q", ftsFinding.Issues)
	}
}

// One row WAL database whose WAL file is grown past the large WAL threshold after the last close
func largeWALDB(t *testing.T, dir string) string {
	t.Helper()
	db := filepath.Join(dir, "state.db")
	ctx := context.Background()
	setup, err := state.ConnectSQLite(db, state.SQLiteConfig{Timeout: 5.0})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE TABLE t(x)"); err != nil {
		t.Fatal(err)
	}
	if rows, err := setup.QueryContext(ctx, "PRAGMA journal_mode=WAL"); err == nil {
		rows.Close()
	} else {
		t.Fatal(err)
	}
	if _, err := setup.ExecContext(ctx, "INSERT INTO t VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	setup.Close()
	growWAL(t, db)
	return db
}

// Extends the WAL file of a database to fifty one megabytes
func growWAL(t *testing.T, db string) {
	t.Helper()
	handle, err := os.OpenFile(db+"-wal", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	if err := handle.Truncate(51 * 1024 * 1024); err != nil {
		t.Fatal(err)
	}
}

// Runs the WAL size check against a database and returns what it found and printed
func walCheck(t *testing.T, db string, shouldFix bool) (*Finding, string) {
	t.Helper()
	var out bytes.Buffer
	d := &Doctor{W: &out, Home: filepath.Dir(db), ProjectRoot: t.TempDir(), DisplayHome: filepath.Dir(db)}
	f := &Finding{}
	if err := doctorStateDBWAL(context.Background(), d, f, shouldFix, db); err != nil {
		t.Fatal(err)
	}
	return f, out.String()
}

func TestDoctorCheckpointRunsOnlyOnTheExclusiveRepairGuard(t *testing.T) {
	db := largeWALDB(t, t.TempDir())
	guarded := 0
	realGuard := doctorExclusiveRepairDBGuard
	toolsSeam(t, &doctorExclusiveRepairDBGuard, func(ctx context.Context, dbPath string, fn func(state.SQLiteConn, error) error) error {
		return realGuard(ctx, dbPath, func(guard state.SQLiteConn, guardErr error) error {
			if guard != nil {
				guarded++
			}
			return fn(guard, guardErr)
		})
	})
	finding, out := walCheck(t, db, true)
	if finding.Fixed != 1 || len(finding.Issues) != 0 {
		t.Fatalf("finding = %+v\n%s", finding, out)
	}
	if guarded != 1 {
		t.Fatalf("checkpoint ran on %d exclusive guards", guarded)
	}
}

func TestSessionCountReadsAHomeWithUriReservedCharacters(t *testing.T) {
	icebathHome := filepath.Join(t.TempDir(), "profile?blue#x")
	if err := os.Mkdir(icebathHome, 0o755); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(icebathHome, "state.db")
	conn, err := state.ConnectSQLite(db, state.SQLiteConfig{Timeout: 5.0})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, statement := range []string{"CREATE TABLE sessions(id TEXT)", "INSERT INTO sessions VALUES ('a'), ('b')"} {
		if _, err := conn.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	conn.Close()
	if count, err := doctorSessionCount(ctx, db); err != nil || count != 2 {
		t.Fatalf("count = %d, err = %v", count, err)
	}
}

// Asserts the one issue of a finding orders stopping the gateway before running the fix
func expectStopBeforeFix(t *testing.T, finding *Finding) {
	t.Helper()
	if len(finding.Issues) != 1 || finding.Fixed != 0 {
		t.Fatalf("finding = %+v", finding)
	}
	stop, fix := strings.Index(finding.Issues[0], "stop the profile's gateway"), strings.Index(finding.Issues[0], "icebath doctor --fix")
	if stop < 0 || fix < 0 || stop >= fix {
		t.Fatalf("issue = %q", finding.Issues[0])
	}
}

func TestLargeWalWarningUnderALiveWriterNeverSuggestsABareFix(t *testing.T) {
	toolsSeam(t, &doctorLiveWriterHoldsDB, func(context.Context, string) (bool, error) { return true, nil })
	finding, _ := walCheck(t, largeWALDB(t, t.TempDir()), false)
	expectStopBeforeFix(t, finding)
}

func TestLargeWalWarningWithoutAHolderStillOrdersStopBeforeFix(t *testing.T) {
	toolsSeam(t, &doctorLiveWriterHoldsDB, func(context.Context, string) (bool, error) { return false, nil })
	finding, _ := walCheck(t, largeWALDB(t, t.TempDir()), false)
	expectStopBeforeFix(t, finding)
}

func TestWalCheckpointSkippedWhileLiveWriterHoldsDb(t *testing.T) {
	db := filepath.Join(t.TempDir(), "state.db")
	ctx := context.Background()
	setup, err := state.ConnectSQLite(db, state.SQLiteConfig{Timeout: 5.0})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE TABLE t(x)"); err != nil {
		t.Fatal(err)
	}
	if rows, err := setup.QueryContext(ctx, "PRAGMA journal_mode=WAL"); err == nil {
		rows.Close()
	} else {
		t.Fatal(err)
	}
	if _, err := setup.ExecContext(ctx, "INSERT INTO t VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	setup.Close()
	holder, err := state.ConnectSQLite(db, state.SQLiteConfig{Timeout: 5.0})
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	var count int64
	if err := holder.QueryRowContext(ctx, "SELECT count(*) FROM t").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(db + "-wal"); err != nil {
		t.Fatalf("holder left no WAL: %v", err)
	}
	growWAL(t, db)
	finding, out := walCheck(t, db, true)
	if finding.Fixed != 0 {
		t.Fatalf("fixed = %d\n%s", finding.Fixed, out)
	}
	found := false
	for _, issue := range finding.Issues {
		found = found || strings.Contains(issue, "gateway")
	}
	if !found {
		t.Fatalf("issues = %q\n%s", finding.Issues, out)
	}
}

func TestDoctorNamesRetiredWalHoldersInsteadOfHealthyStateDb(t *testing.T) {
	d, out := toolsDoctor(t)
	writeTestFile(t, filepath.Join(d.Home, "state.db"), "", 0o644)
	toolsSeam(t, &doctorDeletedSidecarHolders, func(path string) []state.Holder {
		return []state.Holder{{PID: 4242, Target: path + "-wal"}, {PID: 4242, Target: path + "-shm"}}
	})
	finding, err := checkStateDB(context.Background(), d, true)
	if err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if !strings.Contains(text, "4242") || !strings.Contains(text, "retired WAL") || strings.Contains(text, "✓") {
		t.Fatalf("output:\n%s", text)
	}
	if lines := strings.Split(strings.TrimRight(text, "\n"), "\n"); len(lines) != 1 || finding.Fixed != 0 {
		t.Fatalf("probes ran past the retired generation: fixed = %d\n%s", finding.Fixed, text)
	}
	found := false
	for _, issue := range finding.Issues {
		found = found || (strings.Contains(issue, "4242") && strings.Contains(issue, "gateway stop"))
	}
	if !found {
		t.Fatalf("issues = %q", finding.Issues)
	}
}

func TestCheckPluginsDisabledByUpdateListsRecordedPluginsInConfigOrder(t *testing.T) {
	d, out := toolsDoctor(t)
	writeTestFile(t, filepath.Join(d.Home, "config.yaml"), "plugins:\n  disabled: [zeta, alpha, other]\n  disabled_by_update:\n"+
		"    zeta: {reason: needs a newer host}\n    gone: {reason: enabled again}\n    alpha: {}\n", 0o644)
	f, err := checkPluginsDisabledByUpdate(context.Background(), d, false)
	if err != nil {
		t.Fatal(err)
	}
	text := out.String()
	zeta, alpha := strings.Index(text, "zeta is off (needs a newer host)"), strings.Index(text, "alpha is off")
	if !strings.Contains(text, "Plugins Turned Off by an Update") || zeta < 0 || alpha < zeta || strings.Contains(text, "gone is off") {
		t.Fatalf("output:\n%s", text)
	}
	if len(f.ManualIssues) != 2 || !strings.Contains(f.ManualIssues[0], "icebath plugins enable zeta") {
		t.Fatalf("manual issues = %q", f.ManualIssues)
	}
}

func TestCheckDirectoryStructureCreatesMissingPiecesWithFix(t *testing.T) {
	d, out := toolsDoctor(t)
	f, err := checkDirectoryStructure(context.Background(), d, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"cron", "sessions", "logs", "skills", "memories", "SOUL.md"} {
		if _, err := os.Stat(filepath.Join(d.Home, name)); err != nil {
			t.Fatalf("%s was not created: %v\n%s", name, err, out.String())
		}
	}
	if f.Fixed != 6 || !strings.Contains(out.String(), "/SOUL.md with basic template") || !strings.Contains(out.String(), "is the scratch dir") {
		t.Fatalf("fixed = %d\n%s", f.Fixed, out.String())
	}
	out.Reset()
	again, err := checkDirectoryStructure(context.Background(), d, false)
	if err != nil {
		t.Fatal(err)
	}
	if again.Fixed != 0 || !strings.Contains(out.String(), "SOUL.md exists (persona configured)") || !strings.Contains(out.String(), "MEMORY.md not created yet") {
		t.Fatalf("second pass fixed = %d\n%s", again.Fixed, out.String())
	}
}

func TestCheckSkillsHubReportsLockQuarantineAndGithubCredential(t *testing.T) {
	d, out := toolsDoctor(t)
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")
	toolsSeam(t, &doctorWhich, func(string) string { return "" })
	hub := filepath.Join(d.Home, "skills", ".hub")
	writeTestFile(t, filepath.Join(hub, "lock.json"), `{"installed": {"a": {}, "b": {}}}`, 0o644)
	if err := os.MkdirAll(filepath.Join(hub, "quarantine", "held"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := checkSkillsHub(context.Background(), d, false); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Skills Hub directory exists", "Lock file OK (2 hub-installed skill(s))", "1 skill(s) in quarantine", "No GITHUB_TOKEN"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output lacks %q:\n%s", want, out.String())
		}
	}
	out.Reset()
	writeTestFile(t, filepath.Join(hub, "lock.json"), "not json", 0o644)
	t.Setenv("GH_TOKEN", "ghp_example")
	if _, err := checkSkillsHub(context.Background(), d, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Lock file (corrupted or unreadable)") || !strings.Contains(out.String(), "GitHub token configured") {
		t.Fatalf("output:\n%s", out.String())
	}
}
