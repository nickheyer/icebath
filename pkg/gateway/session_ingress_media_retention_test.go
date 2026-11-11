package gateway

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/mediacache"
	"github.com/nickheyer/icebath/pkg/state"
)

// Forgets when each store was last swept so the next settlement sweeps again
func resetMediaSweep() {
	mediaSweepMu.Lock()
	defer mediaSweepMu.Unlock()
	clear(mediaLastSweep)
}

func TestSettledNativeDocumentNamedByTheTranscriptSurvivesSettlement(t *testing.T) {
	notes := map[string]func(path string) string{
		"host-path": func(path string) string {
			return "[The user sent a document: 'report.pdf'. It is saved at: " + path + ".]"
		},
		"agent-visible-path": func(path string) string {
			return "It is saved at: /root/.hermes/cache/documents/" + strings.SplitN(path, "/documents/", 2)[1]
		},
	}
	for _, name := range []string{"host-path", "agent-visible-path"} {
		t.Run(name, func(t *testing.T) {
			resetMediaSweep()
			ctx, db := newTestStateDB(t)
			documents, err := mediacache.DocumentCacheDir(ctx, home.Dir(ctx))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(documents, 0o700); err != nil {
				t.Fatal(err)
			}
			staged := filepath.Join(documents, "doc_1_report.pdf")
			if err := os.WriteFile(staged, []byte("%PDF-1.4 retained"), 0o600); err != nil {
				t.Fatal(err)
			}
			media, err := captureNativeMedia(ctx, []string{staged})
			if err != nil {
				t.Fatal(err)
			}
			retained := rowString(media[0].(*pyjson.Object), "path")
			if err := db.CreateSession(state.NewSession{ID: "s", Source: "telegram"}); err != nil {
				t.Fatal(err)
			}
			if _, err := db.AppendMessage(ctx, "s", map[string]any{"role": "user", "content": notes[name](retained)}, state.AppendGuards{}); err != nil {
				t.Fatal(err)
			}
			epoch, err := db.BeginRuntimeEpoch(ctx, "owner")
			if err != nil {
				t.Fatal(err)
			}
			row, err := db.AdmitSessionInput(ctx, state.SessionInput{Epoch: epoch, PrincipalID: "p", SessionID: "s", RequestID: "m1",
				Payload: pyjson.ObjectOf("text", "page 3?", "native_text_v1", pyjson.ObjectOf("media", media))})
			if err != nil {
				t.Fatal(err)
			}
			claim, _, err := db.ClaimSessionInput(ctx, state.ClaimInput{Epoch: epoch, SessionID: "s"})
			if err != nil || claim == nil {
				t.Fatalf("claim %v %v", claim, err)
			}
			if _, err := db.SettleSessionInput(ctx, state.SettleInput{Epoch: epoch, AdmissionID: rowString(row, "admission_id"),
				Generation: *rowInt(claim, "generation"), Outcome: "completed"}); err != nil {
				t.Fatal(err)
			}
			if err := releaseAdmissionMedia(ctx, db, rowString(row, "admission_id")); err != nil {
				t.Fatal(err)
			}
			if !onDisk(retained) {
				t.Fatal("the follow-up turn is told to open a path settlement deleted")
			}
			old := time.Now().Add(-25 * time.Hour)
			if err := os.Chtimes(retained, old, old); err != nil {
				t.Fatal(err)
			}
			resetMediaSweep()
			if err := releaseAdmissionMedia(ctx, db, rowString(row, "admission_id")); err != nil {
				t.Fatal(err)
			}
			root, err := nativeInputsRoot(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if onDisk(retained) || len(dirEntryNames(t, root)) != 0 {
				t.Fatalf("aged settled media stayed retained: %v", dirEntryNames(t, root))
			}
		})
	}
}
