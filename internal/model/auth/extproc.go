package auth

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/fileutil"
	"github.com/nickheyer/icebath/internal/home"
)

var claudeCodePrefixes = []string{
	"~/.local/bin", "~/.claude/local", "~/bin", "~/.npm-global/bin", "~/.bun/bin",
	"~/.volta/bin", "/opt/homebrew/bin", "/usr/local/bin",
}

var claudeCodeNames = []string{"claude", "claude-code"}

var percentVar = regexp.MustCompile(`%([^%]+)%`)

var dollarVar = regexp.MustCompile(`\$(\w+|\{[^}]*\})`)

func (s *Store) expandPath(ctx context.Context, path string) string {
	expanded := home.ExpandUser(path)
	expanded = dollarVar.ReplaceAllStringFunc(expanded, func(m string) string {
		name := strings.TrimPrefix(m, "$")
		if strings.HasPrefix(name, "{") && strings.HasSuffix(name, "}") {
			name = name[1 : len(name)-1]
		}
		if v := s.lookupEnv(ctx, name); v != "" {
			return v
		}
		return m
	})
	if runtime.GOOS == "windows" {
		expanded = percentVar.ReplaceAllStringFunc(expanded, func(m string) string {
			if v := s.lookupEnv(ctx, strings.Trim(m, "%")); v != "" {
				return v
			}
			return m
		})
	}
	return expanded
}

func isExecutableFile(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular() && fileutil.Accessible(path, fileutil.AccessExec)
}

func hasPathSeparator(name string) bool {
	return strings.ContainsRune(name, os.PathSeparator) || (runtime.GOOS == "windows" && strings.Contains(name, "/"))
}

func (s *Store) knownDirVariants(ctx context.Context, base string) []string {
	if runtime.GOOS != "windows" {
		return []string{base}
	}
	pathext := s.lookupEnv(ctx, "PATHEXT")
	if pathext == "" {
		pathext = ".EXE;.CMD;.BAT"
	}
	out := []string{base}
	for _, ext := range strings.Split(pathext, string(os.PathListSeparator)) {
		if ext != "" {
			out = append(out, base+ext)
		}
	}
	return out
}

func (s *Store) locateCommand(ctx context.Context, name string, knownDirs []string) (string, bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", false
	}
	if hasPathSeparator(name) {
		expanded := s.expandPath(ctx, name)
		if filepath.IsAbs(expanded) && isExecutableFile(expanded) {
			return expanded, true
		}
		return "", false
	}
	if hit, err := exec.LookPath(name); err == nil {
		return hit, true
	}
	for _, dir := range knownDirs {
		base := filepath.Join(s.expandPath(ctx, dir), name)
		for _, variant := range s.knownDirVariants(ctx, base) {
			if isExecutableFile(variant) {
				return variant, true
			}
		}
	}
	return "", false
}

func (s *Store) findClaudeCodeCLI(ctx context.Context, command string) (string, bool) {
	if !slices.Contains(claudeCodeNames, command) {
		return "", false
	}
	return s.locateCommand(ctx, command, claudeCodePrefixes)
}

func (s *Store) ResolveExternalProcessCommand(ctx context.Context, command string) (string, bool) {
	ctx = s.bind(ctx)
	if command == "" {
		return "", false
	}
	lookup := command
	if hasPathSeparator(command) {
		if abs, err := filepath.Abs(s.expandPath(ctx, command)); err == nil {
			lookup = abs
		}
	}
	if found, ok := s.locateCommand(ctx, lookup, []string{"~/.local/bin"}); ok {
		return filepath.Clean(found), true
	}
	return s.findClaudeCodeCLI(ctx, command)
}

func (s *Store) ExternalProcessLaunchEnvNames(ctx context.Context) []string {
	names := []string{}
	for _, spec := range s.dir.Specs(s.bind(ctx)) {
		if spec.AuthType != AuthTypeExternalProcess {
			continue
		}
		for _, name := range append(slices.Clone(spec.ProcessCommandEnv), spec.ProcessArgsEnv) {
			if name != "" && !slices.Contains(names, name) {
				names = append(names, name)
			}
		}
	}
	return names
}

func (s *Store) EnvWriterLaunchDenylist(ctx context.Context) []string {
	names := s.ExternalProcessLaunchEnvNames(ctx)
	for _, name := range config.LaunchEnvNames() {
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	return names
}
