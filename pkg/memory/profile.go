package memory

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
)

// Bare directory or file stem a provider name must be so a hand edited config cannot aim the copy elsewhere
var providerNameRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// External memory.provider named in a parsed config, empty for the built in store or an unsafe name
func ExternalMemoryProvider(cfg map[string]any) string {
	memory, ok := cfg["memory"].(map[string]any)
	if !ok {
		return ""
	}
	name, ok := memory["provider"].(string)
	if !ok || IsCoreMemoryProvider(name) {
		return ""
	}
	name = pyStrip(name)
	if name == "." || name == ".." || !providerNameRe.MatchString(name) {
		return ""
	}
	return name
}

// Copies the provider directory and json file from source into the profile owner only, true when anything copied
func CloneMemoryProviderConfig(sourceDir, profileDir, provider string) (bool, error) {
	if provider == "" {
		return false, nil
	}
	copied := false
	srcDir := filepath.Join(sourceDir, provider)
	if st, err := os.Stat(srcDir); err == nil && st.IsDir() {
		dst := filepath.Join(profileDir, provider)
		if err := copyTree(srcDir, dst); err != nil {
			return copied, err
		}
		_ = filepath.WalkDir(dst, func(path string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				_ = os.Chmod(path, 0o600)
			}
			return nil
		})
		copied = true
	}
	srcFile := filepath.Join(sourceDir, provider+".json")
	if st, err := os.Stat(srcFile); err == nil && st.Mode().IsRegular() {
		dst := filepath.Join(profileDir, provider+".json")
		if err := copyFileWithStat(srcFile, dst); err != nil {
			return copied, err
		}
		_ = os.Chmod(dst, 0o600)
		copied = true
	}
	return copied, nil
}

// Error of a tree copy listing every entry that failed, raised once the rest of the tree is copied
type copyTreeError struct {
	failures []string
}

func (e *copyTreeError) Error() string {
	return "[" + strings.Join(e.failures, ", ") + "]"
}

// Recursive copy following symlinks into an existing or new destination, file metadata kept like copy2
func copyTree(src, dst string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dst, 0o777); err != nil {
		return err
	}
	var failures []string
	for _, entry := range entries {
		from := filepath.Join(src, entry.Name())
		to := filepath.Join(dst, entry.Name())
		st, err := os.Stat(from)
		if err != nil {
			failures = append(failures, fmt.Sprintf("(%s, %s, %s)", pyRepr(from), pyRepr(to), pyRepr(osErrorText(err))))
			continue
		}
		if st.IsDir() {
			if err := copyTree(from, to); err != nil {
				var tree *copyTreeError
				if errors.As(err, &tree) {
					failures = append(failures, tree.failures...)
				} else {
					failures = append(failures, fmt.Sprintf("(%s, %s, %s)", pyRepr(from), pyRepr(to), pyRepr(osErrorText(err))))
				}
			}
			continue
		}
		if err := copyFileWithStat(from, to); err != nil {
			failures = append(failures, fmt.Sprintf("(%s, %s, %s)", pyRepr(from), pyRepr(to), pyRepr(osErrorText(err))))
		}
	}
	if st, err := os.Stat(src); err == nil {
		if err := os.Chmod(dst, st.Mode().Perm()); err == nil {
			_ = os.Chtimes(dst, st.ModTime(), st.ModTime())
		} else {
			failures = append(failures, fmt.Sprintf("(%s, %s, %s)", pyRepr(src), pyRepr(dst), pyRepr(osErrorText(err))))
		}
	}
	if len(failures) > 0 {
		return &copyTreeError{failures: failures}
	}
	return nil
}

// Copies file bytes then the permission bits and modification time of the source
func copyFileWithStat(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o666)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if err := os.Chmod(dst, st.Mode().Perm()); err != nil {
		return err
	}
	return os.Chtimes(dst, st.ModTime(), st.ModTime())
}

// Mapping of a profile config.yaml, nil when absent, unreadable, or not a mapping
func loadProfileConfigDict(profileDir string) map[string]any {
	path := filepath.Join(profileDir, "config.yaml")
	if _, err := os.Stat(path); err != nil {
		return nil
	}
	doc, err := config.ReadDocument(path)
	if err != nil {
		return nil
	}
	return doc
}

// External provider whose config the profile now carries, empty when nothing was cloned for it
func ClonedMemoryProvider(profileDir string) string {
	provider := ExternalMemoryProvider(loadProfileConfigDict(profileDir))
	if provider == "" {
		return ""
	}
	if st, err := os.Stat(filepath.Join(profileDir, provider)); err == nil && st.IsDir() {
		return provider
	}
	if st, err := os.Stat(filepath.Join(profileDir, provider+".json")); err == nil && st.Mode().IsRegular() {
		return provider
	}
	return ""
}

// Profile memory config cloning has no import time effects, profile create calls it directly
func registerProfileMemoryConfigClone() {}
