package state

import (
	"context"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Keep existing constructor upsert on the caller's connection, refusing to recreate a retired session
func (d *SessionDB) insertSessionRowInTransaction(ctx context.Context, conn SQLiteConn, n NewSession) error {
	live, err := queryObjects(ctx, conn, "SELECT 1 FROM sessions WHERE id=?", n.ID)
	if err != nil {
		return err
	}
	if len(live) == 0 {
		retired, err := queryObjects(ctx, conn, "SELECT 1 FROM state_meta WHERE key=?", RetiredPrefix+n.ID)
		if err != nil {
			return err
		}
		if len(retired) > 0 {
			return NewRuntimeStoreError("not_found", nil, nil)
		}
	}
	profileName := n.ProfileName
	if pyStrip(profileName) == "" {
		profileName = d.ownProfileName()
	}
	var prompt any
	if n.SystemPrompt != "" {
		prompt = n.SystemPrompt
	}
	promptHash, err := storeSystemPrompt(ctx, conn, prompt)
	if err != nil {
		return err
	}
	var modelConfig any
	if len(n.ModelConfig) > 0 {
		text, err := pyjson.Dumps(n.ModelConfig)
		if err != nil {
			return err
		}
		modelConfig = text
	}
	_, err = conn.ExecContext(ctx, `INSERT INTO sessions (
           id, source, created_source, user_id, session_key, chat_id, chat_type, thread_id,
           model, model_config, system_prompt, system_prompt_hash,
           parent_session_id, cwd, profile_name, transport_profile, git_repo_root,
           origin_json, display_name, started_at
        )
           VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, ?, ?, ?, ?, ?, ?, ?, ?, ?)
           ON CONFLICT(id) DO UPDATE SET
               source = CASE
                   WHEN sessions.source = 'unknown'
                   THEN COALESCE(excluded.source, 'unknown')
                   ELSE sessions.source
               END,
               model = COALESCE(sessions.model, excluded.model),
               model_config = CASE
                   WHEN excluded.model_config IS NOT NULL
                        AND json_type(
                            sessions.model_config, '$._reset_from'
                        ) IS NOT NULL
                        AND json_remove(
                            sessions.model_config, '$._reset_from'
                        ) = '{}'
                   THEN json_set(
                       excluded.model_config,
                       '$._reset_from',
                       json_extract(
                           sessions.model_config, '$._reset_from'
                       )
                   )
                   ELSE COALESCE(
                       sessions.model_config, excluded.model_config
                   )
               END,
               system_prompt_hash = COALESCE(
                   sessions.system_prompt_hash,
                   excluded.system_prompt_hash
               ),
               system_prompt = CASE
                   WHEN sessions.system_prompt_hash IS NULL
                        AND excluded.system_prompt_hash IS NOT NULL
                   THEN NULL
                   ELSE sessions.system_prompt
               END,
`+upsertKeepExistingSQL,
		n.ID, n.Source, n.Source, nullStr(n.UserID), nullStr(n.SessionKey), nullStr(n.ChatID), nullStr(n.ChatType),
		nullStr(n.ThreadID), nullStr(n.Model), modelConfig, promptHash, nullStr(n.ParentSessionID), nullStr(n.Cwd),
		nullStr(profileName), nullStr(n.TransportProfile), nullStr(n.GitRepoRoot), nullStr(n.OriginJSON),
		nullStr(n.DisplayName), float64(time.Now().UnixNano())/1e9)
	if err != nil {
		return err
	}
	if promptHash != nil {
		if err := deleteUnreferencedSystemPrompts(ctx, conn); err != nil {
			return err
		}
	}
	if n.ParentSessionID != "" {
		if err := inheritParentSessionMetadata(ctx, conn, n.ID); err != nil {
			return err
		}
	}
	return stampCompressionAncestors(ctx, conn, n.ID)
}
