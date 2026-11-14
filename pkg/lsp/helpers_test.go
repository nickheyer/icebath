package lsp

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var (
	mockOnce  sync.Once
	mockPath  string
	mockErr   error
	pkgDir, _ = os.Getwd()
)

// Builds the mock language server once per test binary
func mockBinary(t *testing.T) string {
	t.Helper()
	mockOnce.Do(func() {
		dir, err := os.MkdirTemp("", "lsp-mock-")
		if err != nil {
			mockErr = err
			return
		}
		mockPath = filepath.Join(dir, "mockserver")
		build := exec.Command("go", "build", "-o", mockPath, "./testdata/mockserver")
		build.Dir = pkgDir
		out, err := build.CombinedOutput()
		if err != nil {
			mockErr = fmt.Errorf("building the mock server: %v: %s", err, out)
		}
	})
	if mockErr != nil {
		t.Fatal(mockErr)
	}
	return mockPath
}

// One captured log record
type logRecord struct {
	level  slog.Level
	msg    string
	logger string
}

// Log handler keeping every record for assertions
type captureHandler struct {
	mu      sync.Mutex
	records []logRecord
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	rec := logRecord{level: r.Level, msg: r.Message}
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "logger" {
			rec.logger = a.Value.String()
		}
		return true
	})
	h.mu.Lock()
	h.records = append(h.records, rec)
	h.mu.Unlock()
	return nil
}

func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(string) slog.Handler      { return h }

func (h *captureHandler) all() []logRecord {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]logRecord{}, h.records...)
}

// Records whose message contains every fragment
func (h *captureHandler) matching(fragments ...string) []logRecord {
	var out []logRecord
	for _, r := range h.all() {
		ok := true
		for _, f := range fragments {
			if !strings.Contains(r.msg, f) {
				ok = false
				break
			}
		}
		if ok {
			out = append(out, r)
		}
	}
	return out
}

// Process ledger calls a test runtime saw
type ledger struct {
	mu       sync.Mutex
	owned    []int
	children []int
	released []int
}

func (l *ledger) snapshot() (owned, children, released []int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]int{}, l.owned...), append([]int{}, l.children...), append([]int{}, l.released...)
}

// Line diff opcodes from a longest common subsequence for the test runtime
func lcsOpcodes(a, b []string) []Opcode {
	n, m := len(a), len(b)
	dp := make([][]int, n+1)
	for i := range dp {
		dp[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else {
				dp[i][j] = max(dp[i+1][j], dp[i][j+1])
			}
		}
	}
	var ops []Opcode
	add := func(tag string, i1, i2, j1, j2 int) {
		if len(ops) > 0 && ops[len(ops)-1].Tag == tag {
			ops[len(ops)-1].I2, ops[len(ops)-1].J2 = i2, j2
			return
		}
		ops = append(ops, Opcode{tag, i1, i2, j1, j2})
	}
	i, j := 0, 0
	for i < n || j < m {
		switch {
		case i < n && j < m && a[i] == b[j]:
			add("equal", i, i+1, j, j+1)
			i, j = i+1, j+1
		case j < m && (i == n || dp[i][j+1] >= dp[i+1][j]):
			add("insert", i, i, j, j+1)
			j++
		default:
			add("delete", i, i+1, j, j)
			i++
		}
	}
	var merged []Opcode
	for k := 0; k < len(ops); k++ {
		if k+1 < len(ops) && ops[k].Tag == "delete" && ops[k+1].Tag == "insert" {
			merged = append(merged, Opcode{"replace", ops[k].I1, ops[k].I2, ops[k+1].J1, ops[k+1].J2})
			k++
			continue
		}
		merged = append(merged, ops[k])
	}
	return merged
}

// Test runtime with captured logs, a recording ledger and an empty managed package store
func testRuntime(t *testing.T) (*Runtime, *captureHandler, *ledger) {
	t.Helper()
	h := &captureHandler{}
	led := &ledger{}
	cfg := map[string]any{}
	rt := &Runtime{
		Home:   filepath.Join(t.TempDir(), "home"),
		Log:    slog.New(h),
		Config: func() map[string]any { return cfg },
		EnvFor: func(_ []string, base map[string]string) map[string]string {
			if base == nil {
				return map[string]string{}
			}
			return maps.Clone(base)
		},
		InstalledBinary: func(string) (string, bool) { return "", false },
		SubprocessEnv: func() map[string]string {
			out := map[string]string{}
			for _, kv := range os.Environ() {
				if k, v, ok := strings.Cut(kv, "="); ok {
					out[k] = v
				}
			}
			return out
		},
		DelegatedChildEnv: func(child map[string]string) map[string]string { return child },
		OwnProcessGroups: func(pgids []int) {
			led.mu.Lock()
			led.owned = append(led.owned, pgids...)
			led.mu.Unlock()
		},
		RegisterChild: func(_ context.Context, pid int, purpose string) bool {
			led.mu.Lock()
			led.children = append(led.children, pid)
			led.mu.Unlock()
			return true
		},
		ReleaseProcessGroupOnceEmpty: func(pgid int, _ func(), _ time.Duration) bool {
			led.mu.Lock()
			led.released = append(led.released, pgid)
			led.mu.Unlock()
			return true
		},
		Opcodes:    lcsOpcodes,
		DecodeJSON: func(text string) (any, error) { return decodeJSON([]byte(text)) },
	}
	return rt, h, led
}

// Command that runs the mock language server
func mockCommand(t *testing.T) []string { return []string{mockBinary(t)} }

// Env selecting a mock script
func mockEnv(script string, extra map[string]string) map[string]string {
	env := map[string]string{"MOCK_LSP_SCRIPT": script}
	maps.Copy(env, extra)
	return env
}

// Replaces a registered server with one spawning the mock on the workspace root, restored at cleanup
func installMockServer(t *testing.T, scripts []string, serverID string) func() int {
	t.Helper()
	idx := -1
	for i, s := range Servers {
		if s.ServerID == serverID {
			idx = i
		}
	}
	original := Servers[idx]
	var mu sync.Mutex
	count := 0
	Servers[idx] = &ServerDef{
		ServerID: serverID, Extensions: original.Extensions,
		ResolveRoot: func(_, ws string) string { return ws },
		BuildSpawn: func(root string, _ ServerContext) (*SpawnSpec, error) {
			mu.Lock()
			i := min(count, len(scripts)-1)
			count++
			mu.Unlock()
			return &SpawnSpec{Command: mockCommand(t), WorkspaceRoot: root, Cwd: root, Env: mockEnv(scripts[i], nil),
				InitializationOptions: map[string]any{}}, nil
		},
		Description: "mock " + serverID,
	}
	t.Cleanup(func() { Servers[idx] = original })
	return func() int {
		mu.Lock()
		defer mu.Unlock()
		return count
	}
}

// Git repo with a pyproject so pyright's root resolver finds it
func gitRepo(t *testing.T, parent, name string) string {
	t.Helper()
	repo := filepath.Join(parent, name)
	must(t, os.MkdirAll(filepath.Join(repo, ".git"), 0o755))
	must(t, os.WriteFile(filepath.Join(repo, "pyproject.toml"), []byte("[project]\nname='t'\n"), 0o644))
	return repo
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func writeFile(t *testing.T, path, text string) string {
	t.Helper()
	must(t, os.MkdirAll(filepath.Dir(path), 0o755))
	must(t, os.WriteFile(path, []byte(text), 0o644))
	return path
}

// Started loop for driving clients directly
func testLoop(t *testing.T) *loop {
	t.Helper()
	l := newLoop()
	l.start()
	t.Cleanup(l.stop)
	return l
}

// Runs fn as a loop task and fails the test on a scheduling timeout
func onLoop[T any](t *testing.T, l *loop, fn func(ctx context.Context) (T, error)) (T, error) {
	t.Helper()
	return runOnLoop(l, 60*time.Second, fn)
}

// Polls a condition until it holds or the timeout passes
func waitUntil(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Clears process caches between tests
func resetCaches(t *testing.T) {
	ClearWorkspaceCache()
	resetAnnounceCaches()
	t.Cleanup(func() {
		ClearWorkspaceCache()
		resetAnnounceCaches()
	})
}
