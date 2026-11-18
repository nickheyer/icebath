package install

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/nickheyer/icebath/pkg/pm"
)

// Stamp file beside the binary naming how the tree was installed
const InstallMethodFile = ".install_method"

// Install methods the install core stamps for the script ladder and a source checkout
const (
	MethodScript = "script"
	MethodGit    = "git"
)

// Writes the install method stamp beside the binary of a tree
func WriteInstallMethod(dir, method string) error {
	return os.WriteFile(filepath.Join(dir, InstallMethodFile), []byte(method+"\n"), 0o644)
}

// Renames src over dest, moving a running Windows binary aside first
func ReplaceFile(src, dest string) error { return replaceFile(src, dest) }

// Downloads the target release's binary for this host, verifies its checksum and swaps it over the binary of dir
func ApplyRelease(ctx context.Context, dir string, target *ReleaseTarget, out io.Writer) error {
	dest := InstallBinaryPath(dir)
	goos, goarch := hostKey()
	name := BinaryAssetName(goos, goarch)
	sum, err := target.ExpectedSHA256(ctx, name)
	if err != nil {
		return err
	}
	staged := dest + ".download"
	fmt.Fprintf(out, "→ Downloading Icebath Agent %s (%s-%s)...\n", target.Tag, goos, goarch)
	d := pm.NewDownload([]pm.Source{{URL: target.AssetURL(name), Dest: staged, SHA256: sum}}, pm.DownloadOptions{PartialsDir: filepath.Join(Home(), "bootstrap-cache", "partials")})
	stop := context.AfterFunc(ctx, d.Pause)
	defer stop()
	if _, err := d.Run(func(int64, int64, []pm.RangeSet) error { return nil }); err != nil {
		os.Remove(staged)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if isDigestMismatch(err) {
			return fmt.Errorf("%s digest mismatch from %s (expected %s): %w", filepath.Base(staged), target.AssetURL(name), sum, err)
		}
		return fmt.Errorf("failed to download %s: %w", target.AssetURL(name), err)
	}
	if err := os.Chmod(staged, 0o755); err != nil {
		os.Remove(staged)
		return fmt.Errorf("cannot mark %s executable: %w", staged, err)
	}
	if err := replaceFile(staged, dest); err != nil {
		return fmt.Errorf("cannot place %s: %w", dest, err)
	}
	fmt.Fprintf(out, "✓ Icebath Agent %s installed at %s\n", target.Tag, dest)
	return nil
}
