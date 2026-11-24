package tarutil

import (
	"archive/tar"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Writer of files and trees into the tar stream W, directories in sorted order and symbolic links stored unfollowed; a file met again through another hard link becomes a link member naming the member it was first written as, unless FlattenHardLinks stores its content again, and a non-zero Format is set on every header
type Adder struct {
	W                *tar.Writer
	Format           tar.Format
	FlattenHardLinks bool
	links            map[linkKey][]linkSeen
}

type linkKey struct{ size, mtime int64 }

type linkSeen struct {
	info fs.FileInfo
	name string
}

// Adds the file or directory at path as the member name, a directory's entries below it too when recursive; filter may change each header, or drop the member and a dropped directory's entries by giving back nil; sockets, fifos and devices are skipped, carrying no content and being refused by every extraction here
func (a *Adder) Add(path, name string, recursive bool, filter func(*tar.Header) *tar.Header) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&(fs.ModeSocket|fs.ModeNamedPipe|fs.ModeDevice) != 0 {
		return nil
	}
	link := ""
	if info.Mode()&fs.ModeSymlink != 0 {
		if link, err = os.Readlink(path); err != nil {
			return err
		}
	}
	h, err := tar.FileInfoHeader(info, link)
	if err != nil {
		return err
	}
	h.Name = filepath.ToSlash(name)
	if a.Format != tar.FormatUnknown {
		h.Format = a.Format
	}
	tracked := info.Mode().IsRegular() && !a.FlattenHardLinks
	if tracked {
		if prev, ok := a.seenLink(info); ok {
			h.Typeflag, h.Linkname, h.Size = tar.TypeLink, prev, 0
		}
	}
	if info.IsDir() && !strings.HasSuffix(h.Name, "/") {
		h.Name += "/"
	}
	if filter != nil {
		if h = filter(h); h == nil {
			return nil
		}
	}
	if err := a.W.WriteHeader(h); err != nil {
		return err
	}
	switch {
	case h.Typeflag == tar.TypeReg:
		// Remembered only once written, under the name the filter left, so a later link never names a dropped or renamed member
		if tracked {
			a.remember(info, h.Name)
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		_, err = io.Copy(a.W, f)
		f.Close()
		return err
	case info.IsDir() && recursive:
		entries, err := os.ReadDir(path)
		if err != nil {
			return err
		}
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		slices.Sort(names)
		prefix := strings.TrimSuffix(filepath.ToSlash(name), "/") + "/"
		for _, n := range names {
			if err := a.Add(filepath.Join(path, n), prefix+n, recursive, filter); err != nil {
				return err
			}
		}
	}
	return nil
}

// Member name a file was first written under, when info is another link to it
func (a *Adder) seenLink(info fs.FileInfo) (string, bool) {
	for _, s := range a.links[linkKey{info.Size(), info.ModTime().UnixNano()}] {
		if os.SameFile(s.info, info) {
			return s.name, true
		}
	}
	return "", false
}

func (a *Adder) remember(info fs.FileInfo, name string) {
	if a.links == nil {
		a.links = make(map[linkKey][]linkSeen)
	}
	key := linkKey{info.Size(), info.ModTime().UnixNano()}
	a.links[key] = append(a.links[key], linkSeen{info, name})
}
