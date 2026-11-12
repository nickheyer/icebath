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
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/pet"
	"github.com/nickheyer/icebath/pkg/skills"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tuigateway"
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

// mtime of a path under the watcher home as a signature, nil when it cannot be stat'ed
func homeMtimeNS(ctx context.Context, parts ...string) any {
	if ns, ok := watcherMtimeNS(filepath.Join(append([]string{watcherHome(ctx)}, parts...)...)); ok {
		return ns
	}
	return nil
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

// One stat-able file of a multi file signature
type watchedFile struct {
	Path    string
	MtimeNS int64
	Size    int64
}

// Sorted path, mtime and size of every stat-able file so a same tick add or rewrite still moves it
func filesSig(paths []string) []watchedFile {
	entries := []watchedFile{}
	for _, path := range paths {
		if info, err := os.Stat(path); err == nil {
			entries = append(entries, watchedFile{path, info.ModTime().UnixNano(), info.Size()})
		}
	}
	slices.SortFunc(entries, func(a, b watchedFile) int {
		if c := strings.Compare(a.Path, b.Path); c != 0 {
			return c
		}
		if a.MtimeNS != b.MtimeNS {
			return compareInt64(a.MtimeNS, b.MtimeNS)
		}
		return compareInt64(a.Size, b.Size)
	})
	return entries
}

// Ordering of two int64 values
func compareInt64(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// Session columns whose change drives Desktop projections, activity stamps excluded so heartbeats stay silent
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
		digest.Write([]byte(pyrepr.Repr(pyrepr.Tuple(values))))
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

// Per file stat signature of projects.db and its WAL for the watcher home and every served profile
func projectsSig(ctx context.Context) any {
	paths := []string{}
	for _, root := range watchedHomes(ctx) {
		paths = append(paths, filepath.Join(root, "projects.db"), filepath.Join(root, "projects.db-wal"))
	}
	return filesSig(paths)
}

// Per file stat signature of every live profile's pending and approved pairing ledgers
func pairingSig(ctx context.Context) any {
	entries := []string{}
	for _, root := range pairingRoots(watcherHome(ctx)) {
		dirEntries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, entry := range dirEntries {
			if name := entry.Name(); strings.HasSuffix(name, "-pending.json") || strings.HasSuffix(name, "-approved.json") {
				entries = append(entries, filepath.Join(root, name))
			}
		}
	}
	return filesSig(entries)
}

// Seconds a cached live profile pairing root list stays trusted
var pairingRootsTTLSeconds = 30.0

// Liveness probe of a named profile dir, an existing dir with identity that was not deleted
var namedProfileIsLive = home.ProfileLive

// Live profile pairing roots cached on the home, the profiles dir mtime and a TTL
var pairingRootsCache = struct {
	sync.Mutex
	set      bool
	home     string
	dirNS    int64
	dirKnown bool
	at       float64
	roots    []string
}{}

// Pairing ledger dirs of the home and every live named profile under it
func pairingRoots(h string) []string {
	profilesDir := filepath.Join(h, "profiles")
	dirNS, dirKnown := watcherMtimeNS(profilesDir)
	now := pythread.Monotonic()
	pairingRootsCache.Lock()
	defer pairingRootsCache.Unlock()
	if c := &pairingRootsCache; c.set && c.home == h && c.dirKnown == dirKnown && c.dirNS == dirNS && now-c.at < pairingRootsTTLSeconds {
		return c.roots
	}
	roots := []string{filepath.Join(h, "pairing"), filepath.Join(h, "platforms", "pairing")}
	if entries, err := os.ReadDir(profilesDir); err == nil {
		for _, entry := range entries {
			profileDir := filepath.Join(profilesDir, entry.Name())
			if namedProfileIsLive(profileDir) {
				roots = append(roots, filepath.Join(profileDir, "pairing"), filepath.Join(profileDir, "platforms", "pairing"))
			}
		}
	}
	pairingRootsCache.set, pairingRootsCache.home = true, h
	pairingRootsCache.dirNS, pairingRootsCache.dirKnown = dirNS, dirKnown
	pairingRootsCache.at, pairingRootsCache.roots = now, roots
	return roots
}

// Newest bot relay outbox envelope mtime ever seen, monotone so a drain never fires a spurious pending
var botRelayOutboxSeen = struct {
	sync.Mutex
	ns int64
}{}

// Monotone newest mtime across pending bot relay outbox envelopes, nil before any was seen
func botRelayOutboxSig(ctx context.Context) any {
	h := watcherHome(ctx)
	root := h
	if filepath.Base(filepath.Dir(h)) == "profiles" {
		root = filepath.Dir(filepath.Dir(h))
	}
	botRelayOutboxSeen.Lock()
	defer botRelayOutboxSeen.Unlock()
	outbox := filepath.Join(root, "bot_relay", "outbox")
	if entries, err := os.ReadDir(outbox); err == nil {
		for _, entry := range entries {
			if strings.HasSuffix(entry.Name(), ".json") {
				ns, _ := watcherMtimeNS(filepath.Join(outbox, entry.Name()))
				botRelayOutboxSeen.ns = max(botRelayOutboxSeen.ns, ns)
			}
		}
	}
	if botRelayOutboxSeen.ns == 0 {
		return nil
	}
	return botRelayOutboxSeen.ns
}

// Skill roots stat signature of every scope a session resolved commands in
func skillsSig(ctx context.Context) any {
	return skills.NewScope(home.Dir(ctx), nil).PublishedScopesSignature(ctx)
}

// Enabled pet with an existing sheet and its scale, nil when none renders
func activePet(ctx context.Context) (*pet.InstalledPet, float64, error) {
	enabled, selected, scale, err := petActiveSelection(ctx)
	if err != nil || !enabled || selected == nil || !selected.Exists() {
		return nil, 0, err
	}
	return selected, scale, nil
}

// Slug, spritesheet revision and scale of the active pet, the off signature when none renders
func petSig(ctx context.Context) (sig any) {
	off := []any{"off"}
	rawDisplay := loadCfg(ctx)["display"]
	display, isDict := rawDisplay.(map[string]any)
	if truthy(rawDisplay) && !isDict {
		panic(fmt.Sprintf("AttributeError: '%s' object has no attribute 'get'", pyrepr.TypeName(rawDisplay)))
	}
	petConf, _ := display["pet"].(map[string]any)
	if len(petConf) == 0 || !isTruthyValueDefault(petConf["enabled"], false) {
		return off
	}
	defer func() {
		if recover() != nil {
			sig = off
		}
	}()
	selected, scale, err := activePet(ctx)
	if err != nil || selected == nil {
		return off
	}
	return []any{selected.Slug, petSheetRevision(selected.Spritesheet), scale}
}

// is_truthy_value with an explicit default for an absent value
func isTruthyValueDefault(v any, def bool) bool {
	if v == nil {
		return def
	}
	if s, ok := v.(string); ok {
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "1", "true", "yes", "on":
			return true
		}
		return false
	}
	return truthy(v)
}

// pet.info.meta shaped payload so the renderer decides whether to refetch sprites
func petChangedPayload(ctx context.Context) (payload map[string]any) {
	defer func() {
		if recover() != nil {
			payload = map[string]any{"enabled": false}
		}
	}()
	selected, scale, err := activePet(ctx)
	if err != nil || selected == nil {
		return map[string]any{"enabled": false}
	}
	return map[string]any{"enabled": true, "slug": selected.Slug, "displayName": selected.DisplayName,
		"scale": scale, "spritesheetRevision": petSheetRevision(selected.Spritesheet)}
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

// Watched events in their broadcast order with the stat cheap signatures that drive them
var changeWatches = []changeWatch{
	{"pet.changed", 2.0, petSig, petChangedPayload},
	{"cron.changed", 1.0, func(ctx context.Context) any { return homeMtimeNS(ctx, "cron", "jobs.json") }, emptyChangePayload},
	{"sessions.changed", 0.5, sessionsSig, emptyChangePayload},
	{"platforms.changed", 2.0, func(ctx context.Context) any { return homeMtimeNS(ctx, "gateway_state.json") }, emptyChangePayload},
	{"projects.changed", 2.0, projectsSig, emptyChangePayload},
	{"pairing.changed", 2.0, pairingSig, emptyChangePayload},
	{"skills.changed", 2.0, skillsSig, emptyChangePayload},
	{"bot_relay.outbox.pending", 1.0, botRelayOutboxSig, emptyChangePayload},
}

// Minimum seconds between two broadcasts of a bursty event, its trailing edge kept
var changeBroadcastFloorS = map[string]float64{"sessions.changed": 2.0, "platforms.changed": 5.0}

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
		tuigateway.NoteSkinBroadcast(ctx)
	}()
	go func() {
		loop := context.Background()
		for {
			time.Sleep(changeWatcherTick)
			tuigateway.BroadcastSkinIfChanged(loop)
			broadcastWatchedChanges(loop, pythread.Monotonic())
		}
	}()
}
