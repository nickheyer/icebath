package profiles

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
)

// Characters of a candidate wrapper read when probing for the profile launch marker
const wrapperReadLimit = 8192

// Text every generated wrapper carries ahead of the profile name
const wrapperMarker = "icebath -p"

// Locates an executable on PATH, replaced by tests
var lookPath = exec.LookPath

// Runs which or where with a five second ceiling, returning decoded stdout and exit code
var runWhich = func(name string) (string, int, error) {
	tool := "which"
	if runtime.GOOS == "windows" {
		tool = "where"
	}
	res, err := pysubprocess.RunWith(context.Background(), []string{tool, name}, pysubprocess.Options{Timeout: 5 * time.Second})
	if err != nil {
		return "", -1, err
	}
	return pystr.DecodeUTF8Replace(res.Stdout), res.Returncode, nil
}

// Reports whether a which or where launch failed the way subprocess.run raises FileNotFoundError or TimeoutExpired
func whichSkippable(err error) bool {
	var timeout *pysubprocess.TimeoutExpired
	return errors.As(err, &timeout) || errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist)
}

// Directory holding alias wrapper scripts
func WrapperDir() (string, error) {
	h, err := home.UserHome()
	if err != nil {
		return "", err
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

// First n characters of a utf-8-sig text file read the way a text mode open reads them
func readTextHead(path string, n int) (string, error) { return pyfs.ReadTextChars(path, n, true) }

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
	if err != nil {
		if whichSkippable(err) {
			return "", nil
		}
		return "", err
	}
	if code != 0 {
		return "", nil
	}
	lines := pystr.Splitlines(pystr.Strip(stdout), false)
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
		text := fmt.Sprintf("@echo off\r\nicebath -p %s %%*\r\n", profile)
		err = os.WriteFile(path, []byte(strings.ReplaceAll(text, "\n", "\r\n")), 0o666)
	} else {
		exe, lerr := lookPath("icebath")
		if lerr != nil || exe == "" {
			exe = "icebath"
		}
		err = os.WriteFile(path, []byte(fmt.Sprintf("#!/bin/sh\nexec %s -p %s \"$@\"\n", shellQuote(exe), profile)), 0o666)
		if err == nil {
			var st os.FileInfo
			if st, err = os.Stat(path); err == nil {
				err = os.Chmod(path, st.Mode()&(os.ModePerm|os.ModeSetuid|os.ModeSetgid|os.ModeSticky)|0o111)
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

// Python 3.14 PurePath suffix and stem of a file name, a trailing dot counting as a suffix
func pySuffixStem(name string) (string, string) {
	i := strings.LastIndex(name, ".")
	if i > 0 {
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
	if isWindows {
		sort.SliceStable(entries, func(i, j int) bool { return pystr.Lower(entries[i].Name()) < pystr.Lower(entries[j].Name()) })
	}
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
		rest := content[idx+len(prefix):]
		if pystr.Strip(rest) == "" {
			continue
		}
		token := pystr.Strip(pystr.SplitWhitespace(rest, 1)[0])
		if token == "" {
			continue
		}
		canon, err := NormalizeProfileName(token)
		if err != nil {
			return nil, err
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
