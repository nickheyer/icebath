package profiles

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

// Characters of a candidate wrapper read when probing for the profile launch marker
const wrapperReadLimit = 8192

// Text every generated wrapper carries ahead of the profile name
const wrapperMarker = "icebath -p"

// Locates an executable on PATH, replaced by tests
var lookPath = exec.LookPath

// Runs which or where for name and returns its stdout and exit code, replaced by tests
var runWhich = func(name string) (string, int, error) {
	tool := "which"
	if runtime.GOOS == "windows" {
		tool = "where"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, tool, name)
	cmd.Stdout = &out
	err := cmd.Run()
	if ctx.Err() != nil {
		return "", -1, ctx.Err()
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return strings.ToValidUTF8(out.String(), "�"), exitErr.ExitCode(), nil
	}
	if err != nil {
		return "", -1, err
	}
	return strings.ToValidUTF8(out.String(), "�"), 0, nil
}

// User home directory as Python Path.home resolves it
func userHome() (string, error) {
	if h, err := os.UserHomeDir(); err == nil {
		return h, nil
	}
	u, err := user.Current()
	if err != nil {
		return "", err
	}
	return u.HomeDir, nil
}

// Directory holding alias wrapper scripts
func WrapperDir() (string, error) {
	h, err := userHome()
	if err != nil {
		return "", errors.New("Could not determine home directory.")
	}
	return filepath.Join(h, ".local", "bin"), nil
}

// Wrapper script path of an alias in the wrapper dir
func WrapperPath(alias string) (string, error) { return wrapperPath(alias) }

// Wrapper script path of alias, a bat file on Windows
func wrapperPath(alias string) (string, error) {
	dir, err := WrapperDir()
	if err != nil {
		return "", err
	}
	if runtime.GOOS == "windows" {
		return filepath.Join(dir, alias+".bat"), nil
	}
	return filepath.Join(dir, alias), nil
}

// Generated wrapper of alias, also a bare pre bat wrapper on Windows, empty when none is ours
func FindOurWrapper(alias string) (string, error) {
	first, err := wrapperPath(alias)
	if err != nil {
		return "", err
	}
	candidates := []string{first}
	if runtime.GOOS == "windows" {
		dir, _ := WrapperDir()
		candidates = append(candidates, filepath.Join(dir, alias))
	}
	for _, p := range candidates {
		if isOurWrapper(p) {
			return p, nil
		}
	}
	return "", nil
}

// First n characters of a UTF-8 file with any byte order mark dropped, an error for undecodable text
func readTextHead(path string, n int) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, int64(n*utf8.UTFMax+3)))
	if err != nil {
		return "", err
	}
	raw = bytes.TrimPrefix(raw, []byte("\ufeff"))
	var b strings.Builder
	for count := 0; count < n && len(raw) > 0; count++ {
		r, size := utf8.DecodeRune(raw)
		if r == utf8.RuneError && size <= 1 {
			return "", errors.New("invalid UTF-8 in " + path)
		}
		b.WriteRune(r)
		raw = raw[size:]
	}
	return b.String(), nil
}

// Reports whether path reads as a generated wrapper
func isOurWrapper(path string) bool {
	head, err := readTextHead(path, wrapperReadLimit)
	return err == nil && strings.Contains(head, wrapperMarker)
}

// Collision message for an alias name, empty when the name is safe to use
func CheckAliasCollision(name string) (string, error) {
	canon, err := NormalizeProfileName(name)
	if err != nil {
		return "", err
	}
	if err := ValidateAliasName(canon); err != nil {
		return err.Error(), nil
	}
	if ReservedNames[canon] {
		return fmt.Sprintf("'%s' is a reserved name", canon), nil
	}
	if CLISubcommands[canon] {
		return fmt.Sprintf("'%s' conflicts with an icebath subcommand", canon), nil
	}
	stdout, code, err := runWhich(canon)
	if err != nil || code != 0 {
		return "", nil
	}
	lines := strings.FieldsFunc(strings.TrimSpace(stdout), func(r rune) bool { return r == '\n' || r == '\r' })
	if len(lines) == 0 {
		return "", errors.New("list index out of range")
	}
	existing := lines[0]
	expected, err := wrapperPath(canon)
	if err != nil {
		return "", err
	}
	if existing == expected && isOurWrapper(expected) {
		return "", nil
	}
	return fmt.Sprintf("'%s' conflicts with an existing command (%s)", canon, existing), nil
}

// Reports whether the wrapper directory is on PATH
func IsWrapperDirInPath() (bool, error) {
	dir, err := WrapperDir()
	if err != nil {
		return false, err
	}
	for _, p := range strings.Split(os.Getenv("PATH"), string(os.PathListSeparator)) {
		if p == dir {
			return true, nil
		}
	}
	return false, nil
}

// Python shlex.quote of s
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	safe := true
	for _, r := range s {
		if !(r < 128 && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("@%+=:,./-_", r))) {
			safe = false
			break
		}
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// Writes the wrapper alias launching profile target, or the alias itself, returning empty after a printed write failure
func CreateWrapperScript(name, target string) (string, error) {
	canon, err := NormalizeProfileName(name)
	if err != nil {
		return "", err
	}
	profile := canon
	if target != "" {
		if profile, err = NormalizeProfileName(target); err != nil {
			return "", err
		}
	}
	if err := ValidateAliasName(canon); err != nil {
		return "", err
	}
	dir, err := WrapperDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o777); err != nil {
		fmt.Printf("⚠ Could not create %s: %s\n", dir, pyOSErrorText(err))
		return "", nil
	}
	path, _ := wrapperPath(canon)
	if runtime.GOOS == "windows" {
		err = os.WriteFile(path, []byte(fmt.Sprintf("@echo off\r\nicebath -p %s %%*\r\n", profile)), 0o666)
	} else {
		exe, lerr := lookPath("icebath")
		if lerr != nil || exe == "" {
			exe = "icebath"
		}
		err = os.WriteFile(path, []byte(fmt.Sprintf("#!/bin/sh\nexec %s -p %s \"$@\"\n", shellQuote(exe), profile)), 0o666)
		if err == nil {
			var st os.FileInfo
			if st, err = os.Stat(path); err == nil {
				err = os.Chmod(path, st.Mode().Perm()|0o111)
			}
		}
	}
	if err != nil {
		fmt.Printf("⚠ Could not create wrapper at %s: %s\n", path, pyOSErrorText(err))
		return "", nil
	}
	return path, nil
}

// Python text of an OS error, errno number and description followed by the quoted path
func pyOSErrorText(err error) string {
	var pe *os.PathError
	if !errors.As(err, &pe) {
		return err.Error()
	}
	var errno syscall.Errno
	if errors.As(pe.Err, &errno) {
		return fmt.Sprintf("[Errno %d] %s: '%s'", int(errno), pyErrnoText(errno), pe.Path)
	}
	return fmt.Sprintf("%s: '%s'", pyErrnoText(pe.Err), pe.Path)
}

// Errno description capitalized as Python strerror spells it
func pyErrnoText(err error) string {
	s := err.Error()
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// Removes the generated wrapper of a profile alias, reporting whether one was removed
func RemoveWrapperScript(name string) (bool, error) {
	canon, err := NormalizeProfileName(name)
	if err != nil {
		return false, err
	}
	if ValidateAliasName(canon) != nil {
		return false, nil
	}
	path, err := FindOurWrapper(canon)
	if err != nil || path == "" {
		return false, err
	}
	return os.Remove(path) == nil, nil
}

// Alias of the wrapper launching a profile, empty when none does
func FindAliasForProfile(profileName string) (string, error) {
	canon, err := NormalizeProfileName(profileName)
	if err != nil {
		return "", err
	}
	aliases, err := BuildAliasMap()
	return aliases[canon], err
}

// Python PurePath suffix and stem of a file name
func pySuffixStem(name string) (string, string) {
	i := strings.LastIndex(name, ".")
	if i > 0 && i < len(name)-1 {
		return name[i:], name[:i]
	}
	return "", name
}

// Map from canonical profile to alias read in one pass over the wrapper directory, custom aliases winning
func BuildAliasMap() (map[string]string, error) {
	result := map[string]string{}
	dir, err := WrapperDir()
	if err != nil {
		return nil, err
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return result, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	isWindows := runtime.GOOS == "windows"
	prefix := wrapperMarker + " "
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		if st, err := os.Stat(p); err != nil || !st.Mode().IsRegular() {
			continue
		}
		suffix, stem := pySuffixStem(e.Name())
		if suffix != "" && !(isWindows && suffix == ".bat") {
			continue
		}
		content, err := readTextHead(p, wrapperReadLimit)
		if err != nil {
			continue
		}
		idx := strings.Index(content, prefix)
		if idx == -1 {
			continue
		}
		fields := strings.Fields(content[idx+len(prefix):])
		if len(fields) == 0 {
			continue
		}
		canon, err := NormalizeProfileName(fields[0])
		if err != nil {
			continue
		}
		alias := e.Name()
		if isWindows {
			alias = stem
		}
		if alias == canon {
			if _, ok := result[canon]; !ok {
				result[canon] = alias
			}
		} else {
			result[canon] = alias
		}
	}
	return result, nil
}
