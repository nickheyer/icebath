package install

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/nickheyer/icebath/pkg/pm"
)

// Paths this install would use as the JSON object install.ps1 -ShowResolvedPaths prints
func ResolvedPathsJSON(opts Options) (string, error) {
	r, err := NewRunner(opts, func(Event) {})
	if err != nil {
		return "", err
	}
	userHome, _ := os.UserHomeDir()
	long := func(p string) string {
		if resolved, err := filepath.EvalSymlinks(p); err == nil {
			return resolved
		}
		return p
	}
	report := map[string]any{
		"long_profile_root": long(userHome),
		"normalized":        []string{},
		"resolver":          "install-self",
		"temp":              os.TempDir(),
		"icebath_home":      r.home,
		"install_dir":       r.installDir,
	}
	data, err := json.Marshal(report)
	return string(data), err
}

// User bin the setup script publishes commands into, the home bin on Windows
func setupBinDir(userHome string) string {
	if runtime.GOOS == "windows" {
		return filepath.Join(Home(), "bin")
	}
	return filepath.Join(userHome, ".local", "bin")
}

// Publishes a checkout's built binary as the icebath command, puts the user bin on PATH and seeds the skills dir
func SetupFromCheckout(ctx context.Context, checkout string, report func(string)) error {
	root := checkout
	if !isFile(InstallBinaryPath(root)) {
		return fmt.Errorf("%s is missing; build it before publishing the command", InstallBinaryPath(root))
	}
	if err := WriteInstallMethod(root, MethodGit); err != nil {
		return fmt.Errorf("cannot stamp the install method in %s: %w", root, err)
	}
	userHome, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	bin := setupBinDir(userHome)
	report("→ Setting up icebath command...")
	if _, err := pm.EnsureInstallLaunchers(ctx, root, bin); err != nil {
		return fmt.Errorf("launcher publication failed: %w", err)
	}
	report("✓ Published Icebath commands in " + bin)
	if runtime.GOOS != "windows" {
		if err := setupShellPath(userHome, report); err != nil {
			return err
		}
	}
	return os.MkdirAll(filepath.Join(Home(), "skills"), 0o777)
}

// One guarded user bin export in the login shell rc unless PATH or that rc already names it
func setupShellPath(userHome string, report func(string)) error {
	shell := os.Getenv("SHELL")
	rc := ""
	switch {
	case strings.Contains(shell, "zsh"):
		rc = filepath.Join(userHome, ".zshrc")
	case strings.Contains(shell, "bash"):
		rc = filepath.Join(userHome, ".bashrc")
		if !isFile(rc) {
			rc = filepath.Join(userHome, ".bash_profile")
		}
	default:
		for _, name := range []string{".zshrc", ".bashrc", ".bash_profile"} {
			if isFile(filepath.Join(userHome, name)) {
				rc = filepath.Join(userHome, name)
				break
			}
		}
	}
	if rc == "" {
		return nil
	}
	bin := filepath.Join(userHome, ".local", "bin")
	for _, entry := range filepath.SplitList(os.Getenv("PATH")) {
		if entry == bin {
			report("✓ ~/.local/bin already on PATH")
			return nil
		}
	}
	data, _ := os.ReadFile(rc)
	if strings.Contains(string(data), ".local/bin") {
		report("✓ ~/.local/bin already in " + rc)
		return nil
	}
	f, err := os.OpenFile(rc, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(f, "\n# Icebath Agent — ensure ~/.local/bin is on PATH\nexport PATH=\"$HOME/.local/bin:$PATH\"\n"); err != nil {
		f.Close()
		return err
	}
	report("✓ Added ~/.local/bin to PATH in " + rc)
	return f.Close()
}

// Copies a checkout's bundled skills into the home skills dir without replacing anything there
func CopySkillsNoClobber(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o777)
		}
		if _, err := os.Lstat(target); err == nil {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		return copyFile(path, target, info.Mode().Perm())
	})
}
