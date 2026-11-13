package state

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
)

// Stores prompt once under the hex SHA-256 of its UTF-8 bytes and returns that hash, NULL for no prompt
func storeSystemPrompt(ctx context.Context, c Conn, prompt *string) (sql.NullString, error) {
	if prompt == nil {
		return sql.NullString{}, nil
	}
	sum := sha256.Sum256([]byte(*prompt))
	hash := hex.EncodeToString(sum[:])
	if _, err := c.ExecContext(ctx, "INSERT OR IGNORE INTO system_prompts (hash, prompt) VALUES (?, ?)", hash, *prompt); err != nil {
		return sql.NullString{}, err
	}
	return sql.NullString{String: hash, Valid: true}, nil
}

// Deletes stored prompts no session references as its prompt or its tool pin
func deleteUnreferencedSystemPrompts(ctx context.Context, c Conn) error {
	_, err := c.ExecContext(ctx, "DELETE FROM system_prompts WHERE NOT EXISTS ("+
		"SELECT 1 FROM sessions WHERE sessions.system_prompt_hash = system_prompts.hash) AND NOT EXISTS ("+
		"SELECT 1 FROM sessions WHERE sessions.tool_names = system_prompts.hash)")
	return err
}
