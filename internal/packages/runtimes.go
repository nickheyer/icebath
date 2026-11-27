package packages

import (
	"bytes"
	"cmp"
	"context"
	_ "embed"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/pkg/fsx"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Pins of the runtimes the host declares itself
//
//go:embed runtimes.json
var runtimesLock []byte

// Node release platform of each target
var nodePlatform = map[string]string{
	"win32-x64":        "win-x64",
	"win32-arm64":      "win-arm64",
	"linux-x64":        "linux-x64",
	"linux-arm64":      "linux-arm64",
	"linux-x64-musl":   "linux-x64-musl",
	"linux-arm64-musl": "linux-arm64-musl",
	"darwin-x64":       "darwin-x64",
	"darwin-arm64":     "darwin-arm64",
}

// The runtimes features share and the host's Python environments run on: Python with uv, Node with npm, Git for Windows and the GitHub CLI
func runtimes() Registrant {
	pins := Pins(runtimesLock)
	return Declared{Tools: []Package{
		python(pins[Python]), uv(pins[UV]), node(pins[Node]), npm(pins[NPM]), git(pins[Git]), gh(pins[GH]),
	}}
}

// URL of a Termux package of the pool
func termuxDeb(letter, name, version string) string {
	return fmt.Sprintf("%spool/main/%s/%s/%s_%s_aarch64.deb", TermuxPool, letter, name, name, version)
}

// Interpreter every Python environment of the package manager is built on
func python(pin Pin) Package {
	return Package{
		Name: Python, Optional: true, Pin: pin, NoProbe: true,
		Binary: map[string]string{"win32": "python.exe", "posix": "bin/python3"},
		Deb:    &Deb{Main: "bin/python3.14"},
		URLs: func(version, target string) ([]string, error) {
			if target == Bionic {
				return []string{termuxDeb("p", "python", "3.14.10")}, nil
			}
			release, tag, _ := strings.Cut(version, "+")
			if tag == "" {
				return nil, Failure(Python, fmt.Sprintf("version %s needs the +<release> tag", version), "")
			}
			triple, ok := RustTriple[target]
			if !ok {
				return nil, fmt.Errorf("no download for target %s", target)
			}
			return []string{fmt.Sprintf("https://github.com/astral-sh/python-build-standalone/releases/download/%s/cpython-%s+%s-%s-install_only.tar.gz",
				tag, release, tag, triple)}, nil
		},
		Latest: func(cfg *config.Config, target, locked string) ([]string, error) {
			if target == Bionic || !strings.Contains(locked, "+") {
				return nil, nil
			}
			release, _, _ := strings.Cut(locked, "+")
			parts := strings.Split(release, ".")
			triple, ok := RustTriple[target]
			if !ok {
				return nil, fmt.Errorf("no download for target %s", target)
			}
			return standaloneVersions(cfg, strings.Join(parts[:min(2, len(parts))], "."), triple)
		},
		Stage: func(b *Build) error {
			if err := Flatten(b.Dir); err != nil {
				return err
			}
			if runtime.GOOS == "darwin" {
				signPython(filepath.Join(b.Dir, "bin", "python3"))
			}
			if b.Target == "win32-arm64" {
				if err := os.Remove(filepath.Join(b.Dir, "vcruntime140_1.dll")); err != nil && !os.IsNotExist(err) {
					return err
				}
			}
			if strings.HasPrefix(b.Target, "win32") {
				return fsx.Copy(filepath.Join(b.Dir, "python.exe"), filepath.Join(b.Dir, "python3.exe"))
			}
			return nil
		},
		Verify: func(entry, target string, standard func() string) string {
			if reason := standard(); reason != "" || !strings.HasPrefix(target, "win32") {
				return reason
			}
			alias, err := os.ReadFile(filepath.Join(entry, "python3.exe"))
			if err != nil {
				return "python3.exe is missing beside python.exe"
			}
			if original, err := os.ReadFile(filepath.Join(entry, "python.exe")); err != nil || !bytes.Equal(alias, original) {
				return "python3.exe differs from python.exe"
			}
			return ""
		},
	}
}

// Identifier that keeps macOS privacy grants stable across managed Python generations
const pythonIdentifier = "ai.icebath.managed-python"

// Signs the managed Python ad hoc with a stable identifier on macOS, keeping the download's signature when signing fails
func signPython(bin string) {
	codesign := Which("codesign", os.Getenv("PATH"))
	if codesign == "" {
		slog.Info("macOS codesign is unavailable; using the downloaded Python signature")
		return
	}
	text := func(r result, fallback string) string { return strings.TrimSpace(cmp.Or(r.stderr, r.stdout, fallback)) }
	signed, err := run(context.Background(), command{argv: []string{codesign, "--force", "--deep", "--sign", "-", "--timestamp=none", "--identifier", pythonIdentifier,
		"--requirements", fmt.Sprintf(`=designated => identifier "%s"`, pythonIdentifier), bin}})
	switch {
	case err != nil:
		slog.Warn(fmt.Sprintf("could not sign managed Python %s: %s", bin, err))
		return
	case signed.code != 0:
		slog.Warn(fmt.Sprintf("could not stably sign managed Python %s: %s", bin, text(signed, "codesign failed")))
		return
	}
	verified, err := run(context.Background(), command{argv: []string{codesign, "--verify", "--deep", "--strict", bin}})
	switch {
	case err != nil:
		slog.Warn(fmt.Sprintf("could not sign managed Python %s: %s", bin, err))
	case verified.code != 0:
		slog.Warn(fmt.Sprintf("macOS signature verification failed for managed Python %s: %s", bin, text(verified, "verification failed")))
	}
}

// Astral's uv everywhere it builds, the Termux package on bionic
func uv(pin Pin) Package {
	return Package{
		Name: UV, Deps: []string{Python}, Internal: true, OffPath: true, Pin: pin,
		Binary: map[string]string{"win32": "uv.exe", "posix": "uv"},
		Deb:    &Deb{Main: "bin/uv"},
		URLs: func(version, target string) ([]string, error) {
			if target == Bionic {
				return []string{termuxDeb("u", "uv", version)}, nil
			}
			triple, ok := RustTriple[target]
			if !ok {
				return nil, fmt.Errorf("no download for target %s", target)
			}
			ext := "tar.gz"
			if strings.HasPrefix(target, "win32") {
				ext = "zip"
			}
			return []string{fmt.Sprintf("https://github.com/astral-sh/uv/releases/download/%s/uv-%s.%s", version, triple, ext)}, nil
		},
		Latest: func(cfg *config.Config, target, locked string) ([]string, error) {
			return GitHubTags(cfg, "astral-sh/uv", "")
		},
	}
}

// Node from nodejs.org, its unofficial builds for musl and the Termux package on bionic
func node(pin Pin) Package {
	return Package{
		Name: Node, Pin: pin,
		Binary: map[string]string{"win32": "node.exe", "posix": "bin/node"},
		Deb:    &Deb{Main: "bin/node"},
		URLs: func(version, target string) ([]string, error) {
			if target == Bionic {
				return []string{termuxDeb("n", "nodejs", version)}, nil
			}
			platform, ok := nodePlatform[target]
			if !ok {
				return nil, fmt.Errorf("no download for target %s", target)
			}
			ext, base := "tar.xz", "https://nodejs.org/dist"
			if strings.HasPrefix(target, "win32") {
				ext = "zip"
			}
			if Musl[target] {
				base = "https://unofficial-builds.nodejs.org/download/release"
			}
			return []string{fmt.Sprintf("%s/v%s/node-v%s-%s.%s", base, version, version, platform, ext)}, nil
		},
		Latest: func(cfg *config.Config, target, locked string) ([]string, error) { return nodeVersions(cfg) },
		Repair: repairLibatomic,
	}
}

// Environment of the package manager's npm child: ambient Node and npm options left out, its cache pinned
func npmEnv(cache string, base map[string]string) map[string]string {
	env := map[string]string{}
	for k, v := range base {
		upper := strings.ToUpper(k)
		if strings.HasPrefix(upper, "NPM_CONFIG_") || upper == "NODE_OPTIONS" || upper == "NODE_PATH" || upper == "NODE_ENV" {
			continue
		}
		env[k] = v
	}
	env["npm_config_cache"] = cache
	return env
}

// npm installed by the Node it extends, offline from the checked tarball
func npm(pin Pin) Package {
	return Package{
		Name: NPM, Deps: []string{Node}, Pin: pin, Nested: true, NoProbe: true,
		Binary: map[string]string{"win32": "npm.cmd", "posix": "bin/npm"},
		URLs: func(version, target string) ([]string, error) {
			return []string{fmt.Sprintf("%snpm/-/npm-%s.tgz", npmPublic, version)}, nil
		},
		Latest: func(cfg *config.Config, target, locked string) ([]string, error) {
			tags, err := npmDistTags(cfg, "npm")
			if err != nil || tags["latest"] == "" {
				return nil, err
			}
			return []string{tags["latest"]}, nil
		},
		Unpack: func(b *Build, archive string) error {
			if b.Target == Bionic {
				return npmBionic(b, archive)
			}
			nodeBin, err := b.Binary(Node)
			if err != nil || nodeBin == "" || !fsx.IsFile(nodeBin) {
				return Failure(NPM, "npm extends node, which is not installed", "")
			}
			cli := filepath.Join(filepath.Dir(filepath.Dir(nodeBin)), "lib", "node_modules", "npm", "bin", "npm-cli.js")
			if strings.HasPrefix(b.Target, "win32") {
				cli = filepath.Join(filepath.Dir(nodeBin), "node_modules", "npm", "bin", "npm-cli.js")
			}
			if !fsx.IsFile(cli) {
				return Failure(NPM, "node's entry is missing its bundled npm-cli.js", "")
			}
			if err := os.MkdirAll(b.Dir, 0o777); err != nil {
				return err
			}
			cache, err := os.MkdirTemp("", "icebath-npm-cache-")
			if err != nil {
				return err
			}
			defer os.RemoveAll(cache)
			r, err := run(b.Ctx, command{argv: []string{nodeBin, cli, "install", "--global", "--prefix", b.Dir, "--offline", "--ignore-scripts", "--no-audit",
				"--no-fund", archive}, env: npmEnv(cache, environ()), timeout: 900 * time.Second})
			if err != nil {
				return err
			}
			if r.code != 0 {
				return Failure(NPM, fmt.Sprintf("self-install exited %d: %s", r.code, strs.Tail(r.stderr, 400)), "")
			}
			return nil
		},
	}
}

// npm's package tree under node_modules with Termux shell wrappers for npm and npx
func npmBionic(b *Build, archive string) error {
	unpacked := filepath.Join(b.Dir, ".unpacked")
	if err := Extract(archive, unpacked); err != nil {
		return err
	}
	lib := filepath.Join(b.Dir, "lib", "node_modules", "npm")
	if err := os.MkdirAll(filepath.Dir(lib), 0o777); err != nil {
		return err
	}
	if err := os.Rename(filepath.Join(unpacked, "package"), lib); err != nil {
		return err
	}
	if err := os.Remove(unpacked); err != nil {
		return err
	}
	bin := filepath.Join(b.Dir, "bin")
	if err := os.Mkdir(bin, 0o777); err != nil {
		return err
	}
	for _, name := range []string{"npm", "npx"} {
		script := "#!/data/data/com.termux/files/usr/bin/sh\nhere=\"$(cd \"$(dirname \"$0\")\" && pwd)\"\n" +
			fmt.Sprintf("exec node \"$here/../lib/node_modules/npm/bin/%s-cli.js\" \"$@\"\n", name)
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o755); err != nil {
			return err
		}
	}
	return nil
}

// Git for Windows carrying the bash commands run with there, POSIX systems using their own git by choice
func git(pin Pin) Package {
	gaps := map[string]string{}
	for _, target := range Targets {
		if !strings.HasPrefix(target, "win32") {
			gaps[target] = "POSIX uses system git by choice"
		}
	}
	return Package{
		Name: Git, Optional: true, Gaps: gaps, Pin: pin, Nested: true,
		Binary: map[string]string{"win32": "cmd/git.exe"},
		URLs: func(version, target string) ([]string, error) {
			tag, build, ok := strings.Cut(version, "+")
			if !ok {
				return nil, fmt.Errorf("git version %s must be two parts joined by +", version)
			}
			arch := "64-bit"
			if strings.HasSuffix(target, "arm64") {
				arch = "arm64"
			}
			return []string{fmt.Sprintf("https://github.com/git-for-windows/git/releases/download/v%s.windows.%s/PortableGit-%s.%s-%s.7z.exe",
				tag, build, tag, build, arch)}, nil
		},
		Latest: func(cfg *config.Config, target, locked string) ([]string, error) {
			tags, err := GitHubTags(cfg, "git-for-windows/git", "v")
			if err != nil {
				return nil, err
			}
			var out []string
			for _, tag := range tags {
				if base, build, ok := strings.Cut(tag, ".windows."); ok {
					out = append(out, base+"+"+build)
				}
			}
			return out, nil
		},
		Unpack: func(b *Build, archive string) error {
			if runtime.GOOS != "windows" {
				return Failure(Git, "the pinned artifact is a PortableGit self-extracting 7z, which only runs on Windows",
					"stage win32 git on a Windows host; POSIX hosts use the system git")
			}
			removeQuiet(b.Dir)
			if err := os.MkdirAll(b.Dir, 0o777); err != nil {
				return err
			}
			work, err := os.MkdirTemp(filepath.Dir(b.Dir), ".sfx-")
			if err != nil {
				return err
			}
			defer os.RemoveAll(work)
			exe := filepath.Join(work, filepath.Base(archive))
			if err := fsx.Copy(archive, exe); err != nil {
				return err
			}
			r, err := run(b.Ctx, command{argv: []string{exe, "-o" + b.Dir, "-y"}, timeout: 600 * time.Second})
			if err == errTimedOut {
				return Failure(Git, "PortableGit self-extractor did not finish in 600 s", "")
			}
			if err != nil {
				return err
			}
			if r.code != 0 {
				return Failure(Git, fmt.Sprintf("PortableGit self-extractor exited %d (it reports nothing under -y; usual causes: disk full, path-length limit, "+
					"antivirus lock)", r.code), "")
			}
			return nil
		},
		Env: func(entry, target string) Env {
			return Env{Path: []string{filepath.Join(entry, "cmd"), filepath.Join(entry, "usr", "bin")}}
		},
	}
}

// The GitHub CLI, whose libc independent Linux build serves musl too
func gh(pin Pin) Package {
	return Package{
		Name: GH, Optional: true, Pin: pin,
		Gaps:   map[string]string{Bionic: "the GitHub CLI publishes no Android build"},
		Binary: map[string]string{"win32": "bin/gh.exe", "posix": "bin/gh"},
		URLs: func(version, target string) ([]string, error) {
			osName, arch, _ := strings.Cut(strings.TrimSuffix(target, "-musl"), "-")
			platform, ok := map[string]string{"win32": "windows", "linux": "linux", "darwin": "macOS"}[osName]
			if !ok {
				return nil, fmt.Errorf("no download for operating system %s", osName)
			}
			archName, ok := map[string]string{"x64": "amd64", "arm64": "arm64"}[arch]
			if !ok {
				return nil, fmt.Errorf("no download for architecture %s", arch)
			}
			ext := "tar.gz"
			if osName == "win32" || osName == "darwin" {
				ext = "zip"
			}
			return []string{fmt.Sprintf("https://github.com/cli/cli/releases/download/v%s/gh_%s_%s_%s.%s", version, version, platform, archName, ext)}, nil
		},
		Latest: func(cfg *config.Config, target, locked string) ([]string, error) {
			return GitHubTags(cfg, "cli/cli", "v")
		},
	}
}
