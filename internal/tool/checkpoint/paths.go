package checkpoint

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var commitHashRE = regexp.MustCompile(`^[0-9a-fA-F]{4,64}$`)

func validateHash(hash string) error {
	if strings.TrimSpace(hash) == "" {
		return &InputError{msg: "Empty commit hash"}
	}
	if strings.HasPrefix(hash, "-") {
		return &InputError{msg: fmt.Sprintf("Invalid commit hash (must not start with '-'): %q", hash)}
	}
	if !commitHashRE.MatchString(hash) {
		return &InputError{msg: fmt.Sprintf("Invalid commit hash (expected 4-64 hex characters): %q", hash)}
	}
	return nil
}

func validateFilePath(file, workdir string) error {
	if strings.TrimSpace(file) == "" {
		return &InputError{msg: "Empty file path"}
	}
	if filepath.IsAbs(file) {
		return &InputError{msg: fmt.Sprintf("File path must be relative, got absolute path: %q", file)}
	}
	root, err := canonical(workdir)
	if err != nil {
		return err
	}
	joined, ok := joinRelative(root, file)
	if !ok {
		return &InputError{msg: fmt.Sprintf("File path escapes the working directory via traversal: %q", file)}
	}
	resolved, err := canonical(joined)
	if err != nil {
		return err
	}
	if _, inside := relativeTo(resolved, root); !inside {
		return &InputError{msg: fmt.Sprintf("File path escapes the working directory via traversal: %q", file)}
	}
	return nil
}

func projectHash(workdir string) (string, error) {
	abs, err := canonical(workdir)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(abs))
	return hex.EncodeToString(sum[:])[:16], nil
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func partsOf(rel string) []string {
	var parts []string
	for _, p := range strings.FieldsFunc(rel, isSeparator) {
		if p != "." {
			parts = append(parts, p)
		}
	}
	return parts
}

func posixParts(rel string) []string {
	var parts []string
	for _, p := range strings.Split(rel, "/") {
		if p != "" && p != "." {
			parts = append(parts, p)
		}
	}
	return parts
}

func hasPrefixParts(parts, prefix []string) bool {
	if len(prefix) > len(parts) {
		return false
	}
	for i := range prefix {
		if parts[i] != prefix[i] {
			return false
		}
	}
	return true
}
