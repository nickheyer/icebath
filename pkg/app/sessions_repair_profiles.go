package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/backup"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/logging"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/state"
)

var (
	repairProfilesLog           = logging.GetLogger("hermes_cli.sessions_repair_profiles")
	repairLegacyMainChoices     = []string{"report", "rekey", "move"}
	repairVoiceModeFile         = "gateway_voice_mode.json"
	errRepairEmptyStoreWritable = errors.New("AttributeError: '_EmptyStore' object has no attribute for a repair")
)

// One profile state.db, routing marking the store owning the multiplexer index
type repairProfilesStore struct {
	profile, home string
	routing       bool
}

// Path of the store database
func (s repairProfilesStore) dbPath() string { return filepath.Join(s.home, "state.db") }

// Read surface a scan uses, a real store or the empty stand in for a profile without a database
type repairScanDB interface {
	FindCrossedProfileSessions(ctx context.Context, owner string) (*state.CrossedProfileSessions, error)
	FindProfileLessTelegramTopicRows(ctx context.Context) ([]*pyjson.Object, error)
	ListGatewayRoutingRows(ctx context.Context) ([]*pyjson.Object, error)
	KeyProfilesForChat(ctx context.Context, platform, chatID string) (map[string]bool, error)
}

// Stand in for a live profile whose state.db does not exist yet, finding nothing
type repairEmptyStore struct{}

func (repairEmptyStore) FindCrossedProfileSessions(context.Context, string) (*state.CrossedProfileSessions, error) {
	return &state.CrossedProfileSessions{}, nil
}
func (repairEmptyStore) FindProfileLessTelegramTopicRows(context.Context) ([]*pyjson.Object, error) {
	return nil, nil
}
func (repairEmptyStore) ListGatewayRoutingRows(context.Context) ([]*pyjson.Object, error) {
	return nil, nil
}
func (repairEmptyStore) KeyProfilesForChat(context.Context, string, string) (map[string]bool, error) {
	return map[string]bool{}, nil
}

// Store handles of one run, each store opened at most once and all released at the end
type repairProfilesSession struct {
	readOnly bool
	ctx      context.Context
	dbs      map[string]repairScanDB
	order    []string
}

// Opens each store once, a missing database reading as empty on a read only run
func (s *repairProfilesSession) db(store repairProfilesStore) (repairScanDB, error) {
	path := store.dbPath()
	if db, ok := s.dbs[path]; ok {
		return db, nil
	}
	var db repairScanDB
	if s.readOnly {
		if _, err := os.Stat(path); err != nil {
			db = repairEmptyStore{}
		} else {
			opened, err := state.NewSessionDB(s.ctx, path, true)
			if err != nil {
				return nil, err
			}
			db = opened
		}
	} else {
		acquired, err := state.Acquire(s.ctx, path)
		if err != nil {
			return nil, err
		}
		db = acquired
	}
	s.dbs[path] = db
	s.order = append(s.order, path)
	return db, nil
}

// Real store database a fix writes through
func (s *repairProfilesSession) writable(store repairProfilesStore) (*state.SessionDB, error) {
	db, err := s.db(store)
	if err != nil {
		return nil, err
	}
	real, ok := db.(*state.SessionDB)
	if !ok {
		return nil, errRepairEmptyStoreWritable
	}
	return real, nil
}

// Releases every opened store, closing read only handles and releasing shared ones
func (s *repairProfilesSession) close() {
	for _, path := range s.order {
		real, ok := s.dbs[path].(*state.SessionDB)
		if !ok {
			continue
		}
		func() {
			defer func() { recover() }()
			if s.readOnly {
				real.Close()
			} else {
				state.ReleaseOrClose(real)
			}
		}()
	}
	s.dbs = map[string]repairScanDB{}
	s.order = nil
}

// One crossing a scan found, with the repair apply performs or the reason it is report only
type repairProfilesFinding struct {
	kind, store, subject, detail string
	action, reason               *string
	fix                          func(*repairProfilesSession) (*pyjson.Object, error)
}

// Report dict of the finding
func (f *repairProfilesFinding) asDict() *pyjson.Object {
	optional := func(s *string) any {
		if s == nil {
			return nil
		}
		return *s
	}
	return pyjson.ObjectOf("kind", f.kind, "store", f.store, "subject", f.subject, "detail", f.detail,
		"action", optional(f.action), "reason", optional(f.reason))
}

// Pointer to a copy of text for optional finding fields
func strRef(s string) *string { return &s }

// Default root store plus every live named profile, the root owning the routing index
func enumerateStores(ctx context.Context) ([]repairProfilesStore, error) {
	stores := []repairProfilesStore{{profile: "default", home: profiles.DefaultHome(), routing: true}}
	dirs, err := profiles.IterNamedProfileDirs(true)
	if err != nil {
		return nil, err
	}
	for _, dir := range dirs {
		stores = append(stores, repairProfilesStore{profile: filepath.Base(dir), home: dir})
	}
	return stores, nil
}

// Whether the default gateway serves every profile, false on any failure to tell
func gatewayMultiplexes(ctx context.Context, root string) bool {
	on, err := gateway.DefaultGatewayMultiplexes(ctx, root)
	return err == nil && on
}

// Profile and pid of a live gateway owning one of the stores
type liveGatewayHome struct {
	profile string
	pid     int
}

// Stores whose home a verified live gateway owns
func liveGatewayHomes(ctx context.Context, stores []repairProfilesStore) ([]liveGatewayHome, error) {
	var live []liveGatewayHome
	for _, store := range stores {
		pid, ok, err := gateway.LiveGatewayPidForHome(store.home)
		if err != nil {
			return nil, err
		}
		if ok {
			live = append(live, liveGatewayHome{store.profile, pid})
		}
	}
	return live, nil
}

// Findings of one scan across the stores and the move batches their fixes share
type repairProfilesPlan struct {
	stores       []repairProfilesStore
	byProfile    map[string]repairProfilesStore
	claimed      map[string]bool
	routingStore *repairProfilesStore
	legacyMain   string
	multiplexes  bool
	findings     []*repairProfilesFinding
	moves        map[[2]string]*repairMoveBatch
}

// Plan over the stores, refusing an unknown legacy main policy
func newRepairProfilesPlan(ctx context.Context, stores []repairProfilesStore, legacyMain string) (*repairProfilesPlan, error) {
	valid := false
	for _, c := range repairLegacyMainChoices {
		valid = valid || c == legacyMain
	}
	if !valid {
		return nil, &appValueError{"legacy_main must be one of ('report', 'rekey', 'move')"}
	}
	p := &repairProfilesPlan{stores: stores, byProfile: map[string]repairProfilesStore{}, claimed: map[string]bool{},
		legacyMain: legacyMain, moves: map[[2]string]*repairMoveBatch{}}
	for _, s := range stores {
		p.byProfile[s.profile] = s
		p.claimed[s.profile] = true
	}
	for i := range stores {
		if stores[i].routing {
			p.routingStore = &stores[i]
			break
		}
	}
	if p.routingStore != nil {
		p.multiplexes = gatewayMultiplexes(ctx, p.routingStore.home)
	}
	return p, nil
}

func (p *repairProfilesPlan) add(f *repairProfilesFinding) { p.findings = append(p.findings, f) }

// Findings apply can repair
func (p *repairProfilesPlan) repairable() []*repairProfilesFinding {
	var out []*repairProfilesFinding
	for _, f := range p.findings {
		if f.fix != nil {
			out = append(out, f)
		}
	}
	return out
}

// Scans sessions and topics of every store, then routing, then the routing store voice modes and mirror
func (p *repairProfilesPlan) scan(ctx context.Context, session *repairProfilesSession) error {
	for _, store := range p.stores {
		if err := p.scanSessions(ctx, session, store); err != nil {
			return err
		}
		if err := p.scanTopics(ctx, session, store); err != nil {
			return err
		}
	}
	for _, store := range p.stores {
		if err := p.scanRouting(ctx, session, store); err != nil {
			return err
		}
	}
	if p.routingStore != nil {
		if err := p.scanVoiceModes(ctx, session, *p.routingStore); err != nil {
			return err
		}
		if err := p.scanSessionsJSON(*p.routingStore); err != nil {
			return err
		}
	}
	return nil
}

// Python repr of a row value
func rowRepr(row *pyjson.Object, key string) string { return pyrepr.Repr(rowGet(row, key)) }

// Python str of a row value
func rowStr(row *pyjson.Object, key string) string { return pyrepr.Str(rowGet(row, key)) }

// Python key of a row value usable in sets and dict lookups
func rowKey(row *pyjson.Object, key string) string { return pyrepr.Str(rowGet(row, key)) }

// Plans the wrong store, mislabelled and crossed parent findings of one store
func (p *repairProfilesPlan) scanSessions(ctx context.Context, session *repairProfilesSession, store repairProfilesStore) error {
	db, err := session.db(store)
	if err != nil {
		return err
	}
	crossed, err := db.FindCrossedProfileSessions(ctx, store.profile)
	if err != nil {
		return err
	}
	moving := map[string]bool{}
	var candidates []*pyjson.Object
	for _, r := range crossed.Foreign {
		if p.wouldMove(store, r) {
			candidates = append(candidates, r)
		}
	}
	held := ledgeredLineages(candidates)
	for _, row := range crossed.Foreign {
		id := rowKey(row, "id")
		if reason, ok := held[id]; ok {
			moving[id] = true
			p.add(&repairProfilesFinding{kind: "wrong_store", store: store.profile, subject: id,
				detail: fmt.Sprintf("key %s (%s messages) sits in the %s store", rowRepr(row, "session_key"),
					rowStr(row, "message_count"), store.profile), reason: strRef(reason)})
			continue
		}
		p.planForeignRow(store, row, moving)
	}
	for _, row := range crossed.Mislabelled {
		sid := rowKey(row, "id")
		if moving[sid] {
			continue
		}
		st := store
		p.add(&repairProfilesFinding{kind: "mislabelled", store: store.profile, subject: sid,
			detail: fmt.Sprintf("profile_name=%s but key %s belongs to %s", rowRepr(row, "profile_name"),
				rowRepr(row, "session_key"), rowRepr(row, "key_profile")),
			action: strRef("relabel to " + rowRepr(row, "key_profile")),
			fix: func(s *repairProfilesSession) (*pyjson.Object, error) {
				db, err := s.writable(st)
				if err != nil {
					return nil, err
				}
				n, err := db.RelabelSessionsToKeyProfile(s.ctx, []string{sid})
				if err != nil {
					return nil, err
				}
				return pyjson.ObjectOf("relabelled", n), nil
			}})
	}
	for _, row := range crossed.CrossedParents {
		sid := rowKey(row, "id")
		if moving[sid] {
			continue
		}
		st := store
		p.add(&repairProfilesFinding{kind: "crossed_parent", store: store.profile, subject: sid,
			detail: fmt.Sprintf("%s row inherits from parent %s keyed under %s", rowRepr(row, "key_profile"),
				rowStr(row, "parent_session_id"), rowRepr(row, "parent_profile")),
			action: strRef("sever parent_session_id"),
			fix: func(s *repairProfilesSession) (*pyjson.Object, error) {
				db, err := s.writable(st)
				if err != nil {
					return nil, err
				}
				n, err := db.SeverCrossedParents(s.ctx, []string{sid})
				if err != nil {
					return nil, err
				}
				return pyjson.ObjectOf("severed", n), nil
			}})
	}
	return nil
}

// Whether the plan copies this foreign row to another store
func (p *repairProfilesPlan) wouldMove(store repairProfilesStore, row *pyjson.Object) bool {
	keyProfile := rowGet(row, "key_profile")
	if keyProfile == "default" && store.profile != "default" {
		return p.legacyMain == "move" && p.routingStore != nil
	}
	s, ok := keyProfile.(string)
	return ok && p.claimed[s]
}

// Plans a row keyed to another profile, a move into its store or a report when no profile claims it
func (p *repairProfilesPlan) planForeignRow(store repairProfilesStore, row *pyjson.Object, moving map[string]bool) {
	keyProfile, sid := rowGet(row, "key_profile"), rowKey(row, "id")
	detail := fmt.Sprintf("key %s (%s messages) sits in the %s store", rowRepr(row, "session_key"),
		rowStr(row, "message_count"), store.profile)
	if keyProfile == "default" && store.profile != "default" {
		p.planLegacyMainRow(store, row, moving)
		return
	}
	name, _ := keyProfile.(string)
	if !p.claimed[name] {
		p.add(&repairProfilesFinding{kind: "unclaimed_namespace", store: store.profile, subject: sid, detail: detail,
			reason: strRef(fmt.Sprintf("no live profile named %s; create it, then rerun, or "+
				"`icebath profile migrate-identity %s <target>`", pyrepr.Repr(keyProfile), pyrepr.Str(keyProfile)))})
		return
	}
	moving[sid] = true
	p.add(&repairProfilesFinding{kind: "wrong_store", store: store.profile, subject: sid, detail: detail,
		action: strRef("move to the " + name + " store"), fix: p.moveBatch(store, p.byProfile[name]).fixFor(sid)})
}

// Plans a legacy default namespace row in a named store by the legacy main policy
func (p *repairProfilesPlan) planLegacyMainRow(store repairProfilesStore, row *pyjson.Object, moving map[string]bool) {
	sid := rowKey(row, "id")
	detail := fmt.Sprintf("legacy default-namespace key %s (%s messages) in the %s store",
		rowRepr(row, "session_key"), rowStr(row, "message_count"), store.profile)
	switch {
	case p.legacyMain == "rekey":
		st := store
		p.add(&repairProfilesFinding{kind: "legacy_main", store: store.profile, subject: sid, detail: detail,
			action: strRef("rekey to agent:" + store.profile + ":"),
			fix: func(s *repairProfilesSession) (*pyjson.Object, error) {
				db, err := s.writable(st)
				if err != nil {
					return nil, err
				}
				n, err := db.RekeyLegacyMainSessions(s.ctx, []string{sid}, st.profile)
				if err != nil {
					return nil, err
				}
				return pyjson.ObjectOf("rekeyed", n), nil
			}})
	case p.legacyMain == "move" && p.routingStore != nil:
		moving[sid] = true
		p.add(&repairProfilesFinding{kind: "legacy_main", store: store.profile, subject: sid, detail: detail,
			action: strRef("move to the default store"), fix: p.moveBatch(store, *p.routingStore).fixFor(sid)})
	default:
		p.add(&repairProfilesFinding{kind: "legacy_main", store: store.profile, subject: sid, detail: detail,
			reason: strRef("either a standalone gateway's own history (--legacy-main rekey) or a default " +
				"chat that leaked in (--legacy-main move); the row cannot say which")})
	}
}

// Shared move batch between two stores
func (p *repairProfilesPlan) moveBatch(src, dst repairProfilesStore) *repairMoveBatch {
	key := [2]string{src.dbPath(), dst.dbPath()}
	if b, ok := p.moves[key]; ok {
		return b
	}
	b := &repairMoveBatch{src: src, dst: dst, errs: map[string]error{}}
	p.moves[key] = b
	return b
}

// Plans the relabel of profile less Telegram topic bindings of one store
func (p *repairProfilesPlan) scanTopics(ctx context.Context, session *repairProfilesSession, store repairProfilesStore) error {
	db, err := session.db(store)
	if err != nil {
		return err
	}
	rows, err := db.FindProfileLessTelegramTopicRows(ctx)
	if err != nil {
		return err
	}
	for _, row := range rows {
		st, r := store, row
		p.add(&repairProfilesFinding{kind: "topic_profile_less", store: store.profile,
			subject: fmt.Sprintf("chat %s thread %s", rowStr(row, "chat_id"), rowStr(row, "thread_id")),
			detail:  "telegram topic binding labelled 'default' but keyed " + rowRepr(row, "session_key"),
			action:  strRef("relabel to " + rowRepr(row, "key_profile")),
			fix: func(s *repairProfilesSession) (*pyjson.Object, error) {
				db, err := s.writable(st)
				if err != nil {
					return nil, err
				}
				return db.RelabelTelegramTopicRows(s.ctx, []*pyjson.Object{r})
			}})
	}
	return nil
}

// Plans routing rows outside the routing store and rows for namespaces no profile claims
func (p *repairProfilesPlan) scanRouting(ctx context.Context, session *repairProfilesSession, store repairProfilesStore) error {
	db, err := session.db(store)
	if err != nil {
		return err
	}
	rows, err := db.ListGatewayRoutingRows(ctx)
	if err != nil {
		return err
	}
	for _, row := range rows {
		keyProfile, ok := state.SessionKeyProfile(rowGet(row, "session_key"))
		if !ok {
			continue
		}
		subject := fmt.Sprintf("%s (scope %s)", rowStr(row, "session_key"), rowRepr(row, "scope"))
		st, r := store, row
		if store.routing {
			if !p.claimed[keyProfile] {
				p.add(&repairProfilesFinding{kind: "routing_unclaimed", store: store.profile, subject: subject,
					detail: fmt.Sprintf("routing row for a namespace no live profile claims (%s); every "+
						"inbound event on it logs a missing-profile error", pyrepr.Repr(keyProfile)),
					action: strRef("delete"),
					fix: func(s *repairProfilesSession) (*pyjson.Object, error) {
						db, err := s.writable(st)
						if err != nil {
							return nil, err
						}
						n, err := db.DeleteGatewayRoutingRows(s.ctx, [][2]string{{rowStr(r, "scope"), rowStr(r, "session_key")}})
						if err != nil {
							return nil, err
						}
						return pyjson.ObjectOf("routing_deleted", n), nil
					}})
			}
			continue
		}
		if keyProfile == store.profile && !p.multiplexes {
			continue
		}
		if p.routingStore == nil {
			continue
		}
		rs := *p.routingStore
		p.add(&repairProfilesFinding{kind: "routing_stray", store: store.profile, subject: subject,
			detail: "routing row in the " + store.profile + " store; the index lives in the default store",
			action: strRef("move to the default store (existing row there wins)"),
			fix: func(s *repairProfilesSession) (*pyjson.Object, error) {
				return moveRoutingRow(s, st, rs, r)
			}})
	}
	return nil
}

// Copies a stray routing row into the routing store when absent there, then deletes it from its store
func moveRoutingRow(session *repairProfilesSession, store, routingStore repairProfilesStore, row *pyjson.Object) (*pyjson.Object, error) {
	dst, err := session.writable(routingStore)
	if err != nil {
		return nil, err
	}
	updatedAt, err := pyint.Float(rowGet(row, "updated_at"))
	if err != nil {
		return nil, err
	}
	adopted, err := dst.InsertGatewayRoutingRowsIfAbsent(session.ctx, []state.GatewayRoutingRow{{Scope: rowStr(row, "scope"),
		SessionKey: rowStr(row, "session_key"), EntryJSON: rowStr(row, "entry_json"), UpdatedAt: updatedAt}})
	if err != nil {
		return nil, err
	}
	src, err := session.writable(store)
	if err != nil {
		return nil, err
	}
	deleted, err := src.DeleteGatewayRoutingRows(session.ctx, [][2]string{{rowStr(row, "scope"), rowStr(row, "session_key")}})
	if err != nil {
		return nil, err
	}
	return pyjson.ObjectOf("routing_adopted", adopted, "routing_deleted", deleted), nil
}

// JSON document of a file read as utf-8-sig, false when missing, unreadable or malformed
func repairReadJSON(path string) (any, bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, false, nil
	}
	text, err := pystr.DecodeUTF8Sig(raw)
	if err != nil {
		return nil, false, err
	}
	data, err := pyjson.Loads(text)
	if err != nil {
		var decodeErr *pyjson.DecodeError
		if errors.As(err, &decodeErr) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return data, true, nil
}

// Plans the rekey of unprefixed voice mode entries a single non default bot holds
func (p *repairProfilesPlan) scanVoiceModes(ctx context.Context, session *repairProfilesSession, store repairProfilesStore) error {
	path := filepath.Join(store.home, repairVoiceModeFile)
	loaded, ok, err := repairReadJSON(path)
	if err != nil || !ok {
		return err
	}
	data, isDict := loaded.(*pyjson.Object)
	if !isDict {
		return nil
	}
	var keys []string
	for _, k := range data.Keys {
		if strings.Count(k, ":") == 1 {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		platform, chatID, _ := strings.Cut(key, ":")
		owners := map[string]bool{}
		for _, st := range p.stores {
			db, err := session.db(st)
			if err != nil {
				return err
			}
			got, err := db.KeyProfilesForChat(ctx, platform, chatID)
			if err != nil {
				return err
			}
			for o := range got {
				owners[o] = true
			}
		}
		if len(owners) == 0 || owners["default"] {
			continue
		}
		names := make([]string, 0, len(owners))
		for o := range owners {
			names = append(names, o)
		}
		sort.Strings(names)
		if len(names) > 1 {
			list := make([]any, len(names))
			for i, n := range names {
				list[i] = n
			}
			p.add(&repairProfilesFinding{kind: "voice_profile_less", store: store.profile, subject: key,
				detail: "voice mode entry without a profile; chat is held by " + pyrepr.Repr(list),
				reason: strRef("more than one non-default bot speaks in this chat")})
			continue
		}
		owner, k := names[0], key
		p.add(&repairProfilesFinding{kind: "voice_profile_less", store: store.profile, subject: key,
			detail: fmt.Sprintf("voice mode %s without a profile; chat is held only by %s", pyrepr.Repr(data.Vals[key]),
				pyrepr.Repr(owner)),
			action: strRef("rekey to " + owner + ":" + key),
			fix: func(s *repairProfilesSession) (*pyjson.Object, error) {
				return rekeyVoiceModeEntry(s.ctx, path, k, owner)
			}})
	}
	return nil
}

// Plans the drop of sessions.json mirror entries whose namespace no live profile claims
func (p *repairProfilesPlan) scanSessionsJSON(store repairProfilesStore) error {
	path := filepath.Join(store.home, "sessions", "sessions.json")
	loaded, ok, err := repairReadJSON(path)
	if err != nil || !ok {
		return err
	}
	data, isDict := loaded.(*pyjson.Object)
	if !isDict {
		return nil
	}
	var keys []string
	for _, k := range data.Keys {
		if !strings.HasPrefix(k, "_") {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		keyProfile, ok := state.SessionKeyProfile(key)
		if !ok || p.claimed[keyProfile] {
			continue
		}
		k := key
		p.add(&repairProfilesFinding{kind: "sessions_json_unclaimed", store: store.profile, subject: key,
			detail: fmt.Sprintf("sessions.json mirror entry for a namespace no live profile claims (%s); "+
				"the legacy import re-injects it into routing on every boot", pyrepr.Repr(keyProfile)),
			action: strRef("delete entry"),
			fix: func(s *repairProfilesSession) (*pyjson.Object, error) {
				return dropSessionsJSONEntry(s.ctx, path, k)
			}})
	}
	return nil
}

// Python int of a fix count value
func repairCount(v any) (int64, error) {
	if b, ok := v.(bool); ok {
		if b {
			return 1, nil
		}
		return 0, nil
	}
	return pyint.Int64(v)
}

// Runs every repairable fix after snapshotting each existing store, collecting totals and per finding failures
func (p *repairProfilesPlan) apply(ctx context.Context, session *repairProfilesSession, snapshot func(context.Context, repairProfilesStore) (string, bool)) (*pyjson.Object, error) {
	snapshots := pyjson.NewObject()
	for _, s := range p.stores {
		if _, err := os.Stat(s.dbPath()); err != nil {
			continue
		}
		if id, ok := snapshot(ctx, s); ok {
			snapshots.Set(s.profile, id)
		} else {
			snapshots.Set(s.profile, nil)
		}
	}
	totals := pyjson.NewObject()
	failures := []any{}
	for _, finding := range p.repairable() {
		err := func() error {
			counts, err := finding.fix(session)
			if err != nil {
				return err
			}
			if counts == nil {
				return nil
			}
			for _, key := range counts.Keys {
				n, err := repairCount(counts.Vals[key])
				if err != nil {
					return err
				}
				prev, _ := totals.GetOr(key, int64(0)).(int64)
				totals.Set(key, prev+n)
			}
			return nil
		}()
		if err != nil {
			name, msg := pyrepr.ExceptionName(err), pyrepr.ExceptionMessage(err)
			repairProfilesLog.Warning(fmt.Sprintf("repair-profiles: %s on %s failed\n%s: %s", finding.kind, finding.subject, name, msg))
			failures = append(failures, pyjson.ObjectOf("kind", finding.kind, "subject", finding.subject,
				"error", name+": "+msg))
		}
	}
	return pyjson.ObjectOf("snapshots", snapshots, "totals", totals, "failures", failures), nil
}

// Every session moving from one store to another, exported, imported parents first, then deleted
type repairMoveBatch struct {
	src, dst repairProfilesStore
	ids      []string
	result   map[string]*pyjson.Object
	errs     map[string]error
}

// Registers a session in the batch and returns the fix reporting its outcome
func (b *repairMoveBatch) fixFor(sid string) func(*repairProfilesSession) (*pyjson.Object, error) {
	b.ids = append(b.ids, sid)
	return func(s *repairProfilesSession) (*pyjson.Object, error) { return b.outcome(s, sid) }
}

// Outcome of one session of the batch, its own error raised
func (b *repairMoveBatch) outcome(session *repairProfilesSession, sid string) (*pyjson.Object, error) {
	result, err := b.run(session)
	if err != nil {
		return nil, err
	}
	if e, ok := b.errs[sid]; ok {
		return nil, e
	}
	if r, ok := result[sid]; ok {
		return r, nil
	}
	return pyjson.ObjectOf("missing", 1), nil
}

// Moves the whole batch once, a failing row holding back only its own lineage
func (b *repairMoveBatch) run(session *repairProfilesSession) (map[string]*pyjson.Object, error) {
	if b.result != nil {
		return b.result, nil
	}
	ctx := session.ctx
	srcDB, err := session.writable(b.src)
	if err != nil {
		return nil, err
	}
	dstDB, err := session.writable(b.dst)
	if err != nil {
		return nil, err
	}
	payloads := map[string]*state.SessionMovePayload{}
	var exported []string
	for _, sid := range b.ids {
		payload, err := srcDB.ExportSessionForMove(ctx, sid)
		if err != nil {
			var ledgered *state.SessionLedgeredError
			if errors.As(err, &ledgered) {
				b.errs[sid] = err
				continue
			}
			return nil, err
		}
		if _, seen := payloads[sid]; !seen {
			exported = append(exported, sid)
		}
		payloads[sid] = payload
	}
	var present []*state.SessionMovePayload
	for _, sid := range exported {
		if payloads[sid] != nil {
			present = append(present, payloads[sid])
		}
	}
	ordered := parentsFirst(present)
	result := map[string]*pyjson.Object{}
	for _, sid := range exported {
		if _, failed := b.errs[sid]; payloads[sid] == nil && !failed {
			result[sid] = pyjson.ObjectOf("missing", 1)
		}
	}
	for _, payload := range ordered {
		sid, parent := rowKey(payload.Session, "id"), rowGet(payload.Session, "parent_session_id")
		if parentID, ok := parent.(string); ok {
			if _, failed := b.errs[parentID]; failed {
				b.errs[sid] = &pyrepr.FormatError{Class: "RuntimeError",
					Msg: fmt.Sprintf("its parent %s did not move; kept in the %s store with it", parentID, b.src.profile)}
				continue
			}
		}
		outcome, err := dstDB.ImportMovedSession(ctx, payload, b.dst.profile)
		if err != nil {
			b.errs[sid] = err
			continue
		}
		count, err := dstDB.CountMessagesAll(ctx, sid)
		if err != nil {
			return nil, err
		}
		if count < len(payload.Messages) {
			repairProfilesLog.Warning(fmt.Sprintf("repair-profiles: %s copied into %s with fewer messages than the source; "+
				"source row kept", sid, b.dst.profile))
			result[sid] = pyjson.ObjectOf("copied_incomplete", 1)
			continue
		}
		result[sid] = pyjson.ObjectOf(outcome, 1)
	}
	awaited := map[string]bool{}
	for _, payload := range ordered {
		if _, failed := b.errs[rowKey(payload.Session, "id")]; failed {
			if parent, ok := rowGet(payload.Session, "parent_session_id").(string); ok {
				awaited[parent] = true
			}
		}
	}
	for _, payload := range ordered {
		sid := rowKey(payload.Session, "id")
		r, ok := result[sid]
		if !ok || awaited[sid] {
			continue
		}
		if _, incomplete := r.Get("copied_incomplete"); incomplete {
			continue
		}
		deleted, err := srcDB.DeleteMovedSession(ctx, sid)
		if err != nil {
			b.errs[sid] = err
			continue
		}
		if deleted {
			r.Set("moved", 1)
		}
	}
	b.result = result
	return result, nil
}

// Stray rows that must stay put with the reason, ledgered rows and the lineage linked to them
func ledgeredLineages(foreign []*pyjson.Object) map[string]string {
	byID := map[string]*pyjson.Object{}
	var order []string
	for _, row := range foreign {
		id := rowKey(row, "id")
		if _, seen := byID[id]; !seen {
			order = append(order, id)
		}
		byID[id] = row
	}
	groups := map[string]map[string]bool{}
	for _, sid := range order {
		groups[sid] = map[string]bool{sid: true}
	}
	for _, row := range foreign {
		parent, ok := rowGet(row, "parent_session_id").(string)
		if !ok {
			continue
		}
		id := rowKey(row, "id")
		if _, in := byID[parent]; !in {
			continue
		}
		if sameGroup(groups[parent], groups[id]) {
			continue
		}
		merged := map[string]bool{}
		for m := range groups[parent] {
			merged[m] = true
		}
		for m := range groups[id] {
			merged[m] = true
		}
		for m := range merged {
			groups[m] = merged
		}
	}
	held := map[string]string{}
	for _, sid := range order {
		row := byID[sid]
		var anchors []string
		for m := range groups[sid] {
			if pyrepr.Truthy(rowGet(byID[m], "ledgered")) {
				anchors = append(anchors, m)
			}
		}
		sort.Strings(anchors)
		if pyrepr.Truthy(rowGet(row, "ledgered")) {
			held[sid] = "has gateway admission/worker history, which a move cannot carry"
		} else if len(anchors) > 0 {
			held[sid] = "its lineage holds " + anchors[0] + ", which has gateway admission/worker history"
		}
	}
	return held
}

// Whether two lineage groups are the same set object
func sameGroup(a, b map[string]bool) bool {
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	return fmt.Sprintf("%p", a) == fmt.Sprintf("%p", b)
}

// Moved sessions ordered so every parent in the batch precedes its children, a cycle keeping insertion order
func parentsFirst(payloads []*state.SessionMovePayload) []*state.SessionMovePayload {
	pending := map[string]*state.SessionMovePayload{}
	var order []string
	for _, p := range payloads {
		id := rowKey(p.Session, "id")
		if _, seen := pending[id]; !seen {
			order = append(order, id)
		}
		pending[id] = p
	}
	var ordered []*state.SessionMovePayload
	for len(pending) > 0 {
		var ready []string
		for _, sid := range order {
			p, ok := pending[sid]
			if !ok {
				continue
			}
			parent, isStr := rowGet(p.Session, "parent_session_id").(string)
			if _, waiting := pending[parent]; !isStr || !waiting {
				ready = append(ready, sid)
			}
		}
		if len(ready) == 0 {
			for _, sid := range order {
				if _, ok := pending[sid]; ok {
					ready = append(ready, sid)
				}
			}
		}
		for _, sid := range ready {
			ordered = append(ordered, pending[sid])
			delete(pending, sid)
		}
	}
	return ordered
}

// Membership test of a JSON document the way the in operator answers it
func jsonContains(data any, key string) (bool, error) {
	switch d := data.(type) {
	case *pyjson.Object:
		_, ok := d.Get(key)
		return ok, nil
	case []any:
		for _, v := range d {
			if s, ok := v.(string); ok && s == key {
				return true, nil
			}
		}
		return false, nil
	case string:
		return strings.Contains(d, key), nil
	}
	return false, &pyrepr.FormatError{Class: "TypeError",
		Msg: "argument of type '" + pyrepr.TypeName(data) + "' is not a container or iterable"}
}

// Reloaded JSON document of a fix, failing on any read or parse error
func repairLoadForFix(path string) (any, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	text, err := pystr.DecodeUTF8Sig(raw)
	if err != nil {
		return nil, err
	}
	return pyjson.Loads(text)
}

// Error a dict method raises on another JSON type
func repairNoAttribute(data any, attr string) error {
	return &pyrepr.FormatError{Class: "AttributeError", Msg: "'" + pyrepr.TypeName(data) + "' object has no attribute '" + attr + "'"}
}

// Moves a voice mode entry under the owner prefix, an existing prefixed entry winning
func rekeyVoiceModeEntry(ctx context.Context, path, key, owner string) (*pyjson.Object, error) {
	data, err := repairLoadForFix(path)
	if err != nil {
		return nil, err
	}
	in, err := jsonContains(data, key)
	if err != nil {
		return nil, err
	}
	if !in {
		return pyjson.NewObject(), nil
	}
	obj, ok := data.(*pyjson.Object)
	if !ok {
		return nil, repairNoAttribute(data, "setdefault")
	}
	value := obj.Vals[key]
	obj.Delete(key)
	if _, exists := obj.Get(owner + ":" + key); !exists {
		obj.Set(owner+":"+key, value)
	}
	obj.Delete(key)
	if err := config.AtomicJSONWrite(ctx, path, obj); err != nil {
		return nil, err
	}
	return pyjson.ObjectOf("voice_rekeyed", 1), nil
}

// Drops one sessions.json mirror entry, rewriting the file private
func dropSessionsJSONEntry(ctx context.Context, path, key string) (*pyjson.Object, error) {
	data, err := repairLoadForFix(path)
	if err != nil {
		return nil, err
	}
	in, err := jsonContains(data, key)
	if err != nil {
		return nil, err
	}
	if !in {
		return pyjson.NewObject(), nil
	}
	obj, ok := data.(*pyjson.Object)
	if !ok {
		if _, isList := data.([]any); isList {
			return nil, &pyrepr.FormatError{Class: "TypeError", Msg: "list indices must be integers or slices, not str"}
		}
		return nil, &pyrepr.FormatError{Class: "TypeError", Msg: "'" + pyrepr.TypeName(data) + "' object doesn't support item deletion"}
	}
	obj.Delete(key)
	if err := config.AtomicJSONWriteMode(ctx, path, obj, 0o600, pyjson.Indent(2)); err != nil {
		return nil, err
	}
	return pyjson.ObjectOf("sessions_json_dropped", 1), nil
}

// Opens every store and builds the plan, the caller owning the returned session
func scanStores(ctx context.Context, stores []repairProfilesStore, legacyMain string, readOnly bool) (*repairProfilesPlan, *repairProfilesSession, error) {
	if stores == nil {
		var err error
		if stores, err = enumerateStores(ctx); err != nil {
			return nil, nil, err
		}
	}
	session := &repairProfilesSession{readOnly: readOnly, ctx: ctx, dbs: map[string]repairScanDB{}}
	plan, err := newRepairProfilesPlan(ctx, stores, legacyMain)
	if err == nil {
		err = plan.scan(ctx, session)
	}
	if err != nil {
		session.close()
		return nil, nil, err
	}
	return plan, session, nil
}

// Quick snapshot of a store home labelled repair-profiles, false with a warning when it fails
func defaultSnapshot(ctx context.Context, store repairProfilesStore) (string, bool) {
	label := "repair-profiles"
	id, err := backup.CreateQuickSnapshot(&label, store.home, nil, nil)
	if err != nil {
		repairProfilesLog.Warning(fmt.Sprintf("repair-profiles: snapshot of %s failed: %s", store.home, pyrepr.ExceptionMessage(err)))
		return "", false
	}
	if id == "" {
		return "", false
	}
	return id, true
}
