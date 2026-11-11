package gateway

import "context"

// Stopped manual backend a historical updater recorded, its pid, argv and home
type RespawnCandidate struct {
	Pid  int
	Argv []string
	Home string
}

// Argvs to respawn after an update, no port zero or foreign home backends, one per command line and profile
func FilterDashboardRespawnCandidates(ctx context.Context, candidates []RespawnCandidate, ownHome *string) [][]string {
	converted := make([]respawnCandidate, 0, len(candidates))
	for _, c := range candidates {
		converted = append(converted, respawnCandidate{pid: c.Pid, argv: c.Argv, home: c.Home})
	}
	return filterDashboardRespawnCandidates(ctx, converted, ownHome)
}
