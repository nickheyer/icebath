package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Points the process at an unmanaged home with cold config and home caches
func diagnosticsHome(t *testing.T, h string) {
	t.Helper()
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv(EnvManagedDir, filepath.Join(t.TempDir(), "no-managed"))
	t.Setenv("ICEBATH_MANAGED", "")
	os.Unsetenv("ICEBATH_MANAGED")
	t.Setenv("ICEBATH_HOME_MODE", "")
	os.Unsetenv("ICEBATH_HOME_MODE")
	InvalidateManagedCache()
	quietParseFailures(t)
	ResetCache()
	t.Cleanup(ResetCache)
}

func homeIsEnsured(h string) bool {
	homeEnsuredMu.Lock()
	defer homeEnsuredMu.Unlock()
	return homeEnsured[h]
}

func forgetEnsuredHome(paths ...string) {
	homeEnsuredMu.Lock()
	defer homeEnsuredMu.Unlock()
	for _, p := range paths {
		delete(homeEnsured, p)
	}
}

func mkdirMode(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func permOf(t *testing.T, path string) os.FileMode {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return st.Mode().Perm()
}

func isLink(path string) bool {
	st, err := os.Lstat(path)
	return err == nil && st.Mode()&os.ModeSymlink != 0
}

func TestUnavailableDirectoryLinksAreDiagnosedWithoutCreatingTargets(t *testing.T) {
	for _, subdir := range append([]string{"."}, HomeSubdirs...) {
		t.Run(subdir, func(t *testing.T) {
			tmp := t.TempDir()
			h := filepath.Join(tmp, "icebath")
			link := filepath.Join(h, subdir)
			if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(tmp, "unmounted", "external")
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
			diagnosticsHome(t, h)
			forgetEnsuredHome(h)
			issues := ValidateLoadedConfigStructure(t.Context(), h, facadeLookup, noRemovedBackends)
			if len(issues) == 0 {
				t.Fatal("no issue reported")
			}
			var parts []string
			for _, issue := range issues {
				parts = append(parts, issue.Severity, issue.Message, issue.Hint)
			}
			text := strings.Join(parts, " ")
			if !strings.Contains(text, link) || !strings.Contains(text, target) {
				t.Fatalf("issue names neither the link nor its target: %s", text)
			}
			if lower := strings.ToLower(text); !strings.Contains(lower, "mount") || strings.Contains(lower, "setup") {
				t.Fatalf("issue text %s", text)
			}
			if got, err := os.Readlink(link); err != nil || got != target {
				t.Fatalf("link %q %v", got, err)
			}
			if _, err := os.Lstat(filepath.Dir(target)); err == nil {
				t.Fatal("the missing target's parent was created")
			}
			if homeIsEnsured(h) {
				t.Fatal("a failed initialization was memoized")
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				t.Fatal(err)
			}
			mkdirMode(t, target, 0o750)
			if err := EnsureIcebathHome(t.Context(), h); err != nil {
				t.Fatal(err)
			}
			if !isLink(link) || permOf(t, target) != 0o750 {
				t.Fatalf("link kept %v, target mode %o", isLink(link), permOf(t, target))
			}
			if !homeIsEnsured(h) {
				t.Fatal("initialized home not memoized")
			}
		})
	}
}

func TestInitializationPreservesExternalDirectoryModes(t *testing.T) {
	for _, linked := range []string{"plain", "logs", "home"} {
		t.Run(linked, func(t *testing.T) {
			tmp := t.TempDir()
			h := filepath.Join(tmp, "icebath")
			target := filepath.Join(tmp, "shared")
			mkdirMode(t, target, 0o750)
			if linked == "home" {
				if err := os.Symlink(target, h); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Mkdir(h, 0o755); err != nil {
				t.Fatal(err)
			}
			curator := filepath.Join(target, "curator")
			mkdirMode(t, curator, 0o750)
			if linked == "logs" {
				if err := os.Symlink(target, filepath.Join(h, "logs")); err != nil {
					t.Fatal(err)
				}
			}
			diagnosticsHome(t, h)
			forgetEnsuredHome(h)
			if err := EnsureIcebathHome(t.Context(), h); err != nil {
				t.Fatal(err)
			}
			resolved, err := filepath.EvalSymlinks(h)
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("ICEBATH_HOME", resolved)
			if err := EnsureIcebathHome(t.Context(), resolved); err != nil {
				t.Fatal(err)
			}
			for _, name := range HomeSubdirs {
				if !isDir(filepath.Join(h, name)) {
					t.Fatalf("%s missing", name)
				}
			}
			if st, err := os.Stat(filepath.Join(h, "SOUL.md")); err != nil || !st.Mode().IsRegular() {
				t.Fatalf("SOUL.md %v", err)
			}
			if linked != "plain" {
				owned := h
				if linked == "logs" {
					owned = filepath.Join(h, "logs")
				}
				if !isLink(owned) {
					t.Fatalf("%s is no longer a link", owned)
				}
				if permOf(t, target) != 0o750 || permOf(t, curator) != 0o750 {
					t.Fatalf("external modes %o %o", permOf(t, target), permOf(t, curator))
				}
			} else if got := permOf(t, filepath.Join(h, "logs")); got != 0o700 {
				t.Fatalf("logs mode %o", got)
			}
		})
	}
}

func TestSymlinkedParentAboveHomeIsNotAnOperatorHomeLink(t *testing.T) {
	for _, existing := range []bool{false, true} {
		name := "fresh"
		if existing {
			name = "existing"
		}
		t.Run(name, func(t *testing.T) {
			tmp := t.TempDir()
			realRoot := filepath.Join(tmp, "real")
			if err := os.Mkdir(realRoot, 0o755); err != nil {
				t.Fatal(err)
			}
			alias := filepath.Join(tmp, "alias")
			if err := os.Symlink(realRoot, alias); err != nil {
				t.Fatal(err)
			}
			h := filepath.Join(alias, "icebath")
			if existing {
				mkdirMode(t, h, 0o755)
				for _, sub := range HomeSubdirs {
					mkdirMode(t, filepath.Join(h, sub), 0o755)
				}
				if permOf(t, h) != 0o755 {
					t.Fatalf("precondition mode %o", permOf(t, h))
				}
			}
			diagnosticsHome(t, h)
			forgetEnsuredHome(h, filepath.Join(realRoot, "icebath"))
			if err := EnsureIcebathHome(t.Context(), h); err != nil {
				t.Fatal(err)
			}
			if got := permOf(t, h); got != 0o700 {
				t.Fatalf("home mode %o", got)
			}
			for _, sub := range HomeSubdirs {
				if got := permOf(t, filepath.Join(h, sub)); got != 0o700 {
					t.Fatalf("%s should be 0700, got %o", sub, got)
				}
			}
		})
	}
}

func TestAliasedParentStillLeavesAnOperatorHomeLinkAlone(t *testing.T) {
	tmp := t.TempDir()
	realRoot := filepath.Join(tmp, "real")
	if err := os.Mkdir(realRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(tmp, "alias")
	if err := os.Symlink(realRoot, alias); err != nil {
		t.Fatal(err)
	}
	shared := filepath.Join(realRoot, "shared")
	mkdirMode(t, shared, 0o750)
	mkdirMode(t, filepath.Join(shared, "curator"), 0o750)
	h := filepath.Join(alias, "icebath")
	if err := os.Symlink(shared, h); err != nil {
		t.Fatal(err)
	}
	diagnosticsHome(t, h)
	forgetEnsuredHome(h, shared)
	if err := EnsureIcebathHome(t.Context(), h); err != nil {
		t.Fatal(err)
	}
	if got, err := os.Readlink(h); err != nil || got != shared {
		t.Fatalf("home link %q %v", got, err)
	}
	if permOf(t, shared) != 0o750 || permOf(t, filepath.Join(shared, "curator")) != 0o750 {
		t.Fatalf("operator modes %o %o", permOf(t, shared), permOf(t, filepath.Join(shared, "curator")))
	}
}
