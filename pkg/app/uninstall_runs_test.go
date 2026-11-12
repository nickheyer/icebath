package app

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/commands"
	"github.com/nickheyer/icebath/pkg/profiles/manage"
	"github.com/nickheyer/icebath/pkg/selfupdate"
	"github.com/spf13/cobra"
)

func TestDryRunPrintsPlanWithoutMutating(t *testing.T) {
	tmp := t.TempDir()
	projectRoot := filepath.Join(tmp, "icebath-agent")
	h := filepath.Join(tmp, ".icebath")
	os.MkdirAll(filepath.Join(projectRoot, ".git"), 0o755)
	uninstallWrite(t, filepath.Join(h, "config.yaml"), "model: {}\n")
	t.Setenv("ICEBATH_HOME", h)
	swap(t, &uninstallProjectRoot, func() string { return projectRoot })
	swap(t, &uninstallIsDefaultHome, func(string) bool { return false })
	swap(t, &uninstallDiscoverProfiles, func(context.Context, io.Writer) []manage.ProfileInfo { return nil })
	called := false
	swap(t, &uninstallPerform, func(context.Context, io.Writer, string, string, bool, bool, []manage.ProfileInfo) { called = true })
	var out bytes.Buffer
	if code, err := RunUninstall(context.Background(), map[string]any{"dry_run": true, "yes": true, "full": true}, &out); code != 0 || err != nil {
		t.Fatalf("exit %d %v", code, err)
	}
	text := out.String()
	if called || !strings.Contains(text, "Dry run") || !strings.Contains(text, projectRoot) || !strings.Contains(text, h) {
		t.Fatalf("called %v output %q", called, text)
	}
	if !uninstallExists(projectRoot) || !uninstallExists(h) {
		t.Fatal("dry run removed files")
	}
}

func TestDryRunListsNamedProfilesWithoutDesktopUserdata(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "absent-userdata"))
	t.Setenv("APPDATA", filepath.Join(tmp, "absent-userdata"))
	swap(t, &uninstallIsDefaultHome, func(string) bool { return true })
	swap(t, &uninstallDiscoverProfiles, func(context.Context, io.Writer) []manage.ProfileInfo {
		return []manage.ProfileInfo{{Name: "work", Path: filepath.Join(tmp, "profiles", "work")}}
	})
	var out bytes.Buffer
	printUninstallDryRun(context.Background(), &out, tmp, filepath.Join(tmp, ".icebath"), true)
	if !strings.Contains(out.String(), "Named profiles") || !strings.Contains(out.String(), "work") {
		t.Fatalf("output %q", out.String())
	}
}

func TestBuildUninstallParserAcceptsDryRun(t *testing.T) {
	c := commands.NewCLI()
	var ns map[string]any
	c.Bind("uninstall", func(cmd *cobra.Command, args []string) error {
		ns = c.Namespace(cmd, args)
		return nil
	})
	if err := c.Execute([]string{"uninstall", "--dry-run", "--full"}); err != nil {
		t.Fatal(err)
	}
	if ns["dry_run"] != true || ns["full"] != true {
		t.Fatalf("namespace %v", ns)
	}
}

// Temp home, checkout and desktop data with every step that reaches outside the temp dir replaced
func uninstallGUIUserdataHomes(t *testing.T) (h, projectRoot, userdata string, violations *[]string) {
	t.Helper()
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h = filepath.Join(tmp, "icebath-home")
	projectRoot = filepath.Join(tmp, "checkout")
	os.MkdirAll(h, 0o755)
	os.MkdirAll(projectRoot, 0o755)
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)
	t.Setenv("XDG_CONFIG_HOME", tmp)
	t.Setenv("XDG_DATA_HOME", filepath.Join(tmp, "xdg-data"))
	t.Setenv("APPDATA", tmp)
	t.Setenv("LOCALAPPDATA", filepath.Join(tmp, "localappdata"))
	t.Setenv("ProgramFiles", "")
	userdata = desktopUserdataDir()
	uninstallWrite(t, filepath.Join(userdata, "connections.json"), "{}")
	swap(t, &uninstallIsWindows, func() bool { return false })
	swap(t, &uninstallGatewayServiceStep, func(context.Context, io.Writer) bool { return true })
	swap(t, &uninstallShellConfigsStep, func(context.Context, io.Writer) []string { return nil })
	swap(t, &uninstallWrapperStep, func(io.Writer) []string { return nil })
	swap(t, &uninstallNodeSymlinksStep, func(io.Writer, string) []string { return nil })
	swap(t, &uninstallLegacyRuntimeStep, func(io.Writer, string) []string { return nil })
	swap(t, &uninstallDashboardJobsStep, func(io.Writer) []string { return nil })
	swap(t, &uninstallMacosCacheDirs, func() []string { return nil })
	violations = &[]string{}
	realRmtree := uninstallRmtree
	swap(t, &uninstallRmtree, func(p string) error {
		if !pathUnder(p, tmp) {
			*violations = append(*violations, "rmtree outside tmp: "+p)
			return fmt.Errorf("%s", (*violations)[len(*violations)-1])
		}
		return realRmtree(p)
	})
	swap(t, &uninstallRunCommand, func(argv []string, _ time.Duration) error {
		*violations = append(*violations, fmt.Sprintf("unexpected subprocess: %v", argv))
		return fmt.Errorf("%s", (*violations)[len(*violations)-1])
	})
	return h, projectRoot, userdata, violations
}

func TestUninstallDesktopUserdataKeptUnlessFull(t *testing.T) {
	for _, full := range []bool{false, true} {
		t.Run(fmt.Sprint(full), func(t *testing.T) {
			h, projectRoot, userdata, violations := uninstallGUIUserdataHomes(t)
			performUninstall(context.Background(), io.Discard, projectRoot, h, full, false, nil)
			if len(*violations) != 0 {
				t.Fatalf("violations %v", *violations)
			}
			if uninstallExists(filepath.Join(userdata, "connections.json")) == full {
				t.Fatalf("desktop data exists %v on full %v", !full, full)
			}
		})
	}
}

func TestRemoveDashboardLaunchdJobsBootsOutAndDeletesMatchingPlists(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS-only sweep")
	}
	tmp := t.TempDir()
	agents := filepath.Join(tmp, "LaunchAgents")
	daemons := filepath.Join(tmp, "LaunchDaemons")
	job := func(label string, args []string, dir string) string {
		var b strings.Builder
		b.WriteString(`<?xml version="1.0" encoding="UTF-8"?><plist version="1.0"><dict><key>Label</key><string>` + label +
			`</string><key>ProgramArguments</key><array>`)
		for _, a := range args {
			b.WriteString("<string>" + a + "</string>")
		}
		b.WriteString("</array></dict></plist>")
		p := filepath.Join(dir, label+".plist")
		uninstallWrite(t, p, b.String())
		return p
	}
	dashboard := job("com.user.icebath-dashboard", []string{"/Users/u/.icebath/icebath-agent/venv/bin/icebath", "dashboard", "--port", "9200"}, agents)
	serve := job("com.user.icebath-serve", []string{"icebath", "serve"}, agents)
	unrelated := job("com.user.keep", []string{"/usr/bin/say", "hello"}, agents)
	daemon := job("io.nousresearch.icebath-agent.dashboard", []string{"icebath_cli.main", "dashboard"}, daemons)
	calls := uninstallFakeRun(t)
	swap(t, &uninstallLaunchdPlistDirs, func() [][2]string { return [][2]string{{"agent", agents}, {"daemon", daemons}} })
	removed := removeDashboardLaunchdJobs(io.Discard)
	want := []string{dashboard, serve, daemon}
	slices.Sort(removed)
	slices.Sort(want)
	if !slices.Equal(removed, want) || uninstallExists(dashboard) || uninstallExists(serve) || uninstallExists(daemon) || !uninstallExists(unrelated) {
		t.Fatalf("removed %v", removed)
	}
	if len(*calls) == 0 {
		t.Fatal("no bootout attempted")
	}
	for _, c := range *calls {
		if c[0] != "launchctl" || c[1] != "bootout" {
			t.Fatalf("call %v", c)
		}
	}
}

func TestRemoveDashboardLaunchdJobsSkipsMalformedPlists(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS-only sweep")
	}
	tmp := t.TempDir()
	agents := filepath.Join(tmp, "LaunchAgents")
	uninstallWrite(t, filepath.Join(agents, "broken.plist"), "<plist><dict>&")
	uninstallWrite(t, filepath.Join(agents, "not-a-dict.plist"),
		`<?xml version="1.0" encoding="UTF-8"?><plist version="1.0"><array><string>nope</string></array></plist>`)
	swap(t, &uninstallLaunchdPlistDirs, func() [][2]string {
		return [][2]string{{"agent", agents}, {"daemon", filepath.Join(tmp, "LaunchDaemons")}}
	})
	uninstallFakeRun(t)
	if removed := removeDashboardLaunchdJobs(io.Discard); len(removed) != 0 {
		t.Fatalf("removed %v", removed)
	}
	if !uninstallExists(filepath.Join(agents, "broken.plist")) {
		t.Fatal("malformed plist removed")
	}
}

func TestFullUninstallSweepsMacosCachesAndDashboardLaunchd(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS-only sweep")
	}
	tmp := t.TempDir()
	projectRoot := filepath.Join(tmp, "icebath-agent")
	h := filepath.Join(tmp, ".icebath")
	os.MkdirAll(filepath.Join(projectRoot, ".git"), 0o755)
	os.MkdirAll(h, 0o755)
	t.Setenv("ICEBATH_HOME", h)
	var cacheDirs []string
	for _, name := range []string{"Icebath", "ai.icebath", "icebath-setup", "ai.icebath.setup"} {
		d := filepath.Join(tmp, "caches", name)
		uninstallWrite(t, filepath.Join(d, "Cache"), "x")
		cacheDirs = append(cacheDirs, d)
	}
	var removedCaches []string
	removedJobs := 0
	swap(t, &uninstallProjectRoot, func() string { return projectRoot })
	swap(t, &uninstallIsDefaultHome, func(string) bool { return false })
	swap(t, &uninstallDiscoverProfiles, func(context.Context, io.Writer) []manage.ProfileInfo { return nil })
	swap(t, &uninstallGatewayServiceStep, func(context.Context, io.Writer) bool { return true })
	swap(t, &uninstallShellConfigsStep, func(context.Context, io.Writer) []string { return nil })
	swap(t, &uninstallWrapperStep, func(io.Writer) []string { return nil })
	swap(t, &uninstallNodeSymlinksStep, func(io.Writer, string) []string { return nil })
	swap(t, &uninstallLegacyRuntimeStep, func(io.Writer, string) []string { return nil })
	swap(t, &uninstallRmtreeStep, func(_ io.Writer, p, _ string, _ bool) {
		if p != projectRoot && p != h {
			t.Errorf("unexpected rmtree %s", p)
		}
	})
	swap(t, &uninstallGUIStep, func(io.Writer, string, bool) []string { return []string{"gui"} })
	swap(t, &uninstallMacosCacheDirs, func() []string { return cacheDirs })
	swap(t, &uninstallRmtreeIfExists, func(p string) (bool, error) { removedCaches = append(removedCaches, p); return true, nil })
	swap(t, &uninstallDashboardJobsStep, func(io.Writer) []string { removedJobs++; return nil })
	RunUninstall(context.Background(), map[string]any{"dry_run": false, "yes": true, "full": true}, io.Discard)
	if !slices.Equal(removedCaches, cacheDirs) || removedJobs == 0 {
		t.Fatalf("caches %v jobs %d", removedCaches, removedJobs)
	}
	removedCaches, removedJobs = nil, 0
	RunUninstall(context.Background(), map[string]any{"dry_run": false, "yes": true, "full": false}, io.Discard)
	if len(removedCaches) != 0 || removedJobs != 0 {
		t.Fatalf("keep data touched caches %v jobs %d", removedCaches, removedJobs)
	}
}

// Fake checkout root, a git checkout or a sealed tree stamped with its steward
func uninstallFakeProjectRoot(t *testing.T, git bool, distribution string) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "icebath-agent")
	os.MkdirAll(root, 0o755)
	if git {
		os.MkdirAll(filepath.Join(root, ".git"), 0o755)
	}
	if distribution != "" {
		uninstallWrite(t, filepath.Join(root, "install-stamp.json"), fmt.Sprintf(`{"distribution": %q, "updateMechanism": "external"}`, distribution))
	}
	swap(t, &uninstallProjectRoot, func() string { return root })
	return root
}

func TestGitCheckoutAllowsCodeRemoval(t *testing.T) {
	uninstallFakeProjectRoot(t, true, "")
	if _, refused := codeRemovalRefusal(); refused {
		t.Fatal("git checkout refused")
	}
}

func TestSealedNixTreeRefusesCodeRemoval(t *testing.T) {
	uninstallFakeProjectRoot(t, false, "nix")
	refusal, refused := codeRemovalRefusal()
	if !refused || !strings.Contains(refusal, "Nix") {
		t.Fatalf("refusal %q", refusal)
	}
}

func TestSealedDesktopTreeRefusesCodeRemoval(t *testing.T) {
	uninstallFakeProjectRoot(t, false, "desktop-app")
	refusal, refused := codeRemovalRefusal()
	if !refused || !strings.Contains(refusal, "managed by the desktop app") {
		t.Fatalf("refusal %q", refusal)
	}
}

func TestStamplessSealedTreeStillRefuses(t *testing.T) {
	uninstallFakeProjectRoot(t, false, "")
	if _, refused := codeRemovalRefusal(); !refused {
		t.Fatal("stampless tree allowed")
	}
}

func TestRunUninstallExitsOnSealedTree(t *testing.T) {
	uninstallFakeProjectRoot(t, false, "nix")
	var out bytes.Buffer
	code, err := RunUninstall(context.Background(), map[string]any{"yes": true, "full": true, "dry_run": false}, &out)
	if err != nil || code != 1 || !strings.Contains(out.String(), "Nix") {
		t.Fatalf("exit %d %v %q", code, err, out.String())
	}
}

func TestRunGuiUninstallExitsOnSealedTree(t *testing.T) {
	uninstallFakeProjectRoot(t, false, "desktop-app")
	var out bytes.Buffer
	code, err := RunGUIUninstall(context.Background(), map[string]any{"yes": true}, &out)
	if err != nil || code != 1 || !strings.Contains(out.String(), "managed by the desktop app") {
		t.Fatalf("exit %d %v %q", code, err, out.String())
	}
}

func TestGuiSummaryReportsStewardAndGate(t *testing.T) {
	tmp := t.TempDir()
	guiIsolate(t, tmp)
	h := filepath.Join(tmp, ".icebath")
	agent := filepath.Join(h, "icebath-agent")
	uninstallWrite(t, filepath.Join(agent, "install-stamp.json"), `{"distribution": "nix", "updateMechanism": "external"}`)
	summary := guiInstallSummary(h)
	if summary.Vals["steward"] != "nix" || summary.Vals["code_removal_allowed"] != false {
		t.Fatalf("sealed summary %v", summary.Vals)
	}
	os.MkdirAll(filepath.Join(agent, ".git"), 0o755)
	summary = guiInstallSummary(h)
	if summary.Vals["steward"] != "git" || summary.Vals["code_removal_allowed"] != true {
		t.Fatalf("git summary %v", summary.Vals)
	}
}

func TestGuiSummaryClassifiesTheAgentRoot(t *testing.T) {
	tmp := t.TempDir()
	guiIsolate(t, tmp)
	h := filepath.Join(tmp, ".icebath")
	os.MkdirAll(filepath.Join(h, "icebath-agent", ".git"), 0o755)
	steward, allowed := selfupdate.ClassifyInstall(filepath.Join(h, "icebath-agent"))
	summary := guiInstallSummary(h)
	if summary.Vals["steward"] != steward || summary.Vals["code_removal_allowed"] != allowed || steward != "git" {
		t.Fatalf("summary %v classify %s %v", summary.Vals, steward, allowed)
	}
}
