package home

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"iter"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// Restores every ACL seam after the test and starts it with an empty grant memo
func aclSeams(t *testing.T) {
	t.Helper()
	elevated, lstat, native, whoami, run, walk, access := processIsElevated, aclLstat, grantNative, whoamiSID, runACLCommand, iterTree, grantUserAccessFn
	grantedMu.Lock()
	saved := grantedDirs
	grantedDirs = map[aclIdentity]bool{}
	grantedMu.Unlock()
	t.Cleanup(func() {
		processIsElevated, aclLstat, grantNative, whoamiSID, runACLCommand, iterTree, grantUserAccessFn = elevated, lstat, native, whoami, run, walk, access
		grantedMu.Lock()
		grantedDirs = saved
		grantedMu.Unlock()
	})
	t.Setenv("ICEBATH_HOME", t.TempDir())
}

func resetGranted() {
	grantedMu.Lock()
	grantedDirs = map[aclIdentity]bool{}
	grantedMu.Unlock()
}

// Default slog output captured for the test
func captureWarnings(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func recordAccess(granted *[]string) {
	grantUserAccessFn = func(_ context.Context, p string, _, _ bool) bool { *granted = append(*granted, p); return true }
}

func TestOffWindowsItIsPlainMkdir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix")
	}
	aclSeams(t)
	tmp := t.TempDir()
	var calls []string
	grantUserAccessFn = func(_ context.Context, p string, _, _ bool) bool { calls = append(calls, p); return false }
	target, err := MakeUserDir(context.Background(), filepath.Join(tmp, "a", "b"), 0o700, true, false)
	st, serr := os.Stat(target)
	if err != nil || serr != nil || !st.IsDir() || len(calls) != 0 || st.Mode().Perm() != 0o700 {
		t.Fatalf("%v %v %v %v", err, serr, calls, st)
	}
	if _, err := MakeUserDir(context.Background(), target, 0o777, false, false); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("existing: %v", err)
	}
	if _, err := MakeUserDir(context.Background(), filepath.Join(tmp, "x", "y"), 0o777, false, false); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing parent: %v", err)
	}
}

func TestElevatedGrantsEveryCreatedDirAndAnExistingLeafOnce(t *testing.T) {
	aclSeams(t)
	tmp := t.TempDir()
	var granted []string
	processIsElevated = func() bool { return true }
	recordAccess(&granted)
	leaf, err := MakeUserDir(context.Background(), filepath.Join(tmp, "state", "gateway-locks"), 0o777, true, true)
	if err != nil || !slices.Equal(granted, []string{filepath.Join(tmp, "state"), leaf}) {
		t.Fatalf("%v %v", err, granted)
	}
	if _, err := MakeUserDir(context.Background(), leaf, 0o777, false, true); err != nil || len(granted) != 2 {
		t.Fatalf("memo %v %v", err, granted)
	}
	existing := filepath.Join(tmp, "left-by-an-earlier-elevated-run")
	if err := os.Mkdir(existing, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := MakeUserDir(context.Background(), existing, 0o777, false, true); err != nil || granted[len(granted)-1] != existing {
		t.Fatalf("repair %v %v", err, granted)
	}
}

func TestElevatedExistingLeafWithoutExistOKIsRepairedThenRaises(t *testing.T) {
	aclSeams(t)
	var granted []string
	processIsElevated = func() bool { return true }
	recordAccess(&granted)
	left := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(left, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := MakeUserDir(context.Background(), left, 0o777, false, false); !errors.Is(err, fs.ErrExist) || !slices.Equal(granted, []string{left}) {
		t.Fatalf("%v %v", err, granted)
	}
	if _, err := MakeUserDir(context.Background(), left, 0o777, true, true); err != nil || !slices.Equal(granted, []string{left}) {
		t.Fatalf("memo %v %v", err, granted)
	}
}

func TestAFailedGrantNeverFailsTheMkdirAndIsRetried(t *testing.T) {
	aclSeams(t)
	processIsElevated = func() bool { return true }
	var attempts []string
	grantNative = func(path string, _ bool, _ string) (bool, error) {
		attempts = append(attempts, path)
		return false, errors.New("access denied")
	}
	target, err := MakeUserDir(context.Background(), filepath.Join(t.TempDir(), "d"), 0o777, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := MakeUserDir(context.Background(), target, 0o777, false, true); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(target); err != nil || !st.IsDir() || !slices.Equal(attempts, []string{target, target}) {
		t.Fatalf("%v %v", err, attempts)
	}
}

func TestRemedyNamesThePlatformRepair(t *testing.T) {
	if got := unusableDirRemedy(`C:\x`, true); got != `from an elevated prompt run: icacls "C:\x" /reset /t` {
		t.Fatal(got)
	}
	if !strings.Contains(unusableDirRemedy("/x", false), "chown") {
		t.Fatal("posix remedy")
	}
}

// Fake elevated host whose native grant records the real path it would have touched
func elevatedWindows(t *testing.T) (string, string, *[]string) {
	t.Helper()
	aclSeams(t)
	tmp := t.TempDir()
	h := filepath.Join(tmp, "home")
	outside := filepath.Join(tmp, "Program Files", "X")
	for _, d := range []string{h, outside} {
		if err := os.MkdirAll(d, 0o777); err != nil {
			t.Fatal(err)
		}
	}
	writeACLFile(t, filepath.Join(outside, "app.exe"), "x")
	t.Setenv("ICEBATH_HOME", h)
	processIsElevated = func() bool { return true }
	touched := &[]string{}
	grantNative = func(path string, _ bool, _ string) (bool, error) {
		real, _ := filepath.EvalSymlinks(path)
		*touched = append(*touched, real)
		return false, nil
	}
	return h, outside, touched
}

func writeACLFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0o666); err != nil {
		t.Fatal(err)
	}
}

func symlinkOrFail(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(link) })
}

func escapes(touched []string, outside string) []string {
	var out []string
	for _, p := range touched {
		if p == outside || strings.HasPrefix(p, outside+string(filepath.Separator)) {
			out = append(out, p)
		}
	}
	return out
}

func TestMakeUserDirRefusesAnExistingLeafThatIsALink(t *testing.T) {
	h, outside, touched := elevatedWindows(t)
	logs := captureWarnings(t)
	if err := os.Mkdir(filepath.Join(h, "cache"), 0o777); err != nil {
		t.Fatal(err)
	}
	planted := filepath.Join(h, "cache", "images")
	symlinkOrFail(t, outside, planted)
	for range 2 {
		if _, err := MakeUserDir(context.Background(), planted, 0o777, true, true); err != nil {
			t.Fatal(err)
		}
	}
	if len(*touched) != 0 || !strings.Contains(logs.String(), "never follows a junction") {
		t.Fatalf("%v %s", *touched, logs)
	}
}

func TestMakeUserDirRefusesALinkAmongItsAncestorsInTheHome(t *testing.T) {
	h, outside, touched := elevatedWindows(t)
	symlinkOrFail(t, outside, filepath.Join(h, "cache"))
	made, err := MakeUserDir(context.Background(), filepath.Join(h, "cache", "images", "x"), 0o777, true, true)
	if st, serr := os.Stat(made); err != nil || serr != nil || !st.IsDir() {
		t.Fatalf("%v %v", err, serr)
	}
	if len(*touched) != 0 {
		t.Fatalf("a grant went through a planted link %v", *touched)
	}
}

func TestGrantUserAccessRefusesALinkDirectly(t *testing.T) {
	h, outside, touched := elevatedWindows(t)
	link := filepath.Join(h, "logs")
	symlinkOrFail(t, outside, link)
	if GrantUserAccess(context.Background(), link, ProcessIsElevated(), false) || GrantUserAccess(context.Background(), filepath.Join(link, "deeper"), ProcessIsElevated(), false) {
		t.Fatal("granted through a link")
	}
	if len(*touched) != 0 {
		t.Fatal(*touched)
	}
}

func TestMakeUserDirStillGrantsRealDirsUnderALinkedHome(t *testing.T) {
	aclSeams(t)
	tmp := t.TempDir()
	real := filepath.Join(tmp, "real-home")
	if err := os.Mkdir(real, 0o777); err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(tmp, "linked-home")
	symlinkOrFail(t, real, linked)
	t.Setenv("ICEBATH_HOME", linked)
	processIsElevated = func() bool { return true }
	var calls [][2]string
	grantNative = func(path string, _ bool, expected string) (bool, error) {
		calls = append(calls, [2]string{path, expected})
		return false, nil
	}
	if _, err := MakeUserDir(context.Background(), filepath.Join(linked, "state"), 0o777, false, true); err != nil {
		t.Fatal(err)
	}
	want := [][2]string{{filepath.Join(linked, "state"), filepath.Join(real, "state")}}
	if !slices.Equal(calls, want) {
		t.Fatalf("%v", calls)
	}
}

func walkTree(root string) (map[string]bool, []string) {
	var skipped []string
	seen := map[string]bool{}
	for p, d := range IterTreeWithoutReparsePoints(root, &skipped) {
		rel, _ := filepath.Rel(root, p)
		seen[filepath.ToSlash(rel)] = d
	}
	return seen, skipped
}

func baseNames(paths []string) []string {
	out := make([]string, len(paths))
	for i, p := range paths {
		out[i] = filepath.Base(p)
	}
	slices.Sort(out)
	return out
}

func TestTreeWalkSkipsSymlinksAndNeverEntersThem(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "home")
	outside := filepath.Join(tmp, "outside")
	for _, d := range []string{filepath.Join(root, "a", "b"), filepath.Join(outside, "deep")} {
		if err := os.MkdirAll(d, 0o777); err != nil {
			t.Fatal(err)
		}
	}
	writeACLFile(t, filepath.Join(root, "a", "f.txt"), "x")
	symlinkOrFail(t, outside, filepath.Join(root, "a", "jn"))
	symlinkOrFail(t, filepath.Join(outside, "deep"), filepath.Join(root, "flink"))
	seen, skipped := walkTree(root)
	want := map[string]bool{".": true, "a": true, "a/b": true, "a/f.txt": false}
	if len(seen) != len(want) || !slices.Equal(baseNames(skipped), []string{"flink", "jn"}) {
		t.Fatalf("%v %v", seen, skipped)
	}
	for k, v := range want {
		if d, ok := seen[k]; !ok || d != v {
			t.Fatalf("%v", seen)
		}
	}
}

func TestTreeWalkTreatsAReparseAttributeAsALink(t *testing.T) {
	aclSeams(t)
	root := filepath.Join(t.TempDir(), "home")
	if err := os.MkdirAll(filepath.Join(root, "junction"), 0o777); err != nil {
		t.Fatal(err)
	}
	real := aclLstat
	aclLstat = func(p string) (aclStat, error) {
		st, err := real(p)
		if strings.HasSuffix(p, "junction") {
			st.attrs = FileAttributeReparsePoint | FileAttributeDirectory
		}
		return st, err
	}
	var skipped, seen []string
	for p := range IterTreeWithoutReparsePoints(root, &skipped) {
		seen = append(seen, p)
	}
	if !slices.Equal(seen, []string{root}) || !slices.Equal(skipped, []string{filepath.Join(root, "junction")}) {
		t.Fatalf("%v %v", seen, skipped)
	}
}

func TestTreeGrantNeverTouchesATargetOutsideTheHome(t *testing.T) {
	h, outside, touched := elevatedWindows(t)
	if err := os.Mkdir(filepath.Join(h, "sessions"), 0o777); err != nil {
		t.Fatal(err)
	}
	writeACLFile(t, filepath.Join(h, "sessions", "s.json"), "{}")
	symlinkOrFail(t, outside, filepath.Join(h, "sessions", "jn"))
	if err := os.Link(filepath.Join(outside, "app.exe"), filepath.Join(h, "hardlink.exe")); err != nil {
		t.Fatal(err)
	}
	granted, skipped, failed, err := GrantTreeUserAccess(h, false, ProcessIsElevated())
	if err != nil || len(escapes(*touched, outside)) != 0 || len(failed) != 0 {
		t.Fatalf("%v %v %v", err, *touched, failed)
	}
	if !slices.Equal(baseNames(skipped), []string{"hardlink.exe", "jn"}) || granted != 3 {
		t.Fatalf("%v %d", skipped, granted)
	}
}

func TestTreeGrantIsANoopUnelevated(t *testing.T) {
	aclSeams(t)
	tmp := t.TempDir()
	grantNative = func(string, bool, string) (bool, error) { t.Fatal("granted"); return false, nil }
	for _, elevated := range []bool{false, ProcessIsElevated()} {
		if g, s, f, err := GrantTreeUserAccess(tmp, false, elevated); g != 0 || len(s) != 0 || len(f) != 0 || err != nil {
			t.Fatalf("%d %v %v %v", g, s, f, err)
		}
	}
}

func TestIcaclsFallbackRefusesAnyLinkInTheSubtree(t *testing.T) {
	for _, shape := range []string{"symlink-only", "symlink-before-a-file", "hardlink-only", "nested-hardlink"} {
		t.Run(shape, func(t *testing.T) {
			aclSeams(t)
			tmp := t.TempDir()
			outside := filepath.Join(tmp, "outside")
			root := filepath.Join(tmp, "home")
			for _, d := range []string{outside, root} {
				if err := os.Mkdir(d, 0o777); err != nil {
					t.Fatal(err)
				}
			}
			writeACLFile(t, filepath.Join(outside, "file"), "sentinel")
			switch shape {
			case "symlink-only", "symlink-before-a-file":
				symlinkOrFail(t, outside, filepath.Join(root, "a-link"))
				if shape == "symlink-before-a-file" {
					writeACLFile(t, filepath.Join(root, "z-file"), "x")
				}
			case "hardlink-only":
				if err := os.Link(filepath.Join(outside, "file"), filepath.Join(root, "alias")); err != nil {
					t.Fatal(err)
				}
			default:
				if err := os.Mkdir(filepath.Join(root, "d"), 0o777); err != nil {
					t.Fatal(err)
				}
				if err := os.Link(filepath.Join(outside, "file"), filepath.Join(root, "d", "alias")); err != nil {
					t.Fatal(err)
				}
			}
			whoamiSID = func() (string, error) { return "S-1-5-21-1-2-3-1001", nil }
			var calls [][]string
			runACLCommand = func(argv []string) error { calls = append(calls, argv); return nil }
			var refused *ReparsePointRefused
			if err := grantWithIcacls(root, true, root, linksUnwalked); !errors.As(err, &refused) || len(calls) != 0 {
				t.Fatalf("%v %v", err, calls)
			}
		})
	}
}

func TestIcaclsFallbackStillGrantsAPlainSubtree(t *testing.T) {
	aclSeams(t)
	root := filepath.Join(t.TempDir(), "home")
	if err := os.MkdirAll(filepath.Join(root, "d"), 0o777); err != nil {
		t.Fatal(err)
	}
	writeACLFile(t, filepath.Join(root, "d", "f"), "x")
	whoamiSID = func() (string, error) { return "S-1-5-21-1-2-3-1001", nil }
	var calls [][]string
	runACLCommand = func(argv []string) error { calls = append(calls, argv); return nil }
	if err := grantWithIcacls(root, true, root, linksUnwalked); err != nil || len(calls) != 1 || calls[0][0] != "icacls" || !slices.Contains(calls[0], "/L") {
		t.Fatalf("%v %v", err, calls)
	}
}

func TestARecreatedDirIsGrantedAgain(t *testing.T) {
	aclSeams(t)
	var granted []string
	processIsElevated = func() bool { return true }
	recordAccess(&granted)
	target := filepath.Join(t.TempDir(), "native-inputs", strings.Repeat("0", 64))
	if _, err := MakeUserDir(context.Background(), target, 0o700, true, true); err != nil {
		t.Fatal(err)
	}
	if _, err := MakeUserDir(context.Background(), target, 0o700, false, true); err != nil {
		t.Fatal(err)
	}
	if n := countOf(granted, target); n != 1 {
		t.Fatalf("existing object granted %d times", n)
	}
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if _, err := MakeUserDir(context.Background(), target, 0o700, false, true); err != nil {
		t.Fatal(err)
	}
	if n := countOf(granted, target); n != 2 {
		t.Fatal("the recreated directory never got the grant")
	}
}

func countOf(items []string, want string) int {
	n := 0
	for _, s := range items {
		if s == want {
			n++
		}
	}
	return n
}

func TestRepairingAnExistingDirAlsoGrantsWhatIsAlreadyInsideIt(t *testing.T) {
	aclSeams(t)
	tmp := t.TempDir()
	h := filepath.Join(tmp, "home")
	locks := filepath.Join(h, "gateway-locks")
	outside := filepath.Join(tmp, "outside")
	for _, d := range []string{filepath.Join(locks, "old"), outside} {
		if err := os.MkdirAll(d, 0o777); err != nil {
			t.Fatal(err)
		}
	}
	writeACLFile(t, filepath.Join(locks, "telegram-bot-token-x.lock"), "{}")
	symlinkOrFail(t, outside, filepath.Join(locks, "jn"))
	t.Setenv("ICEBATH_HOME", h)
	var writes [][2]string
	grantNative = func(path string, _ bool, expected string) (bool, error) {
		writes = append(writes, [2]string{path, expected})
		return true, nil
	}
	if _, err := makeUserDir(context.Background(), locks, 0o777, true, true, true); err != nil {
		t.Fatal(err)
	}
	realHome, _ := filepath.EvalSymlinks(h)
	var touched []string
	for _, w := range writes {
		rel, _ := filepath.Rel(h, w[0])
		touched = append(touched, filepath.ToSlash(rel))
		if w[1] != filepath.Join(realHome, rel) {
			t.Fatalf("expected %v", w)
		}
	}
	slices.Sort(touched)
	if !slices.Equal(touched, []string{"gateway-locks", "gateway-locks/old", "gateway-locks/telegram-bot-token-x.lock"}) {
		t.Fatalf("%v", touched)
	}
	resetGranted()
	var again []string
	grantNative = func(path string, _ bool, _ string) (bool, error) { again = append(again, path); return false, nil }
	if _, err := makeUserDir(context.Background(), locks, 0o777, false, true, true); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(again, []string{locks}) {
		t.Fatalf("%v", again)
	}
}

func TestIcaclsTreeFallbackWalksTheTreeOnce(t *testing.T) {
	aclSeams(t)
	tmp := t.TempDir()
	h := filepath.Join(tmp, "home")
	for _, d := range []string{"a/b/c", "dirty/sub"} {
		if err := os.MkdirAll(filepath.Join(h, d), 0o777); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{"a/f1", "a/b/f2", "a/b/c/f3", "dirty/f4", "dirty/sub/f5"} {
		writeACLFile(t, filepath.Join(h, f), "x")
	}
	outside := filepath.Join(tmp, "outside")
	if err := os.Mkdir(outside, 0o777); err != nil {
		t.Fatal(err)
	}
	symlinkOrFail(t, outside, filepath.Join(h, "dirty", "jn"))
	grantNative = func(string, bool, string) (bool, error) { return false, errNoNativeACL }
	whoamiSID = func() (string, error) { return "S-1-5-21-1-2-3-1001", nil }
	var calls []string
	runACLCommand = func(argv []string) error { calls = append(calls, argv[1]); return nil }
	var walks []string
	walk := iterTree
	iterTree = func(root string, skipped *[]string) iter.Seq2[string, bool] {
		walks = append(walks, root)
		return walk(root, skipped)
	}
	granted, skipped, failed, err := GrantTreeUserAccess(h, false, true)
	if err != nil || !slices.Equal(walks, []string{h}) {
		t.Fatalf("one walk, not one per directory %v %v", err, walks)
	}
	rels := func(base string, paths []string) []string {
		out := make([]string, len(paths))
		for i, p := range paths {
			r, _ := filepath.Rel(base, p)
			out[i] = filepath.ToSlash(r)
		}
		slices.Sort(out)
		return out
	}
	clean := []string{"a", "a/b", "a/b/c", "a/b/c/f3", "a/b/f2", "a/f1", "dirty/f4", "dirty/sub", "dirty/sub/f5"}
	if !slices.Equal(rels(h, calls), clean) || !slices.Equal(rels(tmp, skipped), []string{"home", "home/dirty", "home/dirty/jn"}) {
		t.Fatalf("%v %v", rels(h, calls), rels(tmp, skipped))
	}
	if len(failed) != 0 || granted != len(clean) {
		t.Fatalf("%v %d", failed, granted)
	}
}

func TestWhoamiSIDFailureIsNeverCached(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix whoami rejects the Windows switches")
	}
	aclSeams(t)
	whoamiMu.Lock()
	whoamiCached = ""
	whoamiMu.Unlock()
	if sid, err := cachedWhoamiSID(); err == nil || sid != "" || whoamiCached != "" {
		t.Fatalf("%q %v", sid, err)
	}
}

func TestPathMkdirCreatesParentsAndHonoursExistOK(t *testing.T) {
	root := t.TempDir()
	deep := filepath.Join(root, "a", "b")
	if err := PathMkdir(deep, 0o700, false, false); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing parent without parents: %v", err)
	}
	if err := PathMkdir(deep, 0o700, true, false); err != nil {
		t.Fatal(err)
	}
	if err := PathMkdir(deep, 0o700, true, false); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("existing without exist_ok: %v", err)
	}
	if err := PathMkdir(deep, 0o700, true, true); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "f")
	os.WriteFile(file, nil, 0o600)
	if err := PathMkdir(file, 0o700, true, true); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("a file in the way: %v", err)
	}
	if runtime.GOOS != "windows" {
		if st, _ := os.Stat(filepath.Join(root, "a")); st.Mode().Perm()&0o700 != 0o700 {
			t.Fatalf("parent mode %o", st.Mode().Perm())
		}
	}
}

func TestHomeMkdirCarriesTheAclModuleInTheClosure(t *testing.T) {
	aclSeams(t)
	h := filepath.Join(t.TempDir(), "home")
	if err := os.Mkdir(h, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	got, err := Mkdir(context.Background(), filepath.Join(h, "logs"))
	if err != nil || got != filepath.Join(h, "logs") {
		t.Fatalf("Mkdir = %q, %v", got, err)
	}
	if st, err := os.Stat(filepath.Join(h, "logs")); err != nil || !st.IsDir() {
		t.Fatalf("logs dir missing: %v", err)
	}
}

func TestHomeMkdirNeverSilentlySkipsTheElevatedGrant(t *testing.T) {
	aclSeams(t)
	h := filepath.Join(t.TempDir(), "home")
	if err := os.Mkdir(h, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	processIsElevated = func() bool { return true }
	var granted []string
	recordAccess(&granted)
	logs := filepath.Join(h, "logs")
	if _, err := Mkdir(context.Background(), logs); err != nil {
		t.Fatal(err)
	}
	if len(granted) != 1 || granted[0] != logs {
		t.Fatalf("elevated home mkdir granted %v, want the created %s", granted, logs)
	}
}

func TestIcaclsFallbackRefusesAGrantWithNoTrustedRoot(t *testing.T) {
	aclSeams(t)
	whoamiSID = func() (string, error) { return "S-1-5-21-1-2-3-1001", nil }
	var calls [][]string
	runACLCommand = func(argv []string) error { calls = append(calls, argv); return nil }
	var refused *ReparsePointRefused
	if err := grantWithIcacls(t.TempDir(), true, "", linksUnwalked); !errors.As(err, &refused) {
		t.Fatalf("grantWithIcacls without a trusted root = %v", err)
	}
	if len(calls) != 0 {
		t.Fatalf("icacls ran: %v", calls)
	}
}

func TestTaskContextOverrideDecidesTheHomeGrantsAreCheckedBelow(t *testing.T) {
	aclSeams(t)
	tmp := t.TempDir()
	root := filepath.Join(tmp, "root")
	real := filepath.Join(tmp, "real-profile")
	for _, d := range []string{filepath.Join(root, "profiles"), real} {
		if err := os.MkdirAll(d, 0o777); err != nil {
			t.Fatal(err)
		}
	}
	linked := filepath.Join(root, "profiles", "coder")
	symlinkOrFail(t, real, linked)
	t.Setenv("ICEBATH_HOME", root)
	processIsElevated = func() bool { return true }
	var calls [][2]string
	grantNative = func(path string, _ bool, expected string) (bool, error) {
		calls = append(calls, [2]string{path, expected})
		return false, nil
	}
	logs := captureWarnings(t)
	state := filepath.Join(linked, "state")
	scoped := WithOverride(context.Background(), linked)
	if got := componentsBelowHome(scoped, state); !slices.Equal(got, []string{state}) {
		t.Fatalf("scoped components %v", got)
	}
	process := []string{filepath.Join(root, "profiles"), linked, state}
	if got := componentsBelowHome(context.Background(), state); !slices.Equal(got, process) {
		t.Fatalf("process components %v", got)
	}
	if exp, err := expectedFinal(scoped, state); err != nil || exp != filepath.Join(real, "state") {
		t.Fatalf("scoped expected %q %v", exp, err)
	}
	if _, err := MakeUserDir(context.Background(), state, 0o777, false, true); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 0 || !strings.Contains(logs.String(), "never follows a junction") {
		t.Fatalf("the process home must refuse the linked profile, calls %v logs %s", calls, logs)
	}
	if _, err := MakeUserDir(scoped, state, 0o777, false, true); err != nil {
		t.Fatal(err)
	}
	if want := [][2]string{{state, filepath.Join(real, "state")}}; !slices.Equal(calls, want) {
		t.Fatalf("the scoped home is the caller's own link and must be granted below, calls %v", calls)
	}
	other := filepath.Join(tmp, "other-home")
	deep := filepath.Join(other, "logs", "x")
	if got := componentsBelowHome(WithOverride(context.Background(), other), deep); !slices.Equal(got, []string{filepath.Join(other, "logs"), deep}) {
		t.Fatalf("components below an override outside the process home %v", got)
	}
	if got := componentsBelowHome(context.Background(), deep); got != nil {
		t.Fatalf("a path off the process home has no components, got %v", got)
	}
	if exp, err := expectedFinal(context.Background(), deep); err != nil || exp != "" {
		t.Fatalf("expected final off the process home %q %v", exp, err)
	}
}
