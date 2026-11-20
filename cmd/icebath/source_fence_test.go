package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Extensions of interpreted source the Go packages never carry, test data included
var fencedSourceExtensions = map[string]string{
	".py": "Python", ".js": "JavaScript", ".mjs": "JavaScript", ".cjs": "JavaScript", ".ts": "TypeScript", ".tsx": "TypeScript",
}

// Paths excused by hand, empty because only the bundled skill and plugin source trees hold scripts
var fencedSourceAllowList = []string{}

// Directive by which a package bundles a whole directory into the binary as a skill or plugin source tree
var bundledTreeDirective = regexp.MustCompile(`^//go:(?:generate go run \S+/cmd/packtree pack -dir (\S+) |embed all:(\S+)$)`)

func fenceModuleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if st, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil && st.Mode().IsRegular() {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the package directory")
		}
		dir = parent
	}
}

// Module relative directories, slash terminated, that a non test file of pkg or cmd packs or embeds whole
func bundledSourceTrees(t *testing.T, root string) []string {
	t.Helper()
	var trees []string
	for _, top := range []string{"pkg", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, top), func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || filepath.Ext(p) != ".go" || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			raw, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			for _, line := range strings.Split(string(raw), "\n") {
				named := bundledTreeDirective.FindStringSubmatch(strings.TrimSpace(line))
				if named == nil {
					continue
				}
				rel, err := filepath.Rel(root, filepath.Join(filepath.Dir(p), named[1]+named[2]))
				if err != nil {
					return err
				}
				trees = append(trees, filepath.ToSlash(rel)+"/")
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return trees
}

func fencedSourceAllowed(rel string, trees []string) bool {
	for _, tree := range trees {
		if strings.HasPrefix(rel, tree) {
			return true
		}
	}
	for _, entry := range fencedSourceAllowList {
		if rel == entry || (strings.HasSuffix(entry, "/") && strings.HasPrefix(rel, entry)) {
			return true
		}
	}
	return false
}

func TestNoInterpretedSourceUnderPkgCmdOrThirdParty(t *testing.T) {
	root := fenceModuleRoot(t)
	trees := bundledSourceTrees(t, root)
	for _, top := range []string{"pkg", "cmd", "third_party"} {
		start := filepath.Join(root, top)
		if _, err := os.Stat(start); os.IsNotExist(err) {
			continue
		}
		err := filepath.WalkDir(start, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			language, fenced := fencedSourceExtensions[strings.ToLower(filepath.Ext(d.Name()))]
			if d.IsDir() || !fenced {
				return nil
			}
			rel, err := filepath.Rel(root, p)
			if err != nil {
				return err
			}
			if rel = filepath.ToSlash(rel); !fencedSourceAllowed(rel, trees) {
				t.Errorf("%s is %s source outside the bundled skill and plugin source trees", rel, language)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestFenceAllowListIsEmpty(t *testing.T) {
	if len(fencedSourceAllowList) != 0 {
		t.Fatalf("the fence excuses %v by hand, scripts belong only in the bundled skill and plugin source trees", fencedSourceAllowList)
	}
}
