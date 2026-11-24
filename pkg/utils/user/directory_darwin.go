package userutil

import (
	"strconv"
	"strings"
)

// Accounts Directory Services holds for attr (name or uid) equal to key, every account for an empty attr, nil when it has none
func directoryAccounts(attr, key string) ([]Account, error) {
	blocks, err := dscacheutil("user", attr, key)
	var out []Account
	for _, b := range blocks {
		uid, err1 := strconv.Atoi(b["uid"])
		gid, err2 := strconv.Atoi(b["gid"])
		if b["name"] == "" || err1 != nil || err2 != nil {
			continue
		}
		out = append(out, Account{Name: b["name"], UID: uid, GID: gid, Gecos: b["gecos"], Home: b["dir"], Shell: b["shell"]})
	}
	return out, err
}

// Groups Directory Services holds for a group id key with their listed members
func directoryGroups(key string) ([]Group, error) {
	blocks, err := dscacheutil("group", "gid", key)
	var out []Group
	for _, b := range blocks {
		gid, convErr := strconv.Atoi(b["gid"])
		if b["name"] == "" || convErr != nil {
			continue
		}
		out = append(out, Group{Name: b["name"], GID: gid, Members: strings.Fields(b["users"])})
	}
	return out, err
}

// Records dscacheutil prints for a category, each a map of its key: value lines, filtered by one attribute unless attr is empty; none for an empty key or one that would read as an option
func dscacheutil(category, attr, key string) ([]map[string]string, error) {
	if attr != "" && (key == "" || strings.HasPrefix(key, "-")) {
		return nil, nil
	}
	tool := findTool("dscacheutil", "/usr/bin/dscacheutil")
	if tool == "" {
		return nil, errNoDirectory
	}
	args := []string{"-q", category}
	if attr != "" {
		args = append(args, "-a", attr, key)
	}
	out, err := runDirectoryTool(tool, args...)
	if err != nil {
		return nil, err
	}
	var blocks []map[string]string
	var current map[string]string
	for line := range strings.Lines(string(out)) {
		if line = strings.TrimRight(line, "\r\n"); strings.TrimSpace(line) == "" {
			if current != nil {
				blocks = append(blocks, current)
				current = nil
			}
			continue
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		if current == nil {
			current = map[string]string{}
		}
		current[strings.TrimSpace(name)] = strings.TrimSpace(value)
	}
	if current != nil {
		blocks = append(blocks, current)
	}
	return blocks, nil
}
