package webbundle

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"github.com/nickheyer/icebath/pkg/home"
)

// Dashboard bundle a release binary carries, set by the webdist build of cmd/icebath and nil in a source build
var Embedded fs.FS

var (
	unpackMu sync.Mutex
	unpacked = map[string]string{}
)

// Directory the dashboard is served from, the checkout build when it has an index, else the embedded bundle unpacked
func Dist(projectRoot string) string {
	checkout := filepath.Join(projectRoot, "cmd", "icebath", "web_dist")
	if info, err := os.Stat(filepath.Join(checkout, "index.html")); err == nil && info.Mode().IsRegular() {
		return checkout
	}
	if Embedded == nil {
		return checkout
	}
	dir, err := Unpack(home.Dir(context.Background()))
	if err != nil {
		slog.Warn("could not unpack the dashboard bundle embedded in this binary", "error", err)
		return checkout
	}
	return dir
}

// Identity of the embedded bundle, the output hash of its build receipt or a digest of every file
func identity(bundle fs.FS) (string, error) {
	if raw, err := fs.ReadFile(bundle, "hermes-build.json"); err == nil {
		var receipt struct {
			OutputHash string `json:"outputHash"`
		}
		if json.Unmarshal(raw, &receipt) == nil && len(receipt.OutputHash) >= 16 {
			return receipt.OutputHash[:16], nil
		}
	}
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

// Writes the embedded bundle once per identity under the home cache, publishing it with an atomic rename
func Unpack(homeDir string) (string, error) {
	unpackMu.Lock()
	defer unpackMu.Unlock()
	id, err := identity(Embedded)
	if err != nil {
		return "", err
	}
	root := filepath.Join(homeDir, "cache", "web_dist")
	final := filepath.Join(root, id)
	if dir, ok := unpacked[final]; ok {
		return dir, nil
	}
	if info, err := os.Stat(filepath.Join(final, "index.html")); err == nil && info.Mode().IsRegular() {
		unpacked[final] = final
		return final, nil
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", err
	}
	tmp, err := os.MkdirTemp(root, "."+id+"-")
	if err != nil {
		return "", err
	}
	err = fs.WalkDir(Embedded, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		dest := filepath.Join(tmp, filepath.FromSlash(p))
		if d.IsDir() {
			return os.MkdirAll(dest, 0o755)
		}
		raw, err := fs.ReadFile(Embedded, p)
		if err != nil {
			return err
		}
		return os.WriteFile(dest, raw, 0o644)
	})
	if err == nil {
		if err = os.Rename(tmp, final); err != nil {
			if info, statErr := os.Stat(filepath.Join(final, "index.html")); statErr == nil && info.Mode().IsRegular() {
				err = nil
			}
		}
	}
	if err != nil {
		os.RemoveAll(tmp)
		return "", err
	}
	os.RemoveAll(tmp)
	unpacked[final] = final
	return final, nil
}
