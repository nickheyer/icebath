package fetch

import (
	"archive/tar"
	"archive/zip"
	"compress/bzip2"
	"compress/gzip"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
)

// One downloadable file pinned by size and sha256, or by the Git LFS pointer of a host that hides the sha256
type Asset struct {
	Name    string
	URL     string
	SHA256  string
	LFSOid  string
	Size    int64
	Mirrors []string
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

// Returned when Hugging Face refuses a gated file to a caller without an accepted access token
type GatedError struct {
	Name, Repo string
	Code       int
}

func (e *GatedError) Error() string {
	return fmt.Sprintf("%s is gated on Hugging Face (HTTP %d), accept its terms at https://huggingface.co/%s and set HF_TOKEN to a token of that account",
		e.Name, e.Code, e.Repo)
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

// Git blob id of the LFS pointer naming content with a sha256 and size
func lfsPointerOid(sha string, size int64) string {
	pointer := "version https://git-lfs.github.com/spec/v1\noid sha256:" + sha + "\nsize " + strconv.FormatInt(size, 10) + "\n"
	h := sha1.New()
	fmt.Fprintf(h, "blob %d\x00%s", len(pointer), pointer)
	return hex.EncodeToString(h.Sum(nil))
}

// Cache key of an asset, its sha256 or its pointer id
func (a Asset) key() string {
	if a.SHA256 != "" {
		return strings.ToLower(a.SHA256)
	}
	return "lfs-" + strings.ToLower(a.LFSOid)
}

func (a Asset) pinned() bool {
	return (len(a.SHA256) == sha256.Size*2 || (a.SHA256 == "" && len(a.LFSOid) == sha1.Size*2 && a.Size > 0)) &&
		a.Name != "" && a.Name == filepath.Base(a.Name)
}

// Hugging Face access token from the environment or the token file the Hugging Face tools write
func huggingFaceToken() string {
	for _, name := range []string{"HF_TOKEN", "HUGGING_FACE_HUB_TOKEN"} {
		if v := strings.TrimSpace(os.Getenv(name)); v != "" {
			return v
		}
	}
	var paths []string
	if p := os.Getenv("HF_TOKEN_PATH"); p != "" {
		paths = append(paths, p)
	}
	if p := os.Getenv("HF_HOME"); p != "" {
		paths = append(paths, filepath.Join(p, "token"))
	}
	if dir, err := os.UserHomeDir(); err == nil {
		paths = append(paths, filepath.Join(dir, ".cache", "huggingface", "token"))
	}
	for _, p := range paths {
		if data, err := os.ReadFile(p); err == nil {
			if v := strings.TrimSpace(string(data)); v != "" {
				return v
			}
		}
	}
	return ""
}

// Repository of a Hugging Face resolve address, empty for other hosts
func huggingFaceRepo(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host != "huggingface.co" {
		return ""
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) >= 3 && parts[2] == "resolve" {
		return parts[0] + "/" + parts[1]
	}
	return ""
}

// Appends the remainder of the asset from one source to part, resuming at its current length
func download(ctx context.Context, a Asset, source string, part *os.File) error {
	offset, err := part.Seek(0, io.SeekEnd)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
	if err != nil {
		return err
	}
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}
	repo := huggingFaceRepo(source)
	if repo != "" {
		if token := huggingFaceToken(); token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
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
	case repo != "" && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden):
		return &GatedError{Name: a.Name, Repo: repo, Code: resp.StatusCode}
	default:
		return &StatusError{Name: a.Name, Code: resp.StatusCode}
	}
	_, err = io.Copy(part, resp.Body)
	return err
}

// Whether another attempt can help after this failure
func transient(err error) bool {
	var status *StatusError
	var gated *GatedError
	switch {
	case errors.As(err, &gated):
		return false
	case errors.As(err, &status):
		return status.Code >= 500 || status.Code == http.StatusTooManyRequests || status.Code == http.StatusRequestTimeout
	}
	return !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
}

// Downloads from one source with retries on transient failures
func downloadFrom(ctx context.Context, a Asset, source string, part *os.File) error {
	for attempt := 1; ; attempt++ {
		err := download(ctx, a, source, part)
		if err == nil {
			return nil
		}
		if attempt == attempts || !transient(err) {
			return fmt.Errorf("download of %s from %s failed: %w", a.Name, source, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(attempt) * retryGap):
		}
	}
}

// Checks the downloaded bytes against the sha256 pin or the pointer pin
func verify(a Asset, path string) error {
	got, size, err := fileDigest(path)
	if err != nil {
		return err
	}
	if a.Size != 0 && size != a.Size {
		return &DigestError{Name: a.Name, Want: fmt.Sprintf("%d bytes", a.Size), Got: fmt.Sprintf("%s over %d bytes", got, size)}
	}
	if a.SHA256 != "" {
		if got != strings.ToLower(a.SHA256) {
			return &DigestError{Name: a.Name, Want: a.SHA256, Got: got}
		}
		return nil
	}
	if oid := lfsPointerOid(got, size); oid != strings.ToLower(a.LFSOid) {
		return &DigestError{Name: a.Name, Want: "content of LFS pointer " + a.LFSOid, Got: got + " with pointer " + oid}
	}
	return nil
}

// Path of the verified asset in the cache, downloading it from its address then its mirrors on first use
func File(ctx context.Context, a Asset) (string, error) {
	if !a.pinned() {
		return "", fmt.Errorf("asset %q is not pinned by a file name and a sha256", a.Name)
	}
	mu := lockFor(a.key())
	mu.Lock()
	defer mu.Unlock()
	dir := filepath.Join(Root(), a.key())
	final := filepath.Join(dir, a.Name)
	if info, err := os.Stat(final); err == nil && info.Mode().IsRegular() && (a.Size == 0 || info.Size() == a.Size) {
		return final, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	slog.Info("downloading "+a.Name, "bytes", a.Size, "url", a.URL)
	var failures []error
	for _, source := range append([]string{a.URL}, a.Mirrors...) {
		if source == "" {
			continue
		}
		path, err := fetchSource(ctx, a, source, dir, final)
		if err == nil {
			return path, nil
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		failures = append(failures, err)
	}
	if len(failures) == 0 {
		return "", fmt.Errorf("asset %s has no download address", a.Name)
	}
	return "", errors.Join(failures...)
}

// Downloads and verifies the asset from one source into its final path
func fetchSource(ctx context.Context, a Asset, source, dir, final string) (string, error) {
	part, err := os.CreateTemp(dir, a.Name+".*.part")
	if err != nil {
		return "", err
	}
	defer os.Remove(part.Name())
	defer part.Close()
	if err := downloadFrom(ctx, a, source, part); err != nil {
		return "", err
	}
	if err := part.Sync(); err != nil {
		return "", err
	}
	if err := verify(a, part.Name()); err != nil {
		return "", err
	}
	if err := part.Close(); err != nil {
		return "", err
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
	return nil, fmt.Errorf("%s is not a tar, tar.gz, tar.bz2, zip or wheel archive", name)
}

func isZip(name string) bool {
	return strings.HasSuffix(name, ".zip") || strings.HasSuffix(name, ".whl") || strings.HasSuffix(name, ".npz")
}

// Writes one regular archive member below dir
func writeMember(target string, mode os.FileMode, r io.Reader) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode.Perm()|0o600)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, r)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return err
}

// Creates a symlink member after checking it stays inside the tree
func linkMember(archive, name, target, linkname string) error {
	if filepath.IsAbs(linkname) || !filepath.IsLocal(filepath.Join(filepath.Dir(name), linkname)) {
		return fmt.Errorf("%s holds a link outside its tree: %s", archive, name)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	os.Remove(target)
	return os.Symlink(linkname, target)
}

// Unpacks a zip archive into dir, refusing any entry or link that leaves it
func unzip(archive, dir string) error {
	zr, err := zip.OpenReader(archive)
	if err != nil {
		return fmt.Errorf("%s is damaged: %w", filepath.Base(archive), err)
	}
	defer zr.Close()
	for _, f := range zr.File {
		name := strings.TrimSuffix(f.Name, "/")
		if name == "" {
			continue
		}
		if !filepath.IsLocal(name) {
			return fmt.Errorf("%s holds an entry outside its tree: %s", filepath.Base(archive), f.Name)
		}
		target := filepath.Join(dir, name)
		mode := f.Mode()
		switch {
		case mode.IsDir() || strings.HasSuffix(f.Name, "/"):
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case mode&os.ModeSymlink != 0:
			rc, err := f.Open()
			if err != nil {
				return err
			}
			link, err := io.ReadAll(io.LimitReader(rc, 4096))
			rc.Close()
			if err != nil {
				return err
			}
			if err := linkMember(filepath.Base(archive), name, target, string(link)); err != nil {
				return err
			}
		default:
			rc, err := f.Open()
			if err != nil {
				return err
			}
			err = writeMember(target, mode|0o644, rc)
			rc.Close()
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// Unpacks an archive into dir, refusing any entry or link that leaves it
func unpack(archive, dir string) error {
	if isZip(filepath.Base(archive)) {
		return unzip(archive, dir)
	}
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
			if err := writeMember(target, hdr.FileInfo().Mode(), tr); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if err := linkMember(filepath.Base(archive), hdr.Name, target, hdr.Linkname); err != nil {
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
	mu := lockFor(a.key())
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

// Directory under the fetch root for files derived from fetched assets, created on demand
func Derived(parts ...string) (string, error) {
	dir := filepath.Join(append([]string{Root(), "derived"}, parts...)...)
	return dir, os.MkdirAll(dir, 0o755)
}
