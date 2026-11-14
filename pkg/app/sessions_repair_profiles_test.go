package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/profiles/manage"
	"github.com/nickheyer/icebath/pkg/state"
)

const (
	repairRootKey  = "agent:main:telegram:dm:100"
	repairAcmeKey  = "agent:acme:telegram:dm:200"
	repairBetaKey  = "agent:beta:telegram:dm:300"
	repairGhostKey = "agent:ghost:telegram:dm:400"
)

// Root home and two named profiles under a temporary user home
func repairHomes(t *testing.T) map[string]string {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", filepath.Join(tmp, "user"))
	root := filepath.Join(tmp, ".icebath")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", root)
	installSessionsStoreWiring(t)
	for _, name := range []string{"acme", "beta"} {
		if _, err := manage.CreateProfile(context.Background(), name, manage.CreateOptions{NoSkills: true}); err != nil {
			t.Fatal(err)
		}
	}
	return map[string]string{"default": root, "acme": filepath.Join(root, "profiles", "acme"),
		"beta": filepath.Join(root, "profiles", "beta")}
}

func repairOpen(t *testing.T, home string) *state.SessionDB {
	t.Helper()
	db, err := state.NewSessionDB(context.Background(), filepath.Join(home, "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func repairSession(t *testing.T, db *state.SessionDB, sid, key, profile, parent string, messages int) {
	t.Helper()
	parts := strings.Split(key, ":")
	if err := db.CreateSession(state.NewSession{ID: sid, Source: "telegram", SessionKey: key, ChatID: parts[len(parts)-1],
		ChatType: "dm", ProfileName: profile, ParentSessionID: parent, SystemPrompt: "sys"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < messages; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		if _, err := db.AppendMessage(context.Background(), sid, map[string]any{"role": role, "content": fmt.Sprintf("%s m%d", sid, i)}, state.AppendGuards{}); err != nil {
			t.Fatal(err)
		}
	}
}

func repairScope(t *testing.T, homes map[string]string) string {
	t.Helper()
	scope, err := filepath.EvalSymlinks(filepath.Join(homes["default"], "sessions"))
	if err != nil {
		os.MkdirAll(filepath.Join(homes["default"], "sessions"), 0o755)
		if scope, err = filepath.EvalSymlinks(filepath.Join(homes["default"], "sessions")); err != nil {
			t.Fatal(err)
		}
	}
	return scope
}

func repairRoutingJSON(key, sid string) string {
	b, _ := json.Marshal(map[string]string{"session_key": key, "session_id": sid})
	return string(b)
}

// Every defect the command knows spread over the three stores
func repairSeedCrossings(t *testing.T, homes map[string]string) {
	t.Helper()
	ctx := context.Background()
	root, acme, beta := repairOpen(t, homes["default"]), repairOpen(t, homes["acme"]), repairOpen(t, homes["beta"])
	scope := repairScope(t, homes)
	repairSession(t, root, "ok-root", repairRootKey, "default", "", 2)
	repairSession(t, acme, "ok-acme", repairAcmeKey, "acme", "", 2)
	if err := root.SaveGatewayRoutingEntry(ctx, repairAcmeKey, repairRoutingJSON(repairAcmeKey, "ok-acme"), scope); err != nil {
		t.Fatal(err)
	}
	repairSession(t, acme, "mislabelled", "agent:acme:telegram:dm:201", "default", "", 2)
	repairSession(t, root, "stray-parent", "agent:acme:telegram:dm:202", "acme", "", 3)
	repairSession(t, root, "stray-child", "agent:acme:telegram:dm:202", "acme", "stray-parent", 2)
	repairSession(t, acme, "legacy-main", "agent:main:telegram:dm:203", "default", "", 2)
	repairSession(t, root, "ghost", repairGhostKey, "ghost", "", 2)
	repairSession(t, beta, "beta-parent", repairBetaKey, "beta", "", 2)
	repairSession(t, beta, "acme-in-beta", "agent:acme:telegram:dm:204", "acme", "", 2)
	repairSession(t, beta, "beta-child", "agent:beta:telegram:dm:301", "beta", "acme-in-beta", 2)
	if err := beta.SaveGatewayRoutingEntry(ctx, repairBetaKey, repairRoutingJSON(repairBetaKey, "beta-parent"), scope); err != nil {
		t.Fatal(err)
	}
	if err := root.SaveGatewayRoutingEntry(ctx, repairGhostKey, repairRoutingJSON(repairGhostKey, "ghost"), scope); err != nil {
		t.Fatal(err)
	}
	if err := acme.BindTelegramTopic(ctx, state.TelegramTopicBinding{ChatID: "200", ThreadID: "7", UserID: "200",
		SessionKey: repairAcmeKey, SessionID: "ok-acme", ProfileName: "default"}); err != nil {
		t.Fatal(err)
	}
	writeText(t, filepath.Join(homes["default"], "gateway_voice_mode.json"), `{"telegram:200": "all", "telegram:100": "voice_only"}`)
	writeText(t, filepath.Join(homes["default"], "sessions", "sessions.json"), fmt.Sprintf(
		`{"_README": "x", %q: {"session_key": %q, "session_id": "ghost"}, %q: {"session_key": %q, "session_id": "ok-root"}}`,
		repairGhostKey, repairGhostKey, repairRootKey, repairRootKey))
	for _, db := range []*state.SessionDB{root, acme, beta} {
		db.Close()
	}
}

func writeText(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Every user table as sorted row texts, the whole file mutation oracle
func repairDump(t *testing.T, path string) map[string][]string {
	t.Helper()
	conn, err := state.ConnectSQLite(path, state.SQLiteConfig{Timeout: 5})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx := context.Background()
	rows, err := conn.QueryContext(ctx, "SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' "+
		"AND name NOT LIKE '%_fts%' ORDER BY name")
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var name string
		rows.Scan(&name)
		tables = append(tables, name)
	}
	rows.Close()
	out := map[string][]string{}
	for _, table := range tables {
		r, err := conn.QueryContext(ctx, "SELECT * FROM "+table)
		if err != nil {
			t.Fatal(err)
		}
		cols, _ := r.Columns()
		var texts []string
		for r.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			r.Scan(ptrs...)
			texts = append(texts, fmt.Sprintf("%v", vals))
		}
		r.Close()
		sort.Strings(texts)
		out[table] = texts
	}
	return out
}

// Runs repair-profiles with the given namespace overrides, live gateway reader and snapshotter
func repairRun(t *testing.T, overrides map[string]any, live func(context.Context, []repairProfilesStore) ([]liveGatewayHome, error),
	snapshot func(context.Context, repairProfilesStore) (string, bool)) (int, string, string) {
	t.Helper()
	ns := map[string]any{"sessions_action": "repair-profiles", "apply": false, "json": false, "yes": true, "legacy_main": "report"}
	for k, v := range overrides {
		ns[k] = v
	}
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	notTTY := func() bool { return false }
	restore := SetStreams(&cliui.Console{In: strings.NewReader(""), Out: out, StdinTTY: notTTY, StdoutTTY: notTTY}, errOut)
	defer restore()
	r := &sessionsRun{ctx: context.Background(), h: os.Getenv("ICEBATH_HOME"), ns: ns, w: out, printHelp: func() error { return nil }}
	if live == nil {
		live = liveGatewayHomes
	}
	if snapshot == nil {
		snapshot = func(context.Context, repairProfilesStore) (string, bool) { return "snap", true }
	}
	code, err := repairProfilesCommand(r, live, snapshot)
	if err != nil {
		t.Fatalf("repair-profiles: %v\n%s", err, out.String())
	}
	return code, out.String(), errOut.String()
}

type repairReportFinding struct {
	Kind, Store, Subject, Detail string
	Action, Reason               *string
}

type repairReport struct {
	Findings   []repairReportFinding
	Repairable int
}

func repairJSONReport(t *testing.T) repairReport {
	t.Helper()
	code, out, _ := repairRun(t, map[string]any{"json": true}, nil, nil)
	if code != 0 {
		t.Fatalf("report exit %d: %s", code, out)
	}
	var report repairReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	return report
}

func TestDryRunNamesEveryCrossingAndChangesNothing(t *testing.T) {
	homes := repairHomes(t)
	repairSeedCrossings(t, homes)
	before := map[string]map[string][]string{}
	for name, h := range homes {
		before[name] = repairDump(t, filepath.Join(h, "state.db"))
	}
	files := []string{filepath.Join(homes["default"], "gateway_voice_mode.json"), filepath.Join(homes["default"], "sessions", "sessions.json")}
	filesBefore := map[string]string{}
	for _, f := range files {
		b, _ := os.ReadFile(f)
		filesBefore[f] = string(b)
	}
	report := repairJSONReport(t)
	kinds := map[[2]string]bool{}
	for _, f := range report.Findings {
		kinds[[2]string{f.Kind, f.Subject}] = true
	}
	scope := repairScope(t, homes)
	for _, want := range [][2]string{{"mislabelled", "mislabelled"}, {"wrong_store", "stray-parent"}, {"wrong_store", "stray-child"},
		{"legacy_main", "legacy-main"}, {"unclaimed_namespace", "ghost"}, {"wrong_store", "acme-in-beta"}, {"crossed_parent", "beta-child"},
		{"routing_unclaimed", repairGhostKey + " (scope " + pyrepr.Repr(scope) + ")"}, {"topic_profile_less", "chat 200 thread 7"},
		{"voice_profile_less", "telegram:200"}, {"sessions_json_unclaimed", repairGhostKey}} {
		if !kinds[want] {
			t.Fatalf("missing finding %v in %v", want, kinds)
		}
	}
	byKind := map[string]repairReportFinding{}
	for _, f := range report.Findings {
		if f.Kind == "routing_stray" {
			t.Fatalf("routing_stray reported %+v", f)
		}
		switch f.Subject {
		case "ok-root", "ok-acme", "telegram:100", repairRootKey:
			t.Fatalf("healthy control reported %+v", f)
		}
		byKind[f.Kind] = f
	}
	if lm := byKind["legacy_main"]; lm.Action != nil || !strings.Contains(*lm.Reason, "--legacy-main") {
		t.Fatalf("legacy_main %+v", lm)
	}
	if un := byKind["unclaimed_namespace"]; un.Action != nil || !strings.Contains(*un.Reason, "ghost") {
		t.Fatalf("unclaimed %+v", un)
	}
	for name, h := range homes {
		if fmt.Sprint(repairDump(t, filepath.Join(h, "state.db"))) != fmt.Sprint(before[name]) {
			t.Fatalf("%s store changed", name)
		}
	}
	for _, f := range files {
		if b, _ := os.ReadFile(f); string(b) != filesBefore[f] {
			t.Fatalf("%s changed", f)
		}
	}
}

func TestApplySettlesEveryRepairableCrossingAndIsIdempotent(t *testing.T) {
	homes := repairHomes(t)
	repairSeedCrossings(t, homes)
	var snapshots []string
	code, out, _ := repairRun(t, map[string]any{"apply": true}, nil, func(_ context.Context, s repairProfilesStore) (string, bool) {
		snapshots = append(snapshots, s.profile)
		return "snap-" + s.profile, true
	})
	if code != 0 {
		t.Fatalf("apply exit %d: %s", code, out)
	}
	sort.Strings(snapshots)
	if strings.Join(snapshots, ",") != "acme,beta,default" {
		t.Fatalf("snapshots %v", snapshots)
	}
	ctx := context.Background()
	root, acme, beta := repairOpen(t, homes["default"]), repairOpen(t, homes["acme"]), repairOpen(t, homes["beta"])
	defer root.Close()
	defer acme.Close()
	defer beta.Close()
	get := func(db *state.SessionDB, sid string) map[string]any {
		row, err := db.GetSession(ctx, sid)
		if err != nil {
			t.Fatal(err)
		}
		if row == nil {
			return nil
		}
		return row.Vals
	}
	count := func(db *state.SessionDB, sid string) int {
		msgs, err := db.GetMessages(ctx, sid, state.GetMessagesOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return len(msgs)
	}
	if get(acme, "mislabelled")["profile_name"] != "acme" {
		t.Fatal("mislabelled not relabelled")
	}
	if get(root, "stray-parent") != nil || get(root, "stray-child") != nil {
		t.Fatal("strays left in root")
	}
	child := get(acme, "stray-child")
	if child["profile_name"] != "acme" || child["parent_session_id"] != "stray-parent" {
		t.Fatalf("moved child %v", child)
	}
	if count(acme, "stray-parent") != 3 || count(acme, "stray-child") != 2 || get(acme, "stray-parent")["system_prompt"] != "sys" {
		t.Fatal("moved rows incomplete")
	}
	if get(acme, "legacy-main")["session_key"] != "agent:main:telegram:dm:203" || get(root, "ghost")["session_key"] != repairGhostKey {
		t.Fatal("report only rows touched")
	}
	bc := get(beta, "beta-child")
	if bc["parent_session_id"] != nil || bc["profile_name"] != "beta" {
		t.Fatalf("beta child %v", bc)
	}
	if get(beta, "acme-in-beta") != nil || get(acme, "acme-in-beta") == nil {
		t.Fatal("acme-in-beta not moved")
	}
	scope := repairScope(t, homes)
	rootRouting, _ := root.LoadGatewayRoutingEntries(ctx, scope)
	betaRouting, _ := beta.LoadGatewayRoutingEntries(ctx, scope)
	if _, ok := rootRouting[repairAcmeKey]; len(rootRouting) != 1 || !ok {
		t.Fatalf("root routing %v", rootRouting)
	}
	if _, ok := betaRouting[repairBetaKey]; len(betaRouting) != 1 || !ok {
		t.Fatalf("beta routing %v", betaRouting)
	}
	if b, _ := acme.GetTelegramTopicBinding(ctx, "200", "7", "acme"); b == nil {
		t.Fatal("topic binding not relabelled")
	}
	if b, _ := acme.GetTelegramTopicBinding(ctx, "200", "7", "default"); b != nil {
		t.Fatal("default topic binding kept")
	}
	var voice map[string]string
	raw, _ := os.ReadFile(filepath.Join(homes["default"], "gateway_voice_mode.json"))
	json.Unmarshal(raw, &voice)
	if len(voice) != 2 || voice["acme:telegram:200"] != "all" || voice["telegram:100"] != "voice_only" {
		t.Fatalf("voice %v", voice)
	}
	var mirror map[string]any
	raw, _ = os.ReadFile(filepath.Join(homes["default"], "sessions", "sessions.json"))
	json.Unmarshal(raw, &mirror)
	if _, ok := mirror[repairRootKey]; len(mirror) != 2 || !ok || mirror["_README"] == nil {
		t.Fatalf("mirror %v", mirror)
	}
	if get(root, "ok-root")["profile_name"] != "default" || get(acme, "ok-acme")["profile_name"] != "acme" {
		t.Fatal("controls touched")
	}
	report := repairJSONReport(t)
	remaining := map[string]bool{}
	for _, f := range report.Findings {
		remaining[f.Kind] = true
	}
	if len(remaining) != 2 || !remaining["legacy_main"] || !remaining["unclaimed_namespace"] || report.Repairable != 0 {
		t.Fatalf("second scan %+v", report)
	}
}

func TestApplyRefusesWhileAGatewayOwnsAStore(t *testing.T) {
	homes := repairHomes(t)
	repairSeedCrossings(t, homes)
	before := repairDump(t, filepath.Join(homes["default"], "state.db"))
	code, _, errText := repairRun(t, map[string]any{"apply": true}, func(context.Context, []repairProfilesStore) ([]liveGatewayHome, error) {
		return []liveGatewayHome{{"default", 4242}}, nil
	}, nil)
	if code != 1 || !strings.Contains(errText, "pid 4242") {
		t.Fatalf("exit %d err %q", code, errText)
	}
	if fmt.Sprint(repairDump(t, filepath.Join(homes["default"], "state.db"))) != fmt.Sprint(before) {
		t.Fatal("store changed")
	}
}

func TestLegacyMainRekeyAdoptsAStandaloneGatewaysHistory(t *testing.T) {
	homes := repairHomes(t)
	repairSeedCrossings(t, homes)
	if code, out, _ := repairRun(t, map[string]any{"apply": true, "legacy_main": "rekey"}, nil, nil); code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	acme := repairOpen(t, homes["acme"])
	row, err := acme.GetSession(context.Background(), "legacy-main")
	acme.Close()
	if err != nil || row.Vals["session_key"] != "agent:acme:telegram:dm:203" || row.Vals["profile_name"] != "acme" {
		t.Fatalf("row %v %v", row, err)
	}
	for _, f := range repairJSONReport(t).Findings {
		if f.Kind == "legacy_main" {
			t.Fatalf("legacy_main remains %+v", f)
		}
	}
}

func TestMoveIntoAStoreThatAlreadyHoldsTheTitle(t *testing.T) {
	homes := repairHomes(t)
	title := strings.Repeat("G", state.MaxTitleLength)
	root, acme := repairOpen(t, homes["default"]), repairOpen(t, homes["acme"])
	repairSession(t, root, "s-a", "agent:acme:telegram:dm:210", "acme", "", 2)
	repairSession(t, root, "s-b", "agent:acme:telegram:dm:211", "acme", "", 3)
	repairSession(t, acme, "w-1", repairAcmeKey, "acme", "", 2)
	for _, set := range []struct {
		db        *state.SessionDB
		sid, text string
	}{{root, "s-a", title}, {root, "s-b", "Weekend plans"}, {acme, "w-1", title}} {
		if _, err := set.db.SetTitle(set.sid, set.text); err != nil {
			t.Fatal(err)
		}
	}
	root.Close()
	acme.Close()
	if code, out, _ := repairRun(t, map[string]any{"apply": true}, nil, nil); code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	ctx := context.Background()
	root, acme = repairOpen(t, homes["default"]), repairOpen(t, homes["acme"])
	defer root.Close()
	defer acme.Close()
	for _, sid := range []string{"s-a", "s-b"} {
		if row, _ := root.GetSession(ctx, sid); row != nil {
			t.Fatalf("%s left in root", sid)
		}
	}
	for sid, n := range map[string]int{"s-a": 2, "s-b": 3} {
		if msgs, _ := acme.GetMessages(ctx, sid, state.GetMessagesOptions{}); len(msgs) != n {
			t.Fatalf("%s messages %d", sid, len(msgs))
		}
	}
	other, _ := acme.GetSession(ctx, "s-b")
	resident, _ := acme.GetSession(ctx, "w-1")
	if other.Vals["title"] != "Weekend plans" || resident.Vals["title"] != title {
		t.Fatalf("titles %v %v", other.Vals["title"], resident.Vals["title"])
	}
	if resolved, err := acme.ResolveByTitle(title); err != nil || resolved != "w-1" {
		t.Fatalf("resolved %q %v", resolved, err)
	}
	clash, _ := acme.GetSession(ctx, "s-a")
	moved, _ := clash.Vals["title"].(string)
	if moved == "" || moved == title || len([]rune(moved)) > state.MaxTitleLength {
		t.Fatalf("moved title %q", moved)
	}
	for _, f := range repairJSONReport(t).Findings {
		if f.Kind == "wrong_store" {
			t.Fatalf("wrong_store remains %+v", f)
		}
	}
}

func repairAdmit(t *testing.T, path, sid, status string) {
	t.Helper()
	conn, err := state.ConnectSQLite(path, state.SQLiteConfig{Timeout: 5})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var outcome any
	if status == "terminal" {
		outcome = "completed"
	}
	if _, err := conn.ExecContext(context.Background(), "INSERT INTO session_admissions(admission_id,request_id,principal_id,target_session_id,lineage_json,"+
		"payload_json,payload_digest,intent,status,outcome,owner_epoch,generation) "+
		"VALUES(?,?,'p','"+sid+"','[]','{}','d','queue',?,?,1,1)", "adm-"+sid+"-"+status, "req-"+sid, status, outcome); err != nil {
		t.Fatal(err)
	}
}

func TestALedgeredStrayIsReportedNeverCopied(t *testing.T) {
	for _, status := range []string{"terminal", "queued"} {
		t.Run(status, func(t *testing.T) {
			homes := repairHomes(t)
			root := repairOpen(t, homes["default"])
			repairSession(t, root, "led", "agent:acme:telegram:dm:230", "acme", "", 2)
			repairSession(t, root, "led-kid", "agent:acme:telegram:dm:230", "acme", "led", 2)
			repairSession(t, root, "free", "agent:acme:telegram:dm:231", "acme", "", 2)
			root.Close()
			repairAdmit(t, filepath.Join(homes["default"], "state.db"), "led", status)
			findings := map[string]repairReportFinding{}
			for _, f := range repairJSONReport(t).Findings {
				if f.Kind == "wrong_store" {
					findings[f.Subject] = f
				}
			}
			if f := findings["led"]; f.Action != nil || !strings.Contains(*f.Reason, "admission") {
				t.Fatalf("led %+v", f)
			}
			if f := findings["led-kid"]; f.Action != nil || !strings.Contains(*f.Reason, "led") {
				t.Fatalf("led-kid %+v", f)
			}
			if f := findings["free"]; f.Action == nil || *f.Action != "move to the acme store" {
				t.Fatalf("free %+v", f)
			}
			for i := 0; i < 2; i++ {
				if code, out, _ := repairRun(t, map[string]any{"apply": true}, nil, nil); code != 0 {
					t.Fatalf("exit %d: %s", code, out)
				}
			}
			ctx := context.Background()
			root, acme := repairOpen(t, homes["default"]), repairOpen(t, homes["acme"])
			defer root.Close()
			defer acme.Close()
			for _, sid := range []string{"led", "led-kid"} {
				r, _ := root.GetSession(ctx, sid)
				a, _ := acme.GetSession(ctx, sid)
				if r == nil || a != nil {
					t.Fatalf("%s moved", sid)
				}
			}
			if kid, _ := root.GetSession(ctx, "led-kid"); kid.Vals["parent_session_id"] != "led" {
				t.Fatalf("led-kid parent %v", kid.Vals["parent_session_id"])
			}
			free, _ := root.GetSession(ctx, "free")
			msgs, _ := acme.GetMessages(ctx, "free", state.GetMessagesOptions{})
			if free != nil || len(msgs) != 2 {
				t.Fatalf("free %v %d", free, len(msgs))
			}
		})
	}
}

func TestLedgerRowsThatAppearAfterTheScanStillBlockTheCopy(t *testing.T) {
	homes := repairHomes(t)
	root := repairOpen(t, homes["default"])
	repairSession(t, root, "late", "agent:acme:telegram:dm:240", "acme", "", 2)
	root.Close()
	ctx := context.Background()
	plan, session, err := scanStores(ctx, nil, "report", false)
	if err != nil {
		t.Fatal(err)
	}
	repairAdmit(t, filepath.Join(homes["default"], "state.db"), "late", "terminal")
	result, err := plan.apply(ctx, session, func(context.Context, repairProfilesStore) (string, bool) { return "snap", true })
	session.close()
	if err != nil {
		t.Fatal(err)
	}
	failures, _ := rowGet(result, "failures").([]any)
	if len(failures) != 1 {
		t.Fatalf("failures %v", failures)
	}
	first := failures[0].(interface{ Get(string) (any, bool) })
	subject, _ := first.Get("subject")
	errText, _ := first.Get("error")
	if subject != "late" || !strings.Contains(errText.(string), "admission/worker history") {
		t.Fatalf("failure %v %v", subject, errText)
	}
	r, _ := repairOpen(t, homes["default"]).GetSession(ctx, "late")
	a, _ := repairOpen(t, homes["acme"]).GetSession(ctx, "late")
	if r == nil || a != nil {
		t.Fatal("late moved")
	}
}
