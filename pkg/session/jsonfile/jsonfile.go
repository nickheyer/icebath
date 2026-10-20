// Session store keeping one JSON document per session in a directory
package jsonfile

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/nickheyer/icebath/pkg/internal/flock"
	"github.com/nickheyer/icebath/pkg/session"
)

// Directory of session documents named by id
type Store struct {
	Dir string
}

// Path of the document for an id
func (st Store) Path(id string) (string, error) {
	if !session.ValidID(id) {
		return "", session.InvalidID(id)
	}
	return filepath.Join(st.Dir, id+".json"), nil
}

func (st Store) lockPath(id string) string { return filepath.Join(st.Dir, id+".lock") }

// Writes the whole session atomically through a synced temporary file under a per session lock
func (st Store) Save(ctx context.Context, s *session.Session) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	target, err := st.Path(s.ID())
	if err != nil {
		return err
	}
	data, err := session.Marshal(s)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(st.Dir, 0o755); err != nil {
		return err
	}
	lock, err := flock.Acquire(st.lockPath(s.ID()))
	if err != nil {
		return err
	}
	defer lock.Unlock()
	return writeAtomic(target, data)
}

// Writes data to a temporary file, syncs it, renames it over target and syncs the directory
func writeAtomic(target string, data []byte) error {
	dir := filepath.Dir(target)
	tmp, err := os.CreateTemp(dir, filepath.Base(target)+".*.tmp")
	if err != nil {
		return err
	}
	cleanup := func(err error) error {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return cleanup(err)
	}
	if err := tmp.Sync(); err != nil {
		return cleanup(err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), target); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return syncDir(dir)
}

// Flushes the directory entry so a rename survives power loss where the file system allows it
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	if err := d.Sync(); err != nil && !errors.Is(err, syscall.EINVAL) && !errors.Is(err, syscall.ENOTSUP) {
		return err
	}
	return nil
}

func (st Store) Load(ctx context.Context, id string) (*session.Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	target, err := st.Path(id)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(target)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, session.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	s, err := session.Unmarshal(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", target, err)
	}
	return s, nil
}

// Lists stored sessions newest first, reading only the header of each document
func (st Store) List(ctx context.Context) ([]session.Info, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(st.Dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var infos []session.Info
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || e.IsDir() || !session.ValidID(id) {
			continue
		}
		infos = append(infos, st.describe(id, filepath.Join(st.Dir, e.Name())))
	}
	session.SortInfos(infos)
	return infos, nil
}

// Listing entry for one document, carrying the read error when the header is unreadable
func (st Store) describe(id, path string) session.Info {
	f, err := os.Open(path)
	if err != nil {
		return session.Info{ID: id, Error: err.Error()}
	}
	defer f.Close()
	h, err := session.ReadHeaderFrom(f)
	if err != nil {
		return session.Info{ID: id, Error: err.Error()}
	}
	return h.Info()
}

func (st Store) Exists(ctx context.Context, id string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	target, err := st.Path(id)
	if err != nil {
		return false, err
	}
	_, err = os.Stat(target)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

func (st Store) Delete(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	target, err := st.Path(id)
	if err != nil {
		return err
	}
	if _, err := os.Stat(target); errors.Is(err, fs.ErrNotExist) {
		return session.ErrNotFound
	}
	lock, err := flock.Acquire(st.lockPath(id))
	if err != nil {
		return err
	}
	err = os.Remove(target)
	lock.Unlock()
	os.Remove(st.lockPath(id))
	if errors.Is(err, fs.ErrNotExist) {
		return session.ErrNotFound
	}
	return err
}
