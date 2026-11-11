package tuiserver

import (
	"context"

	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// CompletionCwd is the working directory a completion or new session request resolves to
func CompletionCwd(ctx context.Context, params map[string]any) (string, error) {
	return completionCwd(ctx, params)
}

// RepoDiscoveryPolicy is the effective profile local desktop repo scan policy, from raw when it is a dict
func RepoDiscoveryPolicy(ctx context.Context, raw any) map[string]any {
	return repoDiscoveryPolicy(ctx, raw)
}

// ScanDiscoveredReposRemote walks the policy roots on this host into the discovery cache, true when authoritative
func ScanDiscoveredReposRemote(ctx context.Context, pdb *tools.ProjectsDB, policy map[string]any) bool {
	return scanDiscoveredReposRemote(ctx, pdb, policy)
}

// DiscoverReposPayload merges cached scanned repos with session derived roots, junk filtered, most recent first
func DiscoverReposPayload(ctx context.Context, db *state.SessionDB, pdb *tools.ProjectsDB, backfill, includeCached bool) ([]map[string]any, error) {
	return discoverReposPayload(ctx, db, pdb, backfill, includeCached)
}
