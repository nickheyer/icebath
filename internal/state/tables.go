package state

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// Session table behavior the caller decides, from config.yaml and from how the process is wired
type SessionOptions struct {
	// The sessions.cjk_fts setting, the CJK bigram index staying on unless it is set to false
	CJKFullText *bool
	// Renews the startup watchdog lease before schema work that may run long, nil when no watchdog is armed
	StartupProgress func(expected time.Duration, phase string)
	// Transcript logic the agent loop owns and the store runs where a read or rewind needs it
	Transcript TranscriptHooks
	// Gateway media policy the store calls when it retires and collects retained media
	Media         MediaHooks
	Automation    AutomationHooks
	Gateway       GatewayHooks
	LocalSessions LocalSessionHooks
}

type AutomationHooks struct {
	Restate func(family, raw string) (status, encoded, cleared string, err error)
}

var ErrNoAutomationPolicy = errors.New("no automation state codec configured for the session store")

// Agent owned transcript logic the store calls at the points a replay read and a rewind need it
type TranscriptHooks struct {
	// Repairs role alternation of a loaded transcript, returning the repaired list and the repairs made
	RepairSequence func(messages []*Message) ([]*Message, int)
	// Reports internal recovery scaffolding that never reaches the durable transcript
	EphemeralScaffolding func(m *Message) bool
	// Projects live content to the text only form the flush stores
	DurableContent func(c Content) Content
	// Text a retry re-sends for a live user payload, an error when it cannot be replayed losslessly
	RetryableUserText func(c Content) (string, error)
	// Visible text of content for a composer prefill
	FlattenText func(c Content) string
}

// Reports whether the CJK bigram index may be served and kept in sync
func (o SessionOptions) cjkEnabled() bool { return o.CJKFullText == nil || *o.CJKFullText }

// Renews the startup watchdog lease when one is armed
func (o SessionOptions) progress(expected time.Duration, phase string) {
	if o.StartupProgress != nil {
		o.StartupProgress(expected, phase)
	}
}

// Session table state of one store
type tableState struct {
	options SessionOptions
	fts     ftsFlags
	// Local lineage indexes the listings reuse per connection
	lineage lineageCache
	// Column names of the messages table once read, guarded by columnsMu
	columnsMu      sync.Mutex
	messageColumns []string
	tokens         tokenWriter
}

// Full-text availability and recovery bookkeeping of one store, guarded by its own mutex
type ftsFlags struct {
	mu sync.Mutex
	// FTS5 queryable on this store
	enabled bool
	// A prior detach left the base and trigram indexes without sync triggers until a full rebuild
	stale bool
	// Trigram index queryable
	trigram bool
	// CJK tokenizer registered on the writer connection
	cjkLoaded bool
	// CJK index queryable and not marked stale
	cjkAvailable  bool
	fts5Warned    bool
	trigramWarned bool
	// Merge floor lowered once on this store
	usermergeApplied bool
	// Earliest next in-process stale recovery and the backoff that earned it
	retryAfter    time.Time
	retryInterval time.Duration
	// Holder pids that blocked the last deferral, nil when none did
	deferredPIDs []int
}

// Full-text indexes a store can serve right now
type FullTextStatus struct {
	// Base word index queryable and kept in sync
	Enabled bool
	// Indexes detached after corruption, search falling back to LIKE until a rebuild
	Stale bool
	// Trigram substring index queryable
	Trigram bool
	// CJK tokenizer registered on this store's connections
	CJKLoaded bool
	// CJK bigram index queryable and complete
	CJK bool
}

// Full-text indexes this store can serve
func (s *Store) FullText() FullTextStatus {
	f := &s.tables.state.fts
	f.mu.Lock()
	defer f.mu.Unlock()
	return FullTextStatus{Enabled: f.enabled, Stale: f.stale, Trigram: f.trigram, CJKLoaded: f.cjkLoaded, CJK: f.cjkAvailable}
}

// Full-text flags of the store
func (s *Store) fts() *ftsFlags { return &s.tables.state.fts }

// Updates the flags under their mutex
func (f *ftsFlags) set(fn func(f *ftsFlags)) {
	f.mu.Lock()
	fn(f)
	f.mu.Unlock()
}

// Reads one flag value under the mutex
func ftsGet[T any](f *ftsFlags, fn func(f *ftsFlags) T) T {
	f.mu.Lock()
	defer f.mu.Unlock()
	return fn(f)
}

// Registers the CJK tokenizer on the writer when enabled, and on each reader once the writer has it
func setupSessionConn(ctx context.Context, s *Store, c Conn, writer bool) error {
	st := s.tables.state
	if r, lent := c.(*runner); lent {
		r.c.sessions = st
	}
	if writer {
		loaded := false
		if st.options.cjkEnabled() {
			if err := registerCJKTokenizer(c); err != nil {
				slog.Warn(fmt.Sprintf("fts5_cjk tokenizer registration failed for %s: %s", s.path, errText(err)))
			} else {
				loaded = true
			}
		}
		st.fts.set(func(f *ftsFlags) { f.cjkLoaded = loaded })
		return nil
	}
	if !ftsGet(&st.fts, func(f *ftsFlags) bool { return f.cjkLoaded }) {
		return nil
	}
	return registerCJKTokenizer(c)
}

// Read-only attach probe of the base and trigram indexes, any probe failure closing the attach
func probeSessionTables(ctx context.Context, s *Store, c Conn) error {
	base, err := ftsTableProbe(ctx, c, s, "messages_fts")
	if err != nil {
		return err
	}
	enabled := base == probeQueryable
	trigram := false
	if enabled {
		tri, err := ftsTableProbe(ctx, c, s, "messages_fts_trigram")
		if err != nil {
			return err
		}
		trigram = tri == probeQueryable
	}
	s.fts().set(func(f *ftsFlags) { f.enabled, f.trigram = enabled, trigram })
	return nil
}
