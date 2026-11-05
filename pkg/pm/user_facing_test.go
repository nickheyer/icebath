package pm

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Splits a PATH value into its entries
func catPathEntries(path string) []string { return strings.Split(path, string(os.PathListSeparator)) }

// User facing PATH of entries, failing the test on an error
func catUserFacing(t *testing.T, entries ...string) []string {
	t.Helper()
	got, err := UserFacingPath(context.Background(), strings.Join(entries, string(os.PathListSeparator)))
	if err != nil {
		t.Fatal(err)
	}
	return catPathEntries(got)
}

func TestCatUserToolsComeBeforeIcebathAndPythonStopsAtTheStoreAlias(t *testing.T) {
	tmp := t.TempDir()
	var npm, node, python, rg = filepath.Join(tmp, "store", "npm"), filepath.Join(tmp, "store", "node"),
		filepath.Join(tmp, "store", "python"), filepath.Join(tmp, "store", "rg")
	alias := sttMkdir(t, filepath.Join(tmp, "AppData", "Local", "Microsoft", "WindowsApps"))
	catSwap(t, &userFacingStorePathDirs, func(context.Context) ([]string, error) { return []string{npm, node, python, rg}, nil })
	catSwap(t, &userFacingPythonDirs, func(context.Context) ([]string, error) { return []string{python}, nil })
	path := []string{npm, node, python, rg, "/usr/bin", alias, "/home/u/bin"}
	if got := catUserFacing(t, path...); !slices.Equal(got, []string{"/usr/bin", python, alias, "/home/u/bin", npm, node, rg}) {
		t.Fatalf("path = %q", got)
	}
	writeFile(t, filepath.Join(alias, "python3.13.exe"), nil, 0o666)
	if got := catUserFacing(t, path...); !slices.Equal(got, []string{"/usr/bin", alias, "/home/u/bin", python, npm, node, rg}) {
		t.Fatalf("path with a real Store Python = %q", got)
	}
	catSwap(t, &userFacingStorePathDirs, func(context.Context) ([]string, error) { return nil, nil })
	if got := catUserFacing(t, path...); !slices.Equal(got, path) {
		t.Fatalf("path without store dirs = %q", got)
	}
}

func TestCatPathPartsFollowPathlib(t *testing.T) {
	cases := map[string][]string{"/a//b/./c/": {"/", "a", "b", "c"}, "//srv/x": {"//", "srv", "x"}, "///x": {"/", "x"}, "rel/y": {"rel", "y"}}
	for path, want := range cases {
		if got := pathParts(path); os.PathSeparator == '/' && !slices.Equal(got, want) {
			t.Fatalf("parts(%q) = %q", path, got)
		}
	}
}
