package install

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

// Comment line the installer writes above each PATH line it appends
const ShellPathMarker = "# Icebath Agent command"

// Guarded prepend of the user bin that a startup file sourced twice still adds once
const shellPathLine = `case ":$PATH:" in *":$HOME/.local/bin:"*) ;; *) export PATH="$HOME/.local/bin:$PATH" ;; esac`

const fishPathLine = `fish_add_path "$HOME/.local/bin"`

var (
	shellPathSetupRE = regexp.MustCompile(`(?m)^[[:space:]]*([^#[:space:]].*)?PATH=.*\.local/bin`)
	fishPathSetupRE  = regexp.MustCompile(`(?m)^[[:space:]]*fish_add_path.*\.local/bin`)
)

// Appends the PATH line under its marker unless the file already sets up the user bin
func appendShellPath(rc, line string, pattern *regexp.Regexp) (bool, error) {
	if data, err := os.ReadFile(rc); err == nil && pattern.Match(data) {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(rc), 0o777); err != nil {
		return false, err
	}
	f, err := os.OpenFile(rc, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return false, err
	}
	if _, err := fmt.Fprintf(f, "\n%s\n%s\n", ShellPathMarker, line); err != nil {
		f.Close()
		return false, err
	}
	return true, f.Close()
}

// Login shell startup files that get the user bin PATH line, with each line and its setup pattern
func shellPathTargets(userHome, shell string) []struct {
	rc      string
	line    string
	pattern *regexp.Regexp
	ifExist bool
} {
	type target = struct {
		rc      string
		line    string
		pattern *regexp.Regexp
		ifExist bool
	}
	switch filepath.Base(shell) {
	case "zsh":
		return []target{{filepath.Join(userHome, ".zshrc"), shellPathLine, shellPathSetupRE, false}, {filepath.Join(userHome, ".zprofile"), shellPathLine, shellPathSetupRE, false}}
	case "fish":
		return []target{{filepath.Join(userHome, ".config", "fish", "config.fish"), fishPathLine, fishPathSetupRE, false}}
	}
	return []target{
		{filepath.Join(userHome, ".bashrc"), shellPathLine, shellPathSetupRE, false},
		{filepath.Join(userHome, ".profile"), shellPathLine, shellPathSetupRE, false},
		{filepath.Join(userHome, ".bash_profile"), shellPathLine, shellPathSetupRE, true},
		{filepath.Join(userHome, ".bash_login"), shellPathLine, shellPathSetupRE, true},
	}
}

// Puts the user bin on PATH in the login shell's startup files, Windows registering it through command publication
func WireShellPath(userHome, shell string, report func(string)) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	if shell == "" {
		shell = "/bin/bash"
	}
	for _, t := range shellPathTargets(userHome, shell) {
		if t.ifExist {
			if _, err := os.Stat(t.rc); err != nil {
				continue
			}
		}
		added, err := appendShellPath(t.rc, t.line, t.pattern)
		if err != nil {
			return fmt.Errorf("cannot update PATH in %s", t.rc)
		}
		if added {
			report("added ~/.local/bin to PATH in " + t.rc)
		}
	}
	return nil
}

func (r *Runner) wireShellPath() error {
	userHome, err := os.UserHomeDir()
	if err != nil {
		return fail("cannot locate the user home for PATH setup: "+err.Error(), "filesystem_error")
	}
	if err := WireShellPath(userHome, os.Getenv("SHELL"), r.logSuccess); err != nil {
		return fail(err.Error(), "filesystem_error")
	}
	return nil
}

// Removes every marker and PATH line pair the installer appended, reporting the files it rewrote
func UnwireShellPath(userHome string) ([]string, error) {
	var changed []string
	seen := map[string]bool{}
	for _, shell := range []string{"zsh", "fish", "bash"} {
		for _, t := range shellPathTargets(userHome, shell) {
			if seen[t.rc] {
				continue
			}
			seen[t.rc] = true
			data, err := os.ReadFile(t.rc)
			if err != nil {
				continue
			}
			block := "\n" + ShellPathMarker + "\n" + t.line + "\n"
			text := string(data)
			if !strings.Contains(text, block) {
				continue
			}
			info, err := os.Stat(t.rc)
			if err != nil {
				return changed, err
			}
			out := strings.ReplaceAll(text, block, "")
			tmp := t.rc + ".icebath-tmp"
			if err := os.WriteFile(tmp, []byte(out), info.Mode().Perm()); err != nil {
				os.Remove(tmp)
				return changed, err
			}
			target := t.rc
			if resolved, err := filepath.EvalSymlinks(t.rc); err == nil {
				target = resolved
			}
			if err := os.Rename(tmp, target); err != nil {
				os.Remove(tmp)
				return changed, err
			}
			changed = append(changed, t.rc)
		}
	}
	return changed, nil
}
