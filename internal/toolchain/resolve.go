package toolchain

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/internal/home"
)

// Where a resolved program came from
type Origin string

// The explicit override, the store, a copy already on the machine, and a fresh install
const (
	FromOverride Origin = "override"
	FromStore    Origin = "store"
	FromSystem   Origin = "system"
	FromInstall  Origin = "install"
)

// How Resolve looks for the program of an entry
type Lookup struct {
	// Path or command name the user configured, the only answer when set
	Override string
	// Command names a copy on the search path may carry, none forbidding a system copy
	System []string
	// Lets a system copy win over the store copy
	SystemFirst bool
	// Extra test a system copy must pass
	Accept func(ctx context.Context, path string) bool
	// Installs the pin when nothing usable is found
	Install bool
	// Policy and progress of that install
	Ensure EnsureOptions
}

// Program Resolve settled on
type Resolved struct {
	Path   string
	Origin Origin
	// Store install behind the path, nil for an override or a system copy
	Installation *Installation
}

// Reports a value written as a path rather than a bare command name
func hasPathSeparator(value string) bool {
	return strings.ContainsRune(value, '/') || strings.ContainsRune(value, filepath.Separator)
}

// First acceptable copy on the search path among the command names
func (l Lookup) systemCopy(ctx context.Context) string {
	for _, name := range l.System {
		path, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		if l.Accept == nil || l.Accept(ctx, path) {
			return path
		}
	}
	return ""
}

// Finds the program of name by override, store copy, system copy and install in the order the lookup asks
func (s *Store) Resolve(ctx context.Context, name string, l Lookup) (*Resolved, error) {
	if _, err := s.recipe(name); err != nil {
		return nil, err
	}
	if configured := strings.TrimSpace(l.Override); configured != "" {
		expanded := home.ExpandUser(configured)
		found, err := exec.LookPath(expanded)
		if err != nil {
			return nil, &OverrideError{Name: name, Value: configured}
		}
		if hasPathSeparator(expanded) {
			found = expanded
		}
		return &Resolved{Path: found, Origin: FromOverride}, nil
	}
	if l.SystemFirst {
		if path := l.systemCopy(ctx); path != "" {
			return &Resolved{Path: path, Origin: FromSystem}, nil
		}
	}
	inst, err := s.Installed(ctx, name, false)
	switch {
	case err == nil && inst.Binary != "":
		return &Resolved{Path: inst.Binary, Origin: FromStore, Installation: inst}, nil
	case err != nil && !errors.Is(err, ErrNotInstalled):
		return nil, err
	}
	if !l.SystemFirst {
		if path := l.systemCopy(ctx); path != "" {
			return &Resolved{Path: path, Origin: FromSystem}, nil
		}
	}
	if !l.Install {
		return nil, notInstalled(name)
	}
	inst, err = s.Ensure(ctx, name, l.Ensure)
	if err != nil {
		return nil, err
	}
	if inst.Binary == "" {
		return nil, &InstallError{Name: name, Cause: "install has no usable pinned binary"}
	}
	return &Resolved{Path: inst.Binary, Origin: FromInstall, Installation: inst}, nil
}

// Error naming an entry with no usable copy on this machine
func notInstalled(name string) error {
	return fmt.Errorf("%s: %w", name, ErrNotInstalled)
}

// Search path directories of every installed entry that belongs on it, store order, never installing
func (s *Store) PathDirs(ctx context.Context) ([]string, error) {
	host, err := s.hostTarget(ctx)
	if err != nil {
		return nil, err
	}
	var dirs []string
	for _, name := range s.order {
		r := s.recipes[name]
		if r.inHome() || !r.onPath || r.missing(host.target) != "" {
			continue
		}
		loc, err := s.locate(ctx, r, host, false, false)
		if err != nil {
			return nil, err
		}
		if loc == nil {
			continue
		}
		for _, dir := range loc.facts.env(name, loc.root)["PATH"] {
			if dir != "" && !containsString(dirs, dir) {
				dirs = append(dirs, dir)
			}
		}
	}
	return dirs, nil
}

// Reports whether list holds value
func containsString(list []string, value string) bool {
	for _, v := range list {
		if v == value {
			return true
		}
	}
	return false
}

// Search path value with the store directories moved to the front, for the product's own children
func (s *Store) StoreFirstPath(ctx context.Context, path string) (string, error) {
	dirs, err := s.PathDirs(ctx)
	if err != nil || len(dirs) == 0 {
		return path, err
	}
	return strings.Join(append(dirs, withoutDirs(path, dirs)...), string(os.PathListSeparator)), nil
}

// Search path value with the store directories moved behind the user's own, for commands the user or agent runs
func (s *Store) UserFirstPath(ctx context.Context, path string) (string, error) {
	dirs, err := s.PathDirs(ctx)
	if err != nil || len(dirs) == 0 {
		return path, err
	}
	return strings.Join(append(withoutDirs(path, dirs), dirs...), string(os.PathListSeparator)), nil
}

// Elements of a search path value that are none of dirs, compared without case
func withoutDirs(path string, dirs []string) []string {
	skip := map[string]bool{}
	for _, d := range dirs {
		skip[strings.ToLower(d)] = true
	}
	var rest []string
	for _, p := range filepath.SplitList(path) {
		if p != "" && !skip[strings.ToLower(p)] {
			rest = append(rest, p)
		}
	}
	return rest
}

// Environment base with the recorded variables of the installed names applied, their search path directories prepended
func (s *Store) Environ(ctx context.Context, base []string, names ...string) ([]string, error) {
	host, err := s.hostTarget(ctx)
	if err != nil {
		return nil, err
	}
	env := append([]string(nil), base...)
	var pathDirs []string
	for _, name := range names {
		r := s.recipes[name]
		if r == nil || r.inHome() {
			continue
		}
		loc, err := s.locate(ctx, r, host, false, false)
		if err != nil {
			return nil, err
		}
		if loc == nil {
			continue
		}
		for key, values := range loc.facts.env(name, loc.root) {
			if key == "PATH" {
				for _, dir := range values {
					if !containsString(pathDirs, dir) {
						pathDirs = append(pathDirs, dir)
					}
				}
				continue
			}
			env = setEnv(env, key, strings.Join(values, string(os.PathListSeparator)))
		}
	}
	if len(pathDirs) == 0 {
		return env, nil
	}
	prefix := strings.Join(pathDirs, string(os.PathListSeparator))
	for i, kv := range env {
		key, value, _ := strings.Cut(kv, "=")
		if strings.EqualFold(key, "PATH") {
			if value != "" {
				prefix += string(os.PathListSeparator) + value
			}
			env[i] = key + "=" + prefix
			return env, nil
		}
	}
	return append(env, "PATH="+prefix), nil
}

// Environment with key set to value, replacing an existing assignment
func setEnv(env []string, key, value string) []string {
	for i, kv := range env {
		if k, _, _ := strings.Cut(kv, "="); k == key {
			env[i] = key + "=" + value
			return env
		}
	}
	return append(env, key+"="+value)
}
