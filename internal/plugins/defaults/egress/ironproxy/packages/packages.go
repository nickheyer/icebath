// Tools the egress proxy runs: iron-proxy, its archive's release checksums checked and their GPG signature verified where gpg is
package packages

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/packages"
	"github.com/nickheyer/icebath/pkg/fsx"
)

// Name of the iron-proxy package
const IronProxy = "iron-proxy"

// Signing key of iron-proxy's releases, pinned to the commit that published it
const signingKey = "https://raw.githubusercontent.com/paradigmxyz/iron-proxy/be5f255d0d9d10d8573bd65f480dd48a07772bf1/public-key.asc"

// Folder of the staged entry the release's checksums, signature and key are kept in
const provenance = ".provenance"

// Variables of this process a proxy process sees: infrastructure only, never the user's secrets
var Allowed = []string{"PATH", "HOME", "TMPDIR", "TZ", "LANG", "LC_ALL", "LC_CTYPE", "NO_COLOR", "SSL_CERT_DIR", "SSL_CERT_FILE", "SYSTEMROOT", "USERPROFILE"}

// Infrastructure variables of this process
func Infrastructure() map[string]string {
	env := map[string]string{}
	for _, name := range Allowed {
		if value, ok := os.LookupEnv(name); ok {
			env[name] = value
		}
	}
	return env
}

//go:embed lock.json
var lock []byte

func Register(registry *packages.Registry) {
	registry.Register(packages.Declared{Tools: []packages.Package{ironProxy(packages.Pins(lock)[IronProxy])}})
}

func ironProxy(pin packages.Pin) packages.Package {
	return packages.Package{
		Name: IronProxy, Pin: pin, Optional: true,
		Gaps: map[string]string{"win32-x64": "this integration requires a Linux/macOS host (or WSL)", "win32-arm64": "this integration requires a Linux/macOS host (or WSL)",
			packages.Bionic: "upstream does not ship an Android binary"},
		Binary:   map[string]string{"posix": "iron-proxy"},
		ProbeEnv: Infrastructure,
		URLs: func(version, target string) ([]string, error) {
			platform, arch, ok := strings.Cut(strings.TrimSuffix(target, "-musl"), "-")
			if !ok || strings.Contains(arch, "-") {
				return nil, fmt.Errorf("no download for target %s", target)
			}
			if arch == "x64" {
				arch = "amd64"
			}
			base := "https://github.com/paradigmxyz/iron-proxy/releases/download/v" + version
			return []string{fmt.Sprintf("%s/iron-proxy_%s_%s_%s.tar.gz", base, version, platform, arch), base + "/checksums.txt", base + "/checksums.txt.asc",
				signingKey}, nil
		},
		Unpack: unpack,
		Stage:  stage,
	}
}

// Keeps a provenance file of the release beside the entry, extracts the archive itself
func unpack(b *packages.Build, archive string) error {
	name := filepath.Base(archive)
	for _, suffix := range []string{".txt", ".sig", ".pem", ".asc"} {
		if strings.HasSuffix(name, suffix) {
			dir := filepath.Join(b.Dir, provenance)
			if err := os.MkdirAll(dir, 0o777); err != nil {
				return err
			}
			return fsx.Copy(archive, filepath.Join(dir, name))
		}
	}
	if err := packages.Extract(archive, b.Dir); err != nil {
		return err
	}
	return packages.Flatten(b.Dir)
}

// Checks the staged binary, that the release's checksums cover the pinned archive and that their signature verifies
func stage(b *packages.Build) error {
	bin := filepath.Join(b.Dir, "iron-proxy")
	if info, err := os.Lstat(bin); err != nil || !info.Mode().IsRegular() {
		return packages.Failure(IronProxy, "archive binary is not a regular file", "")
	}
	if err := os.Chmod(bin, 0o755); err != nil {
		return err
	}
	if len(b.Archives) == 0 {
		return fmt.Errorf("the lockfile pins no %s artifact for target %s", IronProxy, b.Target)
	}
	archive := b.Archives[0]
	data, err := os.ReadFile(filepath.Join(b.Dir, provenance, "checksums.txt"))
	if err != nil {
		return err
	}
	expected, found := "", false
	for line := range strings.Lines(string(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")))) {
		row := strings.Fields(line)
		if len(row) == 2 && strings.TrimLeft(row[1], "*") == path.Base(archive.URL) {
			expected, found = row[0], true
			break
		}
	}
	if !found || expected != archive.SHA256 {
		return packages.Failure(IronProxy, "release checksums do not cover the pinned archive", "")
	}
	return verify(b.Ctx, filepath.Join(b.Dir, provenance))
}

// Verifies the checksums' signature with the pinned key in a keyring of its own; without gpg, or when the key does not import, the
// checksum of the archive alone stays enforced
func verify(ctx context.Context, dir string) error {
	gpg, err := exec.LookPath("gpg")
	if err != nil {
		slog.Warn("gpg unavailable; iron-proxy archive checksum remains enforced")
		return nil
	}
	home, err := os.MkdirTemp("", "icebath-iron-signature-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(home)
	args := []string{"--homedir", home, "--batch", "--no-tty"}
	signature, key := filepath.Join(dir, "checksums.txt.asc"), filepath.Join(dir, "public-key.asc")
	if !fsx.IsFile(signature) || !fsx.IsFile(key) {
		return packages.Failure(IronProxy, "pinned signature assets missing", "")
	}
	code, err := gpgRun(ctx, gpg, append(args, "--import", key)...)
	if err != nil {
		return err
	}
	if code != 0 {
		slog.Warn("Could not import iron-proxy signing key; archive checksum remains enforced")
		return nil
	}
	if code, err = gpgRun(ctx, gpg, append(args, "--verify", signature, filepath.Join(dir, "checksums.txt"))...); err != nil {
		return err
	}
	if code != 0 {
		return packages.Failure(IronProxy, "GPG signature verification failed", "")
	}
	return nil
}

// Runs gpg with no input and its output swallowed, a minute at most, answering its exit status
func gpgRun(ctx context.Context, gpg string, args ...string) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, gpg, args...)
	_, err := cmd.CombinedOutput()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return 0, fmt.Errorf("%s timed out after 60 seconds", strings.Join(cmd.Args, " "))
	}
	if exit, ok := errors.AsType[*exec.ExitError](err); ok {
		return exit.ExitCode(), nil
	}
	return 0, err
}
