package sessions

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	sqliteconfig "github.com/nickheyer/icebath/internal/plugins/defaults/sessions/sqlite/config"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
	"github.com/nickheyer/icebath/pkg/sqlitex"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Prefixes of the state_meta markers of a backfill of the word and the trigram index: the highest id it indexes itself and how far it got
const (
	wordBackfill    = "fts_rebuild"
	trigramBackfill = "fts_trigram_rebuild"
)

// Pacing of a backfill: rows of the first chunk and their bounds, the time a chunk aims for in the foreground and the background, and how
// much longer than its chunk a background backfill rests
const (
	firstChunk     = 500
	leastChunk     = 50
	mostChunk      = 100_000
	foregroundCost = 250 * time.Millisecond
	backgroundCost = 50 * time.Millisecond
	restFactor     = 4
	leastRest      = 200 * time.Millisecond
)

// Search index a backfill fills from its source view, with the statements that make its view and table and those of the triggers that keep
// it in step, each named after its table
type index struct {
	prefix, table, source, columns string
	body, triggers                 string
}

var (
	wordIndex    = index{wordBackfill, "messages_fts", "messages_fts_src", "content, tool_name, tool_calls", ftsBody, ftsTriggers}
	trigramIndex = index{trigramBackfill, "messages_fts_trigram", "messages_fts_trigram_src", "content, tool_name", trigramBody, trigramTriggers}
)

func (i index) triggerNames() []string {
	return []string{i.table + "_insert", i.table + "_delete", i.table + "_update"}
}

// Whether the object of the name is one of the index: its view, its table or a trigger of it
func (i index) owns(name string) bool {
	name = strings.ToLower(name)
	return name == i.source || name == i.table || slices.Contains(i.triggerNames(), name)
}

// Drops the index whole: its triggers, its table with what it holds, and its view
func (i index) drop(ctx context.Context, q querier) error {
	for _, trigger := range i.triggerNames() {
		if _, err := q.ExecContext(ctx, "DROP TRIGGER IF EXISTS "+trigger); err != nil {
			return err
		}
	}
	_, err := q.ExecContext(ctx, "DROP TABLE IF EXISTS "+i.table+"; DROP VIEW IF EXISTS "+i.source)
	return err
}

// Progress of a backfill: the id it indexed up to and the highest it indexes, false when none runs
func progressOf(ctx context.Context, q querier, prefix string) (int64, int64, bool, error) {
	high, ok, err := meta(ctx, q, prefix+"_high_water")
	if err != nil || !ok {
		return 0, 0, false, err
	}
	total, err := strconv.ParseInt(strings.TrimSpace(high), 10, 64)
	if err != nil || total <= 0 {
		return 0, 0, false, nil
	}
	done, _, err := meta(ctx, q, prefix+"_progress")
	if err != nil {
		return 0, 0, false, err
	}
	indexed, _ := strconv.ParseInt(strings.TrimSpace(done), 10, 64)
	return min(max(indexed, 0), total), total, true, nil
}

// Starts a backfill of the index over every message stored, which its triggers then leave to it; none when there are no messages
func seed(ctx context.Context, q querier, prefix string) error {
	var high int64
	if err := q.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM messages`).Scan(&high); err != nil {
		return err
	}
	if _, err := q.ExecContext(ctx, `DELETE FROM state_meta WHERE key IN (?, ?)`, prefix+"_high_water", prefix+"_progress"); err != nil || high <= 0 {
		return err
	}
	if err := setMeta(ctx, q, prefix+"_high_water", strconv.FormatInt(high, 10)); err != nil {
		return err
	}
	return setMeta(ctx, q, prefix+"_progress", "0")
}

// Whether the search indexes were detached after damage, as the file says, which another process may have changed
func (s *store) isStale(ctx context.Context, q querier) bool {
	_, stale, err := meta(ctx, q, "fts_stale")
	if err != nil {
		return s.stale.Load()
	}
	s.stale.Store(stale)
	return stale
}

// Makes the search indexes again, empty, with their triggers, and starts backfills that fill them while writes go on
func (s *store) reindex(ctx context.Context) error {
	trigram := s.config.GetBool(sqliteconfig.CJKFTS)
	err := s.write(ctx, func(tx *sql.Tx) error {
		for _, i := range []index{wordIndex, trigramIndex} {
			if err := i.drop(ctx, tx); err != nil {
				return err
			}
		}
		if err := seed(ctx, tx, wordBackfill); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, ftsSchema); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM state_meta WHERE key IN (?, ?)`, trigramBackfill+"_high_water", trigramBackfill+"_progress"); err != nil {
			return err
		}
		if trigram {
			if err := seed(ctx, tx, trigramBackfill); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, trigramSchema); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM state_meta WHERE key = 'fts_stale'`)
		return err
	})
	if err == nil {
		s.stale.Store(false)
		s.trigram.Store(trigram)
	}
	return err
}

// Indexes the next chunk of a backfill's messages, finishing it once it reaches the highest; whether it is done and how far it got
func (s *store) chunk(ctx context.Context, i index, rows int) (bool, int64, int64, error) {
	var done bool
	var indexed, total int64
	err := s.write(ctx, func(tx *sql.Tx) error {
		from, high, running, err := progressOf(ctx, tx, i.prefix)
		if err != nil || !running {
			done, indexed, total = true, from, high
			return err
		}
		upper := min(from+int64(rows), high)
		if _, err := tx.ExecContext(ctx, `INSERT INTO `+i.table+`(rowid, `+i.columns+`) SELECT id, `+i.columns+` FROM `+i.source+` WHERE id > ? AND id <= ?`,
			from, upper); err != nil {
			return err
		}
		indexed, total = upper, high
		if upper >= high {
			done = true
			_, err := tx.ExecContext(ctx, `DELETE FROM state_meta WHERE key IN (?, ?)`, i.prefix+"_high_water", i.prefix+"_progress")
			return err
		}
		return setMeta(ctx, tx, i.prefix+"_progress", strconv.FormatInt(upper, 10))
	})
	return done, indexed, total, err
}

// Runs the backfills to their end, in chunks sized to a target time: paced in the background so writes barely notice, told how far it got
// in the foreground
func (s *store) fill(ctx context.Context, paced bool, progress func(label string, indexed, total int64)) error {
	target := foregroundCost
	if paced {
		target = backgroundCost
	}
	for _, i := range []index{wordIndex, trigramIndex} {
		rows := firstChunk
		for {
			started := time.Now()
			done, indexed, total, err := s.chunk(ctx, i, rows)
			if err != nil {
				return err
			}
			if progress != nil && total > 0 {
				progress(i.table, indexed, total)
			}
			if done {
				break
			}
			cost := time.Since(started)
			scale := min(2, max(0.5, float64(target)/float64(max(cost, 100*time.Microsecond))))
			rows = min(mostChunk, max(leastChunk, int(float64(rows)*scale)))
			if paced {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(max(leastRest, cost*restFactor)):
				}
			}
		}
		if i == trigramIndex {
			s.trigram.Store(s.config.GetBool(sqliteconfig.CJKFTS))
		}
	}
	return nil
}

// Whether the indexes still need work: detached after damage, or a backfill not finished
func (s *store) unmended(ctx context.Context, q querier) (bool, bool, error) {
	stale := s.isStale(ctx, q)
	for _, prefix := range []string{wordBackfill, trigramBackfill} {
		if _, _, running, err := progressOf(ctx, q, prefix); err != nil || running {
			return stale, running, err
		}
	}
	return stale, false, nil
}

// Rebuilds detached indexes and finishes backfills in the background, one process at a time, trying again later while another holds the
// work or it fails
func (s *store) mend(ctx context.Context) {
	if !s.mending.CompareAndSwap(false, true) {
		return
	}
	defer s.mending.Store(false)
	wait := time.Minute
	for {
		finished, err := s.mendOnce(ctx)
		if finished {
			return
		}
		if err != nil {
			slog.Warn("Rebuilding the session search index failed; searches scan the messages until it succeeds", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = min(wait*2, time.Hour)
	}
}

func (s *store) mendOnce(ctx context.Context) (bool, error) {
	db, err := s.read(ctx)
	if err != nil {
		return false, err
	}
	stale, running, err := s.unmended(ctx, db)
	if err != nil || !stale && !running {
		return err == nil, err
	}
	finished := false
	err = sqlitex.Locked(s.path+".fts_rebuild.lock", 0, func(locked bool) error {
		if !locked {
			return nil
		}
		if s.isStale(ctx, db) {
			slog.Warn("Rebuilding the detached session search index from the stored messages; searches scan them until it is done")
			if err := s.reindex(ctx); err != nil {
				return err
			}
		}
		if err := s.fill(ctx, true, nil); err != nil {
			return err
		}
		finished = true
		return nil
	})
	return finished, err
}

// Starts mending the indexes in the background of a running server, when it has started
func (s *store) remend() {
	s.mu.Lock()
	ctx := s.background
	s.mu.Unlock()
	if ctx != nil {
		go s.mend(ctx)
	}
}

func (s *store) Indexing(ctx context.Context) (*sessionsv1.Indexing, error) {
	db, err := s.read(ctx)
	if err != nil {
		return nil, err
	}
	indexed, total, running, err := progressOf(ctx, db, wordBackfill)
	if err != nil || !running {
		return nil, err
	}
	return &sessionsv1.Indexing{Indexed: indexed, Total: total, Percent: int32(min(100, indexed*100/total))}, nil
}

// Matches among the messages a backfill of the word index has yet to reach, found by scanning them for every word of the query
func (s *store) gap(ctx context.Context, db *sql.DB, query string, limit int, r *sessionsv1.SearchRequest) ([]*sessionsv1.Hit, error) {
	indexed, total, running, err := progressOf(ctx, db, wordBackfill)
	if err != nil || !running || limit <= 0 {
		return nil, err
	}
	var words []string
	for _, token := range likeToken.FindAllString(query, -1) {
		word := strings.TrimSpace(strings.Trim(strings.Trim(token, `"`), "*"))
		if word != "" && !strings.EqualFold(word, "AND") && !strings.EqualFold(word, "OR") && !strings.EqualFold(word, "NOT") && !strings.EqualFold(word, "NEAR") {
			words = append(words, word)
		}
	}
	if len(words) == 0 {
		return nil, nil
	}
	where := []string{"m.id > ? AND m.id <= ?"}
	args := []any{indexed, total}
	for _, word := range words {
		where = append(where, anyColumn)
		args = append(args, likeArgs(word)...)
	}
	whole := proto.Clone(r).(*sessionsv1.SearchRequest)
	whole.Offset = 0
	return s.like(ctx, db, words[0], where, args, "ORDER BY m.timestamp DESC", limit, whole)
}

// Hits of the index with those of the messages it has yet to reach after them, each message once, at most the limit
func merged(hits, more []*sessionsv1.Hit, limit int) []*sessionsv1.Hit {
	seen := map[int64]bool{}
	for _, hit := range hits {
		seen[hit.GetId()] = true
	}
	for _, hit := range more {
		if len(hits) >= limit {
			break
		}
		if !seen[hit.GetId()] {
			seen[hit.GetId()] = true
			hits = append(hits, hit)
		}
	}
	return hits
}

// Line telling how far a backfill got
func filled(label string, indexed, total int64) string {
	name := "the search index"
	if strings.Contains(label, "trigram") {
		name = "the search index for East Asian text"
	}
	return fmt.Sprintf("  Rebuilding %s: %d%% done, %s of %s messages.", name, min(100, indexed*100/max(total, 1)), strs.Number(float64(indexed)),
		strs.Number(float64(total)))
}
