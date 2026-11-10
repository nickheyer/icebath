package backup

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Writes a file and its parents
func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// Realistic home tree with databases, profiles, the codebase checkout, bytecode and a pid file
func makeHomeTree(t *testing.T, root string) {
	t.Helper()
	writeTree(t, root, map[string]string{
		"config.yaml": "model:\n  provider: openrouter\n", ".env": "OPENROUTER_API_KEY=sk-test-123\n",
		"sessions/abc123.json": "{}", "skills/my-skill/SKILL.md": "# My Skill\n", "skins/cyber.yaml": "name: cyber\n",
		"cron/jobs.json": "[]", "memories/notes.json": "{}",
		"profiles/coder/config.yaml": "model:\n  provider: anthropic\n", "profiles/coder/.env": "ANTHROPIC_API_KEY=sk-ant-123\n",
		"icebath-agent/run_agent.py": "# big file\n", "icebath-agent/.git/HEAD": "ref: refs/heads/main\n",
		"plugins/__pycache__/mod.cpython-312.pyc": "\x00", "gateway.pid": "12345", "logs/agent.log": "log line\n",
	})
	for _, name := range []string{"memory_store.db", "icebath_state.db"} {
		c := testConn(t, filepath.Join(root, name), "")
		mustExec(t, c, "CREATE TABLE sample (value TEXT)", "INSERT INTO sample VALUES ('test')")
		closeQuietly(c)
	}
}

// Home relative slash paths a backup walk selects
func selectedRels(root, out string, skipped map[string]bool) map[string]bool {
	sel := map[string]bool{}
	for _, f := range iterBackupFiles(root, out, skipped) {
		sel[filepath.ToSlash(f.rel)] = true
	}
	return sel
}

// Fails for each excluded path a full backup keeps and each kept path it skips
func checkExclude(t *testing.T, excluded, kept []string) {
	t.Helper()
	for _, rel := range excluded {
		if !shouldExclude(filepath.FromSlash(rel)) {
			t.Errorf("kept %s", rel)
		}
	}
	for _, rel := range kept {
		if shouldExclude(filepath.FromSlash(rel)) {
			t.Errorf("excluded %s", rel)
		}
	}
}

func TestShouldExclude(t *testing.T) {
	t.Run("test_excludes_hermes_agent", func(t *testing.T) {
		checkExclude(t, []string{"icebath-agent/run_agent.py", "icebath-agent/.git/HEAD"}, nil)
	})
	t.Run("test_excludes_backups_dir", func(t *testing.T) {
		checkExclude(t, []string{"backups/pre-update-2026-04-27-063400.zip"}, nil)
	})
	t.Run("test_excludes_state_snapshots_dir", func(t *testing.T) {
		checkExclude(t, []string{quickSnapshotsDir + "/20260814-203829-2026-08-15/state.db",
			quickSnapshotsDir + "/20260814-203829-2026-08-15/manifest.json", "profiles/coder/" + quickSnapshotsDir + "/x/state.db"},
			[]string{"state.db"})
	})
	t.Run("test_excludes_sqlite_sidecars", func(t *testing.T) {
		checkExclude(t, []string{"state.db-wal", "state.db-shm", "state.db-journal", "memory_store.db-wal"}, []string{"state.db"})
	})
	t.Run("test_excludes_managed_runtime_trees_at_root", func(t *testing.T) {
		checkExclude(t, []string{"models/Qwen3.6-27B-Q4_K_M.gguf", "models/assets/mmproj.gguf",
			"runtimes/llamacpp/b10362/cuda/ggml-cuda.dll", "node/node.exe", "profiles/clean/models/big.gguf",
			"profiles/clean/runtimes/llamacpp/x.dll"}, nil)
	})
	t.Run("test_excludes_regenerable_cache_but_keeps_durable_artifacts", func(t *testing.T) {
		checkExclude(t, []string{"cache/model_catalog.json", "cache/chrome-debug/Default/Cookies", "profiles/sage/cache/chrome-debug/cache.db"},
			[]string{"cache/images/x.png", "profiles/sage/cache/citations/ledger.json", "skills/example/cache/notes.md"})
	})
	t.Run("test_keeps_nested_dirs_named_like_runtime_trees", func(t *testing.T) {
		checkExclude(t, nil, []string{"skills/mlops/models/notes.md", "scratch/node/index.js", "profiles/clean/skills/x/models/a.txt"})
	})
	t.Run("test_excludes_desktop_emergency_state_db_baks", func(t *testing.T) {
		checkExclude(t, []string{"state.db.pre-update-emergency-2026-08-15T04-55-33-619Z.bak",
			"profiles/coder/state.db.pre-update-emergency-2026-08-15T04-55-33-619Z.bak"}, []string{"config.yaml.bak"})
	})
}

func TestIterBackupFilesManualAndAutomaticPathsShareOneWalk(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, ".icebath")
	makeHomeTree(t, root)
	writeTree(t, root, map[string]string{"skills/autonomous-ai-agents/icebath-agent/SKILL.md": "# nested skill\n", "models/big.gguf": strings.Repeat("\x00", 64)})
	sel := selectedRels(root, filepath.Join(tmp, "out.zip"), nil)
	if !sel["skills/autonomous-ai-agents/icebath-agent/SKILL.md"] || sel["models/big.gguf"] {
		t.Fatalf("selected %v", sel)
	}
	for s := range sel {
		if strings.HasPrefix(s, "icebath-agent") {
			t.Fatalf("selected checkout file %s", s)
		}
	}
}

func TestIterBackupFilesPrunesBrowserUseCLIProfilesAtHomeRootsOnly(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, ".icebath")
	files := map[string]bool{
		"browser_profiles/browser-use-default/Default/Login Data":             false,
		"browser_profiles/browser-use-default/Default/Network/Cookies":        false,
		"profiles/coder/browser_profiles/browser-use-default/Default/Cookies": false,
		"skills/example/browser_profiles/notes.md":                            true,
	}
	for rel := range files {
		writeTree(t, root, map[string]string{rel: "x"})
	}
	sel := selectedRels(root, filepath.Join(tmp, "out.zip"), nil)
	for rel, keep := range files {
		if sel[rel] != keep {
			t.Errorf("%s selected %v", rel, sel[rel])
		}
	}
}

func TestIterBackupFilesPrunesRegenerableCachesButKeepsDurableAndNested(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, ".icebath")
	files := map[string]bool{
		"cache/model_catalog.json": false, "cache/chrome-debug/Default/Cookies": false,
		"profiles/sage/cache/chrome-debug/cache.db": false, "cache/images/x.png": true, "cache/citations/ledger.json": true,
		"profiles/sage/cache/images/y.png": true, "cache/generated/images/x.png": true,
		"profiles/sage/cache/generated/videos/v.mp4": true, "skills/example/cache/state.db": true,
	}
	want := map[string]bool{}
	for rel, keep := range files {
		writeTree(t, root, map[string]string{rel: "x"})
		if keep {
			want[rel] = true
		}
	}
	skipped := map[string]bool{}
	if sel := selectedRels(root, filepath.Join(tmp, "out.zip"), skipped); !reflect.DeepEqual(sel, want) {
		t.Fatalf("selected %v want %v", sel, want)
	}
	if !skipped[filepath.FromSlash("cache/chrome-debug")] || !skipped[filepath.FromSlash("profiles/sage/cache/chrome-debug")] || skipped["cache"] {
		t.Fatalf("skipped %v", skipped)
	}
}

func TestIterBackupFilesSkippedDirsCollectedForSummary(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, ".icebath")
	makeHomeTree(t, root)
	writeTree(t, root, map[string]string{"models/big.gguf": "\x00"})
	skipped := map[string]bool{}
	selectedRels(root, filepath.Join(tmp, "out.zip"), skipped)
	if !skipped["models"] || !skipped["icebath-agent"] {
		t.Fatalf("skipped %v", skipped)
	}
}

func TestIterBackupFilesNeverArchivesTheOutputOrALinkedFile(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, ".icebath")
	writeTree(t, root, map[string]string{"config.yaml": "x", "out.zip": "partial"})
	writeTree(t, tmp, map[string]string{"outside/secret.txt": "s"})
	os.Symlink(filepath.Join(tmp, "outside", "secret.txt"), filepath.Join(root, "linked.txt"))
	os.Symlink(filepath.Join(tmp, "outside"), filepath.Join(root, "linkdir"))
	sel := selectedRels(root, filepath.Join(root, "out.zip"), nil)
	if !reflect.DeepEqual(sel, map[string]bool{"config.yaml": true}) {
		t.Fatalf("selected %v", sel)
	}
}
