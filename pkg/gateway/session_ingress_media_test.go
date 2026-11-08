package gateway

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

// Plants one retained file under its digest directory and returns its path and reference
func plantRetained(t *testing.T, ctx context.Context, index int, name string) (string, *pyjson.Object) {
	t.Helper()
	root, err := nativeInputsRoot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	digest := fmt.Sprintf("%064x", index)
	if err := os.MkdirAll(filepath.Join(root, digest), 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, digest, name)
	if err := os.WriteFile(path, []byte(fmt.Sprint("bytes", index)), 0o600); err != nil {
		t.Fatal(err)
	}
	ref := pyjson.NewObject()
	ref.Set("path", path)
	ref.Set("sha256", digest)
	return path, ref
}

// Writes one admission row holding the payload in the given status
func admitRow(t *testing.T, ctx context.Context, db *state.SessionDB, id, requestID, status string, payload *pyjson.Object) {
	t.Helper()
	text, err := pyjson.Dumps(payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ExecuteWrite(ctx, func(conn state.SQLiteConn) error {
		_, err := conn.ExecContext(ctx, `INSERT INTO session_admissions(admission_id, request_id, principal_id,
			target_session_id, lineage_json, payload_json, payload_digest, intent, status, owner_epoch, generation)
			VALUES(?,?,?,?,?,?,?,?,?,1,1)`, id, requestID, "owner", "chat", "{}", text, "digest-"+id, "queue", status)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func newMediaTestDB(t *testing.T) (context.Context, *state.SessionDB) {
	ctx, db := newTestStateDB(t)
	if err := db.CreateSession(state.NewSession{ID: "chat", Source: "cli"}); err != nil {
		t.Fatal(err)
	}
	return ctx, db
}

func onDisk(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func mediaPayload(section string, refs ...*pyjson.Object) *pyjson.Object {
	media := []any{}
	for _, r := range refs {
		media = append(media, r)
	}
	inner := pyjson.NewObject()
	inner.Set("media", media)
	payload := pyjson.NewObject()
	payload.Set(section, inner)
	return payload
}

func TestHostedDocumentReferencesNamesOnlyRetainedDocuments(t *testing.T) {
	ctx, _ := newMediaTestDB(t)
	doc, _ := plantRetained(t, ctx, 1, "note.txt")
	image, _ := plantRetained(t, ctx, 2, "api_"+fmt.Sprintf("%032x", 2)+".png")
	foreign := filepath.Join(t.TempDir(), fmt.Sprintf("%064x", 3), "other.txt")
	text := "read" + HostedDocumentLine + doc + "\n" + HostedDocumentLine[1:] + image + "\n" +
		HostedDocumentLine[1:] + foreign + "\n" + HostedDocumentLine[1:] + "relative/x.txt\n"
	payload := pyjson.NewObject()
	payload.Set("text", text)
	refs, err := HostedDocumentReferences("hosted:room:turn", payload)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 {
		t.Fatalf("refs %v", refs)
	}
	path, _ := refs[0].Get("path")
	digest, _ := refs[0].Get("sha256")
	if path != doc || digest != fmt.Sprintf("%064x", 1) {
		t.Fatalf("ref %v %v", path, digest)
	}
	for _, requestID := range []string{"turn-1", "api:hosted:x"} {
		other, err := HostedDocumentReferences(requestID, payload)
		if err != nil || len(other) != 0 {
			t.Fatalf("%s gave %v %v", requestID, other, err)
		}
	}
	payload.Set("text", []any{text})
	if other, err := HostedDocumentReferences("hosted:room:turn", payload); err != nil || len(other) != 0 {
		t.Fatalf("non str text gave %v %v", other, err)
	}
}

func TestAdmissionMediaReferencesJoinsAttachmentAndNativeMedia(t *testing.T) {
	ctx, _ := newMediaTestDB(t)
	_, a := plantRetained(t, ctx, 1, "a.png")
	_, b := plantRetained(t, ctx, 2, "b.png")
	payload := mediaPayload("attachments_v1", a)
	native := pyjson.NewObject()
	native.Set("media", []any{b})
	payload.Set("native_text_v1", native)
	refs, err := AdmissionMediaReferences(payload)
	if err != nil || len(refs) != 2 || refs[0] != a || refs[1] != b {
		t.Fatalf("refs %v %v", refs, err)
	}
	if refs, err := AdmissionMediaReferences(pyjson.NewObject()); err != nil || len(refs) != 0 {
		t.Fatalf("empty payload gave %v %v", refs, err)
	}
	bad := pyjson.NewObject()
	bad.Set("attachments_v1", []any{})
	if _, err := AdmissionMediaReferences(bad); err == nil || err.Error() != "'list' object has no attribute 'get'" {
		t.Fatalf("list section error %v", err)
	}
	inner := pyjson.NewObject()
	inner.Set("media", "ab")
	bad.Set("attachments_v1", inner)
	if _, err := AdmissionMediaReferences(bad); err == nil || err.Error() != "string indices must be integers, not 'str'" {
		t.Fatalf("str media error %v", err)
	}
	inner.Set("media", 3)
	if _, err := AdmissionMediaReferences(bad); err == nil || err.Error() != "'int' object is not iterable" {
		t.Fatalf("int media error %v", err)
	}
}

func TestReleaseUnheldMediaKeepsWhatTheTranscriptNames(t *testing.T) {
	ctx, db := newMediaTestDB(t)
	kept, keptRef := plantRetained(t, ctx, 1, "kept.png")
	escaped, escapedRef := plantRetained(t, ctx, 2, "escaped.png")
	gone, goneRef := plantRetained(t, ctx, 3, "gone.png")
	for _, content := range []string{
		"notes " + fmt.Sprintf("%064x", 1) + "/kept.png",
		`{"p": "` + fmt.Sprintf("%064x", 2) + `\\escaped.png"}`,
	} {
		if _, err := db.AppendMessage(ctx, "chat", map[string]any{"role": "user", "content": content}, state.AppendGuards{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := ReleaseUnheldMediaRetainingHistory(db, []any{keptRef, escapedRef, goneRef}); err != nil {
		t.Fatal(err)
	}
	if !onDisk(kept) || !onDisk(escaped) || onDisk(gone) || onDisk(filepath.Dir(gone)) {
		t.Fatalf("kept %v escaped %v gone %v dir %v", onDisk(kept), onDisk(escaped), onDisk(gone), onDisk(filepath.Dir(gone)))
	}
	released, err := releaseUnheldMedia(ctx, db, []any{keptRef, escapedRef}, false)
	if err != nil || released != 2 || onDisk(kept) || onDisk(escaped) {
		t.Fatalf("history blind release %d %v", released, err)
	}
}

func TestReleaseUnheldMediaKeepsLiveAdmissionAndAPIImageMedia(t *testing.T) {
	ctx, db := newMediaTestDB(t)
	live, liveRef := plantRetained(t, ctx, 1, "live.png")
	settled, settledRef := plantRetained(t, ctx, 2, "settled.png")
	api, apiRef := plantRetained(t, ctx, 3, "api_"+fmt.Sprintf("%032x", 3)+".png")
	doc, _ := plantRetained(t, ctx, 4, "doc.txt")
	admitRow(t, ctx, db, "a-live", "turn-live", "queued", mediaPayload("native_text_v1", liveRef))
	admitRow(t, ctx, db, "a-settled", "turn-settled", "terminal", mediaPayload("attachments_v1", settledRef))
	apiTurn := pyjson.NewObject()
	apiTurn.Set("media", []any{apiRef})
	apiPayload := pyjson.NewObject()
	apiPayload.Set("api_turn_v1", apiTurn)
	admitRow(t, ctx, db, "a-api", "turn-api", "terminal", apiPayload)
	hosted := pyjson.NewObject()
	hosted.Set("text", "read"+HostedDocumentLine+doc+"\n")
	admitRow(t, ctx, db, "a-hosted", "hosted:room:turn", "started", hosted)
	docRefs, err := HostedDocumentReferences("hosted:room:turn", hosted)
	if err != nil || len(docRefs) != 1 {
		t.Fatalf("doc refs %v %v", docRefs, err)
	}
	released, err := releaseUnheldMedia(ctx, db, []any{liveRef, settledRef, apiRef, docRefs[0]}, false)
	if err != nil || released != 1 {
		t.Fatalf("released %d %v", released, err)
	}
	if !onDisk(live) || onDisk(settled) || !onDisk(api) || !onDisk(doc) {
		t.Fatalf("live %v settled %v api %v doc %v", onDisk(live), onDisk(settled), onDisk(api), onDisk(doc))
	}
}

func TestReleaseUnheldMediaRetainsLeasedAliasedAndMissingHeldPaths(t *testing.T) {
	ctx, db := newMediaTestDB(t)
	leased, leasedRef := plantRetained(t, ctx, 1, "leased.png")
	mediaLeaseMu.Lock()
	mediaLeases[leased]++
	mediaLeaseMu.Unlock()
	t.Cleanup(func() {
		mediaLeaseMu.Lock()
		delete(mediaLeases, leased)
		mediaLeaseMu.Unlock()
	})
	held, heldRef := plantRetained(t, ctx, 2, "held.png")
	alias, aliasRef := plantRetained(t, ctx, 3, "alias.png")
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(held, alias); err != nil {
		t.Fatal(err)
	}
	admitRow(t, ctx, db, "a-held", "turn-held", "queued", mediaPayload("attachments_v1", heldRef))
	released, err := releaseUnheldMedia(ctx, db, []any{leasedRef, aliasRef}, false)
	if err != nil || released != 0 || !onDisk(leased) || !onDisk(alias) {
		t.Fatalf("released %d %v leased %v alias %v", released, err, onDisk(leased), onDisk(alias))
	}
	if err := os.Remove(held); err != nil {
		t.Fatal(err)
	}
	released, err = releaseUnheldMedia(ctx, db, []any{aliasRef}, false)
	if err != nil || released != 0 || !onDisk(alias) {
		t.Fatalf("missing held path released %d %v", released, err)
	}
}

func TestReleaseUnheldMediaIgnoresForeignAndRejectsMalformedReferences(t *testing.T) {
	ctx, db := newMediaTestDB(t)
	foreign := filepath.Join(t.TempDir(), fmt.Sprintf("%064x", 1), "x.png")
	if err := os.MkdirAll(filepath.Dir(foreign), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(foreign, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	ref := pyjson.NewObject()
	ref.Set("path", foreign)
	ref.Set("sha256", fmt.Sprintf("%064x", 1))
	_, mismatched := plantRetained(t, ctx, 2, "m.png")
	mismatched.Set("sha256", "other")
	released, err := releaseUnheldMedia(ctx, db, []any{ref, mismatched}, true)
	if err != nil || released != 0 || !onDisk(foreign) {
		t.Fatalf("released %d %v", released, err)
	}
	noPath := pyjson.NewObject()
	if _, err := releaseUnheldMedia(ctx, db, []any{noPath}, true); err == nil || err.Error() != "'path'" {
		t.Fatalf("missing path error %v", err)
	}
	intPath := pyjson.NewObject()
	intPath.Set("path", 7)
	if _, err := releaseUnheldMedia(ctx, db, []any{intPath}, true); err == nil || !strings.Contains(err.Error(), "not 'int'") {
		t.Fatalf("int path error %v", err)
	}
}
