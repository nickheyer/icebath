package profile

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"
)

// Characters of a candidate alias read when looking for the launch line of one icebath wrote
const aliasHead = 8192

// What every alias icebath writes says ahead of the profile it opens
const aliasMarker = "icebath -p"

// Folder of the aliases, short commands that open icebath on a profile
func AliasDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "bin"), nil
}

// File of an alias in the folder of aliases, a .bat on Windows
func AliasPath(alias string) (string, error) {
	dir, err := AliasDir()
	if err != nil {
		return "", err
	}
	if runtime.GOOS == "windows" {
		return filepath.Join(dir, alias+".bat"), nil
	}
	return filepath.Join(dir, alias), nil
}

// First characters of a text file, a byte order mark dropped
func head(path string, n int) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	buf := make([]byte, n)
	read, err := f.Read(buf)
	if err != nil && read == 0 {
		return "", err
	}
	return strings.TrimPrefix(string(buf[:read]), "\xef\xbb\xbf"), nil
}

// Whether the file is an alias icebath wrote
func ours(path string) bool {
	text, err := head(path, aliasHead)
	return err == nil && strings.Contains(text, aliasMarker)
}

// Alias icebath wrote under the name, and on Windows one written before aliases became .bat files; empty when there is none
func OurAlias(alias string) (string, error) {
	first, err := AliasPath(alias)
	if err != nil {
		return "", err
	}
	candidates := []string{first}
	if runtime.GOOS == "windows" {
		dir, _ := AliasDir()
		candidates = append(candidates, filepath.Join(dir, alias))
	}
	for _, path := range candidates {
		if ours(path) {
			return path, nil
		}
	}
	return "", nil
}

// Why the name can't be an alias, empty when it can: reserved, a command of icebath, or a command already on the PATH that is not an alias
// icebath wrote
func Collision(name string) (string, error) {
	canon, err := Normalize(name)
	if err != nil {
		return "", err
	}
	if err := ValidAlias(canon); err != nil {
		return err.Error(), nil
	}
	if reserved[canon] {
		return fmt.Sprintf("'%s' is a reserved name", canon), nil
	}
	if subcommands[canon] {
		return fmt.Sprintf("'%s' conflicts with an icebath subcommand", canon), nil
	}
	tool := "which"
	if runtime.GOOS == "windows" {
		tool = "where"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, tool, canon).Output()
	if errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "", nil
	}
	if _, failed := errors.AsType[*exec.ExitError](err); failed {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	existing, _, _ := strings.Cut(strings.TrimSpace(strings.ReplaceAll(string(out), "\r\n", "\n")), "\n")
	if existing == "" {
		return "", nil
	}
	expected, err := AliasPath(canon)
	if err != nil {
		return "", err
	}
	if existing == expected && ours(expected) {
		return "", nil
	}
	return fmt.Sprintf("'%s' conflicts with an existing command (%s)", canon, existing), nil
}

// Whether the folder of aliases is on the PATH
func AliasesOnPath() (bool, error) {
	dir, err := AliasDir()
	if err != nil {
		return false, err
	}
	return slices.Contains(filepath.SplitList(os.Getenv("PATH")), dir), nil
}

// Text as a POSIX shell takes it word for word
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	if strings.IndexFunc(s, func(r rune) bool {
		return !(r < 128 && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("@%+=:,./-_", r)))
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// Writes the alias that opens icebath on the profile target, or on the profile of its own name: its file, or an error saying why it could
// not be written
func MakeAlias(name, target string) (string, error) {
	canon, err := Normalize(name)
	if err != nil {
		return "", err
	}
	profile := canon
	if target != "" {
		if profile, err = Normalize(target); err != nil {
			return "", err
		}
	}
	if err := ValidAlias(canon); err != nil {
		return "", err
	}
	dir, err := AliasDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("Could not create %s: %s", dir, failure(err))
	}
	path, err := AliasPath(canon)
	if err != nil {
		return "", err
	}
	if runtime.GOOS == "windows" {
		err = os.WriteFile(path, []byte(fmt.Sprintf("@echo off\r\nicebath -p %s %%*\r\n", profile)), 0o644)
	} else {
		exe, lookErr := exec.LookPath("icebath")
		if lookErr != nil || exe == "" {
			exe = "icebath"
		}
		err = os.WriteFile(path, []byte(fmt.Sprintf("#!/bin/sh\nexec %s -p %s \"$@\"\n", shellQuote(exe), profile)), 0o755)
		if err == nil {
			err = os.Chmod(path, 0o755)
		}
	}
	if err != nil {
		return "", fmt.Errorf("Could not create wrapper at %s: %s", path, failure(err))
	}
	return path, nil
}

// Reason a file operation failed followed by the path it failed on
func failure(err error) string {
	if pathErr, ok := errors.AsType[*fs.PathError](err); ok {
		return pathErr.Err.Error() + ": " + pathErr.Path
	}
	return err.Error()
}

// Removes the alias icebath wrote under the name, whether there was one
func RemoveAlias(name string) (bool, error) {
	canon, err := Normalize(name)
	if err != nil {
		return false, err
	}
	if ValidAlias(canon) != nil {
		return false, nil
	}
	path, err := OurAlias(canon)
	if err != nil || path == "" {
		return false, err
	}
	return os.Remove(path) == nil, nil
}

// Moves the alias of a renamed profile to its new name, made for it even when the old name had none: the lines saying how that went
func FollowAlias(old, renamed string) []string {
	if _, err := RemoveAlias(old); err != nil {
		return []string{"⚠ " + failure(err)}
	}
	collision, err := Collision(renamed)
	switch {
	case err != nil:
		return []string{"⚠ " + failure(err)}
	case collision != "":
		return []string{fmt.Sprintf("⚠ Cannot create alias '%s' — %s", renamed, collision)}
	}
	if _, err := MakeAlias(renamed, ""); err != nil {
		return []string{"⚠ " + err.Error()}
	}
	return []string{"✓ Alias updated: " + renamed}
}

// Removes the alias icebath wrote for a deleted profile: the line saying it went, none when there was none
func DropAlias(name string) []string {
	path, err := OurAlias(name)
	if err != nil {
		return []string{"⚠ " + failure(err)}
	}
	if path == "" {
		return nil
	}
	if removed, err := RemoveAlias(name); err != nil || !removed {
		return []string{fmt.Sprintf("⚠ Could not remove %s", path)}
	}
	return []string{"✓ Removed " + path}
}

// Alias that opens the profile, empty when none does
func AliasOf(profile string) (string, error) {
	canon, err := Normalize(profile)
	if err != nil {
		return "", err
	}
	aliases, err := Aliases()
	return aliases[canon], err
}

// Alias of each profile, read in one pass over the folder of aliases, one named otherwise winning over one named for the profile
func Aliases() (map[string]string, error) {
	out := map[string]string{}
	dir, err := AliasDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	windows := runtime.GOOS == "windows"
	if windows {
		slices.SortStableFunc(entries, func(a, b os.DirEntry) int {
			return strings.Compare(strings.ToLower(a.Name()), strings.ToLower(b.Name()))
		})
	}
	for _, entry := range entries {
		path := filepath.Join(dir, entry.Name())
		if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
			continue
		}
		// The extension after the last dot, none for a name whose only dot leads it
		ext := ""
		if at := strings.LastIndex(entry.Name(), "."); at > 0 {
			ext = entry.Name()[at:]
		}
		if ext != "" && !(windows && ext == ".bat") {
			continue
		}
		content, err := head(path, aliasHead)
		if err != nil {
			continue
		}
		at := strings.Index(content, aliasMarker+" ")
		if at < 0 {
			continue
		}
		fields := strings.Fields(content[at+len(aliasMarker)+1:])
		if len(fields) == 0 {
			continue
		}
		canon, err := Normalize(fields[0])
		if err != nil {
			continue
		}
		alias := entry.Name()
		if windows {
			alias = strings.TrimSuffix(alias, ext)
		}
		if _, have := out[canon]; alias != canon || !have {
			out[canon] = alias
		}
	}
	return out, nil
}
