package tarutil

import (
	"archive/tar"
	"compress/gzip"
	"os"
	"path/filepath"
)

// Writes base.tar.gz in GNU format holding the tree at rootDir/baseDir under the name baseDir, entries in sorted order, symbolic links stored as links and every hard link as a file of its own, through a temp file beside it that is flushed and renamed into place once complete and removed on any failure or panic; gives back the archive path
func CreateTarGz(base, rootDir, baseDir string) (string, error) {
	archive := base + ".tar.gz"
	f, err := os.CreateTemp(filepath.Dir(archive), ".archive_*.tar.gz.tmp")
	if err != nil {
		return "", err
	}
	tmp := f.Name()
	renamed := false
	defer func() {
		if !renamed {
			f.Close()
			os.Remove(tmp)
		}
	}()
	gz, err := gzip.NewWriterLevel(f, gzip.BestCompression)
	if err != nil {
		return "", err
	}
	tw := tar.NewWriter(gz)
	adder := &Adder{W: tw, Format: tar.FormatGNU, FlattenHardLinks: true}
	if err := adder.Add(filepath.Join(rootDir, baseDir), baseDir, true, nil); err != nil {
		return "", err
	}
	if err := tw.Close(); err != nil {
		return "", err
	}
	if err := gz.Close(); err != nil {
		return "", err
	}
	if err := f.Sync(); err != nil {
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, archive); err != nil {
		return "", err
	}
	renamed = true
	return archive, nil
}
