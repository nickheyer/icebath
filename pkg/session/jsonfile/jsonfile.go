// Session store keeping one JSON document per session in a directory
package jsonfile

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

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

// Writes the whole session atomically through a temporary file
func (st Store) Save(s *session.Session) error {
	target, err := st.Path(s.ID)
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
	tmp, err := os.CreateTemp(st.Dir, s.ID+".*.tmp")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), target); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}

func (st Store) Load(id string) (*session.Session, error) {
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
func (st Store) List() ([]session.Info, error) {
	entries, err := os.ReadDir(st.Dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var infos []session.Info
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || e.IsDir() || !session.ValidID(id) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(st.Dir, e.Name()))
		if err != nil {
			return nil, err
		}
		h, n, err := session.UnmarshalHeader(data)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
		infos = append(infos, session.Info{ID: h.ID, Created: h.Created, Updated: h.Updated, Title: h.Title, Model: h.Model, Parent: h.Parent, Labels: h.Labels, Messages: n, Usage: h.Usage})
	}
	session.SortInfos(infos)
	return infos, nil
}

func (st Store) Delete(id string) error {
	target, err := st.Path(id)
	if err != nil {
		return err
	}
	err = os.Remove(target)
	if errors.Is(err, fs.ErrNotExist) {
		return session.ErrNotFound
	}
	return err
}
