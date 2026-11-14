package selfupdate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Replaces a package seam for one test and restores it afterwards
func toolsSeam[T any](t *testing.T, target *T, value T) {
	t.Helper()
	previous := *target
	*target = value
	t.Cleanup(func() { *target = previous })
}

// Doctor over a fresh home and project tree that prints into the returned buffer
func toolsDoctor(t *testing.T) (*Doctor, *bytes.Buffer) {
	t.Helper()
	root := t.TempDir()
	icebathHome := filepath.Join(root, ".icebath")
	project := filepath.Join(root, "project")
	for _, dir := range []string{icebathHome, project} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("ICEBATH_HOME", icebathHome)
	t.Setenv("HOME", root)
	out := &bytes.Buffer{}
	return &Doctor{W: out, Home: icebathHome, ProjectRoot: project, DisplayHome: icebathHome}, out
}

// Doctor that prints into the returned buffer and reads the home the environment already names
func toolsDoctorKeepingEnv(t *testing.T) (*Doctor, *bytes.Buffer) {
	t.Helper()
	out := &bytes.Buffer{}
	icebathHome := os.Getenv("ICEBATH_HOME")
	return &Doctor{W: out, Home: icebathHome, ProjectRoot: t.TempDir(), DisplayHome: icebathHome}, out
}

// First output line holding a fragment, failing the test when none does
func toolsLineWith(t *testing.T, out, fragment string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, fragment) {
			return line
		}
	}
	t.Fatalf("no line holds %q in:\n%s", fragment, out)
	return ""
}

func TestDoctorToolAvailabilitySummaryMissingApiKeySummaryIgnoresDisabledToolsets(t *testing.T) {
	d, _ := toolsDoctor(t)
	unavailable := []doctorToolset{
		{Name: "rl", MissingVars: []string{"TINKER_API_KEY"}},
		{Name: "web", MissingVars: []string{"EXA_API_KEY"}},
	}
	toolsSeam(t, &doctorEnabledCLIToolsets, func(context.Context, *Doctor) (map[string]bool, bool) {
		return map[string]bool{"web": true}, true
	})
	filtered := doctorMissingAPIKeyToolsetsForSummary(context.Background(), d, unavailable)
	if len(filtered) != 1 || filtered[0].Name != "web" {
		t.Fatalf("filtered = %+v", filtered)
	}
}

func TestDoctorToolAvailabilitySummaryImageGenWithoutProviderReportsSetupHintNotSystemDependency(t *testing.T) {
	d, out := toolsDoctor(t)
	unavailable := []doctorToolset{
		{Name: "image_gen", EnvVars: []string{}, Tools: []string{"image_generate"}},
		{Name: "homeassistant", EnvVars: []string{}, Tools: []string{}},
	}
	toolsSeam(t, &doctorEnabledCLIToolsets, func(context.Context, *Doctor) (map[string]bool, bool) {
		return map[string]bool{"image_gen": true}, true
	})
	toolsSeam(t, &doctorApplyToolAvailabilityOverrides, func(a []string, u []doctorToolset) ([]string, []doctorToolset) { return a, u })
	toolsSeam(t, &doctorWebCapabilityRows, func(context.Context, *Doctor) []doctorWebRow { return nil })
	toolsSeam(t, &doctorModelTools, func(context.Context, *Doctor) ([]string, []doctorToolset, map[string]map[string]any, error) {
		return []string{}, unavailable, map[string]map[string]any{"image_gen": {"name": "image_gen"}, "homeassistant": {"name": "homeassistant"}}, nil
	})
	f, err := checkToolAvailability(context.Background(), d, false)
	if err != nil {
		t.Fatal(err)
	}
	imageLine := toolsLineWith(t, out.String(), "image_gen")
	if !strings.Contains(imageLine, "icebath tools") || strings.Contains(imageLine, "system dependency") || !strings.Contains(imageLine, "unavailable") {
		t.Fatalf("image_gen line = %q", imageLine)
	}
	if line := toolsLineWith(t, out.String(), "homeassistant"); !strings.Contains(line, "system dependency not met") {
		t.Fatalf("homeassistant line = %q", line)
	}
	found := false
	for _, issue := range f.Issues {
		found = found || strings.Contains(issue, "icebath setup")
	}
	if !found {
		t.Fatalf("issues = %q", f.Issues)
	}
}

// Web provider standing in for a selected backend with a fixed availability answer
type fakeDoctorWebProvider struct {
	tools.WebProvider
	name      string
	available bool
}

func (p fakeDoctorWebProvider) Name() string                     { return p.name }
func (p fakeDoctorWebProvider) DisplayName() string              { return p.name }
func (p fakeDoctorWebProvider) Available(*tools.Env) bool        { return p.available }
func (p fakeDoctorWebProvider) KeylessAvailable(*tools.Env) bool { return false }
func (p fakeDoctorWebProvider) SupportsSearch() bool             { return true }
func (p fakeDoctorWebProvider) SupportsExtract() bool            { return true }

func TestDoctorToolAvailabilitySummaryWebCapabilityRowsWarnWhenSelectedProviderNotReady(t *testing.T) {
	d, _ := toolsDoctor(t)
	unavailable := fakeDoctorWebProvider{name: "firecrawl", available: false}
	toolsSeam(t, &doctorActiveSearchProvider, func(*tools.Env) tools.WebProvider { return unavailable })
	toolsSeam(t, &doctorActiveExtractProvider, func(*tools.Env) tools.WebProvider { return unavailable })
	rows := doctorWebCapabilityRows(context.Background(), d)
	if len(rows) == 0 {
		t.Fatal("no rows")
	}
	for _, row := range rows {
		if row.Status != "warn" {
			t.Fatalf("row = %+v", row)
		}
	}
}

func TestDoctorToolAvailabilitySummaryWebCapabilityRowsOkWhenProviderReady(t *testing.T) {
	d, _ := toolsDoctor(t)
	ready := fakeDoctorWebProvider{name: "ddgs", available: true}
	toolsSeam(t, &doctorActiveSearchProvider, func(*tools.Env) tools.WebProvider { return ready })
	toolsSeam(t, &doctorActiveExtractProvider, func(*tools.Env) tools.WebProvider { return ready })
	rows := doctorWebCapabilityRows(context.Background(), d)
	if len(rows) == 0 {
		t.Fatal("no rows")
	}
	for _, row := range rows {
		if row.Status != "ok" {
			t.Fatalf("row = %+v", row)
		}
	}
}

func TestDoctorToolAvailabilityOverridesMarksKanbanAvailableOnlyWhenMissingWorkerEnvGate(t *testing.T) {
	t.Setenv("ICEBATH_KANBAN_TASK", "")
	os.Unsetenv("ICEBATH_KANBAN_TASK")
	available, unavailable := doctorApplyToolAvailabilityOverrides([]string{},
		[]doctorToolset{{Name: "kanban", EnvVars: []string{}, Tools: []string{"kanban_show"}}})
	if !reflect.DeepEqual(available, []string{"kanban"}) || len(unavailable) != 0 {
		t.Fatalf("available = %q, unavailable = %+v", available, unavailable)
	}
}

func TestDoctorToolAvailabilityOverridesLeavesKanbanUnavailableWhenWorkerEnvIsSet(t *testing.T) {
	t.Setenv("ICEBATH_KANBAN_TASK", "probe")
	kanbanEntry := doctorToolset{Name: "kanban", EnvVars: []string{}, Tools: []string{"kanban_show"}}
	available, unavailable := doctorApplyToolAvailabilityOverrides([]string{}, []doctorToolset{kanbanEntry})
	if len(available) != 0 || !reflect.DeepEqual(unavailable, []doctorToolset{kanbanEntry}) {
		t.Fatalf("available = %q, unavailable = %+v", available, unavailable)
	}
}

func TestDoctorReportsVercelBackendDiagnostics(t *testing.T) {
	d, out := toolsDoctor(t)
	t.Setenv("TERMINAL_ENV", "vercel_sandbox")
	t.Setenv("TERMINAL_VERCEL_RUNTIME", "python3.13")
	t.Setenv("TERMINAL_CONTAINER_DISK", "2048")
	t.Setenv("VERCEL_TOKEN", "super-secret-value")
	t.Setenv("VERCEL_PROJECT_ID", "")
	os.Unsetenv("VERCEL_PROJECT_ID")
	t.Setenv("VERCEL_OIDC_TOKEN", "")
	t.Setenv("VERCEL_TEAM_ID", "team")
	toolsSeam(t, &doctorIsContainer, func() bool { return false })
	if _, err := checkTerminalBackend(context.Background(), d, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "VERCEL_PROJECT_ID") {
		t.Fatalf("missing auth var is not named:\n%s", out.String())
	}
	if strings.Contains(out.String(), "super-secret-value") {
		t.Fatalf("token value echoed:\n%s", out.String())
	}
}

// Shared setup for the Node and Chromium rows, PATH lookups answering node and npm only
func doctorEnvForBrowser(t *testing.T) (*Doctor, *bytes.Buffer) {
	t.Helper()
	d, out := toolsDoctor(t)
	if err := os.WriteFile(filepath.Join(d.Home, "config.yaml"), []byte("memory: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PREFIX", "/usr")
	toolsSeam(t, &doctorWhich, func(cmd string) string {
		if cmd == "node" || cmd == "npm" {
			return "/usr/bin/node"
		}
		return ""
	})
	return d, out
}

func TestRunDoctorReportsInstalledChromium(t *testing.T) {
	d, out := doctorEnvForBrowser(t)
	toolsSeam(t, &doctorChromiumExecutable, func(*tools.Env) string { return "/pm/chromium/chrome" })
	if _, err := checkNodeAndBrowser(context.Background(), d, false); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if !strings.Contains(text, "Chromium") || !strings.Contains(text, "/pm/chromium/chrome") || strings.Contains(text, "Chromium not installed") {
		t.Fatalf("output:\n%s", text)
	}
}

func TestDoctorFixDoesNotClaimSuccessWithoutPublishedBinary(t *testing.T) {
	d, out := doctorEnvForBrowser(t)
	toolsSeam(t, &doctorChromiumExecutable, func(*tools.Env) string { return "" })
	toolsSeam(t, &doctorIsTermux, func() bool { return false })
	toolsSeam(t, &doctorPMEnsure, func(context.Context, string) error { return nil })
	if err := doctorCheckChromium(context.Background(), d, true); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if !strings.Contains(text, "Chromium not installed") || !strings.Contains(text, "icebath pm install chromium") {
		t.Fatalf("output:\n%s", text)
	}
}

func TestDoctorFixReportsPmInstallFailure(t *testing.T) {
	d, out := doctorEnvForBrowser(t)
	toolsSeam(t, &doctorChromiumExecutable, func(*tools.Env) string { return "" })
	toolsSeam(t, &doctorIsTermux, func() bool { return false })
	toolsSeam(t, &doctorPMEnsure, func(context.Context, string) error {
		return &pm.InstallError{Package: "chromium", Cause: "offline", Remedy: "retry"}
	})
	if err := doctorCheckChromium(context.Background(), d, true); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if !strings.Contains(text, "Chromium install failed") || !strings.Contains(text, "offline") {
		t.Fatalf("output:\n%s", text)
	}
}

func TestMissingChromiumIsInfoWhenConfigNeedsNone(t *testing.T) {
	for _, tc := range []struct{ name, browserYAML, reason string }{
		{"cdp", "browser:\n  cdp_url: http://127.0.0.1:9222\n", "CDP override"},
		{"lightpanda", "browser:\n  engine: lightpanda\n", "lightpanda"},
		{"cloud", "browser:\n  cloud_provider: browserbase\n", "browserbase"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, out := toolsDoctor(t)
			if err := os.WriteFile(filepath.Join(d.Home, "config.yaml"), []byte(tc.browserYAML), 0o644); err != nil {
				t.Fatal(err)
			}
			t.Setenv("BROWSER_CDP_URL", "")
			os.Unsetenv("BROWSER_CDP_URL")
			toolsSeam(t, &doctorChromiumExecutable, func(*tools.Env) string { return "" })
			if err := doctorCheckChromium(context.Background(), d, false); err != nil {
				t.Fatal(err)
			}
			text := out.String()
			if !strings.Contains(text, "not needed") || !strings.Contains(text, tc.reason) || strings.Contains(text, "icebath pm install chromium") {
				t.Fatalf("output:\n%s", text)
			}
		})
	}
}

func TestRunDoctorReportsShadowedLightpandaEngine(t *testing.T) {
	d, out := doctorEnvForBrowser(t)
	toolsSeam(t, &doctorUsingLightpandaEngine, func(*tools.Env) bool { return true })
	toolsSeam(t, &doctorLightpandaStatus, func(*tools.Env) (bool, string) { return false, "cloud provider Browserbase is selected" })
	if _, err := checkNodeAndBrowser(context.Background(), d, false); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if !strings.Contains(text, "browser.engine=lightpanda is shadowed") || !strings.Contains(text, "Browserbase") {
		t.Fatalf("output:\n%s", text)
	}
}

func TestRunDoctorWarnsWhenLightpandaBinaryMissing(t *testing.T) {
	d, out := doctorEnvForBrowser(t)
	toolsSeam(t, &doctorUsingLightpandaEngine, func(*tools.Env) bool { return true })
	toolsSeam(t, &doctorLightpandaStatus, func(*tools.Env) (bool, string) { return true, "Icebath spawns `lightpanda serve`" })
	toolsSeam(t, &doctorFindLightpandaBinary, func(*tools.Env) string { return "" })
	if _, err := checkNodeAndBrowser(context.Background(), d, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Lightpanda selected but binary not found") {
		t.Fatalf("output:\n%s", out.String())
	}
}

func TestDockerDaemonProbeUsesVersionNotInfo(t *testing.T) {
	var calls [][]string
	toolsSeam(t, &doctorFindDocker, func() string { return "/usr/bin/docker" })
	toolsSeam(t, &doctorRunOK, func(_ context.Context, cmd []string, _ time.Duration) (bool, error) {
		calls = append(calls, cmd)
		return true, nil
	})
	toolsSeam(t, &doctorRequire, func(io.Writer, bool, doctorRow, doctorRow, string, *[]string) {})
	issues := []string{}
	if err := doctorCheckDockerBackend(context.Background(), io.Discard, "docker", false, &issues); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(calls, [][]string{{"/usr/bin/docker", "version"}}) {
		t.Fatalf("calls = %q", calls)
	}
}

func TestDockerBackendReadyWhenOnlyPodmanResolves(t *testing.T) {
	var probed [][]string
	var out bytes.Buffer
	issues := []string{}
	toolsSeam(t, &doctorFindDocker, func() string { return "/usr/bin/podman" })
	toolsSeam(t, &doctorRunOK, func(_ context.Context, cmd []string, _ time.Duration) (bool, error) {
		probed = append(probed, cmd)
		return true, nil
	})
	if err := doctorCheckDockerBackend(context.Background(), &out, "docker", false, &issues); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Podman (reachable)") {
		t.Fatalf("output:\n%s", out.String())
	}
	if len(issues) != 0 {
		t.Fatalf("issues = %q", issues)
	}
	if !reflect.DeepEqual(probed, [][]string{{"/usr/bin/podman", "version"}}) {
		t.Fatalf("probed = %q", probed)
	}
}

// npm audit JSON carrying the given vulnerability counts
func auditJSON(critical, high, moderate int) string {
	raw, _ := json.Marshal(map[string]any{"metadata": map[string]any{"vulnerabilities": map[string]any{
		"critical": critical, "high": high, "moderate": moderate, "low": 0, "info": 0, "total": critical + high + moderate,
	}}})
	return string(raw)
}

// Runs one root audit against a canned npm answer and returns the output and manual issues
func runAuditOne(t *testing.T, auditExtra []string, auditStdout string) (string, []string) {
	t.Helper()
	issues := []string{}
	toolsSeam(t, &doctorSubprocessRun, func(_ context.Context, argv []string, _ pysubprocess.Options) (*pysubprocess.Completed, error) {
		return &pysubprocess.Completed{Args: argv, Returncode: 0, Stdout: []byte(auditStdout)}, nil
	})
	var out bytes.Buffer
	doctorAuditOne(context.Background(), &out, "npm", "C:/fake/root", "Root npm package", auditExtra, &issues)
	return out.String(), issues
}

func TestRootRemedyNeverPrescribesLocalAuditFix(t *testing.T) {
	out, issues := runAuditOne(t, []string{"--workspaces=false"}, auditJSON(0, 1, 1))
	if strings.Contains(out, "run: cd") || strings.Contains(out, "npm audit fix") || !strings.Contains(out, "lockfile bump") {
		t.Fatalf("output:\n%s", out)
	}
	want := []string{"Root npm package has 2 npm vulnerabilities; wait for an upstream lockfile fix, then update " +
		"your installation (Docker: pull a rebuilt image; recreating the same image restores the same dependencies)"}
	if !reflect.DeepEqual(issues, want) {
		t.Fatalf("issues = %q", issues)
	}
}

func TestCleanTreeReportsNoKnownVulnerabilities(t *testing.T) {
	out, issues := runAuditOne(t, []string{"--workspaces=false"}, auditJSON(0, 0, 0))
	if !strings.Contains(out, "no known vulnerabilities") || len(issues) != 0 {
		t.Fatalf("output:\n%s\nissues = %q", out, issues)
	}
}

func TestRootAuditRowIsNotMislabeledAsAgentBrowser(t *testing.T) {
	d, out := toolsDoctor(t)
	if err := os.MkdirAll(filepath.Join(d.ProjectRoot, "node_modules"), 0o755); err != nil {
		t.Fatal(err)
	}
	toolsSeam(t, &doctorPMToolPath, func(_ context.Context, name string) string {
		if name == "npm" {
			return "/usr/bin/npm"
		}
		return ""
	})
	toolsSeam(t, &doctorWhatsAppSession, func(context.Context) (string, bool, bool, error) { return "", false, false, nil })
	toolsSeam(t, &doctorSubprocessRun, func(_ context.Context, argv []string, _ pysubprocess.Options) (*pysubprocess.Completed, error) {
		return &pysubprocess.Completed{Args: argv, Returncode: 0, Stdout: []byte(auditJSON(0, 0, 0))}, nil
	})
	if _, err := checkNpmAudit(context.Background(), d, false); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToLower(out.String()), "agent-browser") || !strings.Contains(out.String(), "Root npm package deps") {
		t.Fatalf("output:\n%s", out.String())
	}
}

func TestWorkspaceRuntimeFindingsAreNotDismissedAsBuildTooling(t *testing.T) {
	for _, tc := range []struct{ workspace, runtimePackage string }{{"web", "react-router"}} {
		t.Run(tc.workspace, func(t *testing.T) {
			auditArgs := []string{"npm", "audit", "--json", "--workspace", tc.workspace}
			raw, _ := json.Marshal(map[string]any{
				"vulnerabilities": map[string]any{
					tc.runtimePackage: map[string]any{"severity": "high", "isDirect": true},
					"vitest":          map[string]any{"severity": "moderate", "isDirect": true},
				},
				"metadata": map[string]any{"vulnerabilities": map[string]any{"high": 1, "moderate": 1}},
			})
			var ran []string
			toolsSeam(t, &doctorSubprocessRun, func(_ context.Context, argv []string, _ pysubprocess.Options) (*pysubprocess.Completed, error) {
				ran = argv
				return &pysubprocess.Completed{Args: argv, Returncode: 1, Stdout: raw}, nil
			})
			issues := []string{}
			var out bytes.Buffer
			doctorAuditOne(context.Background(), &out, "npm", t.TempDir(), tc.workspace+" workspace", auditArgs[3:], &issues)
			if !reflect.DeepEqual(ran, auditArgs) {
				t.Fatalf("argv = %q", ran)
			}
			text := out.String()
			for _, want := range []string{"runtime and build dependencies", "1 high, 1 moderate", "upstream lockfile bump"} {
				if !strings.Contains(text, want) {
					t.Fatalf("output lacks %q:\n%s", want, text)
				}
			}
			if strings.Contains(text, "not runtime") {
				t.Fatalf("output:\n%s", text)
			}
			if len(issues) != 1 || !strings.HasPrefix(issues[0], tc.workspace+" workspace has 2 npm vulnerabilities") {
				t.Fatalf("issues = %q", issues)
			}
		})
	}
}

func TestNpmAuditReportsTheInProcessWhatsAppSessionNotABridge(t *testing.T) {
	for _, tc := range []struct {
		name            string
		present, paired bool
		err             error
		want            string
		manual          int
	}{
		{"absent", false, false, nil, "", 0},
		{"paired", true, true, nil, "✓ WhatsApp session (paired, /wa/session)", 0},
		{"unpaired", true, false, nil, "⚠ WhatsApp session not paired (/wa/session)", 1},
		{"unreadable", true, false, errors.New("database is locked"), "⚠ WhatsApp session store unreadable (database is locked)", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, out := toolsDoctor(t)
			toolsSeam(t, &doctorPMToolPath, func(context.Context, string) string { return "" })
			toolsSeam(t, &doctorWhatsAppSession, func(context.Context) (string, bool, bool, error) {
				return "/wa/session", tc.present, tc.paired, tc.err
			})
			f, err := checkNpmAudit(context.Background(), d, false)
			if err != nil {
				t.Fatal(err)
			}
			text := out.String()
			if strings.Contains(text, "bridge") || (tc.want == "" && strings.Contains(text, "WhatsApp")) || !strings.Contains(text, tc.want) {
				t.Fatalf("output:\n%s", text)
			}
			if len(f.ManualIssues) != tc.manual || (tc.manual == 1 && !strings.Contains(f.ManualIssues[0], "icebath whatsapp")) {
				t.Fatalf("manual issues %q", f.ManualIssues)
			}
		})
	}
}
