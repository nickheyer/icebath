//go:build unix && !darwin

package userutil

import (
	"errors"
	"os/exec"
	"strings"
)

// Accounts the name service switch holds for a name or user id key, every one it can list for an empty key, nil when it has none
func directoryAccounts(_, key string) ([]Account, error) {
	lines, err := getent("passwd", key)
	var out []Account
	for _, line := range lines {
		if fields, ok := splitRecord(line, 7); ok {
			if a, ok := parseAccount(fields); ok {
				out = append(out, a)
			}
		}
	}
	return out, err
}

// Groups the name service switch holds for a group id key
func directoryGroups(key string) ([]Group, error) {
	lines, err := getent("group", key)
	var out []Group
	for _, line := range lines {
		if fields, ok := splitRecord(line, 4); ok {
			if g, ok := parseGroup(fields); ok {
				out = append(out, g)
			}
		}
	}
	return out, err
}

// Lines getent prints for a database and key, every entry for an empty key, nil when the key is unknown or the database cannot be listed
func getent(database, key string) ([]string, error) {
	if strings.HasPrefix(key, "-") {
		return nil, nil
	}
	tool := findTool("getent", "/usr/bin/getent", "/bin/getent", "/usr/local/bin/getent", "/usr/sbin/getent")
	if tool == "" {
		return nil, errNoDirectory
	}
	args := []string{database}
	if key != "" {
		args = append(args, key)
	}
	out, err := runDirectoryTool(tool, args...)
	var exit *exec.ExitError
	if errors.As(err, &exit) && (exit.ExitCode() == 2 || exit.ExitCode() == 3) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var lines []string
	for line := range strings.Lines(string(out)) {
		if line = strings.TrimRight(line, "\r\n"); line != "" {
			lines = append(lines, line)
		}
	}
	return lines, nil
}
