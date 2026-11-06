package agent

import (
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

type codexSessionStore struct{ db *state.DB }

func (s codexSessionStore) GetModelConfigValue(id, key string) (any, bool, error) {
	return s.db.GetModelConfigValue(id, key)
}

func (s codexSessionStore) PatchModelConfig(id string, patch map[string]any) error {
	return s.db.PatchModelConfig(id, patch)
}

// Queues the runtime's token counts as the session store delta they map onto
func (s codexSessionStore) QueueTokenCounts(sessionID, source string, counts map[string]any) error {
	return s.db.QueueTokenCounts(sessionID, provider.CodexTokenCountDelta(source, counts))
}

// Session store view the codex app server runtime writes through, nil without a session DB
func codexSessionDBFor(db *state.DB) provider.CodexSessionDB {
	if db == nil {
		return nil
	}
	return codexSessionStore{db: db}
}
