package toolchain

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/fileutil"
)

const (
	legacyLockWait     = 2 * time.Second
	llamaReleasePrefix = "https://github.com/ggml-org/llama.cpp/releases/download/"
)

var legacyDigestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// One engine directory an earlier layout installed, with the release archive digests its manifest recorded
type LegacyInstall struct {
	Version string
	Dir     string
	Assets  map[string]string
}

// Moves the first acceptable legacy install of name into the writable store and records it, returning the directories refused
func (s *Store) AdoptLegacy(ctx context.Context, name string, installs []LegacyInstall) (bool, []string, error) {
	r, err := s.recipe(name)
	if err != nil {
		return false, nil, err
	}
	host, err := s.hostTarget(ctx)
	if err != nil {
		return false, nil, err
	}
	_, root, err := s.roots(ctx)
	if err != nil {
		return false, nil, err
	}
	if err := os.MkdirAll(root, 0o777); err != nil {
		return false, nil, err
	}
	wait, cancel := context.WithTimeout(ctx, legacyLockWait)
	l, err := fileutil.OpenLock(wait, filepath.Join(root, installLockName), fileutil.LockPerm(0o600), fileutil.RecordHolder())
	cancel()
	if err != nil {
		var timeout *fileutil.LockTimeoutError
		if ctx.Err() == nil && (errors.Is(err, fileutil.ErrLocked) || errors.Is(err, context.DeadlineExceeded) || errors.As(err, &timeout)) {
			return false, nil, nil
		}
		return false, nil, err
	}
	defer l.Release()
	loc, err := s.locate(ctx, r, host, false, true)
	if err != nil {
		return false, nil, err
	}
	if loc != nil {
		return false, nil, nil
	}
	var refused []string
	for _, in := range installs {
		adopted, err := s.adoptOne(ctx, r, host, root, in)
		if err != nil {
			return false, refused, err
		}
		if adopted {
			return true, refused, nil
		}
		refused = append(refused, in.Dir)
	}
	return false, refused, nil
}

// Renames one legacy directory into place and records its receipt, moving it back when the receipt cannot be written
func (s *Store) adoptOne(ctx context.Context, r *recipe, host hostTarget, root string, in LegacyInstall) (bool, error) {
	artifacts := s.legacyArtifacts(r.name, in.Version, host, in.Assets)
	entryName := r.directory(in.Version, host.target)
	entry := filepath.Join(root, entryName)
	if artifacts == nil {
		return false, nil
	}
	if _, err := os.Lstat(entry); err == nil {
		return false, nil
	}
	if reason := s.verifyEntry(ctx, r, in.Dir, host.target, host.target); reason != "" {
		slog.Warn(fmt.Sprintf("pre-store llama.cpp at %s left in place: %s", in.Dir, reason))
		return false, nil
	}
	if err := os.Rename(in.Dir, entry); err != nil {
		slog.Warn(fmt.Sprintf("pre-store llama.cpp at %s left in place: %v", in.Dir, err))
		return false, nil
	}
	digest, err := treeDigest(entry, true)
	if err == nil {
		f := &facts{path: filepath.Join(root, factsName)}
		err = f.record(ctx, r.name, fact{
			Entry: entryName, Version: in.Version, Env: templatize(r.environment(entry, host.target), root),
			Target: string(host.target), Artifacts: artifacts, Digest: digest,
		})
	}
	if err != nil {
		if rerr := os.Rename(entry, in.Dir); rerr != nil {
			return false, errors.Join(err, rerr)
		}
		return false, err
	}
	_ = os.Remove(filepath.Dir(in.Dir))
	slog.Info(fmt.Sprintf("moved llama.cpp b%s (%s) from %s into the toolchain store", in.Version, r.name, in.Dir))
	return true, nil
}

// Digests the legacy manifest holds for the release archives the lock names at version, nil when one is missing
func (s *Store) legacyArtifacts(name, version string, host hostTarget, assets map[string]string) []string {
	pinned, _ := s.lock.version(name)
	var shas []string
	for _, a := range s.lock.artifacts(name, host) {
		if !strings.HasPrefix(a.URL, llamaReleasePrefix) {
			continue
		}
		asset := strings.ReplaceAll(a.fileName(), "b"+pinned, "b"+version)
		sha := assets[asset]
		if !legacyDigestPattern.MatchString(sha) {
			return nil
		}
		shas = append(shas, sha)
	}
	if len(shas) == 0 {
		return nil
	}
	return shas
}
