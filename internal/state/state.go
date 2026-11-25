package state

import (
	"encoding/gob"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
)

const file = "state.bin"

type State struct {
	Backend string

	dir string
}

func Load(dir string) (*State, error) {
	s := &State{dir: dir}
	f, err := os.Open(filepath.Join(dir, file))
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if err := gob.NewDecoder(f).Decode(s); err != nil {
		return nil, fmt.Errorf("read state %s: %w", f.Name(), err)
	}
	return s, nil
}

func (s *State) SetBackend(backend string) error {
	if backend != "" {
		u, err := url.Parse(backend)
		if err != nil {
			return err
		}
		if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("%q is not an http or https URL", backend)
		}
	}
	previous := s.Backend
	s.Backend = backend
	if err := s.save(); err != nil {
		s.Backend = previous
		return err
	}
	return nil
}

func (s *State) save() error {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(s.dir, file+".*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := gob.NewEncoder(f).Encode(s); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), filepath.Join(s.dir, file))
}
