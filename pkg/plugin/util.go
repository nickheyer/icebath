package plugin

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/pkg/agent"
)

// JSON of a value, empty when it cannot be encoded
func jsonText(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(raw)
}

// Hook configuration read from a YAML map, by way of its JSON
func hooksFromMap(m map[string]any) (agent.HookConfig, error) {
	raw, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	return agent.ParseHooks(raw, false)
}

// Reports whether a component path starts with ./ or is the root itself
func relativePath(p string) bool {
	return p == "." || p == "./" || strings.HasPrefix(p, "./")
}

// Absolute path of a component path inside the root, refusing one that escapes it
func inside(root, p string) (string, error) {
	if strings.Contains(p, "..") {
		return "", fmt.Errorf("path %q contains .., which could be a path traversal attempt", p)
	}
	if filepath.Separator != '\\' && strings.Contains(p, "\\") {
		return "", fmt.Errorf("path %q holds a backslash", p)
	}
	full := filepath.Clean(filepath.Join(root, p))
	resolved, err := filepath.EvalSymlinks(full)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("path not found: %s", p)
		}
		return "", err
	}
	rootResolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		rootResolved = root
	}
	rel, err := filepath.Rel(rootResolved, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes plugin directory: %s", p)
	}
	return full, nil
}

// Reports whether the path exists
func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// Reports whether the path is a directory
func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// Path with a leading ~ replaced by the home directory
func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[1:])
		}
	}
	return p
}

// Strings of a JSON value that is a string or a list of strings
func stringList(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var one string
	if err := json.Unmarshal(raw, &one); err == nil {
		return []string{one}, nil
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err != nil {
		return nil, errors.New("expected a path or a list of paths")
	}
	return many, nil
}

// Id of a plugin with every character other than a letter, digit, underscore or dash turned into a dash
func DataID(id string) string {
	var b strings.Builder
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}

// Hex digest of a text
func sha256Sum(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// Digest of a text, for telling repeated content apart
func Digest(text string) string { return sha256Sum(text) }

// Reports whether a tool name matches a rule tool glob such as mcp__github__*
func SafeGlob(pattern, name string) bool {
	if pattern == name {
		return true
	}
	if strings.HasSuffix(pattern, "*") {
		return strings.HasPrefix(name, strings.TrimSuffix(pattern, "*"))
	}
	return false
}
