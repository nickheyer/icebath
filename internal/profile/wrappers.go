package profile

import (
	"context"
	"errors"
	"fmt"
	"github.com/nickheyer/icebath/pkg/strs"
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
		return fmt.Sprintf("%s is a name icebath keeps for itself.", canon), nil
	}
	if subcommands[canon] {
		return fmt.Sprintf("%s is the name of an icebath command.", canon), nil
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
	return fmt.Sprintf("%s is already a command on this computer, at %s.", canon, existing), nil
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
		return "", fmt.Errorf("The folder %s could not be made: %s.", dir, failure(err))
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
		return "", fmt.Errorf("The alias %s could not be written: %s.", path, failure(err))
	}
	return path, nil
}

// Reason a file operation failed followed by the path it failed on
func failure(err error) string {
	if pathErr, ok := errors.AsType[*fs.PathError](err); ok {
		return pathErr.Err.Error() + " at " + pathErr.Path
	}
	return strings.TrimSuffix(strings.TrimSpace(err.Error()), ".")
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

// Writes the shell command that opens icebath on the profile, named after it: what was done, or why nothing was, each a sentence
func Command(name string) (done, failed string) {
	collision, err := Collision(name)
	switch {
	case err != nil:
		return "", "The shell command could not be made: " + failure(err) + "."
	case collision != "":
		return "", "No shell command was made, because " + strs.Lower(strings.TrimSuffix(collision, ".")) + "."
	}
	path, err := MakeAlias(name, "")
	if err != nil {
		return "", strs.Sentence(err.Error())
	}
	if onPath, err := AliasesOnPath(); err == nil && !onPath {
		dir, _ := AliasDir()
		return "Made the shell command " + Tilde(path) + ". Add " + Tilde(dir) + " to your PATH to run it by name.", ""
	}
	canon, _ := Normalize(name)
	return "The shell command " + canon + " opens icebath on this profile.", ""
}

// Moves the shell command of a renamed profile to its new name, made for it even when the old name had none: what was done, or what failed
func FollowAlias(old, renamed string) (done, failed string) {
	if _, err := RemoveAlias(old); err != nil {
		return "", "The shell command " + old + " could not be removed: " + failure(err) + "."
	}
	collision, err := Collision(renamed)
	switch {
	case err != nil:
		return "", "No shell command named " + renamed + " was made: " + failure(err) + "."
	case collision != "":
		return "", "No shell command named " + renamed + " was made, because " + strs.Lower(strings.TrimSuffix(collision, ".")) + "."
	}
	if _, err := MakeAlias(renamed, ""); err != nil {
		return "", strs.Sentence(err.Error())
	}
	return "The shell command is " + renamed + " now.", ""
}

// Removes the shell command icebath wrote for a deleted profile: what was done, nothing when there was none, or what failed
func DropAlias(name string) (done, failed string) {
	path, err := OurAlias(name)
	if err != nil {
		return "", "icebath could not look for the shell command " + name + ": " + failure(err) + "."
	}
	if path == "" {
		return "", ""
	}
	if removed, err := RemoveAlias(name); err != nil || !removed {
		return "", "The shell command " + Tilde(path) + " could not be removed."
	}
	return "Removed the shell command " + Tilde(path) + ".", ""
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
