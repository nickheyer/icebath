package pm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Environment variable selecting the install engine helper mode of a re-executed test binary
const ieHelperEnv = "IE_PM_HELPER"

// Re-executed test binary entry for install engine children chosen by the parent test
func TestIEHelperProcess(t *testing.T) {
	mode := os.Getenv(ieHelperEnv)
	if mode == "" {
		return
	}
	var args []string
	json.Unmarshal([]byte(os.Getenv(ieHelperEnv+"_ARGS")), &args)
	switch mode {
	case "crash-publish":
		lockfilePath = func() string { return args[0] }
		MustRegister(&ieFakeTool{BinaryPackage: ieBinary("faketool", "bin/faketool")})
		original := publishStaged
		publishStaged = func(store *Store, staged, name string) (string, error) {
			if !strings.HasPrefix(name, "faketool-") {
				return original(store, staged, name)
			}
			if args[2] == "after-publish" {
				original(store, staged, name)
			}
			os.Exit(17)
			return "", nil
		}
		var err error
		if args[1] == "install" {
			_, err = ensure(context.Background(), "faketool", EnsureOptions{Explicit: true, BaseEnv: map[string]string{}})
		} else {
			_, err = stageOnly(context.Background(), "faketool", "linux-arm64-bionic", StageOnlyOptions{})
		}
		fmt.Fprintln(os.Stderr, "publish never crashed", err)
		os.Exit(3)
	case "worker":
		os.Exit(ieWorkerHelper(args))
	}
	fmt.Fprintln(os.Stderr, "unknown helper mode", mode)
	os.Exit(5)
}

// Command re-executing this test binary in an install engine helper mode
func ieHelperCommand(mode string, args ...string) *exec.Cmd {
	encoded, _ := json.Marshal(args)
	cmd := exec.Command(os.Args[0], "-test.run=^TestIEHelperProcess$", "-test.count=1")
	cmd.Env = append(os.Environ(), ieHelperEnv+"="+mode, ieHelperEnv+"_ARGS="+string(encoded))
	return cmd
}

// Captures engine log records for the test
func ieCaptureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &buf
}

// Realizes faketool as a host install or a bionic cross target stage
func (e *ieEnv) realize(t *testing.T, route string) (string, error) {
	t.Helper()
	ctx := context.Background()
	if route == "install" {
		_, err := ensure(ctx, "faketool", EnsureOptions{Explicit: true, BaseEnv: map[string]string{}})
		target, _ := CurrentTarget()
		return filepath.Join(e.runtime, e.fake.StoreEntry("1.0", target)), err
	}
	return stageOnly(ctx, "faketool", "linux-arm64-bionic", StageOnlyOptions{})
}

// Bytes of a file, nil when it does not exist
func ieMaybeRead(path string) []byte {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return data
}

func TestAuthoritySameVersionDifferentShaIsNotInstalledAndRepaired(t *testing.T) {
	for _, route := range []string{"install", "stage"} {
		t.Run(route, func(t *testing.T) {
			e := newIEEnv(t)
			logs := ieCaptureLogs(t)
			entry, err := e.realize(t, route)
			if err != nil {
				t.Fatal(err)
			}
			if route == "install" && !ieInstalled(t, "faketool") {
				t.Fatal("not installed")
			}
			if route == "stage" {
				writeFile(t, filepath.Join(entry, "sentinel"), []byte("published"), 0o644)
				if again, _ := e.realize(t, route); again != entry || string(readFile(t, filepath.Join(entry, "sentinel"))) != "published" {
					t.Fatal("a current stage was republished")
				}
			}
			digestB := e.tar(t, "faketool-1.0-b.tar.gz", map[string]string{"bin/faketool": "#!B"})
			lock := NewLockfile(e.lockPath)
			lock.SetPin("faketool", "1.0", map[string]any{"any": map[string]any{"url": e.baseURL + "/faketool-1.0-b.tar.gz", "sha256": digestB}})
			lock.Save()
			if route == "stage" {
				if again, err := e.realize(t, route); err != nil || again != entry {
					t.Fatalf("restage %v", err)
				}
				if string(readFile(t, filepath.Join(entry, "bin/faketool"))) != "#!B" || pathExists(filepath.Join(entry, "sentinel")) {
					t.Fatal("restage kept the old bytes")
				}
				factsPath, _ := FactsPath(context.Background())
				if pathExists(factsPath) {
					t.Fatal("stage wrote facts")
				}
				return
			}
			if ieInstalled(t, "faketool") {
				t.Fatal("a re-pin with the same version still counts as installed")
			}
			problems, err := Check(context.Background(), true)
			if err != nil || len(problems) != 1 || problems[0] != "faketool: not installed or outdated" {
				t.Fatalf("check %v %v", problems, err)
			}
			if _, err := e.realize(t, route); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(logs.String(), "1.0/"+e.digest[:12]) {
				t.Fatalf("repair was not logged: %s", logs.String())
			}
			fact, _ := e.facts(t).Get("faketool")
			target, _ := CurrentTarget()
			if string(readFile(t, filepath.Join(e.runtime, pyrepr.Str(fact["entry"]), "bin/faketool"))) != "#!B" ||
				fact["target"] != target || !jsonEquals(fact["artifacts"], []any{digestB}) {
				t.Fatalf("fact %v", fact)
			}
		})
	}
}

func TestAuthorityInstallRepairsCorruptEntryFromVerifiedArchive(t *testing.T) {
	for _, matching := range []bool{false, true} {
		for _, damage := range []string{"missing-binary", "changed-bytes", "entry-is-file", "missing-entry"} {
			t.Run(fmt.Sprintf("%s-%v", damage, matching), func(t *testing.T) {
				e := newIEEnv(t)
				logs := ieCaptureLogs(t)
				if _, err := e.ensure(t, "faketool", EnsureOptions{}); err != nil {
					t.Fatal(err)
				}
				factsPath := filepath.Join(e.runtime, "facts.json")
				entry := filepath.Join(e.runtime, e.entry(t, "faketool"))
				binary := filepath.Join(entry, "bin", "faketool")
				switch damage {
				case "missing-binary":
					os.Remove(binary)
				case "changed-bytes":
					os.WriteFile(binary, []byte("corrupted"), 0o755)
				default:
					os.RemoveAll(entry)
					if damage == "entry-is-file" {
						os.WriteFile(entry, []byte("not a directory"), 0o644)
					}
				}
				if !matching {
					var data map[string]any
					json.Unmarshal(readFile(t, factsPath), &data)
					delete(data["packages"].(map[string]any)["faketool"].(map[string]any), "artifacts")
					encoded, _ := json.Marshal(data)
					os.WriteFile(factsPath, encoded, 0o644)
				}
				if _, err := e.ensure(t, "faketool", EnsureOptions{Explicit: true}); err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(logs.String(), "repair: faketool re-realized") || string(readFile(t, binary)) != "#!x" {
					t.Fatalf("not repaired: %s", logs.String())
				}
				fact, _ := e.facts(t).Get("faketool")
				if digest, _ := TreeDigest(entry); fact["digest"] != digest {
					t.Fatal("digest does not bind the repaired bytes")
				}
			})
		}
	}
}

// Panic value of the injected interruption
type ieInterrupt struct{}

func TestAuthorityFailedReplacementPreservesEntryAndFacts(t *testing.T) {
	for _, route := range []string{"install", "stage"} {
		for _, failure := range []string{"invalid", "fetch", "unpack", "verify", "publish", "published-verify", "interrupt", "facts"} {
			if route == "stage" && failure == "facts" {
				continue
			}
			for _, initial := range []bool{false, true} {
				name := fmt.Sprintf("%s-%s-%s", route, failure, map[bool]string{false: "replacement", true: "first-install"}[initial])
				t.Run(name, func(t *testing.T) { ieFailedReplacement(t, route, failure, initial) })
			}
		}
	}
}

// One failed replacement case, ending with a retry from the verified cached archives
func ieFailedReplacement(t *testing.T, route, failure string, initial bool) {
	e := newIEEnv(t)
	target := "linux-arm64-bionic"
	if route == "install" {
		target, _ = CurrentTarget()
	}
	if !initial {
		if _, err := e.realize(t, route); err != nil {
			t.Fatal(err)
		}
	}
	factsPath := filepath.Join(e.runtime, "facts.json")
	oldFacts := ieMaybeRead(factsPath)
	entry := filepath.Join(e.runtime, e.fake.StoreEntry("1.0", target))
	marker := filepath.Join(entry, ".pm-stage-pin.json")
	oldMarker := ieMaybeRead(marker)
	files := map[string]string{"bin/faketool": "#!new"}
	if failure == "invalid" {
		files = map[string]string{"bin/unrelated": "bad layout"}
	}
	digest := e.tar(t, "replacement.tar.gz", files)
	extra := e.tar(t, "data.tar.gz", map[string]string{"share/data": "second archive"})
	lock := NewLockfile(e.lockPath)
	lock.SetPin("faketool", "1.0", map[string]any{"any": []any{
		map[string]any{"url": e.baseURL + "/replacement.tar.gz", "sha256": digest},
		map[string]any{"url": e.baseURL + "/data.tar.gz", "sha256": extra},
	}})
	lock.Save()
	fail := func() error {
		if failure == "interrupt" {
			panic(ieInterrupt{})
		}
		if failure == "publish" || failure == "facts" {
			return &fs.PathError{Op: "write", Path: entry, Err: errors.New("injected failure")}
		}
		return NewInstallError("faketool", "injected failure", "")
	}
	restore := func() {}
	switch failure {
	case "publish", "published-verify", "interrupt":
		original := publishStaged
		publishStaged = func(store *Store, staged, name string) (string, error) {
			if name != filepath.Base(entry) {
				return original(store, staged, name)
			}
			if failure == "published-verify" {
				published, err := original(store, staged, name)
				os.Remove(filepath.Join(published, "bin/faketool"))
				return published, err
			}
			return "", fail()
		}
		restore = func() { publishStaged = original }
	case "facts":
		original := recordToolFact
		recordToolFact = func(*Facts, string, string, string, EnvDiff, string, FactRecord) error { return fail() }
		restore = func() { recordToolFact = original }
	case "fetch":
		original := fetchArtifacts
		fetchArtifacts = func(*Store, []Artifact, string, ProgressFunc, *PauseEvent) ([]string, error) { return nil, fail() }
		restore = func() { fetchArtifacts = original }
	case "verify":
		e.fake.verifyHook = func(path, target string) string {
			if path != entry {
				return fail().Error()
			}
			return ""
		}
		restore = func() { e.fake.verifyHook = nil }
	case "unpack":
		e.fake.unpackHook = func(archive, staged, target string) error {
			if pathExists(filepath.Join(staged, "share/data")) {
				return fail()
			}
			return nil
		}
		restore = func() { e.fake.unpackHook = nil }
	}
	func() {
		defer restore()
		defer func() {
			r := recover()
			if failure == "interrupt" && r != (ieInterrupt{}) {
				t.Fatalf("interruption was swallowed: %v", r)
			}
			if failure != "interrupt" && r != nil {
				panic(r)
			}
		}()
		_, err := e.realize(t, route)
		if failure != "interrupt" && !isInstallError(err) {
			t.Fatalf("got %v, want an install error", err)
		}
	}()
	if !initial && string(ieMaybeRead(filepath.Join(entry, "bin/faketool"))) != "#!x" {
		t.Fatal("the failed replacement lost the old entry")
	}
	if !bytes.Equal(ieMaybeRead(factsPath), oldFacts) {
		t.Fatal("the failed replacement changed facts")
	}
	if !initial && !bytes.Equal(ieMaybeRead(marker), oldMarker) {
		t.Fatal("the failed replacement changed the stage marker")
	}
	archives := []string{filepath.Join(e.runtime, "fetch-"+digest), filepath.Join(e.runtime, "fetch-"+extra)}
	if failure != "fetch" {
		for _, archive := range archives {
			if !isDirectory(archive) {
				t.Fatalf("verified archive %s was dropped", archive)
			}
		}
	}
	if failure == "invalid" || failure == "fetch" {
		return
	}
	os.Remove(filepath.Join(e.docroot, "replacement.tar.gz"))
	os.Remove(filepath.Join(e.docroot, "data.tar.gz"))
	if _, err := e.realize(t, route); err != nil {
		t.Fatalf("retry from cached bytes: %v", err)
	}
	if string(readFile(t, filepath.Join(entry, "bin/faketool"))) != "#!new" || string(readFile(t, filepath.Join(entry, "share/data"))) != "second archive" {
		t.Fatal("retry did not publish the new pin")
	}
	for _, archive := range archives {
		if pathExists(archive) {
			t.Fatal("published archives were kept")
		}
	}
}

func TestAuthorityKilledReplacementRecoversOnNextInstall(t *testing.T) {
	for _, route := range []string{"install", "stage"} {
		for _, interruption := range []string{"before-publish", "after-publish"} {
			t.Run(route+"-"+interruption, func(t *testing.T) {
				e := newIEEnv(t)
				target := "linux-arm64-bionic"
				if route == "install" {
					target, _ = CurrentTarget()
				}
				if _, err := e.realize(t, route); err != nil {
					t.Fatal(err)
				}
				oldFact, _ := e.facts(t).Get("faketool")
				entry := filepath.Join(e.runtime, e.fake.StoreEntry("1.0", target))
				oldDigest, _ := TreeDigest(entry)
				digest := e.tar(t, "replacement.tar.gz", map[string]string{"bin/faketool": "#!new"})
				lock := NewLockfile(e.lockPath)
				lock.SetPin("faketool", "1.0", map[string]any{"any": map[string]any{"url": e.baseURL + "/replacement.tar.gz", "sha256": digest}})
				lock.Save()
				child := ieHelperCommand("crash-publish", e.lockPath, route, interruption)
				out, _ := child.CombinedOutput()
				if child.ProcessState.ExitCode() != 17 {
					t.Fatalf("child exited %d: %s", child.ProcessState.ExitCode(), out)
				}
				if fact, _ := e.facts(t).Get("faketool"); !jsonDeepEqual(fact, oldFact) {
					t.Fatalf("killed publisher changed facts %v", fact)
				}
				prefix := ".previous-"
				if route == "stage" {
					prefix += "stage-"
				}
				previous := filepath.Join(e.runtime, prefix+filepath.Base(entry))
				if digest, _ := TreeDigest(previous); digest != oldDigest {
					t.Fatal("the old bytes were not kept outside scratch")
				}
				if _, err := e.realize(t, route); err != nil {
					t.Fatal(err)
				}
				if string(readFile(t, filepath.Join(entry, "bin/faketool"))) != "#!new" || pathExists(previous) {
					t.Fatal("recovery did not publish the new pin")
				}
				if route == "install" {
					fact, _ := e.facts(t).Get("faketool")
					if current, _ := TreeDigest(entry); fact["digest"] != current {
						t.Fatal("facts do not bind the recovered entry")
					}
					return
				}
				var pin map[string]any
				json.Unmarshal(readFile(t, filepath.Join(entry, ".pm-stage-pin.json")), &pin)
				if !jsonEquals(pin["sha256"], []any{digest}) || pathExists(filepath.Join(e.runtime, "facts.json")) {
					t.Fatalf("stage marker %v", pin)
				}
			})
		}
	}
}

func TestAuthorityFailedRestorePreservesBothInterruptedVersions(t *testing.T) {
	e := newIEEnv(t)
	if _, err := e.ensure(t, "faketool", EnsureOptions{}); err != nil {
		t.Fatal(err)
	}
	entry := filepath.Join(e.runtime, e.entry(t, "faketool"))
	previous := filepath.Join(e.runtime, ".previous-"+filepath.Base(entry))
	writeFile(t, filepath.Join(previous, "saved"), []byte("previous"), 0o644)
	os.WriteFile(filepath.Join(entry, "bin", "faketool"), []byte("uncommitted replacement"), 0o755)
	original := renameEntry
	renameEntry = func(from, to string) error {
		if from == previous {
			return &os.LinkError{Op: "rename", Old: from, New: to, Err: errors.New("restore refused")}
		}
		return original(from, to)
	}
	defer func() { renameEntry = original }()
	_, err := e.ensure(t, "faketool", EnsureOptions{Explicit: true})
	if err == nil || !strings.Contains(err.Error(), "restore refused") {
		t.Fatalf("got %v", err)
	}
	if string(readFile(t, filepath.Join(entry, "bin", "faketool"))) != "uncommitted replacement" || string(readFile(t, filepath.Join(previous, "saved"))) != "previous" {
		t.Fatal("a failed restore lost one of the versions")
	}
}

func TestAuthorityFetchCacheVerificationIsOwnedByDownloader(t *testing.T) {
	for _, poisoned := range []bool{false, true} {
		e := newIEEnv(t)
		store := NewStore(filepath.Join(e.runtime, "scratch-store"))
		fetch := func() string {
			scratch, cleanup, err := store.Scratch()
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()
			path, err := store.Fetch(e.baseURL+"/faketool-1.0.tar.gz", e.digest, scratch, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			return path
		}
		first := fetch()
		if hexSHA256(readFile(t, first)) != e.digest {
			t.Fatal("first fetch digest")
		}
		if poisoned {
			os.WriteFile(first, []byte("poisoned"), 0o644)
		}
		if second := fetch(); second != first || hexSHA256(readFile(t, second)) != e.digest {
			t.Fatal("the cache served unverified bytes")
		}
	}
}

func TestAuthorityLegacyFactWithoutIdentityIsNotInstalled(t *testing.T) {
	e := newIEEnv(t)
	if _, err := e.ensure(t, "faketool", EnsureOptions{}); err != nil {
		t.Fatal(err)
	}
	factsPath := filepath.Join(e.runtime, "facts.json")
	var data map[string]any
	json.Unmarshal(readFile(t, factsPath), &data)
	fact := data["packages"].(map[string]any)["faketool"].(map[string]any)
	data["packages"].(map[string]any)["faketool"] = map[string]any{"entry": fact["entry"], "version": "1.0", "env": fact["env"]}
	encoded, _ := json.Marshal(data)
	os.WriteFile(factsPath, encoded, 0o644)
	if ieInstalled(t, "faketool") {
		t.Fatal("a legacy fact is vouched for")
	}
	if _, err := e.ensure(t, "faketool", EnsureOptions{}); err != nil {
		t.Fatal(err)
	}
	if fresh, _ := e.facts(t).Get("faketool"); !ieInstalled(t, "faketool") || !jsonEquals(fresh["artifacts"], []any{e.digest}) {
		t.Fatalf("legacy fact not repaired %v", fresh)
	}
}

func TestAuthorityUnprovenEntryWithoutFactsIsRebuilt(t *testing.T) {
	e := newIEEnv(t)
	target, _ := CurrentTarget()
	entry := filepath.Join(e.runtime, e.fake.StoreEntry("1.0", target))
	writeFile(t, filepath.Join(entry, "bin/faketool"), []byte("unproven"), 0o755)
	writeFile(t, filepath.Join(entry, "sentinel"), nil, 0o644)
	if _, err := ensure(context.Background(), "faketool", EnsureOptions{Explicit: true}); err != nil {
		t.Fatal(err)
	}
	fact, _ := e.facts(t).Get("faketool")
	digest, _ := TreeDigest(entry)
	if string(readFile(t, filepath.Join(entry, "bin/faketool"))) != "#!x" || pathExists(filepath.Join(entry, "sentinel")) || fact["digest"] != digest {
		t.Fatal("an unproven entry survived")
	}
}

func TestAuthorityTreeDigestIsContentBound(t *testing.T) {
	e := newIEEnv(t)
	a, b := filepath.Join(e.tmp, "tree-a"), filepath.Join(e.tmp, "tree-b")
	for _, root := range []string{a, b} {
		writeFile(t, filepath.Join(root, "sub", "f.txt"), []byte("hello"), 0o644)
		writeFile(t, filepath.Join(root, "top.txt"), []byte("t"), 0o644)
	}
	digest := func(root string) string {
		d, err := TreeDigest(root)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	if digest(a) != digest(b) {
		t.Fatal("same content digests differently")
	}
	os.WriteFile(filepath.Join(b, "top.txt"), []byte("u"), 0o644)
	if digest(a) == digest(b) {
		t.Fatal("a changed byte digests the same")
	}
	if err := os.Symlink("sub/f.txt", filepath.Join(b, "link")); err != nil {
		t.Skip("symlinks unavailable on this host")
	}
	first := digest(b)
	os.Remove(filepath.Join(b, "link"))
	os.Symlink("top.txt", filepath.Join(b, "link"))
	if digest(b) == first {
		t.Fatal("retargeting a link kept the digest")
	}
}

func TestAuthorityTreeDigestIgnoresPycache(t *testing.T) {
	e := newIEEnv(t)
	tree := filepath.Join(e.tmp, "py-entry")
	writeFile(t, filepath.Join(tree, "Lib", "os.py"), []byte("print('stdlib')"), 0o644)
	before, _ := TreeDigest(tree)
	writeFile(t, filepath.Join(tree, "Lib", "__pycache__", "os.cpython-311.pyc"), []byte("\x00compiled-bytes\x00"), 0o644)
	writeFile(t, filepath.Join(tree, "Lib", "json", "__pycache__", "tool.cpython-311.pyc"), []byte("\x00more\x00"), 0o644)
	if after, _ := TreeDigest(tree); after != before {
		t.Fatal("bytecode caches changed the digest")
	}
	os.WriteFile(filepath.Join(tree, "Lib", "os.py"), []byte("print('tampered')"), 0o644)
	if after, _ := TreeDigest(tree); after == before {
		t.Fatal("a tampered source kept the digest")
	}
	os.WriteFile(filepath.Join(tree, "Lib", "os.py"), []byte("print('stdlib')"), 0o644)
	if after, _ := TreeDigest(tree); after != before {
		t.Fatal("caches contributed to the digest")
	}
}
