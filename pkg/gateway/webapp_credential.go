package gateway

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Home relative file holding the Webapp browser credential
var WebappTokenPath = filepath.Join("webapp", "session-token")

// Shape of a secrets.token_urlsafe(32) credential, the only one the launch page accepts
var webappTokenFormat = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

// BrowserCredentialError reports a Webapp browser credential that cannot be stored, naming the file and why
type BrowserCredentialError struct {
	Msg string
	Err error
}

func (e *BrowserCredentialError) Error() string { return e.Msg }
func (e *BrowserCredentialError) Unwrap() error { return e.Err }

// Python OSError strerror of err, the capitalized errno text, else the error itself
func osStrerror(err error) string {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		text := errno.Error()
		if text != "" {
			return strings.ToUpper(text[:1]) + text[1:]
		}
	}
	return err.Error()
}

// Reports whether path grants group or other access or belongs to another user, false on Windows
func webappReadableByOtherUsers(path string) bool {
	if runtime.GOOS == "windows" {
		return false
	}
	st, err := os.Stat(path)
	if err != nil {
		return false
	}
	return st.Mode().Perm()&0o077 != 0 || statOwnerUID(st) != os.Geteuid()
}

// Makes an existing credential folder user only again on POSIX, warning when the chmod fails
func webappCloseToOtherUsers(folder string) {
	if runtime.GOOS == "windows" {
		return
	}
	st, err := os.Stat(folder)
	if err == nil && st.Mode().Perm()&0o077 != 0 {
		err = os.Chmod(folder, 0o700)
	}
	if err == nil || errors.Is(err, fs.ErrNotExist) {
		return
	}
	slog.Warn(fmt.Sprintf("Could not make %s user-only: %s", folder, pyfs.OSErrorText(err)))
}

// Session token a Webapp browser holds, created on first use, replaced on rotate or when unreadable, malformed or exposed
func BrowserSessionToken(ctx context.Context, home string, rotate bool) (string, error) {
	path := filepath.Join(home, WebappTokenPath)
	webappCloseToOtherUsers(filepath.Dir(path))
	if !rotate {
		token := ""
		if raw, err := os.ReadFile(path); err == nil && utf8.Valid(raw) {
			token = pystr.Strip(strings.TrimPrefix(string(raw), "\xef\xbb\xbf"))
		}
		if webappTokenFormat.MatchString(token) && !webappReadableByOtherUsers(path) {
			return token, nil
		}
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(buf)
	err := webappMkdirAll(filepath.Dir(path), 0o700)
	if err == nil {
		err = pyfs.AtomicWriteTextMode(ctx, path, token+"\n", "", 0o600)
	}
	if err != nil {
		return "", &BrowserCredentialError{
			Msg: fmt.Sprintf("could not store the Webapp's browser credential in %s (%s)", path, osStrerror(err)), Err: err}
	}
	return token, nil
}

// Path.mkdir(mode, parents=True, exist_ok=True), parents made with the default mode and a non directory raising
func webappMkdirAll(dir string, mode os.FileMode) error {
	err := os.Mkdir(dir, mode)
	if err == nil {
		return nil
	}
	if errors.Is(err, fs.ErrNotExist) && filepath.Dir(dir) != dir {
		if perr := webappMkdirAll(filepath.Dir(dir), 0o777); perr != nil {
			return perr
		}
		if err = os.Mkdir(dir, mode); err == nil {
			return nil
		}
	}
	if st, serr := os.Stat(dir); serr == nil && st.IsDir() {
		return nil
	}
	return err
}
