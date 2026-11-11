package stageduploads

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/installidentity"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Layout of a staged browser upload under a profile home
const (
	UploadsDir             = "uploads"
	StagedPrefix           = "web-"
	ChunkBytes             = 1024 * 1024
	UploadRetentionSeconds = 7 * 24 * 60 * 60
)

var safeFilenameRE = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// Value error a staged upload receipt or source is refused with
type ValueError struct{ Msg string }

func (e *ValueError) Error() string      { return e.Msg }
func (e *ValueError) PyTypeName() string { return "ValueError" }

// Bounded basename of a picked file keeping its sanitized extension, attachment when nothing safe is left
func SafeFilename(value string) string {
	if value == "" {
		value = "attachment"
	}
	stem := pystr.StripChars(safeFilenameRE.ReplaceAllString(pypath.PathStem(value), "-"), ".-")
	if stem == "" {
		stem = "attachment"
	}
	suffix := pystr.RstripChars(safeFilenameRE.ReplaceAllString(pypath.PathSuffix(value), "-"), ".-")
	if len(suffix) > 119 {
		suffix = suffix[:119]
	}
	if keep := 120 - len(suffix); len(stem) > keep {
		stem = stem[:keep]
	}
	return stem + suffix
}

// Removes staged regular files older than the retention window, never following symlinks
func PruneStaleUploads(root string, now *float64) {
	current := float64(time.Now().UnixNano()) / 1e9
	if now != nil {
		current = *now
	}
	cutoff := current - UploadRetentionSeconds
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), StagedPrefix) {
			continue
		}
		path := filepath.Join(root, entry.Name())
		info, err := os.Lstat(path)
		if err != nil {
			continue
		}
		mtime := float64(info.ModTime().UnixNano()) / 1e9
		if info.Mode().IsRegular() && mtime < cutoff {
			_ = os.Remove(path)
		}
	}
}

// Staged upload receipt of a published file, nil without an install identity
func StagedUploadDescriptor(ctx context.Context, path, profileHome string) (*pyjson.Object, error) {
	installID, err := installidentity.GetInstallID(ctx, nil)
	if err != nil {
		return nil, err
	}
	if installID == "" {
		return nil, nil
	}
	return pyjson.ObjectOf("install_id", installID, "path", path, "profile_home", profileHome), nil
}

// Profile home and path of a receipt this installation issued
func ParseStagedUpload(ctx context.Context, descriptor any) (string, string, error) {
	d, ok := descriptor.(map[string]any)
	if !ok {
		if o, isObj := descriptor.(*pyjson.Object); isObj && o != nil {
			d, ok = o.Vals, true
		}
	}
	if !ok {
		return "", "", &ValueError{"invalid staged upload"}
	}
	installID, err := installidentity.GetInstallID(ctx, nil)
	if err != nil {
		return "", "", err
	}
	if got, isStr := d["install_id"].(string); installID == "" || !isStr || got != installID {
		return "", "", &ValueError{"Staged attachment belongs to another icebath backend; select the file again"}
	}
	rawHome, homeOK := d["profile_home"].(string)
	rawPath, pathOK := d["path"].(string)
	if !homeOK || rawHome == "" || !pathOK || rawPath == "" {
		return "", "", &ValueError{"invalid staged upload source"}
	}
	return rawHome, rawPath, nil
}

// Strictly resolved absolute path, the not found error when any component is missing
func resolveStrict(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}

// Whether two paths are equal as pathlib compares them on this host
func samePath(a, b string) bool {
	return pypath.Normcase(pypath.PathStr(a)) == pypath.Normcase(pypath.PathStr(b))
}

// Staged regular file a raw path names under home within the browser size cap
func ResolveStagedUpload(h, rawPath string) (string, error) {
	if err := home.AssertProfileLive(h); err != nil {
		return "", err
	}
	if info, err := os.Lstat(rawPath); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return "", &ValueError{"staged upload is no longer a regular file"}
	}
	source, err := resolveStrict(rawPath)
	if err != nil {
		return "", err
	}
	uploads, err := resolveStrict(filepath.Join(h, UploadsDir))
	if err != nil {
		return "", err
	}
	info, statErr := os.Stat(source)
	if !samePath(filepath.Dir(source), uploads) || !strings.HasPrefix(filepath.Base(source), StagedPrefix) || statErr != nil || !info.Mode().IsRegular() {
		return "", &ValueError{"staged upload is outside its source profile"}
	}
	if info.Size() > home.WebappAttachmentMaxBytes {
		return "", &ValueError{"staged upload exceeds the browser attachment size limit"}
	}
	return source, nil
}

// Whether a resolved target is a regular file directly in the staged uploads folder of home
func IsStagedUpload(h, target string) bool {
	uploads, err := resolveStrict(filepath.Join(h, UploadsDir))
	if err != nil {
		return false
	}
	if !samePath(filepath.Dir(target), uploads) || !strings.HasPrefix(filepath.Base(target), StagedPrefix) {
		return false
	}
	info, err := os.Stat(target)
	return err == nil && info.Mode().IsRegular()
}
