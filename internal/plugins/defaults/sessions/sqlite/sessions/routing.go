package sessions

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"time"

	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Sources of sessions no messaging gateway routes: the local surfaces and automation
var unrouted = []string{"cli", "tui", "gui", "desktop", "oneshot", "tool", "acp", "cron", "subagent", "kanban", "bot_room", "a2a", "webhook",
	"api_server", "import", "recovered", "curator", "batch", "homeassistant", "webui", "local", "unknown", "claude", "codex"}

// Profile a gateway session key belongs to, false for a key that names none
func keyProfile(key string) (string, bool) {
	parts := strings.Split(key, ":")
	if len(parts) < 2 || parts[0] != "agent" {
		return "", false
	}
	switch parts[1] {
	case "", "main":
		return "default", true
	case "main~":
		return "main", true
	}
	return parts[1], true
}

type gatewayRow struct {
	id, source, key, chatID, chatType, threadID, userID, origin, parent string
	started, active                                                     float64
	messages                                                            int64
}

func (s *store) RepairRouting(ctx context.Context, r *sessionsv1.RepairRoutingRequest) (*sessionsv1.RepairRoutingResponse, error) {
	db, err := s.read(ctx)
	if err != nil {
		return nil, err
	}
	gap := r.GetMaxGapSeconds()
	if gap <= 0 {
		gap = 900
	}
	rows, err := db.QueryContext(ctx, `SELECT id, source, COALESCE(session_key, ''), COALESCE(chat_id, ''), COALESCE(chat_type, ''), COALESCE(thread_id, ''),
		COALESCE(user_id, ''), COALESCE(origin_json, ''), COALESCE(parent_session_id, ''), started_at, COALESCE(last_activity_at, started_at), message_count
		FROM sessions WHERE source NOT IN (`+marks(len(unrouted))+`) ORDER BY started_at`, strings2any(unrouted)...)
	if err != nil {
		return nil, err
	}
	var all []gatewayRow
	for rows.Next() {
		var g gatewayRow
		if err := rows.Scan(&g.id, &g.source, &g.key, &g.chatID, &g.chatType, &g.threadID, &g.userID, &g.origin, &g.parent, &g.started, &g.active,
			&g.messages); err != nil {
			rows.Close()
			return nil, err
		}
		all = append(all, g)
	}
	rows.Close()
	byID := map[string]gatewayRow{}
	for _, g := range all {
		byID[g.id] = g
	}
	out := &sessionsv1.RepairRoutingResponse{}
	claims := map[string][]string{}
	for _, g := range all {
		if g.key != "" || g.chatID != "" || g.origin != "" {
			continue
		}
		orphan := &sessionsv1.Orphan{Id: g.id, Source: g.source, Messages: g.messages}
		out.Orphans = append(out.Orphans, orphan)
		if g.parent != "" {
			parent, ok := byID[g.parent]
			if !ok || parent.source != g.source || parent.key == "" {
				orphan.Reason = "The session it continues has no route back to a chat of the same app."
				continue
			}
			orphan.SessionKey, orphan.DonorId, orphan.Evidence = parent.key, parent.id, "lineage"
			claims[parent.id] = append(claims[parent.id], g.id)
			continue
		}
		var donors []gatewayRow
		for _, other := range all {
			if other.source == g.source && other.key != "" && other.started < g.started && g.started-other.active <= gap && g.started >= other.active-gap {
				donors = append(donors, other)
			}
		}
		switch len(donors) {
		case 0:
			orphan.Reason = fmt.Sprintf("No session with a route went quiet within %s before this one started.", strs.Span(time.Duration(gap*float64(time.Second))))
		case 1:
			orphan.SessionKey, orphan.DonorId, orphan.Evidence = donors[0].key, donors[0].id, "contiguity"
			claims[donors[0].id] = append(claims[donors[0].id], g.id)
		default:
			orphan.Reason = "More than one session with a route could be the one it continues."
		}
	}
	for _, orphan := range out.GetOrphans() {
		if orphan.GetDonorId() != "" && len(claims[orphan.GetDonorId()]) > 1 {
			orphan.SessionKey, orphan.Evidence, orphan.Reason = "", "", "Another session without a route claims the same session it continues."
		}
	}
	if !r.GetApply() {
		return out, nil
	}
	err = s.write(ctx, func(tx *sql.Tx) error {
		for _, orphan := range out.GetOrphans() {
			if orphan.GetSessionKey() == "" {
				continue
			}
			donor := byID[orphan.GetDonorId()]
			result, err := tx.ExecContext(ctx, `UPDATE sessions SET session_key = ?, chat_id = NULLIF(?, ''), chat_type = NULLIF(?, ''),
				thread_id = NULLIF(?, ''), user_id = COALESCE(user_id, NULLIF(?, '')), origin_json = NULLIF(?, '')
				WHERE id = ? AND session_key IS NULL AND chat_id IS NULL AND origin_json IS NULL`,
				donor.key, donor.chatID, donor.chatType, donor.threadID, donor.userID, donor.origin, orphan.GetId())
			if err != nil {
				return err
			}
			n, _ := result.RowsAffected()
			orphan.Repaired = n > 0
		}
		return nil
	})
	if err == nil {
		s.changed()
	}
	return out, err
}

func (s *store) RepairProfiles(ctx context.Context, r *sessionsv1.RepairProfilesRequest, live func(profile string) bool) (*sessionsv1.RepairProfilesResponse, error) {
	db, err := s.read(ctx)
	if err != nil {
		return nil, err
	}
	out := &sessionsv1.RepairProfilesResponse{Applied: map[string]int64{}}
	type fix struct {
		finding *sessionsv1.ProfileFinding
		apply   func(tx *sql.Tx) (string, error)
	}
	var fixes []fix
	rows, err := db.QueryContext(ctx, `SELECT s.id, s.profile, COALESCE(s.session_key, ''), s.message_count, COALESCE(p.id, ''), COALESCE(p.profile, '')
		FROM sessions s LEFT JOIN sessions p ON p.id = s.parent_session_id ORDER BY s.started_at`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id, profile, key, parent, parentProfile string
		var messages int64
		if err := rows.Scan(&id, &profile, &key, &messages, &parent, &parentProfile); err != nil {
			rows.Close()
			return nil, err
		}
		owner, keyed := keyProfile(key)
		switch {
		case keyed && owner == "default" && profile != "default" && live(profile):
			finding := &sessionsv1.ProfileFinding{Kind: "legacy_main", Subject: id,
				Detail: fmt.Sprintf("The profile %s holds it, with %s, under the old main key %s.", profile, strs.Count(int(messages), "message"), key)}
			var apply func(tx *sql.Tx) (string, error)
			switch r.GetLegacyMain() {
			case "rekey":
				finding.Action = "Give it the key agent:" + profile + ": of its profile."
				rekeyed := "agent:" + profile + ":" + strings.SplitN(key, ":", 3)[len(strings.SplitN(key, ":", 3))-1]
				apply = func(tx *sql.Tx) (string, error) {
					_, err := tx.ExecContext(ctx, `UPDATE sessions SET session_key = ? WHERE id = ?`, rekeyed, id)
					return "rekeyed", err
				}
			case "move":
				finding.Action = "Move it to the default profile."
				apply = func(tx *sql.Tx) (string, error) {
					_, err := tx.ExecContext(ctx, `UPDATE sessions SET profile = 'default' WHERE id = ?`, id)
					return "moved", err
				}
			default:
				finding.Reason = "It may be the history of the profile itself, which --legacy-main rekey keeps there, or a chat of the default profile " +
					"that leaked in, which --legacy-main move gives back. The session can't tell which."
			}
			fixes = append(fixes, fix{finding: finding, apply: apply})
		case !live(profile) && (!keyed || !live(owner)):
			fixes = append(fixes, fix{finding: &sessionsv1.ProfileFinding{Kind: "unclaimed_namespace", Subject: id,
				Detail: fmt.Sprintf("It belongs to the profile %s and has %s.", profile, strs.Count(int(messages), "message")),
				Reason: fmt.Sprintf("There is no profile named %s. Create it and run this again, or move the session from a chat with /profile.", profile)}})
		case keyed && owner != profile && live(owner):
			target := owner
			fixes = append(fixes, fix{finding: &sessionsv1.ProfileFinding{Kind: "mislabelled", Subject: id,
				Detail: fmt.Sprintf("It is filed under %s, but its key %s belongs to %s.", profile, key, owner), Action: "File it under " + owner + "."},
				apply: func(tx *sql.Tx) (string, error) {
					_, err := tx.ExecContext(ctx, `UPDATE sessions SET profile = ? WHERE id = ?`, target, id)
					return "relabelled", err
				}})
		}
		if parent != "" && parentProfile != profile {
			fixes = append(fixes, fix{finding: &sessionsv1.ProfileFinding{Kind: "crossed_parent", Subject: id,
				Detail: fmt.Sprintf("It is in %s and continues the session %s of %s.", profile, parent, parentProfile), Action: "Cut the link to that session."},
				apply: func(tx *sql.Tx) (string, error) {
					_, err := tx.ExecContext(ctx, `UPDATE sessions SET parent_session_id = NULL WHERE id = ?`, id)
					return "severed", err
				}})
		}
	}
	rows.Close()
	routes, err := db.QueryContext(ctx, `SELECT scope, session_key FROM gateway_routing ORDER BY scope, session_key`)
	if err != nil {
		return nil, err
	}
	for routes.Next() {
		var scope, key string
		if err := routes.Scan(&scope, &key); err != nil {
			routes.Close()
			return nil, err
		}
		owner, keyed := keyProfile(key)
		if keyed && !live(owner) {
			fixes = append(fixes, fix{finding: &sessionsv1.ProfileFinding{Kind: "routing_unclaimed", Subject: key + " in " + scope,
				Detail: fmt.Sprintf("It is a route of %s, a profile that no longer exists, and every message that comes in on it logs an error.", owner),
				Action: "Delete it."},
				apply: func(tx *sql.Tx) (string, error) {
					_, err := tx.ExecContext(ctx, `DELETE FROM gateway_routing WHERE scope = ? AND session_key = ?`, scope, key)
					return "routing_deleted", err
				}})
		}
	}
	routes.Close()
	for _, f := range fixes {
		out.Findings = append(out.Findings, f.finding)
	}
	if !r.GetApply() || !slices.ContainsFunc(fixes, func(f fix) bool { return f.apply != nil }) {
		return out, nil
	}
	backup := fmt.Sprintf("%s.repair-profiles-%s", s.path, time.Now().Format("20060102_150405"))
	if _, err := db.ExecContext(ctx, `VACUUM INTO ?`, backup); err != nil {
		return nil, err
	}
	out.Backup = backup
	for _, f := range fixes {
		if f.apply == nil {
			continue
		}
		err := s.write(ctx, func(tx *sql.Tx) error {
			key, err := f.apply(tx)
			if err == nil {
				out.Applied[key]++
			}
			return err
		})
		if err != nil {
			out.Failures = append(out.Failures, fmt.Sprintf("%s could not be repaired: %v", f.finding.GetSubject(), err))
		}
	}
	s.changed()
	return out, nil
}
