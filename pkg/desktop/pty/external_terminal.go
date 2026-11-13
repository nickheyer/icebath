package pty

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/desktop/connections"
	"github.com/nickheyer/icebath/pkg/desktop/hardening"
)

// Argv resuming a session in the TUI, pinned to the profile when it is a valid profile id
func TuiResumeArgs(sessionID, profile string) []string {
	var head []string
	if pinned := connections.BackendProfileArg(profile); pinned != nil {
		head = []string{"--profile", *pinned}
	}
	return append(head, "--tui", "--resume", sessionID)
}

// Single quoted value for the POSIX launcher script
func PosixQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

// Double quoted value for a cmd.exe script line
func WindowsQuote(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
}

// One variable the launcher script exports, in export order
type EnvPair struct {
	Key   string
	Value string
}

// Backend environment the launcher exports without PATH and empty values, ICEBATH_HOME set to the desktop home
func TerminalScriptEnv(backendEnv map[string]string, icebathHome string) []EnvPair {
	keys := make([]string, 0, len(backendEnv))
	for key := range backendEnv {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var out []EnvPair
	for _, key := range keys {
		if strings.ToUpper(key) == "PATH" || backendEnv[key] == "" {
			continue
		}
		out = append(out, EnvPair{Key: key, Value: backendEnv[key]})
	}
	if icebathHome != "" {
		for i := range out {
			if out[i].Key == "ICEBATH_HOME" {
				out[i].Value = icebathHome
				return out
			}
		}
		out = append(out, EnvPair{Key: "ICEBATH_HOME", Value: icebathHome})
	}
	return out
}

// What the launcher script runs, where and with which exports
type TerminalScriptSpec struct {
	Command  string
	Args     []string
	Cwd      string
	Env      []EnvPair
	Platform string
}

// Launcher script text, a cmd script on Windows and an exec of the TUI under /bin/sh elsewhere
func BuildTerminalScript(spec TerminalScriptSpec) string {
	argv := append([]string{spec.Command}, spec.Args...)
	if spec.Platform == "win32" {
		lines := []string{"@echo off", "cd /d " + WindowsQuote(spec.Cwd)}
		for _, e := range spec.Env {
			lines = append(lines, "set "+WindowsQuote(e.Key+"="+e.Value))
		}
		quoted := make([]string, len(argv))
		for i, a := range argv {
			quoted[i] = WindowsQuote(a)
		}
		lines = append(lines, strings.Join(quoted, " "), "")
		return strings.Join(lines, "\r\n")
	}
	lines := []string{"#!/bin/sh", "cd " + PosixQuote(spec.Cwd) + " || exit 1"}
	for _, e := range spec.Env {
		lines = append(lines, "export "+e.Key+"="+PosixQuote(e.Value))
	}
	quoted := make([]string, len(argv))
	for i, a := range argv {
		quoted[i] = PosixQuote(a)
	}
	lines = append(lines, "exec "+strings.Join(quoted, " "), "")
	return strings.Join(lines, "\n")
}

// Launcher extension, .cmd on Windows, .command on macOS and .sh elsewhere
func TerminalScriptExtension(platform string) string {
	switch platform {
	case "win32":
		return ".cmd"
	case "darwin":
		return ".command"
	}
	return ".sh"
}

// Linux emulators in resolution order with the flag that precedes the program
var linuxTerminals = []struct{ command, flag string }{
	{"x-terminal-emulator", "-e"},
	{"gnome-terminal", "--"},
	{"konsole", "-e"},
	{"xfce4-terminal", "-x"},
	{"tilix", "-e"},
	{"kitty", ""},
	{"alacritty", "-e"},
	{"wezterm", "-e"},
	{"foot", ""},
	{"xterm", "-e"},
}

// Program and argv that open a script in a terminal window
type TerminalLaunch struct {
	Command string
	Args    []string
}

// Argv opening scriptPath in the user terminal, open on macOS, Windows Terminal or cmd, the Linux ladder, nil when none
func ResolveTerminalLaunch(scriptPath string, findOnPath func(string) string, platform string) *TerminalLaunch {
	if platform == "darwin" {
		return &TerminalLaunch{Command: "open", Args: []string{scriptPath}}
	}
	if platform == "win32" {
		if wt := findOnPath("wt.exe"); wt != "" {
			return &TerminalLaunch{Command: wt, Args: []string{"cmd.exe", "/k", scriptPath}}
		}
		return &TerminalLaunch{Command: "cmd.exe", Args: []string{"/c", "start", "", "cmd.exe", "/k", scriptPath}}
	}
	for _, t := range linuxTerminals {
		if resolved := findOnPath(t.command); resolved != "" {
			var args []string
			if t.flag != "" {
				args = append(args, t.flag)
			}
			return &TerminalLaunch{Command: resolved, Args: append(args, "/bin/sh", scriptPath)}
		}
	}
	return nil
}

// Outcome of handing a session to the user terminal
type OpenResult struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// Hands a session to the user terminal emulator running the TUI resumed on it through a launcher script
func (s *Service) OpenSessionInTerminal(sessionID any, opts any) OpenResult {
	id, ok := sessionID.(string)
	if !ok || jsTrim(id) == "" {
		return OpenResult{Error: "invalid-session-id"}
	}
	result, err := s.openSessionInTerminal(id, opts)
	if err != nil {
		s.opts.Log("[terminal] open in terminal failed: " + err.Error())
		return OpenResult{Error: err.Error()}
	}
	return result
}

func (s *Service) openSessionInTerminal(id string, opts any) (OpenResult, error) {
	profile, _ := get(opts, "profile").(string)
	resolved, err := s.opts.Resolve(TuiResumeArgs(jsTrim(id), jsTrim(profile)))
	if err != nil {
		return OpenResult{}, err
	}
	if resolved.Command == "" {
		return OpenResult{Error: "Icebath is not installed yet"}, nil
	}
	cwd := s.opts.SanitizeCwd(get(opts, "cwd"))
	scriptDir := filepath.Join(s.opts.UserData, "open-in-terminal")
	if err := os.MkdirAll(scriptDir, 0o777); err != nil {
		return OpenResult{}, hardening.NodeErr(err)
	}
	suffix := make([]byte, 6)
	if _, err := rand.Read(suffix); err != nil {
		return OpenResult{}, err
	}
	scriptPath := filepath.Join(scriptDir, "icebath-"+hex.EncodeToString(suffix)+TerminalScriptExtension(s.opts.Platform))
	script := BuildTerminalScript(TerminalScriptSpec{Command: resolved.Command, Args: resolved.Args, Cwd: cwd,
		Env: TerminalScriptEnv(resolved.Env, s.opts.IcebathHome), Platform: s.opts.Platform})
	if err := os.WriteFile(scriptPath, []byte(script), 0o700); err != nil {
		return OpenResult{}, hardening.NodeErr(err)
	}
	launch := ResolveTerminalLaunch(scriptPath, s.findOnPath, s.opts.Platform)
	if launch == nil {
		return OpenResult{Error: "No terminal emulator found"}, nil
	}
	s.opts.Log(fmt.Sprintf("[terminal] opening session %s via %s", id, launch.Command))
	cmd := exec.Command(launch.Command, launch.Args...)
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return OpenResult{}, err
	}
	go func() { _ = cmd.Wait() }()
	return OpenResult{OK: true}, nil
}
