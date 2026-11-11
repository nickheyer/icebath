package gateway

import (
	"context"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Whether the terminal backend of the context profile is Docker
func DockerEnvActive(ctx context.Context) (bool, error) { return dockerEnvActive(ctx) }

// Host path of a container absolute path through the Docker mounts, false when nothing maps it
func TranslateDockerContainerMediaPath(ctx context.Context, candidate, workspaceKey string, quiet bool, workspaceCwd string) (string, bool, error) {
	return translateDockerContainerMediaPath(ctx, candidate, "", workspaceKey, quiet, workspaceCwd)
}

// Candidate host sandbox directory names of a session, best first
func DockerSandboxDirCandidates(ctx context.Context, sessionKey, workspaceKey string) ([]string, error) {
	return dockerSandboxDirCandidates(ctx, sessionKey, workspaceKey)
}

// Profile cwd a shared container workspace is only assumed to mount
func SharedCwdMountGuess(ctx context.Context, workspaceKey string) ([]string, error) {
	return sharedCwdMountGuess(ctx, workspaceKey)
}

// Sandbox root of the context profile terminal policy
func DockerSandboxRoot(ctx context.Context) (string, error) {
	return tools.SandboxDir(mediaToolsEnv(ctx))
}
