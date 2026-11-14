package app

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const (
	apparmorBlockedScript = "#!/bin/sh\necho 'FATAL:zygote_host_impl_linux.cc(128)] No usable sandbox!' >&2\nkill -TRAP $$\n"
	apparmorAllowedScript = "#!/bin/sh\nexit 0\n"
)

// Packaged app whose main bundle answers the probe switch and whose executable runs script
func apparmorPackagedApp(t *testing.T, tmp, script string) string {
	unpacked := filepath.Join(tmp, "linux-unpacked")
	writeTestFile(t, filepath.Join(unpacked, "resources", "app.asar.unpacked", "dist", "electron-main.mjs"),
		"if (process.argv.includes('"+sandboxProbeSwitch+"')) process.exit(0)\n", 0o644)
	exe := writeTestFile(t, filepath.Join(unpacked, "Icebath"), script, 0o755)
	writeTestFile(t, filepath.Join(unpacked, "chrome-sandbox"), "", 0o755)
	return exe
}

// Linux host restricting unprivileged user namespaces with no sudo for the setuid helper
func apparmorRestrictedHost(t *testing.T, tmp string) string {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("linux only")
	}
	swapGUISteps(t)
	sysctl := writeTestFile(t, filepath.Join(tmp, "apparmor_restrict_unprivileged_userns"), "1\n", 0o644)
	prev := apparmorRestrictSysctl
	apparmorRestrictSysctl = sysctl
	t.Cleanup(func() { apparmorRestrictSysctl = prev })
	gui.which = func(string) string { return "" }
	return sysctl
}

func TestNoSandboxFallbackNamesApparmorOnlyWhenApparmorIsTheReason(t *testing.T) {
	for _, restricted := range []bool{false, true} {
		tmp := t.TempDir()
		sysctl := apparmorRestrictedHost(t, tmp)
		content := "0\n"
		if restricted {
			content = "1\n"
		}
		writeTestFile(t, sysctl, content, 0o644)
		t.Setenv("ELECTRON_DISABLE_SANDBOX", "1")
		gui.usernsAvailable = func() bool { return false }
		exe := apparmorPackagedApp(t, tmp, apparmorBlockedScript)
		var out bytes.Buffer
		launch, ok := packagedDesktopLaunchCommand(&out, exe)
		if !ok {
			t.Fatalf("restricted %v exit, output %s", restricted, out.String())
		}
		assertArgv(t, launch, []string{exe, "--no-sandbox"})
		if strings.Contains(out.String(), "--install-apparmor-profile") != restricted || strings.Contains(out.String(), "AppArmor") != restricted {
			t.Fatalf("restricted %v output %s", restricted, out.String())
		}
	}
}

func TestGenericUnshareSuccessDoesNotOverrideABlockedBinary(t *testing.T) {
	tmp := t.TempDir()
	apparmorRestrictedHost(t, tmp)
	exe := apparmorPackagedApp(t, tmp, apparmorBlockedScript)
	gui.usernsAvailable = func() bool { return true }
	if desktopLinuxSandboxFixup(&bytes.Buffer{}, exe) {
		t.Fatal("blocked binary took the userns path")
	}
}

func TestApparmorProfileForTheBinaryEnablesTheUsernsPath(t *testing.T) {
	tmp := t.TempDir()
	apparmorRestrictedHost(t, tmp)
	exe := apparmorPackagedApp(t, tmp, apparmorAllowedScript)
	gui.usernsAvailable = func() bool { return false }
	if !desktopLinuxSandboxFixup(&bytes.Buffer{}, exe) || !desktopLinuxNeedsDisableSetuidSandbox(exe) {
		t.Fatal("allowed binary did not take the userns path")
	}
}

// Sudo that logs and runs its argv as this user and an apparmor_parser failing on one flag
func apparmorFakeRootTools(t *testing.T, tmp, parserFailsOn string) (string, string) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("linux only")
	}
	swapGUISteps(t)
	t.Setenv("ICEBATH_HOME", filepath.Join(tmp, "home"))
	binDir := filepath.Join(tmp, "fakebin")
	sudoLog := filepath.Join(tmp, "sudo.log")
	writeTestFile(t, filepath.Join(binDir, "sudo"), "#!/bin/sh\necho \"$*\" >> \""+sudoLog+"\"\nexec \"$@\"\n", 0o755)
	writeTestFile(t, filepath.Join(binDir, "apparmor_parser"), "#!/bin/sh\nfor a; do [ \"$a\" = \""+parserFailsOn+"\" ] && exit 1; done\nexit 0\n", 0o755)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	profileDir := filepath.Join(tmp, "apparmor.d")
	if err := os.MkdirAll(profileDir, 0o755); err != nil {
		t.Fatal(err)
	}
	prev := apparmorProfileDir
	apparmorProfileDir = profileDir
	t.Cleanup(func() { apparmorProfileDir = prev })
	return profileDir, sudoLog
}

// Names in a directory
func dirNames(t *testing.T, dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, filepath.Join(dir, e.Name()))
	}
	return names
}

// Installs the profile and fails on an error
func installAppArmorForTest(t *testing.T, exe string) bool {
	t.Helper()
	ok, err := apparmorInstallProfile(context.Background(), &bytes.Buffer{}, exe)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

func TestInstallPathWithApparmorGlobCharactersIsRefusedBeforeSudo(t *testing.T) {
	tmp := t.TempDir()
	profileDir, sudoLog := apparmorFakeRootTools(t, tmp, "")
	exe := apparmorPackagedApp(t, filepath.Join(tmp, "a{x}b"), apparmorAllowedScript)
	if installAppArmorForTest(t, exe) {
		t.Fatal("unsafe path installed")
	}
	if _, err := os.Stat(sudoLog); err == nil || len(dirNames(t, profileDir)) != 0 {
		t.Fatal("sudo ran or a profile was written")
	}
}

func TestProfileThatFailsToParseOrLoadLeavesNothingInTheProfileDir(t *testing.T) {
	for _, failingStep := range []string{"--skip-kernel-load", "-r"} {
		t.Run(failingStep, func(t *testing.T) {
			tmp := t.TempDir()
			profileDir, sudoLog := apparmorFakeRootTools(t, tmp, failingStep)
			exe := apparmorPackagedApp(t, tmp, apparmorAllowedScript)
			if installAppArmorForTest(t, exe) {
				t.Fatal("failing parser installed")
			}
			if len(dirNames(t, profileDir)) != 0 {
				t.Fatalf("profile dir %q", dirNames(t, profileDir))
			}
			var names []string
			if raw, err := os.ReadFile(sudoLog); err == nil {
				for _, call := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
					names = append(names, filepath.Base(strings.Fields(call)[0]))
				}
			}
			want := []string{}
			if failingStep == "-r" {
				want = []string{"tee", "apparmor_parser", "rm"}
			}
			assertArgv(t, names, want)
		})
	}
}

func TestAFailedProfileWriteKeepsTheInstalledProfile(t *testing.T) {
	tmp := t.TempDir()
	profileDir, sudoLog := apparmorFakeRootTools(t, tmp, "")
	writeTestFile(t, filepath.Join(tmp, "fakebin", "sudo"), "#!/bin/sh\necho \"$*\" >> \""+sudoLog+"\"\n[ \"$1\" = tee ] && ulimit -f 0\nexec \"$@\"\n", 0o755)
	exe := apparmorPackagedApp(t, tmp, apparmorAllowedScript)
	installed := writeTestFile(t, apparmorProfilePath(exe), apparmorProfileText(exe), 0o644)
	before, _ := os.ReadFile(installed)
	if installAppArmorForTest(t, exe) {
		t.Fatal("failed write reported installed")
	}
	after, _ := os.ReadFile(installed)
	if !bytes.Equal(before, after) {
		t.Fatal("installed profile changed")
	}
	assertArgv(t, dirNames(t, profileDir), []string{installed})
}

func TestInstallStagesLoadsThenReplacesTheOldProfile(t *testing.T) {
	tmp := t.TempDir()
	profileDir, sudoLog := apparmorFakeRootTools(t, tmp, "")
	sysctl := writeTestFile(t, filepath.Join(tmp, "apparmor_restrict_unprivileged_userns"), "1\n", 0o644)
	prev := apparmorRestrictSysctl
	apparmorRestrictSysctl = sysctl
	t.Cleanup(func() { apparmorRestrictSysctl = prev })
	exe := apparmorPackagedApp(t, tmp, apparmorAllowedScript)
	target := writeTestFile(t, apparmorProfilePath(exe), "# an older Icebath profile\n", 0o644)
	if !installAppArmorForTest(t, exe) {
		t.Fatal("install failed")
	}
	raw, _ := os.ReadFile(sudoLog)
	var calls [][]string
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		calls = append(calls, strings.Fields(line))
	}
	if len(calls) != 3 {
		t.Fatalf("calls %q", calls)
	}
	staged := calls[0][1]
	assertArgv(t, []string{filepath.Base(calls[0][0]), filepath.Base(calls[1][0]), filepath.Base(calls[2][0])}, []string{"tee", "apparmor_parser", "mv"})
	assertArgv(t, calls[1][1:], []string{"-r", staged})
	assertArgv(t, calls[2][1:], []string{"-f", staged, target})
	assertArgv(t, dirNames(t, profileDir), []string{target})
	if got, _ := os.ReadFile(target); string(got) != apparmorProfileText(exe) {
		t.Fatalf("profile %q", got)
	}
}

func TestInstallApparmorProfileRefusesBeforeAnyWarningOrSudo(t *testing.T) {
	swapGUISteps(t)
	gui.platform = func() string { return "darwin" }
	var calls [][]string
	prev := apparmorRun
	apparmorRun = func(_ io.Writer, command []string, _ *string) (int, bool) {
		calls = append(calls, command)
		return 0, true
	}
	t.Cleanup(func() { apparmorRun = prev })
	var out bytes.Buffer
	ok, err := apparmorInstallProfile(context.Background(), &out, filepath.Join(t.TempDir(), "Icebath"))
	if ok || err != nil || len(calls) != 0 {
		t.Fatalf("ok %v err %v calls %q", ok, err, calls)
	}
	if strings.TrimSpace(out.String()) != "✗ --install-apparmor-profile is Linux only: AppArmor profiles do not exist on this OS." {
		t.Fatalf("output %q", out.String())
	}
}
