package sessions

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/profile"
)

// Start of the session keys of a profile's chats: agent:main: for the default profile, agent:main~: for one named main
func namespace(profile string) string {
	switch profile {
	case "default":
		return "agent:main:"
	case "main":
		return "agent:main~:"
	}
	return "agent:" + profile + ":"
}

// Removes what the gateway keeps of a profile's chats: its routes, their hygiene and the beats of backends serving it
func purgeIdentity(ctx context.Context, tx *sql.Tx, profile string) error {
	ns := namespace(profile)
	for _, statement := range []string{
		`DELETE FROM gateway_routing WHERE substr(session_key, 1, ?) = ?`,
		`DELETE FROM gateway_hygiene_state WHERE substr(session_key, 1, ?) = ?`,
	} {
		if _, err := tx.ExecContext(ctx, statement, len(ns), ns); err != nil {
			return err
		}
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM backend_heartbeats WHERE profile = ?`, profile)
	return err
}

// Key with the namespace of the old profile changed to the new one's, the same key when it lies in no such namespace
func rekeyed(key, oldNS, newNS string) string {
	if rest, ok := strings.CutPrefix(key, oldNS); ok {
		return newNS + rest
	}
	return key
}

// JSON object with its profile and session key moved to the new profile, as it was when it is no object
func rekeyJSON(text, old, renamed, oldNS, newNS string) string {
	var doc map[string]any
	if json.Unmarshal([]byte(text), &doc) != nil {
		return text
	}
	if doc["profile"] == old {
		doc["profile"] = renamed
	}
	if key, ok := doc["session_key"].(string); ok {
		doc["session_key"] = rekeyed(key, oldNS, newNS)
	}
	if origin, ok := doc["origin"].(map[string]any); ok && origin["profile"] == old {
		origin["profile"] = renamed
	}
	data, err := json.Marshal(doc)
	if err != nil {
		return text
	}
	return string(data)
}

// Moves every session of a profile, the keys of its chats, its routes and the beats of backends serving it to its new name, at once; refused
// when the new name already holds a session title or a route the move would collide with
func (s *store) Rekey(ctx context.Context, old, renamed string) error {
	if old == renamed {
		return nil
	}
	oldNS, newNS := namespace(old), namespace(renamed)
	err := s.write(ctx, func(tx *sql.Tx) error {
		var title string
		err := tx.QueryRowContext(ctx, `SELECT a.title FROM sessions a JOIN sessions b ON b.profile = ? AND b.title = a.title WHERE a.profile = ? AND
			a.title IS NOT NULL LIMIT 1`, renamed, old).Scan(&title)
		if err == nil {
			return fmt.Errorf("profile %s already has a session titled %q that %s's would collide with", renamed, title, old)
		}
		if err != sql.ErrNoRows {
			return err
		}
		var scope, target string
		err = tx.QueryRowContext(ctx, `SELECT a.scope, ? || substr(a.session_key, ?) FROM gateway_routing a JOIN gateway_routing b ON b.scope = a.scope
			AND b.session_key = ? || substr(a.session_key, ?) WHERE substr(a.session_key, 1, ?) = ? LIMIT 1`,
			newNS, len(oldNS)+1, newNS, len(oldNS)+1, len(oldNS), oldNS).Scan(&scope, &target)
		if err == nil {
			return fmt.Errorf("profile routing collision in scope %q: %q", scope, target)
		}
		if err != sql.ErrNoRows {
			return err
		}
		for _, statement := range []string{
			`UPDATE sessions SET profile = ? WHERE profile = ?`,
			`UPDATE backend_heartbeats SET profile = ? WHERE profile = ?`,
		} {
			if _, err := tx.ExecContext(ctx, statement, renamed, old); err != nil {
				return err
			}
		}
		// Folders the sessions worked in that lay in the profile's folder are in its new one now
		oldDir, newDir := s.config.ProfileDir(old), s.config.ProfileDir(renamed)
		width := utf8.RuneCountInString(oldDir)
		for _, column := range []string{"cwd", "git_repo_root"} {
			if _, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE sessions SET %[1]s = ? || substr(%[1]s, ?) WHERE profile = ? AND (%[1]s = ? OR
				substr(%[1]s, 1, ?) = ?)`, column), newDir, width+1, renamed, oldDir, width+1, oldDir+string(filepath.Separator)); err != nil {
				return err
			}
		}
		for _, statement := range []string{
			`UPDATE sessions SET session_key = ? || substr(session_key, ?) WHERE substr(session_key, 1, ?) = ?`,
			`UPDATE gateway_hygiene_state SET session_key = ? || substr(session_key, ?) WHERE substr(session_key, 1, ?) = ?`,
			`UPDATE conversation_generations SET session_key = ? || substr(session_key, ?) WHERE substr(session_key, 1, ?) = ?`,
		} {
			if _, err := tx.ExecContext(ctx, statement, newNS, len(oldNS)+1, len(oldNS), oldNS); err != nil {
				return err
			}
		}
		rows, err := tx.QueryContext(ctx, `SELECT id, origin_json FROM sessions WHERE origin_json IS NOT NULL AND origin_json LIKE '%"profile"%'`)
		if err != nil {
			return err
		}
		origins := map[string]string{}
		for rows.Next() {
			var id, origin string
			if err := rows.Scan(&id, &origin); err != nil {
				rows.Close()
				return err
			}
			if changed := rekeyJSON(origin, old, renamed, oldNS, newNS); changed != origin {
				origins[id] = changed
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for id, origin := range origins {
			if _, err := tx.ExecContext(ctx, `UPDATE sessions SET origin_json = ? WHERE id = ?`, origin, id); err != nil {
				return err
			}
		}
		rows, err = tx.QueryContext(ctx, `SELECT scope, session_key, entry_json FROM gateway_routing WHERE substr(session_key, 1, ?) = ?`, len(oldNS), oldNS)
		if err != nil {
			return err
		}
		type route struct{ scope, key, entry string }
		var routes []route
		for rows.Next() {
			var r route
			if err := rows.Scan(&r.scope, &r.key, &r.entry); err != nil {
				rows.Close()
				return err
			}
			routes = append(routes, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, r := range routes {
			if _, err := tx.ExecContext(ctx, `UPDATE gateway_routing SET session_key = ?, entry_json = ? WHERE scope = ? AND session_key = ?`,
				rekeyed(r.key, oldNS, newNS), rekeyJSON(r.entry, old, renamed, oldNS, newNS), r.scope, r.key); err != nil {
				return err
			}
		}
		return nil
	})
	if err == nil {
		s.changed()
	}
	return s.failed(err)
}

// Failure of a profile's purge or move naming the database it failed on
func (s *store) failed(err error) error {
	if err == nil {
		return nil
	}
	return &profile.StoreError{Store: s.path, Err: err}
}
