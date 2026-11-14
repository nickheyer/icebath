package config

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Config file with the given text in a fresh directory standing for the home
func backupFixture(t *testing.T, text string) (string, string) {
	t.Helper()
	t.Setenv(BackendEnv, "")
	os.Unsetenv(BackendEnv)
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfg, []byte(text), 0o666); err != nil {
		t.Fatal(err)
	}
	return dir, cfg
}

// Replaces the backup name stamp with ten consecutive seconds of one day
func backupStamps(t *testing.T) {
	t.Helper()
	prev, i := backupTimestamp, 0
	backupTimestamp = func() string { i++; return fmt.Sprintf("2026010100000%d", i-1) }
	t.Cleanup(func() { backupTimestamp = prev })
}

func backupText(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestRepeatBackupsDedupeAndRotate(t *testing.T) {
	dir, cfg := backupFixture(t, "model: a\n")
	backupStamps(t)
	first := mustBackup(t, cfg, "pre-setup", 2)
	if first == "" || filepath.Dir(first) != filepath.Join(dir, "backups", "config") {
		t.Fatalf("first backup %q", first)
	}
	if again := mustBackup(t, cfg, "pre-setup", 2); again != "" {
		t.Fatalf("identical bytes were backed up again as %q", again)
	}
	for i := range 3 {
		if err := os.WriteFile(cfg, []byte(fmt.Sprintf("model: %d\n", i)), 0o666); err != nil {
			t.Fatal(err)
		}
		mustBackup(t, cfg, "pre-setup", 2)
	}
	kept := ListConfigBackups(cfg, "pre-setup")
	if len(kept) != 2 || backupText(t, kept[0]) != "model: 2\n" {
		t.Fatalf("kept %v", kept)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() {
			files = append(files, e.Name())
		}
	}
	if !reflect.DeepEqual(files, []string{"config.yaml"}) {
		t.Fatalf("files beside the config %v", files)
	}
}

func TestSameSizeEditWithinOneMtimeTickIsStillBackedUp(t *testing.T) {
	_, cfg := backupFixture(t, "model: a\n")
	backupStamps(t)
	if mustBackup(t, cfg, "pre-setup", DefaultBackupKeep) == "" {
		t.Fatal("the first backup was skipped")
	}
	if again := mustBackup(t, cfg, "pre-setup", DefaultBackupKeep); again != "" {
		t.Fatalf("identical bytes were backed up again as %q", again)
	}
	before, err := os.Stat(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, []byte("model: b\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(cfg, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}
	second := mustBackup(t, cfg, "pre-setup", DefaultBackupKeep)
	if second == "" || backupText(t, second) != "model: b\n" {
		t.Fatalf("second backup %q", second)
	}
}

func TestLegacySiblingsMoveButUserNamedCopiesStay(t *testing.T) {
	dir, cfg := backupFixture(t, "model: a\n")
	for _, name := range []string{"config.yaml.bak.1778718391", "config.yaml.corrupt.20260729-093706.bak", "config.yaml.bak-pre-migrate-xai-20260515-120000"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("old"), 0o666); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml.bak-my-note"), []byte("mine"), 0o666); err != nil {
		t.Fatal(err)
	}
	mustBackup(t, cfg, "corrupt", DefaultBackupKeep)
	root := filepath.Join(dir, "backups", "config")
	for _, name := range []string{"config.yaml.bak.1778718391", "config.yaml.corrupt.20260729-093706.bak"} {
		if _, err := os.Stat(filepath.Join(root, name)); err != nil {
			t.Fatalf("%s was not moved: %v", name, err)
		}
	}
	if got := backupText(t, filepath.Join(dir, "config.yaml.bak-my-note")); got != "mine" {
		t.Fatalf("the user's copy now reads %q", got)
	}
	for _, pattern := range []string{"config.yaml.bak.*", "config.yaml.corrupt.*"} {
		if left, _ := filepath.Glob(filepath.Join(dir, pattern)); len(left) != 0 {
			t.Fatalf("%s still beside the config: %v", pattern, left)
		}
	}
}

func TestNewestGoodBackupIsReadByTheConfigLoader(t *testing.T) {
	dir, cfg := backupFixture(t, "unused: 1\n")
	root := filepath.Join(dir, "backups", "config")
	if err := os.MkdirAll(root, 0o777); err != nil {
		t.Fatal(err)
	}
	good := filepath.Join(root, "config.yaml.good.20260101-000000")
	if err := os.WriteFile(good, []byte("flag: on\nanswer: y\nname: plain\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	doc, err := LoadNewestGoodBackup(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]any{"flag": true, "answer": true, "name": "plain"}; !reflect.DeepEqual(doc, want) {
		t.Fatalf("YAML 1.1 booleans must load as booleans, got %#v", doc)
	}
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	if err := os.WriteFile(good, []byte("a: 1\na: 2\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	if doc, err := LoadNewestGoodBackup(context.Background(), cfg); err != nil || doc != nil {
		t.Fatalf("a backup with a duplicate key is unreadable, got %#v %v", doc, err)
	}
	if !strings.Contains(logs.String(), "Last-known-good backup "+good+" is unreadable: ") {
		t.Fatalf("warning text %q", logs.String())
	}
	if err := os.WriteFile(good, []byte("- a\n- b\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	if doc, err := LoadNewestGoodBackup(context.Background(), cfg); err != nil || doc != nil {
		t.Fatalf("a backup that is no mapping answers nothing, got %#v %v", doc, err)
	}
}

func TestBackupRotationFollowsThePythonSliceAndLogsTheUpstreamWarning(t *testing.T) {
	dir, cfg := backupFixture(t, "model: 0\n")
	backupStamps(t)
	for i := range 3 {
		if err := os.WriteFile(cfg, []byte(fmt.Sprintf("model: %d\n", i)), 0o666); err != nil {
			t.Fatal(err)
		}
		if mustBackup(t, cfg, "r", 5) == "" {
			t.Fatalf("backup %d was skipped", i)
		}
	}
	if err := os.WriteFile(cfg, []byte("model: last\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	if mustBackup(t, cfg, "r", -1) == "" {
		t.Fatal("the backup under a negative keep was skipped")
	}
	if kept := ListConfigBackups(cfg, "r"); len(kept) != 3 || backupText(t, kept[0]) != "model: last\n" || backupText(t, kept[2]) != "model: 1\n" {
		t.Fatalf("a keep of minus one drops only the oldest, kept %v", kept)
	}
	if st, err := os.Stat(filepath.Join(dir, "backups", "config")); err != nil || !st.IsDir() {
		t.Fatalf("backups dir %v", err)
	}
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	blocked := t.TempDir()
	other := filepath.Join(blocked, "config.yaml")
	if err := os.WriteFile(other, []byte("model: a\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blocked, "backups"), []byte("a file where the directory belongs"), 0o666); err != nil {
		t.Fatal(err)
	}
	if got := mustBackup(t, other, "pre-setup", DefaultBackupKeep); got != "" {
		t.Fatalf("a backup landed at %q", got)
	}
	if !strings.Contains(logs.String(), "Could not back up "+other+" (pre-setup): ") {
		t.Fatalf("warning text %q", logs.String())
	}
}
