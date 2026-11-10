package pm

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

func TestUpdateVersionKeySortsNumericAndSuffixes(t *testing.T) {
	for _, c := range [][2]string{{"2.53.0+5", "2.53.0+3"}, {"3.14.7+20260901", "3.14.7+20260900"}, {"26.8.1", "26.7.0"}, {"10362", "10361"}, {"9.0.1-rc1", "9.0.1"}} {
		if compareVersions(c[0], c[1]) <= 0 {
			t.Errorf("%s should sort after %s", c[0], c[1])
		}
	}
}

func TestUpdateMinorOf(t *testing.T) {
	if m := MinorOf("26.7.0"); !m.Ok || m.key() != "26.7" {
		t.Fatalf("26.7.0 %v", m)
	}
	if m := MinorOf("3.14.7+20260901"); !m.Ok || m.key() != "3.14" {
		t.Fatalf("3.14 %v", m)
	}
	if MinorOf("10362").Ok {
		t.Fatal("single component has a minor")
	}
}

func TestUpdateBestInMinor(t *testing.T) {
	versions := []string{"9.0.3", "9.0.1", "9.1.0", "8.4.9"}
	if got, ok := BestInMinor(versions, MinorOf("9.0")); !ok || got != "9.0.3" {
		t.Fatalf("9.0 %q", got)
	}
	if got, ok := BestInMinor(versions, MinorOf("9.1")); !ok || got != "9.1.0" {
		t.Fatalf("9.1 %q", got)
	}
	if _, ok := BestInMinor(versions, MinorOf("10.0")); ok {
		t.Fatal("10.0 found")
	}
}

// Package answering canned candidates per target
type peCannedPackage struct {
	BasePackage
	latest map[string][]string
}

func (p *peCannedPackage) LatestVersions(target, locked string) ([]string, error) {
	return p.latest[target], nil
}

func TestUpdateResolvePackageDecisions(t *testing.T) {
	type tc struct {
		style      string
		candidates [][2]any
		locked     string
		version    string
		perTarget  map[string]string
		changed    bool
		reason     string
	}
	cases := []tc{
		{"semver", [][2]any{{"a", []string{"10.0.1", "9.0.3"}}, {"b", []string{"9.0.3", "10.0.1"}}}, "9.0.3", "10.0.1", map[string]string{"a": "10.0.1", "b": "10.0.1"}, true, ""},
		{"semver", [][2]any{{"a", []string{"26.7.0"}}}, "26.7.0", "26.7.0", map[string]string{"a": "26.7.0"}, false, ""},
		{"semver", [][2]any{{"a", []string{"2.97.0"}}, {"b", []string{"2.96.0"}}}, "2.95.0", "", map[string]string{}, false, "no shared version"},
		{"minor", [][2]any{{"a", []string{"9.1.2", "9.0.1"}}, {"b", []string{"9.1.0", "9.0.1"}}}, "9.0.1", "9.1", map[string]string{"a": "9.1.2", "b": "9.1.0"}, true, ""},
		{"minor", [][2]any{{"a", []string{"9.1.2"}}, {"b", []string{"9.0.3"}}}, "9.0.1", "", map[string]string{}, false, "no shared minor"},
		{"minor", [][2]any{{"a", []string{"9.1.2"}}, {"b", []string{"9.1.0"}}}, "9.1", "9.1", map[string]string{"a": "9.1.2", "b": "9.1.0"}, false, ""},
		{"semver", [][2]any{{"a", []string{}}}, "1208+145", "", map[string]string{}, false, "no source"},
		{"semver", [][2]any{{"a", []string{"26.8.1"}}, {"b", []string{}}}, "26.7.0", "26.8.1", map[string]string{"a": "26.8.1"}, true, ""},
	}
	for i, c := range cases {
		p := &peCannedPackage{BasePackage: BasePackage{Name: "node", VersionStyle: c.style}, latest: map[string][]string{}}
		var targets []string
		for _, cand := range c.candidates {
			p.latest[cand[0].(string)] = cand[1].([]string)
			targets = append(targets, cand[0].(string))
		}
		p.bind(p)
		locked := c.locked
		got, err := ResolvePackage(p, targets, &locked, nil)
		if err != nil {
			t.Fatal(err)
		}
		version := ""
		if got.Version != nil {
			version = *got.Version
		}
		per := map[string]string{}
		for _, tv := range got.PerTarget {
			per[tv.Target] = tv.Version
		}
		if version != c.version || !reflect.DeepEqual(per, c.perTarget) || got.Changed() != c.changed || (c.reason != "" && got.Reason != c.reason) {
			t.Errorf("case %d: %q %v %v %q", i, version, per, got.Changed(), got.Reason)
		}
	}
}

// Fixture package with canned latest versions or a lookup failure, pinned only on this host
type peUpdateFixture struct {
	BasePackage
	versions []string
	failure  error
	lookups  int
	fetchURL func(version, target string) (string, error)
}

func (p *peUpdateFixture) MissingReason(target string) string {
	if current, _ := CurrentTarget(); target == current {
		return ""
	}
	return "not a fixture target"
}

func (p *peUpdateFixture) LatestVersions(target, locked string) ([]string, error) {
	p.lookups++
	return p.versions, p.failure
}

func (p *peUpdateFixture) FetchURL(version, target string) (string, error) {
	if p.fetchURL != nil {
		return p.fetchURL(version, target)
	}
	return fmt.Sprintf("https://fixture.example/%s-%s-%s.tgz", p.Name, version, target), nil
}

func (p *peUpdateFixture) KnownSHA256(version, url string) (string, error) {
	if p.fetchURL != nil {
		return "", nil
	}
	return strings.Repeat("0", 64), nil
}

// Registers packages for one test, restoring whatever those names held before
func peRegister(t *testing.T, list ...Package) {
	t.Helper()
	for _, p := range list {
		name := p.Base().Name
		registryMu.RLock()
		previous, had := packages[name]
		registryMu.RUnlock()
		if err := Register(p); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			registryMu.Lock()
			if had {
				packages[name] = previous
			} else {
				delete(packages, name)
			}
			registryMu.Unlock()
		})
	}
}

// Lockfile under a test directory that the process lockfile path answers with, each fixture pinned at 1.0
func peUpdateLock(t *testing.T, tmp string, list ...Package) *Lockfile {
	t.Helper()
	path := filepath.Join(tmp, "lock.json")
	ieEmptyLock(t, path)
	lock := NewLockfile(path)
	for _, p := range list {
		lock.SetPin(p.Base().Name, "1.0", map[string]any{})
	}
	if err := lock.Save(); err != nil {
		t.Fatal(err)
	}
	peSwap(t, &lockfilePath, func() string { return path })
	peSwap(t, &repoRoot, func() string { return tmp })
	t.Setenv(EnvRuntimeDir, filepath.Join(tmp, "tools"))
	return lock
}

// Makes every mutation and dependency refresh fail the test
func peForbidUpdateMutation(t *testing.T) {
	t.Helper()
	fail := func(what string) { t.Fatalf("lookup-only result reached %s", what) }
	peSwap(t, &updatePinArtifacts, func(Package, *Resolved, map[string]any) (map[string]any, error) { fail("pinning"); return nil, nil })
	peSwap(t, &updateInstallNames, func(context.Context, []string) int { fail("installation"); return 1 })
	peSwap(t, &updateRunLive, func(context.Context, io.Writer, []string, string, map[string]string, time.Duration) (int, string, error) {
		fail("a live command")
		return 1, "", nil
	})
	peSwap(t, &updateLockProject, func(context.Context, LockProjectOptions) error { fail("a lock refresh"); return nil })
	peSwap(t, &installSyncVenv, func(context.Context, SyncVenvOptions) error { fail("a venv sync"); return nil })
}

func TestFailedResolutionStopsEveryApplyPath(t *testing.T) {
	for _, check := range []bool{true, false} {
		for _, mixed := range []bool{true, false} {
			t.Run(fmt.Sprint(check, mixed), func(t *testing.T) {
				tmp := isolateHome(t)
				failed := &peUpdateFixture{BasePackage: BasePackage{Name: "failed-lookup"}, failure: errors.New("fixture index unavailable")}
				healthy := &peUpdateFixture{BasePackage: BasePackage{Name: "healthy-lookup"}, versions: []string{"2.0"}}
				list := []Package{failed}
				if mixed {
					list = append(list, healthy)
				}
				peRegister(t, list...)
				lock := peUpdateLock(t, tmp, list...)
				peForbidUpdateMutation(t)
				before := peRead(t, lock.Path)
				var names []string
				for _, p := range list {
					names = append(names, p.Base().Name)
				}
				var out bytes.Buffer
				rc, err := CmdUpdate(context.Background(), UpdateArgs{Names: names, Check: check, UV: true, NPM: true}, &out)
				if err != nil || rc != 1 || !strings.Contains(out.String(), "fixture index unavailable") {
					t.Fatalf("rc %d %v %q", rc, err, out.String())
				}
				if peRead(t, lock.Path) != before || existsOrLink(filepath.Join(tmp, "tools")) {
					t.Fatal("failed lookup wrote state")
				}
				if mixed && healthy.lookups != 1 {
					t.Fatalf("healthy lookups %d", healthy.lookups)
				}
			})
		}
	}
}

func TestCurrentAndManualResultsAreSuccessfulWithoutWrites(t *testing.T) {
	for _, versions := range [][]string{{"1.0"}, {}} {
		t.Run(fmt.Sprint(len(versions)), func(t *testing.T) {
			tmp := isolateHome(t)
			p := &peUpdateFixture{BasePackage: BasePackage{Name: "no-update"}, versions: versions}
			peRegister(t, p)
			lock := peUpdateLock(t, tmp, p)
			peForbidUpdateMutation(t)
			before := peRead(t, lock.Path)
			for _, check := range []bool{true, false} {
				if rc, err := CmdUpdate(context.Background(), UpdateArgs{Names: []string{"no-update"}, Check: check}, io.Discard); err != nil || rc != 0 {
					t.Fatalf("check %v: %d %v", check, rc, err)
				}
			}
			if peRead(t, lock.Path) != before || existsOrLink(filepath.Join(tmp, "tools")) {
				t.Fatal("no-op update wrote state")
			}
		})
	}
}

// Pin isolation fixture, a broken and a healthy package with the venv step finalizing an ok receipt
func pePinIsolation(t *testing.T) (*Lockfile, *peUpdateFixture, *peUpdateFixture) {
	t.Helper()
	tmp := isolateHome(t)
	broken := &peUpdateFixture{BasePackage: BasePackage{Name: "broken-pin"}, versions: []string{"2.0"}}
	healthy := &peUpdateFixture{BasePackage: BasePackage{Name: "healthy-pin"}, versions: []string{"3.0"}}
	peRegister(t, broken, healthy)
	lock := peUpdateLock(t, tmp, broken, healthy)
	peSwap(t, &updatePinArtifacts, func(p Package, d *Resolved, current map[string]any) (map[string]any, error) {
		if p.Base().Name == "broken-pin" {
			return nil, &PyError{Class: "ValueError", Message: "fixture pool skew: 404"}
		}
		return pinArtifacts(p, d, current)
	})
	peSwap(t, &updateInstallNames, func(context.Context, []string) int { return 0 })
	peSwap(t, &updateSyncVenvStep, func(ctx context.Context, out io.Writer) (bool, error) {
		FinalizeReceipt(ctx, "ok", 0, BeginReceipt(ctx, "sync"))
		return true, nil
	})
	return lock, broken, healthy
}

// Decision moving a fixture to a version on this host
func peDecision(name, version string) *Resolved {
	target, _ := CurrentTarget()
	return &Resolved{Name: name, Locked: strPtr("1.0"), Style: "semver", Version: strPtr(version),
		PerTarget: []TargetVersion{{target, version}}, ArtifactUpdates: map[string][]string{}}
}

func TestOneFailingPinDoesNotBlockTheRest(t *testing.T) {
	lock, _, _ := pePinIsolation(t)
	ctx := context.Background()
	var out bytes.Buffer
	rc, err := applyPins(ctx, &out, []*Resolved{peDecision("broken-pin", "2.0"), peDecision("healthy-pin", "3.0")}, lock)
	if err != nil || rc != 1 {
		t.Fatalf("rc %d %v", rc, err)
	}
	for _, want := range []string{"✗ broken-pin pin failed: fixture pool skew: 404", "✓ healthy-pin pinned 1.0 → 3.0", "pinned 1, failed 1: broken-pin"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q in %q", want, out.String())
		}
	}
	reread := NewLockfile(lock.Path)
	if v, _ := reread.Version("healthy-pin"); v != "3.0" {
		t.Fatalf("healthy %q", v)
	}
	if v, _ := reread.Version("broken-pin"); v != "1.0" {
		t.Fatalf("broken %q", v)
	}
	latest := LatestReceipt(ctx)
	steps, _ := latest["steps"].([]any)
	if latest["outcome"] != "failed" || pyrepr.Str(latest["exit_code"]) != "1" || len(steps) != 1 || steps[0].(map[string]any)["name"] != "pin broken-pin" {
		t.Fatalf("receipt %v", latest)
	}
}

func TestAllPinsFailingLeavesLockfileUntouched(t *testing.T) {
	lock, _, _ := pePinIsolation(t)
	before := peRead(t, lock.Path)
	var out bytes.Buffer
	rc, err := applyPins(context.Background(), &out, []*Resolved{peDecision("broken-pin", "2.0")}, lock)
	if err != nil || rc != 1 || !strings.Contains(out.String(), "✗ broken-pin pin failed") || !strings.Contains(out.String(), "every pin failed; lockfile untouched") {
		t.Fatalf("rc %d %v %q", rc, err, out.String())
	}
	if peRead(t, lock.Path) != before {
		t.Fatal("lockfile changed")
	}
}

func TestUpdateSameMinorRefreshUpdatesRealBytesAndPreservesUnresolvedTargets(t *testing.T) {
	for _, next := range []string{"1.2.3", "1.2.4"} {
		t.Run(next, func(t *testing.T) {
			tmp := isolateHome(t)
			srv := newRangeServer(t)
			var buf bytes.Buffer
			zw := zip.NewWriter(&buf)
			w, _ := zw.Create("tool.txt")
			w.Write([]byte("new rolling build"))
			zw.Close()
			body := buf.Bytes()
			srv.set("/new-build.zip", body)
			requests := &dltLog{}
			dltConfigure(srv, func(s *rangeServer) {
				s.intercept = func(w http.ResponseWriter, r *http.Request) bool { requests.add(r.URL.Path); return false }
			})
			target, _ := CurrentTarget()
			held := map[string]any{"url": "https://upstream.invalid/held.deb", "sha256": strings.Repeat("b", 64)}
			heldTarget := "linux-arm64-bionic"
			if target == heldTarget {
				heldTarget = "win32-arm64"
			}
			rolling := &peUpdateFixture{BasePackage: BasePackage{Name: "rolling-tool", VersionStyle: "minor"}}
			rolling.versions = []string{next}
			rolling.fetchURL = func(version, tgt string) (string, error) {
				if version != next || tgt != target {
					t.Fatalf("fetch %s %s", version, tgt)
				}
				return srv.url("/new-build.zip"), nil
			}
			rollingLatest := &peRollingPackage{peUpdateFixture: rolling}
			peRegister(t, rollingLatest)
			lock := peUpdateLock(t, tmp)
			lock.SetPin("rolling-tool", "1.2.3", map[string]any{target: map[string]any{"url": srv.url("/old-build.zip"), "sha256": strings.Repeat("a", 64)}, heldTarget: held})
			lock.SetPin("unrelated", "2.0", map[string]any{"any": held})
			lock.Save()
			var syncs int
			peSwap(t, &updateSyncVenvStep, func(ctx context.Context, out io.Writer) (bool, error) {
				syncs++
				fmt.Fprintln(out, "✓ venv")
				return true, nil
			})
			peSwap(t, &updateInstallNames, func(ctx context.Context, names []string) int {
				for _, name := range names {
					if _, err := ensure(ctx, name, EnsureOptions{Explicit: true}); err != nil {
						t.Logf("install %s: %v", name, err)
						return 1
					}
				}
				return 0
			})
			before := peRead(t, lock.Path)
			ctx := context.Background()
			args := UpdateArgs{Names: []string{"rolling-tool"}, Check: true}
			if rc, err := CmdUpdate(ctx, args, io.Discard); err != nil || rc != 1 {
				t.Fatalf("check %d %v", rc, err)
			}
			if peRead(t, lock.Path) != before || len(requests.list()) != 0 || syncs != 0 || existsOrLink(filepath.Join(tmp, "tools")) {
				t.Fatal("check wrote state or fetched")
			}
			args.Check = false
			if rc, err := CmdUpdate(ctx, args, io.Discard); err != nil || rc != 0 {
				t.Fatalf("apply %d %v", rc, err)
			}
			var after, original map[string]map[string]any
			json.Unmarshal([]byte(peRead(t, lock.Path)), &struct{ Packages *map[string]map[string]any }{&after})
			json.Unmarshal([]byte(before), &struct{ Packages *map[string]map[string]any }{&original})
			if !reflect.DeepEqual(after["unrelated"], original["unrelated"]) || after["rolling-tool"]["version"] != original["rolling-tool"]["version"] ||
				!reflect.DeepEqual(after["rolling-tool"]["artifacts"].(map[string]any)[heldTarget], held) {
				t.Fatalf("lock after %v", after)
			}
			if got := NewLockfile(lock.Path).Artifacts("rolling-tool", target); len(got) != 1 || got[0]["url"] != srv.url("/new-build.zip") || got[0]["sha256"] != hexSHA256(body) {
				t.Fatalf("artifacts %v", got)
			}
			facts, _ := NewFacts(filepath.Join(tmp, "tools", "facts.json"), false)
			fact, _ := facts.Get("rolling-tool")
			if peRead(t, filepath.Join(tmp, "tools", pyrepr.Str(fact["entry"]), "tool.txt")) != "new rolling build" || syncs != 1 {
				t.Fatalf("installed fact %v syncs %d", fact, syncs)
			}
			for _, path := range requests.list() {
				if path != "/new-build.zip" {
					t.Fatalf("requests %v", requests.list())
				}
			}
			pinned := peRead(t, lock.Path)
			seen := len(requests.list())
			syncs = 0
			for _, check := range []bool{true, false} {
				args.Check = check
				if rc, err := CmdUpdate(ctx, args, io.Discard); err != nil || rc != 0 {
					t.Fatalf("repeat %v: %d %v", check, rc, err)
				}
			}
			if peRead(t, lock.Path) != pinned || len(requests.list()) != seen || syncs != 0 {
				t.Fatal("current pin refetched or rewrote")
			}
		})
	}
}

// Minor style fixture whose candidates exist only on this host
type peRollingPackage struct{ *peUpdateFixture }

func (p *peRollingPackage) LatestVersions(target, locked string) ([]string, error) {
	if current, _ := CurrentTarget(); target == current {
		return p.versions, nil
	}
	return nil, nil
}

func (p *peRollingPackage) MissingReason(target string) string { return "" }

// Termux pass fixture, a retired library and a retired bionic tool row with the pool faked
func peTermuxDeployment(t *testing.T) (string, *Lockfile) {
	t.Helper()
	tmp := t.TempDir()
	tablePath := filepath.Join(tmp, "termux_runtime_libs.json")
	if err := SaveTermuxTable(rptTable(rptIconvOld), tablePath); err != nil {
		t.Fatal(err)
	}
	lockfile := rptTermuxLockfile(t, tmp, map[string]any{"uv": rptUV(rptUVOld)})
	peSwap(t, &termuxTablePath, func() string { return tablePath })
	peSwap(t, &updateLockfile, func() *Lockfile { return lockfile })
	peSwap(t, &termuxIndex, func() (map[string]PoolPackage, error) {
		return rptPool(PoolPackage{"uv", "0.12.15", rptUVNew, strings.Repeat("u", 64)}), nil
	})
	peSwap(t, &termuxHashURL, rptServed(map[string]string{rptIconv: strings.Repeat("i", 64), rptUVNew: strings.Repeat("u", 64)}))
	return tablePath, lockfile
}

func TestUpdateTermuxPassCheckReportsWithoutWriting(t *testing.T) {
	tablePath, _ := peTermuxDeployment(t)
	before := peRead(t, tablePath)
	var out bytes.Buffer
	if rc, err := termuxPass(context.Background(), &out, true); err != nil || rc != 1 {
		t.Fatalf("rc %d %v", rc, err)
	}
	for _, want := range []string{"libiconv", "1.19", "uv@linux-arm64-bionic", "0.12.15"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q in %q", want, out.String())
		}
	}
	if peRead(t, tablePath) != before {
		t.Fatal("check wrote the table")
	}
}

func TestUpdateTermuxPassRepinsTheRetiredRowsAndSaves(t *testing.T) {
	tablePath, lockfile := peTermuxDeployment(t)
	if rc, err := termuxPass(context.Background(), io.Discard, false); err != nil || rc != 0 {
		t.Fatalf("rc %d %v", rc, err)
	}
	written, err := LoadTermuxTable(tablePath)
	if err != nil {
		t.Fatal(err)
	}
	if got := rptPlain(rptLib(t, written, "libiconv")); !reflect.DeepEqual(got, map[string]any{"sha256": strings.Repeat("i", 64), "url": rptIconv, "version": "1.19"}) {
		t.Fatalf("libiconv %v", got)
	}
	if !reflect.DeepEqual(rptPlain(rptLib(t, written, "glib")), rptPlain(rptLib(t, rptTable(rptIconvOld), "glib"))) {
		t.Fatal("glib changed")
	}
	if got := lockfile.Artifacts("uv", "linux-arm64-bionic")[0]["url"]; got != rptUVNew {
		t.Fatalf("uv %v", got)
	}
}

func TestUpdateTermuxPassReportsADroppedPackageAsAFailure(t *testing.T) {
	tmp := t.TempDir()
	tablePath := filepath.Join(tmp, "termux_runtime_libs.json")
	row := rptObj("sha256", strings.Repeat("n", 64), "version", "1", "url", TermuxPool+"pool/main/g/gone/gone_1_aarch64.deb")
	if err := SaveTermuxTable(rptObj("libs", rptObj("gone", row)), tablePath); err != nil {
		t.Fatal(err)
	}
	peSwap(t, &termuxTablePath, func() string { return tablePath })
	peSwap(t, &updateLockfile, func() *Lockfile { return rptTermuxLockfile(t, tmp, map[string]any{}) })
	peSwap(t, &termuxIndex, func() (map[string]PoolPackage, error) { return map[string]PoolPackage{}, nil })
	var out bytes.Buffer
	if rc, err := termuxPass(context.Background(), &out, false); err != nil || rc != 1 || !strings.Contains(out.String(), "no longer carries it") {
		t.Fatalf("rc %d %v %q", rc, err, out.String())
	}
	written, _ := LoadTermuxTable(tablePath)
	if !reflect.DeepEqual(rptPlain(rptLib(t, written, "gone")), rptPlain(row)) {
		t.Fatal("dropped row changed")
	}
}

func TestUpdateCommandRoutesTermuxToItsOwnPass(t *testing.T) {
	var seen []bool
	peSwap(t, &updateTermuxPass, func(ctx context.Context, out io.Writer, check bool) (int, error) {
		seen = append(seen, check)
		return 0, nil
	})
	peSwap(t, &updateLockfile, func() *Lockfile { t.Fatal("resolved a package"); return nil })
	var out bytes.Buffer
	if rc, err := CmdUpdate(context.Background(), UpdateArgs{Check: true, Names: []string{"node"}, Termux: true}, &out); err != nil || rc != 0 {
		t.Fatalf("rc %d %v", rc, err)
	}
	if !reflect.DeepEqual(seen, []bool{true}) || !strings.Contains(out.String(), "ignoring names") {
		t.Fatalf("seen %v out %q", seen, out.String())
	}
}

// Checkout copy carrying the upstream installers, their pin generator and its PM package
func peBootstrapCheckout(t *testing.T) (string, *Lockfile) {
	t.Helper()
	root := t.TempDir()
	for _, rel := range []string{"scripts/gen-bootstrap-pins.py", "scripts/install.sh", "scripts/install.ps1"} {
		writeFile(t, filepath.Join(root, rel), readFile(t, filepath.Join(peReferences, rel)), 0o755)
	}
	if err := os.CopyFS(filepath.Join(root, "pm"), os.DirFS(filepath.Join(peReferences, "pm"))); err != nil {
		t.Fatal(err)
	}
	lock := NewLockfile(filepath.Join(root, "pm", "lock.json"))
	peSwap(t, &updateLockfile, func() *Lockfile { return lock })
	peSwap(t, &updateHashURL, func(string) (string, error) { return strings.Repeat("0", 64), nil })
	peSwap(t, &updateInstallNames, func(context.Context, []string) int { return 0 })
	peSwap(t, &updateSyncVenvStep, func(context.Context, io.Writer) (bool, error) { return true, nil })
	python := peHostPython(t)
	peSwap(t, &bootstrapPinsPython, func(context.Context) (string, error) { return python, nil })
	return root, lock
}

// Runs the installers' pin generator in check mode
func peFragmentsCheck(t *testing.T, root string) (int, string) {
	t.Helper()
	cmd := exec.Command(peHostPython(t), filepath.Join(root, "scripts", "gen-bootstrap-pins.py"), "--check")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	code, err := exitStatus(cmd.Run())
	if err != nil {
		t.Fatal(err)
	}
	return code, out.String()
}

// Re-pins uv on every non bionic target the way an update decision does
func peUVRepin(t *testing.T, lock *Lockfile) int {
	t.Helper()
	var per []TargetVersion
	for _, target := range AllTargets {
		if target != "linux-arm64-bionic" {
			per = append(per, TargetVersion{target, "99.0.0"})
		}
	}
	rc, err := applyPins(context.Background(), io.Discard, []*Resolved{{Name: "uv", Locked: lockedVersion(lock, "uv"), Style: "semver",
		Version: strPtr("99.0.0"), PerTarget: per, ArtifactUpdates: map[string][]string{}}}, lock)
	if err != nil {
		t.Fatal(err)
	}
	return rc
}

func TestUVRepinRegeneratesInstallerPins(t *testing.T) {
	bump := func(t *testing.T, lock *Lockfile) int {
		name, version := "uv", "99.0.0"
		rc, err := CmdLock(withSyncOutput(context.Background(), io.Discard), LockArgs{Name: &name, Version: &version})
		if err != nil {
			t.Fatal(err)
		}
		return rc
	}
	for _, c := range []struct {
		id    string
		repin func(*testing.T, *Lockfile) int
	}{{"update", peUVRepin}, {"bump", bump}} {
		t.Run(c.id, func(t *testing.T) {
			root, lock := peBootstrapCheckout(t)
			if rc := c.repin(t, lock); rc != 0 {
				t.Fatalf("rc %d", rc)
			}
			if code, out := peFragmentsCheck(t, root); code != 0 {
				t.Fatalf("fragments stale: %s", out)
			}
		})
	}
}

func TestRetryWithCurrentPinsRepairsFragmentsAFailedRunLeftStale(t *testing.T) {
	root, lock := peBootstrapCheckout(t)
	regenerate := updateRegenerate
	peSwap(t, &updateRegenerate, func(context.Context, *Lockfile) (int, error) { return 1, nil })
	if rc := peUVRepin(t, lock); rc != 1 {
		t.Fatalf("rc %d", rc)
	}
	if code, _ := peFragmentsCheck(t, root); code == 0 {
		t.Fatal("the failed run must leave the fragments stale")
	}
	updateRegenerate = regenerate
	if rc, err := applyPins(context.Background(), io.Discard, nil, lock); err != nil || rc != 0 {
		t.Fatalf("retry %d %v", rc, err)
	}
	if code, out := peFragmentsCheck(t, root); code != 0 {
		t.Fatalf("fragments stale: %s", out)
	}
}

func TestExportedSwapUpdateResolution(t *testing.T) {
	resolved := &Resolved{Name: "python"}
	restore := SwapUpdateResolution(func(Package, []string, *string, map[string]any) (*Resolved, error) { return resolved, nil },
		func(Package, *Resolved, map[string]any) (map[string]any, error) {
			return map[string]any{"pinned": true}, nil
		})
	got, _ := updateResolvePackage(nil, nil, nil, nil)
	pinned, _ := updatePinArtifacts(nil, nil, nil)
	restore()
	if got != resolved || pinned["pinned"] != true {
		t.Fatalf("swapped resolution %v %v", got, pinned)
	}
}
