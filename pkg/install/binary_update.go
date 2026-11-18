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

// Install methods the install core stamps for the script ladder, the WinGet nested installer and a source checkout
const (
	MethodScript = "script"
	MethodWinGet = "winget"
	MethodGit    = "git"
)

// Writes the install method stamp beside the binary of a tree
func WriteInstallMethod(dir, method string) error {
	return os.WriteFile(filepath.Join(dir, InstallMethodFile), []byte(method+"\n"), 0o644)
}

// Renames src over dest, moving a running Windows binary aside first
func ReplaceFile(src, dest string) error { return replaceFile(src, dest) }

// Downloads the target release's archive for this host, verifies its checksum and swaps its binary and config templates into dir
func ApplyRelease(ctx context.Context, dir string, target *ReleaseTarget, out io.Writer) error {
	goos, goarch := hostKey()
	name := ReleaseArchiveName(target.Tag, goos, goarch)
	sum, err := target.ExpectedSHA256(ctx, name)
	if err != nil {
		return err
	}
	unpacked, err := os.MkdirTemp(dir, ".icebath-release-")
	if err != nil {
		return fmt.Errorf("cannot stage the release in %s: %w", dir, err)
	}
	defer os.RemoveAll(unpacked)
	staged := filepath.Join(unpacked, name)
	fmt.Fprintf(out, "→ Downloading Icebath Agent %s (%s-%s)...\n", target.Tag, goos, goarch)
	d := pm.NewDownload([]pm.Source{{URL: target.AssetURL(name), Dest: staged, SHA256: sum}}, pm.DownloadOptions{PartialsDir: filepath.Join(Home(), "bootstrap-cache", "partials")})
	stop := context.AfterFunc(ctx, d.Pause)
	defer stop()
	if _, err := d.Run(func(int64, int64, []pm.RangeSet) error { return nil }); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if isDigestMismatch(err) {
			return fmt.Errorf("%s digest mismatch from %s (expected %s): %w", name, target.AssetURL(name), sum, err)
		}
		return fmt.Errorf("failed to download %s: %w", target.AssetURL(name), err)
	}
	if err := extractReleaseArchive(staged, unpacked, goos); err != nil {
		return err
	}
	if err := placeUnpackedRelease(unpacked, dir, goos); err != nil {
		return err
	}
	fmt.Fprintf(out, "✓ Icebath Agent %s installed at %s\n", target.Tag, InstallBinaryPath(dir))
	return nil
}
