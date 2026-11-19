package selfupdate

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/hostinfo"
	"github.com/nickheyer/icebath/pkg/pm"
)

// Doctor print matcher for TLS rows that keys on words instead of spacing
func tlsOutNormalized(out string) string {
	return strings.Join(strings.Fields(strings.ToLower(out)), " ")
}

func TestCheckCertificatesExercisesRealTlsPolicy(t *testing.T) {
	d, out := toolsDoctor(t)
	toolsSeam(t, &doctorInstallTrustStore, func() bool { return true })
	issues := []string{}
	if err := doctorCheckCertificatesRows(context.Background(), d, false, &issues); err != nil {
		t.Fatal(err)
	}
	text := tlsOutNormalized(out.String())
	if !strings.Contains(text, "✓") || !strings.Contains(text, "tls") || strings.Contains(text, "skipped") || strings.Contains(text, "verification active") {
		t.Fatalf("output: %s", text)
	}
	if !strings.Contains(text, "platform trust") && !strings.Contains(text, "configured") {
		t.Fatalf("output: %s", text)
	}
}

func TestCheckCertificatesFallbackNamesOpensslWithoutPlatformTrustClaim(t *testing.T) {
	d, out := toolsDoctor(t)
	toolsSeam(t, &doctorInstallTrustStore, func() bool { return false })
	issues := []string{}
	if err := doctorCheckCertificatesRows(context.Background(), d, false, &issues); err != nil {
		t.Fatal(err)
	}
	raw := out.String()
	text := tlsOutNormalized(raw)
	if !strings.Contains(raw, "⚠") || !strings.Contains(text, "trust store") || !strings.Contains(text, "openssl") ||
		strings.Contains(text, "verification active") || !strings.Contains(text, "available") {
		t.Fatalf("output: %s", raw)
	}
	for _, line := range strings.Split(raw, "\n") {
		if strings.HasPrefix(line, "✓") && strings.Contains(line, "platform trust") {
			t.Fatalf("platform trust claimed: %s", line)
		}
	}
}

func TestDoctorPlatformHintsSqliteUpgradeHintRecreatesDockerContainers(t *testing.T) {
	d, _ := toolsDoctor(t)
	toolsSeam(t, &doctorDetectInstallMethod, func(context.Context, string, string) (string, error) { return "docker", nil })
	hint, err := doctorSQLiteUpgradeHint(context.Background(), d, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(hint, "docker pull ghcr.io/nickheyer/icebath:latest") || strings.Contains(hint, "icebath update") {
		t.Fatalf("hint = %q", hint)
	}
}

func TestDoctorPlatformHintsSqliteUpgradeHintAptStampNamesTermuxExternalUpdate(t *testing.T) {
	d, _ := toolsDoctor(t)
	hint, err := doctorSQLiteUpgradeHint(context.Background(), d, "apt")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(hint, "run `"+config.TermuxInstallCommand+"`") || strings.Contains(hint, "icebath update") {
		t.Fatalf("hint = %q", hint)
	}
}

func TestDoctorPlatformHintsSqliteUpgradeHintPackageManagerStampNamesItsUpgrade(t *testing.T) {
	d, _ := toolsDoctor(t)
	for method, want := range map[string]string{"homebrew": "run `brew upgrade --cask icebath`", "chocolatey": "run `choco upgrade icebath`",
		"pip": "run `pipx upgrade icebath-cli`", "dpkg": "run `sudo apt update && sudo apt install --only-upgrade icebath`"} {
		hint, err := doctorSQLiteUpgradeHint(context.Background(), d, method)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(hint, want) || strings.Contains(hint, "icebath update") {
			t.Fatalf("%s hint = %q", method, hint)
		}
	}
	for _, method := range []string{"script", "setup-app"} {
		if hint, err := doctorSQLiteUpgradeHint(context.Background(), d, method); err != nil || !strings.Contains(hint, "run `icebath update`") {
			t.Fatalf("%s hint = %q %v", method, hint, err)
		}
	}
}

func TestDoctorPlatformHintsSqliteUpgradeHintPreservesNixGuidanceAsProse(t *testing.T) {
	d, _ := toolsDoctor(t)
	guidance := config.RecommendedUpdateCommandForMethod("nix")
	hint, err := doctorSQLiteUpgradeHint(context.Background(), d, "nix")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(hint, guidance) || strings.Contains(hint, "run `"+guidance+"`") || strings.Contains(hint, "icebath update") {
		t.Fatalf("hint = %q", hint)
	}
}

func TestStagedRuntimeVenvNothingStagedStatesTheGoRuntimeAlone(t *testing.T) {
	d, out := toolsDoctor(t)
	toolsSeam(t, &doctorStagedRuntimeRow, nil)
	f, err := checkRuntime(context.Background(), d, false)
	if err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if !strings.Contains(text, "Go "+hostinfo.GoVersion()) || strings.Contains(text, "Runtime venv staged") ||
		strings.Contains(text, "virtual environment") || strings.Contains(text, "Python") || len(f.Issues) != 0 {
		t.Fatalf("output:\n%s issues %q", text, f.Issues)
	}
}

// Source tree, data home and command link location of one command installation case
type commandTree struct {
	d       *Doctor
	out     *bytes.Buffer
	project string
	home    string
	command string
}

// Builds a git stamped source tree with its own data home and an empty command dir on PATH
func commandInstallTree(t *testing.T) commandTree {
	t.Helper()
	root := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	dataHome, project := filepath.Join(root, "data"), filepath.Join(root, "source")
	for _, dir := range []string{dataHome, project} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeTestFile(t, filepath.Join(project, ".install_method"), "git", 0o644)
	t.Setenv("ICEBATH_HOME", dataHome)
	t.Setenv("ICEBATH_RUNTIME_DIR", filepath.Join(dataHome, "tools"))
	t.Setenv("PREFIX", "")
	os.Unsetenv("PREFIX")
	t.Setenv("HOME", root)
	toolsSeam(t, &doctorUserHomeDir, func() (string, error) { return root, nil })
	command := filepath.Join(root, ".local", "bin", "icebath")
	if err := os.MkdirAll(filepath.Dir(command), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Dir(command))
	out := &bytes.Buffer{}
	return commandTree{&Doctor{W: out, Home: dataHome, ProjectRoot: project, DisplayHome: dataHome}, out, project, dataHome, command}
}

// Writes a file with its parents, failing the test on error
func writeTestFile(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
}

// Records a selected dependency generation for a project the way a committed sync does
func commandGeneration(t *testing.T, project string) string {
	t.Helper()
	stateDir := pm.InstallStateDir(context.Background(), project)
	selected := filepath.Join(stateDir, "environments", "selected", "venv")
	writeTestFile(t, filepath.Join(selected, "pyvenv.cfg"), "home = fixture\nversion = 3.11.9\n", 0o644)
	site, err := pm.SitePackages(selected)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(site, 0o755); err != nil {
		t.Fatal(err)
	}
	facts, _ := json.Marshal(map[string]any{"packages": map[string]any{"venv": map[string]any{"environment": selected}}})
	writeTestFile(t, filepath.Join(stateDir, "facts.json"), string(facts), 0o644)
	return selected
}

// Gives a project its install binary and a tool store that records an interpreter
func commandPMSource(t *testing.T, project, dataHome string) {
	t.Helper()
	writeTestFile(t, filepath.Join(project, "icebath"), "#!/bin/sh\necho selected\n", 0o755)
	interpreter := filepath.Join(dataHome, "tools", "python-fixture", "bin", "python3")
	if err := os.MkdirAll(filepath.Dir(interpreter), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/bin/sh", interpreter); err != nil {
		t.Fatal(err)
	}
	facts, _ := json.Marshal(map[string]any{"packages": map[string]any{"python": map[string]any{"entry": "python-fixture"}}})
	writeTestFile(t, filepath.Join(dataHome, "tools", "facts.json"), string(facts), 0o644)
}

// Reports whether a path is a symbolic link
func isTestSymlink(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode()&os.ModeSymlink != 0
}

func TestPmGenerationDoesNotRequireALegacyConsoleScript(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix only")
	}
	tree := commandInstallTree(t)
	selected := commandGeneration(t, tree.project)
	commandPMSource(t, tree.project, tree.home)
	staged, err := pm.StageLauncher(context.Background(), "icebath", tree.project, filepath.Dir(tree.command))
	if err != nil || staged != tree.command {
		t.Fatalf("staged = %q, err = %v", staged, err)
	}
	f, err := checkCommandInstallation(context.Background(), tree.d, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Issues) != 0 || len(f.ManualIssues) != 0 {
		t.Fatalf("finding = %+v\n%s", f, tree.out.String())
	}
	if _, err := os.Lstat(filepath.Join(tree.project, "venv")); err == nil {
		t.Fatal("legacy venv created")
	}
	if _, err := os.Lstat(filepath.Join(selected, "bin", "icebath")); err == nil {
		t.Fatal("console script created in the generation")
	}
	if info, err := os.Lstat(tree.command); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("command is not a regular file: %v", err)
	}
}

func TestPmFixPublishesAGenerationAwareLauncher(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix only")
	}
	for _, prior := range []string{"missing", "legacy"} {
		t.Run(prior, func(t *testing.T) {
			tree := commandInstallTree(t)
			commandPMSource(t, tree.project, tree.home)
			commandGeneration(t, tree.project)
			stale := filepath.Join(tree.project, "venv", "bin", "icebath")
			writeTestFile(t, stale, "#!/bin/sh\nexit 99\n", 0o755)
			if prior == "legacy" {
				if err := os.Symlink(stale, tree.command); err != nil {
					t.Fatal(err)
				}
			}
			f, err := checkCommandInstallation(context.Background(), tree.d, true)
			if err != nil {
				t.Fatal(err)
			}
			if f.Fixed != 1 {
				t.Fatalf("fixed = %d\n%s", f.Fixed, tree.out.String())
			}
			if isTestSymlink(tree.command) {
				t.Fatal("command is still a symlink")
			}
			cmd := exec.Command(tree.command)
			cmd.Dir = filepath.Dir(tree.project)
			cmd.Env = append(os.Environ(), "PATH=/usr/bin:/bin")
			stdout, err := cmd.Output()
			if err != nil || strings.TrimSpace(string(stdout)) != "selected" {
				t.Fatalf("stdout = %q, err = %v", stdout, err)
			}
			if info, err := os.Stat(stale); err != nil || !info.Mode().IsRegular() {
				t.Fatalf("stale entry removed: %v", err)
			}
		})
	}
}

func TestFixPreservesUserManagedCommands(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix only")
	}
	for _, kind := range []string{"wrapper", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			tree := commandInstallTree(t)
			commandPMSource(t, tree.project, tree.home)
			commandGeneration(t, tree.project)
			wrapper := filepath.Join(filepath.Dir(tree.project), "custom-wrapper")
			body := "#!/bin/sh\nexit 42\n"
			writeTestFile(t, wrapper, body, 0o755)
			if kind == "symlink" {
				if err := os.Symlink(wrapper, tree.command); err != nil {
					t.Fatal(err)
				}
			} else {
				writeTestFile(t, tree.command, body, 0o755)
			}
			f, err := checkCommandInstallation(context.Background(), tree.d, true)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(filepath.Join(filepath.Dir(tree.command), "icebath-acp")); f.Fixed != 1 || err != nil {
				t.Fatalf("fixed = %d, icebath-acp %v\n%s", f.Fixed, err, tree.out.String())
			}
			if raw, err := os.ReadFile(tree.command); err != nil || string(raw) != body {
				t.Fatalf("command body = %q, err = %v", raw, err)
			}
			if kind == "symlink" {
				resolved, err := filepath.EvalSymlinks(tree.command)
				if !isTestSymlink(tree.command) || err != nil || resolved != wrapper {
					t.Fatalf("command no longer links the wrapper: %q %v", resolved, err)
				}
				if !strings.Contains(strings.ToLower(strings.Join(f.ManualIssues, "\n")), "manual") {
					t.Fatalf("manual issues = %q", f.ManualIssues)
				}
			}
		})
	}
}

func TestRemediesAndLauncherRepairsRespectInstallOwner(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix only")
	}
	for _, tc := range []struct{ method, remedy string }{
		{"git", "setup-icebath.sh"}, {"nix", "Nix"}, {"docker", "docker pull"}, {"apt", "pkg install proot-distro"},
		{"script", "Reinstall with `curl -fsSL icebath.app | sh`"}, {"setup-app", "Reinstall with `curl -fsSL icebath.app | sh`"}, {"chocolatey", "choco upgrade icebath"},
		{"pip", "pipx upgrade icebath-cli"}, {"homebrew", "brew upgrade --cask icebath"}, {"scoop", "scoop update icebath"},
		{"winget", "winget upgrade --id nickheyer.icebath"}, {"dpkg", "sudo apt install --only-upgrade icebath"},
		{"rpm", "sudo dnf upgrade icebath"}, {"pacman", "paru -Syu icebath-bin"}, {"apk", "apk upgrade icebath"},
	} {
		t.Run(tc.method, func(t *testing.T) {
			tree := commandInstallTree(t)
			writeTestFile(t, filepath.Join(tree.project, ".install_method"), tc.method, 0o644)
			entry := filepath.Join(tree.project, "venv", "bin", "icebath")
			writeTestFile(t, entry, "#!/bin/sh\nexit 0\n", 0o755)
			findings, err := checkCommandInstallation(context.Background(), tree.d, true)
			if err != nil {
				t.Fatal(err)
			}
			hint, err := doctorReinstallHint(context.Background(), tree.d)
			if err != nil {
				t.Fatal(err)
			}
			text := tree.out.String() + "\n" + strings.Join(append(append(findings.Issues, findings.ManualIssues...), hint), "\n")
			if !strings.Contains(text, tc.remedy) || strings.Contains(text, "pip install") {
				t.Fatalf("output:\n%s", text)
			}
			if _, err := os.Lstat(tree.command); err == nil {
				t.Fatalf("command published from a tree without an install binary\n%s", text)
			}
			if tc.method == "git" || tc.method == "script" || tc.method == "setup-app" {
				return
			}
			if strings.Contains(text, "setup-icebath") || strings.Contains(text, "Reinstall with") {
				t.Fatalf("output:\n%s", text)
			}
		})
	}
}

func TestCheckPythonEnvironmentRaisesNoIssueWithoutAManagedInterpreter(t *testing.T) {
	d, out := toolsDoctor(t)
	toolsSeam(t, &doctorStagedRuntimeRow, nil)
	toolsSeam(t, &doctorDetectInstallMethod, func(context.Context, string, string) (string, error) { return "git", nil })
	f, err := checkRuntime(context.Background(), d, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Issues) != 0 || len(f.ManualIssues) != 0 {
		t.Fatalf("issues = %q manual %q", f.Issues, f.ManualIssues)
	}
	if strings.Contains(out.String(), "Python (missing)") || !strings.Contains(out.String(), "SQLite ") {
		t.Fatalf("output:\n%s", out.String())
	}
}

func TestCheckPythonEnvironmentStatesTheGoRuntimeVersion(t *testing.T) {
	d, out := toolsDoctor(t)
	toolsSeam(t, &doctorStagedRuntimeRow, nil)
	if _, err := checkRuntime(context.Background(), d, false); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if want := "Go " + hostinfo.GoVersion(); toolsLineWith(t, text, want) != "  ✓ "+want {
		t.Fatalf("runtime row is not %q in:\n%s", want, text)
	}
}

func TestRequiredPackagesListsOptionalCapabilitiesWithTheirRows(t *testing.T) {
	d, out := toolsDoctor(t)
	toolsSeam(t, &doctorPackages, []doctorPackage{
		{"Loaded library", func(context.Context) bool { return true }},
		{"Absent library", func(context.Context) bool { return false }},
	})
	f, err := checkOptionalCapabilities(context.Background(), d, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Issues) != 0 || len(f.ManualIssues) != 0 {
		t.Fatalf("issues = %q %q", f.Issues, f.ManualIssues)
	}
	text := out.String()
	if !strings.Contains(text, "✓ Loaded library (optional)") || !strings.Contains(text, "⚠ Absent library (optional, not installed)") {
		t.Fatalf("output:\n%s", text)
	}
}

func TestRequiredPackagesNamesNoPythonLibraryCompiledIntoTheBinary(t *testing.T) {
	for _, p := range doctorPackages {
		for _, compiled := range []string{"OpenAI SDK", "Rich", "python-dotenv", "ruamel.yaml", "HTTPX", "Croniter", "python-telegram-bot", "discord.py"} {
			if strings.Contains(p.Name, compiled) {
				t.Fatalf("row %q names %q, which the binary carries", p.Name, compiled)
			}
		}
	}
}
