package cron

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/pm"
)

// Resolved site packages of the interpreter this process runs as, false when it cannot be read
var installedPurelib = func() (string, bool) {
	exe, err := os.Executable()
	if err != nil {
		return "", false
	}
	prefix, err := pypath.Resolve(filepath.Dir(exe))
	if err != nil {
		return "", false
	}
	site, err := pm.SitePackages(prefix)
	if err != nil {
		return "", false
	}
	resolved, err := pypath.Resolve(site)
	if err != nil {
		return "", false
	}
	return resolved, true
}

// Site packages of the generation PM committed for this install, empty when none is committed or present
func committedDependencySitePackages(s *Store, ctx context.Context, projectRoot string) (string, error) {
	environment, err := pm.CommittedVenv(ctx, projectRoot)
	if err != nil {
		s.log.Warn(fmt.Sprintf("cron worker: could not read the committed dependency environment: %s", err))
		return "", nil
	}
	if environment == "" {
		return "", nil
	}
	selected, err := pm.SitePackages(environment)
	if err != nil {
		return "", err
	}
	if !isDir(selected) {
		return "", nil
	}
	return selected, nil
}

// Prepends the tree and the committed generation to the worker env's own PYTHONPATH unless the tree is purelib
func pinHermesTreeOnPythonpath(s *Store, ctx context.Context, workerEnv map[string]string, repoRoot string) (map[string]string, error) {
	root := repoRoot
	purelib, ok := installedPurelib()
	resolved, err := pypath.Resolve(root)
	if err != nil {
		return nil, err
	}
	if ok && purelib == resolved {
		return workerEnv, nil
	}
	var existing []string
	for _, entry := range strings.Split(workerEnv["PYTHONPATH"], string(os.PathListSeparator)) {
		if entry != "" {
			existing = append(existing, entry)
		}
	}
	dependency, err := committedDependencySitePackages(s, ctx, root)
	if err != nil {
		return nil, err
	}
	pinned := []string{root}
	if dependency != "" {
		pinned = append(pinned, dependency)
	}
	seen := map[string]bool{}
	var ordered []string
	for _, entry := range append(pinned, existing...) {
		if !seen[entry] {
			seen[entry] = true
			ordered = append(ordered, entry)
		}
	}
	workerEnv["PYTHONPATH"] = strings.Join(ordered, string(os.PathListSeparator))
	return workerEnv, nil
}
