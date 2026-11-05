package tools

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// Swaps a package variable for the duration of a test
func cpSwap[T any](t *testing.T, ptr *T, v T) {
	t.Helper()
	old := *ptr
	*ptr = v
	t.Cleanup(func() { *ptr = old })
}

// Thread safe log sink for asserting on warnings
type cpLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *cpLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *cpLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// Isolated checkpoint world with a temp base, a seeded project and a log capture
type cpFixture struct {
	t    *testing.T
	tmp  string
	base string
	work string
	env  *Env
	logs *cpLogBuffer
}

// Real path of a fresh temp directory
func cpTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// Builds the fixture, pointing the checkpoint base at tmp/checkpoints and seeding tmp/project
func newCPFixture(t *testing.T) *cpFixture {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Fatalf("git is required for checkpoint tests: %v", err)
	}
	tmp := cpTempDir(t)
	f := &cpFixture{t: t, tmp: tmp, base: filepath.Join(tmp, "checkpoints"), work: filepath.Join(tmp, "project"), logs: &cpLogBuffer{}}
	cpSwap(t, &checkpointBaseFn, func(*Env) string { return f.base })
	f.env = &Env{Home: filepath.Join(tmp, "icebath-home"), Log: slog.New(slog.NewTextHandler(f.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))}
	f.mkdir(f.work)
	f.write(filepath.Join(f.work, "main.py"), "print('hello')\n")
	f.write(filepath.Join(f.work, "README.md"), "# Project\n")
	return f
}

// Manager with the given budgets on the fixture env
func (f *cpFixture) mgr(enabled bool, maxSnapshots, maxTotalMB, maxFileMB int) *CheckpointManager {
	return NewCheckpointManager(f.env, CheckpointManagerOptions{Enabled: enabled, MaxSnapshots: maxSnapshots, MaxTotalSizeMB: maxTotalMB, MaxFileSizeMB: maxFileMB})
}

// Default enabled manager with fifty snapshots
func (f *cpFixture) enabled() *CheckpointManager { return f.mgr(true, 50, 500, 10) }

func (f *cpFixture) mkdir(p string) {
	f.t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		f.t.Fatal(err)
	}
}

func (f *cpFixture) write(p, content string) {
	f.t.Helper()
	f.mkdir(filepath.Dir(p))
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *cpFixture) read(p string) string {
	f.t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		f.t.Fatal(err)
	}
	return string(b)
}

func (f *cpFixture) exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// Store of the fixture base
func (f *cpFixture) store() string { return checkpointStorePath(f.base) }

// Newest checkpoint hash of a directory, failing when none exists
func (f *cpFixture) newest(m *CheckpointManager, dir string) string {
	f.t.Helper()
	cps := m.ListCheckpoints(dir)
	if len(cps) == 0 {
		f.t.Fatalf("no checkpoints for %s", dir)
	}
	return cps[0].Hash
}

// Reasons of the directory's checkpoints newest first
func cpReasons(m *CheckpointManager, dir string) []string {
	out := []string{}
	for _, c := range m.ListCheckpoints(dir) {
		out = append(out, c.Reason)
	}
	return out
}

// Runs git with user config neutralised and a fixed identity, failing the test on error
func cpGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=Test", "-c", "user.email=test@example.invalid"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return string(out)
}

// Creates a nested repository with one committed file
func cpNestedRepo(t *testing.T, dir, file, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cpGit(t, dir, "init", "-q")
	if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cpGit(t, dir, "add", file)
	cpGit(t, dir, "commit", "-qm", "seed")
}

// Boolean success of a result map
func cpSuccess(r map[string]any) bool {
	ok, _ := r["success"].(bool)
	return ok
}

// String list of a result map key
func cpList(r map[string]any, key string) []string {
	v, _ := r[key].([]string)
	return v
}

// Reports whether a list holds the value
func cpHas(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// Seeds a pre v2 per project shadow repo directly under the base, optionally backdated
func cpSeedLegacyRepo(t *testing.T, base, name, workdir string, mtime *time.Time) string {
	t.Helper()
	shadow := filepath.Join(base, name)
	for _, d := range []string{shadow, filepath.Join(shadow, "info")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{
		filepath.Join(shadow, "HEAD"):                  "ref: refs/heads/main\n",
		filepath.Join(shadow, checkpointWorkdirMarker): workdir + "\n",
		filepath.Join(shadow, "info", "exclude"):       "node_modules/\n",
	}
	for p, c := range files {
		if err := os.WriteFile(p, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if mtime != nil {
		filepath.WalkDir(shadow, func(p string, d os.DirEntry, err error) error {
			os.Chtimes(p, *mtime, *mtime)
			return nil
		})
	}
	return shadow
}

// Decoded JSON object file
func cpReadJSON(t *testing.T, p string) map[string]any {
	t.Helper()
	v, err := readCheckpointJSON(p)
	if err != nil {
		t.Fatal(err)
	}
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("%s is not an object", p)
	}
	return m
}

// Writes a JSON object file
func cpWriteJSON(t *testing.T, p string, v any) {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// Points HOME, USERPROFILE and ICEBATH_HOME at a fresh fake home
func cpFakeHome(t *testing.T, tmp string) string {
	t.Helper()
	h := filepath.Join(tmp, "home")
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", h)
	t.Setenv("USERPROFILE", h)
	t.Setenv("ICEBATH_HOME", filepath.Join(h, ".icebath"))
	t.Setenv("HOMEDRIVE", "")
	t.Setenv("HOMEPATH", "")
	return h
}
