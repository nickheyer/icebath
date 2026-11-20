package backup

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Snapshot directory with an optional manifest body under a home
func mkSnapshot(t *testing.T, h, id, manifest string) string {
	t.Helper()
	d := filepath.Join(QuickSnapshotRoot(h), id)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if manifest != "" {
		os.WriteFile(filepath.Join(d, "manifest.json"), []byte(manifest), 0o644)
	}
	return d
}

func TestListQuickSnapshotsNewestFirstWithStubsForBrokenManifests(t *testing.T) {
	h := t.TempDir()
	if got, err := ListQuickSnapshots(20, h); err != nil || len(got) != 0 {
		t.Fatalf("empty %v %v", got, err)
	}
	mkSnapshot(t, h, "20260101-000000", `{"id": "20260101-000000", "file_count": 3}`)
	mkSnapshot(t, h, "20260102-000000", "\xef\xbb\xbf"+`{"id": "20260102-000000"}`)
	mkSnapshot(t, h, "20260103-000000", "{broken")
	mkSnapshot(t, h, "20260104-000000", "")
	mkSnapshot(t, h, ".hidden", `{"id": "hidden"}`)
	mkSnapshot(t, h, "20260105-000000.partial", `{"id": "partial"}`)
	got, err := ListQuickSnapshots(20, h)
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{}
	for _, m := range got {
		ids = append(ids, m.(*pyjson.Object).Vals["id"].(string))
	}
	if len(ids) != 3 || ids[0] != "20260103-000000" || ids[1] != "20260102-000000" || ids[2] != "20260101-000000" {
		t.Fatalf("ids %v", ids)
	}
	if stub := got[0].(*pyjson.Object); stub.Vals["file_count"] != int64(0) || stub.Vals["total_size"] != int64(0) {
		t.Fatalf("stub %v", stub.Vals)
	}
	if got, _ := ListQuickSnapshots(1, h); len(got) != 1 {
		t.Fatalf("limit %v", got)
	}
	mkSnapshot(t, h, "20260109-000000", "\xff\xfe")
	if _, err := ListQuickSnapshots(20, h); err == nil {
		t.Fatal("undecodable manifest swallowed")
	}
}

func TestQuickSnapshotDirRejectsInvalidAndEscapingIDs(t *testing.T) {
	h := t.TempDir()
	good := mkSnapshot(t, h, "20260101-000000", `{}`)
	mkSnapshot(t, h, "nomanifest", "")
	if got, err := QuickSnapshotDir("20260101-000000", h); got != good || err != nil {
		t.Fatalf("dir %q %v", got, err)
	}
	outside := filepath.Join(h, "outside")
	mkSnapshot(t, outside, "x", `{}`)
	os.Symlink(filepath.Join(QuickSnapshotRoot(outside), "x"), filepath.Join(QuickSnapshotRoot(h), "escape"))
	for _, id := range []string{"", ".", "..", "../x", `a\b`, "nomanifest", "missing", "escape"} {
		if got, err := QuickSnapshotDir(id, h); got != "" || err != nil {
			t.Errorf("id %q gave %q %v", id, got, err)
		}
	}
}

func TestPruneQuickSnapshotsKeepsTheNewestFinishedOnes(t *testing.T) {
	h := t.TempDir()
	if got, err := PruneQuickSnapshots(1, h); got != 0 || err != nil {
		t.Fatalf("missing root pruned %d %v", got, err)
	}
	for _, id := range []string{"20260101-000000", "20260102-000000", "20260103-000000", "20260104-000000.partial", ".tmp"} {
		mkSnapshot(t, h, id, `{}`)
	}
	if got, err := PruneQuickSnapshots(1, h); got != 2 || err != nil {
		t.Fatalf("pruned %d %v", got, err)
	}
	left, _ := entriesNewestFirst(QuickSnapshotRoot(h))
	if len(left) != 3 || left[0] != "20260104-000000.partial" || left[1] != "20260103-000000" || left[2] != ".tmp" {
		t.Fatalf("left %v", left)
	}
}
