package app

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyshlex"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Switch the packaged entry point answers before any window, lock or backend exists
const sandboxProbeSwitch = "--icebath-sandbox-probe"

const (
	apparmorProbeTimeout = 15 * time.Second
	apparmorSudoTimeout  = 300 * time.Second
	// Characters AppArmor reads as glob or string syntax inside a quoted attachment path
	apparmorAAREUnsafe = "?*[]{}^\"\\"
)

var (
	// Sysctl that restricts unprivileged user namespaces under AppArmor
	apparmorRestrictSysctl = "/proc/sys/kernel/apparmor_restrict_unprivileged_userns"
	// Directory the per binary profiles are installed into
	apparmorProfileDir = "/etc/apparmor.d"
)

// True when AppArmor restricts unprivileged user namespaces on this host
func apparmorRestrictsUserns() bool {
	raw, err := os.ReadFile(apparmorRestrictSysctl)
	if err != nil {
		return false
	}
	text, err := pystr.DecodeUTF8Sig(raw)
	return err == nil && pystr.Strip(text) == "1"
}

// Whether a file holds needle, read in overlapping chunks so a large archive never sits in memory
func fileContains(path string, needle []byte) (bool, error) {
	fh, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer fh.Close()
	r := bufio.NewReaderSize(fh, 1<<20)
	window := make([]byte, 0, 1<<20+len(needle))
	chunk := make([]byte, 1<<20)
	for {
		n, err := r.Read(chunk)
		window = append(window, chunk[:n]...)
		if bytes.Contains(window, needle) {
			return true, nil
		}
		if keep := len(needle) - 1; len(window) > keep {
			window = append(window[:0], window[len(window)-keep:]...)
		}
		if err == io.EOF {
			return false, nil
		}
		if err != nil {
			return false, err
		}
	}
}

// True when the packaged main bundle carries the probe switch, so the build answers the probe
func buildAnswersProbe(packagedExecutable string) bool {
	resources := filepath.Join(filepath.Dir(packagedExecutable), "resources")
	needle := []byte(sandboxProbeSwitch)
	for _, bundle := range []string{filepath.Join(resources, "app.asar.unpacked", "dist", "electron-main.mjs"), filepath.Join(resources, "app.asar")} {
		if found, err := fileContains(bundle, needle); err == nil && found {
			return true
		}
	}
	return false
}

// Whether Chromium's namespace sandbox starts for the binary, unknown on an unrestricted host or a build without the probe
func chromiumUsernsSandboxVerdict(packagedExecutable string) (bool, bool) {
	if guiPlatform() != "linux" || !apparmorRestrictsUserns() {
		return false, false
	}
	if !buildAnswersProbe(packagedExecutable) {
		return false, false
	}
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "ELECTRON_RUN_AS_NODE=") {
			env = append(env, kv)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), apparmorProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, packagedExecutable, "--disable-setuid-sandbox", sandboxProbeSwitch)
	cmd.Dir, cmd.Env = filepath.Dir(packagedExecutable), env
	if err := cmd.Run(); err != nil {
		return false, true
	}
	return true, true
}

// Resolved install path of the packaged binary
func apparmorExecutable(packagedExecutable string) string {
	resolved, err := pypath.Resolve(packagedExecutable)
	if err != nil {
		return packagedExecutable
	}
	return resolved
}

// One profile name per install path so two checkouts never overwrite each other's grant
func apparmorProfileName(packagedExecutable string) string {
	digest := sha256.Sum256([]byte(apparmorExecutable(packagedExecutable)))
	return "icebath-desktop-" + hex.EncodeToString(digest[:])[:10]
}

// Unconfined profile with the userns grant attached to the binary's path
func apparmorProfileText(packagedExecutable string) string {
	name := apparmorProfileName(packagedExecutable)
	exe := apparmorExecutable(packagedExecutable)
	return "# Icebath Desktop: let Chromium's user-namespace sandbox start under\n" +
		"# kernel.apparmor_restrict_unprivileged_userns=1 (written by `icebath desktop --install-apparmor-profile`).\n" +
		"abi <abi/4.0>,\n" +
		"include <tunables/global>\n\n" +
		fmt.Sprintf("profile %s \"%s\" flags=(unconfined) {\n", name, exe) +
		"  userns,\n\n" +
		fmt.Sprintf("  include if exists <local/%s>\n", name) +
		"}\n"
}

// Installed profile path of the binary
func apparmorProfilePath(packagedExecutable string) string {
	return filepath.Join(apparmorProfileDir, apparmorProfileName(packagedExecutable))
}

// Sorted characters of the install path an AppArmor attachment cannot carry literally
func apparmorUnsafePathCharacters(packagedExecutable string) string {
	seen := map[rune]bool{}
	for _, ch := range apparmorExecutable(packagedExecutable) {
		if strings.ContainsRune(apparmorAAREUnsafe, ch) || !pystr.IsPrintable(ch) {
			seen[ch] = true
		}
	}
	chars := make([]rune, 0, len(seen))
	for ch := range seen {
		chars = append(chars, ch)
	}
	slices.Sort(chars)
	return string(chars)
}

// Whether this account can replace the binary or any directory above it
func apparmorUserCanReplace(packagedExecutable string) bool {
	exe := apparmorExecutable(packagedExecutable)
	if accessWritable(exe) {
		return true
	}
	for parent := filepath.Dir(exe); ; parent = filepath.Dir(parent) {
		if accessWritable(parent) {
			return true
		}
		if filepath.Dir(parent) == parent {
			return false
		}
	}
}

// Runs one install step, false after printing why when it could not start or timed out
var apparmorRun = func(w io.Writer, command []string, stdinText *string) (int, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), apparmorSudoTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, command[0], command[1:]...)
	if stdinText != nil {
		cmd.Stdin = strings.NewReader(*stdinText)
	} else {
		cmd.Stdout = os.Stdout
	}
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		err = &pysubprocess.TimeoutExpired{Cmd: command, Timeout: int(apparmorSudoTimeout / time.Second)}
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return pysubprocess.Returncode(exitErr), true
	}
	if err != nil {
		text := err.Error()
		if pypath.IsOSError(err) {
			text = pypath.OSErrorText(err)
		}
		fmt.Fprintf(w, "✗ %s failed: %s\n", strings.Join(command, " "), text)
		return 0, false
	}
	return 0, true
}

// Compiles the profile as this user without loading it, so a bad profile never reaches the profile dir
func apparmorProfileParses(ctx context.Context, w io.Writer, parser, text string) (bool, error) {
	scratch, err := os.MkdirTemp(home.EnsureScratchDir(ctx, ""), "apparmor-")
	if err != nil {
		return false, err
	}
	candidate := filepath.Join(scratch, "icebath-desktop.profile")
	if err := os.WriteFile(candidate, []byte(text), 0o666); err != nil {
		_ = os.RemoveAll(scratch)
		return false, err
	}
	code, ran := apparmorRun(w, []string{parser, "--skip-kernel-load", "--skip-cache", candidate}, nil)
	_ = os.RemoveAll(scratch)
	if !ran {
		fmt.Fprintln(w, "✗ Could not check the generated profile; nothing was written to /etc/apparmor.d.")
		return false, nil
	}
	if code != 0 {
		fmt.Fprintln(w, "✗ apparmor_parser rejected the generated profile; nothing was written to /etc/apparmor.d.")
		return false, nil
	}
	return true, nil
}

// Stages the profile beside the target, loads it and only then renames it into place
func apparmorWriteAndLoad(w io.Writer, sudo, parser, target, text string) bool {
	staged := filepath.Join(filepath.Dir(target), "."+filepath.Base(target)+".icebath-new")
	failure := ""
	if code, ok := apparmorRun(w, []string{sudo, "tee", staged}, &text); !ok || code != 0 {
		failure = fmt.Sprintf("✗ Could not write %s.", target)
	} else if code, ok := apparmorRun(w, []string{sudo, parser, "-r", staged}, nil); !ok || code != 0 {
		failure = fmt.Sprintf("✗ apparmor_parser could not load the profile for %s.", target)
	} else if code, ok := apparmorRun(w, []string{sudo, "mv", "-f", staged, target}, nil); ok && code == 0 {
		return true
	} else {
		failure = fmt.Sprintf("✗ The profile is loaded until reboot, but moving it to %s failed.", target)
	}
	if code, ok := apparmorRun(w, []string{sudo, "rm", "-f", staged}, nil); ok && code == 0 {
		fmt.Fprintln(w, failure+" Any profile already there is unchanged.")
	} else {
		fmt.Fprintln(w, failure+fmt.Sprintf(" Remove %s with sudo rm.", staged))
	}
	return false
}

// Writes and loads the binary's profile through sudo and prints the outcome
func apparmorInstallProfile(ctx context.Context, w io.Writer, packagedExecutable string) (bool, error) {
	if guiPlatform() != "linux" {
		fmt.Fprintln(w, "✗ --install-apparmor-profile is Linux only: AppArmor profiles do not exist on this OS.")
		return false, nil
	}
	target := apparmorProfilePath(packagedExecutable)
	if unsafe := apparmorUnsafePathCharacters(packagedExecutable); unsafe != "" {
		fmt.Fprintf(w, "✗ The install path %s contains %s, which AppArmor reads as\n", apparmorExecutable(packagedExecutable), pyrepr.StrRepr(unsafe))
		fmt.Fprintln(w, "  pattern syntax (apparmor.d(5), Globbing), so a profile could not name this binary alone.")
		fmt.Fprintln(w, "  Move the checkout to a path without these characters, or use the setuid chrome-sandbox helper.")
		return false, nil
	}
	sudo := ""
	if found := tools.LocateCommand("sudo"); len(found) > 0 {
		sudo = found[0]
	}
	parser := "apparmor_parser"
	if found := tools.LocateCommandKnown("apparmor_parser", []string{"/usr/sbin", "/sbin"}); len(found) > 0 {
		parser = found[0]
	}
	text := apparmorProfileText(packagedExecutable)
	if apparmorUserCanReplace(packagedExecutable) {
		fmt.Fprintln(w, "⚠ This profile lets whatever program sits at this path, which your account can replace, use user"+
			" namespaces, so any code running as you gains what the restriction withholds; the setuid"+
			" chrome-sandbox helper avoids that.")
	}
	if sudo == "" {
		fmt.Fprintln(w, "✗ Installing the AppArmor profile needs sudo, which was not found.")
		fmt.Fprintf(w, "  As root, write %s with:\n\n%s\n", target, text)
		fmt.Fprintf(w, "  then load it: apparmor_parser -r %s\n", target)
		return false, nil
	}
	if parsed, err := apparmorProfileParses(ctx, w, parser, text); err != nil || !parsed {
		return false, err
	}
	fmt.Fprintf(w, "→ Installing AppArmor profile %s (sudo required)...\n", target)
	if !apparmorWriteAndLoad(w, sudo, parser, target, text) {
		return false, nil
	}
	if verdict, known := chromiumUsernsSandboxVerdict(packagedExecutable); known && !verdict {
		fmt.Fprintln(w, "✗ The profile is loaded, but Chromium's sandbox still cannot start for this binary.")
		return false, nil
	}
	fmt.Fprintf(w, "✓ AppArmor profile installed; Icebath Desktop can use Chromium's user-namespace sandbox (%s).\n", target)
	return true, nil
}

// Prints why the launch falls back to no sandbox, the AppArmor fixes only when the restriction is the reason
func printNoSandboxFallback(w io.Writer, packagedExecutable string) {
	if !apparmorRestrictsUserns() {
		fmt.Fprintln(w, "⚠ Falling back to --no-sandbox because ELECTRON_DISABLE_SANDBOX=1 is set and the Electron"+
			" sandbox helper could not be configured.")
		return
	}
	fmt.Fprintln(w, "⚠ Falling back to --no-sandbox because this Linux host restricts unprivileged user namespaces"+
		" and the Electron sandbox helper could not be configured.")
	helper := pyshlex.Quote(filepath.Join(filepath.Dir(packagedExecutable), "chrome-sandbox"))
	fmt.Fprintln(w, "  Ubuntu's AppArmor policy (kernel.apparmor_restrict_unprivileged_userns=1) blocks Chromium's")
	fmt.Fprintln(w, "  user-namespace sandbox for Icebath Desktop. To keep the sandbox on, run ONE of these:")
	fmt.Fprintf(w, "    sudo chown root:root %s && sudo chmod 4755 %s\n", helper, helper)
	fmt.Fprintln(w, "        Chromium's setuid sandbox helper (recommended; redo after each rebuild)")
	fmt.Fprintln(w, "    icebath desktop --install-apparmor-profile")
	fmt.Fprintln(w, "        AppArmor profile for this path (survives rebuilds; any program running as you can use it)")
}
