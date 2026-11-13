//go:build linux || darwin

package state

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"golang.org/x/sys/unix"

	"github.com/nickheyer/icebath/internal/fileutil"
)

// Durably captures the lost WAL generation this process still holds open beside the database and returns the capture directory
func (e *Engine) captureRetiredGeneration(ctx context.Context, path string, sidecars map[string]fileIdentity, trigger string) (string, error) {
	walID, ok := sidecars[walSuffix]
	if !ok {
		return "", &CaptureError{msg: fmt.Sprintf("no recorded WAL generation identity for %s; refusing to guess it by pathname", path)}
	}
	wal, ok := ownDescriptorFor(walID)
	if !ok {
		return "", &CaptureError{msg: fmt.Sprintf("this process no longer holds the retired WAL inode %v of %s", identityList(walID), path)}
	}
	var st unix.Stat_t
	if err := unix.Fstat(wal.fd, &st); err != nil {
		return "", &CaptureError{msg: fmt.Sprintf("cannot stat the retired WAL of %s: %v", path, err), cause: err}
	}
	now := time.Now()
	stem := captureStem(path, now)
	dir := filepath.Dir(path)
	final := filepath.Join(dir, stem)
	for n := 1; exists(final) || exists(final+".partial"); n++ {
		final = filepath.Join(dir, fmt.Sprintf("%s-%d", stem, n))
	}
	staging := final + ".partial"
	if err := e.writeCapture(ctx, path, staging, wal.fd, st.Size, walID, sidecars, trigger, now); err != nil {
		_ = os.RemoveAll(staging)
		var ce *CaptureError
		if errors.As(err, &ce) {
			return "", err
		}
		return "", &CaptureError{msg: fmt.Sprintf("could not write the retired WAL generation of %s under %s: %v", path, staging, err), cause: err}
	}
	if err := os.Rename(staging, final); err != nil {
		_ = os.RemoveAll(staging)
		return "", &CaptureError{msg: fmt.Sprintf("could not write the retired WAL generation of %s under %s: %v", path, staging, err), cause: err}
	}
	if err := fileutil.FsyncDir(dir); err != nil {
		return "", &CaptureError{msg: fmt.Sprintf("could not write the retired WAL generation of %s under %s: %v", path, staging, err), cause: err}
	}
	return final, nil
}

// Copies the WAL, a still held SHM, the main image or header and the manifest into staging, all flushed
func (e *Engine) writeCapture(ctx context.Context, path, staging string, walFD int, walSize int64, walID fileIdentity,
	sidecars map[string]fileIdentity, trigger string, now time.Time) error {
	if err := os.MkdirAll(filepath.Dir(staging), 0o777); err != nil {
		return err
	}
	if err := os.Mkdir(staging, 0o777); err != nil {
		return err
	}
	name := filepath.Base(path)
	walCopy, err := copyRange(fdReader(walFD), filepath.Join(staging, name+walSuffix), walSize)
	if err != nil {
		return err
	}
	walCopy["identity"] = identityList(walID)
	atPath := map[string]any{}
	for suffix, id := range sidecarIdentities(path) {
		atPath[suffix] = identityList(id)
	}
	manifest := map[string]any{
		"version": retiredManifestVersion, "database": path, "trigger": trigger, "pid": os.Getpid(),
		"captured_at": now.UTC().Format("2006-01-02T15:04:05Z"), "go": runtime.Version(), "sqlite": SQLiteVersion(),
		"wal": walCopy, "shm": nil, "path_generation_at_capture": atPath, "note": retiredNote,
	}
	if shmID, ok := sidecars[shmSuffix]; ok {
		if shm, ok := ownDescriptorFor(shmID); ok {
			var st unix.Stat_t
			if unix.Fstat(shm.fd, &st) == nil {
				shmCopy, err := copyRange(fdReader(shm.fd), filepath.Join(staging, name+shmSuffix), st.Size)
				var short *CaptureError
				if errors.As(err, &short) {
					return err
				}
				if err == nil {
					shmCopy["identity"] = identityList(shmID)
					manifest["shm"] = shmCopy
				}
			}
		}
	}
	header, ok := e.preadDB(path, 0, sqliteHeaderBytes)
	if !ok {
		return &CaptureError{msg: fmt.Sprintf("cannot read the main image header of %s", path)}
	}
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	main := map[string]any{"identity": nil, "size": fi.Size(), "header": parseHeader(header)}
	if id, ok := statIdentity(path); ok {
		main["identity"] = identityList(id)
	}
	if fi.Size() <= retiredMainImageMax {
		image, err := copyRange(func(off int64, n int) ([]byte, error) {
			if b, ok := e.preadDB(path, off, n); ok {
				return b, nil
			}
			return nil, nil
		}, filepath.Join(staging, name), fi.Size())
		if err != nil {
			return err
		}
		main["mode"] = "copied"
		for k, v := range image {
			main[k] = v
		}
	} else {
		headerFile := filepath.Join(staging, name+".header")
		if err := writeSynced(headerFile, header); err != nil {
			return err
		}
		main["mode"], main["file"], main["bytes"] = "header_only", filepath.Base(headerFile), len(header)
	}
	manifest["main"] = main
	style := fileutil.JSONStyle{Lines: true, Indent: 2, Sorted: true}
	if err := fileutil.WriteJSON(ctx, filepath.Join(staging, retiredManifest), manifest, style); err != nil {
		return err
	}
	return fileutil.FsyncDir(staging)
}

// Positioned reads from a descriptor SQLite owns, never closing, moving or truncating it
func fdReader(fd int) func(off int64, n int) ([]byte, error) {
	return func(off int64, n int) ([]byte, error) {
		buf := make([]byte, n)
		got, err := unix.Pread(fd, buf, off)
		if err != nil {
			return nil, err
		}
		return buf[:got], nil
	}
}

// Writes data to a new file and flushes it to stable storage before closing
func writeSynced(path string, data []byte) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		return errors.Join(err, f.Close())
	}
	if err := f.Sync(); err != nil {
		return errors.Join(err, f.Close())
	}
	return f.Close()
}
