package keys

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Two programs of different versions sharing one file settle after one write each instead of rewriting it at every poll
func TestSharedFileNeverPingPongs(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	newer, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	older := Default()
	older.path = path
	gone := older.actions[len(older.actions)-1].ID
	older.actions = older.actions[:len(older.actions)-1]
	delete(older.binds, gone)
	delete(older.index, gone)
	if err := older.Reload(); err == nil {
		t.Fatalf("the older program read %s without noticing it", gone)
	}
	if newer.Stale() {
		t.Fatal("a read by the older program made the file stale for the newer one")
	}
	if err := older.Save(); err != nil {
		t.Fatal(err)
	}
	if !newer.Stale() {
		t.Fatal("the older program wrote the file and the newer one did not see it")
	}
	if err := newer.Reload(); err != nil {
		t.Fatal(err)
	}
	if _, ok := newer.Action(gone); !ok {
		t.Fatalf("the newer program lost %s on a reload", gone)
	}
	if older.Stale() {
		t.Fatal("a reload by the newer program wrote the file again")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != string(older.Text()) {
		t.Fatal("the file changed under a reload")
	}
	if newer.Stale() {
		t.Fatal("the newer program keeps seeing the file as stale")
	}
	time.Sleep(10 * time.Millisecond)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if newer.Stale() || older.Stale() {
		t.Fatal("the same text written again read as an edit")
	}
	if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !newer.Stale() {
		t.Fatal("an outside edit was missed")
	}
}

// A file that lacks actions of this version is written whole once at open
func TestOpenCompletesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	if err := os.WriteFile(path, []byte("{\n  \"global\": {\n    \"palette\": [\"alt+space\"]\n  }\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if m.First("palette") != "alt+space" {
		t.Fatalf("palette bound to %q after open", m.First("palette"))
	}
	data, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(data) != string(m.Text()) {
		t.Fatal("open did not write the file whole")
	}
	if m.Stale() {
		t.Fatal("the file open wrote reads as stale")
	}
}
