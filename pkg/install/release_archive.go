package install

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Files a release archive carries beside the binary that an install keeps next to it, the commented config templates
var ReleaseCompanionFiles = []string{"cli-config.yaml.example", ".env.example"}

// Release archive name of a version for goos and goarch, the zip on Windows and the gzipped tar elsewhere
func ReleaseArchiveName(version, goos, goarch string) string {
	name := "icebath_" + strings.TrimPrefix(version, "v") + "_" + goos + "_" + goarch
	if goos == "windows" {
		return name + ".zip"
	}
	return name + ".tar.gz"
}

// Binary file name inside a release archive for goos
func archiveBinaryName(goos string) string {
	if goos == "windows" {
		return "icebath.exe"
	}
	return "icebath"
}

// Extracts the binary and the companion files from the archive root into dir, refusing an archive without the binary
func extractReleaseArchive(archive, dir, goos string) error {
	wanted := map[string]bool{archiveBinaryName(goos): true}
	for _, name := range ReleaseCompanionFiles {
		wanted[name] = true
	}
	write := func(name string, mode os.FileMode, r io.Reader) error {
		out, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, r); err != nil {
			out.Close()
			return err
		}
		return out.Close()
	}
	found := map[string]bool{}
	if strings.HasSuffix(archive, ".zip") {
		zr, err := zip.OpenReader(archive)
		if err != nil {
			return fmt.Errorf("cannot open %s: %w", filepath.Base(archive), err)
		}
		defer zr.Close()
		for _, f := range zr.File {
			name := strings.TrimPrefix(f.Name, "./")
			if !wanted[name] || f.FileInfo().IsDir() {
				continue
			}
			rc, err := f.Open()
			if err != nil {
				return fmt.Errorf("cannot read %s from %s: %w", name, filepath.Base(archive), err)
			}
			err = write(name, 0o644, rc)
			rc.Close()
			if err != nil {
				return fmt.Errorf("cannot unpack %s: %w", name, err)
			}
			found[name] = true
		}
	} else {
		in, err := os.Open(archive)
		if err != nil {
			return err
		}
		defer in.Close()
		gz, err := gzip.NewReader(in)
		if err != nil {
			return fmt.Errorf("cannot open %s: %w", filepath.Base(archive), err)
		}
		tr := tar.NewReader(gz)
		for {
			hdr, err := tr.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return fmt.Errorf("cannot read %s: %w", filepath.Base(archive), err)
			}
			name := strings.TrimPrefix(hdr.Name, "./")
			if !wanted[name] || hdr.Typeflag != tar.TypeReg {
				continue
			}
			if err := write(name, 0o644, tr); err != nil {
				return fmt.Errorf("cannot unpack %s: %w", name, err)
			}
			found[name] = true
		}
	}
	if !found[archiveBinaryName(goos)] {
		return fmt.Errorf("%s has no %s", filepath.Base(archive), archiveBinaryName(goos))
	}
	return nil
}

// Places an unpacked release in dir, its binary over the install binary and each companion file beside it
func placeUnpackedRelease(unpacked, dir, goos string) error {
	staged := filepath.Join(unpacked, archiveBinaryName(goos))
	if err := os.Chmod(staged, 0o755); err != nil {
		return fmt.Errorf("cannot mark %s executable: %w", staged, err)
	}
	dest := InstallBinaryPath(dir)
	if err := replaceFile(staged, dest); err != nil {
		return fmt.Errorf("cannot place %s: %w", dest, err)
	}
	return placeCompanionFiles(unpacked, dir)
}

// Copies the companion files present in src into dir, leaving dir's copies where src has none
func placeCompanionFiles(src, dir string) error {
	if samePath(src, dir) {
		return nil
	}
	for _, name := range ReleaseCompanionFiles {
		from := filepath.Join(src, name)
		if !isFile(from) {
			continue
		}
		if err := copyFile(from, filepath.Join(dir, name), 0o644); err != nil {
			return fmt.Errorf("cannot place %s: %w", filepath.Join(dir, name), err)
		}
	}
	return nil
}
