package pyfs

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

func TestWalkIsTopDownPrunableAndNeverFollowsDirectoryLinks(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"a/b", "a/skip/deep", "c"} {
		os.MkdirAll(filepath.Join(root, d), 0o755)
	}
	for _, f := range []string{"top.txt", "a/one.txt", "a/b/two.txt", "a/skip/deep/never.txt", "c/three.txt"} {
		os.WriteFile(filepath.Join(root, f), []byte("x"), 0o644)
	}
	os.Symlink(filepath.Join(root, "c"), filepath.Join(root, "a", "linkdir"))
	os.Symlink(filepath.Join(root, "top.txt"), filepath.Join(root, "a", "linkfile"))
	seen := map[string][2][]string{}
	var order []string
	Walk(root, false, func(dirpath string, dirnames, filenames []string) []string {
		rel, _ := filepath.Rel(root, dirpath)
		sort.Strings(dirnames)
		sort.Strings(filenames)
		seen[rel] = [2][]string{dirnames, filenames}
		order = append(order, rel)
		kept := []string{}
		for _, d := range dirnames {
			if d != "skip" {
				kept = append(kept, d)
			}
		}
		return kept
	})
	if got := seen["a"]; !reflect.DeepEqual(got, [2][]string{{"b", "linkdir", "skip"}, {"linkfile", "one.txt"}}) {
		t.Fatalf("a entries %v", got)
	}
	if _, ok := seen[filepath.Join("a", "skip")]; ok {
		t.Fatal("descended a pruned directory")
	}
	if _, ok := seen[filepath.Join("a", "linkdir")]; ok {
		t.Fatal("followed a directory symlink")
	}
	if order[0] != "." || len(order) != 4 {
		t.Fatalf("order %v", order)
	}
	Walk(filepath.Join(root, "missing"), false, func(string, []string, []string) []string {
		t.Fatal("visited an unreadable top")
		return nil
	})
}
