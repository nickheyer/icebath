package sessions

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/sessions"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Keys of the counts a refresh of every session's prompt, or one session's, moves up
const (
	refreshAll     = "prompt_refresh.epoch"
	refreshSession = "prompt_refresh.epoch:"
)

func (s *store) SystemPrompt(ctx context.Context, id string) (string, error) {
	db, err := s.read(ctx)
	if err != nil {
		return "", err
	}
	return systemPrompt(ctx, db, id)
}

func systemPrompt(ctx context.Context, q querier, id string) (string, error) {
	var prompt sql.NullString
	err := q.QueryRowContext(ctx, `SELECT p.prompt FROM sessions s LEFT JOIN system_prompts p ON p.hash = s.system_prompt_hash WHERE s.id = ?`, id).Scan(&prompt)
	if errors.Is(err, sql.ErrNoRows) {
		return "", sessions.ErrNotFound
	}
	return prompt.String, err
}

func (s *store) SetSystemPrompt(ctx context.Context, id, prompt string, tools []string) error {
	err := s.write(ctx, func(tx *sql.Tx) error {
		return storePrompt(ctx, tx, id, prompt, tools)
	})
	if err == nil {
		s.changed(id)
	}
	return err
}

// Keeps the prompt once by its hash however many sessions send it, with the tools it was built for; an empty prompt clears it
func storePrompt(ctx context.Context, q querier, id, prompt string, tools []string) error {
	var hash, pin any
	if prompt != "" {
		sum := sha256.Sum256([]byte(prompt))
		hash = hex.EncodeToString(sum[:])
		if _, err := q.ExecContext(ctx, `INSERT OR IGNORE INTO system_prompts (hash, prompt) VALUES (?, ?)`, hash, prompt); err != nil {
			return err
		}
	}
	if tools != nil {
		data, err := json.Marshal(tools)
		if err != nil {
			return err
		}
		pin = string(data)
	}
	result, err := q.ExecContext(ctx, `UPDATE sessions SET system_prompt_hash = ?, tool_names = COALESCE(?, tool_names) WHERE id = ?`, hash, pin, id)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return sessions.ErrNotFound
	}
	_, err = q.ExecContext(ctx, `DELETE FROM system_prompts WHERE hash NOT IN (SELECT system_prompt_hash FROM sessions WHERE system_prompt_hash IS NOT NULL)`)
	return err
}

func (s *store) PromptEpoch(ctx context.Context, id string) (int64, error) {
	db, err := s.read(ctx)
	if err != nil {
		return 0, err
	}
	var epoch sql.NullInt64
	err = db.QueryRowContext(ctx, `SELECT MAX(CAST(value AS INTEGER)) FROM state_meta WHERE key IN (?, ?)`, refreshAll, refreshSession+id).Scan(&epoch)
	return epoch.Int64, err
}

func (s *store) RefreshPrompts(ctx context.Context, id string) (int64, error) {
	var held int64
	err := s.write(ctx, func(tx *sql.Tx) error {
		var current sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT MAX(CAST(value AS INTEGER)) FROM state_meta WHERE key = ? OR substr(key, 1, ?) = ?`,
			refreshAll, len(refreshSession), refreshSession).Scan(&current); err != nil {
			return err
		}
		key := refreshAll
		if id != "" {
			key = refreshSession + id
		}
		if err := setMeta(ctx, tx, key, strconv.FormatInt(current.Int64+1, 10)); err != nil {
			return err
		}
		query, args := `SELECT COUNT(*) FROM sessions WHERE system_prompt_hash IS NOT NULL`, []any{}
		if id != "" {
			query, args = query+` AND id = ?`, []any{id}
		}
		return tx.QueryRowContext(ctx, query, args...).Scan(&held)
	})
	return held, err
}

func (s *store) RepairPrompts(ctx context.Context, request *sessionsv1.RepairPromptsRequest, expected func(tools []string) []string) (*sessionsv1.RepairPromptsResponse, error) {
	db, err := s.read(ctx)
	if err != nil {
		return nil, err
	}
	out := &sessionsv1.RepairPromptsResponse{SessionId: request.GetSessionId()}
	if id := request.GetSessionId(); id != "" {
		prompt, err := systemPrompt(ctx, db, id)
		if err != nil {
			return nil, err
		}
		if prompt == "" {
			out.Empty = true
			return out, nil
		}
		out.Findings = append(out.Findings, &sessionsv1.PromptFinding{Id: id, Reason: "targeted clear", PromptChars: int64(utf8.RuneCountInString(prompt))})
	} else {
		rows, err := db.QueryContext(ctx, `SELECT s.id, p.prompt, COALESCE(s.tool_names, '') FROM sessions s JOIN system_prompts p ON p.hash = s.system_prompt_hash
			ORDER BY s.started_at`)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id, prompt, pin string
			if err := rows.Scan(&id, &prompt, &pin); err != nil {
				rows.Close()
				return nil, err
			}
			if strings.TrimSpace(prompt) == "" {
				continue
			}
			finding := &sessionsv1.PromptFinding{Id: id, PromptChars: int64(utf8.RuneCountInString(prompt))}
			var tools []string
			if pin == "" || json.Unmarshal([]byte(pin), &tools) != nil {
				finding.Reason = "the tools[] pin is unavailable or unreadable"
				out.Unverifiable = append(out.Unverifiable, finding)
				continue
			}
			var missing []string
			for _, text := range expected(tools) {
				if text = strings.TrimSpace(text); text != "" && !strings.Contains(prompt, text) {
					missing = append(missing, strs.Head(strings.Join(strings.Fields(text), " "), 60))
				}
			}
			if len(missing) > 0 {
				finding.Reason = "guidance missing while the tools[] pin carries its tools: " + strings.Join(missing, "; ")
				out.Findings = append(out.Findings, finding)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	if !request.GetApply() || len(out.GetFindings()) == 0 {
		return out, nil
	}
	err = s.write(ctx, func(tx *sql.Tx) error {
		out.Cleared = nil
		for _, finding := range out.GetFindings() {
			if err := storePrompt(ctx, tx, finding.GetId(), "", nil); err != nil {
				return err
			}
			out.Cleared = append(out.Cleared, finding.GetId())
		}
		return nil
	})
	if err == nil {
		s.changed(out.GetCleared()...)
	}
	return out, err
}
