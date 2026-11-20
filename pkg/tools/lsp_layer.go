package tools

import (
	"os"
	"time"

	"github.com/nickheyer/icebath/pkg/workspaces"
)

// Seams of the LSP layer, bound by lsp_bridge.go in a build that links it and unset in one without
var (
	lspServiceLookup    func(env *Env) lspService
	lspStaticHandles    func(ext string) bool
	lspInlineBudget     func(started time.Time) time.Duration
	lspDiagnosticsBlock func(path string, diagnostics []map[string]any, header string) string
	lspReleaseWorkspace func(path string) int
)

// Active LSP service of the session's profile, nil when disabled, unbuildable or not linked
func lspServiceFor(env *Env) lspService {
	if lspServiceLookup == nil {
		return nil
	}
	return lspServiceLookup(env)
}

// Whether a built in server claims the extension, none in a build without the LSP layer
func lspStaticRegistryHandles(ext string) bool {
	return lspStaticHandles != nil && lspStaticHandles(ext)
}

// Time a write may still wait for its verdict inline, zero start meaning unknown
func lspInlineWait(started time.Time) time.Duration {
	if lspInlineBudget == nil {
		return 0
	}
	return lspInlineBudget(started)
}

// Worktrees the operator pointed the session at, the launch dir and the surface set workspace, none for scheduled work
func lspOperatorWorkspaceRoots(env *Env) map[string]bool {
	if os.Getenv("ICEBATH_KANBAN_TASK") != "" {
		return map[string]bool{}
	}
	anchors := []func() (string, error){os.Getwd}
	if !isCronSession(env) {
		anchors = append(anchors, func() (string, error) { return ResolveAgentCwd(env) })
	}
	return workspaces.OperatorWorkspaceRoots(anchors)
}

// lsp.trusted_workspaces as directories, a malformed value trusting nothing extra
func lspParseTrustedWorkspaces(raw any) []string {
	roots, _ := workspaces.TrustedWorkspaces(raw)
	return roots
}

// Whether a root lies in a trusted directory or an operator worktree
func lspIsTrustedWorkspace(root string, trusted []string, operatorRoots map[string]bool) bool {
	return workspaces.IsTrustedWorkspace(root, trusted, operatorRoots)
}
