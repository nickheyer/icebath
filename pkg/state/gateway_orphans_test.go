package state

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

const (
	orUser = "6308981865"
	orKey  = "agent:main:telegram:dm:6308981865"
)

type orOpts struct {
	unkeyed        bool
	source         string
	userID         *string
	startedAt      *float64
	lastActivityAt *float64
	endReason      any
	parent         string
	modelConfig    map[string]any
	messages       int
}

func orF(v float64) *float64 { return &v }

// Creates one telegram row keyed to the incident peer unless unkeyed, with its messages and timing
func orMk(t *testing.T, db *SessionDB, id string, o orOpts) {
	t.Helper()
	source := o.source
	if source == "" {
		source = "telegram"
	}
	user := orUser
	if o.userID != nil {
		user = *o.userID
	}
	n := NewSession{ID: id, Source: source, UserID: user, ParentSessionID: o.parent, ModelConfig: o.modelConfig}
	if !o.unkeyed {
		n.SessionKey, n.ChatID, n.ChatType = orKey, orUser, "dm"
	}
	readSession(t, db, n)
	for i := 0; i < o.messages; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		readAppend(t, db, id, map[string]any{"role": role, "content": "m" + string(rune('0'+i))})
	}
	if !o.unkeyed {
		profileExec(t, db, "UPDATE sessions SET origin_json = ?, display_name = ? WHERE id = ?", `{"platform": "telegram", "chat_id": "6308981865"}`, "Teknium", id)
	}
	if o.startedAt != nil {
		profileExec(t, db, "UPDATE sessions SET started_at = ? WHERE id = ?", *o.startedAt, id)
	}
	if o.messages > 0 {
		ts := o.lastActivityAt
		if ts == nil {
			ts = o.startedAt
		}
		profileExec(t, db, "UPDATE messages SET timestamp = ? WHERE session_id = ?", optFloat(ts), id)
	}
	profileExec(t, db, "UPDATE sessions SET last_activity_at = ?, ended_at = ?, end_reason = ? WHERE id = ?", optFloat(o.lastActivityAt), nil, o.endReason, id)
}

// Builds the reported incident of a keyed stale row and an unkeyed live row
func orIncident(t *testing.T, db *SessionDB, parentLink bool) (string, string) {
	t.Helper()
	at := now()
	staleLast := at - 3*86400
	stale, orphan := "20260803_120103_37976afb", "20260806_161836_04c1d6c6"
	orMk(t, db, stale, orOpts{startedAt: orF(at - 6*86400), lastActivityAt: orF(staleLast), endReason: "agent_close", messages: 4})
	parent := ""
	if parentLink {
		parent = stale
	}
	orMk(t, db, orphan, orOpts{unkeyed: true, startedAt: orF(staleLast + 60), lastActivityAt: orF(at - 3600), parent: parent, messages: 6})
	return stale, orphan
}

func orLastActivity(t *testing.T, db *SessionDB, id string) float64 {
	t.Helper()
	row, err := db.readOne(context.Background(), "SELECT last_activity_at FROM sessions WHERE id = ?", id)
	if err != nil {
		t.Fatal(err)
	}
	f, _ := pyFloatOf(rowValue(row, "last_activity_at"))
	return f
}

func orFind(t *testing.T, db *SessionDB) []*pyjson.Object {
	t.Helper()
	records, err := db.FindOrphanedGatewaySessions(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	return records
}

func orAdopt(t *testing.T, db *SessionDB, orphan, donor string) bool {
	t.Helper()
	ok, err := db.AdoptOrphanedGatewaySession(context.Background(), orphan, donor)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

func orSession(t *testing.T, db *SessionDB, id string) *pyjson.Object {
	t.Helper()
	row, err := db.GetSession(context.Background(), id)
	if err != nil || row == nil {
		t.Fatalf("session %s %v %v", id, row, err)
	}
	return row
}

func TestIncidentOrphanIsAdoptableByContiguity(t *testing.T) {
	db := usageDB(t)
	stale, orphan := orIncident(t, db, false)
	records := orFind(t, db)
	if len(records) != 1 {
		t.Fatalf("records %v", records)
	}
	r := records[0]
	if rowValue(r, "orphan_id") != orphan || rowValue(r, "donor_id") != stale || rowValue(r, "session_key") != orKey ||
		rowValue(r, "evidence") != "contiguity" || rowValue(r, "adoptable") != true || rowValue(r, "message_count") != int64(6) {
		t.Fatalf("record %v", r)
	}
}

func TestParentLinkIsUsedWithoutATimeWindow(t *testing.T) {
	db := usageDB(t)
	stale, orphan := orIncident(t, db, true)
	longAgo := now() - 400*86400
	profileExec(t, db, "UPDATE sessions SET last_activity_at = ? WHERE id = ?", longAgo, stale)
	profileExec(t, db, "UPDATE messages SET timestamp = ? WHERE session_id = ?", longAgo, stale)
	r := orFind(t, db)[0]
	if rowValue(r, "orphan_id") != orphan || rowValue(r, "donor_id") != stale || rowValue(r, "evidence") != "lineage" || rowValue(r, "adoptable") != true {
		t.Fatalf("record %v", r)
	}
}

func TestSessionsWithoutMessagesAreNotReported(t *testing.T) {
	db := usageDB(t)
	orMk(t, db, "empty", orOpts{unkeyed: true, startedAt: orF(now())})
	if records := orFind(t, db); len(records) != 0 {
		t.Fatalf("records %v", records)
	}
}

func TestTwoPredecessorsInTheWindowFailClosed(t *testing.T) {
	db := usageDB(t)
	stale, _ := orIncident(t, db, false)
	quietAt := orLastActivity(t, db, stale)
	empty := ""
	orMk(t, db, "20260803_120104_other", orOpts{userID: &empty, startedAt: orF(quietAt - 3600), lastActivityAt: orF(quietAt), endReason: "agent_close", messages: 2})
	profileExec(t, db, "UPDATE sessions SET user_id = NULL, session_key = ? WHERE id = ?", "agent:main:telegram:dm:999", "20260803_120104_other")
	r := orFind(t, db)[0]
	if rowValue(r, "adoptable") != false || !strings.Contains(pyStr(rowValue(r, "reason")), "ambiguous") {
		t.Fatalf("record %v", r)
	}
}

func TestTwoOrphansClaimingOnePredecessorFailClosed(t *testing.T) {
	db := usageDB(t)
	stale, _ := orIncident(t, db, false)
	orMk(t, db, "20260806_161840_second", orOpts{unkeyed: true, startedAt: orF(orLastActivity(t, db, stale) + 90), lastActivityAt: orF(now() - 3600), messages: 3})
	records := orFind(t, db)
	if len(records) != 2 {
		t.Fatalf("records %v", records)
	}
	for _, r := range records {
		if rowValue(r, "adoptable") != false || !strings.Contains(pyStr(rowValue(r, "reason")), "ambiguous") {
			t.Fatalf("record %v", r)
		}
	}
}

func TestDelegateChildrenAreNotOrphans(t *testing.T) {
	db := usageDB(t)
	stale, orphan := orIncident(t, db, false)
	profileExec(t, db, "DELETE FROM messages WHERE session_id = ?", orphan)
	profileExec(t, db, "DELETE FROM sessions WHERE id = ?", orphan)
	orMk(t, db, "20260806_161836_delegate", orOpts{unkeyed: true, startedAt: orF(orLastActivity(t, db, stale) + 60), lastActivityAt: orF(now() - 3600),
		modelConfig: map[string]any{"_delegate_from": stale}, messages: 3})
	if records := orFind(t, db); len(records) != 0 {
		t.Fatalf("records %v", records)
	}
}

func TestAPredecessorOfAnotherPlatformIsNotADonor(t *testing.T) {
	db := usageDB(t)
	stale, _ := orIncident(t, db, false)
	profileExec(t, db, "UPDATE sessions SET source = 'discord' WHERE id = ?", stale)
	r := orFind(t, db)[0]
	if rowValue(r, "adoptable") != false || rowValue(r, "donor_id") != nil {
		t.Fatalf("record %v", r)
	}
}

func orResolve(t *testing.T, db *SessionDB) *pyjson.Object {
	t.Helper()
	user, key, chat, kind := orUser, orKey, orUser, "dm"
	row, err := db.FindLatestGatewaySessionForPeer(context.Background(), GatewayPeerQuery{Source: "telegram", UserID: &user, SessionKey: &key, ChatID: &chat, ChatType: &kind})
	if err != nil || row == nil {
		t.Fatalf("resolve %v %v", row, err)
	}
	return row
}

func TestAdoptionMovesRecoveryToTheLiveConversation(t *testing.T) {
	db := usageDB(t)
	stale, orphan := orIncident(t, db, false)
	if got := rowValue(orResolve(t, db), "id"); got != stale {
		t.Fatalf("before %v", got)
	}
	if !orAdopt(t, db, orphan, stale) {
		t.Fatal("adoption refused")
	}
	after := orResolve(t, db)
	if rowValue(after, "id") != orphan || rowValue(after, "chat_id") != orUser || !pyTruthy(rowValue(after, "origin_json")) || rowValue(after, "parent_session_id") != stale {
		t.Fatalf("after %v", after)
	}
}

func TestPredecessorIsRetiredUnderANonResumableReason(t *testing.T) {
	db := usageDB(t)
	stale, orphan := orIncident(t, db, false)
	orAdopt(t, db, orphan, stale)
	row := orSession(t, db, stale)
	if rowValue(row, "end_reason") != "superseded_by_repair" || rowValue(row, "ended_at") == nil {
		t.Fatalf("donor %v", row)
	}
}

func TestAdoptionIsIdempotent(t *testing.T) {
	db := usageDB(t)
	stale, orphan := orIncident(t, db, false)
	if !orAdopt(t, db, orphan, stale) || orAdopt(t, db, orphan, stale) {
		t.Fatal("adoption is not idempotent")
	}
	if records := orFind(t, db); len(records) != 0 {
		t.Fatalf("records %v", records)
	}
}

func TestExistingColumnsAreNeverOverwritten(t *testing.T) {
	db := usageDB(t)
	stale, orphan := orIncident(t, db, false)
	profileExec(t, db, "UPDATE sessions SET display_name = ? WHERE id = ?", "Renamed", orphan)
	orAdopt(t, db, orphan, stale)
	if got := rowValue(orSession(t, db, orphan), "display_name"); got != "Renamed" {
		t.Fatalf("display_name %v", got)
	}
}

func TestCrossSourceAdoptionIsRefused(t *testing.T) {
	db := usageDB(t)
	stale, orphan := orIncident(t, db, false)
	profileExec(t, db, "UPDATE sessions SET source = 'discord' WHERE id = ?", orphan)
	if orAdopt(t, db, orphan, stale) || rowValue(orSession(t, db, orphan), "session_key") != nil {
		t.Fatal("cross source adoption went through")
	}
}

func TestUnkeyedDonorIsRefused(t *testing.T) {
	db := usageDB(t)
	_, orphan := orIncident(t, db, false)
	orMk(t, db, "no_key_donor", orOpts{unkeyed: true, startedAt: orF(now() - 100)})
	if orAdopt(t, db, orphan, "no_key_donor") {
		t.Fatal("unkeyed donor adopted")
	}
}

func TestDonorMustShareTheOrphansProfile(t *testing.T) {
	root := filepath.Join(t.TempDir(), "icebath")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", root)
	store := usageOpen(t, filepath.Join(root, "state.db"))
	stale, orphan := orIncident(t, store, false)
	stamp := func(profile any) {
		profileExec(t, store, "UPDATE sessions SET profile_name = ? WHERE id = ?", profile, stale)
	}
	stamp("bot2")
	if r := orFind(t, store)[0]; rowValue(r, "adoptable") != false || rowValue(r, "donor_id") != nil {
		t.Fatalf("sibling donor named %v", r)
	}
	if orAdopt(t, store, orphan, stale) || rowValue(orSession(t, store, orphan), "session_key") != nil {
		t.Fatal("sibling profile adoption went through")
	}
	stamp(nil)
	if r := orFind(t, store)[0]; rowValue(r, "adoptable") != true || rowValue(r, "donor_id") != stale {
		t.Fatalf("legacy donor refused %v", r)
	}
	profileExec(t, store, "UPDATE sessions SET parent_session_id = ? WHERE id = ?", stale, orphan)
	stamp("bot2")
	if r := orFind(t, store)[0]; rowValue(r, "adoptable") != false || rowValue(r, "donor_id") != nil {
		t.Fatalf("sibling lineage donor named %v", r)
	}
	stamp("default")
	if r := orFind(t, store)[0]; rowValue(r, "evidence") != "lineage" || rowValue(r, "donor_id") != stale {
		t.Fatalf("own lineage donor refused %v", r)
	}
}
