package state

import (
	"context"
	"path/filepath"
	"slices"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

const legacyRoutingDDL = `
        CREATE TABLE gateway_routing (
            session_key TEXT PRIMARY KEY,
            entry_json TEXT NOT NULL,
            updated_at REAL NOT NULL
        , "scope" TEXT DEFAULT '')
    `

func routingPKColumns(t *testing.T, db *SessionDB) []string {
	t.Helper()
	var pk []string
	if err := db.executeWrite(context.Background(), func(conn SQLiteConn) error {
		var err error
		pk, err = livePKColumns(context.Background(), conn, "gateway_routing")
		return err
	}, nil); err != nil {
		t.Fatal(err)
	}
	return pk
}

func TestLegacyPKRebuiltToComposite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	conn := openRaw(t, path)
	execAll(t, conn, legacyRoutingDDL, "INSERT INTO gateway_routing (scope, session_key, entry_json, updated_at) VALUES ('/home/u/.icebath/sessions', 'agent:main:telegram:dm:1', '{}', 1.0)")
	conn.Close()
	db := usageOpen(t, path)
	if pk := routingPKColumns(t, db); !slices.Equal(pk, []string{"scope", "session_key"}) {
		t.Fatal(pk)
	}
	entries, err := db.LoadGatewayRoutingEntries(context.Background(), "/home/u/.icebath/sessions")
	if err != nil || len(entries) != 1 || entries["agent:main:telegram:dm:1"] != "{}" {
		t.Fatal(entries, err)
	}
}

func TestCurrentShapeLeftUntouched(t *testing.T) {
	ctx := context.Background()
	db := usageDB(t)
	if err := db.SaveGatewayRoutingEntry(ctx, "k1", "{}", "s"); err != nil {
		t.Fatal(err)
	}
	if pk := routingPKColumns(t, db); !slices.Equal(pk, []string{"scope", "session_key"}) {
		t.Fatal(pk)
	}
	if err := db.executeWrite(ctx, func(conn SQLiteConn) error { return db.healGatewayRoutingPK(ctx, conn) }, nil); err != nil {
		t.Fatal(err)
	}
	entries, err := db.LoadGatewayRoutingEntries(ctx, "s")
	if err != nil || len(entries) != 1 || entries["k1"] != "{}" {
		t.Fatal(entries, err)
	}
}

func TestCreatedSourcePreservedAcrossCrossPlatformResume(t *testing.T) {
	ctx := context.Background()
	db := usageDB(t)
	if err := db.CreateSession(NewSession{ID: "tui-sess", Source: "tui"}); err != nil {
		t.Fatal(err)
	}
	wmAppend(t, db, "tui-sess", map[string]any{"role": "user", "content": "created on desktop"}, AppendGuards{})
	key, chat := "agent:main:telegram:dm:1", "1"
	if err := db.RecordGatewaySessionPeer(ctx, "tui-sess", GatewayPeer{Source: "telegram", SessionKey: &key, ChatID: &chat}); err != nil {
		t.Fatal(err)
	}
	if crossField(t, db, "tui-sess", "source") != "telegram" || crossField(t, db, "tui-sess", "created_source") != "tui" {
		t.Fatal("peer recording moved provenance or kept the old routing source")
	}
	if err := db.EnsureSession(NewSession{ID: "tui-sess", Source: "discord"}); err != nil {
		t.Fatal(err)
	}
	if got := crossField(t, db, "tui-sess", "created_source"); got != "tui" {
		t.Fatal(got)
	}
	slackKey, slackChat := "agent:main:slack:ch:2", "2"
	if err := db.RecordGatewaySessionPeer(ctx, "slack-sess", GatewayPeer{Source: "slack", SessionKey: &slackKey, ChatID: &slackChat}); err != nil {
		t.Fatal(err)
	}
	if got := crossField(t, db, "slack-sess", "created_source"); got != "slack" {
		t.Fatal(got)
	}
}

func TestUnsafeModelConfigDoesNotBreakSessionSurfaces(t *testing.T) {
	installMessagingPlatform(t)
	for _, unsafe := range []string{"{not-json", "[]", `"scalar"`, "5", "null"} {
		t.Run(unsafe, func(t *testing.T) {
			ctx := context.Background()
			db := usageDB(t)
			for _, n := range []NewSession{{ID: "root", Source: "telegram"}, {ID: "compression-parent", Source: "telegram"}} {
				if err := db.CreateSession(n); err != nil {
					t.Fatal(err)
				}
			}
			wmAppend(t, db, "root", map[string]any{"role": "user", "content": "root message"}, AppendGuards{})
			if err := db.EndSession("compression-parent", "compression"); err != nil {
				t.Fatal(err)
			}
			for _, n := range []NewSession{{ID: "compression-child", Source: "telegram", ParentSessionID: "compression-parent"}, {ID: "routing-orphan", Source: "telegram"}} {
				if err := db.CreateSession(n); err != nil {
					t.Fatal(err)
				}
			}
			wmAppend(t, db, "compression-child", map[string]any{"role": "user", "content": "child message"}, AppendGuards{})
			wmAppend(t, db, "routing-orphan", map[string]any{"role": "user", "content": "orphan message"}, AppendGuards{})
			profileExec(t, db, "UPDATE sessions SET model_config = ? WHERE id IN (?, ?, ?)", unsafe, "root", "compression-child", "routing-orphan")
			for _, ordered := range []bool{false, true} {
				rows, err := db.ListSessionsRich(ctx, ListSessionsRichOptions{Source: "telegram", OrderByLastActive: ordered})
				if err != nil {
					t.Fatal(err)
				}
				found := false
				for _, r := range rows {
					if id, _ := r.Get("id"); id == "root" {
						found = true
					}
				}
				if !found {
					t.Fatal("root missing from the rich list", ordered)
				}
			}
			if n, err := db.SessionCount(ctx, SessionCountOptions{Source: "telegram", ExcludeChildren: true}); n != 3 || err != nil {
				t.Fatal(n, err)
			}
			bySource, err := db.SessionCountBySource(ctx, false, false, true)
			if err != nil {
				t.Fatal(err)
			}
			if got, _ := bySource.Get("telegram"); got != 3 && got != int64(3) {
				t.Fatal(got)
			}
			if chain, err := db.GetCompressionChain(ctx, "compression-parent"); err != nil || !slices.Equal(chain, []string{"compression-parent", "compression-child"}) {
				t.Fatal(chain, err)
			}
			key := "agent:main:telegram:dm:recovered"
			if err := db.RecordGatewaySessionPeer(ctx, "compression-child", GatewayPeer{Source: "telegram", SessionKey: &key, IncludeCompressionAncestors: true}); err != nil {
				t.Fatal(err)
			}
			if got := crossField(t, db, "compression-parent", "session_key"); got != key {
				t.Fatal(got)
			}
			orphans, err := db.FindOrphanedGatewaySessions(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.ContainsFunc(orphans, func(o *pyjson.Object) bool { id, _ := o.Get("orphan_id"); return id == "routing-orphan" }) {
				t.Fatal("routing orphan not found", len(orphans))
			}
		})
	}
}
