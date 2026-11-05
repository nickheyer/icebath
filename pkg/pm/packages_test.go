package pm

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// Replaces a package level seam for the rest of the test
func catSwap[T any](t *testing.T, seam *T, value T) {
	t.Helper()
	saved := *seam
	*seam = value
	t.Cleanup(func() { *seam = saved })
}

// The pm core suite's FakeTool, a binary package at bin/faketool with the probe chosen by the caller
func catFakeTool(probe bool, args []string) *BinaryPackage {
	p := &BinaryPackage{
		BasePackage: BasePackage{Name: "faketool"},
		BinaryRel:   map[string]string{"win32": "bin/faketool", "posix": "bin/faketool"},
		NoProbe:     !probe,
		ProbeArgs:   args,
	}
	p.bind(p)
	return p
}

// Registered package, failing the test when the catalogue lacks it
func catPackage(t *testing.T, name string) Package {
	t.Helper()
	p, err := GetPackage(name)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// Host target, failing the test on an unsupported host
func catTarget(t *testing.T) string {
	t.Helper()
	target, err := CurrentTarget()
	if err != nil {
		t.Fatal(err)
	}
	return target
}

// Fails the test when a reason does not hold every fragment
func catContains(t *testing.T, reason string, fragments ...string) {
	t.Helper()
	for _, fragment := range fragments {
		if !strings.Contains(reason, fragment) {
			t.Fatalf("%q does not contain %q", reason, fragment)
		}
	}
}

// Keeps macOS codesign out of Python staging
func catNoSigning(t *testing.T) {
	catSwap(t, &signManagedPython, func(string) bool { return false })
}

// The shipped lockfile
func catLock() *Lockfile { return NewLockfile(LockfilePath()) }

func TestCatPythonPackageURLCarriesReleaseTag(t *testing.T) {
	python := catPackage(t, "python")
	url, err := python.FetchURL("3.14.7+20260901", "win32-arm64")
	if err != nil {
		t.Fatal(err)
	}
	catContains(t, url, "download/20260901/", "cpython-3.14.7+20260901-aarch64-pc-windows-msvc-install_only")
	_, err = python.FetchURL("3.14.7", "win32-arm64")
	dsWantInstallError(t, err, "")
}

func TestCatPythonStageDropsUnloadableX64VCRuntimeOnArm64(t *testing.T) {
	catNoSigning(t)
	staged := t.TempDir()
	writeFile(t, filepath.Join(staged, "python.exe"), []byte("launcher"), 0o666)
	writeFile(t, filepath.Join(staged, "vcruntime140_1.dll"), []byte("x64"), 0o666)
	writeFile(t, filepath.Join(staged, "vcruntime140.dll"), []byte("arm64"), 0o666)
	if err := catPackage(t, "python").Stage(nil, staged, "3.14.7", "win32-arm64"); err != nil {
		t.Fatal(err)
	}
	if sttExists(filepath.Join(staged, "vcruntime140_1.dll")) {
		t.Fatal("the x64 VC runtime survived ARM64 staging")
	}
	if !isRegularFile(filepath.Join(staged, "vcruntime140.dll")) {
		t.Fatal("the ARM64 VC runtime was dropped")
	}
}

func TestCatPythonStageKeepsVCRuntimesOnOtherTargets(t *testing.T) {
	catNoSigning(t)
	staged := t.TempDir()
	writeFile(t, filepath.Join(staged, "python.exe"), []byte("launcher"), 0o666)
	writeFile(t, filepath.Join(staged, "vcruntime140_1.dll"), []byte("x64"), 0o666)
	if err := catPackage(t, "python").Stage(nil, staged, "3.14.7", "win32-x64"); err != nil {
		t.Fatal(err)
	}
	if !isRegularFile(filepath.Join(staged, "vcruntime140_1.dll")) {
		t.Fatal("the x64 VC runtime was dropped on x64")
	}
}

func TestCatWindowsPythonAlsoAnswersToPython3(t *testing.T) {
	for _, target := range []string{"win32-x64", "win32-arm64"} {
		t.Run(target, func(t *testing.T) {
			catNoSigning(t)
			python := catPackage(t, "python")
			staged := t.TempDir()
			writeFile(t, filepath.Join(staged, "python.exe"), []byte("launcher"), 0o666)
			if err := python.Stage(nil, staged, "3.14.7", target); err != nil {
				t.Fatal(err)
			}
			alias := filepath.Join(staged, "python3.exe")
			if string(readFile(t, alias)) != "launcher" {
				t.Fatal("python3.exe is not a copy of python.exe")
			}
			if reason := python.Verify(staged, target); reason != "" {
				t.Fatalf("verify = %q", reason)
			}
			writeFile(t, alias, []byte("older launcher"), 0o666)
			catContains(t, python.Verify(staged, target), "differs")
			if err := os.Remove(alias); err != nil {
				t.Fatal(err)
			}
			catContains(t, python.Verify(staged, target), "python3.exe is missing")
		})
	}
}

func TestCatPython3AliasIsWindowsOnly(t *testing.T) {
	catNoSigning(t)
	python := catPackage(t, "python")
	staged := t.TempDir()
	writeFile(t, filepath.Join(staged, "bin", "python3"), []byte("interpreter"), 0o666)
	if err := python.Stage(nil, staged, "3.14.7", "linux-x64"); err != nil {
		t.Fatal(err)
	}
	if sttExists(filepath.Join(staged, "python3.exe")) {
		t.Fatal("python3.exe staged off Windows")
	}
	if reason := python.Verify(staged, "linux-x64"); reason != "" {
		t.Fatalf("verify = %q", reason)
	}
}

func TestCatVerifyMissingBinaryReportsPathAndListing(t *testing.T) {
	entry := filepath.Join(t.TempDir(), "entry")
	sttMkdir(t, filepath.Join(entry, "bin"))
	writeFile(t, filepath.Join(entry, "doc"), []byte("x"), 0o666)
	catContains(t, catFakeTool(false, nil).Verify(entry, catTarget(t)), "bin/faketool", "missing", "doc")
}

func TestCatVerifyProbeFailureReportsReason(t *testing.T) {
	entry := filepath.Join(t.TempDir(), "entry")
	writeFile(t, filepath.Join(entry, "bin", "faketool"), []byte("\x00\x01 not an executable"), 0o666)
	catContains(t, catFakeTool(true, nil).Verify(entry, catTarget(t)), "--version")
}

func TestCatProbeArgsOverrideUsedByVerify(t *testing.T) {
	entry := filepath.Join(t.TempDir(), "entry")
	writeFile(t, filepath.Join(entry, "bin", "faketool"), []byte("\x00\x01 not an executable"), 0o666)
	reason := catFakeTool(true, []string{"-version"}).Verify(entry, catTarget(t))
	catContains(t, reason, "-version")
	if strings.Contains(reason, "--version") {
		t.Fatalf("%q still describes the default probe", reason)
	}
}

func TestCatFfmpegBinaryRelFollowsTheBuildSource(t *testing.T) {
	ffmpeg := catPackage(t, "ffmpeg")
	entry := t.TempDir()
	writeFile(t, filepath.Join(entry, "ffmpeg"), []byte("x"), 0o666)
	writeFile(t, filepath.Join(entry, "bin", "ffmpeg"), []byte("x"), 0o666)
	for target, want := range map[string]string{
		"darwin-x64": filepath.Join(entry, "ffmpeg"), "darwin-arm64": filepath.Join(entry, "ffmpeg"),
		"linux-x64": filepath.Join(entry, "bin", "ffmpeg"), "linux-arm64": filepath.Join(entry, "bin", "ffmpeg"),
	} {
		if got := ffmpeg.Binary(entry, target); got != want {
			t.Fatalf("%s binary = %q, want %q", target, got, want)
		}
	}
	if args := ffmpeg.(*Ffmpeg).probeArgs(); !reflect.DeepEqual(args, []string{"-version"}) {
		t.Fatalf("probe args = %q", args)
	}
}

func TestCatVerifyArchMismatchReportsTarget(t *testing.T) {
	target := catTarget(t)
	wrong := uint16(0x8664)
	if strings.HasSuffix(target, "-x64") || strings.Contains(target, "-x64-") {
		wrong = 0xAA64
	}
	buf := append([]byte("MZ"), make([]byte, 0x3E)...)
	binary.LittleEndian.PutUint32(buf[0x3C:0x40], 0x40)
	buf = append(buf, "PE\x00\x00"...)
	buf = binary.LittleEndian.AppendUint16(buf, wrong)
	buf = append(buf, make([]byte, 64)...)
	entry := t.TempDir()
	writeFile(t, filepath.Join(entry, "bin", "faketool"), buf, 0o666)
	catContains(t, catFakeTool(false, nil).Verify(entry, target), "not a", target)
}

// Lock row of a package for one target, failing the test when it is absent
func catBionicRow(t *testing.T, name string) map[string]any {
	t.Helper()
	rows := catLock().Artifacts(name, bionicTarget)
	if len(rows) == 0 || rows[0] == nil {
		t.Fatalf("%s has no linux-arm64-bionic artifact", name)
	}
	return rows[0]
}

// The bionic row is https, matches its supplier's filename and is what fetch_url builds from the version it names
func catAssertPinnedBionicRow(t *testing.T, name, pattern string) {
	t.Helper()
	row := catBionicRow(t, name)
	url, _ := row["url"].(string)
	if !strings.HasPrefix(url, "https://") {
		t.Fatalf("%s bionic row not https: %q", name, url)
	}
	re := regexp.MustCompile(pattern)
	m := re.FindStringSubmatch(url)
	if m == nil {
		t.Fatalf("%s bionic url shape: %s", name, url)
	}
	if sha, _ := row["sha256"].(string); !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(sha) {
		t.Fatalf("%s bionic sha256 %q", name, sha)
	}
	got, err := catPackage(t, name).FetchURL(m[re.SubexpIndex("ver")], bionicTarget)
	if err != nil || got != url {
		t.Fatalf("%s fetch_url = %q, %v, want %q", name, got, err, url)
	}
}

func TestCatPythonBionicRowMatchesSupplier(t *testing.T) {
	catAssertPinnedBionicRow(t, "python", `/p/python/python_(?P<ver>[0-9.]+(?:-[0-9]+)?)_aarch64\.deb$`)
}

func TestCatNodeBionicRowMatchesSupplier(t *testing.T) {
	catAssertPinnedBionicRow(t, "node", `nodejs_(?P<ver>[0-9.]+)-1_aarch64\.deb$`)
}

func TestCatUvBionicRowMatchesSupplier(t *testing.T) {
	catAssertPinnedBionicRow(t, "uv", `/u/uv/uv_(?P<ver>[0-9.]+)_aarch64\.deb$`)
}

func TestCatPythonBionicPinIsIndependentOfDesktopBuildVersion(t *testing.T) {
	py := catPackage(t, "python")
	version, _ := catLock().Version("python")
	got, err := py.FetchURL(version, bionicTarget)
	if err != nil || got != catBionicRow(t, "python")["url"] {
		t.Fatalf("bionic url = %q, %v", got, err)
	}
	if name := py.(*Python).DebPackageName(); name != "python" {
		t.Fatalf("deb package = %q", name)
	}
}

func TestCatMuslUserlandGetsASatisfiableNativeMuslClosure(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("platforms linux")
	}
	fakeSh := filepath.Join(t.TempDir(), "sh")
	writeFile(t, fakeSh, append(append([]byte("\x7fELF"), make([]byte, 60)...), []byte("/lib/ld-musl-x86_64.so.1\x00")...), 0o666)
	catSwap(t, &nativeLinuxUsesMusl, func() (bool, bool) { return elfLoaderIsMusl(fakeSh) })
	catSwap(t, &nativeMachine, func() string { return "x86_64" })
	catSwap(t, &isBionicLibc, func() bool { return false })
	target := catTarget(t)
	if target != "linux-x64-musl" {
		t.Fatalf("target = %q", target)
	}
	lock := catLock()
	roots, err := SourceInstallPackages(AllPackages())
	if err != nil {
		t.Fatal(err)
	}
	closure, err := Walk(roots)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, p := range closure {
		name := p.Base().Name
		names[name] = true
		if reason := p.MissingReason(target); reason != "" {
			t.Fatalf("%s is missing on musl: %s", name, reason)
		}
		if _, pinned := lock.Version(name); pinned && len(lock.Artifacts(name, target)) == 0 {
			t.Fatalf("%s has no %s artifact", name, target)
		}
	}
	for _, name := range []string{"python", "uv", "node"} {
		if !names[name] {
			t.Fatalf("%s is not in the musl closure", name)
		}
		if url, _ := lock.Artifacts(name, target)[0]["url"].(string); !strings.Contains(url, "musl") {
			t.Fatalf("%s musl url %q", name, url)
		}
	}
}

func TestCatLlamacppBackendsHavePinsForTheirSupportedTargets(t *testing.T) {
	lock := catLock()
	backends := []string{"cpu", "cuda", "vulkan", "metal", "hip"}
	versions := map[string]bool{}
	for _, backend := range backends {
		version, _ := lock.Version("llamacpp-" + backend)
		versions[version] = true
	}
	if len(versions) != 1 {
		t.Fatalf("llama.cpp backends pin %d versions", len(versions))
	}
	for _, backend := range backends {
		name := "llamacpp-" + backend
		p := catPackage(t, name)
		version, _ := lock.Version(name)
		if version == "" {
			t.Fatalf("%s is unpinned", name)
		}
		for _, target := range AllTargets {
			artifacts := lock.Artifacts(name, target)
			if p.MissingReason(target) != "" {
				if len(artifacts) > 0 {
					t.Fatalf("%s pins %s which upstream does not build", name, target)
				}
				continue
			}
			if len(artifacts) == 0 {
				t.Fatalf("%s has no %s pin", name, target)
			}
			var urls []string
			for _, a := range artifacts {
				urls = append(urls, a["url"].(string))
				sum, err := hex.DecodeString(a["sha256"].(string))
				if err != nil || len(sum) != 32 {
					t.Fatalf("%s %s sha256 %v", name, target, a["sha256"])
				}
			}
			want, err := p.FetchURLs(version, target)
			if err != nil || !reflect.DeepEqual(urls, want) {
				t.Fatalf("%s %s urls %q, fetch_urls %q %v", name, target, urls, want, err)
			}
			if backend == "cuda" && !slices.ContainsFunc(urls, func(u string) bool { return strings.Contains(u, "cudart-") }) {
				t.Fatalf("%s %s pins no cudart", name, target)
			}
			if target == "linux-x64" || target == "linux-arm64" {
				if !slices.ContainsFunc(urls, func(u string) bool { return strings.Contains(u, "/libgomp1_") && strings.HasSuffix(u, ".deb") }) {
					t.Fatalf("%s %s pins no libgomp deb", name, target)
				}
			}
		}
	}
}

func TestCatTermuxHasNoPMBrowserDefault(t *testing.T) {
	isolateHome(t)
	names := catLock().Names()
	linux, err := DefaultPackages(context.Background(), names, "linux-x64", map[string]bool{})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(linux, "chromium") {
		t.Fatalf("linux defaults %q lack chromium", linux)
	}
	bionic, err := DefaultPackages(context.Background(), names, bionicTarget, map[string]bool{})
	if err != nil || len(bionic) != 0 {
		t.Fatalf("bionic defaults = %q, %v", bionic, err)
	}
}

func TestCatDefaultInstallCarriesTheComputerUseDriverWhereverItBuilds(t *testing.T) {
	isolateHome(t)
	lock := catLock()
	driver := catPackage(t, "cua-driver")
	for _, target := range AllTargets {
		defaults, err := DefaultPackages(context.Background(), lock.Names(), target, map[string]bool{})
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range defaults {
			if len(lock.Artifacts(name, target)) == 0 {
				t.Fatalf("default %s has no %s artifact", name, target)
			}
		}
		if slices.Contains(defaults, "cua-driver") != (driver.MissingReason(target) == "") {
			t.Fatalf("cua-driver default on %s disagrees with its gaps", target)
		}
	}
	declined, err := DefaultPackages(context.Background(), lock.Names(), "linux-x64", map[string]bool{"cua-driver": true})
	if err != nil || slices.Contains(declined, "cua-driver") {
		t.Fatalf("declined defaults = %q, %v", declined, err)
	}
}

func TestCatChromiumBinaryAndEnvSurviveStoreRelocation(t *testing.T) {
	for target, relative := range map[string]string{
		"linux-x64":    "chrome-linux64/chrome",
		"linux-arm64":  "chrome-linux/chrome",
		"darwin-arm64": "chrome-mac-arm64/Google Chrome for Testing.app/Contents/MacOS/Google Chrome for Testing",
		"win32-x64":    "chrome-win64/chrome.exe",
		"win32-arm64":  "chrome-win64/chrome.exe",
	} {
		t.Run(target, func(t *testing.T) {
			p := newChromium()
			tmp := t.TempDir()
			store := filepath.Join(tmp, "store")
			entry := filepath.Join(store, p.StoreEntry("1234+145.0.0.0", target))
			executable := filepath.Join(entry, filepath.FromSlash(relative))
			writeFile(t, executable, []byte("browser-fixture"), 0o755)
			if got := p.Binary(entry, target); got != executable {
				t.Fatalf("binary = %q, want %q", got, executable)
			}
			facts, err := NewFacts(filepath.Join(store, "facts.json"), false)
			if err != nil {
				t.Fatal(err)
			}
			env, err := PackageEnv(p, entry, target)
			if err != nil {
				t.Fatal(err)
			}
			if err := facts.Record("chromium", "1234+145.0.0.0", filepath.Base(entry), env, store, FactRecord{}); err != nil {
				t.Fatal(err)
			}
			moved := filepath.Join(tmp, "relocated")
			if err := os.Rename(store, moved); err != nil {
				t.Fatal(err)
			}
			relocated, err := NewFacts(filepath.Join(moved, "facts.json"), false)
			if err != nil {
				t.Fatal(err)
			}
			want := filepath.Join(moved, filepath.Base(entry), filepath.FromSlash(relative))
			if got := p.Binary(filepath.Join(moved, filepath.Base(entry)), target); got != want {
				t.Fatalf("relocated binary = %q, want %q", got, want)
			}
			if got := relocated.EnvFor("chromium", moved); !reflect.DeepEqual(got, EnvDiff{"PLAYWRIGHT_BROWSERS_PATH": moved}) {
				t.Fatalf("relocated env = %v", got)
			}
		})
	}
}

func TestCatChromiumEnvRefusesAnEntryWithoutItsExecutable(t *testing.T) {
	entry := t.TempDir()
	_, err := PackageEnv(newChromium(), entry, "linux-x64")
	dsWantInstallError(t, err, "Chromium executable missing under")
}

func TestCatMacOSCuaSelectsBundleBinaryAndPreservesSignature(t *testing.T) {
	for _, target := range []string{"darwin-arm64", "darwin-x64"} {
		t.Run(target, func(t *testing.T) {
			p := catPackage(t, "cua-driver")
			lock := catLock()
			version, ok := lock.Version("cua-driver")
			if !ok {
				t.Fatal("cua-driver is unpinned")
			}
			members := []member{
				{"cua-driver", []byte("standalone executable fixture")},
				{"CuaDriver.app/Contents/MacOS/cua-driver", []byte("bundle executable fixture")},
				{"CuaDriver.app/Contents/MacOS/cua-cursor-theme", []byte("cursor helper fixture")},
				{"CuaDriver.app/Contents/Info.plist", []byte("bundle metadata fixture")},
				{"CuaDriver.app/Contents/_CodeSignature/CodeResources", []byte("signature fixture")},
			}
			var archived []member
			for _, m := range members {
				archived = append(archived, member{"cua-release/" + m.Name, m.Data})
			}
			tmp := t.TempDir()
			archive := filepath.Join(tmp, "cua.tar.gz")
			writeFile(t, archive, makeTarGz(t, archived, 0o644), 0o666)
			staged := filepath.Join(tmp, "staged")
			if err := p.Unpack(archive, staged, target); err != nil {
				t.Fatal(err)
			}
			if err := p.Stage(NewStore(filepath.Join(tmp, "store")), staged, version, target); err != nil {
				t.Fatal(err)
			}
			if got, want := p.Binary(staged, target), filepath.Join(staged, "CuaDriver.app", "Contents", "MacOS", "cua-driver"); got != want {
				t.Fatalf("binary = %q, want %q", got, want)
			}
			for _, m := range members {
				if !bytes.Equal(readFile(t, filepath.Join(staged, filepath.FromSlash(m.Name))), m.Data) {
					t.Fatalf("%s changed in staging", m.Name)
				}
			}
			url, err := p.FetchURL(version, target)
			if err != nil || lock.Artifacts("cua-driver", target)[0]["url"] != url {
				t.Fatalf("pinned url disagrees with fetch_url %q %v", url, err)
			}
			if strings.Contains(url, "-binary.tar.gz") {
				t.Fatalf("macOS fetches the binary archive %q", url)
			}
		})
	}
}

func TestCatWindowsCuaKeepsUIAccessAndCursorHelpers(t *testing.T) {
	p := catPackage(t, "cua-driver")
	tmp := t.TempDir()
	var members []member
	for _, name := range []string{"cua-driver.exe", "cua-driver-uia.exe", "cua-cursor-theme.exe", "cua_driver_sdk.dll"} {
		members = append(members, member{name, []byte(name)})
	}
	archive := filepath.Join(tmp, "cua.zip")
	writeFile(t, archive, makeZip(t, members, nil), 0o666)
	staged := filepath.Join(tmp, "staged")
	if err := p.Unpack(archive, staged, "win32-x64"); err != nil {
		t.Fatal(err)
	}
	if err := p.Stage(NewStore(filepath.Join(tmp, "store")), staged, "fixture", "win32-x64"); err != nil {
		t.Fatal(err)
	}
	if got := p.Binary(staged, "win32-x64"); got != filepath.Join(staged, "cua-driver.exe") {
		t.Fatalf("binary = %q", got)
	}
	for _, m := range members {
		if !bytes.Equal(readFile(t, filepath.Join(staged, m.Name)), m.Data) {
			t.Fatalf("%s changed in staging", m.Name)
		}
	}
}

func TestCatCuaDriverProbeNeverMintsTelemetryState(t *testing.T) {
	catSwap(t, &CuaDriverChildEnv, nil)
	env, err := newCuaDriver().ProbeEnv()
	if err != nil || env["CUA_DRIVER_RS_TELEMETRY_ENABLED"] != "0" {
		t.Fatalf("probe env telemetry = %q, %v", env["CUA_DRIVER_RS_TELEMETRY_ENABLED"], err)
	}
	catSwap(t, &CuaDriverChildEnv, func() (map[string]string, error) { return map[string]string{"FROM": "tools"}, nil })
	if env, _ := newCuaDriver().ProbeEnv(); !reflect.DeepEqual(env, map[string]string{"FROM": "tools"}) {
		t.Fatalf("probe env = %v", env)
	}
	catSwap(t, &CuaDriverChildEnv, func() (map[string]string, error) { return nil, errors.New("config unreadable") })
	if env, _ := newCuaDriver().ProbeEnv(); env["CUA_DRIVER_RS_TELEMETRY_ENABLED"] != "0" {
		t.Fatalf("failed child env kept telemetry %v", env)
	}
}

func TestCatDmgbuildStagesPairedRuntimeAndStaysOutOfPayload(t *testing.T) {
	for target, cpu := range map[string]uint32{"darwin-arm64": 0x0100000C, "darwin-x64": 0x01000007} {
		t.Run(target, func(t *testing.T) {
			p := catPackage(t, "dmgbuild")
			lock := catLock()
			version, ok := lock.Version("dmgbuild")
			if !ok {
				t.Fatal("dmgbuild is unpinned")
			}
			url, err := p.FetchURL(version, target)
			if err != nil || lock.Artifacts("dmgbuild", target)[0]["url"] != url {
				t.Fatalf("pinned url disagrees with fetch_url %q %v", url, err)
			}
			if latest, err := p.LatestVersions(target, version); err != nil || len(latest) != 0 {
				t.Fatalf("latest versions = %q, %v", latest, err)
			}
			if base := p.Base(); !base.Internal || base.OnPath() {
				t.Fatal("dmgbuild must be internal and off PATH")
			}
			for _, other := range AllTargets {
				if !strings.HasPrefix(other, "darwin-") && p.MissingReason(other) == "" {
					t.Fatalf("dmgbuild claims %s", other)
				}
			}
			python := binary.LittleEndian.AppendUint32(binary.LittleEndian.AppendUint32(nil, 0xFEEDFACF), cpu)
			python = append(python, make([]byte, 56)...)
			launcher := []byte("#!/usr/bin/env bash\nexec \"$(dirname \"$0\")/python/bin/python3\" -m dmgbuild \"$@\"\n")
			tmp := t.TempDir()
			archive := filepath.Join(tmp, "dmgbuild.tar.gz")
			writeFile(t, archive, makeTarGz(t, []member{{"./python/bin/python3", python}, {"./dmgbuild", launcher}}, 0o755), 0o666)
			catSwap(t, &runProcess, func(spec processSpec) (processResult, error) {
				t.Fatalf("staging executed the foreign supplier: %q", spec.Argv)
				return processResult{}, nil
			})
			staged := filepath.Join(tmp, "staged")
			if err := p.Unpack(archive, staged, target); err != nil {
				t.Fatal(err)
			}
			if err := p.Stage(NewStore(filepath.Join(tmp, "store")), staged, version, target); err != nil {
				t.Fatal(err)
			}
			if got := p.Binary(staged, target); got != filepath.Join(staged, "dmgbuild") {
				t.Fatalf("binary = %q", got)
			}
			if reason := p.Verify(staged, target); reason != "" {
				t.Fatalf("verify = %q", reason)
			}
			if !bytes.Equal(readFile(t, filepath.Join(staged, "dmgbuild")), launcher) {
				t.Fatal("launcher changed in staging")
			}
			wrong := "darwin-arm64"
			if target == "darwin-arm64" {
				wrong = "darwin-x64"
			}
			catContains(t, p.Verify(staged, wrong), "not a")
			if err := os.Remove(filepath.Join(staged, "python", "bin", "python3")); err != nil {
				t.Fatal(err)
			}
			catContains(t, p.Verify(staged, target), "python/bin/python3 missing")
		})
	}
}

// Facts recording one installed entry for a package, the shape install location lookups return
func catFacts(t *testing.T, store *Store, name, entry string) *Facts {
	t.Helper()
	facts, err := NewFacts(filepath.Join(t.TempDir(), "facts.json"), false)
	if err != nil {
		t.Fatal(err)
	}
	if err := facts.Record(name, "1", entry, EnvDiff{}, store.Root, FactRecord{}); err != nil {
		t.Fatal(err)
	}
	return facts
}

func TestCatArm64WindowsRipgrepStagesPythonsVCRuntimeBesideRg(t *testing.T) {
	tmp := t.TempDir()
	store := NewStore(filepath.Join(tmp, "store"))
	vcRuntime := filepath.Join(store.Entry("python-1"), "vcruntime140.dll")
	writeFile(t, vcRuntime, []byte("arm64 vc runtime"), 0o666)
	catSwap(t, &catalogueLockfile, func() *Lockfile { return nil })
	facts := catFacts(t, store, "python", "python-1")
	catSwap(t, &catalogueInstalledLocation, func(p Package, lock *Lockfile, target string) (*Location, error) {
		return &Location{Facts: facts, Store: store}, nil
	})
	staged := filepath.Join(tmp, "scratch", "tree")
	writeFile(t, filepath.Join(staged, "ripgrep-15.2.0-aarch64-pc-windows-msvc", "rg.exe"), []byte("rg"), 0o666)
	if err := catPackage(t, "ripgrep").Stage(store, staged, "15.2.0", "win32-arm64"); err != nil {
		t.Fatal(err)
	}
	if !isRegularFile(filepath.Join(staged, "rg.exe")) {
		t.Fatal("rg.exe was not hoisted")
	}
	if !bytes.Equal(readFile(t, filepath.Join(staged, "vcruntime140.dll")), readFile(t, vcRuntime)) {
		t.Fatal("vcruntime140.dll is not python's copy")
	}
}

func TestCatPythonInstallsBeforeRipgrepSoStagingCanReadIt(t *testing.T) {
	order, err := Walk([]string{"ripgrep"})
	if err != nil {
		t.Fatal(err)
	}
	index := func(name string) int {
		return slices.IndexFunc(order, func(p Package) bool { return p.Base().Name == name })
	}
	if index("python") < 0 || index("python") > index("ripgrep") {
		t.Fatal("python does not install before ripgrep")
	}
}

// The pinned PortableGit self extractor in a fetch entry the fake run never executes
func catPortableGit(t *testing.T, tmp string) string {
	t.Helper()
	archive := filepath.Join(tmp, "fetch", "PortableGit-2.53.0.3-64-bit.7z.exe")
	writeFile(t, archive, []byte("pinned bytes; the fake run never executes them"), 0o666)
	return archive
}

// Replaces child runs with a recorder answering one exit code
func catFakeRun(t *testing.T, code int, calls *[]processSpec) {
	catSwap(t, &runProcess, func(spec processSpec) (processResult, error) {
		if calls != nil {
			*calls = append(*calls, spec)
		}
		return processResult{Code: code}, nil
	})
}

func TestCatGitUnpackExecutesAScratchCopyNeverTheCachedBytes(t *testing.T) {
	catSwap(t, &hostIsWindows, true)
	var calls []processSpec
	catFakeRun(t, 0, &calls)
	tmp := t.TempDir()
	archive := catPortableGit(t, tmp)
	staged := filepath.Join(tmp, "scratch", "tree")
	if err := newGit().Unpack(archive, staged, "win32-x64"); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 {
		t.Fatalf("%d extractor runs", len(calls))
	}
	argv := calls[0].Argv
	if !reflect.DeepEqual(argv[1:], []string{"-o" + staged, "-y"}) {
		t.Fatalf("extractor argv %q", argv)
	}
	exe := argv[0]
	if strings.HasPrefix(exe, filepath.Dir(archive)+string(filepath.Separator)) {
		t.Fatal("the cached bytes were executed")
	}
	if !strings.HasPrefix(exe, filepath.Dir(staged)+string(filepath.Separator)) {
		t.Fatalf("scratch copy %q is not beside the staging tree", exe)
	}
	if sttExists(exe) {
		t.Fatal("the scratch copy survived the run")
	}
	if !calls[0].NullStdin || calls[0].Capture {
		t.Fatal("the extractor inherits pipes its children could hold open")
	}
}

func TestCatGitUnpackNamesTheExtractorExitCode(t *testing.T) {
	catSwap(t, &hostIsWindows, true)
	catFakeRun(t, 7, nil)
	tmp := t.TempDir()
	dsWantInstallError(t, newGit().Unpack(catPortableGit(t, tmp), filepath.Join(tmp, "scratch", "tree"), "win32-x64"), "exited 7")
}

func TestCatGitUnpackTimeoutIsAPackageError(t *testing.T) {
	catSwap(t, &hostIsWindows, true)
	catSwap(t, &runProcess, func(spec processSpec) (processResult, error) {
		return processResult{}, &processTimeoutError{Argv: spec.Argv, Timeout: spec.Timeout}
	})
	tmp := t.TempDir()
	dsWantInstallError(t, newGit().Unpack(catPortableGit(t, tmp), filepath.Join(tmp, "scratch", "tree"), "win32-x64"), "did not finish")
}

func TestCatGitUnpackRequiresAWindowsHost(t *testing.T) {
	var calls []processSpec
	catFakeRun(t, 0, &calls)
	catSwap(t, &hostIsWindows, false)
	tmp := t.TempDir()
	err := newGit().Unpack(catPortableGit(t, tmp), filepath.Join(tmp, "out"), "win32-x64")
	dsWantInstallError(t, err, "Windows host")
	if strings.Contains(err.Error(), "retry") {
		t.Fatalf("remedy suggests retrying: %v", err)
	}
	if len(calls) != 0 {
		t.Fatal("the guard ran the extractor")
	}
}

func TestCatNpmSelfInstallLeavesDownloadEntryHoldingOnlyTheArchive(t *testing.T) {
	target := catTarget(t)
	tmp := t.TempDir()
	store := NewStore(filepath.Join(tmp, "store"))
	nodeBin := catPackage(t, "node").Binary(store.Entry("node-1"), target)
	writeFile(t, nodeBin, []byte("node"), 0o755)
	cliRoot := filepath.Join(filepath.Dir(filepath.Dir(nodeBin)), "lib")
	if strings.HasPrefix(target, "win32") {
		cliRoot = filepath.Dir(nodeBin)
	}
	writeFile(t, filepath.Join(cliRoot, "node_modules", "npm", "bin", "npm-cli.js"), nil, 0o666)
	catSwap(t, &catalogueLockfile, func() *Lockfile { return nil })
	facts := catFacts(t, store, "node", "node-1")
	catSwap(t, &catalogueInstalledLocation, func(p Package, lock *Lockfile, target string) (*Location, error) {
		return &Location{Facts: facts, Store: store}, nil
	})
	var caches []string
	catSwap(t, &runProcess, func(spec processSpec) (processResult, error) {
		cache := spec.Env["npm_config_cache"]
		writeFile(t, filepath.Join(cache, "_cacache", "content-v2", "sha512", "b8", "85", "blob"), []byte("tarball"), 0o666)
		caches = append(caches, cache)
		return processResult{}, nil
	})
	entry := store.Entry("fetch-" + strings.Repeat("a", 64))
	archive := filepath.Join(entry, "npm-1.0.0.tgz")
	writeFile(t, archive, []byte("archive"), 0o666)
	staged := filepath.Join(tmp, "scratch", "tree")
	if err := catPackage(t, "npm").Unpack(archive, staged, target); err != nil {
		t.Fatal(err)
	}
	sttSamePaths(t, sttChildren(t, entry), []string{archive})
	if len(caches) == 0 || strings.HasPrefix(caches[0], staged) {
		t.Fatalf("npm cache %q", caches)
	}
}

func TestCatNpmEnvKeepsAmbientNodeOptionsOut(t *testing.T) {
	env := NpmEnv("/cache", map[string]string{
		"npm_config_registry": "x", "NPM_CONFIG_PREFIX": "y", "NODE_OPTIONS": "--inspect", "node_path": "z",
		"Node_Env": "production", "PATH": "/bin",
	})
	if !reflect.DeepEqual(env, map[string]string{"PATH": "/bin", "npm_config_cache": "/cache"}) {
		t.Fatalf("npm env = %v", env)
	}
}

func TestCatStagedRepairInstallsLibatomicThenReprobes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("platforms posix")
	}
	loader := "node: error while loading shared libraries: libatomic.so.1: cannot open shared object file: No such file or directory"
	tmp := t.TempDir()
	node := filepath.Join(tmp, "bin", "node")
	writeFile(t, node, []byte("#!/bin/sh\necho \""+loader+"\" >&2\nexit 127\n"), 0o755)
	catSwap(t, &packagesCurrentTarget, func() (string, error) { return "linux-x64", nil })
	calls := 0
	catSwap(t, &tryInstallLibatomic, func(context.Context) (bool, string) {
		calls++
		writeFile(t, node, []byte("#!/bin/sh\necho v26.7.0\n"), 0o755)
		return true, "install with sudo dnf install -y libatomic"
	})
	p := newNodejs()
	first := p.Verify(tmp, "linux-x64")
	reason, remedy := p.RepairStagedVerification(tmp, "linux-x64", first)
	if calls != 1 || reason != "" || remedy != "" {
		t.Fatalf("repair = %q %q after %d installs (first %q)", reason, remedy, calls, first)
	}
}

func TestCatNodeCandidatesKeepTermuxDockerOutOfUpdates(t *testing.T) {
	versions, err := catPackage(t, "termux-docker").LatestVersions(bionicTarget, "")
	if err != nil || len(versions) != 0 {
		t.Fatalf("termux-docker latest = %q, %v", versions, err)
	}
}

func TestCatTermuxDockerIsAPinNeverStaged(t *testing.T) {
	p := catPackage(t, "termux-docker")
	if pin, ok := p.(PinOnlyPackage); !ok || !pin.PinOnly() {
		t.Fatal("termux-docker is not pin only")
	}
	if p.MissingReason(bionicTarget) != "" || p.MissingReason("linux-x64") == "" {
		t.Fatal("termux-docker targets only bionic")
	}
	dsWantInstallError(t, p.Unpack("image", t.TempDir(), bionicTarget), "pin, not a downloadable artifact")
}

func TestCatProbeRunnerReportsExitTimeoutAndSpawnFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("platforms posix")
	}
	result, err := startProcess(processSpec{Argv: []string{"/bin/sh", "-c", "echo out; echo err >&2; exit 3"}, Capture: true})
	if err != nil || result.Code != 3 || string(result.Stdout) != "out\n" || string(result.Stderr) != "err\n" {
		t.Fatalf("run = %+v, %v", result, err)
	}
	_, err = startProcess(processSpec{Argv: []string{"/bin/sh", "-c", "sleep 30 & wait"}, Capture: true, Timeout: 200 * time.Millisecond})
	var timeout *processTimeoutError
	if !errors.As(err, &timeout) || !strings.Contains(err.Error(), "timed out after 0.2 seconds") {
		t.Fatalf("timeout = %v", err)
	}
	missing := filepath.Join(t.TempDir(), "absent")
	_, err = startProcess(processSpec{Argv: []string{missing}})
	if got := pyOSErrorText(err); got != "[Errno 2] No such file or directory: "+pyRepr(missing) {
		t.Fatalf("spawn failure = %q", got)
	}
}

func TestCatCopy2KeepsModeAndTimes(t *testing.T) {
	tmp := t.TempDir()
	src := filepath.Join(tmp, "src")
	writeFile(t, src, []byte("payload"), 0o750)
	stamp := time.Unix(1700000000, 123456789)
	if err := os.Chtimes(src, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	dir := sttMkdir(t, filepath.Join(tmp, "dir"))
	if err := copy2(src, dir); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(filepath.Join(dir, "src"))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && st.Mode().Perm() != 0o750 {
		t.Fatalf("mode = %v", st.Mode())
	}
	if !st.ModTime().Equal(stamp) {
		t.Fatalf("mtime = %v", st.ModTime())
	}
	if string(readFile(t, filepath.Join(dir, "src"))) != "payload" {
		t.Fatal("content differs")
	}
}

func TestCatUvStaysInternalWhileNodeAndNpmShip(t *testing.T) {
	if !catPackage(t, "uv").Base().Internal {
		t.Fatal("uv is not internal")
	}
	for _, name := range []string{"node", "npm"} {
		if catPackage(t, name).Base().Internal {
			t.Fatalf("%s is internal", name)
		}
	}
}

func TestCatBrowserDefaultIsFullChromiumOnly(t *testing.T) {
	closure, err := Walk([]string{"chromium"})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range closure {
		if p.Base().Name == "chromium-headless-shell" {
			t.Fatal("the browser closure carries the headless shell")
		}
	}
	if !slices.Contains(DefaultPackageNames(), "chromium") {
		t.Fatal("chromium is not a default package")
	}
	if _, err := GetPackage("chromium-headless-shell"); err == nil {
		t.Fatal("a headless shell package is registered")
	}
}
