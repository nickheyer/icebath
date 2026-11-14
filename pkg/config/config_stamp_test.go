package config

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"gopkg.in/yaml.v3"
)

func quietParseFailures(t *testing.T) {
	saved := ParseFailureOut
	ParseFailureOut = io.Discard
	ResetConfigParseFailures()
	t.Cleanup(func() { ParseFailureOut = saved; ResetConfigParseFailures() })
}

func writeConfig(t *testing.T, h string, data string) string {
	t.Helper()
	path := filepath.Join(h, "config.yaml")
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCheckConfigVersionUsesRawOnDiskVersion(t *testing.T) {
	h := t.TempDir()
	writeConfig(t, h, "model: {}\n")
	cur, latest, err := CheckConfigVersion(context.Background(), h, false)
	if err != nil || cur != 0 || latest != Version {
		t.Fatalf("got %d %d %v", cur, latest, err)
	}
	writeConfig(t, h, "_config_version: 7\n")
	if cur, _, _ := CheckConfigVersion(context.Background(), h, false); cur != 7 {
		t.Fatalf("stamped = %d", cur)
	}
	if cur, latest, _ := CheckConfigVersion(context.Background(), t.TempDir(), false); cur != latest {
		t.Fatalf("missing file = %d %d", cur, latest)
	}
}

func TestStrictCheckRejectsInvalidConfig(t *testing.T) {
	quietParseFailures(t)
	cases := []struct {
		name, data, match string
		tolerant          int
	}{
		{"malformed-yaml", "model: [unterminated\n", "not valid YAML", Version},
		{"list-root", "- just_a_list\n", "must be a mapping", 0},
		{"empty-list-root", "[]\n", "must be a mapping", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := t.TempDir()
			writeConfig(t, h, c.data)
			_, _, err := CheckConfigVersion(context.Background(), h, true)
			var inv *InvalidUserConfigError
			if !errors.As(err, &inv) || !strings.Contains(err.Error(), c.match) {
				t.Fatalf("strict err = %v", err)
			}
			cur, latest, err := CheckConfigVersion(context.Background(), h, false)
			if err != nil || cur != c.tolerant || latest != Version {
				t.Fatalf("tolerant = %d %d %v", cur, latest, err)
			}
		})
	}
}

func TestCoerceConfigVersion(t *testing.T) {
	for in, want := range map[any]int{true: 0, false: 0, nil: 0, 3: 3, -2: 0, 2.9: 2, "4": 4, " 5 ": 5, "2.5": 0, "x": 0} {
		if got := coerceConfigVersion(in); got != want {
			t.Fatalf("coerce(%v) = %d, want %d", in, got, want)
		}
	}
}

type stampRecorder struct {
	versions []int
	adopted  []string
	worker   bool
}

func (r *stampRecorder) hooks() StampHooks {
	return StampHooks{
		WorkerSnapshotActive: func() bool { return r.worker },
		CoreVersions:         []int{24, 31},
		RecordCoreUse: func(h string, cfg map[string]any, v int) bool {
			r.versions = append(r.versions, v)
			return true
		},
		AdoptAgentBrowserEnv: func(h string, cfg map[string]any) []string { return r.adopted },
	}
}

func TestNoteFirstStampRecordsEveryLeftCoreVersion(t *testing.T) {
	r := &stampRecorder{adopted: []string{"AGENT_BROWSER_X -> browser.x"}}
	got := NoteFirstStamp("h", map[string]any{}, map[string]any{"_config_version": 31}, r.hooks())
	if len(got) != 1 || len(r.versions) != 2 || r.versions[0] != 24 || r.versions[1] != 31 {
		t.Fatalf("moves %v versions %v", got, r.versions)
	}
	for _, c := range []struct{ onDisk, toWrite map[string]any }{
		{map[string]any{"_config_version": 3}, map[string]any{"_config_version": 31}},
		{map[string]any{}, map[string]any{"model": "x"}},
	} {
		r := &stampRecorder{adopted: []string{"m"}}
		if got := NoteFirstStamp("h", c.onDisk, c.toWrite, r.hooks()); got != nil || r.versions != nil {
			t.Fatalf("%v -> %v %v", c, got, r.versions)
		}
	}
	frozen := &stampRecorder{adopted: []string{"m"}, worker: true}
	if got := NoteFirstStamp("h", map[string]any{}, map[string]any{"_config_version": 1}, frozen.hooks()); got != nil || frozen.versions != nil {
		t.Fatalf("frozen worker stamped: %v %v", got, frozen.versions)
	}
}

func TestStampMissingConfigWritesOnlyWhenBrowserEnvMoves(t *testing.T) {
	t.Setenv("ICEBATH_MANAGED", "")
	h := t.TempDir()
	if wrote, err := StampMissingConfig(context.Background(), h, (&stampRecorder{}).hooks()); wrote || err != nil {
		t.Fatalf("nothing to adopt wrote %v %v", wrote, err)
	}
	if _, err := os.Stat(filepath.Join(h, "config.yaml")); !os.IsNotExist(err) {
		t.Fatal("config.yaml created without browser settings")
	}
	if wrote, err := StampMissingConfig(context.Background(), h, (&stampRecorder{adopted: []string{"m"}}).hooks()); !wrote || err != nil {
		t.Fatalf("adopting wrote %v %v", wrote, err)
	}
	data, _ := os.ReadFile(filepath.Join(h, "config.yaml"))
	var doc map[string]any
	if err := yaml.Unmarshal(data, &doc); err != nil || doc["_config_version"] != Version {
		t.Fatalf("written = %q", data)
	}
	if st, _ := os.Stat(filepath.Join(h, "config.yaml")); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %o", st.Mode().Perm())
	}
	if wrote, _ := StampMissingConfig(context.Background(), h, (&stampRecorder{adopted: []string{"m"}}).hooks()); wrote {
		t.Fatal("existing config restamped")
	}
	managed := t.TempDir()
	t.Setenv("ICEBATH_MANAGED", "nixos")
	if wrote, _ := StampMissingConfig(context.Background(), managed, (&stampRecorder{adopted: []string{"m"}}).hooks()); wrote {
		t.Fatal("managed home stamped")
	}
}

func TestParseFailureProbeTracksTheBrokenBytes(t *testing.T) {
	quietParseFailures(t)
	h := t.TempDir()
	path := writeConfig(t, h, "model: [unterminated\n")
	if _, err := ReadDocument(path); err != nil {
		if werr := WarnConfigParseFailure(context.Background(), path, err, "defaults"); werr != nil {
			t.Fatal(werr)
		}
	}
	if msg, ok := ActiveConfigParseFailure(h); !ok || msg == "" {
		t.Fatal("expected an active parse failure")
	}
	if backups := ListConfigBackups(path, "corrupt"); len(backups) != 1 {
		t.Fatalf("corrupt backups = %v", backups)
	}
	writeConfig(t, h, "model:\n  default: x\n")
	if _, ok := ActiveConfigParseFailure(h); ok {
		t.Fatal("fixed file still reported")
	}
	if _, ok := ActiveConfigParseFailure(t.TempDir()); ok {
		t.Fatal("valid home reported")
	}
}

func TestTransientReadErrorIsNotACorruptConfig(t *testing.T) {
	quietParseFailures(t)
	var out strings.Builder
	ParseFailureOut = &out
	h := t.TempDir()
	path := writeConfig(t, h, "display:\n  skin: mono\n")
	readErr := &os.PathError{Op: "open", Path: path, Err: syscall.EMFILE}
	saved := readFailureBytes
	readFailureBytes = func(string) ([]byte, error) { return nil, readErr }
	if werr := WarnConfigParseFailure(context.Background(), path, readErr, "defaults"); werr != nil {
		t.Fatal(werr)
	}
	if _, ok := ActiveConfigParseFailure(h); !ok {
		t.Fatal("unreadable file not reported while the fault holds")
	}
	readFailureBytes = saved
	if _, ok := ActiveConfigParseFailure(h); ok {
		t.Fatal("readable file still reported after a transient read error")
	}
	if got := ListConfigBackups(path, "corrupt"); len(got) != 0 {
		t.Fatalf("intact file copied away: %v", got)
	}
	if !strings.Contains(out.String(), "could not be read") {
		t.Fatalf("stderr = %q", out.String())
	}
}

func TestFormatConfigParseFailureWording(t *testing.T) {
	_, err := ReadDocument(writeConfig(t, t.TempDir(), "a: 1\nb: [x\n"))
	msg := FormatConfigParseFailure("/h/config.yaml", err, "last-known-good")
	if !strings.Contains(msg, "has a formatting error at line") || !strings.Contains(msg, "loaded before the edit") ||
		!strings.Contains(msg, "fix line") {
		t.Fatalf("msg = %q", msg)
	}
	perm := &os.PathError{Op: "open", Path: "/h/config.yaml", Err: syscall.EACCES}
	msg = FormatConfigParseFailure("/h/config.yaml", perm, "refuse-write")
	if !strings.Contains(msg, "could not be read. Nothing was written") || !strings.HasSuffix(msg, fixPerms) {
		t.Fatalf("perm msg = %q", msg)
	}
	if msg := FormatConfigParseFailure("/h/config.yaml", errors.New("boom"), "other"); !strings.Contains(msg, "default settings") ||
		!strings.Contains(msg, "fix the problem") {
		t.Fatalf("default msg = %q", msg)
	}
}

type backendDown struct{}

func (backendDown) Error() string          { return "backend down" }
func (backendDown) SystemExitCode() string { return "remote config unavailable" }

func TestRefuseFailedReadOnlyForFallbacks(t *testing.T) {
	h := t.TempDir()
	if err := RefuseFailedRead(h, "/h/config.yaml", map[string]any{"a": 1}); err != nil {
		t.Fatal(err)
	}
	read := &FailedConfigRead{ReadError: &os.PathError{Op: "open", Path: "/h/config.yaml", Err: syscall.EIO}}
	if err := RefuseFailedRead(h, "/h/config.yaml", read); err == nil || !strings.Contains(err.Error(), "could not be read, so this change was not saved. Try again") {
		t.Fatalf("read error refusal = %v", err)
	}
	exit := &FailedConfigRead{ReadError: backendDown{}}
	if err := RefuseFailedRead(h, "/h/config.yaml", exit); err == nil || err.Error() != "The config could not be read, so this change was not saved: remote config unavailable" {
		t.Fatalf("backend refusal = %v", err)
	}
	_, perr := ReadDocument(writeConfig(t, h, "a: [\n"))
	bad := &FailedConfigRead{ReadError: perr}
	err := RefuseFailedRead(h, "/h/config.yaml", bad)
	if err == nil || !strings.Contains(err.Error(), "has a formatting error (line") || !strings.Contains(err.Error(), "/backups/config/ over config.yaml") ||
		!strings.Contains(err.Error(), "Details: ") {
		t.Fatalf("yaml refusal = %v", err)
	}
}

func TestBackupConfigDedupesSweepsAndPrunes(t *testing.T) {
	h := t.TempDir()
	path := writeConfig(t, h, "a: 1\n")
	legacy := filepath.Join(h, "config.yaml.bak.1")
	os.WriteFile(legacy, []byte("old"), 0o600)
	first := mustBackup(t, path, "good", 2)
	if first == "" || mustBackup(t, path, "good", 2) != "" {
		t.Fatal("identical bytes backed up twice")
	}
	if _, err := os.Stat(filepath.Join(BackupsDir(path), "config.yaml.bak.1")); err != nil {
		t.Fatal("legacy sibling not swept")
	}
	if got, err := LoadNewestGoodBackup(context.Background(), path); err != nil || got["a"] != 1 {
		t.Fatalf("good backup = %v", got)
	}
	os.WriteFile(filepath.Join(BackupsDir(path), "config.yaml.good.20000101-000000"), []byte("a: 0\n"), 0o600)
	os.WriteFile(filepath.Join(BackupsDir(path), "config.yaml.good.19990101-000000"), []byte("a: -1\n"), 0o600)
	os.Remove(first)
	writeConfig(t, h, "a: 2\n")
	if mustBackup(t, path, "good", 2) == "" {
		t.Fatal("changed file not backed up")
	}
	if got := ListConfigBackups(path, "good"); len(got) != 2 {
		t.Fatalf("kept %v", got)
	}
	if mustBackup(t, filepath.Join(h, "missing.yaml"), "good", 2) != "" {
		t.Fatal("missing file backed up")
	}
}
