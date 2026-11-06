package pyfs

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Refusal of a member by the tarfile data filter, Class naming the tarfile.FilterError subclass
type TarFilterError struct {
	Class string
	Msg   string
}

func (e *TarFilterError) Error() string { return e.Msg }

// Reports whether target equals dest or lies beneath it the way commonpath agrees with dest
func commonWithin(target, dest string) bool {
	if target == dest {
		return true
	}
	prefix := dest
	if !strings.HasSuffix(prefix, string(filepath.Separator)) {
		prefix += string(filepath.Separator)
	}
	return strings.HasPrefix(target, prefix)
}

// Filtered name and mode of a member under the data filter, mode -1 when the filter drops it
func dataFilter(h *tar.Header, dest string) (string, int64, error) {
	name := h.Name
	if strings.HasPrefix(name, "/") || strings.HasPrefix(name, string(filepath.Separator)) {
		name = strings.TrimLeft(name, "/"+string(filepath.Separator))
	}
	if filepath.IsAbs(name) {
		return "", 0, &TarFilterError{"AbsolutePathError", fmt.Sprintf("member %s has an absolute path", pyrepr.StrRepr(h.Name))}
	}
	target := realpathNonStrict(filepath.Join(dest, name))
	if !commonWithin(target, dest) {
		return "", 0, &TarFilterError{"OutsideDestinationError", fmt.Sprintf("%s would be extracted to %s, which is outside the destination", pyrepr.StrRepr(h.Name), pyrepr.StrRepr(target))}
	}
	mode := h.Mode & 0o755
	switch h.Typeflag {
	case tar.TypeReg, tar.TypeRegA, tar.TypeLink, tar.TypeGNUSparse, tar.TypeCont:
		if mode&0o100 == 0 {
			mode &^= 0o111
		}
		mode |= 0o600
	case tar.TypeDir, tar.TypeSymlink:
		mode = -1
	default:
		return "", 0, &TarFilterError{"SpecialFileError", fmt.Sprintf("%s is a special file", pyrepr.StrRepr(h.Name))}
	}
	if h.Typeflag == tar.TypeLink || h.Typeflag == tar.TypeSymlink {
		if filepath.IsAbs(h.Linkname) {
			return "", 0, &TarFilterError{"AbsoluteLinkError", fmt.Sprintf("%s is a link to an absolute path", pyrepr.StrRepr(h.Name))}
		}
		linkTarget := filepath.Join(dest, h.Linkname)
		if h.Typeflag == tar.TypeSymlink {
			linkTarget = filepath.Join(dest, filepath.Dir(name), h.Linkname)
		}
		linkTarget = realpathNonStrict(linkTarget)
		if !commonWithin(linkTarget, dest) {
			return "", 0, &TarFilterError{"LinkOutsideDestinationError", fmt.Sprintf("%s would link to %s, which is outside the destination", pyrepr.StrRepr(h.Name), pyrepr.StrRepr(linkTarget))}
		}
	}
	return name, mode, nil
}

// Extracts an uncompressed tar into destination like TarFile.extractall with filter data
func ExtractTarData(archive, destination string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	return ExtractTarDataReader(f, destination)
}

// Extracts a tar stream into destination like TarFile.extractall with filter data
func ExtractTarDataReader(f io.Reader, destination string) error {
	dest := realpathNonStrict(destination)
	type dirTime struct {
		path  string
		mtime time.Time
	}
	var dirs []dirTime
	reader := tar.NewReader(f)
	for {
		h, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if h.Typeflag == tar.TypeXGlobalHeader || h.Typeflag == tar.TypeXHeader {
			continue
		}
		name, mode, err := dataFilter(h, dest)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, strings.TrimRight(name, "/"))
		if parent := filepath.Dir(target); parent != "" {
			if err := os.MkdirAll(parent, 0o777); err != nil {
				return err
			}
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.Mkdir(target, 0o777); err != nil {
				if info, statErr := os.Stat(target); statErr != nil || !info.IsDir() {
					return err
				}
			}
			dirs = append(dirs, dirTime{target, h.ModTime})
			continue
		case tar.TypeSymlink:
			if _, err := os.Lstat(target); err == nil {
				if err := os.Remove(target); err != nil {
					return err
				}
			}
			if err := os.Symlink(h.Linkname, target); err != nil {
				return err
			}
			continue
		case tar.TypeLink:
			if _, err := os.Lstat(target); err == nil {
				if err := os.Remove(target); err != nil {
					return err
				}
			}
			if err := os.Link(filepath.Join(destination, h.Linkname), target); err != nil {
				return err
			}
		default:
			out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o666)
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, reader); err != nil {
				out.Close()
				return err
			}
			if err := out.Close(); err != nil {
				return err
			}
		}
		if err := os.Chmod(target, os.FileMode(mode)); err != nil {
			return err
		}
		if err := os.Chtimes(target, h.ModTime, h.ModTime); err != nil {
			return err
		}
	}
	sort.Slice(dirs, func(i, j int) bool { return dirs[i].path > dirs[j].path })
	for _, dir := range dirs {
		if err := os.Chtimes(dir.path, dir.mtime, dir.mtime); err != nil {
			return err
		}
	}
	return nil
}
