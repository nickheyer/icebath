package dashboard

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"regexp"
	"runtime"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

const sandboxCopyMissing = "File is no longer in this session's workspace"

const osReadable = 4

var workspaceKeyRE = regexp.MustCompile(`^[0-9a-f]{16}$`)

// Tools env of the context profile and its bound terminal scope
func filesToolsEnv(ctx context.Context) *tools.Env {
	return &tools.Env{Home: home.Dir(ctx), TerminalScope: tools.GetTerminalScope(ctx)}
}

// Digest and host cwd of the session per workspace Docker sandbox, both empty when it has none
func sessionWorkspace(ctx context.Context, sessionID, cwd string) (string, string, error) {
	overrides, err := tools.ResolveTaskOverrides(filesToolsEnv(ctx), sessionID)
	if err != nil {
		return "", "", err
	}
	if key, ok := overrides["workspace_key"].(string); ok && workspaceKeyRE.MatchString(key) {
		bound, _ := overrides["cwd"].(string)
		return key, bound, nil
	}
	host := pystr.Strip(cwd)
	if host == "" || strings.Contains(host, "\x00") || isContainerCwd(host) {
		return "", "", nil
	}
	expanded := pypath.Expanduser(host)
	if fi, err := os.Stat(expanded); err != nil || !fi.IsDir() {
		return "", "", nil
	}
	return tools.WorkspaceKeyFor(expanded), expanded, nil
}

// Whether a cwd is the container workspace or root rather than a host folder
func isContainerCwd(host string) bool {
	for _, root := range []string{"/workspace", "/root"} {
		if host == root || strings.HasPrefix(host, root+"/") {
			return true
		}
	}
	return false
}

// One translation of a container path under the bound scope, 409 for a Save found only in a fallback sandbox
func translateAgentPath(ctx context.Context, candidate, workspaceKey string, ownSandboxOnly bool, workspaceCwd string) (string, bool, error) {
	active, err := gateway.DockerEnvActive(ctx)
	if err != nil || !active {
		return "", false, err
	}
	translated, ok, err := gateway.TranslateDockerContainerMediaPath(ctx, candidate, workspaceKey, true, workspaceCwd)
	if err != nil || !ok {
		return "", false, err
	}
	if ownSandboxOnly {
		fallback, err := inFallbackSandbox(ctx, translated, workspaceKey)
		if err != nil {
			return "", false, err
		}
		if !fallback {
			if fallback, err = inSharedCwdMountGuess(ctx, translated, workspaceKey); err != nil {
				return "", false, err
			}
		}
		if fallback {
			return "", false, NewHTTPException(409, sandboxCopyMissing)
		}
	}
	return translated, true, nil
}

// Whether target sits in a Docker sandbox other than the session own first candidate
func inFallbackSandbox(ctx context.Context, target, workspaceKey string) (bool, error) {
	root, err := gateway.DockerSandboxRoot(ctx)
	if err != nil {
		return false, err
	}
	names, err := gateway.DockerSandboxDirCandidates(ctx, "", workspaceKey)
	if err != nil || len(names) < 2 {
		return false, err
	}
	base := pypath.Join(root, "docker")
	for _, name := range names[1:] {
		if pathIsUnder(resolvedOrSelf(pypath.Join(base, name)), target) {
			return true, nil
		}
	}
	return false, nil
}

// Whether target sits in the cwd a shared container workspace is only assumed to mount
func inSharedCwdMountGuess(ctx context.Context, target, workspaceKey string) (bool, error) {
	roots, err := gateway.SharedCwdMountGuess(ctx, workspaceKey)
	if err != nil {
		return false, err
	}
	for _, root := range roots {
		if pathIsUnder(root, target) {
			return true, nil
		}
	}
	return false, nil
}

// Host bind mount of an agent container path, false off Docker or on a failed translation
func agentContainerPath(ctx context.Context, raw, profileHome, workspaceKey string, ownSandboxOnly bool, workspaceCwd string) (string, bool, error) {
	if !strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "//") || slices.Contains(strings.Split(raw, "/"), "..") {
		return "", false, nil
	}
	translated, ok, err := func() (string, bool, error) {
		scoped := tuigateway.AgentPathScope(ctx, profileHome)
		translated, ok, err := translateAgentPath(scoped, raw, workspaceKey, ownSandboxOnly, workspaceCwd)
		if err != nil || ok || tools.GetTerminalScope(scoped) != nil {
			return translated, ok, err
		}
		retry := tools.WithProfileTerminalScope(scoped, home.Dir(scoped), nil)
		return translateAgentPath(retry, raw, workspaceKey, ownSandboxOnly, workspaceCwd)
	}()
	if err != nil {
		var httpErr *HTTPException
		if errors.As(err, &httpErr) {
			return "", false, err
		}
		slog.Debug("agent container path translation failed", "path", raw, "error", err)
		return "", false, nil
	}
	return translated, ok, nil
}

// Host path of an agent facing path, container paths mapped to their host mount
func agentFsPath(ctx context.Context, rawPath string, cwd *string, profileHome, workspaceKey string, decodeFallback, ownSandboxOnly bool, workspaceCwd string) (string, error) {
	raw := pystr.Strip(rawPath)
	if runtime.GOOS != "windows" {
		host, herr := tuigateway.FsPath(rawPath, cwd, decodeFallback)
		if herr != nil {
			return "", NewHTTPException(herr.Status, herr.Detail)
		}
		if pyfs.Access(host, osReadable) {
			return host, nil
		}
		translated, ok, err := agentContainerPath(ctx, fileURLContainerText(raw), profileHome, workspaceKey, ownSandboxOnly, workspaceCwd)
		if err != nil {
			return "", err
		}
		if ok {
			return translated, nil
		}
		return host, nil
	}
	translated, ok, err := agentContainerPath(ctx, raw, profileHome, workspaceKey, ownSandboxOnly, workspaceCwd)
	if err != nil {
		return "", err
	}
	if ok {
		return translated, nil
	}
	if tuigateway.IsUnmappedContainerPath(ctx, raw, profileHome) {
		return "", NewHTTPException(404, tuigateway.SandboxNotFound)
	}
	host, herr := tuigateway.FsPath(rawPath, cwd, decodeFallback)
	if herr != nil {
		return "", NewHTTPException(herr.Status, herr.Detail)
	}
	return host, nil
}

// Path a POSIX gateway file URL names so a container path spelled as a URL translates like the bare path
func fileURLContainerText(raw string) string {
	if runtime.GOOS == "windows" || !strings.HasPrefix(strings.ToLower(raw), "file:") {
		return raw
	}
	p, err := tuigateway.FileURLPath(raw)
	if err != nil {
		return raw
	}
	return p
}
