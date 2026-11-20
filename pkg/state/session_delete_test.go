package state

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Installs gateway and tools doubles for the retirement hooks, returning the released media references
func installRetirementHooks(t *testing.T) *[]any {
	t.Helper()
	released := &[]any{}
	prevA, prevH, prevR := AdmissionMediaReferences, HostedDocumentReferences, ReleaseUnheldMedia
	prevF, prevN, prevS := SpilloverSQLFilter, SpilloverNamesInText, ReferencedSpilloverNames
	AdmissionMediaReferences = func(payload *pyjson.Object) ([]*pyjson.Object, error) {
		var out []*pyjson.Object
		for _, field := range []string{"attachments_v1", "native_text_v1"} {
			section, _ := payload.Get(field)
			if so, ok := section.(*pyjson.Object); ok {
				media, _ := so.Get("media")
				list, _ := media.([]any)
				for _, m := range list {
					out = append(out, m.(*pyjson.Object))
				}
			}
		}
		return out, nil
	}
	HostedDocumentReferences = func(string, *pyjson.Object) ([]*pyjson.Object, error) { return nil, nil }
	ReleaseUnheldMedia = func(_ *SessionDB, refs []any) error {
		*released = append(*released, refs...)
		return nil
	}
	SpilloverSQLFilter = "(instr(content, 'spillover') > 0 OR instr(api_content, 'spillover') > 0)"
	SpilloverNamesInText = func(texts ...string) map[string]bool { return map[string]bool{} }
	ReferencedSpilloverNames = func(string) (map[string]bool, bool) { return map[string]bool{}, true }
	t.Cleanup(func() {
		AdmissionMediaReferences, HostedDocumentReferences, ReleaseUnheldMedia = prevA, prevH, prevR
		SpilloverSQLFilter, SpilloverNamesInText, ReferencedSpilloverNames = prevF, prevN, prevS
	})
	return released
}

func wantGuardError(t *testing.T, err error) {
	t.Helper()
	var guard *SessionActiveWriteGuardError
	if !errors.As(err, &guard) {
		t.Fatalf("want SessionActiveWriteGuardError, got %v", err)
	}
}

func TestDeleteSessionRefusesWhenWriteGuardActive(t *testing.T) {
	installRetirementHooks(t)
	db := leaseDB(t)
	ctx := context.Background()
	readSession(t, db, NewSession{ID: "sess-lease", Source: "test"})
	readSession(t, db, NewSession{ID: "sess-cmp", Source: "test"})
	turn, cmp := leaseHolder("1"), strings.Replace(leaseHolder("1"), ":turn=", ":cmp=", 1)
	if !leaseTry(t, db, "sess-lease", turn, 300) || !db.TryAcquireCompressionLock("sess-cmp", cmp, 300) {
		t.Fatal("guards")
	}
	guarded := DeleteSessionOptions{ExcludeActiveWriteGuards: true}
	for _, sid := range []string{"sess-lease", "sess-cmp"} {
		_, err := db.DeleteSession(ctx, sid, guarded)
		wantGuardError(t, err)
		if guardSession(t, db, sid) == nil {
			t.Fatalf("%s deleted under its guard", sid)
		}
	}
	leaseRelease(t, db, "sess-lease", turn)
	db.ReleaseCompressionLock("sess-cmp", cmp)
	readSession(t, db, NewSession{ID: "sess-compressed", Source: "test"})
	leaseEnd(t, db, "sess-compressed")
	for _, sid := range []string{"sess-lease", "sess-cmp", "sess-compressed"} {
		if ok, err := db.DeleteSession(ctx, sid, guarded); err != nil || !ok {
			t.Fatalf("delete %s = %v, %v", sid, ok, err)
		}
		if guardSession(t, db, sid) != nil {
			t.Fatalf("%s survived", sid)
		}
	}
}

func TestDeleteSessionsBulkSkipsActiveWriteGuards(t *testing.T) {
	installRetirementHooks(t)
	db := leaseDB(t)
	ctx := context.Background()
	readSession(t, db, NewSession{ID: "bulk-active", Source: "test"})
	readSession(t, db, NewSession{ID: "bulk-idle", Source: "test"})
	turn := leaseHolder("bulk")
	if !leaseTry(t, db, "bulk-active", turn, 300) {
		t.Fatal("lease")
	}
	skipped := []string{}
	n, err := db.DeleteSessions(ctx, []string{"bulk-active", "bulk-idle"}, DeleteSessionsOptions{ExcludeActiveWriteGuards: true, SkippedIDs: &skipped})
	if err != nil || n != 1 || !reflect.DeepEqual(skipped, []string{"bulk-active"}) {
		t.Fatalf("bulk = %d, %v skipped %v", n, err, skipped)
	}
	if guardSession(t, db, "bulk-active") == nil || guardSession(t, db, "bulk-idle") != nil {
		t.Fatal("bulk delete removed the wrong rows")
	}
	leaseRelease(t, db, "bulk-active", turn)
	readSession(t, db, NewSession{ID: "deleg-root", Source: "test"})
	readSession(t, db, NewSession{ID: "deleg-child", Source: "test", ParentSessionID: "deleg-root", ModelConfig: map[string]any{"_delegate_from": "deleg-root"}})
	child := leaseHolder("child")
	if !leaseTry(t, db, "deleg-child", child, 300) {
		t.Fatal("child lease")
	}
	_, err = db.DeleteSession(ctx, "deleg-root", DeleteSessionOptions{ExcludeActiveWriteGuards: true})
	wantGuardError(t, err)
	skipped = []string{}
	if n, err := db.DeleteSessions(ctx, []string{"deleg-root"}, DeleteSessionsOptions{ExcludeActiveWriteGuards: true, SkippedIDs: &skipped}); err != nil || n != 0 || !reflect.DeepEqual(skipped, []string{"deleg-root"}) {
		t.Fatalf("guarded root bulk = %d, %v skipped %v", n, err, skipped)
	}
	if guardSession(t, db, "deleg-root") == nil || guardSession(t, db, "deleg-child") == nil {
		t.Fatal("guarded delegate cascade went through")
	}
	leaseRelease(t, db, "deleg-child", child)
}

func TestDeleteSessionIfEmptyRefusesWhenWriteGuardActive(t *testing.T) {
	installRetirementHooks(t)
	db := leaseDB(t)
	ctx := context.Background()
	readSession(t, db, NewSession{ID: "empty-live", Source: "test"})
	turn := leaseHolder("1")
	if !leaseTry(t, db, "empty-live", turn, 300) {
		t.Fatal("lease")
	}
	if ok, err := db.DeleteSessionIfEmpty(ctx, "empty-live", ""); err != nil || ok || guardSession(t, db, "empty-live") == nil {
		t.Fatalf("guarded empty delete = %v, %v", ok, err)
	}
	leaseRelease(t, db, "empty-live", turn)
	if ok, err := db.DeleteSessionIfEmpty(ctx, "empty-live", ""); err != nil || !ok || guardSession(t, db, "empty-live") != nil {
		t.Fatalf("empty delete = %v, %v", ok, err)
	}
}

func TestDeleteRetiresTerminalReceiptsAndRoutes(t *testing.T) {
	released := installRetirementHooks(t)
	db := leaseDB(t)
	ctx := context.Background()
	readSession(t, db, NewSession{ID: "gone", Source: "test"})
	profileExec(t, db, "INSERT INTO state_meta(key,value) VALUES(?,?)", "gateway.api.settings.v1.gone", "{}")
	profileExec(t, db, "INSERT INTO state_meta(key,value) VALUES(?,?)", LocalAPIBindingPrefix+"gone", "{}")
	profileExec(t, db, "INSERT INTO state_meta(key,value) VALUES(?,?)", LocalPolicyPrefix+"owner", `{"entry": {"session_id": "gone"}}`)
	profileExec(t, db, "INSERT INTO state_meta(key,value) VALUES(?,?)", "gateway.mutation.v1.r1", `{"result": {"session_id": "gone", "summary": "secret", "revision": 3}}`)
	profileExec(t, db, "INSERT INTO state_meta(key,value) VALUES(?,?)", MediaRetiredPrefix+"x", `{"path": "/nonexistent/media.png"}`)
	if ok, err := db.DeleteSession(ctx, "gone", DeleteSessionOptions{}); err != nil || !ok {
		t.Fatalf("delete = %v, %v", ok, err)
	}
	meta := func(key string) any {
		row, err := db.readOne(ctx, "SELECT value FROM state_meta WHERE key=?", key)
		if err != nil {
			t.Fatal(err)
		}
		if row == nil {
			return nil
		}
		return rowValue(row, "value")
	}
	marker, _ := pyjson.Loads(pyStr(meta(RetiredPrefix + "gone")))
	if mo, ok := marker.(*pyjson.Object); !ok || mo.Vals["own"] != true {
		t.Fatalf("retired marker %v", marker)
	}
	for _, key := range []string{"gateway.api.settings.v1.gone", LocalAPIBindingPrefix + "gone", LocalPolicyPrefix + "owner", MediaRetiredPrefix + "x"} {
		if v := meta(key); v != nil {
			t.Fatalf("%s survived as %v", key, v)
		}
	}
	if got := meta("gateway.mutation.v1.r1"); got != `{"result":{"revision":3,"session_id":"gone","summary":null}}` {
		t.Fatalf("mutation receipt %v", got)
	}
	if len(*released) != 1 {
		t.Fatalf("released %v", *released)
	}
}

func TestTerminalResultEncodingRoundTrips(t *testing.T) {
	raw, _ := pyjson.Loads(`{"status": "ok", "result": {"final_response": "hi", "messages": [{"role": "user", "content": "q"}, {"role": "assistant", "content": "hi"}]}}`)
	enc := encodeTerminalResult(raw)
	text, _ := pyjson.Dumps(enc)
	if !strings.Contains(text, `"_stored": {"v": 1, "reply_ref": 1}`) || !strings.Contains(text, `{"role": "assistant", "content": null}`) {
		t.Fatalf("encoded %s", text)
	}
	back, err := loadTerminalResult(text)
	if err != nil || !pyjson.Equal(back, raw) {
		t.Fatalf("decoded %v, %v", back, err)
	}
	escaped, _ := pyjson.Loads(`{"_stored": 5}`)
	etext, _ := pyjson.Dumps(encodeTerminalResult(escaped))
	if back, _ := loadTerminalResult(etext); !pyjson.Equal(back, escaped) {
		t.Fatalf("raw envelope %v", back)
	}
	if back, _ := loadTerminalResult(`{"_stored": {"v": true, "raw": 1}}`); pyStr(back.(*pyjson.Object).Vals["_stored"]) == "1" {
		t.Fatal("bool version must not unwrap")
	}
}
