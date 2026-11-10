package gateway

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"unicode/utf16"
)

const storedWebappToken = "sssssssssssssssssssssssssssssssssssssssssss"

// Stored credential with the folder and file modes applied after writing
func storeWebappToken(t *testing.T, h string, folderMode, fileMode os.FileMode) string {
	t.Helper()
	path := filepath.Join(h, WebappTokenPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(storedWebappToken+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, fileMode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Dir(path), folderMode); err != nil {
		t.Fatal(err)
	}
	return path
}

func permOf(t *testing.T, p string) os.FileMode {
	t.Helper()
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return st.Mode().Perm()
}

func mustToken(t *testing.T, h string, rotate bool) string {
	t.Helper()
	tok, err := BrowserSessionToken(context.Background(), h, rotate)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func TestACredentialOtherUsersCouldReadIsReplaced(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix platform test")
	}
	for _, mode := range []os.FileMode{0o644, 0o640, 0o604} {
		t.Run(mode.String(), func(t *testing.T) {
			dir := t.TempDir()
			path := storeWebappToken(t, dir, 0o700, mode)
			token := mustToken(t, dir, false)
			if token == storedWebappToken {
				t.Fatal("exposed credential reused")
			}
			if raw, _ := os.ReadFile(path); strings.TrimSpace(string(raw)) != token {
				t.Fatalf("stored %q", raw)
			}
			if got := permOf(t, path); got != 0o600 {
				t.Fatalf("mode %o", got)
			}
		})
	}
}

func TestAPrivateCredentialInAnOpenFolderIsKeptAndTheFolderClosed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix platform test")
	}
	dir := t.TempDir()
	path := storeWebappToken(t, dir, 0o755, 0o600)
	if got := mustToken(t, dir, false); got != storedWebappToken {
		t.Fatalf("%q", got)
	}
	if permOf(t, filepath.Dir(path)) != 0o700 || permOf(t, path) != 0o600 {
		t.Fatalf("modes %o %o", permOf(t, filepath.Dir(path)), permOf(t, path))
	}
}

func TestSigningOutClosesAnOpenCredentialFolderToo(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix platform test")
	}
	dir := t.TempDir()
	path := storeWebappToken(t, dir, 0o755, 0o600)
	if got := mustToken(t, dir, true); got == storedWebappToken {
		t.Fatal("rotate kept the credential")
	}
	if permOf(t, filepath.Dir(path)) != 0o700 || permOf(t, path) != 0o600 {
		t.Fatalf("modes %o %o", permOf(t, filepath.Dir(path)), permOf(t, path))
	}
}

func TestAPrivateCredentialIsReused(t *testing.T) {
	dir := t.TempDir()
	first := mustToken(t, dir, false)
	if got := mustToken(t, dir, false); got != first {
		t.Fatalf("%q != %q", got, first)
	}
}

func TestACredentialFileThatIsNotTextIsReplaced(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, WebappTokenPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	units := utf16.Encode([]rune(storedWebappToken + "\n"))
	raw := []byte{0xff, 0xfe}
	for _, u := range units {
		raw = append(raw, byte(u), byte(u>>8))
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	token := mustToken(t, dir, false)
	if token == storedWebappToken {
		t.Fatal("undecodable credential reused")
	}
	if got, _ := os.ReadFile(path); strings.TrimSpace(string(got)) != token {
		t.Fatalf("stored %q", got)
	}
}

func TestACredentialThatCannotBeStoredNamesItsFile(t *testing.T) {
	for _, block := range []string{"folder-is-a-file", "token-is-a-folder"} {
		t.Run(block, func(t *testing.T) {
			dir := t.TempDir()
			if block == "folder-is-a-file" {
				if err := os.WriteFile(filepath.Join(dir, "webapp"), []byte("not a folder\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			} else if err := os.MkdirAll(filepath.Join(dir, WebappTokenPath), 0o755); err != nil {
				t.Fatal(err)
			}
			_, err := BrowserSessionToken(context.Background(), dir, false)
			var bce *BrowserCredentialError
			if !errors.As(err, &bce) {
				t.Fatalf("%v", err)
			}
			var pe *os.PathError
			var le *os.LinkError
			if !errors.As(err, &pe) && !errors.As(err, &le) {
				t.Fatalf("not an OS error %v", err)
			}
			if !strings.Contains(err.Error(), filepath.Join(dir, WebappTokenPath)) {
				t.Fatalf("%q", err.Error())
			}
		})
	}
}

func TestAGatewayRestartKeepsTheBrowserCredential(t *testing.T) {
	h := filepath.Join(t.TempDir(), "home")
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	first := mustToken(t, h, false)
	if len(first) < 43 || mustToken(t, h, false) != first {
		t.Fatalf("%q", first)
	}
	if runtime.GOOS != "windows" && permOf(t, filepath.Join(h, WebappTokenPath)) != 0o600 {
		t.Fatal("token file is not owner only")
	}
}

func TestSigningOutReplacesTheBrowserCredential(t *testing.T) {
	h := filepath.Join(t.TempDir(), "home")
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	first := mustToken(t, h, false)
	second := mustToken(t, h, true)
	if second == first || mustToken(t, h, false) != second {
		t.Fatalf("%q %q", first, second)
	}
}

func TestACredentialTheBrowserWouldRefuseIsReplaced(t *testing.T) {
	for _, stored := range []string{"short", strings.Repeat("x", 60), "not/url+safe" + strings.Repeat("a", 31)} {
		t.Run(stored, func(t *testing.T) {
			h := filepath.Join(t.TempDir(), "home")
			path := filepath.Join(h, WebappTokenPath)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(stored+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			token := mustToken(t, h, false)
			if token == stored || !regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`).MatchString(token) {
				t.Fatalf("%q", token)
			}
		})
	}
}
