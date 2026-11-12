package pm

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Whether this host executes PE images, the only host able to run PortableGit's extractor, replaced by tests
var hostIsWindows = runtime.GOOS == "windows"

// Git is Git for Windows carrying the bash contract, POSIX targets using the system git by choice
type Git struct {
	BinaryPackage
}

func newGit() *Git {
	gaps := map[string]string{}
	for _, target := range []string{"linux-x64", "linux-arm64", "linux-x64-musl", "linux-arm64-musl", "darwin-x64", "darwin-arm64"} {
		gaps[target] = "POSIX uses system git by choice"
	}
	p := &Git{BinaryPackage{
		BasePackage: BasePackage{Name: "git", Optional: true, Gaps: gaps},
		BinaryRel:   map[string]string{"win32": "cmd/git.exe"},
		NoFlatten:   true,
	}}
	p.bind(p)
	return p
}

func (p *Git) FetchURL(version, target string) (string, error) {
	tag, build, err := unpackTwo(strings.Split(version, "+"))
	if err != nil {
		return "", err
	}
	arch := "64-bit"
	if strings.HasSuffix(target, "arm64") {
		arch = "arm64"
	}
	return "https://github.com/git-for-windows/git/releases/download/" +
		fmt.Sprintf("v%s.windows.%s/PortableGit-%s.%s-%s.7z.exe", tag, build, tag, build, arch), nil
}

// Release tags like v2.53.0.windows.3 become lock versions like 2.53.0+3
func (p *Git) LatestVersions(target, locked string) ([]string, error) {
	tags, err := GithubReleaseTags("git-for-windows/git", "v")
	if err != nil {
		return nil, err
	}
	var out []string
	for _, tag := range tags {
		if base, build, found := strings.Cut(tag, ".windows."); found {
			out = append(out, base+"+"+build)
		}
	}
	return out, nil
}

// Runs a scratch copy of the PortableGit self extractor into the staged tree, refusing hosts that cannot run PE images
func (p *Git) Unpack(archive, staged, target string) error {
	if !hostIsWindows {
		return NewInstallError(p.Name,
			"the pinned artifact is a PortableGit self-extracting 7z, which only runs on Windows",
			"stage win32 git on a Windows host; POSIX hosts use the system git")
	}
	_ = os.RemoveAll(staged)
	if err := os.MkdirAll(filepath.Dir(staged), 0o777); err != nil {
		return err
	}
	if err := os.Mkdir(staged, 0o777); err != nil {
		return err
	}
	work, err := os.MkdirTemp(filepath.Dir(staged), ".sfx-")
	if err != nil {
		return err
	}
	code, runErr := func() (int, error) {
		defer os.RemoveAll(work)
		exe := filepath.Join(work, filepath.Base(archive))
		if err := copy2(archive, exe); err != nil {
			return 0, err
		}
		result, err := runProcess(processSpec{
			Argv: []string{Native(exe), "-o" + Native(staged), "-y"}, NullStdin: true, Timeout: 600 * time.Second,
		})
		var timeout *processTimeoutError
		if errors.As(err, &timeout) {
			return 0, NewInstallError(p.Name, "PortableGit self-extractor did not finish in 600 s", "")
		}
		return result.Code, err
	}()
	if runErr != nil {
		return runErr
	}
	if code != 0 {
		return NewInstallError(p.Name, fmt.Sprintf("PortableGit self-extractor exited %d (it reports nothing under -y; "+
			"usual causes: disk full, path-length limit, antivirus lock)", code), "")
	}
	return nil
}

func (p *Git) Env(entry, target string) EnvDiff {
	return EnvDiff{"PATH": []string{filepath.Join(entry, "cmd"), filepath.Join(entry, "usr", "bin")}}
}

// Gh is the GitHub CLI, whose libc independent Linux archive serves musl too
type Gh struct {
	BinaryPackage
}

func newGh() *Gh {
	p := &Gh{BinaryPackage{
		BasePackage: BasePackage{Name: "gh", Optional: true},
		BinaryRel:   map[string]string{"win32": "bin/gh.exe", "posix": "bin/gh"},
	}}
	p.bind(p)
	return p
}

func (p *Gh) FetchURL(version, target string) (string, error) {
	lookup := target
	if MuslTargets[target] {
		lookup = strings.TrimSuffix(target, "-musl")
	}
	osname, arch, err := unpackTwo(strings.Split(lookup, "-"))
	if err != nil {
		return "", err
	}
	plat, ok := map[string]string{"win32": "windows", "linux": "linux", "darwin": "macOS"}[osname]
	if !ok {
		return "", keyError(osname)
	}
	archName, ok := map[string]string{"x64": "amd64", "arm64": "arm64"}[arch]
	if !ok {
		return "", keyError(arch)
	}
	ext := "tar.gz"
	if osname == "win32" || osname == "darwin" {
		ext = "zip"
	}
	return fmt.Sprintf("https://github.com/cli/cli/releases/download/v%s/gh_%s_%s_%s.%s", version, version, plat, archName, ext), nil
}

func (p *Gh) LatestVersions(target, locked string) ([]string, error) {
	return GithubReleaseTags("cli/cli", "v")
}

// Ffmpeg is BtbN on Windows and Linux, martin-riedl on macOS and the termux deb on bionic
type Ffmpeg struct {
	bionicDebArm
}

func newFfmpeg() *Ffmpeg {
	gaps := map[string]string{}
	for target := range MuslTargets {
		gaps[target] = "BtbN Linux builds link glibc dynamically"
	}
	p := &Ffmpeg{bionicDebArm{
		BinaryPackage: BinaryPackage{
			BasePackage: BasePackage{Name: "ffmpeg", Gaps: gaps, VersionStyle: "minor"},
			BinaryRel: map[string]string{
				"win32": "bin/ffmpeg.exe", "linux-x64": "bin/ffmpeg", "linux-arm64": "bin/ffmpeg", "posix": "ffmpeg",
			},
			ProbeArgs: []string{"-version"},
		},
		DebName:    "ffmpeg",
		MainBinRel: "bin/ffmpeg",
	}}
	p.bind(p)
	return p
}

// Resolves the exact advertised asset for the version and target, refusing to relabel other bytes
func (p *Ffmpeg) FetchURL(version, target string) (string, error) {
	if target == bionicTarget {
		return fmt.Sprintf("https://packages.termux.dev/apt/termux-main/pool/main/f/ffmpeg/ffmpeg_%s_aarch64.deb", version), nil
	}
	if MuslTargets[target] {
		return "", NewInstallError(p.Name, fmt.Sprintf("unavailable on %s: %s", target, pyNoneStr(p.dispatch().MissingReason(target))), "")
	}
	osname, arch, err := unpackTwo(strings.Split(target, "-"))
	if err != nil {
		return "", err
	}
	if osname == "win32" || osname == "linux" {
		index, err := BtbnIndex()
		if err != nil {
			return "", err
		}
		if artifact, ok := index[target][version]; ok {
			return fmt.Sprintf("https://github.com/BtbN/FFmpeg-Builds/releases/download/%s/%s", artifact[0], artifact[1]), nil
		}
	} else if epoch, ok := MartinRiedlIndex()[target][version]; ok {
		sourceArch := arch
		if arch == "x64" {
			sourceArch = "amd64"
		}
		return fmt.Sprintf("https://ffmpeg.martin-riedl.de/download/macos/%s/%s_%s/ffmpeg.zip", sourceArch, epoch, version), nil
	}
	return "", NewInstallError(p.Name, fmt.Sprintf("no advertised %s artifact for %s", version, target),
		"retry when the upstream index is available, or keep the existing pin")
}

func (p *Ffmpeg) LatestVersions(target, locked string) ([]string, error) {
	if MuslTargets[target] {
		return nil, nil
	}
	switch target {
	case "win32-x64", "win32-arm64", "linux-x64", "linux-arm64":
		return BtbnVersions(target)
	}
	return MartinRiedlVersions(target), nil
}

// Python str of an optional string, None for the empty answer
func pyNoneStr(s string) string {
	if s == "" {
		return "None"
	}
	return s
}

// Ripgrep is BurntSushi's static build, ARM64 Windows carrying the VC runtime it links from python's entry
type Ripgrep struct {
	BinaryPackage
}

func newRipgrep() *Ripgrep {
	p := &Ripgrep{BinaryPackage{
		BasePackage: BasePackage{Name: "ripgrep", Deps: []string{"python"}},
		BinaryRel:   map[string]string{"win32": "rg.exe", "posix": "rg"},
	}}
	p.bind(p)
	return p
}

// Flattens, then on ARM64 Windows copies python's vcruntime140.dll beside rg.exe
func (p *Ripgrep) Stage(store *Store, staged, version, target string) error {
	if err := p.BinaryPackage.Stage(store, staged, version, target); err != nil {
		return err
	}
	if target != "win32-arm64" {
		return nil
	}
	python, err := GetPackage("python")
	if err != nil {
		return err
	}
	location, err := catalogueInstalledLocation(python, catalogueLockfile(), target)
	if err != nil {
		return err
	}
	var fact map[string]any
	if location != nil {
		fact, _ = location.Facts.Get("python")
	}
	if fact == nil {
		return NewInstallError(p.Name, "ARM64 rg.exe needs vcruntime140.dll from python, which is not installed", "")
	}
	entryName, present := fact["entry"]
	if !present {
		return keyError("entry")
	}
	vcRuntime := filepath.Join(location.Store.Entry(pyrepr.Str(entryName)), "vcruntime140.dll")
	if !isRegularFile(vcRuntime) {
		return NewInstallError(p.Name, "python's entry has no vcruntime140.dll", "")
	}
	return copy2(vcRuntime, filepath.Join(staged, "vcruntime140.dll"))
}

func (p *Ripgrep) Verify(entry, target string) string {
	if target == bionicTarget {
		return p.BasePackage.Verify(entry, target)
	}
	return p.BinaryPackage.Verify(entry, target)
}

func (p *Ripgrep) FetchURL(version, target string) (string, error) {
	if target == bionicTarget {
		target = "linux-arm64"
	}
	triple, ok := rustTriple[target]
	if !ok {
		return "", keyError(target)
	}
	triple = strings.ReplaceAll(triple, "-unknown-linux-gnu", "-unknown-linux-musl")
	ext := "tar.gz"
	if strings.HasPrefix(target, "win32") {
		ext = "zip"
	}
	return fmt.Sprintf("https://github.com/BurntSushi/ripgrep/releases/download/%s/ripgrep-%s-%s.%s", version, version, triple, ext), nil
}

func (p *Ripgrep) LatestVersions(target, locked string) ([]string, error) {
	return GithubReleaseTags("BurntSushi/ripgrep", "")
}

// Computer use driver child environment, set by the binary, an error where Python's cua_driver_child_env raises
var CuaDriverChildEnv func() (map[string]string, error)

// CuaDriver is the computer use driver, shipped by default wherever upstream builds it
type CuaDriver struct {
	BinaryPackage
}

func newCuaDriver() *CuaDriver {
	gaps := map[string]string{bionicTarget: "cua-driver does not publish an Android build"}
	for target := range MuslTargets {
		gaps[target] = "cua-driver does not publish a musl build"
	}
	p := &CuaDriver{BinaryPackage{
		BasePackage: BasePackage{Name: "cua-driver", Optional: true, Default: true, Gaps: gaps},
		BinaryRel: map[string]string{
			"darwin-arm64": "CuaDriver.app/Contents/MacOS/cua-driver",
			"darwin-x64":   "CuaDriver.app/Contents/MacOS/cua-driver",
			"win32":        "cua-driver.exe",
			"posix":        "cua-driver",
		},
	}}
	p.bind(p)
	return p
}

// macOS takes the directory archive carrying the signed app identity, other targets the binary archive
func (p *CuaDriver) FetchURL(version, target string) (string, error) {
	arch, ok := map[string]string{
		"darwin-x64": "darwin-universal", "darwin-arm64": "darwin-universal", "linux-x64": "linux-x86_64",
		"linux-arm64": "linux-arm64", "win32-x64": "windows-x86_64", "win32-arm64": "windows-arm64",
	}[target]
	if !ok {
		return "", keyError(target)
	}
	ext := "tar.gz"
	if strings.HasPrefix(target, "win32") {
		ext = "zip"
	}
	variant := "-binary"
	if strings.HasPrefix(target, "darwin") {
		variant = ""
	}
	return "https://github.com/trycua/cua/releases/download/" +
		fmt.Sprintf("cua-driver-rs-v%s/cua-driver-rs-%s-%s%s.%s", version, version, arch, variant, ext), nil
}

func (p *CuaDriver) LatestVersions(target, locked string) ([]string, error) {
	return GithubReleaseTags("trycua/cua", "cua-driver-rs-v")
}

// Hoists the release directory and drops the bundled SDK
func (p *CuaDriver) Stage(store *Store, staged, version, target string) error {
	if err := FlattenSingleDir(staged); err != nil {
		return err
	}
	if sdk := filepath.Join(staged, "sdk"); isDirectory(sdk) {
		_ = os.RemoveAll(sdk)
	}
	return nil
}

// The probe is a first run, so it never mints telemetry state
func (p *CuaDriver) ProbeEnv() (map[string]string, error) {
	if env, err := CuaDriverChildEnv(); err == nil {
		return env, nil
	}
	env := environMap()
	env["CUA_DRIVER_RS_TELEMETRY_ENABLED"] = "0"
	return env, nil
}

// Chromium is the Chrome for Testing or Playwright mirror build, laid out the way Playwright resolves browsers
type Chromium struct {
	BasePackage
	CDN    string
	CfT    map[string]string
	Mirror map[string]string
}

func newChromium() *Chromium {
	gaps := map[string]string{bionicTarget: "no Chromium build for Android/Termux"}
	for target := range MuslTargets {
		gaps[target] = "Playwright/Chrome-for-Testing publishes no musl build"
	}
	p := &Chromium{
		BasePackage: BasePackage{Name: "chromium", Optional: true, Default: true, OffPath: true, Gaps: gaps,
			EmulatedArchTargets: []string{"win32-arm64"}},
		CDN: "https://cdn.playwright.dev",
		CfT: map[string]string{
			"linux-x64": "linux64", "darwin-x64": "mac-x64", "darwin-arm64": "mac-arm64", "win32-x64": "win64", "win32-arm64": "win64",
		},
		Mirror: map[string]string{"linux-arm64": "linux-arm64"},
	}
	p.bind(p)
	return p
}

// Entry named by Playwright revision only, all its resolver reads
func (p *Chromium) StoreEntry(version, target string) string {
	revision, _ := partition(version, "+")
	return strings.ReplaceAll(p.Name, "-", "_") + "-" + revision
}

func (p *Chromium) FetchURL(version, target string) (string, error) {
	revision, chrome := partition(version, "+")
	if plat := p.CfT[target]; plat != "" && chrome != "" {
		return fmt.Sprintf("%s/builds/cft/%s/%s/chrome-%s.zip", p.CDN, chrome, plat, plat), nil
	}
	mirror, ok := p.Mirror[target]
	if !ok {
		return "", keyError(target)
	}
	return fmt.Sprintf("%s/dbazure/download/playwright/builds/chromium/%s/chromium-%s.zip", p.CDN, revision, mirror), nil
}

// First executable under the entry in path order, whichever enclosing directory the archive used
func (p *Chromium) Binary(entry, target string) string {
	names := map[string]bool{"chrome": true, "chromium": true, "Google Chrome for Testing": true, "Chromium": true}
	if strings.HasPrefix(target, "win32") {
		names = map[string]bool{"chrome.exe": true}
	}
	for _, path := range sortedDescendants(entry) {
		if names[filepath.Base(path)] && isRegularFile(path) {
			return path
		}
	}
	return ""
}

func (p *Chromium) Stage(store *Store, staged, version, target string) error {
	return os.WriteFile(filepath.Join(staged, "INSTALLATION_COMPLETE"), nil, 0o666)
}

func (p *Chromium) Verify(entry, target string) string {
	if !isRegularFile(filepath.Join(entry, "INSTALLATION_COMPLETE")) {
		return fmt.Sprintf("INSTALLATION_COMPLETE missing under %s; %s", Native(entry), entryListing(entry, 12))
	}
	bin := p.dispatch().Binary(entry, target)
	if bin == "" {
		return "Chromium executable missing under " + Native(entry)
	}
	return p.BinaryReason(bin, entry, target)
}

// Refuses an entry with no executable, the error Python's env raises
func (p *Chromium) CheckEnv(entry, target string) error {
	if p.dispatch().Binary(entry, target) == "" {
		return NewInstallError(p.Name, "Chromium executable missing under "+Native(entry), "")
	}
	return nil
}

// Points Playwright at the store root that holds the entry
func (p *Chromium) Env(entry, target string) EnvDiff {
	return EnvDiff{"PLAYWRIGHT_BROWSERS_PATH": filepath.Dir(entry)}
}

// Every path below root in pathlib sort order, never descending into directory symlinks like rglob
func sortedDescendants(root string) []string {
	var out []string
	var visit func(dir string)
	visit = func(dir string) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range entries {
			path := filepath.Join(dir, e.Name())
			out = append(out, path)
			if e.IsDir() {
				visit(path)
			}
		}
	}
	visit(root)
	sortPathlib(out)
	return out
}

// Sorts paths by their normcased component lists the way sorted() orders pathlib paths
func sortPathlib(paths []string) {
	keys := make(map[string][]string, len(paths))
	for _, path := range paths {
		keys[path] = strings.Split(filepath.ToSlash(osNormcase(path)), "/")
	}
	sort.SliceStable(paths, func(i, j int) bool {
		a, b := keys[paths[i]], keys[paths[j]]
		for k := 0; k < len(a) && k < len(b); k++ {
			if a[k] != b[k] {
				return a[k] < b[k]
			}
		}
		return len(a) < len(b)
	})
}
