package dashboard

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/stageduploads"
)

// None, empty and current all name the dashboard's own profile
func isCurrentProfile(profile string) bool {
	requested := pystr.Strip(profile)
	return requested == "" || pystr.Lower(requested) == "current"
}

// Error of a block with target unlinked when the block failed, a cleanup error never masking the cause
func removedOnFailure(target string, block func() error) error {
	err := block()
	if err != nil {
		_ = pypath.Unlink(target)
	}
	return err
}

// Profile home an upload is published into, 400 or 404 for a bad name and 404 for a dead home
func resolveUploadHome(ctx context.Context, profile string) (string, error) {
	var h string
	if isCurrentProfile(profile) {
		h = home.Dir(ctx)
	} else {
		dir, err := resolveProfileDir(pystr.Strip(profile))
		if err != nil {
			return "", err
		}
		h = dir
	}
	if err := home.AssertProfileLive(h); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", NewHTTPException(404, "Profile home is unavailable")
		}
		return "", err
	}
	return h, nil
}

// Subdirectory of a home created without recreating a dead named home, 404, 403 denied or 500 failed otherwise
func ensureProfileSubdir(ctx context.Context, h, name, denied, failed string, mode *os.FileMode) (string, error) {
	directory := filepath.Join(h, name)
	err := func() error {
		if _, err := home.Mkdir(ctx, directory); err != nil {
			return err
		}
		if mode != nil {
			return os.Chmod(directory, *mode)
		}
		return nil
	}()
	if err != nil {
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return "", NewHTTPException(404, "Profile was deleted during upload")
		case errors.Is(err, fs.ErrPermission):
			return "", NewHTTPException(403, denied)
		case pypath.IsOSError(err):
			return "", NewHTTPException(500, failed+": "+pypath.OSErrorText(err))
		}
		return "", err
	}
	return directory, nil
}

// Sixteen random hex digits, the Python secrets.token_hex of eight bytes
func tokenHex8() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Publishes staged bytes as a new owner only file under the profile uploads folder
func publishStagedUpload(ctx context.Context, staged io.ReadSeeker, profileHome, filename string) (string, error) {
	mode := os.FileMode(0o700)
	uploadRoot, err := ensureProfileSubdir(ctx, profileHome, stageduploads.UploadsDir, "Upload directory is not writable",
		"Could not create upload directory", &mode)
	if err != nil {
		return "", err
	}
	stageduploads.PruneStaleUploads(uploadRoot, nil)
	target := filepath.Join(uploadRoot, stageduploads.StagedPrefix+tokenHex8()+"-"+stageduploads.SafeFilename(filename))
	err = removedOnFailure(target, func() error {
		handle, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		werr := func() error {
			if _, err := staged.Seek(0, io.SeekStart); err != nil {
				return err
			}
			buf := make([]byte, stageduploads.ChunkBytes)
			for {
				n, rerr := staged.Read(buf)
				if n > 0 {
					if _, err := handle.Write(buf[:n]); err != nil {
						return err
					}
				}
				if rerr == io.EOF {
					break
				}
				if rerr != nil {
					return rerr
				}
			}
			return handle.Sync()
		}()
		if cerr := handle.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			return werr
		}
		return home.AssertProfileLive(profileHome)
	})
	if err != nil {
		return "", stageFailure(err)
	}
	return target, nil
}

// HTTP error of a failed staging write, 404 for a deleted profile and 500 for any other OS error
func stageFailure(err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return NewHTTPException(404, "Profile was deleted during upload")
	}
	if pypath.IsOSError(err) {
		return NewHTTPException(500, "Could not stage file: "+pypath.OSErrorText(err))
	}
	return err
}
