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

// Install methods the install core stamps for the script ladder and the setup app
const (
	MethodScript   = "script"
	MethodSetupApp = "setup-app"
	MethodGit      = "git"
)

// Writes the install method stamp beside the binary of a tree
func WriteInstallMethod(dir, method string) error {
	return os.WriteFile(filepath.Join(dir, InstallMethodFile), []byte(method+"\n"), 0o644)
}

// Renames src over dest, moving a running Windows binary aside first
func ReplaceFile(src, dest string) error { return replaceFile(src, dest) }

// Release a binary self update selected and whether it replaced the installed binary
type BinaryUpdate struct {
	Tag     string
	Commit  string
	Changed bool
}

// Replaces the binary of dir with the release of commit, else the newest release of branch
func UpdateBinary(ctx context.Context, dir, repoURL, commit, branch string, out io.Writer) (BinaryUpdate, error) {
	repo, err := ParseRepo(repoURL)
	if err != nil {
		return BinaryUpdate{}, err
	}
	var target *ReleaseTarget
	if commit != "" {
		target, err = releaseForCommit(ctx, repo, commit)
		if err != nil {
			return BinaryUpdate{}, fmt.Errorf("could not list releases of %s: %w", repo.Web, err)
		}
		if target == nil {
			return BinaryUpdate{}, fmt.Errorf("no published release of %s was built from commit %s", repo.Web, commit)
		}
	} else {
		target, err = releaseForBranch(ctx, repo, branch)
		if err != nil {
			return BinaryUpdate{}, fmt.Errorf("could not list releases of %s: %w", repo.Web, err)
		}
		if target == nil {
			return BinaryUpdate{}, fmt.Errorf("no published release of %s follows branch %s", repo.Web, branch)
		}
	}
	dest := InstallBinaryPath(dir)
	current := InstalledRelease(ctx, dest)
	if current.Tag == target.Tag && (current.Commit == "" || target.Commit == "" || current.Commit == target.Commit) {
		if target.Commit == "" {
			target.Commit = current.Commit
		}
		return BinaryUpdate{Tag: target.Tag, Commit: target.Commit}, nil
	}
	goos, goarch := hostKey()
	name := BinaryAssetName(goos, goarch)
	sum, err := target.ExpectedSHA256(ctx, name)
	if err != nil {
		return BinaryUpdate{}, err
	}
	staged := dest + ".download"
	fmt.Fprintf(out, "→ Downloading Icebath Agent %s (%s-%s)...\n", target.Tag, goos, goarch)
	d := pm.NewDownload([]pm.Source{{URL: target.AssetURL(name), Dest: staged, SHA256: sum}}, pm.DownloadOptions{PartialsDir: filepath.Join(Home(), "bootstrap-cache", "partials")})
	stop := context.AfterFunc(ctx, d.Pause)
	defer stop()
	if _, err := d.Run(func(int64, int64, []pm.RangeSet) error { return nil }); err != nil {
		os.Remove(staged)
		if ctx.Err() != nil {
			return BinaryUpdate{}, ctx.Err()
		}
		if isDigestMismatch(err) {
			return BinaryUpdate{}, fmt.Errorf("%s digest mismatch from %s (expected %s): %w", filepath.Base(staged), target.AssetURL(name), sum, err)
		}
		return BinaryUpdate{}, fmt.Errorf("failed to download %s: %w", target.AssetURL(name), err)
	}
	if err := os.Chmod(staged, 0o755); err != nil {
		os.Remove(staged)
		return BinaryUpdate{}, fmt.Errorf("cannot mark %s executable: %w", staged, err)
	}
	if err := replaceFile(staged, dest); err != nil {
		return BinaryUpdate{}, fmt.Errorf("cannot place %s: %w", dest, err)
	}
	if target.Commit == "" {
		target.Commit = InstalledRelease(ctx, dest).Commit
	}
	fmt.Fprintf(out, "✓ Icebath Agent %s installed at %s\n", target.Tag, dest)
	return BinaryUpdate{Tag: target.Tag, Commit: target.Commit, Changed: true}, nil
}
