package pm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// ProxySubprocessEnvAllowlist is the infrastructure only environment an iron-proxy child may see
var ProxySubprocessEnvAllowlist = []string{
	"PATH", "HOME", "TMPDIR", "TZ", "LANG", "LC_ALL", "LC_CTYPE", "NO_COLOR", "SSL_CERT_DIR", "SSL_CERT_FILE", "SYSTEMROOT", "USERPROFILE",
}

// Infrastructure only environment of this process, never the operator's secrets
func IronProxyAllowlistedEnv() map[string]string {
	env := map[string]string{}
	for _, name := range ProxySubprocessEnvAllowlist {
		if v, ok := os.LookupEnv(name); ok {
			env[name] = v
		}
	}
	return env
}

// Bws is the Bitwarden secrets manager CLI, one static musl build serving every Linux userland
type Bws struct {
	BinaryPackage
}

func (b *Bws) FetchURL(version, target string) (string, error) {
	triple, ok := rustTriple[target]
	if !ok {
		return "", fmt.Errorf("KeyError: %s", pyrepr.StrRepr(target))
	}
	triple = strings.ReplaceAll(triple, "linux-gnu", "linux-musl")
	platform := triple
	if strings.HasPrefix(target, "darwin") {
		platform = "macos-universal"
	}
	return fmt.Sprintf("https://github.com/bitwarden/sdk-sm/releases/download/bws-v%s/bws-%s-%s.zip", version, platform, version), nil
}

func (b *Bws) Stage(store *Store, staged, version, target string) error {
	if err := b.BinaryPackage.Stage(store, staged, version, target); err != nil {
		return err
	}
	if bin := b.dispatch().Binary(staged, target); bin != "" && isRegularFile(bin) {
		return os.Chmod(bin, 0o755)
	}
	return nil
}

// ProvenanceVerifier checks a signed binary's pinned provenance files
type ProvenanceVerifier interface {
	VerifyProvenance(directory string) error
}

// SignedBinary acquires pinned provenance files in the same artifact transaction as its archive
type SignedBinary struct {
	BinaryPackage
}

// Gaps of every signed binary integration
func signedBinaryGaps() map[string]string {
	return map[string]string{
		"win32-x64":          "this integration requires a Linux/macOS host (or WSL)",
		"win32-arm64":        "this integration requires a Linux/macOS host (or WSL)",
		"linux-arm64-bionic": "upstream does not ship an Android binary",
	}
}

func (s *SignedBinary) Unpack(archive, staged, target string) error {
	name := filepath.Base(archive)
	for _, suffix := range []string{".txt", ".sig", ".pem", ".asc"} {
		if strings.HasSuffix(name, suffix) {
			directory := filepath.Join(staged, ".provenance")
			if err := os.MkdirAll(directory, 0o777); err != nil {
				return err
			}
			return copyFile(archive, filepath.Join(directory, name))
		}
	}
	if err := s.BinaryPackage.Unpack(archive, staged, target); err != nil {
		return err
	}
	return FlattenSingleDir(staged)
}

// Copies a file's bytes to a new destination
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o666)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func (s *SignedBinary) Stage(store *Store, staged, version, target string) error {
	bin := s.dispatch().Binary(staged, target)
	lst, lerr := os.Lstat(bin)
	if bin == "" || !isRegularFile(bin) || (lerr == nil && lst.Mode()&os.ModeSymlink != 0) {
		return NewInstallError(s.Name, "archive binary is not a regular file", "")
	}
	if err := os.Chmod(bin, 0o755); err != nil {
		return err
	}
	provenance := filepath.Join(staged, ".provenance")
	artifacts := NewLockfile(LockfilePath()).Artifacts(s.Name, target)
	if len(artifacts) == 0 {
		return errors.New("IndexError: list index out of range")
	}
	archive := artifacts[0]
	filename, err := archiveFilename(archive)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(filepath.Join(provenance, "checksums.txt"))
	if err != nil {
		return err
	}
	expected := ""
	found := false
	for _, line := range pystr.Splitlines(string(trimBOM(data)), false) {
		row := strings.Fields(line)
		if len(row) == 2 && strings.TrimLeft(row[1], "*") == filename {
			expected, found = row[0], true
			break
		}
	}
	if !found || !jsonEquals(archive["sha256"], expected) {
		return NewInstallError(s.Name, "release checksums do not cover the pinned archive", "")
	}
	return s.dispatch().(ProvenanceVerifier).VerifyProvenance(provenance)
}

// Schemes whose last path segment urlparse splits params from
var urlUsesParams = map[string]bool{
	"": true, "ftp": true, "hdl": true, "prospero": true, "http": true, "imap": true, "https": true, "shttp": true,
	"rtsp": true, "rtsps": true, "rtspu": true, "sip": true, "sips": true, "mms": true, "sftp": true, "tel": true,
}

// File name of a pinned archive URL the way Path of the urlparse path names it
func archiveFilename(archive map[string]any) (string, error) {
	raw, ok := archive["url"]
	if !ok {
		return "", errors.New("KeyError: 'url'")
	}
	rawURL, isString := raw.(string)
	if !isString {
		if pyrepr.Truthy(raw) {
			return "", fmt.Errorf("AttributeError: '%s' object has no attribute 'decode'", pyrepr.TypeName(raw))
		}
		return "", errors.New("TypeError: argument should be a str or an os.PathLike object where __fspath__ returns a str, not 'bytes'")
	}
	parts, err := pyURLSplit(rawURL)
	if err != nil {
		return "", err
	}
	urlPath := parts.Path
	if urlUsesParams[parts.Scheme] && strings.Contains(urlPath, ";") {
		from := max(strings.LastIndex(urlPath, "/"), 0)
		if i := strings.IndexByte(urlPath[from:], ';'); i >= 0 {
			urlPath = urlPath[:from+i]
		}
	}
	return pyPathName(urlPath), nil
}

// Final component of a path the way pathlib Path name reads it on this platform
func pyPathName(p string) string {
	rest, seps := p, "/"
	if runtime.GOOS == "windows" {
		_, _, rest = pypath.NtSplitRoot(p)
		seps = `\/`
	}
	parts := strings.FieldsFunc(rest, func(r rune) bool { return strings.ContainsRune(seps, r) })
	for i := len(parts) - 1; i >= 0; i-- {
		if parts[i] != "." {
			return parts[i]
		}
	}
	return ""
}

// IronProxy is the egress proxy, its archive checksum enforced and its GPG signature checked when gpg exists
type IronProxy struct {
	SignedBinary
	SigningKeyURL string
}

// Probe environment of the proxy binary, infrastructure variables only
func (p *IronProxy) ProbeEnv() (map[string]string, error) {
	return IronProxyAllowlistedEnv(), nil
}

func (p *IronProxy) FetchURL(version, target string) (string, error) {
	lookup := target
	if MuslTargets[target] {
		lookup = strings.TrimSuffix(target, "-musl")
	}
	parts := strings.Split(lookup, "-")
	if len(parts) > 2 {
		return "", fmt.Errorf("ValueError: too many values to unpack (expected 2, got %d)", len(parts))
	}
	if len(parts) < 2 {
		return "", fmt.Errorf("ValueError: not enough values to unpack (expected 2, got %d)", len(parts))
	}
	platform, arch := parts[0], parts[1]
	if arch == "x64" {
		arch = "amd64"
	}
	return fmt.Sprintf("https://github.com/paradigmxyz/iron-proxy/releases/download/v%s/iron-proxy_%s_%s_%s.tar.gz", version, version, platform, arch), nil
}

func (p *IronProxy) FetchURLs(version, target string) ([]string, error) {
	archive, err := p.FetchURL(version, target)
	if err != nil {
		return nil, err
	}
	base := archive[:strings.LastIndex(archive, "/")]
	return []string{archive, base + "/checksums.txt", base + "/checksums.txt.asc", p.SigningKeyURL}, nil
}

func (p *IronProxy) VerifyProvenance(directory string) error {
	gpg, err := exec.LookPath("gpg")
	if err != nil {
		slog.Warn("gpg unavailable; iron-proxy archive checksum remains enforced")
		return nil
	}
	homeDir, err := os.MkdirTemp("", "icebath-iron-signature-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(homeDir)
	args := []string{"--homedir", homeDir, "--batch", "--no-tty"}
	signature := filepath.Join(directory, "checksums.txt.asc")
	key := filepath.Join(directory, "public-key.asc")
	if !isRegularFile(signature) || !isRegularFile(key) {
		return NewInstallError(p.Name, "pinned signature assets missing", "")
	}
	if code, err := runGPG(gpg, append(append([]string{}, args...), "--import", Native(key))); err != nil {
		return err
	} else if code != 0 {
		slog.Warn("Could not import iron-proxy signing key; archive checksum remains enforced")
		return nil
	}
	code, err := runGPG(gpg, append(append([]string{}, args...), "--verify", Native(signature), Native(filepath.Join(directory, "checksums.txt"))))
	if err != nil {
		return err
	}
	if code != 0 {
		return NewInstallError(p.Name, "GPG signature verification failed", "")
	}
	return nil
}

// Runs gpg with no input and captured output under a one minute timeout, returning its exit code
func runGPG(gpg string, args []string) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, gpg, args...)
	_, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return 0, fmt.Errorf("Command '%s' timed out after 60 seconds", pyrepr.Repr(cmd.Args))
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), nil
	}
	if err != nil {
		return 0, err
	}
	return 0, nil
}

// Registers the optional security executables
func init() {
	MustRegister(&Bws{BinaryPackage: BinaryPackage{
		BasePackage: BasePackage{Name: "bws", Optional: true,
			Gaps: map[string]string{"linux-arm64-bionic": "upstream does not ship an Android binary"}},
		BinaryRel: map[string]string{"posix": "bws", "win32": "bws.exe"},
	}})
	MustRegister(&IronProxy{
		SignedBinary: SignedBinary{BinaryPackage: BinaryPackage{
			BasePackage: BasePackage{Name: "iron-proxy", Optional: true, Gaps: signedBinaryGaps()},
			BinaryRel:   map[string]string{"posix": "iron-proxy"},
		}},
		SigningKeyURL: "https://raw.githubusercontent.com/paradigmxyz/iron-proxy/be5f255d0d9d10d8573bd65f480dd48a07772bf1/public-key.asc",
	})
}
