package acp

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

func wslHost(t *testing.T) {
	t.Helper()
	previous := home.SetWSLProbe(func() bool { return true })
	t.Cleanup(func() { home.SetWSLProbe(previous) })
}

func jsonField(t *testing.T, row *pyjson.Object, key string) *pyjson.Object {
	t.Helper()
	text, _ := row.GetOr(key, nil).(string)
	v, err := pyjson.Loads(text)
	if err != nil {
		t.Fatalf("%s is not JSON: %v", key, err)
	}
	obj, ok := v.(*pyjson.Object)
	if !ok {
		t.Fatalf("%s is not an object: %#v", key, v)
	}
	return obj
}

func getRow(t *testing.T, db *state.SessionDB, sid string) *pyjson.Object {
	t.Helper()
	row, err := db.GetSession(context.Background(), sid)
	if err != nil {
		t.Fatal(err)
	}
	return row
}

func TestTranslateAcpCwdConvertsWindowsDrivePathWhenWsl(t *testing.T) {
	wslHost(t)
	if got := translateACPCwd(`E:\Projects\AI\paperclip`); got != "/mnt/e/Projects/AI/paperclip" {
		t.Fatalf("got %q", got)
	}
}

func TestSymlinkAliasComparesEqual(t *testing.T) {
	dir := t.TempDir()
	realDir, alias := filepath.Join(dir, "real"), filepath.Join(dir, "alias")
	if err := os.Mkdir(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realDir, alias); err != nil {
		t.Skip("symlinks unavailable")
	}
	if normalizeCwdForCompare(alias) != normalizeCwdForCompare(realDir) {
		t.Fatal("alias and target compare different")
	}
}

func TestDistinctDirsStillCompareDifferent(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	for _, p := range []string{a, b} {
		if err := os.Mkdir(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if normalizeCwdForCompare(a) == normalizeCwdForCompare(b) {
		t.Fatal("distinct dirs compare equal")
	}
}

func TestMissingPathKeepsLexicalNormalization(t *testing.T) {
	if normalizeCwdForCompare("/nonexistent-icebath-test/x/../y") != normalizeCwdForCompare("/nonexistent-icebath-test/y") {
		t.Fatal("missing path not normalized lexically")
	}
}
