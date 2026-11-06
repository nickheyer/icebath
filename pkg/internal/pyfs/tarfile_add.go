package pyfs

import (
	"archive/tar"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// TarAdder writes paths into a tar stream like tarfile.TarFile.add, tracking hard links across calls
type TarAdder struct {
	W     *tar.Writer
	links map[[2]int64][]tarLinkSeen
}

type tarLinkSeen struct {
	info os.FileInfo
	name string
}

// Adds path as arcname, recursing into directories in sorted order, a nil filter result skips the member and its children
func (a *TarAdder) Add(path, arcname string, recursive bool, filter func(h *tar.Header) *tar.Header) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	link := ""
	if info.Mode()&os.ModeSymlink != 0 {
		if link, err = os.Readlink(path); err != nil {
			return err
		}
	}
	h, err := tar.FileInfoHeader(info, link)
	if err != nil {
		if info.Mode()&os.ModeSocket != 0 {
			return nil
		}
		return err
	}
	h.Name = filepath.ToSlash(arcname)
	h.Mode = int64(info.Mode().Perm()) | h.Mode&0o7000
	if info.Mode().IsRegular() {
		if prev, ok := a.seenLink(info); ok {
			h.Typeflag, h.Linkname, h.Size = tar.TypeLink, prev, 0
		} else {
			a.remember(info, h.Name)
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
		sort.Strings(names)
		for _, n := range names {
			if err := a.Add(filepath.Join(path, n), strings.TrimSuffix(arcname, "/")+"/"+n, recursive, filter); err != nil {
				return err
			}
		}
	}
	return nil
}

func (a *TarAdder) seenLink(info os.FileInfo) (string, bool) {
	for _, s := range a.links[[2]int64{info.Size(), info.ModTime().UnixNano()}] {
		if os.SameFile(s.info, info) {
			return s.name, true
		}
	}
	return "", false
}

func (a *TarAdder) remember(info os.FileInfo, name string) {
	if a.links == nil {
		a.links = map[[2]int64][]tarLinkSeen{}
	}
	key := [2]int64{info.Size(), info.ModTime().UnixNano()}
	a.links[key] = append(a.links[key], tarLinkSeen{info, name})
}

// Member names of a gzip or plain tar archive in stream order
func TarMemberNames(r io.Reader) ([]string, error) {
	tr := tar.NewReader(r)
	var out []string
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return out, err
		}
		out = append(out, strings.TrimSuffix(h.Name, "/"))
	}
}
