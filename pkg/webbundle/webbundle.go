package webbundle

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/install"
)

// Browser bundle the binary serves
type Bundle string

const (
	Dashboard Bundle = "web"
	Webapp    Bundle = "webapp"
)

// Release asset holding both bundles under their checkout directory names
const ReleaseAsset = "icebath-web.tar.gz"

// Bundles a webdist build of cmd/icebath carries, empty in every other build
var Embedded = map[Bundle]fs.FS{}

var (
	resolvedMu sync.Mutex
	resolved   = map[Bundle]string{}
)

// Directory a bundle occupies under cmd/icebath of a checkout and inside the release asset
func (b Bundle) distName() string {
	if b == Webapp {
		return "webapp_dist"
	}
	return "web_dist"
}

// Variable that points the bundle at a directory chosen by the operator
func (b Bundle) envName() string {
	if b == Webapp {
		return "ICEBATH_WEBAPP_DIST"
	}
	return "ICEBATH_WEB_DIST"
}

// Human name of the bundle
func (b Bundle) label() string {
	if b == Webapp {
		return "Webapp bundle"
	}
	return "dashboard bundle"
}

// Directory serving the bundle from the override, the embedded copy, the matching release asset or a checkout build
func Dir(ctx context.Context, b Bundle, progress io.Writer) (string, error) {
	if raw, ok := os.LookupEnv(b.envName()); ok {
		dir := expandHome(raw)
		if !hasIndex(dir) {
			return "", fmt.Errorf("%s names %s, which holds no index.html", b.envName(), dir)
		}
		return dir, nil
	}
	resolvedMu.Lock()
	defer resolvedMu.Unlock()
	if dir, ok := resolved[b]; ok && hasIndex(dir) {
		return dir, nil
	}
	dir, err := locate(ctx, b, progress)
	if err != nil {
		return "", err
	}
	resolved[b] = dir
	return dir, nil
}

// First source that holds the bundle, a plain error naming make web when none does
func locate(ctx context.Context, b Bundle, progress io.Writer) (string, error) {
	if bundle := Embedded[b]; bundle != nil {
		return unpackEmbedded(b, bundle)
	}
	if tag := install.RunningReleaseTag(); tag != "" {
		root, err := releaseBundles(ctx, tag, progress)
		if err != nil {
			return "", fmt.Errorf("the %s of icebath %s could not be fetched: %w", b.label(), tag, err)
		}
		dir := filepath.Join(root, b.distName())
		if !hasIndex(dir) {
			return "", fmt.Errorf("release asset %s of icebath %s holds no %s", ReleaseAsset, tag, b.label())
		}
		return dir, nil
	}
	if root := install.SourceCheckoutRoot(); root != "" {
		dir := filepath.Join(root, "cmd", "icebath", b.distName())
		if hasIndex(dir) {
			return dir, nil
		}
		return "", fmt.Errorf("the %s is not built, run make web in %s", b.label(), root)
	}
	return "", fmt.Errorf("the %s is not built, run make web in the icebath checkout this binary was built from", b.label())
}

// Reports a directory whose index.html is a regular file
func hasIndex(dir string) bool {
	if dir == "" {
		return false
	}
	info, err := os.Stat(filepath.Join(dir, "index.html"))
	return err == nil && info.Mode().IsRegular()
}

// Path with a leading tilde replaced by the user home
func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		if userHome, err := os.UserHomeDir(); err == nil {
			return filepath.Join(userHome, p[1:])
		}
	}
	return p
}

// Cache root every unpacked or downloaded bundle lives under
func cacheRoot() string { return filepath.Join(home.Root(), "cache", "webbundle") }

// Digest of every path and byte of a bundle tree
func identity(bundle fs.FS) (string, error) {
	h := sha256.New()
	err := fs.WalkDir(bundle, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		f, err := bundle.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		io.WriteString(h, p+"\x00")
		_, err = io.Copy(h, f)
		return err
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil))[:16], nil
}

// Writes an embedded bundle once per identity under the cache, publishing it with an atomic rename
func unpackEmbedded(b Bundle, bundle fs.FS) (string, error) {
	id, err := identity(bundle)
	if err != nil {
		return "", err
	}
	final := filepath.Join(cacheRoot(), string(b)+"-"+id)
	if hasIndex(final) {
		return final, nil
	}
	return publish(final, func(tmp string) error {
		return fs.WalkDir(bundle, ".", func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			dest := filepath.Join(tmp, filepath.FromSlash(p))
			if d.IsDir() {
				return os.MkdirAll(dest, 0o755)
			}
			raw, err := fs.ReadFile(bundle, p)
			if err != nil {
				return err
			}
			return os.WriteFile(dest, raw, 0o644)
		})
	}, func(dir string) bool { return hasIndex(dir) })
}

// Fills a fresh sibling of final through fill and renames it into place, keeping a copy another process published first
func publish(final string, fill func(tmp string) error, complete func(dir string) bool) (string, error) {
	parent := filepath.Dir(final)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return "", err
	}
	tmp, err := os.MkdirTemp(parent, "."+filepath.Base(final)+"-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	if err := fill(tmp); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, final); err != nil && !complete(final) {
		return "", err
	}
	return final, nil
}

// Both bundles of a release unpacked under the cache, downloaded and checked against checksums.txt when missing
func releaseBundles(ctx context.Context, tag string, progress io.Writer) (string, error) {
	final := filepath.Join(cacheRoot(), tag)
	complete := func(dir string) bool {
		return hasIndex(filepath.Join(dir, Dashboard.distName())) && hasIndex(filepath.Join(dir, Webapp.distName()))
	}
	if complete(final) {
		return final, nil
	}
	repo, err := install.ParseRepo(install.DefaultRepoURL)
	if err != nil {
		return "", err
	}
	target := &install.ReleaseTarget{Tag: tag, Repo: repo}
	sum, err := target.ExpectedSHA256(ctx, ReleaseAsset)
	if err != nil {
		return "", err
	}
	if progress != nil {
		fmt.Fprintf(progress, "→ Downloading the browser bundles of icebath %s...\n", tag)
	}
	slog.Info("downloading the browser bundles", "tag", tag, "asset", ReleaseAsset)
	return publish(final, func(tmp string) error {
		archive := filepath.Join(tmp, ReleaseAsset)
		if err := download(ctx, target.AssetURL(ReleaseAsset), archive, sum); err != nil {
			return err
		}
		defer os.Remove(archive)
		if err := extractTarGz(archive, tmp); err != nil {
			return fmt.Errorf("could not unpack %s: %w", ReleaseAsset, err)
		}
		if !complete(tmp) {
			return fmt.Errorf("%s holds no %s and %s", ReleaseAsset, Dashboard.distName(), Webapp.distName())
		}
		return nil
	}, complete)
}

// Downloads url to dest, refusing a body whose sha256 is not want
func download(ctx context.Context, url, dest, want string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "icebath")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
	}
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, h), resp.Body); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return fmt.Errorf("%s does not match checksums.txt (expected %s, got %s)", path.Base(url), want, got)
	}
	return nil
}

// Unpacks the directories and regular files of a gzip tar under dest, refusing any entry that leaves it
func extractTarGz(archive, dest string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		name := path.Clean(strings.TrimPrefix(hdr.Name, "./"))
		if name == "." {
			continue
		}
		if path.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") {
			return fmt.Errorf("entry %s leaves the archive root", hdr.Name)
		}
		target := filepath.Join(dest, filepath.FromSlash(name))
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return err
			}
			if err := out.Close(); err != nil {
				return err
			}
		default:
			return fmt.Errorf("entry %s is neither a file nor a directory", hdr.Name)
		}
	}
}
