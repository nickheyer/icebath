package pty

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Interactive shell a terminal spawns with its flags and display name
type ShellSpec struct {
	Args    []string
	Command string
	Name    string
}

// LC_CTYPE of a terminal, the user value, else UTF-8 on macOS and LANG or C.UTF-8 elsewhere
func TerminalLcCtype(env map[string]string, platform string) string {
	if env["LC_CTYPE"] != "" {
		return env["LC_CTYPE"]
	}
	if platform == "darwin" {
		return "UTF-8"
	}
	if env["LANG"] != "" {
		return env["LANG"]
	}
	return "C.UTF-8"
}

// Whether path is absolute and executable by this user
func isExecutableFile(path string) bool {
	if path == "" || !filepath.IsAbs(path) {
		return false
	}
	return accessExecutable(path)
}

// Login interactive flags for zsh and bash, interactive only for every other POSIX shell
func posixShellSpec(shellPath string) ShellSpec {
	name := filepath.Base(shellPath)
	args := []string{"-i"}
	if strings.Contains(name, "zsh") || strings.Contains(name, "bash") {
		args = []string{"-il"}
	}
	return ShellSpec{Args: args, Command: shellPath, Name: name}
}

// Spawn spec of a shell path by family, PowerShell without its logo, cmd bare, the rest POSIX interactive
func shellSpecFor(shellPath string) ShellSpec {
	name := strings.ToLower(filepath.Base(shellPath))
	if strings.HasPrefix(name, "pwsh") || strings.HasPrefix(name, "powershell") {
		return ShellSpec{Args: []string{"-NoLogo"}, Command: shellPath, Name: name}
	}
	if strings.HasPrefix(name, "cmd") {
		return ShellSpec{Args: []string{}, Command: shellPath, Name: name}
	}
	return posixShellSpec(shellPath)
}

// Windows PowerShell 5.1 at its System32 path, else powershell.exe on PATH
func (s *Service) windowsPowerShellPath() string {
	systemRoot := os.Getenv("SystemRoot")
	if systemRoot == "" {
		systemRoot = os.Getenv("windir")
	}
	if systemRoot == "" {
		systemRoot = `C:\Windows`
	}
	builtin := filepath.Join(systemRoot, "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
	if isExecutableFile(builtin) {
		return builtin
	}
	return s.findOnPath("powershell.exe")
}

// PowerShell 7, then Windows PowerShell 5.1, then COMSPEC or cmd.exe
func (s *Service) windowsShellSpec() ShellSpec {
	command := s.findOnPath("pwsh.exe")
	if command == "" {
		command = s.findOnPath("pwsh")
	}
	if command == "" {
		command = s.windowsPowerShellPath()
	}
	if command == "" {
		command = os.Getenv("COMSPEC")
	}
	if command == "" {
		command = "cmd.exe"
	}
	return shellSpecFor(command)
}

// Interactive shell of the embedded terminal, ICEBATH_DESKTOP_SHELL then SHELL off Windows, else the best one installed
func (s *Service) terminalShellCommand() ShellSpec {
	override := os.Getenv("ICEBATH_DESKTOP_SHELL")
	if override == "" && !s.opts.IsWindows {
		override = os.Getenv("SHELL")
	}
	override = jsTrim(override)
	if override != "" {
		resolved := override
		if !isExecutableFile(override) {
			resolved = s.findOnPath(override)
		}
		if resolved != "" {
			return shellSpecFor(resolved)
		}
	}
	if s.opts.IsWindows {
		return s.windowsShellSpec()
	}
	for _, candidate := range []string{"/bin/zsh", "/bin/bash", "/bin/sh"} {
		if isExecutableFile(candidate) {
			return posixShellSpec(candidate)
		}
	}
	return posixShellSpec("/bin/sh")
}

// Requested directory when it exists, its parent for a file, the home directory when it cannot be read
func (s *Service) safeTerminalCwd(cwd any) string {
	raw := s.opts.HomeDir()
	if jsTruthy(cwd) {
		raw = jsString(cwd)
	}
	candidate, err := filepath.Abs(raw)
	if err != nil {
		return s.opts.HomeDir()
	}
	info, err := os.Stat(candidate)
	if err != nil {
		return s.opts.HomeDir()
	}
	if info.IsDir() {
		return candidate
	}
	return filepath.Dir(candidate)
}

// MSYS path conversion off for Git Bash children on Windows unless the user set it
func applyWindowsMsysBashEnvDefaults(env map[string]string, isWindows bool) map[string]string {
	if !isWindows {
		return env
	}
	if _, ok := env["MSYS_NO_PATHCONV"]; !ok {
		env["MSYS_NO_PATHCONV"] = "1"
	}
	if _, ok := env["MSYS2_ARG_CONV_EXCL"]; !ok {
		env["MSYS2_ARG_CONV_EXCL"] = "*"
	}
	return env
}

// Process environment without npm and color detection leaks, set up as a truecolor xterm inside the desktop
func (s *Service) terminalShellEnv() map[string]string {
	env := map[string]string{}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok && k != "" {
			env[k] = v
		}
	}
	for key := range env {
		if key == "npm_config_prefix" || strings.HasPrefix(key, "npm_config_") || strings.HasPrefix(key, "npm_package_") {
			delete(env, key)
		}
	}
	delete(env, "NO_COLOR")
	delete(env, "FORCE_COLOR")
	delete(env, "COLORFGBG")
	env["COLORTERM"] = "truecolor"
	env["LC_CTYPE"] = TerminalLcCtype(env, s.opts.Platform)
	env["TERM"] = "xterm-256color"
	env["TERM_PROGRAM"] = "Icebath"
	env["TERM_PROGRAM_VERSION"] = s.opts.AppVersion()
	env["ICEBATH_DESKTOP_TERMINAL"] = "1"
	return applyWindowsMsysBashEnvDefaults(env, s.opts.IsWindows)
}

var wslKernelRelease = regexp.MustCompile(`(?i)microsoft|wsl`)

// Whether this Linux host is WSL, by environment or kernel release
func isWslEnvironment(platform string) bool {
	if platform != "linux" {
		return false
	}
	if os.Getenv("WSL_DISTRO_NAME") != "" || os.Getenv("WSL_INTEROP") != "" {
		return true
	}
	release, err := os.ReadFile("/proc/sys/kernel/osrelease")
	return err == nil && wslKernelRelease.Match(release)
}

// Whether a path names a Windows program a WSL shell cannot run natively
func isWindowsBinaryPathInWsl(path string, isWsl bool) bool {
	if !isWsl {
		return false
	}
	normalized := strings.ToLower(strings.ReplaceAll(path, `\`, "/"))
	for _, ext := range []string{".exe", ".cmd", ".bat", ".ps1"} {
		if strings.HasSuffix(normalized, ext) {
			return true
		}
	}
	return false
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// Extensions tried on PATH, PATHEXT before the bare name on Windows and only the bare name elsewhere
func buildPathExtCandidates(pathext string, isWindows bool) []string {
	if !isWindows {
		return []string{""}
	}
	if pathext == "" {
		pathext = ".COM;.EXE;.BAT;.CMD"
	}
	var out []string
	for _, ext := range strings.Split(pathext, ";") {
		if ext != "" {
			out = append(out, ext)
		}
	}
	return append(out, "")
}

// Existing explicit path or the first PATH hit of a command, empty when neither resolves
func (s *Service) findOnPath(command string) string {
	if command == "" {
		return ""
	}
	if filepath.IsAbs(command) || strings.ContainsRune(command, os.PathSeparator) || (s.opts.IsWindows && strings.Contains(command, "/")) {
		if !fileExists(command) || isWindowsBinaryPathInWsl(command, s.isWsl) {
			return ""
		}
		return command
	}
	extensions := buildPathExtCandidates(os.Getenv("PATHEXT"), s.opts.IsWindows)
	for _, entry := range filepath.SplitList(os.Getenv("PATH")) {
		if entry == "" {
			continue
		}
		for _, ext := range extensions {
			if candidate := filepath.Join(entry, command+ext); fileExists(candidate) {
				return candidate
			}
		}
	}
	return ""
}

// Live working directory of a shell, from /proc on Linux and lsof on macOS, nil elsewhere or when unreadable
func readProcessCwd(platform string, pid int) *string {
	if pid <= 0 {
		return nil
	}
	switch platform {
	case "linux":
		target, err := os.Readlink("/proc/" + strconv.Itoa(pid) + "/cwd")
		if err != nil || target == "" {
			return nil
		}
		return &target
	case "darwin":
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, "lsof", "-a", "-p", strconv.Itoa(pid), "-d", "cwd", "-Fn").Output()
		if err != nil {
			return nil
		}
		for _, line := range strings.Split(string(out), "\n") {
			if strings.HasPrefix(line, "n") {
				cwd := line[1:]
				return &cwd
			}
		}
		return nil
	}
	return nil
}
