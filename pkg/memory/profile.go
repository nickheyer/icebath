package memory

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/settings"
	"github.com/nickheyer/icebath/pkg/tools"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
)

// Bare directory or file stem a provider name must be so a hand edited config cannot aim the copy elsewhere
var providerNameRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// Name of an external memory provider, empty for none, a core provider or an unsafe name
func externalProviderName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" || IsCoreMemoryProvider(name) || name == "." || name == ".." || !providerNameRe.MatchString(name) {
		return ""
	}
	return name
}

// External memory provider a profile's settings name, empty when it names none
func ProfileMemoryProvider(ctx context.Context, profileDir string) string {
	return externalProviderName(settings.MemoryProvider.Get(home.WithOverride(ctx, profileDir)))
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
	return "could not copy " + strings.Join(e.failures, "; ")
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
			failures = append(failures, fmt.Sprintf("%s -> %s: %s", from, to, osErrorText(err)))
			continue
		}
		if st.IsDir() {
			if err := copyTree(from, to); err != nil {
				var tree *copyTreeError
				if errors.As(err, &tree) {
					failures = append(failures, tree.failures...)
				} else {
					failures = append(failures, fmt.Sprintf("%s -> %s: %s", from, to, osErrorText(err)))
				}
			}
			continue
		}
		if err := copyFileWithStat(from, to); err != nil {
			failures = append(failures, fmt.Sprintf("%s -> %s: %s", from, to, osErrorText(err)))
		}
	}
	if st, err := os.Stat(src); err == nil {
		if err := os.Chmod(dst, st.Mode().Perm()); err == nil {
			_ = os.Chtimes(dst, st.ModTime(), st.ModTime())
		} else {
			failures = append(failures, fmt.Sprintf("%s -> %s: %s", src, dst, osErrorText(err)))
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

// External provider whose config the profile now carries, empty when nothing was cloned for it
func ClonedMemoryProvider(ctx context.Context, profileDir string) string {
	provider := ProfileMemoryProvider(ctx, profileDir)
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

// NotifyMemoryProvider runs call on the memory provider the config of a profile selects with that profile's home and its own secrets bound, best effort so a failing provider never fails the create or rename that called it
func NotifyMemoryProvider(ctx context.Context, profileDir string, call func(ctx context.Context, p Provider) error) {
	name := ""
	failed := func(reason any) {
		pkgLog().Warn(fmt.Sprintf("memory provider %s: profile hook failed for %s: %v", anyutil.Quote(name), profileDir, reason))
	}
	defer func() {
		if r := recover(); r != nil {
			failed(r)
		}
	}()
	if ProfileMemoryProvider(ctx, profileDir) == "" {
		return
	}
	provider.HydrateProfileSecretSources(profileDir)
	scoped := provider.BindProfileSecretScope(home.WithOverride(ctx, profileDir), profileDir)
	env := &tools.Env{Home: profileDir}
	if name = externalProviderName(ActiveMemoryProvider(scoped, env)); name == "" {
		return
	}
	p := LoadMemoryProviderWithSkills(scoped, env, name, false)
	if p == nil {
		return
	}
	defer DismissProvider(scoped, p)
	if err := call(scoped, p); err != nil {
		failed(err)
	}
}
