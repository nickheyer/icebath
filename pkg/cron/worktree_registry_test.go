package cron

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func mkRepo(t *testing.T, root, name string) string {
	t.Helper()
	repo := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	real, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	return real
}

func TestWorktreeRegistryRecordsDedupesAndDropsVanishedRepos(t *testing.T) {
	h, root := t.TempDir(), t.TempDir()
	if got := worktreeRegisteredRepos(h); len(got) != 0 {
		t.Fatalf("missing registry = %v", got)
	}
	if _, err := os.Stat(worktreeRegistryPath(h)); !os.IsNotExist(err) {
		t.Fatal("a read must never create the registry")
	}
	a, b := mkRepo(t, root, "a"), mkRepo(t, root, "b")
	RecordWorktreeRepo(context.Background(), h, a)
	RecordWorktreeRepo(context.Background(), h, b)
	RecordWorktreeRepo(context.Background(), h, a)
	if got := worktreeRegisteredRepos(h); !reflect.DeepEqual(got, []string{a, b}) {
		t.Fatalf("registered = %v", got)
	}
	if err := os.RemoveAll(a); err != nil {
		t.Fatal(err)
	}
	if got := worktreeRegisteredRepos(h); !reflect.DeepEqual(got, []string{b}) {
		t.Fatalf("vanished repo filtered on read = %v", got)
	}
	c := mkRepo(t, root, "c")
	RecordWorktreeRepo(context.Background(), h, c)
	if got := readWorktreeRegistry(worktreeRegistryPath(h)); !reflect.DeepEqual(got, []string{b, c}) {
		t.Fatalf("vanished repo dropped on write = %v", got)
	}
	RecordWorktreeRepo(context.Background(), h, "")
}

func TestWorktreeRegistryToleratesMalformedFiles(t *testing.T) {
	h, root := t.TempDir(), t.TempDir()
	repo := mkRepo(t, root, "r")
	for _, body := range []string{"not json", `["x"]`, `{"repos": "x"}`, `{"repos": [1, "", null]}`} {
		if err := os.WriteFile(worktreeRegistryPath(h), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := worktreeRegisteredRepos(h); len(got) != 0 {
			t.Fatalf("%s -> %v", body, got)
		}
	}
	if err := os.WriteFile(worktreeRegistryPath(h), []byte(`{"version": 1, "repos": [7, "`+repo+`"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := worktreeRegisteredRepos(h); !reflect.DeepEqual(got, []string{repo}) {
		t.Fatalf("mixed list = %v", got)
	}
}
