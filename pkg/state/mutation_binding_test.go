package state

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

const historyLogin = `auth:v1:["basic","","e2e"]`

const testHostOwner = "uid:1000"

// Installs the gateway owner identity rules over a fixed host owner and the shipped platform labels
func installOwnerIdentityDoubles(t *testing.T) {
	t.Helper()
	prevParse, prevSame, prevAgree, prevLabels := ParseAuthenticatedSubject, SameOwner, OwnersAgree, MessagingPlatformLabels
	ParseAuthenticatedSubject = func(subject any) ([3]string, bool) {
		var parts [3]string
		text, ok := subject.(string)
		if !ok || !strings.HasPrefix(text, "auth:v1:") {
			return parts, false
		}
		decoded, err := pyjson.Loads(text[len("auth:v1:"):])
		list, isList := decoded.([]any)
		if err != nil || !isList || len(list) != 3 {
			return parts, false
		}
		for i, item := range list {
			value, isStr := item.(string)
			if !isStr {
				return [3]string{}, false
			}
			parts[i] = value
		}
		return parts, true
	}
	logins := map[string]bool{"basic": true, "nous": true, "self-hosted": true, "session-token": true, "server-internal": true}
	ownerLogin := func(provider, user string) bool {
		if provider == "local" {
			return strings.TrimSpace(user) == testHostOwner
		}
		return logins[provider]
	}
	SameOwner = func(stored, subject any) bool {
		if pyjson.Equal(stored, subject) {
			return true
		}
		text, ok := stored.(string)
		if !ok || text == "" || !pyTruthy(subject) {
			return false
		}
		if label, mapped := LoginLabel(subject); mapped && text == label {
			return true
		}
		if subject != testHostOwner {
			return false
		}
		if parts, parsed := ParseAuthenticatedSubject(text); parsed && strings.TrimSpace(parts[2]) != "" && ownerLogin(parts[0], parts[2]) {
			return true
		}
		provider, user, sep := strings.Cut(text, ":")
		return sep && strings.TrimSpace(user) != "" && provider != "nous" && ownerLogin(provider, user)
	}
	OwnersAgree = func(first, second any) bool { return SameOwner(first, second) || SameOwner(second, first) }
	if MessagingPlatformLabels == nil {
		MessagingPlatformLabels = func() []string {
			return []string{"local", "telegram", "discord", "whatsapp", "slack", "signal", "mattermost", "matrix", "homeassistant", "email", "sms", "dingtalk", "api_server", "webhook"}
		}
	}
	t.Cleanup(func() {
		ParseAuthenticatedSubject, SameOwner, OwnersAgree = prevParse, prevSame, prevAgree
		if prevLabels != nil {
			MessagingPlatformLabels = prevLabels
		}
	})
}

func historyDB(t *testing.T) (context.Context, *SessionDB) {
	t.Helper()
	installOwnerIdentityDoubles(t)
	db := mustSessionDB(t, filepath.Join(t.TempDir(), "state.db"), false)
	t.Cleanup(func() { db.Close() })
	return context.Background(), db
}

func authorizeAs(ctx context.Context, db *SessionDB, subject, sessionID string, claim bool) (bool, error) {
	actor := HistoryActor{ProfileID: "p", Subject: subject, Capabilities: []string{"session:read", "session:create"}}
	var ok bool
	err := db.executeWrite(ctx, func(conn SQLiteConn) error {
		var err error
		ok, err = AuthorizeHistory(ctx, conn, actor, sessionID, claim)
		return err
	}, nil)
	return ok, err
}

func TestTheLoginLabelMapsForAnIssuerLessLogin(t *testing.T) {
	installOwnerIdentityDoubles(t)
	if label, ok := LoginLabel(historyLogin); !ok || label != "basic:e2e" {
		t.Fatalf("label %q %v", label, ok)
	}
}

func TestLabelsThatMustNotMap(t *testing.T) {
	installOwnerIdentityDoubles(t)
	for _, subject := range []any{
		`auth:v1:["basic","https://idp.example","e2e"]`, `auth:v1:["basic","",""]`, `auth:v1:["basic","","  "]`,
		`auth:v1:["ba:sic","","e2e"]`, `auth:v1:["","","e2e"]`, `auth:v1:["auth","","v1:[\"x\"]"]`, `auth:v1:"basic"`, "uid:501",
		`auth:v1:[1,2,3]`, `auth:v1:["a","b"]`, "auth:v1:[", nil, 7,
	} {
		if label, ok := LoginLabel(subject); ok {
			t.Fatalf("%v mapped to %q", subject, label)
		}
	}
}

func TestHistoryLoginLabelRows(t *testing.T) {
	ctx, db := historyDB(t)
	for id, user := range map[string]any{"desktop-row": "basic:e2e", "mallory-row": "basic:mallory", "subject-row": historyLogin, "unowned-row": nil, "alice-row": "basic:alice"} {
		profileExec(t, db, "INSERT INTO sessions(id,source,user_id,started_at) VALUES(?,'desktop',?,?)", id, user, now())
	}
	for _, c := range []struct {
		name, subject, row string
		refused            bool
	}{
		{"a desktop labelled row takes its login", historyLogin, "desktop-row", false},
		{"a foreign account still refused", historyLogin, "mallory-row", true},
		{"the exact subject still matches", historyLogin, "subject-row", false},
		{"an issuer bearing login cannot touch the labelled row", `auth:v1:["basic","https://idp.example","e2e"]`, "desktop-row", true},
		{"an unowned row still admits any local actor", historyLogin, "unowned-row", false},
	} {
		ok, err := authorizeAs(ctx, db, c.subject, c.row, false)
		if c.refused && refusalReason(t, err) != "permission_denied" || !c.refused && (err != nil || !ok) {
			t.Fatalf("%s: %v %v", c.name, ok, err)
		}
	}
	bob, alice := `auth:v1:["basic","","bob"]`, `auth:v1:["basic","","alice"]`
	if _, err := authorizeAs(ctx, db, bob, "alice-row", true); refusalReason(t, err) != "permission_denied" {
		t.Fatalf("forwarded bob: %v", err)
	}
	if ok, err := authorizeAs(ctx, db, alice, "alice-row", false); err != nil || !ok {
		t.Fatalf("alice: %v %v", ok, err)
	}
}

func TestAuthorizeHistoryClaimsOnceAndRoutedRowsAreNeverHistory(t *testing.T) {
	ctx, db := historyDB(t)
	profileExec(t, db, "INSERT INTO sessions(id,source,started_at) VALUES('cli-row','cli',?)", now())
	profileExec(t, db, "INSERT INTO sessions(id,source,session_key,started_at) VALUES('routed','cli','k',?)", now())
	profileExec(t, db, "INSERT INTO sessions(id,source,started_at) VALUES('cron-row','cron',?)", now())
	if ok, err := authorizeAs(ctx, db, "alice", "cli-row", true); err != nil || !ok {
		t.Fatalf("claim %v %v", ok, err)
	}
	if got := storedValue(t, db, "SELECT value FROM state_meta WHERE key=?", BindingPrefix+"cli-row"); got != `["p","alice"]` {
		t.Fatalf("binding %v", got)
	}
	if _, err := authorizeAs(ctx, db, "bob", "cli-row", false); refusalReason(t, err) != "permission_denied" {
		t.Fatalf("bound row %v", err)
	}
	for _, sid := range []string{"routed", "cron-row", "missing"} {
		if ok, err := authorizeAs(ctx, db, "alice", sid, false); err != nil || ok {
			t.Fatalf("%s: %v %v", sid, ok, err)
		}
	}
	actor := HistoryActor{ProfileID: "p", Subject: "alice"}
	if err := db.executeWrite(ctx, func(conn SQLiteConn) error {
		if err := ImportHistoryControl(ctx, conn, actor, []string{"cli-row", "fresh"}); err != nil {
			return err
		}
		_, err := AuthorizeHistory(ctx, conn, HistoryActor{ProfileID: "p", Subject: "bob"}, "cron-row", false)
		return err
	}, nil); err != nil {
		t.Fatal(err)
	}
	if got := storedValue(t, db, "SELECT value FROM state_meta WHERE key=?", BindingPrefix+"fresh"); got != `["p","alice"]` {
		t.Fatalf("import binding %v", got)
	}
	if !BindingNames(`["p","alice"]`, "p", "alice") || BindingNames(`["q","alice"]`, "p", "alice") || BindingNames("{", "p", "alice") || BindingNames(nil, "p", "alice") {
		t.Fatal("binding_names")
	}
}

func TestClientHistoryRowsAndLegacyValidation(t *testing.T) {
	_, db := historyDB(t)
	mobile := pyjson.ObjectOf("source", "mobile", "user_id", nil)
	if !ClientHistoryRow(mobile) || !LegacyHistoryRow(mobile) || ClientLabel("telegram") || ClientLabel("cli") || ClientLabel("  ") {
		t.Fatal("client labels")
	}
	if source, err := LegacyCreationSource(mobile); err != nil || source != "gui" {
		t.Fatalf("mobile creation %v %v", source, err)
	}
	if source, err := LegacyCreationSource(pyjson.ObjectOf("source", "desktop")); err != nil || source != "gui" {
		t.Fatalf("desktop creation %v %v", source, err)
	}
	if ClientHistoryRow(pyjson.ObjectOf("source", "mobile", "session_key", "k")) {
		t.Fatal("routed client row")
	}
	nous := pyjson.ObjectOf("source", "desktop", "user_id", "nous:acct")
	if label, _ := LegacyAccountLabel(nous); !label || db.ValidateLegacyRow(nous, nil) != nil {
		t.Fatal("nous account label blocks adoption")
	}
	for name, row := range map[string]*pyjson.Object{
		"foreign user":    pyjson.ObjectOf("source", "cli", "user_id", "telegram:42"),
		"routed":          pyjson.ObjectOf("source", "cli", "chat_id", "c"),
		"foreign profile": pyjson.ObjectOf("source", "cli", "profile_name", "other"),
		"messaging":       pyjson.ObjectOf("source", "telegram"),
	} {
		if refusalReason(t, db.ValidateLegacyRow(row, "alice")) != "not_found" {
			t.Fatalf("%s adopted", name)
		}
	}
	if err := db.ValidateLegacyRow(pyjson.ObjectOf("source", "cli", "user_id", "alice", "profile_name", "default"), "alice"); err != nil {
		t.Fatal(err)
	}
	if err := db.ValidateLegacyRow(pyjson.ObjectOf("source", "tui", "user_id", "basic:e2e"), testHostOwner); err != nil {
		t.Fatalf("owner login stamp %v", err)
	}
}

func TestDeleteTargetsWalksContinuationsAndDelegatesWithinTheOwner(t *testing.T) {
	ctx, db := historyDB(t)
	for _, row := range [][]any{
		{"root", nil, "compression", nil, "alice"},
		{"cont", "root", nil, nil, "alice"},
		{"fork", "root", nil, `{"_branched_from":"root"}`, "alice"},
		{"dlg", "cont", nil, `{"_delegate_from":"cont"}`, "alice"},
		{"cont2", "dlg", nil, nil, "alice"},
		{"foreign", "root", nil, nil, "telegram:9"},
	} {
		profileExec(t, db, "INSERT INTO sessions(id,source,parent_session_id,end_reason,model_config,user_id,started_at) VALUES(?,'cli',?,?,?,?,?)",
			row[0], row[1], row[2], row[3], row[4], now())
	}
	profileExec(t, db, "UPDATE sessions SET end_reason='compression' WHERE id='dlg'")
	var targets []string
	if err := db.readCtx(ctx, false, func(conn SQLiteConn) error {
		var err error
		targets, err = DeleteTargets(ctx, conn, "cont")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if want := []string{"cont", "cont2", "dlg", "root"}; !reflect.DeepEqual(targets, want) {
		t.Fatalf("targets %v want %v", targets, want)
	}
}

func TestBindLegacyTargetClaimsAnIdleUnboundRowOnce(t *testing.T) {
	ctx, db := historyDB(t)
	profileExec(t, db, "INSERT INTO sessions(id,source,started_at) VALUES('legacy','mobile',?)", now())
	profileExec(t, db, "INSERT INTO sessions(id,source,started_at) VALUES('imported','cli',?)", now())
	profileExec(t, db, "INSERT INTO state_meta(key,value) VALUES(?,?)", BindingPrefix+"imported", `["p","bob"]`)
	receipt := func(target, sessionID string) *pyjson.Object {
		return pyjson.ObjectOf("legacy_session_id", target, "session_id", sessionID, "principal_id", "alice", "profile_id", "p",
			"route", "local:"+sessionID, "entry", pyjson.ObjectOf("origin", pyjson.ObjectOf("platform", "local", "chat_id", sessionID)))
	}
	bind := func(r *pyjson.Object) error {
		return db.executeWrite(ctx, func(conn SQLiteConn) error { return db.bindLegacyTarget(ctx, conn, r) }, nil)
	}
	if refusalReason(t, bind(receipt("legacy", "other"))) != "storage_unavailable" {
		t.Fatal("mismatched receipt bound")
	}
	if refusalReason(t, bind(receipt("missing", "missing"))) != "not_found" {
		t.Fatal("missing row bound")
	}
	if refusalReason(t, bind(receipt("imported", "imported"))) != "permission_denied" {
		t.Fatal("foreign import bound")
	}
	if err := bind(receipt("legacy", "legacy")); err != nil {
		t.Fatal(err)
	}
	if got := storedValue(t, db, "SELECT source || '|' || session_key || '|' || chat_id || '|' || user_id || '|' || chat_type || '|' || origin_json FROM sessions WHERE id='legacy'"); got != `desktop|local:legacy|legacy|alice|dm|{"chat_id":"legacy","platform":"local"}` {
		t.Fatalf("bound row %v", got)
	}
	if got := storedValue(t, db, "SELECT value FROM state_meta WHERE key=?", LocalLegacyPrefix+"legacy"); got != `{"legacy_session_id":"legacy","principal_id":"alice","profile_id":"p","session_id":"legacy"}` {
		t.Fatalf("legacy binding %v", got)
	}
	if refusalReason(t, bind(receipt("legacy", "legacy"))) != "not_found" {
		t.Fatal("rebound a routed row")
	}
}
