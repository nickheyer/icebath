package gateway

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

var workspaceKeyRE = regexp.MustCompile(`^[0-9a-f]{16}$`)

// Tools env of the active home carrying the turn terminal scope bound to ctx
func mediaToolsEnv(ctx context.Context) *tools.Env {
	return &tools.Env{Home: home.Dir(ctx), TerminalScope: tools.GetTerminalScope(ctx)}
}

// Scope aware TERMINAL setting of the active profile, a refused scope returned as the error
func tenv(ctx context.Context, name, def string) (string, error) {
	return tools.TerminalEnvRead(mediaToolsEnv(ctx), name, def)
}

// Whether the terminal mounts the host cwd at the container workspace
func cwdMountEnabled(ctx context.Context) (bool, error) {
	v, err := tenv(ctx, "TERMINAL_DOCKER_MOUNT_CWD_TO_WORKSPACE", "false")
	if err != nil {
		return false, err
	}
	switch strings.ToLower(v) {
	case "true", "1", "yes":
		return true, nil
	}
	return false, nil
}

// Candidate host sandbox directory names of the session, best first
func dockerSandboxDirCandidates(ctx context.Context, sessionKey, workspaceKey string) ([]string, error) {
	base := "default"
	if name := home.ProfileNameFor(home.Dir(ctx)); name != "" && name != "default" {
		base = "profile:" + name
	}
	var candidates []string
	shared, err := tenv(ctx, "TERMINAL_DOCKER_SHARED_CONTAINER_KEY", "")
	if err != nil {
		return nil, err
	}
	if shared = pystr.Strip(shared); shared != "" {
		candidates = append(candidates, tools.SanitizeTaskIDForPath("shared:"+shared))
	} else if workspaceKeyRE.MatchString(workspaceKey) {
		mount, err := cwdMountEnabled(ctx)
		if err != nil {
			return nil, err
		}
		if mount {
			candidates = append(candidates, tools.SanitizeTaskIDForPath(base+":ws:"+workspaceKey))
		}
	}
	if base != "default" {
		candidates = append(candidates, tools.SanitizeTaskIDForPath(base))
	}
	candidates = append(candidates, "default")
	if sessionKey != "" {
		candidates = append(candidates, tools.SanitizeTaskIDForPath("session:"+sessionKey))
	}
	return candidates, nil
}

// Existing sandbox leaf directories of the session candidates, empty unless Docker is persistent
func dockerPersistentSandboxRoots(ctx context.Context, sessionKey, leaf, workspaceKey string) ([]string, error) {
	persistent, err := dockerPersistentActive(ctx)
	if err != nil || !persistent {
		return nil, err
	}
	sandbox, err := tools.SandboxDir(mediaToolsEnv(ctx))
	if err != nil {
		return nil, nil
	}
	names, err := dockerSandboxDirCandidates(ctx, sessionKey, workspaceKey)
	if err != nil {
		return nil, nil
	}
	var roots []string
	for _, name := range names {
		cand, err := pypath.Resolve(filepath.Join(sandbox, "docker", name, leaf))
		if err != nil {
			return nil, nil
		}
		if isDir(cand) {
			roots = append(roots, cand)
		}
	}
	return roots, nil
}

// Whether the path is an existing directory after following symlinks
func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// Whether the path is an existing regular file after following symlinks
func isRegularFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

// Resolved host cwd mounted at the container workspace, empty when it is not a directory
func mountedWorkspaceCwd(ctx context.Context, workspaceCwd string) ([]string, error) {
	cwd := workspaceCwd
	if cwd == "" {
		v, err := tenv(ctx, "TERMINAL_CWD", "")
		if err != nil {
			return nil, err
		}
		cwd = v
	}
	if cwd == "" {
		wd, err := os.Getwd()
		if err != nil {
			return nil, err
		}
		cwd = wd
	}
	host, err := pypath.Resolve(pypath.Expanduser(cwd))
	if err != nil {
		return nil, nil
	}
	if !isDir(host) {
		return nil, nil
	}
	return []string{host}, nil
}

// Host directory a workspace bound session mounts, empty when its cwd digest names another container
func sessionMountedWorkspace(ctx context.Context, workspaceKey, workspaceCwd string) ([]string, error) {
	cwd := pystr.Strip(workspaceCwd)
	if cwd == "" || strings.ContainsRune(cwd, 0) || !pypath.IsAbs(pypath.Expanduser(cwd)) {
		return nil, nil
	}
	if tools.WorkspaceKeyFor(cwd) != workspaceKey {
		return nil, nil
	}
	return mountedWorkspaceCwd(ctx, cwd)
}

// Profile cwd standing in for a shared container workspace, empty unless the shared key and cwd mount apply
func sharedCwdMountGuess(ctx context.Context, workspaceKey string) ([]string, error) {
	persistent, err := dockerPersistentActive(ctx)
	if err != nil || !persistent || !workspaceKeyRE.MatchString(workspaceKey) {
		return nil, err
	}
	mount, err := cwdMountEnabled(ctx)
	if err != nil || !mount {
		return nil, err
	}
	shared, err := tenv(ctx, "TERMINAL_DOCKER_SHARED_CONTAINER_KEY", "")
	if err != nil || pystr.Strip(shared) == "" {
		return nil, err
	}
	return mountedWorkspaceCwd(ctx, "")
}

// Host directories standing in for the container workspace, cwd mount first then the session sandbox
func defaultDockerWorkspaceHostRoots(ctx context.Context, sessionKey, workspaceKey, workspaceCwd string) ([]string, error) {
	persistent, err := dockerPersistentActive(ctx)
	if err != nil || !persistent {
		return nil, err
	}
	var roots []string
	mount, err := cwdMountEnabled(ctx)
	if err != nil {
		return nil, err
	}
	if mount {
		if !workspaceKeyRE.MatchString(workspaceKey) {
			return mountedWorkspaceCwd(ctx, "")
		}
		shared, err := tenv(ctx, "TERMINAL_DOCKER_SHARED_CONTAINER_KEY", "")
		if err != nil {
			return nil, err
		}
		var more []string
		if pystr.Strip(shared) != "" {
			more, err = sharedCwdMountGuess(ctx, workspaceKey)
		} else {
			more, err = sessionMountedWorkspace(ctx, workspaceKey, workspaceCwd)
		}
		if err != nil {
			return nil, err
		}
		roots = append(roots, more...)
	}
	sandboxes, err := dockerPersistentSandboxRoots(ctx, sessionKey, "workspace", workspaceKey)
	if err != nil {
		return nil, err
	}
	for _, root := range sandboxes {
		if !containsPath(roots, root) {
			roots = append(roots, root)
		}
	}
	return roots, nil
}

// Whether the list holds a path equal to p under pathlib comparison
func containsPath(list []string, p string) bool {
	for _, q := range list {
		if samePath(q, p) {
			return true
		}
	}
	return false
}

// Whether two paths compare equal as pathlib paths
func samePath(a, b string) bool {
	rel, ok := pypath.PathRelativeTo(a, b)
	return ok && rel == "."
}

// Whether path sits at or under root component wise
func pathIsWithin(path, root string) bool {
	_, ok := pypath.PathRelativeTo(path, root)
	return ok
}
