package pm

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

// Points the default root and the store at tmp, the store sitting in a payload beside its uv-cache
func catSeedHome(t *testing.T, root, store string) {
	t.Helper()
	t.Setenv(home.EnvHome, root)
	t.Setenv(EnvRuntimeDir, store)
	config.ResetCache()
}

// Runs the cache seed, failing the test on an error
func catUvCacheDir(t *testing.T) string {
	t.Helper()
	machine, err := UvCacheDir(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return machine
}

func TestCatUvCacheDirSeedsFromPayload(t *testing.T) {
	tmp := t.TempDir()
	root := sttMkdir(t, filepath.Join(tmp, "home"))
	payload := filepath.Join(tmp, "payload")
	seeded := filepath.Join(payload, "uv-cache", "wheels-v5", "some.pkg")
	writeFile(t, seeded, []byte("x"), 0o666)
	catSeedHome(t, root, filepath.Join(payload, "tools"))
	sttRepoRoot(t, sttMkdir(t, filepath.Join(tmp, "code")))
	machine := catUvCacheDir(t)
	if machine != filepath.Join(root, "cache", "uv") {
		t.Fatalf("machine cache = %q", machine)
	}
	if string(readFile(t, filepath.Join(machine, "wheels-v5", "some.pkg"))) != "x" {
		t.Fatal("seed was not copied out")
	}
	if !isRegularFile(filepath.Join(machine, ".seeded")) {
		t.Fatal("seed marker missing")
	}
	writeFile(t, seeded, []byte("changed"), 0o666)
	catUvCacheDir(t)
	if string(readFile(t, filepath.Join(machine, "wheels-v5", "some.pkg"))) != "x" {
		t.Fatal("a marked seed copied again")
	}
}

func TestCatUvCacheDirColdMachineNoPayload(t *testing.T) {
	tmp := t.TempDir()
	root := sttMkdir(t, filepath.Join(tmp, "home"))
	catSeedHome(t, root, filepath.Join(tmp, "nowhere", "tools"))
	sttRepoRoot(t, sttMkdir(t, filepath.Join(tmp, "code")))
	machine := catUvCacheDir(t)
	if machine != filepath.Join(root, "cache", "uv") || !isRegularFile(filepath.Join(machine, ".seeded")) {
		t.Fatalf("cold machine cache %q unmarked", machine)
	}
}

// The seed tests' sealed payload shipping uv-cache beside its store, returning the empty machine cache and payload cache
func catPayloadCache(t *testing.T) (string, string) {
	t.Helper()
	tmp := t.TempDir()
	payload := filepath.Join(tmp, "payload")
	writeFile(t, filepath.Join(payload, "uv-cache", "archive-v0", "bucket", "module.py"), []byte("seeded = True\n"), 0o666)
	store := sttMkdir(t, filepath.Join(payload, "tools"))
	catSeedHome(t, filepath.Join(tmp, "home"), store)
	sttRepoRoot(t, sttMkdir(t, filepath.Join(tmp, "repo-root")))
	return UvCachePath(), filepath.Join(payload, "uv-cache")
}

// Creates a payload link or skips where the host cannot create symlinks
func catSymlinkOrSkip(t *testing.T, target, link string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(link), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("host cannot create symlinks: %v", err)
	}
}

// The payload with uv's wheel shape, a relative link into archive-v0
func catPayloadCacheWithWheelLink(t *testing.T) (string, string) {
	t.Helper()
	machine, payload := catPayloadCache(t)
	wheel := filepath.Join(payload, "wheels-v6", "pypi", "pkg")
	catSymlinkOrSkip(t, "../../../archive-v0/bucket", filepath.Join(wheel, "1.0-abc123"))
	writeFile(t, filepath.Join(wheel, "1.0-abc123.http"), []byte("x"), 0o666)
	return machine, payload
}

// Seeded wheel link inside the machine cache
func catSeededLink(machine string) string {
	return filepath.Join(machine, "wheels-v6", "pypi", "pkg", "1.0-abc123")
}

// Fails the test unless path is a symlink naming target
func catWantLink(t *testing.T, path, target string) {
	t.Helper()
	if !isSymlink(path) {
		t.Fatalf("%s is not a symlink", path)
	}
	if got, err := os.Readlink(path); err != nil || got != target {
		t.Fatalf("%s links to %q, %v", path, got, err)
	}
}

func TestCatUpdateMergesTheNewPayloadIntoAnExistingSeed(t *testing.T) {
	machine, payload := catPayloadCache(t)
	code := sttMkdir(t, filepath.Join(t.TempDir(), "code"))
	writeFile(t, filepath.Join(code, "uv.lock"), []byte("version = 1  # old\n"), 0o666)
	sttRepoRoot(t, code)
	catUvCacheDir(t)
	writeFile(t, filepath.Join(payload, "archive-v0", "bucket", "new.py"), []byte("new pin\n"), 0o666)
	writeFile(t, filepath.Join(code, "uv.lock"), []byte("version = 1  # updated\n"), 0o666)
	catUvCacheDir(t)
	if string(readFile(t, filepath.Join(machine, "archive-v0", "bucket", "new.py"))) != "new pin\n" {
		t.Fatal("the updated payload did not reach the machine cache")
	}
}

func TestCatPartialSeedIsRetriedOnTheNextInstall(t *testing.T) {
	machine, _ := catPayloadCache(t)
	real := seedTree
	attempts := 0
	catSwap(t, &seedTree, func(src, dst string) error {
		attempts++
		if attempts == 1 {
			return &os.PathError{Op: "write", Path: dst, Err: errors.New("no space left on device")}
		}
		return real(src, dst)
	})
	catUvCacheDir(t)
	if isRegularFile(filepath.Join(machine, ".seeded")) {
		t.Fatal("a failed seed recorded completion")
	}
	catUvCacheDir(t)
	if !isRegularFile(filepath.Join(machine, ".seeded")) {
		t.Fatal("the retried seed recorded nothing")
	}
	if string(readFile(t, filepath.Join(machine, "archive-v0", "bucket", "module.py"))) != "seeded = True\n" {
		t.Fatal("the retry did not copy the bucket")
	}
}

func TestCatRetryFinishesASeedThatDiedInsideADirectory(t *testing.T) {
	machine, payload := catPayloadCache(t)
	writeFile(t, filepath.Join(payload, "archive-v0", "bucket", "pkg-1.0.dist-info", "METADATA"), []byte("Name: pkg\n"), 0o666)
	real := seedTree
	interrupted := false
	catSwap(t, &seedTree, func(src, dst string) error {
		if strings.HasSuffix(src, ".dist-info") && !interrupted {
			interrupted = true
			return &os.PathError{Op: "copy", Path: src, Err: errors.New("copy interrupted")}
		}
		return real(src, dst)
	})
	catUvCacheDir(t)
	bucket := filepath.Join(machine, "archive-v0", "bucket")
	if sttExists(filepath.Join(bucket, "pkg-1.0.dist-info")) || sttExists(filepath.Join(machine, ".seeded")) {
		t.Fatal("the interrupted bucket was recorded as done")
	}
	catUvCacheDir(t)
	if string(readFile(t, filepath.Join(bucket, "module.py"))) != "seeded = True\n" ||
		string(readFile(t, filepath.Join(bucket, "pkg-1.0.dist-info", "METADATA"))) != "Name: pkg\n" ||
		!isRegularFile(filepath.Join(machine, ".seeded")) {
		t.Fatal("the retry left the bucket incomplete")
	}
}

func TestCatRetryRecopiesAFileTheInterruptedSeedLeftShort(t *testing.T) {
	machine, _ := catPayloadCache(t)
	short := filepath.Join(machine, "archive-v0", "bucket", "module.py")
	writeFile(t, short, []byte("seed"), 0o666)
	catUvCacheDir(t)
	if string(readFile(t, short)) != "seeded = True\n" || !isRegularFile(filepath.Join(machine, ".seeded")) {
		t.Fatal("the short copy was kept")
	}
}

func TestCatMarkerFromASeedlessImageDoesNotBlockALaterSeed(t *testing.T) {
	machine, payload := catPayloadCache(t)
	code := sttMkdir(t, filepath.Join(t.TempDir(), "code"))
	writeFile(t, filepath.Join(code, "uv.lock"), []byte("version = 1\n"), 0o666)
	sttRepoRoot(t, code)
	seedless := filepath.Join(t.TempDir(), "uv-cache-hidden")
	if err := os.Rename(payload, seedless); err != nil {
		t.Fatal(err)
	}
	catUvCacheDir(t)
	if !isRegularFile(filepath.Join(machine, ".seeded")) {
		t.Fatal("a seedless image wrote no marker")
	}
	if err := os.Rename(seedless, payload); err != nil {
		t.Fatal(err)
	}
	catUvCacheDir(t)
	if !isRegularFile(filepath.Join(machine, "archive-v0", "bucket", "module.py")) {
		t.Fatal("the later seed was blocked by the seedless marker")
	}
}

func TestCatSeedPreservesAPayloadWheelLink(t *testing.T) {
	machine, _ := catPayloadCacheWithWheelLink(t)
	catUvCacheDir(t)
	link := catSeededLink(machine)
	catWantLink(t, link, "../../../archive-v0/bucket")
	if resolvePath(link) != resolvePath(filepath.Join(machine, "archive-v0", "bucket")) {
		t.Fatal("the relative link does not resolve to the seeded archive")
	}
	if !isRegularFile(filepath.Join(machine, "wheels-v6", "pypi", "pkg", "1.0-abc123.http")) {
		t.Fatal("the wheel metadata was not seeded")
	}
}

func TestCatSeedPreservesALinkAtThePayloadRoot(t *testing.T) {
	machine, payload := catPayloadCache(t)
	catSymlinkOrSkip(t, "archive-v0/bucket", filepath.Join(payload, "linked-bucket"))
	catUvCacheDir(t)
	link := filepath.Join(machine, "linked-bucket")
	catWantLink(t, link, "archive-v0/bucket")
	if string(readFile(t, filepath.Join(link, "module.py"))) != "seeded = True\n" {
		t.Fatal("the root link does not reach the seeded bucket")
	}
}

func TestCatSeedHealsADirectoryLeftByTheOldCopy(t *testing.T) {
	machine, _ := catPayloadCacheWithWheelLink(t)
	poisoned := catSeededLink(machine)
	writeFile(t, filepath.Join(poisoned, "module.py"), []byte("stale copy\n"), 0o666)
	catUvCacheDir(t)
	catWantLink(t, poisoned, "../../../archive-v0/bucket")
	if string(readFile(t, filepath.Join(poisoned, "module.py"))) != "seeded = True\n" {
		t.Fatal("the stale directory survived")
	}
}

func TestCatReseedingOverAnExistingGoodLinkIsIdempotent(t *testing.T) {
	machine, _ := catPayloadCacheWithWheelLink(t)
	catUvCacheDir(t)
	writeFile(t, filepath.Join(machine, ".seeded"), []byte("stale"), 0o666)
	catUvCacheDir(t)
	catWantLink(t, catSeededLink(machine), "../../../archive-v0/bucket")
}

func TestCatPayload2MarkerReseedsAVolumeMarkedByTheOldSeed(t *testing.T) {
	machine, _ := catPayloadCacheWithWheelLink(t)
	catUvCacheDir(t)
	marker := filepath.Join(machine, ".seeded")
	current := string(readFile(t, marker))
	before, _ := partition(current, "+")
	writeFile(t, marker, []byte(before+"+payload"), 0o666)
	poisoned := catSeededLink(machine)
	if err := os.Remove(poisoned); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(poisoned, "module.py"), []byte("stale copy\n"), 0o666)
	catUvCacheDir(t)
	if !isSymlink(poisoned) || string(readFile(t, marker)) != current {
		t.Fatal("the old payload marker was trusted")
	}
}

func TestCatSeedPublishesArchivesBeforeTheLinksIntoThem(t *testing.T) {
	machine, payload := catPayloadCacheWithWheelLink(t)
	catSwap(t, &seedReadDir, func(dir string) ([]os.DirEntry, error) {
		entries, err := os.ReadDir(dir)
		if dir == payload {
			slices.Reverse(entries)
		}
		return entries, err
	})
	real := copySeedFile
	catSwap(t, &copySeedFile, func(src, dst string) error {
		if slices.Contains(strings.Split(filepath.ToSlash(src), "/"), "archive-v0") {
			return &os.PathError{Op: "write", Path: dst, Err: errors.New("disk full")}
		}
		return real(src, dst)
	})
	catUvCacheDir(t)
	if isSymlink(catSeededLink(machine)) {
		t.Fatal("a wheel link was published before its archive")
	}
	if sttExists(filepath.Join(machine, ".seeded")) {
		t.Fatal("a failed seed recorded completion")
	}
}

// The publication tests' payload holding one complete entry, returning its source and machine destination
func catSeedPaths(t *testing.T) (string, string) {
	t.Helper()
	tmp := t.TempDir()
	machine := filepath.Join(tmp, "machine")
	payload := filepath.Join(tmp, "payload")
	store := sttMkdir(t, filepath.Join(payload, "tools"))
	source := filepath.Join(payload, "uv-cache", "entry")
	writeFile(t, source, []byte("complete-new-payload"), 0o666)
	catSeedHome(t, machine, store)
	repo := sttMkdir(t, filepath.Join(tmp, "repo"))
	writeFile(t, filepath.Join(repo, "uv.lock"), []byte("version = 1\n"), 0o666)
	sttRepoRoot(t, repo)
	return source, filepath.Join(machine, "cache", "uv", "entry")
}

// Names in a directory
func catNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestCatSeedPublishesOnlyACompleteFile(t *testing.T) {
	for _, previous := range []string{"", "old"} {
		t.Run("previous="+previous, func(t *testing.T) {
			source, destination := catSeedPaths(t)
			sttMkdir(t, filepath.Dir(destination))
			if previous != "" {
				writeFile(t, destination, []byte(previous), 0o666)
			}
			var observed []string
			catSwap(t, &seedCopy2, func(src, dst string) error {
				writeFile(t, dst, []byte("partial"), 0o666)
				seen := "<absent>"
				if data, err := os.ReadFile(destination); err == nil {
					seen = string(data)
				}
				observed = append(observed, seen)
				return copy2(src, dst)
			})
			catUvCacheDir(t)
			want := previous
			if want == "" {
				want = "<absent>"
			}
			if len(observed) != 1 || observed[0] != want {
				t.Fatalf("readers saw %q during the copy", observed)
			}
			if string(readFile(t, destination)) != string(readFile(t, source)) {
				t.Fatal("the published entry is not the payload's")
			}
			if names := catNames(t, filepath.Dir(destination)); !slices.Equal(names, []string{".seeded", "entry"}) {
				t.Fatalf("machine cache holds %q", names)
			}
		})
	}
}

func TestCatInterruptedSeedPreservesReadersAndRetries(t *testing.T) {
	for _, previous := range []string{"", "old"} {
		t.Run("previous="+previous, func(t *testing.T) {
			source, destination := catSeedPaths(t)
			sttMkdir(t, filepath.Dir(destination))
			if previous != "" {
				writeFile(t, destination, []byte(previous), 0o666)
			}
			attempts := 0
			catSwap(t, &seedCopy2, func(src, dst string) error {
				attempts++
				if attempts == 1 {
					writeFile(t, dst, []byte("partial"), 0o666)
					return &os.PathError{Op: "copy", Path: dst, Err: errors.New("synthetic copy interruption")}
				}
				return copy2(src, dst)
			})
			catUvCacheDir(t)
			if previous == "" {
				if sttExists(destination) {
					t.Fatal("an interrupted copy became visible")
				}
			} else if string(readFile(t, destination)) != previous {
				t.Fatal("an interrupted copy replaced the live entry")
			}
			if sttExists(filepath.Join(filepath.Dir(destination), ".seeded")) {
				t.Fatal("an interrupted seed recorded completion")
			}
			wantNames := []string(nil)
			if previous != "" {
				wantNames = []string{"entry"}
			}
			if names := catNames(t, filepath.Dir(destination)); !slices.Equal(names, wantNames) {
				t.Fatalf("machine cache holds %q", names)
			}
			catUvCacheDir(t)
			if string(readFile(t, destination)) != string(readFile(t, source)) {
				t.Fatal("the retry did not publish the payload entry")
			}
			if names := catNames(t, filepath.Dir(destination)); !slices.Equal(names, []string{".seeded", "entry"}) {
				t.Fatalf("machine cache holds %q", names)
			}
		})
	}
}
