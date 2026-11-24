package tuiserver

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/blake2b"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/skills"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tuihost"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	timeutil "github.com/nickheyer/icebath/pkg/utils/time"
)

// Active profile home of the watcher probes, the context override else the launch home
func watcherHome(ctx context.Context) string {
	if override := home.Override(ctx); override != "" {
		return override
	}
	return importedHome()
}

// st_mtime_ns of a path, false when it cannot be stat'ed
func watcherMtimeNS(path string) (int64, bool) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, false
	}
	return info.ModTime().UnixNano(), true
}

// Newest mtime across paths with unstat-able ones ignored, false when none stat'ed
var newestMtimeNS = newestStatMtimeNS

// Newest stat mtime across paths, false when none stat'ed
func newestStatMtimeNS(paths ...string) (int64, bool) {
	newest, found := int64(0), false
	for _, path := range paths {
		if ns, ok := watcherMtimeNS(path); ok && (!found || ns > newest) {
			newest, found = ns, true
		}
	}
	return newest, found
}

// Session columns whose change fires sessions.changed, activity stamps excluded so heartbeats stay silent
var sessionSignatureFields = []string{
	"id", "source", "session_key", "display_name", "model", "parent_session_id",
	"started_at", "ended_at", "end_reason", "message_count", "tool_call_count",
	"cwd", "git_branch", "git_repo_root", "title", "title_source", "profile_name",
	"archived", "pinned", "hidden", "last_read_at", "handoff_state",
}

// Cache key of a session store digest, the db and WAL mtime or a racy key no mtime equals
type dbSigKey struct {
	NS    int64
	Known bool
	Racy  bool
}

// Whether a cached key is the given mtime
func (k dbSigKey) matches(ns int64, known bool) bool {
	return !k.Racy && k.Known == known && (!known || k.NS == ns)
}

// Signature of a store whose sessions table could not be read, the bare mtime contract
type mtimeFallbackSig struct {
	NS    int64
	Known bool
}

// Digest of the session rows over the columns the store has
type sessionRowsSig struct {
	Fields string
	Digest [16]byte
}

// Per store digest cache keyed by path behind the db and WAL mtime
var sessionsDBSigCache = struct {
	sync.Mutex
	byPath map[string]sessionsDBSigEntry
}{byPath: map[string]sessionsDBSigEntry{}}

// One cached store digest
type sessionsDBSigEntry struct {
	key dbSigKey
	sig any
}

// Digest of the list and transcript relevant session rows of a store, nil when the store is absent
func sessionDBContentSig(dbPath string) any {
	ns, known := newestMtimeNS(dbPath, dbPath+"-wal")
	sessionsDBSigCache.Lock()
	cached, hasCached := sessionsDBSigCache.byPath[dbPath]
	sessionsDBSigCache.Unlock()
	if hasCached && cached.key.matches(ns, known) {
		return cached.sig
	}
	if _, err := os.Stat(dbPath); err != nil {
		sessionsDBSigCache.Lock()
		sessionsDBSigCache.byPath[dbPath] = sessionsDBSigEntry{dbSigKey{NS: ns, Known: known}, nil}
		sessionsDBSigCache.Unlock()
		return nil
	}
	signature, err := readSessionRowsSig(dbPath, ns, known)
	if err != nil {
		if hasCached && cached.sig != nil {
			if _, fallback := cached.sig.(mtimeFallbackSig); !fallback {
				return cached.sig
			}
		}
		signature = mtimeFallbackSig{ns, known}
	}
	sessionsDBSigCache.Lock()
	sessionsDBSigCache.byPath[dbPath] = sessionsDBSigEntry{settledMtime(ns, known), signature}
	sessionsDBSigCache.Unlock()
	return signature
}

// Reads the session rows of a store read only and digests their repr, the mtime contract for a foreign schema
func readSessionRowsSig(dbPath string, ns int64, known bool) (signature any, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%v", r)
		}
	}()
	uri, err := state.ReadOnlyDBURI(dbPath)
	if err != nil {
		return nil, err
	}
	conn, err := state.ConnectTrackedDB(uri, state.WithTrackingPath(dbPath), state.WithURI(), state.WithTimeout(0.05))
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	ctx := context.Background()
	infoRows, err := conn.QueryContext(ctx, "PRAGMA table_info(sessions)")
	if err != nil {
		return nil, err
	}
	available := map[string]bool{}
	for infoRows.Next() {
		var cid int64
		var name string
		var colType, dflt any
		var notNull, pk int64
		if err := infoRows.Scan(&cid, &name, &colType, &notNull, &dflt, &pk); err != nil {
			infoRows.Close()
			return nil, err
		}
		available[name] = true
	}
	if err := infoRows.Err(); err != nil {
		infoRows.Close()
		return nil, err
	}
	infoRows.Close()
	fields := []string{}
	for _, field := range sessionSignatureFields {
		if available[field] {
			fields = append(fields, field)
		}
	}
	if len(fields) == 0 {
		return mtimeFallbackSig{ns, known}, nil
	}
	order := ""
	if available["id"] {
		order = " ORDER BY id"
	}
	rows, err := conn.QueryContext(ctx, fmt.Sprintf("SELECT %s FROM sessions%s", strings.Join(fields, ", "), order))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	digest, err := blake2b.New(16, nil)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		values := make([]any, len(fields))
		targets := make([]any, len(fields))
		for i := range values {
			targets[i] = &values[i]
		}
		if err := rows.Scan(targets...); err != nil {
			return nil, err
		}
		digest.Write([]byte(anyutil.Quote(pyrepr.Tuple(values))))
		digest.Write([]byte{0})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var sum [16]byte
	copy(sum[:], digest.Sum(nil))
	return sessionRowsSig{strings.Join(fields, "\x00"), sum}, nil
}

// Cache key of an mtime old enough to trust, else a racy key no mtime equals
func settledMtime(ns int64, known bool) dbSigKey {
	if !known || config.MtimeIsRacy(ns) {
		return dbSigKey{Racy: true}
	}
	return dbSigKey{NS: ns, Known: true}
}

// Watcher home followed by every served profile home in a stable order
func watchedHomes(ctx context.Context) []string {
	servedProfileMu.Lock()
	served := make([]string, 0, len(servedProfileHomes))
	for h := range servedProfileHomes {
		served = append(served, h)
	}
	servedProfileMu.Unlock()
	slices.Sort(served)
	return append([]string{watcherHome(ctx)}, served...)
}

// Session table content across the active and served profile stores
func sessionsSig(ctx context.Context) any {
	sig := []any{}
	for _, root := range watchedHomes(ctx) {
		sig = append(sig, sessionDBContentSig(filepath.Join(root, "state.db")))
	}
	return sig
}

// Skill roots stat signature of every scope a session resolved commands in
func skillsSig(ctx context.Context) any {
	return skills.NewScope(home.Dir(ctx), nil).PublishedScopesSignature(ctx)
}

// One watched event, its check interval, signature and payload
type changeWatch struct {
	event    string
	interval float64
	sig      func(ctx context.Context) any
	payload  func(ctx context.Context) map[string]any
}

// Empty event payload
func emptyChangePayload(context.Context) map[string]any { return map[string]any{} }

// Watched events in their broadcast order with the stat cheap signatures that drive them, pet.changed first where pets are linked
var changeWatches = []changeWatch{
	{"sessions.changed", 0.5, sessionsSig, emptyChangePayload},
	{"skills.changed", 2.0, skillsSig, emptyChangePayload},
}

// Minimum seconds between two broadcasts of a bursty event, its trailing edge kept
var changeBroadcastFloorS = map[string]float64{"sessions.changed": 2.0}

// Last signature, check time and broadcast time of each watched event
var changeState = struct {
	sync.Mutex
	sigs        map[string]any
	checkedAt   map[string]float64
	broadcastAt map[string]float64
}{sigs: map[string]any{}, checkedAt: map[string]float64{}, broadcastAt: map[string]float64{}}

// Signature of one watch, false when its probe failed
func watchSignature(ctx context.Context, watch changeWatch) (sig any, ok bool) {
	defer func() {
		if recover() != nil {
			sig, ok = nil, false
		}
	}()
	return watch.sig(ctx), true
}

// One pass recomputing due signatures and broadcasting each event whose signature moved, first sight seeding silently
func broadcastWatchedChanges(ctx context.Context, now float64) {
	for _, watch := range changeWatches {
		changeState.Lock()
		last, checked := changeState.checkedAt[watch.event]
		if !checked {
			last = -watch.interval
		}
		if now-last < watch.interval {
			changeState.Unlock()
			continue
		}
		changeState.checkedAt[watch.event] = now
		changeState.Unlock()
		sig, ok := watchSignature(ctx, watch)
		if !ok {
			continue
		}
		changeState.Lock()
		previous, seen := changeState.sigs[watch.event]
		if !seen {
			changeState.sigs[watch.event] = sig
			changeState.Unlock()
			continue
		}
		if reflect.DeepEqual(sig, previous) {
			changeState.Unlock()
			continue
		}
		floor := changeBroadcastFloorS[watch.event]
		lastBroadcast, broadcasted := changeState.broadcastAt[watch.event]
		if !broadcasted {
			lastBroadcast = -floor
		}
		if floor != 0 && now-lastBroadcast < floor {
			changeState.Unlock()
			continue
		}
		changeState.sigs[watch.event] = sig
		changeState.broadcastAt[watch.event] = now
		changeState.Unlock()
		func() {
			defer func() { recover() }()
			broadcastGlobalEvent(watch.event, watch.payload(ctx))
		}()
	}
}

// Whether the process change watcher goroutine was started
var skinWatcherStarted = struct {
	sync.Mutex
	started bool
}{}

// Interval between two passes of the change watcher
var changeWatcherTick = 500 * time.Millisecond

// Starts the one change watcher of the process that turns on-disk signatures into broadcast events
func ensureSkinWatcher(ctx context.Context) {
	skinWatcherStarted.Lock()
	if skinWatcherStarted.started {
		skinWatcherStarted.Unlock()
		return
	}
	skinWatcherStarted.started = true
	skinWatcherStarted.Unlock()
	func() {
		defer func() { recover() }()
		tuihost.NoteSkinBroadcast(ctx)
	}()
	go func() {
		loop := context.Background()
		for {
			time.Sleep(changeWatcherTick)
			tuihost.BroadcastSkinIfChanged(loop)
			broadcastWatchedChanges(loop, timeutil.Monotonic())
		}
	}()
}
