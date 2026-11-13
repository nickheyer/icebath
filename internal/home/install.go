package home

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"strings"

	"github.com/google/uuid"

	"github.com/nickheyer/icebath/internal/fileutil"
)

// Grammar of a valid install identity, thirty-two lower case hex digits
var installIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// Error for an install identity that is neither readable nor persistable under the root
var ErrNoInstallID = errors.New("install identity is neither readable nor persistable")

// Valid identity in the file at path, mint true only for a missing or malformed file, never a read failure
func readInstallID(path string) (id string, mint bool) {
	text, err := fileutil.ReadText(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", true
	}
	if err != nil {
		return "", false
	}
	if v := strings.ToLower(strings.TrimSpace(text)); installIDPattern.MatchString(v) {
		return v, false
	}
	return "", true
}

// Opaque identity every profile of the install under the process root shares, minted once and cached for the process
func (r *Resolver) InstallID(ctx context.Context) (string, error) {
	root, err := DefaultRoot()
	if err != nil {
		return "", err
	}
	r.idMu.Lock()
	defer r.idMu.Unlock()
	if r.id != "" && (r.idRoot == "" || r.idRoot == root) {
		return r.id, nil
	}
	id, err := r.readOrCreateInstallID(ctx, root)
	if err != nil {
		return "", err
	}
	r.idRoot, r.id = root, id
	return id, nil
}

// Identity under root, atomically minted under the cross-process lock when missing, readable on a read-only root
func (r *Resolver) readOrCreateInstallID(ctx context.Context, root string) (string, error) {
	path := InstallIDFile.In(root)
	if id, _ := readInstallID(path); id != "" {
		return id, nil
	}
	if err := r.Mkdir(ctx, root); err != nil {
		return "", fmt.Errorf("%w: %w", ErrNoInstallID, err)
	}
	lock, err := fileutil.OpenLock(ctx, InstallIDLock.In(root), fileutil.LockPerm(0o600))
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrNoInstallID, err)
	}
	defer lock.Release()
	existing, mint := readInstallID(path)
	if !mint {
		if existing == "" {
			return "", ErrNoInstallID
		}
		return existing, nil
	}
	u, err := uuid.NewRandom()
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrNoInstallID, err)
	}
	err = fileutil.WriteText(ctx, path, hex.EncodeToString(u[:])+"\n",
		fileutil.TempPrefix(".install_id-"), fileutil.Perm(0o600), fileutil.SyncDir())
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrNoInstallID, err)
	}
	committed, err := fileutil.ReadText(path)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrNoInstallID, err)
	}
	if v := strings.ToLower(strings.TrimSpace(committed)); installIDPattern.MatchString(v) {
		return v, nil
	}
	return "", ErrNoInstallID
}
