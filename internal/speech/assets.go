package speech

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/internal/packages"
	"github.com/nickheyer/icebath/pkg/fsx"
)

// One file speech fetches on first use, such as a voice or a model, pinned by its sha256
type Asset struct {
	Name   string
	URL    string
	SHA256 string
	Size   int64
}

// Folder the fetched speech assets live in, one folder each by digest
func (s *Speech) assets() string {
	return filepath.Join(s.Dir(), "cache", "fetch")
}

// Folder of the fetched speech assets, beside which a plugin keeps what it makes of them, such as converted voices
func (s *Speech) Assets() string { return s.assets() }

var assetLocks sync.Map

func lockAsset(digest string) func() {
	mu, _ := assetLocks.LoadOrStore(digest, &sync.Mutex{})
	mu.(*sync.Mutex).Lock()
	return mu.(*sync.Mutex).Unlock
}

// Whether the asset is fetched already, so using it downloads nothing
func (s *Speech) Fetched(a Asset) bool {
	info, err := os.Stat(filepath.Join(s.assets(), a.SHA256, a.Name))
	return err == nil && info.Mode().IsRegular() && (a.Size == 0 || info.Size() == a.Size)
}

// Path of the verified asset, downloaded on first use
func (s *Speech) File(ctx context.Context, a Asset) (string, error) {
	if len(a.SHA256) != sha256.Size*2 || a.Name == "" || a.Name != filepath.Base(a.Name) {
		return "", fmt.Errorf("asset %q is not pinned by a file name and a sha256", a.Name)
	}
	defer lockAsset(a.SHA256)()
	path := filepath.Join(s.assets(), a.SHA256, a.Name)
	if s.Fetched(a) {
		return path, nil
	}
	slog.Info("downloading "+a.Name, "bytes", a.Size, "url", a.URL)
	download := s.packages.Download([]packages.Source{{URL: a.URL, Dest: path, SHA256: strings.ToLower(a.SHA256)}})
	if _, err := download.Run(ctx, nil); err != nil {
		return "", fmt.Errorf("download of %s from %s failed: %w", a.Name, a.URL, err)
	}
	return path, nil
}

// Folder holding the unpacked contents of a verified archive, unpacked on first use
func (s *Speech) Tree(ctx context.Context, a Asset) (string, error) {
	archive, err := s.File(ctx, a)
	if err != nil {
		return "", err
	}
	defer lockAsset(a.SHA256)()
	tree := filepath.Join(filepath.Dir(archive), "tree")
	if fsx.IsDir(tree) {
		return tree, nil
	}
	staging, err := os.MkdirTemp(filepath.Dir(archive), "tree.*.part")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(staging)
	if err := packages.Extract(archive, staging); err != nil {
		return "", fmt.Errorf("%s does not unpack: %w", a.Name, err)
	}
	if err := os.Rename(staging, tree); err != nil && !fsx.IsDir(tree) {
		return "", err
	}
	return tree, nil
}
