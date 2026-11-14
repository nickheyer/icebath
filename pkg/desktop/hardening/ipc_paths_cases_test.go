package hardening

import (
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func codeOf(err error) string {
	var pe *IPCPathError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

func fileURL(p string) string { return (&url.URL{Scheme: "file", Path: filepath.ToSlash(p)}).String() }

func TestSensitiveFileBlockReasonBlocksObviousSecretFilePatterns(t *testing.T) {
	if r, ok := SensitiveFileBlockReason("/tmp/.env"); !ok || !strings.Contains(r, ".env") {
		t.Fatalf(".env %q", r)
	}
	if r, ok := SensitiveFileBlockReason("/tmp/.env.example"); ok {
		t.Fatalf(".env.example %q", r)
	}
	if r, _ := SensitiveFileBlockReason("/Users/me/.ssh/id_ed25519"); !strings.Contains(r, "SSH") {
		t.Fatalf("ssh %q", r)
	}
	if r, _ := SensitiveFileBlockReason("/tmp/server-cert.pem"); !strings.Contains(r, ".pem") {
		t.Fatalf("pem %q", r)
	}
}

func TestPathHelpersRejectBlankNonStringNulAndWindowsDeviceSyntax(t *testing.T) {
	for _, in := range []any{"", "   ", nil, "safe\x00name.txt"} {
		if _, err := ResolveReadableFileForIpc(in, ReadableFileOptions{Purpose: "File preview"}); codeOf(err) != "invalid-path" {
			t.Fatalf("%q gave %v", in, err)
		}
	}
	for _, p := range []string{`\\?\C:\secret.txt`, `\\.\C:\secret.txt`, `\\?\UNC\server\share\secret.txt`, "GLOBALROOT/Device/HarddiskVolumeShadowCopy1/secret.txt"} {
		if _, err := ResolveRequestedPathForIpc(p, "File preview", ""); codeOf(err) != "device-path" {
			t.Fatalf("%q gave %v", p, err)
		}
		if _, err := ResolveReadableFileForIpc(p, ReadableFileOptions{Purpose: "File preview"}); codeOf(err) != "device-path" {
			t.Fatalf("%q gave %v", p, err)
		}
	}
	if _, err := ResolveRequestedPathForIpc("file:///%E0%A4%A", "File preview", ""); codeOf(err) != "invalid-path" {
		t.Fatalf("bad url %v", err)
	}
	if _, err := ResolveReadableFileForIpc("file:///%E0%A4%A", ReadableFileOptions{Purpose: "File preview"}); codeOf(err) != "invalid-path" {
		t.Fatalf("bad url %v", err)
	}
}

func TestResolveRequestedPathForIpcResolvesRelativePathsFromTheTrimmedBaseDirectory(t *testing.T) {
	base := filepath.Join(os.TempDir(), "hermes-desktop-base")
	got, err := ResolveRequestedPathForIpc("notes.txt", "File preview", "  "+base+"  ")
	if err != nil || got != filepath.Join(base, "notes.txt") {
		t.Fatalf("got %q %v", got, err)
	}
}

func TestResolveRequestedPathForIpcExpandsToTheHomeDirectory(t *testing.T) {
	home, _ := os.UserHomeDir()
	if got, _ := ResolveRequestedPathForIpc("~", "Directory read", ""); got != filepath.Clean(home) {
		t.Fatalf("~ gave %q", got)
	}
	if got, _ := ResolveRequestedPathForIpc("~/www/project", "Directory read", ""); got != filepath.Join(home, "www", "project") {
		t.Fatalf("~/ gave %q", got)
	}
	if got, _ := ResolveRequestedPathForIpc("~other/secret", "Directory read", os.TempDir()); got != filepath.Join(os.TempDir(), "~other", "secret") {
		t.Fatalf("~other gave %q", got)
	}
}

func TestResolveReadableFileForIpcValidatesExistenceTypeSizeAndSensitivity(t *testing.T) {
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	text := filepath.Join(dir, "notes.txt")
	os.WriteFile(text, []byte("hello world"), 0o644)
	got, err := ResolveReadableFileForIpc("notes.txt", ReadableFileOptions{BaseDir: dir, MaxBytes: 256, Purpose: "File preview"})
	if err != nil || got.ResolvedPath != text || got.Stat.Size() != 11 {
		t.Fatalf("relative %v %v", got, err)
	}
	if got, err := ResolveReadableFileForIpc(fileURL(text), ReadableFileOptions{Purpose: "File preview"}); err != nil || got.ResolvedPath != text {
		t.Fatalf("file url %v %v", got, err)
	}
	spaced := filepath.Join(dir, "notes with spaces.txt")
	os.WriteFile(spaced, []byte("space ok"), 0o644)
	if got, err := ResolveReadableFileForIpc(fileURL(spaced), ReadableFileOptions{Purpose: "File preview"}); err != nil || got.ResolvedPath != spaced {
		t.Fatalf("spaced %v %v", got, err)
	}
	if _, err := ResolveReadableFileForIpc("missing.txt", ReadableFileOptions{BaseDir: dir, Purpose: "Text preview"}); err == nil || !strings.Contains(err.Error(), "file does not exist") {
		t.Fatalf("missing %v", err)
	}
	nested := filepath.Join(dir, "directory")
	os.Mkdir(nested, 0o755)
	if _, err := ResolveReadableFileForIpc(nested, ReadableFileOptions{Purpose: "Text preview"}); err == nil || !strings.Contains(err.Error(), "path points to a directory") {
		t.Fatalf("dir %v", err)
	}
	large := filepath.Join(dir, "large.txt")
	os.WriteFile(large, []byte(strings.Repeat("x", 40)), 0o644)
	if _, err := ResolveReadableFileForIpc(large, ReadableFileOptions{MaxBytes: 8, Purpose: "File preview"}); err == nil || !strings.Contains(err.Error(), "file is too large") {
		t.Fatalf("large %v", err)
	}
	env := filepath.Join(dir, ".env")
	os.WriteFile(env, []byte("SECRET_TOKEN=123"), 0o644)
	if _, err := ResolveReadableFileForIpc(env, ReadableFileOptions{Purpose: "File preview"}); err == nil || !strings.Contains(err.Error(), "blocked for sensitive file") {
		t.Fatalf("env %v", err)
	}
	tmpl := filepath.Join(dir, ".env.example")
	os.WriteFile(tmpl, []byte("EXAMPLE_TOKEN=value"), 0o644)
	if got, err := ResolveReadableFileForIpc(tmpl, ReadableFileOptions{Purpose: "File preview"}); err != nil || got.ResolvedPath != tmpl {
		t.Fatalf("template %v %v", got, err)
	}
}

func TestResolveReadableFileForIpcBlocksCommonSensitiveFiles(t *testing.T) {
	dir := t.TempDir()
	os.Mkdir(filepath.Join(dir, ".ssh"), 0o755)
	for _, p := range []string{".env", ".npmrc", filepath.Join(".ssh", "id_ed25519"), "cert.pem", "cert.p12", "cert.pfx"} {
		full := filepath.Join(dir, p)
		os.WriteFile(full, []byte("secret"), 0o644)
		if _, err := ResolveReadableFileForIpc(full, ReadableFileOptions{Purpose: "File preview"}); codeOf(err) != "sensitive-file" {
			t.Fatalf("%s gave %v", p, err)
		}
	}
	allowed := filepath.Join(dir, ".env.example")
	os.WriteFile(allowed, []byte("EXAMPLE_TOKEN=value"), 0o644)
	if got, err := ResolveReadableFileForIpc(allowed, ReadableFileOptions{Purpose: "File preview"}); err != nil || got.ResolvedPath != allowed {
		t.Fatalf("allowed %v %v", got, err)
	}
}

func TestResolveReadableFileForIpcBlocksSymlinksWhoseRealpathIsSensitive(t *testing.T) {
	dir := t.TempDir()
	env := filepath.Join(dir, ".env")
	link := filepath.Join(dir, "safe-name.txt")
	os.WriteFile(env, []byte("SECRET_TOKEN=123"), 0o644)
	if err := os.Symlink(env, link); err != nil {
		if os.IsPermission(err) {
			return
		}
		t.Fatal(err)
	}
	if _, err := ResolveReadableFileForIpc(link, ReadableFileOptions{Purpose: "File preview"}); codeOf(err) != "sensitive-file" {
		t.Fatalf("link %v", err)
	}
}
