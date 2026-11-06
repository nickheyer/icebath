package photon

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/pm"
)

// Files that define the sidecar, node_modules stays out so npm installs inside the mirror
var mirrorFiles = []string{"index.mjs", "package.json", "package-lock.json", "patch-spectrum-mixed-attachments.mjs"}

// NpmErrorLogMaxChars caps how much of the persisted npm failure log is surfaced
const NpmErrorLogMaxChars = 300

// Patched sidecar dir and npm error log, empty values resolve on first use
var (
	SidecarDirOverride string
	NpmErrorLogPath    string
	sidecarMu          sync.Mutex
)

// SourceSidecarDir is the sidecar tree shipped beside the photon plugin
func SourceSidecarDir() string {
	return filepath.Join(pm.RepoRoot(), "plugins", "platforms", "photon", "sidecar")
}

// DirWritable probes file creation in the directory because stat lies on read only mounts
func DirWritable(path string) bool {
	probe := filepath.Join(path, ".icebath-write-probe")
	f, err := os.OpenFile(probe, os.O_CREATE|os.O_WRONLY, 0o666)
	if err != nil {
		return false
	}
	f.Close()
	now := time.Now()
	if err := os.Chtimes(probe, now, now); err != nil {
		return false
	}
	return os.Remove(probe) == nil
}

var dirWritable = DirWritable

func statMtime(fi os.FileInfo) float64 {
	t := fi.ModTime()
	return float64(t.Unix()) + float64(t.Nanosecond())*1e-9
}

// Reports whether the lockfile postdates the npm install marker, false on any stat failure
func lockNewerThanInstall(sidecarDir string) bool {
	lock, err := os.Stat(filepath.Join(sidecarDir, "package-lock.json"))
	if err != nil {
		return false
	}
	marker, err := os.Stat(filepath.Join(sidecarDir, "node_modules", ".package-lock.json"))
	if err != nil {
		return false
	}
	return statMtime(lock) > statMtime(marker)
}

// Deep filecmp.cmp of two regular files, false for any other file type or a stat failure
func filesEqual(a, b string) (bool, error) {
	sa, err := os.Stat(a)
	if err != nil {
		return false, err
	}
	sb, err := os.Stat(b)
	if err != nil {
		return false, err
	}
	if !sa.Mode().IsRegular() || !sb.Mode().IsRegular() {
		return false, nil
	}
	if sa.Size() != sb.Size() {
		return false, nil
	}
	da, err := os.ReadFile(a)
	if err != nil {
		return false, err
	}
	db, err := os.ReadFile(b)
	if err != nil {
		return false, err
	}
	return bytes.Equal(da, db), nil
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// Copies every changed sidecar file into the mirror, leaving node_modules alone
func refreshMirror(source, mirror string) error {
	if err := os.MkdirAll(mirror, 0o777); err != nil {
		return err
	}
	for _, name := range mirrorFiles {
		src, dst := filepath.Join(source, name), filepath.Join(mirror, name)
		if !exists(src) {
			continue
		}
		copyIt := !exists(dst)
		if !copyIt {
			same, err := filesEqual(src, dst)
			if err != nil {
				return err
			}
			copyIt = !same
		}
		if copyIt {
			if err := pyfs.Copy2(src, dst); err != nil {
				return err
			}
		}
	}
	return nil
}

// ResolveSidecarDir picks the env override, a writable or freshly baked source, else the mirror under the home
func ResolveSidecarDir(ctx context.Context, sourceDir string) string {
	source := sourceDir
	if source == "" {
		source = SourceSidecarDir()
	}
	if override := os.Getenv("PHOTON_SIDECAR_DIR"); override != "" {
		return override
	}
	if dirWritable(source) {
		return source
	}
	if exists(filepath.Join(source, "node_modules")) && !lockNewerThanInstall(source) {
		return source
	}
	mirror := filepath.Join(home.Dir(ctx), "photon", "sidecar")
	if err := refreshMirror(source, mirror); err != nil {
		slog.Warn(fmt.Sprintf("[photon] install tree is read-only and mirroring the sidecar to %s failed (%v) — falling back to the read-only source dir; dependency installs will not be possible", mirror, err))
		return source
	}
	return mirror
}

// SidecarDir resolves the sidecar runtime dir once on first use
func SidecarDir(ctx context.Context) string {
	sidecarMu.Lock()
	defer sidecarMu.Unlock()
	if SidecarDirOverride == "" {
		SidecarDirOverride = ResolveSidecarDir(ctx, "")
	}
	return SidecarDirOverride
}

// NpmErrorLog is the persisted npm failure log path beside the sidecar
func NpmErrorLog(ctx context.Context) string {
	if NpmErrorLogPath != "" {
		return NpmErrorLogPath
	}
	return filepath.Join(SidecarDir(ctx), ".photon-npm-error.log")
}
