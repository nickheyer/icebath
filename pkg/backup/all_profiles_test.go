package backup

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// Writes a profile home with a config and an optional cron store of the given job count
func mkProfile(t *testing.T, h string, jobs int) string {
	t.Helper()
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte("model: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if jobs > 0 {
		list := []map[string]string{}
		for i := range jobs {
			list = append(list, map[string]string{"id": fmt.Sprintf("j%d", i)})
		}
		raw, _ := json.Marshal(map[string]any{"jobs": list})
		if err := os.MkdirAll(filepath.Join(h, "cron"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(h, "cron", "jobs.json"), raw, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return h
}

// Default home with three jobs plus work and sparks profiles, the root home in effect
func profilesFixture(t *testing.T) map[string]string {
	t.Helper()
	tmp := t.TempDir()
	def := mkProfile(t, filepath.Join(tmp, "home"), 3)
	work := mkProfile(t, filepath.Join(tmp, "home", "profiles", "work"), 5)
	sparks := mkProfile(t, filepath.Join(tmp, "home", "profiles", "sparks"), 0)
	t.Setenv("ICEBATH_HOME", def)
	return map[string]string{"default": def, "work": work, "sparks": sparks}
}

// Names of the sibling pairs in order
func siblingNames(pairs [][2]string) []string {
	names := []string{}
	for _, p := range pairs {
		names = append(names, p[0])
	}
	return names
}

func TestSiblingEnumerationExcludesInvokingProfile(t *testing.T) {
	p := profilesFixture(t)
	if got := siblingNames(SiblingProfileHomes(p["default"])); !reflect.DeepEqual(got, []string{"sparks", "work"}) {
		t.Fatalf("names %v", got)
	}
}

func TestSiblingEnumerationInvokedFromNamedProfileIncludesDefault(t *testing.T) {
	p := profilesFixture(t)
	got := SiblingProfileHomes(p["work"])
	want := [][2]string{{"default", p["default"]}, {"sparks", p["sparks"]}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("pairs %v want %v", got, want)
	}
}

func TestSiblingEnumerationNeverRaises(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("ICEBATH_HOME", filepath.Join(tmp, "missing", "home"))
	if got := SiblingProfileHomes(tmp); len(got) != 0 {
		t.Fatalf("pairs %v", got)
	}
	blocked := filepath.Join(tmp, "blocked")
	mkProfile(t, blocked, 0)
	if err := os.WriteFile(filepath.Join(blocked, "profiles"), []byte("not a dir"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", blocked)
	if got := SiblingProfileHomes(blocked); len(got) != 0 {
		t.Fatalf("pairs %v", got)
	}
}

func TestSiblingEnumerationSkipsInvalidIdsAndKeepsPythonEndAnchor(t *testing.T) {
	p := profilesFixture(t)
	for _, name := range []string{"Upper", "_lead", "default", "trail\n"} {
		mkProfile(t, filepath.Join(p["default"], "profiles", name), 0)
	}
	got := siblingNames(SiblingProfileHomes(p["default"]))
	if !reflect.DeepEqual(got, []string{"sparks", "trail\n", "work"}) {
		t.Fatalf("names %q", got)
	}
}

func TestQuickSnapshotRootIsStateSnapshotsUnderTheHome(t *testing.T) {
	tmp := t.TempDir()
	if got := QuickSnapshotRoot(tmp); got != filepath.Join(tmp, "state-snapshots") {
		t.Fatalf("root %s", got)
	}
	t.Setenv("ICEBATH_HOME", tmp)
	if got := QuickSnapshotRoot(""); got != filepath.Join(tmp, "state-snapshots") {
		t.Fatalf("default root %s", got)
	}
}
