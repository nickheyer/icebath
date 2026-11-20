package pm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

var libatomicInstallers = map[string][]string{
	"apt-get": {"apt-get", "install", "-y", "libatomic1"},
	"dnf":     {"dnf", "install", "-y", "libatomic"},
	"yum":     {"yum", "install", "-y", "libatomic"},
	"zypper":  {"zypper", "--non-interactive", "install", "libatomic1"},
	"pacman":  {"pacman", "-S", "--needed", "--noconfirm", "gcc-libs"},
	"apk":     {"apk", "add", "libatomic"},
}

var (
	debianFamily = map[string]bool{"debian": true, "ubuntu": true, "linuxmint": true, "pop": true, "raspbian": true, "kali": true, "elementary": true}
	rpmFamily    = map[string]bool{"rhel": true, "fedora": true, "centos": true, "rocky": true, "almalinux": true, "ol": true, "oracle": true, "amzn": true}
	archFamily   = map[string]bool{"arch": true, "manjaro": true, "endeavouros": true, "garuda": true, "artix": true}
)

// Outcome of the one libatomic attempt this process makes
type libatomicOutcome struct {
	attempted bool
	remedy    string
}

var (
	libatomicMu      sync.Mutex
	libatomicAttempt *libatomicOutcome
	osReleaseFiles   = []string{"/etc/os-release", "/usr/lib/os-release"}
	osReleaseLine    = regexp.MustCompile(`^([a-zA-Z0-9_]+)=(["']?)(.*)$`)
	osReleaseUnquote = regexp.MustCompile("\\\\([\\\\$\"'`])")
)

// Fields of the freedesktop os-release file with its mandatory defaults, an error when none is readable
func freedesktopOSRelease() (map[string]string, error) {
	var lastErr error
	for _, path := range osReleaseFiles {
		data, err := os.ReadFile(path)
		if err != nil {
			lastErr = err
			continue
		}
		info := map[string]string{"NAME": "Linux", "ID": "linux", "PRETTY_NAME": "Linux"}
		for _, line := range strings.Split(universalNewlines(string(data)), "\n") {
			m := osReleaseLine.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			value := m[3]
			if m[2] != "" {
				if strings.HasSuffix(value, m[2]) && len(value) >= len(m[2]) {
					value = value[:len(value)-len(m[2])]
				} else {
					value = m[2] + value
				}
			}
			info[m[1]] = osReleaseUnquote.ReplaceAllString(value, "$1")
		}
		return info, nil
	}
	if lastErr == nil {
		lastErr = errors.New("Unable to read files /etc/os-release, /usr/lib/os-release")
	}
	return nil, lastErr
}

// Lowercased ID and ID_LIKE tokens of the host distribution
func releaseTokens() map[string]bool {
	fields, err := freedesktopOSRelease()
	if err != nil {
		return map[string]bool{}
	}
	blob := strings.ToLower(fields["ID"] + " " + fields["ID_LIKE"])
	tokens := map[string]bool{}
	for _, t := range strings.Fields(strings.ReplaceAll(blob, ",", " ")) {
		tokens[t] = true
	}
	return tokens
}

// Package managers to try for a distribution family, every known one when the family is unknown
func managerOrder(tokens map[string]bool) []string {
	intersects := func(family map[string]bool) bool {
		for t := range tokens {
			if family[t] {
				return true
			}
		}
		return false
	}
	switch {
	case intersects(debianFamily):
		return []string{"apt-get"}
	case intersects(rpmFamily):
		return []string{"dnf", "yum"}
	}
	for t := range tokens {
		if strings.Contains(t, "suse") || strings.HasPrefix(t, "sles") {
			return []string{"zypper"}
		}
	}
	if intersects(archFamily) {
		return []string{"pacman"}
	}
	if tokens["alpine"] {
		return []string{"apk"}
	}
	return []string{"dnf", "yum", "apt-get", "zypper", "pacman", "apk"}
}

// Executable path of a command on PATH like shutil which, a relative PATH entry kept, empty when absent
var libatomicWhich = func(name string) string {
	path, err := exec.LookPath(name)
	if err != nil && !errors.Is(err, exec.ErrDot) {
		return ""
	}
	return path
}

// Runs a prepared install command to completion
var libatomicRun = func(cmd *exec.Cmd) error { return cmd.Run() }

// Install command of the first available package manager for this host
var hostInstallCommand = func() []string {
	for _, manager := range managerOrder(releaseTokens()) {
		if libatomicWhich(manager) != "" {
			return libatomicInstallers[manager]
		}
	}
	return nil
}

// Reports whether this process runs as root
var isRoot = func() bool {
	return runtime.GOOS != "windows" && os.Geteuid() == 0
}

var shellSafe = regexp.MustCompile(`^[\w@%+=:,./-]+$`)

// Shell quotes one argument the way shlex quote does
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	if shellSafe.MatchString(s) && isASCII(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// Reports a pure ASCII string
func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// Shell quoted command line the way shlex join renders it
func shellJoin(argv []string) string {
	parts := make([]string, len(argv))
	for i, a := range argv {
		parts[i] = shellQuote(a)
	}
	return strings.Join(parts, " ")
}

// Auto repair argv, its display form and a runnable manual remedy, sudo always non interactive inside the lock
func commandPlan(command []string) ([]string, string, string) {
	if isRoot() {
		shown := shellJoin(command)
		return append([]string{}, command...), shown, shown
	}
	if sudo := libatomicWhich("sudo"); sudo != "" {
		argv := append([]string{sudo, "-n"}, command...)
		return argv, shellJoin(append([]string{"sudo", "-n"}, command...)), shellJoin(append([]string{"sudo"}, command...))
	}
	return nil, "", "as root: " + shellJoin(command)
}

// Linux soname a host binary's dynamic loader would resolve for this architecture
func sharedLibraryLoadable(soname string) bool {
	target, err := CurrentTarget()
	if err != nil {
		return false
	}
	matches := func(path string) bool {
		if !isRegularFile(path) {
			return false
		}
		match, known := MachineMatchesBinary(path, target)
		return !known || match
	}
	for _, dir := range filepath.SplitList(os.Getenv("LD_LIBRARY_PATH")) {
		if dir != "" && matches(filepath.Join(dir, soname)) {
			return true
		}
	}
	if path := ldconfigLookup(soname, target); path != "" && matches(path) {
		return true
	}
	triplet := map[string]string{"x64": "x86_64-linux-gnu", "arm64": "aarch64-linux-gnu"}[strings.Split(target, "-")[1]]
	for _, dir := range []string{"/lib/" + triplet, "/usr/lib/" + triplet, "/lib64", "/usr/lib64", "/lib", "/usr/lib"} {
		if matches(filepath.Join(dir, soname)) {
			return true
		}
	}
	return false
}

// Path ldconfig's cache maps a soname to for the host architecture, empty when unlisted
func ldconfigLookup(soname, target string) string {
	tag := map[string]string{"x64": "x86-64", "arm64": "AArch64"}[strings.Split(target, "-")[1]]
	for _, candidate := range []string{"/sbin/ldconfig", "/usr/sbin/ldconfig", "ldconfig"} {
		out, err := exec.Command(candidate, "-p").Output()
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(out), "\n") {
			name, rest, ok := strings.Cut(strings.TrimSpace(line), " (")
			if !ok || name != soname {
				continue
			}
			tags, path, ok := strings.Cut(rest, ") => ")
			if ok && strings.Contains(tags, tag) {
				return strings.TrimSpace(path)
			}
		}
		return ""
	}
	return ""
}

// Installs libatomic interactively before PM takes its lock, only where sudo would actually prompt
func InstallLibatomicBeforeLock(ctx context.Context) {
	if runtime.GOOS != "linux" || isRoot() || !hasTerminal() {
		return
	}
	target, err := CurrentTarget()
	if err != nil {
		return
	}
	if MuslTargets[target] || strings.HasSuffix(target, "-bionic") {
		return
	}
	if sharedLibraryLoadable("libatomic.so.1") {
		return
	}
	command := hostInstallCommand()
	if command == nil {
		return
	}
	argv, _, _ := commandPlan(command)
	if len(argv) == 0 {
		return
	}
	sudo := argv[0]
	if runQuiet(ctx, []string{sudo, "-n", "true"}, false, 10*time.Second) {
		return
	}
	fmt.Println("→ Node.js needs libatomic.so.1; sudo may ask for your password to install it")
	installWith(ctx, []string{sudo}, command, true, 0)
}

// Reports a controlling terminal sudo could prompt on
func hasTerminal() bool {
	f, err := os.OpenFile("/dev/tty", os.O_RDWR|noCTTY, 0)
	if err != nil {
		return false
	}
	f.Close()
	return true
}

// Runs a command with inherited output and reports success, a zero timeout meaning none
func runQuiet(ctx context.Context, argv []string, interactive bool, timeout time.Duration) bool {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	if interactive {
		cmd.Stdin = os.Stdin
	}
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return libatomicRun(cmd) == nil
}

// Runs the install command, refreshing empty Debian package lists once when apt-get fails
func installWith(ctx context.Context, prefix, command []string, interactive bool, timeout time.Duration) bool {
	full := append(append([]string{}, prefix...), command...)
	if runQuiet(ctx, full, interactive, timeout) {
		return true
	}
	if command[0] != "apt-get" {
		return false
	}
	return runQuiet(ctx, append(append([]string{}, prefix...), "apt-get", "update", "-qq"), interactive, timeout) &&
		runQuiet(ctx, full, interactive, timeout)
}

// Tries the distro libatomic package once per process and returns whether it ran and the remedy hint
func TryInstallLibatomic(ctx context.Context) (bool, string) {
	libatomicMu.Lock()
	defer libatomicMu.Unlock()
	if libatomicAttempt == nil {
		attempted, remedy := attemptLibatomic(ctx)
		libatomicAttempt = &libatomicOutcome{attempted: attempted, remedy: remedy}
	}
	return libatomicAttempt.attempted, libatomicAttempt.remedy
}

// One non interactive attempt at the host libatomic package
func attemptLibatomic(ctx context.Context) (bool, string) {
	command := hostInstallCommand()
	if command == nil {
		return false, "official Node needs libatomic.so.1; install the distro package that provides it and rerun"
	}
	argv, attemptDisplay, remedyDisplay := commandPlan(command)
	remedy := fmt.Sprintf("install the missing runtime library with %s and rerun", remedyDisplay)
	if argv == nil {
		return false, remedy
	}
	fmt.Printf("→ Node needs libatomic.so.1; trying non-interactive %s\n", attemptDisplay)
	return installWith(ctx, argv[:len(argv)-len(command)], command, false, 300*time.Second), remedy
}
