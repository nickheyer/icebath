package pm

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

func TestInstallPolicyRealPolicyIsBooleanAndFailsClosed(t *testing.T) {
	cases := []struct {
		content string
		want    bool
	}{
		{"{}\n", true},
		{"security:\n  allow_lazy_installs: true\n", true},
		{"security:\n  allow_lazy_installs: false\n", false},
		{"security:\n  allow_lazy_installs: 'false'\n", false},
		{"security: [\n", false},
	}
	for _, c := range cases {
		isolateHome(t)
		os.Unsetenv(EnvDisableLazyInstalls)
		path := home.ConfigPath(home.Process())
		writeFile(t, path, []byte(c.content), 0o644)
		config.ResetCache()
		if got := LazyInstallsAllowed(); got != c.want {
			t.Fatalf("%q answered %v", c.content, got)
		}
		if string(readFile(t, path)) != c.content {
			t.Fatalf("policy read rewrote %q", c.content)
		}
	}
}

func TestInstallPolicyInternalDisableOverridesEnabledConfig(t *testing.T) {
	isolateHome(t)
	writeFile(t, home.ConfigPath(home.Process()), []byte("security:\n  allow_lazy_installs: true\n"), 0o644)
	t.Setenv(EnvDisableLazyInstalls, "1")
	if LazyInstallsAllowed() {
		t.Fatal("the internal disable lost to config")
	}
}

func TestInstallPolicyRegistryGapIsRefusedWithoutAWorker(t *testing.T) {
	isolateHome(t)
	target, err := CurrentTarget()
	if err != nil || strings.HasPrefix(target, "win32") {
		t.Skip("git is a registry gap on POSIX targets")
	}
	previous := workerArgv
	workerArgv = func() ([]string, error) { t.Fatal("a PM worker was started"); return nil, nil }
	defer func() { workerArgv = previous }()
	_, err = Ensure(context.Background(), "git", EnsureOptions{BaseEnv: map[string]string{}})
	ie := ieWantInstallError(t, err, "POSIX uses system git by choice")
	if ie.Remedy != "none" || ie.Package != "git" {
		t.Fatalf("refusal %+v", ie)
	}
}

// Zip archive of named members
func ieZip(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for _, name := range sortedKeys(files) {
		f, err := w.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Store})
		if err != nil {
			t.Fatal(err)
		}
		f.Write(files[name])
	}
	w.Close()
	return buf.Bytes()
}

// One download coverage report
type ieTick struct {
	done, total int64
	ranges      []RangeSet
}

func TestInstallPausePreservesArchivesAndResumesTheSamePin(t *testing.T) {
	for _, route := range []string{"install", "stage"} {
		for _, cached := range []bool{false, true} {
			t.Run(route+map[bool]string{false: "", true: "-cached"}[cached], func(t *testing.T) {
				iePauseCase(t, route, cached)
			})
		}
	}
}

// Pauses a two archive install mid transfer and resumes it without refetching finished bytes
func iePauseCase(t *testing.T, route string, cached bool) {
	tmp := isolateHome(t)
	ctx := context.Background()
	root := filepath.Join(tmp, "store")
	t.Setenv(EnvRuntimeDir, root)
	lockPath := filepath.Join(tmp, "lock.json")
	ieEmptyLock(t, lockPath)
	previous := lockfilePath
	lockfilePath = func() string { return lockPath }
	defer func() { lockfilePath = previous }()
	ieSwapRegistry(t, &BasePackage{Name: "download-components"})
	server := newRangeServer(t)
	runtimeDLL := bytes.Repeat(func() []byte {
		b := make([]byte, 256)
		for i := range b {
			b[i] = byte(i)
		}
		return b
	}(), 64*1024)
	contents := map[string][]byte{"engine.dat": []byte("engine component"), "runtime.dll": runtimeDLL}
	payloads := [][]byte{ieZip(t, map[string][]byte{"engine.dat": contents["engine.dat"]}), ieZip(t, map[string][]byte{"runtime.dll": runtimeDLL})}
	var pins []any
	var shas []string
	for i, payload := range payloads {
		path := "/component-" + string(rune('0'+i)) + ".zip"
		server.set(path, payload)
		pins = append(pins, map[string]any{"url": server.url(path), "sha256": hexSHA256(payload)})
		shas = append(shas, hexSHA256(payload))
	}
	lock := NewLockfile(lockPath)
	lock.SetPin("download-components", "1", map[string]any{"any": pins})
	if err := lock.Save(); err != nil {
		t.Fatal(err)
	}
	store := NewStore(root)
	if cached {
		scratch, cleanup, err := store.Scratch()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.Fetch(server.url("/component-0.zip"), shas[0], scratch, nil, nil); err != nil {
			t.Fatal(err)
		}
		cleanup()
	}
	other := filepath.Join(root, "fetch-"+strings.Repeat("f", 64), "other.zip")
	writeFile(t, other, []byte("another install"), 0o644)
	unrelated := filepath.Join(PartialsRoot(), "unrelated.part")
	writeFile(t, unrelated, []byte("in progress"), 0o644)
	pause := NewPauseEvent()
	var mu sync.Mutex
	var ticks []ieTick
	var stages [][4]any
	progress := func(stage string, done, total int64, label string) error {
		mu.Lock()
		stages = append(stages, [4]any{stage, done, total, label})
		mu.Unlock()
		if stage == "download" && label == "2/2" && int64(len(payloads[0])) < done && done < total {
			pause.Set()
		}
		return nil
	}
	downloadProgress := func(done, total int64, ranges []RangeSet) error {
		mu.Lock()
		ticks = append(ticks, ieTick{done, total, ranges})
		mu.Unlock()
		return nil
	}
	realize := func(progress InstallProgress, downloadProgress ProgressFunc) (string, error) {
		if route == "install" {
			_, err := ensure(ctx, "download-components", EnsureOptions{Explicit: true, Progress: progress, PauseEvent: pause, DownloadProgress: downloadProgress})
			return "", err
		}
		return stageOnly(ctx, "download-components", "linux-arm64-bionic", StageOnlyOptions{Progress: progress, PauseEvent: pause, DownloadProgress: downloadProgress})
	}
	_, err := realize(progress, downloadProgress)
	if !IsDownloadKind(err, DownloadWasPaused) {
		t.Fatalf("got %v, want a pause, stages %v", err, stages)
	}
	expected := int64(len(payloads[0]) + len(payloads[1]))
	if len(ticks) == 0 {
		t.Fatal("no download progress")
	}
	want0 := int64(0)
	if cached {
		want0 = int64(len(payloads[0]))
	}
	if ticks[0].done != want0 {
		t.Fatalf("first tick %d, want %d", ticks[0].done, want0)
	}
	var dones []int64
	for _, tick := range ticks {
		var covered int64
		for _, set := range tick.ranges {
			for _, r := range set.Ranges {
				covered += r.End - r.Start
			}
		}
		if tick.total != expected || covered != tick.done {
			t.Fatalf("tick %+v covers %d of %d", tick, covered, expected)
		}
		dones = append(dones, tick.done)
	}
	if !sort.SliceIsSorted(dones, func(i, j int) bool { return dones[i] < dones[j] }) {
		t.Fatalf("progress went backwards %v", dones)
	}
	factsPath, _ := FactsPath(ctx)
	facts, _ := NewFacts(factsPath, false)
	if _, ok := facts.Get("download-components"); ok {
		t.Fatal("a paused install recorded facts")
	}
	if matches, _ := filepath.Glob(filepath.Join(PartialsRoot(), "*.ranges")); len(matches) == 0 {
		t.Fatal("pause dropped the partial ranges")
	}
	var firstRequests []rangeSeen
	for _, seen := range server.seen() {
		if seen.Path == "/component-0.zip" {
			firstRequests = append(firstRequests, seen)
		}
	}
	if !isDirectory(filepath.Join(root, "fetch-"+shas[0])) {
		t.Fatal("the finished archive did not enter the cache")
	}
	if !cached && len(firstRequests) == 0 {
		t.Fatal("first archive was never requested")
	}
	pause.Clear()
	stages = nil
	entry, err := realize(func(stage string, done, total int64, label string) error {
		mu.Lock()
		stages = append(stages, [4]any{stage, done, total, label})
		mu.Unlock()
		return nil
	}, downloadProgress)
	if err != nil {
		t.Fatal(err)
	}
	if last := ticks[len(ticks)-1]; last.done != expected || last.total != expected {
		t.Fatalf("last tick %+v", last)
	}
	facts, _ = NewFacts(factsPath, false)
	fact, recorded := facts.Get("download-components")
	if route == "stage" {
		if recorded {
			t.Fatal("a stage recorded host facts")
		}
		var marker map[string]any
		json.Unmarshal(readFile(t, filepath.Join(entry, ".pm-stage-pin.json")), &marker)
		if !reflect.DeepEqual(marker, map[string]any{"target": "linux-arm64-bionic", "sha256": []any{shas[0], shas[1]}}) {
			t.Fatalf("stage marker %v", marker)
		}
		if pathExists(factsPath) {
			t.Fatal("a stage created the facts file")
		}
	} else {
		entry = filepath.Join(root, pyrepr.Str(fact["entry"]))
		digest, _ := TreeDigest(entry)
		if !jsonEquals(fact["artifacts"], []any{shas[0], shas[1]}) || fact["digest"] != digest {
			t.Fatalf("fact %v", fact)
		}
		if pathExists(filepath.Join(entry, ".pm-stage-pin.json")) {
			t.Fatal("a host install carries a stage marker")
		}
	}
	for name, body := range contents {
		if !bytes.Equal(readFile(t, filepath.Join(entry, name)), body) {
			t.Fatalf("%s differs", name)
		}
	}
	var again []rangeSeen
	for _, seen := range server.seen() {
		if seen.Path == "/component-0.zip" {
			again = append(again, seen)
		}
	}
	if !reflect.DeepEqual(again, firstRequests) {
		t.Fatalf("resume refetched the finished archive %v %v", again, firstRequests)
	}
	var unpacks []any
	seenStages := map[any]bool{}
	for _, s := range stages {
		seenStages[s[0]] = true
		if s[0] == "unpack" {
			unpacks = append(unpacks, s[3])
		}
	}
	if !reflect.DeepEqual(unpacks, []any{"1/2", "2/2"}) || !seenStages["download"] || !seenStages["verify"] {
		t.Fatalf("stages %v", stages)
	}
	parts, _ := filepath.Glob(filepath.Join(PartialsRoot(), "*.part"))
	if !reflect.DeepEqual(parts, []string{unrelated}) {
		t.Fatalf("partials %v", parts)
	}
	fetches, _ := filepath.Glob(filepath.Join(root, "fetch-*"))
	if !reflect.DeepEqual(fetches, []string{filepath.Dir(other)}) || string(readFile(t, other)) != "another install" ||
		string(readFile(t, unrelated)) != "in progress" {
		t.Fatalf("unrelated downloads changed %v", fetches)
	}
	server.mu.Lock()
	server.payloads = map[string][]byte{}
	server.mu.Unlock()
	if _, err := realize(nil, nil); err != nil {
		t.Fatalf("warm reuse needed the server: %v", err)
	}
}

func TestInstallPolicyStageWithoutLockPinRefuses(t *testing.T) {
	newIEEnv(t)
	_, err := stageOnly(context.Background(), "deptool", "linux-arm64-bionic", StageOnlyOptions{})
	ie := ieWantInstallError(t, err, "not in the lockfile")
	if ie.Remedy != "add it with `icebath pm lock --bump`" {
		t.Fatalf("remedy %q", ie.Remedy)
	}
	var unknown *UnknownPackageError
	if _, err := stageOnly(context.Background(), "nope", "linux-x64", StageOnlyOptions{}); !errors.As(err, &unknown) {
		t.Fatalf("got %v", err)
	}
}
