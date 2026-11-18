//go:build !icebath_core && !icebath_noextras

package pm

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Optional Python feature sets with their requirement specs and hash pinned resolutions for every target
//
//go:embed extras_lock.json
var extrasLockJSON []byte

// One locked artifact, its download address and digest
type LockedFile struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}

// One package of a resolved extra with the targets its marker admits and every artifact the lock allows
type LockedPackage struct {
	Name    string       `json:"name"`
	Version string       `json:"version"`
	Marker  string       `json:"marker,omitempty"`
	Index   string       `json:"index,omitempty"`
	Wheels  []LockedFile `json:"wheels,omitempty"`
	Sdist   *LockedFile  `json:"sdist,omitempty"`
}

// Package index a requirement of an extra resolves from, limited to the platforms its marker admits
type ExtraSource struct {
	Index  string `json:"index"`
	Marker string `json:"marker,omitempty"`
}

// One optional feature set, its hand written spec and the resolution pm lock writes beside it
type ExtraEntry struct {
	Feature     string                 `json:"feature"`
	Aliases     []string               `json:"aliases,omitempty"`
	Anchors     []string               `json:"anchors"`
	Requires    []string               `json:"requires"`
	Sources     map[string]ExtraSource `json:"sources,omitempty"`
	Tools       []string               `json:"tools,omitempty"`
	Unsupported map[string]string      `json:"unsupported,omitempty"`
	Build       map[string][]string    `json:"build,omitempty"`
	Packages    []LockedPackage        `json:"packages"`
	BuildDeps   []LockedPackage        `json:"build_requires,omitempty"`
}

// Lock file of every extra, the interpreter line its resolutions target
type extrasLockFile struct {
	Schema int                    `json:"schema"`
	Python string                 `json:"python"`
	Extras map[string]*ExtraEntry `json:"extras"`
}

var extrasLockOnce = sync.OnceValues(func() (*extrasLockFile, error) { return parseExtrasLock(extrasLockJSON) })

// Decoded lock file, every extra keyed by its canonical name
func parseExtrasLock(data []byte) (*extrasLockFile, error) {
	var lock extrasLockFile
	if err := json.Unmarshal(data, &lock); err != nil {
		return nil, fmt.Errorf("extras lock: %w", err)
	}
	if lock.Extras == nil {
		lock.Extras = map[string]*ExtraEntry{}
	}
	return &lock, nil
}

// Embedded extras lock, a broken embed failing every caller the same way
func extrasLock() *extrasLockFile {
	lock, err := extrasLockOnce()
	if err != nil {
		panic(err)
	}
	return lock
}

// Canonical names of every extra the binary carries, sorted
func KnownExtras() []string {
	return sortedKeys(extrasLock().Extras)
}

// Canonical extra a name or alias selects, false for a name the lock does not carry
func ResolveExtra(name string) (string, *ExtraEntry, bool) {
	key := ExtraKey(name)
	lock := extrasLock()
	if entry, ok := lock.Extras[key]; ok {
		return key, entry, true
	}
	for _, canonical := range sortedKeys(lock.Extras) {
		for _, alias := range lock.Extras[canonical].Aliases {
			if ExtraKey(alias) == key {
				return canonical, lock.Extras[canonical], true
			}
		}
	}
	return "", nil, false
}

// Refusal of an extra name the binary does not carry, listing the names it does
func UnknownExtraError(name string) *InstallError {
	return NewInstallError(name, "no extra of that name", "known extras are "+strings.Join(KnownExtras(), ", "))
}

// Interpreter version the extras resolve against, major dot minor and the full release
func extrasPythonVersion() (string, string, error) {
	version, ok := currentLockfile().Version("python")
	if !ok {
		return "", "", fmt.Errorf("pm lock pins no python")
	}
	full, _, _ := strings.Cut(version, "+")
	major, rest, _ := strings.Cut(full, ".")
	minor, _, _ := strings.Cut(rest, ".")
	return major + "." + minor, full, nil
}

// Marker values of a target under the pinned interpreter
func targetMarkerEnvironment(target string) (map[string]string, error) {
	short, full, err := extrasPythonVersion()
	if err != nil {
		return nil, err
	}
	env := map[string]string{"python_version": short, "python_full_version": full, "implementation_name": "cpython",
		"implementation_version": full, "platform_python_implementation": "CPython", "platform_release": "", "platform_version": ""}
	switch {
	case strings.HasPrefix(target, "linux-"):
		env["sys_platform"], env["platform_system"], env["os_name"] = "linux", "Linux", "posix"
		env["platform_machine"] = map[bool]string{true: "aarch64", false: "x86_64"}[strings.HasPrefix(target, "linux-arm64")]
		if strings.HasSuffix(target, "-bionic") {
			env["platform_release"] = "android"
		}
	case strings.HasPrefix(target, "darwin-"):
		env["sys_platform"], env["platform_system"], env["os_name"] = "darwin", "Darwin", "posix"
		env["platform_machine"] = map[bool]string{true: "arm64", false: "x86_64"}[target == "darwin-arm64"]
	case strings.HasPrefix(target, "win32-"):
		env["sys_platform"], env["platform_system"], env["os_name"] = "win32", "Windows", "nt"
		env["platform_machine"] = map[bool]string{true: "ARM64", false: "AMD64"}[target == "win32-arm64"]
	default:
		return nil, fmt.Errorf("unknown target %s", target)
	}
	return env, nil
}

// Whether a package marker admits a target, an empty marker admitting every target
func markerAdmits(marker, target string) (bool, error) {
	if strings.TrimSpace(marker) == "" {
		return true, nil
	}
	parsed, err := parseMarker(marker)
	if err != nil {
		return false, err
	}
	env, err := targetMarkerEnvironment(target)
	if err != nil {
		return false, err
	}
	return evaluateMarker(parsed, nil, env)
}

// Targets a wheel serves under the pinned interpreter, read from the compatibility tags of its file name
func wheelTargets(file string) []string {
	name := file
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	name = strings.ReplaceAll(name, "%2B", "+")
	parts := strings.Split(strings.TrimSuffix(name, ".whl"), "-")
	if len(parts) < 5 || !strings.HasSuffix(name, ".whl") {
		return nil
	}
	pyTags, abi, platforms := parts[len(parts)-3], parts[len(parts)-2], parts[len(parts)-1]
	short, _, err := extrasPythonVersion()
	if err != nil {
		return nil
	}
	own := strings.ReplaceAll(short, ".", "")
	ownMinor, _ := strconv.Atoi(strings.TrimPrefix(own, "3"))
	pyOK := false
	for _, abiTag := range strings.Split(abi, ".") {
		for _, tag := range strings.Split(pyTags, ".") {
			switch {
			case abiTag == "none" && (tag == "py3" || tag == "py"+own || tag == "cp"+own):
				pyOK = true
			case abiTag == "cp"+own && tag == "cp"+own:
				pyOK = true
			case abiTag == "abi3" && strings.HasPrefix(tag, "cp3"):
				if minor, err := strconv.Atoi(strings.TrimPrefix(tag, "cp3")); err == nil && minor <= ownMinor {
					pyOK = true
				}
			}
		}
	}
	if !pyOK {
		return nil
	}
	served := map[string]bool{}
	for _, platform := range strings.Split(platforms, ".") {
		for _, target := range platformTagTargets(platform) {
			served[target] = true
		}
	}
	return sortedKeys(served)
}

// Targets one wheel platform tag runs on
func platformTagTargets(tag string) []string {
	arch := func(x64, arm64 string) []string {
		switch {
		case strings.HasSuffix(tag, "_x86_64"):
			return []string{x64}
		case strings.HasSuffix(tag, "_aarch64"):
			return []string{arm64}
		}
		return nil
	}
	switch {
	case tag == "any":
		return append([]string{}, AllTargets...)
	case strings.HasPrefix(tag, "manylinux") || tag == "linux_x86_64" || tag == "linux_aarch64":
		return arch("linux-x64", "linux-arm64")
	case strings.HasPrefix(tag, "musllinux"):
		return arch("linux-x64-musl", "linux-arm64-musl")
	case strings.HasPrefix(tag, "android_") && strings.HasSuffix(tag, "_arm64_v8a"):
		return []string{"linux-arm64-bionic"}
	case strings.HasPrefix(tag, "macosx_"):
		switch {
		case strings.HasSuffix(tag, "_universal2"):
			return []string{"darwin-arm64", "darwin-x64"}
		case strings.HasSuffix(tag, "_arm64"):
			return []string{"darwin-arm64"}
		case strings.HasSuffix(tag, "_x86_64") || strings.HasSuffix(tag, "_intel"):
			return []string{"darwin-x64"}
		}
	case tag == "win_amd64":
		return []string{"win32-x64"}
	case tag == "win_arm64":
		return []string{"win32-arm64"}
	}
	return nil
}

// Packages of an extra a target installs with the wheels serving that target
func (e *ExtraEntry) packagesFor(target string) ([]LockedPackage, error) {
	return packagesServing(e.Packages, target)
}

// Build requirements of an extra's source builds on a target with the artifacts serving that target
func (e *ExtraEntry) buildDepsFor(target string) ([]LockedPackage, error) {
	return packagesServing(e.BuildDeps, target)
}

// Packages whose marker admits the target, each keeping its sdist and the wheels serving that target
func packagesServing(packages []LockedPackage, target string) ([]LockedPackage, error) {
	var out []LockedPackage
	for _, p := range packages {
		admitted, err := markerAdmits(p.Marker, target)
		if err != nil {
			return nil, err
		}
		if !admitted {
			continue
		}
		kept := p
		kept.Wheels = nil
		for _, w := range p.Wheels {
			if containsString(wheelTargets(w.URL), target) {
				kept.Wheels = append(kept.Wheels, w)
			}
		}
		out = append(out, kept)
	}
	return out, nil
}

// Quoted TOML basic string
func tomlQuote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s) + `"`
}

// Lock document uv installs exactly, each package with only the artifacts serving this target
func pylockDocument(short string, packages []LockedPackage) []byte {
	var b strings.Builder
	b.WriteString("lock-version = \"1.0\"\ncreated-by = \"icebath\"\nrequires-python = " + tomlQuote("=="+short+".*") + "\n")
	file := func(f LockedFile) string {
		return "{ url = " + tomlQuote(f.URL) + ", hashes = { sha256 = " + tomlQuote(f.SHA256) + " } }"
	}
	for _, p := range packages {
		b.WriteString("\n[[packages]]\nname = " + tomlQuote(p.Name) + "\nversion = " + tomlQuote(p.Version) + "\n")
		if p.Marker != "" {
			b.WriteString("marker = " + tomlQuote(p.Marker) + "\n")
		}
		if p.Index != "" {
			b.WriteString("index = " + tomlQuote(p.Index) + "\n")
		}
		if p.Sdist != nil {
			b.WriteString("sdist = " + file(*p.Sdist) + "\n")
		}
		if len(p.Wheels) > 0 {
			rows := make([]string, len(p.Wheels))
			for i, w := range p.Wheels {
				rows[i] = "    " + file(w)
			}
			b.WriteString("wheels = [\n" + strings.Join(rows, ",\n") + ",\n]\n")
		}
	}
	return []byte(b.String())
}

// Build constraints file pinning every build requirement by version and the hashes of its artifacts
func buildConstraintsText(packages []LockedPackage) []byte {
	var b strings.Builder
	for _, p := range packages {
		b.WriteString(p.Name + "==" + p.Version)
		files := append([]LockedFile{}, p.Wheels...)
		if p.Sdist != nil {
			files = append(files, *p.Sdist)
		}
		for _, f := range files {
			b.WriteString(" \\\n    --hash=sha256:" + f.SHA256)
		}
		b.WriteString("\n")
	}
	return []byte(b.String())
}

// Compiler suites per system, what an extra that builds from source needs installed first
var compilerInstall = []string{
	"Debian or Ubuntu   sudo apt install build-essential",
	"Fedora or RHEL     sudo dnf install gcc gcc-c++ make",
	"Arch               sudo pacman -S base-devel",
	"openSUSE           sudo zypper install gcc gcc-c++ make",
	"Alpine             sudo apk add build-base",
	"macOS              xcode-select --install",
	"Windows            winget install --id Microsoft.VisualStudio.2022.BuildTools --override \"--add Microsoft.VisualStudio.Workload.VCTools --includeRecommended --passive\"",
}

// Whether this machine has a C and a C++ compiler a source build can call
var compilerPresent = func() bool {
	switch runtime.GOOS {
	case "windows":
		if _, err := exec.LookPath("cl.exe"); err == nil {
			return true
		}
		component := "Microsoft.VisualStudio.Component.VC.Tools.x86.x64"
		if runtime.GOARCH == "arm64" {
			component = "Microsoft.VisualStudio.Component.VC.Tools.ARM64"
		}
		vswhere := filepath.Join(os.Getenv("ProgramFiles(x86)"), "Microsoft Visual Studio", "Installer", "vswhere.exe")
		out, err := exec.Command(vswhere, "-latest", "-products", "*", "-requires", component, "-property", "installationPath").Output()
		return err == nil && strings.TrimSpace(string(out)) != ""
	case "darwin":
		return exec.Command("xcode-select", "-p").Run() == nil
	}
	has := func(names ...string) bool {
		for _, name := range names {
			if _, err := exec.LookPath(name); err == nil {
				return true
			}
		}
		return false
	}
	return has(os.Getenv("CC"), "cc", "gcc", "clang") && has(os.Getenv("CXX"), "c++", "g++", "clang++")
}

// Git install command per system, the one build tool pm leaves to the system outside Windows
var gitInstall = []string{
	"Debian or Ubuntu   sudo apt install git",
	"Fedora or RHEL     sudo dnf install git",
	"Arch               sudo pacman -S git",
	"openSUSE           sudo zypper install git",
	"Alpine             sudo apk add git",
	"macOS              xcode-select --install",
}

// Whether a build tool comes from the system on this platform rather than from pm
func systemBuildTool(tool string) bool {
	return tool == "git" && runtime.GOOS != "windows"
}

// Lines naming one install command per system
func installLines(lines []string) string {
	return "\n      " + strings.Join(lines, "\n      ")
}

// Refusal of an extra this target cannot carry or whose source builds need a compiler this machine lacks
func extraPreflight(name string, entry *ExtraEntry, target string, out io.Writer) error {
	if reason, ok := entry.Unsupported[target]; ok {
		return NewInstallError(name, entry.Feature+" cannot be installed on "+target+": "+reason, "use a platform the extra supports")
	}
	if builds := entry.Build[target]; len(builds) > 0 {
		sorted := append([]string{}, builds...)
		sort.Strings(sorted)
		if !compilerPresent() {
			return NewInstallError(name, fmt.Sprintf("%s compiles %s from source on %s and this machine has no C and C++ compiler", entry.Feature, strings.Join(sorted, ", "), target),
				"install a compiler, then run `"+InstallHint(name)+"` again"+installLines(compilerInstall))
		}
		fmt.Fprintf(out, "  %s builds %s from source with this machine's compiler\n", name, strings.Join(sorted, ", "))
	}
	for _, tool := range entry.Tools {
		if !systemBuildTool(tool) {
			fmt.Fprintf(out, "  %s builds with icebath's own %s\n", name, tool)
			continue
		}
		found, err := exec.LookPath(tool)
		if err != nil {
			return NewInstallError(name, fmt.Sprintf("%s fetches sources with %s while it builds and this machine has no %s", entry.Feature, tool, tool),
				"install it, then run `"+InstallHint(name)+"` again"+installLines(gitInstall))
		}
		fmt.Fprintf(out, "  %s builds with %s\n", name, found)
	}
	return nil
}
