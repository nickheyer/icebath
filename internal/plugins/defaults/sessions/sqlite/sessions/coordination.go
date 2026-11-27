package sessions

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"os"
	"regexp"
	"slices"
	"strconv"
	"syscall"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/nickheyer/icebath/internal/sessions"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Leases expired longer than this ago are swept away
const leaseSweep = 24 * time.Hour

var holderPID = regexp.MustCompile(`(?:^|:)pid=(\d+)(?::|$)`)

// Whether the process a lease holder names is gone, which frees its lease at once. A holder naming this process, or none, is alive
func holderDead(holder string) bool {
	match := holderPID.FindStringSubmatch(holder)
	if match == nil {
		return false
	}
	pid, err := strconv.Atoi(match[1])
	if err != nil || pid <= 0 || pid == os.Getpid() {
		return false
	}
	return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
}

// Takes the row of a lease table for the holder: a lease expired or held by a dead process is reclaimed first
func claim(ctx context.Context, tx *sql.Tx, table, id, holder string, ttl time.Duration) (bool, error) {
	at := now()
	var current string
	var expires float64
	err := tx.QueryRowContext(ctx, `SELECT holder, expires_at FROM `+table+` WHERE session_id = ?`, id).Scan(&current, &expires)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return false, err
	case current == holder:
		_, err := tx.ExecContext(ctx, `UPDATE `+table+` SET expires_at = ? WHERE session_id = ? AND holder = ?`, at+max(ttl.Seconds(), 0.1), id, holder)
		return err == nil, err
	case expires <= at || holderDead(current):
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE session_id = ? AND holder = ?`, id, current); err != nil {
			return false, err
		}
	default:
		return false, nil
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO `+table+` (session_id, holder, acquired_at, expires_at) VALUES (?, ?, ?, ?)`, id, holder, at,
		at+max(ttl.Seconds(), 0.1))
	return err == nil, err
}

func (s *store) AcquireTurn(ctx context.Context, id, holder string, ttl time.Duration) (bool, error) {
	acquired := false
	err := s.writeWithin(ctx, time.Second, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM session_turn_leases WHERE expires_at < ?`, now()-leaseSweep.Seconds()); err != nil {
			return err
		}
		var err error
		acquired, err = claim(ctx, tx, "session_turn_leases", id, holder, ttl)
		return err
	})
	if busy(err) {
		return false, nil
	}
	return acquired, err
}

func (s *store) RefreshTurn(ctx context.Context, id, holder string, ttl time.Duration) (bool, error) {
	return s.refresh(ctx, "session_turn_leases", id, holder, ttl)
}

func (s *store) ReleaseTurn(ctx context.Context, id, holder string) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM session_turn_leases WHERE session_id = ? AND holder = ?`, id, holder)
		return err
	})
}

func (s *store) refresh(ctx context.Context, table, id, holder string, ttl time.Duration) (bool, error) {
	refreshed := false
	err := s.write(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `UPDATE `+table+` SET expires_at = ? WHERE session_id = ? AND holder = ?`, now()+max(ttl.Seconds(), 0.1), id, holder)
		if err != nil {
			return err
		}
		n, _ := result.RowsAffected()
		refreshed = n > 0
		return nil
	})
	return refreshed, err
}

func (s *store) Beat(ctx context.Context, b sessions.Backend) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO backend_heartbeats (backend_id, pid, started_at, last_heartbeat, profile, host) VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT (backend_id) DO UPDATE SET last_heartbeat = excluded.last_heartbeat`, b.ID, b.PID, seconds(b.Started), now(), b.Profile, b.Host)
		return err
	})
}

func (s *store) EndBeat(ctx context.Context, id string) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM backend_heartbeats WHERE backend_id = ?`, id)
		return err
	})
}

func (s *store) Backends(ctx context.Context) ([]sessions.Backend, error) {
	db, err := s.read(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `SELECT backend_id, pid, started_at, profile, host FROM backend_heartbeats ORDER BY started_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []sessions.Backend
	for rows.Next() {
		var b sessions.Backend
		var started float64
		if err := rows.Scan(&b.ID, &b.PID, &started, &b.Profile, &b.Host); err != nil {
			return nil, err
		}
		b.Started = fromSeconds(started)
		out = append(out, b)
	}
	return out, rows.Err()
}

func (s *store) SweepOrphans(ctx context.Context, sources []string, idle time.Duration) ([]string, error) {
	if idle <= 0 || len(sources) == 0 {
		return nil, nil
	}
	var victims []string
	err := s.write(ctx, func(tx *sql.Tx) error {
		victims = nil
		at := now()
		cutoff := at - idle.Seconds()
		// Rows a live process may own are kept: a process that beat within twice the idle window and started before the session
		rows, err := tx.QueryContext(ctx, `SELECT id FROM sessions s WHERE ended_at IS NULL AND source IN (`+marks(len(sources))+`)
			AND s.pinned = 0 AND started_at < ? AND COALESCE(last_activity_at, started_at) < ?
			AND NOT EXISTS (SELECT 1 FROM backend_heartbeats h WHERE h.last_heartbeat >= ? AND h.started_at <= s.started_at + ?)`,
			append(strings2any(sources), cutoff, cutoff, at-2*idle.Seconds(), 2*idle.Seconds())...)
		if err != nil {
			return err
		}
		candidates, err := collect(rows)
		if err != nil {
			return err
		}
		held, err := guarded(ctx, tx, candidates)
		if err != nil {
			return err
		}
		for _, id := range candidates {
			if !slices.Contains(held, id) {
				victims = append(victims, id)
			}
		}
		for _, chunk := range chunks(victims) {
			if _, err := tx.ExecContext(ctx, `UPDATE sessions SET ended_at = ?, end_reason = ? WHERE ended_at IS NULL AND id IN (`+marks(len(chunk))+`)`,
				append([]any{at, sessions.EndOrphan}, strings2any(chunk)...)...); err != nil {
				return err
			}
		}
		return nil
	})
	if err == nil && len(victims) > 0 {
		s.changed(victims...)
	}
	return victims, err
}

func (s *store) Admit(ctx context.Context, session, text, backend string) (*sessionsv1.Admission, error) {
	admission := &sessionsv1.Admission{Id: "adm_" + strs.Random(12), SessionId: session, Text: text, Status: "queued", Created: timestamppb.Now()}
	err := s.writeWithin(ctx, transcriptPatience, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO session_admissions (admission_id, session_id, text, status, backend_id, created_at) VALUES (?, ?, ?, 'queued', ?, ?)`,
			admission.GetId(), session, text, backend, seconds(admission.GetCreated().AsTime()))
		return err
	})
	return admission, err
}

func (s *store) Begin(ctx context.Context, id, backend string) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE session_admissions SET status = 'started', backend_id = ?, started_at = ?, attempts = attempts + 1
			WHERE admission_id = ? AND status IN ('queued', 'unknown')`, backend, now(), id)
		return err
	})
}

func (s *store) Settle(ctx context.Context, id, outcome string) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE session_admissions SET status = 'terminal', outcome = ?, settled_at = ? WHERE admission_id = ? AND status <> 'terminal'`,
			outcome, now(), id)
		return err
	})
}

func (s *store) Pending(ctx context.Context, session string) ([]*sessionsv1.Admission, error) {
	db, err := s.read(ctx)
	if err != nil {
		return nil, err
	}
	return admissions(ctx, db, `WHERE session_id = ? AND status <> 'terminal'`, session)
}

func admissions(ctx context.Context, q querier, where string, args ...any) ([]*sessionsv1.Admission, error) {
	rows, err := q.QueryContext(ctx, `SELECT admission_id, session_id, text, status, created_at, started_at, attempts FROM session_admissions `+where+
		` ORDER BY seq`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*sessionsv1.Admission
	for rows.Next() {
		a := &sessionsv1.Admission{}
		var created float64
		var started sql.NullFloat64
		if err := rows.Scan(&a.Id, &a.SessionId, &a.Text, &a.Status, &created, &started, &a.Attempts); err != nil {
			return nil, err
		}
		a.Created = timestamppb.New(fromSeconds(created))
		if started.Valid {
			a.Started = timestamppb.New(fromSeconds(started.Float64))
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *store) Recover(ctx context.Context, alive []string) ([]*sessionsv1.Admission, error) {
	var lost []*sessionsv1.Admission
	err := s.write(ctx, func(tx *sql.Tx) error {
		dead, args := "", []any{}
		if len(alive) > 0 {
			dead, args = ` AND COALESCE(backend_id, '') NOT IN (`+marks(len(alive))+`)`, strings2any(alive)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE session_admissions SET status = 'unknown' WHERE status = 'started'`+dead, args...); err != nil {
			return err
		}
		var err error
		lost, err = admissions(ctx, tx, `WHERE status = 'unknown' OR status = 'queued'`+dead, args...)
		return err
	})
	return lost, err
}

func (s *store) Discard(ctx context.Context, session string, ids []string) ([]string, error) {
	var discarded []string
	err := s.write(ctx, func(tx *sql.Tx) error {
		lost, err := admissions(ctx, tx, `WHERE session_id = ? AND status = 'unknown'`, session)
		if err != nil {
			return err
		}
		discarded = nil
		for _, a := range lost {
			if len(ids) > 0 && !slices.Contains(ids, a.GetId()) {
				continue
			}
			if _, err := tx.ExecContext(ctx, `UPDATE session_admissions SET status = 'terminal', outcome = 'discarded', settled_at = ? WHERE admission_id = ?`,
				now(), a.GetId()); err != nil {
				return err
			}
			discarded = append(discarded, a.GetId())
		}
		for _, id := range ids {
			if !slices.Contains(discarded, id) {
				return fmt.Errorf("%s is not a lost message of this session.", id)
			}
		}
		return nil
	})
	if err == nil {
		s.changed(session)
	}
	return discarded, err
}

func (s *store) AcquireCompression(ctx context.Context, id, holder string, ttl time.Duration) (bool, error) {
	acquired := false
	err := s.write(ctx, func(tx *sql.Tx) error {
		var err error
		acquired, err = claim(ctx, tx, "compression_locks", id, holder, ttl)
		return err
	})
	return acquired, err
}

func (s *store) RefreshCompression(ctx context.Context, id, holder string, ttl time.Duration) (bool, error) {
	return s.refresh(ctx, "compression_locks", id, holder, ttl)
}

func (s *store) ReleaseCompression(ctx context.Context, id, holder string) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM compression_locks WHERE session_id = ? AND holder = ?`, id, holder)
		return err
	})
}

func (s *store) CompressionHolder(ctx context.Context, id string) (string, error) {
	db, err := s.read(ctx)
	if err != nil {
		return "", err
	}
	var holder string
	err = db.QueryRowContext(ctx, `SELECT holder FROM compression_locks WHERE session_id = ? AND expires_at >= ?`, id, now()).Scan(&holder)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return holder, err
}

func (s *store) CompressionCooldown(ctx context.Context, id string) (sessions.Cooldown, error) {
	db, err := s.read(ctx)
	if err != nil {
		return sessions.Cooldown{}, err
	}
	var c sessions.Cooldown
	var until, deadline sql.NullFloat64
	var failure sql.NullString
	err = db.QueryRowContext(ctx, `SELECT compression_failure_cooldown_until, compression_failure_error, compression_fallback_streak,
		compression_ineffective_count, compression_recovery_deadline, compression_overload_streak FROM sessions WHERE id = ?`, id).
		Scan(&until, &failure, &c.FallbackStreak, &c.IneffectiveCount, &deadline, &c.OverloadStreak)
	if errors.Is(err, sql.ErrNoRows) {
		return c, sessions.ErrNotFound
	}
	if until.Valid {
		c.Until = fromSeconds(until.Float64)
	}
	if deadline.Valid {
		c.RecoveryDeadline = fromSeconds(deadline.Float64)
	}
	c.Error = failure.String
	return c, err
}

func (s *store) SetCompressionCooldown(ctx context.Context, id string, c sessions.Cooldown) error {
	optional := func(t time.Time) any {
		if t.IsZero() {
			return nil
		}
		return seconds(t)
	}
	return s.change(ctx, id, `UPDATE sessions SET compression_failure_cooldown_until = ?, compression_failure_error = NULLIF(?, ''),
		compression_fallback_streak = ?, compression_ineffective_count = ?, compression_recovery_deadline = ?, compression_overload_streak = ? WHERE id = ?`,
		optional(c.Until), c.Error, c.FallbackStreak, c.IneffectiveCount, optional(c.RecoveryDeadline), c.OverloadStreak, id)
}

func (s *store) Compact(ctx context.Context, id string, through int64, list []*sessionsv1.Message, guard sessions.Guard) error {
	err := s.writeWithin(ctx, transcriptPatience, func(tx *sql.Tx) error {
		if err := checkGuard(ctx, tx, id, guard); err != nil {
			return err
		}
		// Messages at the start of the list that are stored already stay as they are, the rest of those up to through folded away
		var kept int64
		for len(list) > 0 && list[0].GetId() > 0 {
			kept = max(kept, list[0].GetId())
			list = list[1:]
		}
		rows, err := tx.QueryContext(ctx, `SELECT message_uid FROM messages WHERE session_id = ? AND active = 1 AND id > ? AND id <= ? ORDER BY id`, id, kept,
			through)
		if err != nil {
			return err
		}
		uids, err := collect(rows)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE messages SET active = 0, compacted = 1 WHERE session_id = ? AND active = 1 AND id > ? AND id <= ?`, id, kept,
			through); err != nil {
			return err
		}
		absorbed := false
		for _, m := range list {
			if m.GetSummary() && !absorbed {
				absorbed = true
				for _, uid := range uids {
					if !slices.Contains(m.Absorbed, uid) {
						m.Absorbed = append(m.Absorbed, uid)
					}
				}
			}
			m.Compacted, m.Removed = false, false
			if err := insert(ctx, tx, id, m); err != nil {
				return err
			}
		}
		// Messages stored meanwhile, after the ones the compaction saw, follow its messages
		later, err := tx.QueryContext(ctx, `SELECT id FROM messages WHERE session_id = ? AND active = 1 AND id > ? AND id < ? ORDER BY id`, id, through,
			firstID(list))
		if err != nil {
			return err
		}
		moved, err := collectIDs(later)
		if err != nil {
			return err
		}
		for _, row := range moved {
			copied, err := one(ctx, tx, row)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE messages SET active = 0, compacted = 0 WHERE id = ?`, row); err != nil {
				return err
			}
			copied.Id = 0
			if err := insert(ctx, tx, id, copied); err != nil {
				return err
			}
		}
		return recount(ctx, tx, id)
	})
	if err == nil {
		s.changed(id)
	}
	return err
}

// Id of the first message stored of the list, past every id when none was
func firstID(list []*sessionsv1.Message) int64 {
	for _, m := range list {
		if m.GetId() > 0 {
			return m.GetId()
		}
	}
	return math.MaxInt64
}

// One message of a session by its id
func one(ctx context.Context, q querier, id int64) (*sessionsv1.Message, error) {
	return scanMessage(q.QueryRowContext(ctx, `SELECT `+messageColumns+` FROM messages WHERE id = ?`, id))
}

func collectIDs(rows *sql.Rows) ([]int64, error) {
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (s *store) Continue(ctx context.Context, id string, child *sessionsv1.Session, list []*sessionsv1.Message) (*sessionsv1.Session, error) {
	childID := cmp.Or(child.GetId(), newID(time.Now()))
	err := s.writeWithin(ctx, transcriptPatience, func(tx *sql.Tx) error {
		parent, err := get(ctx, tx, id)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE sessions SET ended_at = ?, end_reason = ? WHERE id = ?`, now(), sessions.EndCompression, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO sessions (id, profile, source, created_source, user_id, session_key, chat_id, chat_type, thread_id,
			display_name, origin_json, model, model_config, system_prompt_hash, tool_names, parent_session_id, started_at, cwd, git_branch, git_repo_root,
			billing_provider, billing_base_url, pinned, yolo)
			SELECT ?, profile, source, created_source, user_id, session_key, chat_id, chat_type, thread_id, display_name, origin_json,
			COALESCE(NULLIF(?, ''), model), model_config, system_prompt_hash, tool_names, id, ?, cwd, git_branch, git_repo_root, billing_provider,
			billing_base_url, pinned, yolo FROM sessions WHERE id = ?`, childID, child.GetModel(), now(), id); err != nil {
			return err
		}
		for _, m := range list {
			if err := insert(ctx, tx, childID, m); err != nil {
				return err
			}
		}
		if err := recount(ctx, tx, childID); err != nil {
			return err
		}
		if parent.GetTitle() != "" {
			if _, err := tx.ExecContext(ctx, `UPDATE sessions SET title = NULL WHERE id = ?`, id); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, `UPDATE sessions SET title = ?, title_source = ? WHERE id = ?`, parent.GetTitle(),
				cmp.Or(parent.GetTitleSource(), sessions.TitleUser), childID)
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.changed(id, childID)
	return s.Get(ctx, childID)
}

func (s *store) Tip(ctx context.Context, id string) (string, error) {
	db, err := s.read(ctx)
	if err != nil {
		return "", err
	}
	current := id
	for range 1000 {
		var child string
		err := db.QueryRowContext(ctx, `SELECT c.id FROM sessions c JOIN sessions p ON p.id = c.parent_session_id WHERE p.id = ? AND `+continuation+`
			ORDER BY c.started_at DESC LIMIT 1`, current).Scan(&child)
		if errors.Is(err, sql.ErrNoRows) {
			return current, nil
		}
		if err != nil {
			return "", err
		}
		current = child
	}
	return current, nil
}

func (s *store) Lineage(ctx context.Context, id string) ([]string, error) {
	db, err := s.read(ctx)
	if err != nil {
		return nil, err
	}
	return chainOf(ctx, db, id)
}

func (s *store) Root(ctx context.Context, id string) (string, error) {
	db, err := s.read(ctx)
	if err != nil {
		return "", err
	}
	current, seen := id, map[string]bool{}
	for !seen[current] {
		seen[current] = true
		var parent sql.NullString
		err := db.QueryRowContext(ctx, `SELECT parent_session_id FROM sessions WHERE id = ?`, current).Scan(&parent)
		if errors.Is(err, sql.ErrNoRows) || err == nil && parent.String == "" {
			return current, nil
		}
		if err != nil {
			return "", err
		}
		current = parent.String
	}
	return current, nil
}

func (s *store) Routes(ctx context.Context, scope string) (map[string]string, error) {
	db, err := s.read(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `SELECT session_key, entry_json FROM gateway_routing WHERE scope = ?`, scope)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var key, entry string
		if err := rows.Scan(&key, &entry); err != nil {
			return nil, err
		}
		out[key] = entry
	}
	return out, rows.Err()
}

func (s *store) SetRoute(ctx context.Context, scope, key, entry string) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO gateway_routing (scope, session_key, entry_json, updated_at) VALUES (?, ?, ?, ?)
			ON CONFLICT (scope, session_key) DO UPDATE SET entry_json = excluded.entry_json, updated_at = excluded.updated_at`, scope, key, entry, now())
		return err
	})
}

func (s *store) DeleteRoute(ctx context.Context, scope, key string) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM gateway_routing WHERE scope = ? AND session_key = ?`, scope, key)
		return err
	})
}

func (s *store) PeerSession(ctx context.Context, source string, peer *sessionsv1.Peer) (string, error) {
	db, err := s.read(ctx)
	if err != nil {
		return "", err
	}
	var id string
	err = db.QueryRowContext(ctx, `SELECT id FROM sessions WHERE source = ? AND chat_id = ? AND COALESCE(thread_id, '') = ?
		AND (? = '' OR COALESCE(user_id, '') = ?) AND (? = '' OR COALESCE(chat_type, '') = ?) AND COALESCE(end_reason, '') <> 'compression'
		ORDER BY started_at DESC LIMIT 1`, source, peer.GetChatId(), peer.GetThreadId(), peer.GetUserId(), peer.GetUserId(), peer.GetChatType(),
		peer.GetChatType()).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

func (s *store) NextGeneration(ctx context.Context, source, key string) (int64, error) {
	var generation int64
	err := s.write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO conversation_generations (source, session_key, generation) VALUES (?, ?, 1)
			ON CONFLICT (source, session_key) DO UPDATE SET generation = generation + 1`, source, key); err != nil {
			return err
		}
		return tx.QueryRowContext(ctx, `SELECT generation FROM conversation_generations WHERE source = ? AND session_key = ?`, source, key).Scan(&generation)
	})
	return generation, err
}

func (s *store) RequestHandoff(ctx context.Context, id, platform string) error {
	return s.change(ctx, id, `UPDATE sessions SET handoff_state = 'pending', handoff_platform = ?, handoff_error = NULL WHERE id = ?`, platform, id)
}

func (s *store) ClaimHandoffs(ctx context.Context, platform string) ([]string, error) {
	var claimed []string
	err := s.write(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT id FROM sessions WHERE handoff_state = 'pending' AND handoff_platform = ? ORDER BY started_at`, platform)
		if err != nil {
			return err
		}
		if claimed, err = collect(rows); err != nil {
			return err
		}
		for _, chunk := range chunks(claimed) {
			if _, err := tx.ExecContext(ctx, `UPDATE sessions SET handoff_state = 'running' WHERE id IN (`+marks(len(chunk))+`)`, strings2any(chunk)...); err != nil {
				return err
			}
		}
		return nil
	})
	return claimed, err
}

func (s *store) FinishHandoff(ctx context.Context, id, failure string) error {
	state := "completed"
	if failure != "" {
		state = "failed"
	}
	return s.change(ctx, id, `UPDATE sessions SET handoff_state = ?, handoff_error = NULLIF(?, '') WHERE id = ?`, state, failure, id)
}

func (s *store) HygieneFailures(ctx context.Context, key string, failed bool) (int, error) {
	var streak int
	err := s.write(ctx, func(tx *sql.Tx) error {
		if !failed {
			_, err := tx.ExecContext(ctx, `DELETE FROM gateway_hygiene_state WHERE session_key = ?`, key)
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO gateway_hygiene_state (session_key, failure_streak) VALUES (?, 1)
			ON CONFLICT (session_key) DO UPDATE SET failure_streak = failure_streak + 1`, key); err != nil {
			return err
		}
		return tx.QueryRowContext(ctx, `SELECT failure_streak FROM gateway_hygiene_state WHERE session_key = ?`, key).Scan(&streak)
	})
	return streak, err
}

func (s *store) NeverActive(ctx context.Context, older time.Duration) ([]*sessionsv1.Session, error) {
	db, err := s.read(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `SELECT `+columns+` FROM sessions s WHERE s.session_key IS NOT NULL AND s.message_count = 0 AND s.tool_call_count = 0
		AND s.input_tokens + s.output_tokens = 0 AND s.title IS NULL AND s.pinned = 0 AND s.started_at < ?
		AND NOT EXISTS (SELECT 1 FROM messages m WHERE m.session_id = s.id) ORDER BY s.started_at`, now()-older.Seconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*sessionsv1.Session
	for rows.Next() {
		session, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, session)
	}
	return out, rows.Err()
}
