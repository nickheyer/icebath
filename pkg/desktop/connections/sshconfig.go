package connections

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/desktop/git"
)

var (
	sshHostLine    = regexp.MustCompile(`(?i)^host\s+(.+)$`)
	sshIncludeLine = regexp.MustCompile(`(?i)^include\s+(.+)$`)
	whitespace     = regexp.MustCompile(`\s+`)
)

// ParseSSHConfigHosts lists literal Host aliases, dropping wildcard and negated patterns
func ParseSSHConfigHosts(text string) []string {
	hosts := []string{}
	seen := map[string]bool{}
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		m := sshHostLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		for _, pattern := range whitespace.Split(m[1], -1) {
			if pattern == "" || strings.ContainsAny(pattern, "*?") || strings.HasPrefix(pattern, "!") || seen[pattern] {
				continue
			}
			seen[pattern] = true
			hosts = append(hosts, pattern)
		}
	}
	return hosts
}

// ParseSSHConfigIncludes lists the tokens of every Include directive
func ParseSSHConfigIncludes(text string) []string {
	includes := []string{}
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		m := sshIncludeLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		for _, token := range whitespace.Split(m[1], -1) {
			if token != "" {
				includes = append(includes, token)
			}
		}
	}
	return includes
}

// SSHConfigDeps are the reads CollectSSHConfigHosts makes, the real filesystem when unset
type SSHConfigDeps struct {
	ReadFile func(path string) (string, bool)
	HomeDir  string
	GlobSync func(pattern string) []string
}

// CollectSSHConfigHosts walks ~/.ssh/config and its Include files read only, listing literal aliases
func CollectSSHConfigHosts(rootPath string, deps SSHConfigDeps) []string {
	readFile := deps.ReadFile
	if readFile == nil {
		readFile = func(p string) (string, bool) {
			b, err := os.ReadFile(p)
			return string(b), err == nil
		}
	}
	homeDir := deps.HomeDir
	if homeDir == "" {
		homeDir, _ = os.UserHomeDir()
	}
	root := rootPath
	if root == "" {
		root = filepath.Join(homeDir, ".ssh", "config")
	}
	sshDir := filepath.Join(homeDir, ".ssh")
	out := []string{}
	seen := map[string]bool{}
	visited := map[string]bool{}
	resolveInclude := func(token string) string {
		switch {
		case strings.HasPrefix(token, "~/"):
			return filepath.Join(homeDir, token[2:])
		case filepath.IsAbs(token):
			return token
		}
		return filepath.Join(sshDir, token)
	}
	var walk func(string, int)
	walk = func(path string, depth int) {
		if depth > 8 || visited[path] {
			return
		}
		visited[path] = true
		text, ok := readFile(path)
		if !ok {
			return
		}
		for _, host := range ParseSSHConfigHosts(text) {
			if !seen[host] {
				seen[host] = true
				out = append(out, host)
			}
		}
		for _, token := range ParseSSHConfigIncludes(text) {
			target := resolveInclude(token)
			expanded := []string{target}
			if deps.GlobSync != nil {
				expanded = deps.GlobSync(target)
			}
			for _, p := range expanded {
				walk(p, depth+1)
			}
		}
	}
	walk(root, 0)
	return out
}

// SSHResolved is the effective target ssh -G reports for a host alias
type SSHResolved struct {
	Hostname     *string `json:"hostname"`
	User         *string `json:"user"`
	Port         *int    `json:"port"`
	IdentityFile *string `json:"identityFile"`
}

// ParseSSHGOutput reads the first hostname, user, port and identityfile of ssh -G output
func ParseSSHGOutput(text string) SSHResolved {
	var out SSHResolved
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		sp := strings.Index(line, " ")
		if line == "" || sp == -1 {
			continue
		}
		key := strings.ToLower(line[:sp])
		value := strings.TrimSpace(line[sp+1:])
		switch {
		case key == "hostname" && (out.Hostname == nil || *out.Hostname == ""):
			out.Hostname = &value
		case key == "user" && (out.User == nil || *out.User == ""):
			out.User = &value
		case key == "port" && out.Port == nil:
			if n := parseIntPrefix(value); n != 0 {
				out.Port = &n
			}
		case key == "identityfile" && (out.IdentityFile == nil || *out.IdentityFile == ""):
			out.IdentityFile = &value
		}
	}
	return out
}

var leadingInt = regexp.MustCompile(`^[+-]?\d+`)

// Integer prefix the way Number.parseInt reads it, zero when there is none
func parseIntPrefix(s string) int {
	m := leadingInt.FindString(strings.TrimSpace(s))
	n, _ := strconv.Atoi(m)
	return n
}

// WindowsSSHEnv is the Windows environment the ssh client probe is rooted in
type WindowsSSHEnv struct {
	SystemRoot      string
	LocalAppData    string
	ProgramFiles    string
	ProgramFilesX86 string
}

// System32OpenSSH is the in box Windows OpenSSH client path
func System32OpenSSH(systemRoot string) string {
	if systemRoot == "" {
		systemRoot = `C:\Windows`
	}
	return git.WinJoin(systemRoot, "System32", "OpenSSH", "ssh.exe")
}

// GitForWindowsSSHCandidates maps every git install root to its bundled ssh.exe, deduped and in order
func GitForWindowsSSHCandidates(env WindowsSSHEnv, fs git.CandidateFS) []string {
	var out []string
	seen := map[string]bool{}
	for _, gitExe := range git.WindowsGitCandidates(git.WindowsGitEnv{LocalAppData: env.LocalAppData, ProgramFiles: env.ProgramFiles, ProgramFilesX86: env.ProgramFilesX86}, fs) {
		root := git.WinDir(git.WinDir(gitExe))
		if !seen[root] {
			seen[root] = true
			out = append(out, git.WinJoin(root, "usr", "bin", "ssh.exe"))
		}
	}
	return out
}

// ResolveSSHBinary picks the ssh client every desktop spawn uses, bare ssh off Windows
func ResolveSSHBinary(platform, override string, env WindowsSSHEnv, fs git.CandidateFS) string {
	if platform != "win32" {
		return "ssh"
	}
	if explicit := strings.TrimSpace(override); explicit != "" {
		return explicit
	}
	if inbox := System32OpenSSH(env.SystemRoot); fs.Exists(inbox) {
		return inbox
	}
	for _, candidate := range GitForWindowsSSHCandidates(env, fs) {
		if fs.Exists(candidate) {
			return candidate
		}
	}
	return "ssh"
}

// osCandidateFS probes the real filesystem
type osCandidateFS struct{}

func (osCandidateFS) Exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func (osCandidateFS) ReadDir(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}
	return names, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// DesktopSSHBinary resolves the ssh client for this host with the desktop.ssh_path override
func DesktopSSHBinary(platform, override string) string {
	systemRoot := os.Getenv("SystemRoot")
	if systemRoot == "" {
		systemRoot = envOr("windir", `C:\Windows`)
	}
	return ResolveSSHBinary(platform, override, WindowsSSHEnv{
		SystemRoot: systemRoot, LocalAppData: os.Getenv("LOCALAPPDATA"),
		ProgramFiles: envOr("ProgramFiles", `C:\Program Files`), ProgramFilesX86: envOr("ProgramFiles(x86)", `C:\Program Files (x86)`),
	}, osCandidateFS{})
}

// SSHResolveTimeout bounds one ssh -G resolution
const SSHResolveTimeout = 10 * time.Second

// ResolveSSHHost runs ssh -G for an alias and parses the effective target
func ResolveSSHHost(sshBinary string, host any) (SSHResolved, error) {
	value := strings.TrimSpace(jsOrString(host))
	if value == "" {
		return SSHResolved{}, errors.New("SSH host is required.")
	}
	ctx, cancel := context.WithTimeout(context.Background(), SSHResolveTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, sshBinary, "-G", "--", value)
	hideChildWindow(cmd)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return SSHResolved{}, errors.New("SSH config resolution timed out.")
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return SSHResolved{}, errors.New(msg)
		}
		return SSHResolved{}, errors.New("Could not resolve SSH host.")
	}
	if err != nil {
		return SSHResolved{}, err
	}
	return ParseSSHGOutput(stdout.String()), nil
}
