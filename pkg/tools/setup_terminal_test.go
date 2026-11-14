package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/pm"
)

// Scripted setup console recording every printed line
type fakeSetupConsole struct {
	prompt func(question, def string, password bool) string
	yesNo  func(question string, def bool) bool
	choice func(question string, choices []string, def int) int
	out    []string
}

func (c *fakeSetupConsole) Prompt(q, def string, password bool) string {
	return c.prompt(q, def, password)
}
func (c *fakeSetupConsole) PromptYesNo(q string, def bool) bool { return c.yesNo(q, def) }
func (c *fakeSetupConsole) PromptChoice(q string, choices []string, def int) int {
	return c.choice(q, choices, def)
}
func (c *fakeSetupConsole) PrintHeader(title string) { c.out = append(c.out, title) }
func (c *fakeSetupConsole) PrintInfo(text string)    { c.out = append(c.out, text) }
func (c *fakeSetupConsole) PrintSuccess(text string) { c.out = append(c.out, text) }
func (c *fakeSetupConsole) PrintWarning(text string) { c.out = append(c.out, text) }
func (c *fakeSetupConsole) Println()                 { c.out = append(c.out, "") }
func (c *fakeSetupConsole) text() string             { return strings.Join(c.out, "\n") }
func answersFrom(t *testing.T, answers ...string) func(string, string, bool) string {
	return func(q, def string, password bool) string {
		if len(answers) == 0 {
			t.Fatalf("unexpected prompt %q", q)
		}
		a := answers[0]
		answers = answers[1:]
		return a
	}
}

// Installs a console and isolates every env var the wizard publishes
func withSetupConsole(t *testing.T, c *fakeSetupConsole, keys ...string) {
	prev := SetupTerminalConsole
	SetupTerminalConsole = c
	t.Cleanup(func() { SetupTerminalConsole = prev })
	for _, k := range append(keys, "TERMINAL_ENV", "TERMINAL_MODAL_MODE", "TERMINAL_VERCEL_IMAGE") {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
}

func newTerminalSetup(t *testing.T, env *Env) *terminalSetup {
	return &terminalSetup{ctx: context.Background(), env: env, ui: SetupTerminalConsole, h: envHome(env)}
}

func loadSetupConfig(t *testing.T, env *Env) map[string]any {
	tree, err := config.Load(envHome(env), os.LookupEnv)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]any(tree)
}

var sshKeys = []string{"TERMINAL_SSH_HOST", "TERMINAL_SSH_USER", "TERMINAL_SSH_PORT", "TERMINAL_SSH_KEY"}

// Answering the default port removes a previously saved port and a real port persists
func TestSSHWizardPortHandling(t *testing.T) {
	for _, c := range []struct {
		name, saved, answer string
		want                string
		present             bool
	}{
		{"port reset to 22 removes saved port", "2222", "22", "", false},
		{"non default port still saves", "", "2222", "2222", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			env := newTestToolEnv(t)
			t.Setenv("ICEBATH_HOME", env.Home)
			console := &fakeSetupConsole{prompt: answersFrom(t, "host.example.com", "me", c.answer, "/tmp/key"),
				yesNo: func(string, bool) bool { return false }}
			withSetupConsole(t, console, sshKeys...)
			s := newTerminalSetup(t, env)
			if c.saved != "" {
				if err := s.saveEnv("TERMINAL_SSH_PORT", c.saved); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.backendSSH(map[string]any{}); err != nil {
				t.Fatal(err)
			}
			got, ok, err := config.GetEnvValue(s.ctx, s.h, "TERMINAL_SSH_PORT")
			if err != nil || ok != c.present || got != c.want {
				t.Fatalf("TERMINAL_SSH_PORT = %q %v %v", got, ok, err)
			}
		})
	}
}

// Podman satisfies the docker backend and the wizard names it instead of warning
func TestDockerWizardReportsTheResolvedPodmanRuntime(t *testing.T) {
	env := newTestToolEnv(t)
	console := &fakeSetupConsole{yesNo: func(string, bool) bool { return false }}
	withSetupConsole(t, console)
	prev := setupFindDocker
	setupFindDocker = func(*Env) string { return "/usr/bin/podman" }
	t.Cleanup(func() { setupFindDocker = prev })
	if err := newTerminalSetup(t, env).backendDocker(map[string]any{"terminal": map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	out := console.text()
	if !strings.Contains(out, "Podman found: /usr/bin/podman") || strings.Contains(out, "not found in PATH") {
		t.Fatalf("output %q", out)
	}
}

// Makes the managed modal checks pass for the wizard
func withManagedModal(t *testing.T) {
	prevTools, prevGateway, prevAuth := setupManagedNousToolsEnabled, setupManagedGatewayReady, NousSubscriptionAuthPresent
	setupManagedNousToolsEnabled = func(*Env) bool { return true }
	setupManagedGatewayReady = func(_ *Env, vendor string) bool { return vendor == "modal" }
	NousSubscriptionAuthPresent = func(*Env, map[string]any) bool { return true }
	t.Cleanup(func() {
		setupManagedNousToolsEnabled, setupManagedGatewayReady, NousSubscriptionAuthPresent = prevTools, prevGateway, prevAuth
	})
}

func backendChoice(t *testing.T, backendIdx, billingIdx int) func(string, []string, int) int {
	return func(q string, choices []string, def int) int {
		switch q {
		case "Select terminal backend:":
			return backendIdx
		case "Select how Modal execution should be billed:":
			if billingIdx >= 0 {
				return billingIdx
			}
		}
		t.Fatalf("Unexpected prompt_choice call: %s", q)
		return 0
	}
}

// Choosing an own Modal account persists the direct mode
func TestModalSetupPersistsDirectModeWhenUserChoosesTheirOwnAccount(t *testing.T) {
	env := newTestToolEnv(t)
	t.Setenv("ICEBATH_HOME", env.Home)
	withManagedModal(t)
	console := &fakeSetupConsole{choice: backendChoice(t, 2, 1), prompt: answersFrom(t, "token-id", "token-secret", "")}
	withSetupConsole(t, console, "MODAL_TOKEN_ID", "MODAL_TOKEN_SECRET")
	cfg := loadSetupConfig(t, env)
	if err := SetupTerminalBackend(context.Background(), env, cfg); err != nil {
		t.Fatal(err)
	}
	terminal := cfg["terminal"].(map[string]any)
	if terminal["backend"] != "modal" || terminal["modal_mode"] != "direct" {
		t.Fatalf("terminal %v", terminal)
	}
}

func clearVercelEnv() []string {
	keys := []string{"VERCEL_OIDC_TOKEN", "VERCEL_TOKEN", "VERCEL_PROJECT_ID", "VERCEL_TEAM_ID"}
	return keys
}

// The Vercel wizard drops the OIDC token and saves the access token, project and team
func TestVercelSetupConfiguresAccessTokenAuth(t *testing.T) {
	env := newTestToolEnv(t)
	t.Setenv("ICEBATH_HOME", env.Home)
	console := &fakeSetupConsole{choice: backendChoice(t, 5, -1),
		prompt: answersFrom(t, "vercel/sandbox/python:3.14", "yes", "2", "4096", "token", "project", "team")}
	withSetupConsole(t, console, clearVercelEnv()...)
	os.Setenv("VERCEL_OIDC_TOKEN", "old-oidc")
	cfg := loadSetupConfig(t, env)
	if err := SetupTerminalBackend(context.Background(), env, cfg); err != nil {
		t.Fatal(err)
	}
	terminal := cfg["terminal"].(map[string]any)
	if terminal["backend"] != "vercel_sandbox" || terminal["vercel_image"] != "vercel/sandbox/python:3.14" || terminal["container_disk"] != 51200 {
		t.Fatalf("terminal %v", terminal)
	}
	if _, ok := os.LookupEnv("VERCEL_OIDC_TOKEN"); ok {
		t.Fatal("VERCEL_OIDC_TOKEN kept")
	}
	for k, want := range map[string]string{"TERMINAL_VERCEL_IMAGE": "vercel/sandbox/python:3.14", "VERCEL_TOKEN": "token", "VERCEL_PROJECT_ID": "project", "VERCEL_TEAM_ID": "team"} {
		if got := os.Getenv(k); got != want {
			t.Fatalf("%s = %q, want %q", k, got, want)
		}
	}
}

// The nearest Vercel link file prefills the project and team prompts
func TestVercelSetupPrefillsProjectAndTeamFromLinkFile(t *testing.T) {
	env := newTestToolEnv(t)
	t.Setenv("ICEBATH_HOME", env.Home)
	project := filepath.Join(t.TempDir(), "project")
	nested := filepath.Join(project, "app", "src")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(project, ".vercel"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, ".vercel", "project.json"), []byte(`{"projectId": "linked-project", "orgId": "linked-team"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(nested)
	answers := []string{"", "no", "1", "5120", "token", "", ""}
	defaults := map[string]string{}
	console := &fakeSetupConsole{choice: backendChoice(t, 5, -1), prompt: func(q, def string, password bool) string {
		defaults[q] = def
		a := answers[0]
		answers = answers[1:]
		if a == "" {
			return def
		}
		return a
	}}
	withSetupConsole(t, console, clearVercelEnv()...)
	cfg := loadSetupConfig(t, env)
	cfg["terminal"].(map[string]any)["container_disk"] = 999
	if err := SetupTerminalBackend(context.Background(), env, cfg); err != nil {
		t.Fatal(err)
	}
	terminal := cfg["terminal"].(map[string]any)
	if terminal["backend"] != "vercel_sandbox" || terminal["container_persistent"] != false || terminal["container_disk"] != 51200 {
		t.Fatalf("terminal %v", terminal)
	}
	for k, want := range map[string]string{"VERCEL_TOKEN": "token", "VERCEL_PROJECT_ID": "linked-project", "VERCEL_TEAM_ID": "linked-team"} {
		if got := os.Getenv(k); got != want {
			t.Fatalf("%s = %q, want %q", k, got, want)
		}
	}
	if defaults["    Vercel project ID"] != "linked-project" || defaults["    Vercel team ID"] != "linked-team" {
		t.Fatalf("defaults %v", defaults)
	}
	if !strings.Contains(console.text(), "Vercel Sandbox does not support custom disk sizing") {
		t.Fatalf("output %q", console.text())
	}
}

// Malformed, non object and blank link files yield no defaults while invalid UTF-8 raises
func TestReadNearestVercelProjectBranches(t *testing.T) {
	for _, c := range []struct {
		body    string
		want    map[string]string
		wantErr bool
	}{
		{`{"projectId": "p", "orgId": "  "}`, map[string]string{"projectId": "p"}, false},
		{"\ufeff{\"orgId\": \"o\", \"projectId\": 3}", map[string]string{"orgId": "o"}, false},
		{`[1]`, map[string]string{}, false},
		{`{bad`, map[string]string{}, false},
		{"\xff", nil, true},
	} {
		dir := t.TempDir()
		os.MkdirAll(filepath.Join(dir, ".vercel"), 0o755)
		file := filepath.Join(dir, ".vercel", "project.json")
		os.WriteFile(file, []byte(c.body), 0o644)
		got, err := readNearestVercelProject(file)
		if (err != nil) != c.wantErr || fmt.Sprint(got) != fmt.Sprint(c.want) {
			t.Fatalf("%q -> %v %v", c.body, got, err)
		}
	}
}

func TestSetupSyncFailureHandledMatchesTheWizardClasses(t *testing.T) {
	handled := []error{pm.NewInstallError("venv", "resolution refused", ""), &pm.PyError{Class: "ValueError", Message: "bad"}, &pm.PyError{Class: "PermissionError", Message: "denied"}}
	for _, err := range handled {
		if !SetupSyncFailureHandled(err) {
			t.Fatalf("%v not handled", err)
		}
	}
	if SetupSyncFailureHandled(&pm.PyError{Class: "RuntimeError", Message: "boom"}) || SetupSyncFailureHandled(fmt.Errorf("boom")) {
		t.Fatal("an unhandled class was reported handled")
	}
}

// Drives the SSH wizard with scripted answers and the connection test declined
func runSSHWizard(t *testing.T, env *Env, answers []string) {
	t.Helper()
	i := 0
	console := &fakeSetupConsole{
		prompt: func(string, string, bool) string { i++; return answers[i-1] },
		yesNo:  func(string, bool) bool { return false },
	}
	withSetupConsole(t, console)
	if err := newTerminalSetup(t, env).backendSSH(map[string]any{}); err != nil {
		t.Fatal(err)
	}
}

func TestSshPortResetTo22RemovesSavedPort(t *testing.T) {
	env := newTestToolEnv(t)
	t.Setenv("ICEBATH_HOME", env.Home)
	t.Setenv("TERMINAL_SSH_PORT", "")
	os.Unsetenv("TERMINAL_SSH_PORT")
	if err := config.SaveEnvValue(env.Home, "TERMINAL_SSH_PORT", "2222"); err != nil {
		t.Fatal(err)
	}
	runSSHWizard(t, env, []string{"host.example.com", "me", "22", "/tmp/key"})
	if v, ok, err := config.GetEnvValue(context.Background(), env.Home, "TERMINAL_SSH_PORT"); err != nil || ok {
		t.Fatalf("TERMINAL_SSH_PORT = %q %v %v", v, ok, err)
	}
}

func TestSshPortNonDefaultStillSaves(t *testing.T) {
	env := newTestToolEnv(t)
	t.Setenv("ICEBATH_HOME", env.Home)
	t.Setenv("TERMINAL_SSH_PORT", "")
	os.Unsetenv("TERMINAL_SSH_PORT")
	runSSHWizard(t, env, []string{"host.example.com", "me", "2222", "/tmp/key"})
	if v, ok, err := config.GetEnvValue(context.Background(), env.Home, "TERMINAL_SSH_PORT"); err != nil || !ok || v != "2222" {
		t.Fatalf("TERMINAL_SSH_PORT = %q %v %v", v, ok, err)
	}
}
