package fetch

import (
	"archive/tar"
	"compress/bzip2"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
)

// One downloadable file pinned by size and sha256
type Asset struct {
	Name   string
	URL    string
	SHA256 string
	Size   int64
}

// Returned when downloaded bytes do not hash to the pinned digest
type DigestError struct {
	Name, Want, Got string
}

func (e *DigestError) Error() string {
	return fmt.Sprintf("%s failed its integrity check: sha256 %s, expected %s", e.Name, e.Got, e.Want)
}

// Returned when the server answers a download with a failing status
type StatusError struct {
	Name string
	Code int
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("download of %s failed with HTTP %d", e.Name, e.Code)
}

const attempts = 4

var (
	client   = &http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, ResponseHeaderTimeout: 60 * time.Second}}
	locks    sync.Map
	retryGap = 2 * time.Second
)

// Directory under the icebath data root where fetched assets live
func Root() string { return filepath.Join(home.Root(), "cache", "fetch") }

func lockFor(key string) *sync.Mutex {
	mu, _ := locks.LoadOrStore(key, &sync.Mutex{})
	return mu.(*sync.Mutex)
}

func fileDigest(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	return hex.EncodeToString(h.Sum(nil)), n, err
}

// Appends the remainder of the asset to part, resuming at its current length
func download(ctx context.Context, a Asset, part *os.File) error {
	offset, err := part.Seek(0, io.SeekEnd)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, nil)
	if err != nil {
		return err
	}
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusOK && offset > 0:
		if err := part.Truncate(0); err != nil {
			return err
		}
		if _, err := part.Seek(0, io.SeekStart); err != nil {
			return err
		}
	case resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusPartialContent:
	case resp.StatusCode == http.StatusRequestedRangeNotSatisfiable:
		return nil
	default:
		return &StatusError{Name: a.Name, Code: resp.StatusCode}
	}
	_, err = io.Copy(part, resp.Body)
	return err
}

// Whether another attempt can help after this failure
func transient(err error) bool {
	var status *StatusError
	if errors.As(err, &status) {
		return status.Code >= 500 || status.Code == http.StatusTooManyRequests || status.Code == http.StatusRequestTimeout
	}
	return !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
}

// Path of the verified asset in the cache, downloading it on first use
func File(ctx context.Context, a Asset) (string, error) {
	if len(a.SHA256) != sha256.Size*2 || a.Name == "" || a.Name != filepath.Base(a.Name) {
		return "", fmt.Errorf("asset %q is not pinned by a file name and a sha256", a.Name)
	}
	mu := lockFor(a.SHA256)
	mu.Lock()
	defer mu.Unlock()
	dir := filepath.Join(Root(), a.SHA256)
	final := filepath.Join(dir, a.Name)
	if info, err := os.Stat(final); err == nil && info.Mode().IsRegular() && (a.Size == 0 || info.Size() == a.Size) {
		return final, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	part, err := os.CreateTemp(dir, a.Name+".*.part")
	if err != nil {
		return "", err
	}
	defer os.Remove(part.Name())
	defer part.Close()
	slog.Info("downloading "+a.Name, "bytes", a.Size, "url", a.URL)
	for attempt := 1; ; attempt++ {
		err = download(ctx, a, part)
		if err == nil {
			break
		}
		if attempt == attempts || !transient(err) {
			return "", fmt.Errorf("download of %s from %s failed: %w", a.Name, a.URL, err)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(time.Duration(attempt) * retryGap):
		}
	}
	if err := part.Sync(); err != nil {
		return "", err
	}
	got, size, err := fileDigest(part.Name())
	if err != nil {
		return "", err
	}
	if got != strings.ToLower(a.SHA256) || (a.Size != 0 && size != a.Size) {
		return "", &DigestError{Name: a.Name, Want: a.SHA256, Got: got}
	}
	if err := os.Rename(part.Name(), final); err != nil {
		return "", err
	}
	return final, nil
}

// Opens the tar stream of an archive by its compression suffix
func tarStream(name string, r io.Reader) (*tar.Reader, error) {
	switch {
	case strings.HasSuffix(name, ".tar.bz2"):
		return tar.NewReader(bzip2.NewReader(r)), nil
	case strings.HasSuffix(name, ".tar.gz"), strings.HasSuffix(name, ".tgz"):
		gz, err := gzip.NewReader(r)
		if err != nil {
			return nil, err
		}
		return tar.NewReader(gz), nil
	case strings.HasSuffix(name, ".tar"):
		return tar.NewReader(r), nil
	}
	return nil, fmt.Errorf("%s is not a tar, tar.gz or tar.bz2 archive", name)
}

// Unpacks an archive into dir, refusing any entry or link that leaves it
func unpack(archive, dir string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	tr, err := tarStream(filepath.Base(archive), f)
	if err != nil {
		return err
	}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("%s is damaged: %w", filepath.Base(archive), err)
		}
		if !filepath.IsLocal(hdr.Name) {
			return fmt.Errorf("%s holds an entry outside its tree: %s", filepath.Base(archive), hdr.Name)
		}
		target := filepath.Join(dir, hdr.Name)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, hdr.FileInfo().Mode().Perm()|0o600)
			if err != nil {
				return err
			}
			_, err = io.Copy(out, tr)
			if cerr := out.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
		case tar.TypeSymlink:
			if !filepath.IsLocal(filepath.Join(filepath.Dir(hdr.Name), hdr.Linkname)) || filepath.IsAbs(hdr.Linkname) {
				return fmt.Errorf("%s holds a link outside its tree: %s", filepath.Base(archive), hdr.Name)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			os.Remove(target)
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return err
			}
		}
	}
}

// Directory holding the unpacked contents of a verified archive, unpacking it on first use
func Tree(ctx context.Context, a Asset) (string, error) {
	archive, err := File(ctx, a)
	if err != nil {
		return "", err
	}
	mu := lockFor(a.SHA256)
	mu.Lock()
	defer mu.Unlock()
	tree := filepath.Join(filepath.Dir(archive), "tree")
	if info, err := os.Stat(tree); err == nil && info.IsDir() {
		return tree, nil
	}
	staging, err := os.MkdirTemp(filepath.Dir(archive), "tree.*.part")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(staging)
	if err := unpack(archive, staging); err != nil {
		return "", err
	}
	if err := os.Rename(staging, tree); err != nil {
		if info, serr := os.Stat(tree); serr == nil && info.IsDir() {
			return tree, nil
		}
		return "", err
	}
	return tree, nil
}
