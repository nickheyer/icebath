package pm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Distro libatomic repair Node's staged verification asks for, replaced by tests
var tryInstallLibatomic = TryInstallLibatomic

// Uv is astral's prebuilt uv everywhere upstream builds it and the termux main repo deb on bionic
type Uv struct {
	bionicDebArm
}

func newUv() *Uv {
	p := &Uv{bionicDebArm{
		BinaryPackage: BinaryPackage{
			BasePackage: BasePackage{Name: "uv", Deps: []string{"python"}, Internal: true, OffPath: true},
			BinaryRel:   map[string]string{"win32": "uv.exe", "posix": "uv"},
		},
		DebName:    "uv",
		MainBinRel: "bin/uv",
	}}
	p.bind(p)
	return p
}

func (p *Uv) FetchURL(version, target string) (string, error) {
	if target == bionicTarget {
		return fmt.Sprintf("https://packages.termux.dev/apt/termux-main/pool/main/u/uv/uv_%s_aarch64.deb", version), nil
	}
	triple, ok := rustTriple[target]
	if !ok {
		return "", missingKey("no download for target " + target)
	}
	ext := "tar.gz"
	if strings.HasPrefix(target, "win32") {
		ext = "zip"
	}
	return fmt.Sprintf("https://github.com/astral-sh/uv/releases/download/%s/uv-%s.%s", version, triple, ext), nil
}

// POSIX interpreter name the pinned Windows Python also answers to
const WindowsPython3 = "python3.exe"

// Python is the pinned interpreter for launchers and every PM managed uv command
type Python struct {
	bionicDebArm
	BionicURL string
}

func newPython() *Python {
	p := &Python{
		bionicDebArm: bionicDebArm{
			BinaryPackage: BinaryPackage{
				BasePackage: BasePackage{Name: "python", Optional: true},
				BinaryRel:   map[string]string{"win32": "python.exe", "posix": "bin/python3"},
				NoProbe:     true,
			},
			DebName:    "python",
			MainBinRel: "bin/python3.14",
		},
		BionicURL: "https://packages.termux.dev/apt/termux-main/pool/main/p/python/python_3.14.10_aarch64.deb",
	}
	p.bind(p)
	return p
}

func (p *Python) Verify(entry, target string) string {
	reason := p.bionicDebArm.Verify(entry, target)
	if reason != "" || !strings.HasPrefix(target, "win32") {
		return reason
	}
	alias, aliasErr := os.ReadFile(filepath.Join(entry, WindowsPython3))
	if aliasErr == nil {
		original, err := os.ReadFile(filepath.Join(entry, "python.exe"))
		if err == nil && bytes.Equal(alias, original) {
			return ""
		}
		aliasErr = err
	}
	if aliasErr != nil {
		return WindowsPython3 + " is missing beside python.exe"
	}
	return WindowsPython3 + " differs from python.exe"
}

func (p *Python) FetchURL(version, target string) (string, error) {
	if target == bionicTarget {
		return p.BionicURL, nil
	}
	pyver, tag := partition(version, "+")
	if tag == "" {
		return "", NewInstallError(p.Name, fmt.Sprintf("version %s needs the +<release> tag", version), "")
	}
	triple, ok := rustTriple[target]
	if !ok {
		return "", missingKey("no download for target " + target)
	}
	return "https://github.com/astral-sh/python-build-standalone/releases/download/" +
		fmt.Sprintf("%s/cpython-%s+%s-%s-install_only.tar.gz", tag, pyver, tag, triple), nil
}

// Nodejs is nodejs.org for glibc, mac and Windows, unofficial builds for musl and the termux deb on bionic
type Nodejs struct {
	bionicDebArm
}

func newNodejs() *Nodejs {
	p := &Nodejs{bionicDebArm{
		BinaryPackage: BinaryPackage{
			BasePackage: BasePackage{Name: "node"},
			BinaryRel:   map[string]string{"win32": "node.exe", "posix": "bin/node"},
		},
		DebName:    "nodejs",
		MainBinRel: "bin/node",
	}}
	p.bind(p)
	return p
}

func (p *Nodejs) FetchURL(version, target string) (string, error) {
	if target == bionicTarget {
		return fmt.Sprintf("https://packages.termux.dev/apt/termux-main/pool/main/n/nodejs/nodejs_%s_aarch64.deb", version), nil
	}
	plat, ok := nodePlat[target]
	if !ok {
		return "", missingKey("no download for target " + target)
	}
	ext := "tar.xz"
	if strings.HasPrefix(target, "win32") {
		ext = "zip"
	}
	base := "https://nodejs.org/dist"
	if MuslTargets[target] {
		base = "https://unofficial-builds.nodejs.org/download/release"
	}
	return fmt.Sprintf("%s/v%s/node-v%s-%s.%s", base, version, version, plat, ext), nil
}

// Installs the host libatomic official Linux Node needs, then re-probes, never from inspection paths
func (p *Nodejs) RepairStagedVerification(entry, target, reason string) (string, string) {
	current, err := packagesCurrentTarget()
	if err != nil {
		return reason + "; " + err.Error(), ""
	}
	if target != current || !strings.HasPrefix(target, "linux") || !strings.Contains(reason, "libatomic.so.1") {
		return reason, ""
	}
	installed, remedy := tryInstallLibatomic(context.Background())
	if !installed {
		return reason, remedy
	}
	retried := p.dispatch().Verify(entry, target)
	if strings.Contains(retried, "libatomic.so.1") {
		return retried, "the libatomic package installed, but Node still cannot load libatomic.so.1; check the loader path"
	}
	return retried, ""
}

// TermuxDocker is the termux docker image pinned by registry digest, a pin pm never downloads
type TermuxDocker struct {
	BasePackage
}

func newTermuxDocker() *TermuxDocker {
	p := &TermuxDocker{BasePackage{Name: "termux-docker", Optional: true}}
	p.bind(p)
	return p
}

func (p *TermuxDocker) PinOnly() bool { return true }

func (p *TermuxDocker) MissingReason(target string) string {
	if target == bionicTarget {
		return ""
	}
	return "docker image target is linux-arm64-bionic"
}

func (p *TermuxDocker) FetchURL(version, target string) (string, error) {
	return "docker://termux/termux-docker@" + version, nil
}

func (p *TermuxDocker) Unpack(archive, staged, target string) error {
	return NewInstallError(p.Name, "a docker image digest is a pin, not a downloadable artifact", "")
}

func (p *TermuxDocker) Verify(entry, target string) string { return "" }

// Environment for PM's npm child with ambient Node and npm options removed and its cache pinned
func NpmEnv(cacheDir string, base map[string]string) map[string]string {
	if base == nil {
		base = environMap()
	}
	env := map[string]string{}
	for key, value := range base {
		upper := strings.ToUpper(key)
		if strings.HasPrefix(strings.ToLower(key), "npm_config_") || upper == "NODE_OPTIONS" || upper == "NODE_PATH" || upper == "NODE_ENV" {
			continue
		}
		env[key] = value
	}
	env["npm_config_cache"] = cacheDir
	return env
}

// Lockfile the catalogue reads installed locations against, replaced by tests
var catalogueLockfile = currentLockfile

// Installed location of a package for the catalogue's staging steps, replaced by tests
var catalogueInstalledLocation = func(p Package, lock *Lockfile, target string) (*Location, error) {
	return InstalledLocation(context.Background(), p, lock, target, LocationOptions{})
}

// Npm installs itself with the node it extends, offline from the verified tarball
type Npm struct {
	BinaryPackage
}

func newNpm() *Npm {
	p := &Npm{BinaryPackage{
		BasePackage: BasePackage{Name: "npm", Deps: []string{"node"}, URL: "https://registry.npmjs.org/npm/-/npm-{version}.tgz"},
		BinaryRel:   map[string]string{"win32": "npm.cmd", "posix": "bin/npm"},
		NoFlatten:   true,
		NoProbe:     true,
	}}
	p.bind(p)
	return p
}

// Bionic gets the package tree plus termux shell wrappers, every other target runs npm's self install
func (p *Npm) Unpack(archive, staged, target string) error {
	if target == bionicTarget {
		return p.unpackBionic(archive, staged)
	}
	node, err := GetPackage("node")
	if err != nil {
		return err
	}
	location, err := catalogueInstalledLocation(node, catalogueLockfile(), target)
	if err != nil {
		return err
	}
	if location == nil {
		return NewInstallError(p.Name, "npm extends node, which is not installed", "")
	}
	fact, ok := location.Facts.Get("node")
	if !ok || fact == nil {
		return NewInstallError(p.Name, "npm extends node, which is not installed", "")
	}
	entryName, present := fact["entry"]
	if !present {
		return missingKey("the node install record has no entry")
	}
	nodeBin := node.Binary(location.Store.Entry(pyrepr.Str(entryName)), target)
	if nodeBin == "" || !isRegularFile(nodeBin) {
		return NewInstallError(p.Name, "node's entry is missing its binary", "")
	}
	bundledCLI := filepath.Join(filepath.Dir(filepath.Dir(nodeBin)), "lib", "node_modules", "npm", "bin", "npm-cli.js")
	if strings.HasPrefix(target, "win32") {
		bundledCLI = filepath.Join(filepath.Dir(nodeBin), "node_modules", "npm", "bin", "npm-cli.js")
	}
	if !isRegularFile(bundledCLI) {
		return NewInstallError(p.Name, "node's entry is missing its bundled npm-cli.js", "")
	}
	if err := os.MkdirAll(staged, 0o777); err != nil {
		return err
	}
	cache, err := os.MkdirTemp("", "icebath-npm-cache-")
	if err != nil {
		return err
	}
	result, runErr := runProcess(processSpec{
		Argv: []string{Native(nodeBin), Native(bundledCLI), "install", "--global", "--prefix", Native(staged), "--offline",
			"--ignore-scripts", "--no-audit", "--no-fund", Native(archive)},
		Env: NpmEnv(cache, nil), Capture: true, Timeout: 900 * time.Second,
	})
	_ = os.RemoveAll(cache)
	if runErr != nil {
		return runErr
	}
	for _, stream := range [][]byte{result.Stdout, result.Stderr} {
		if err := strictUTF8(stream); err != nil {
			return fmt.Errorf("the npm install output is %w", err)
		}
	}
	if result.Code != 0 {
		return NewInstallError(p.Name, fmt.Sprintf("self-install exited %d: %s", result.Code, lastRunes(universalNewlines(string(result.Stderr)), 400)), "")
	}
	return nil
}

// Moves the tarball's package tree into node_modules and writes termux shell wrappers for npm and npx
func (p *Npm) unpackBionic(archive, staged string) error {
	unpacked := filepath.Join(staged, ".unpacked")
	if err := Extract(archive, unpacked); err != nil {
		return err
	}
	lib := filepath.Join(staged, "lib", "node_modules", "npm")
	if err := os.MkdirAll(filepath.Dir(lib), 0o777); err != nil {
		return err
	}
	if err := os.Rename(filepath.Join(unpacked, "package"), lib); err != nil {
		return err
	}
	if err := os.Remove(unpacked); err != nil {
		return err
	}
	bindir := filepath.Join(staged, "bin")
	if err := os.Mkdir(bindir, 0o777); err != nil {
		return err
	}
	for _, name := range []string{"npm", "npx"} {
		wrapper := filepath.Join(bindir, name)
		script := "#!/data/data/com.termux/files/usr/bin/sh\n" +
			"here=\"$(cd \"$(dirname \"$0\")\" && pwd)\"\n" +
			fmt.Sprintf("exec node \"$here/../lib/node_modules/npm/bin/%s-cli.js\" \"$@\"\n", name)
		if err := os.WriteFile(wrapper, []byte(script), 0o666); err != nil {
			return err
		}
		if err := os.Chmod(wrapper, 0o755); err != nil {
			return err
		}
	}
	return nil
}

// Managed interpreter signing on macOS, replaced by tests
var signManagedPython = SignManagedPython

// Signs on macOS, drops the unloadable x64 VC runtime on ARM64 Windows and stages the python3 copy on Windows
func (p *Python) Stage(store *Store, staged, version, target string) error {
	if err := p.bionicDebArm.Stage(store, staged, version, target); err != nil {
		return err
	}
	if bin := p.dispatch().Binary(staged, target); bin != "" && runtime.GOOS == "darwin" {
		signManagedPython(bin)
	}
	if target == "win32-arm64" {
		if err := os.Remove(filepath.Join(staged, "vcruntime140_1.dll")); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if strings.HasPrefix(target, "win32") {
		return copy2(filepath.Join(staged, "python.exe"), filepath.Join(staged, WindowsPython3))
	}
	return nil
}
