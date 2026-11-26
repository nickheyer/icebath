package state

import (
	"encoding/gob"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sync"
)

// File of the state of a profile, after its name
const suffix = "-state.bin"

// What icebath keeps between runs for one profile, apart from its settings
type State struct {
	Backend string

	mu      sync.Mutex
	dir     string
	profile string
}

func New(dir string) *State {
	return &State{dir: dir}
}

func (s *State) path(profile string) string {
	return filepath.Join(s.dir, profile+suffix)
}

// Reads the state of the profile, which is the one kept from now on
func (s *State) Use(profile string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	path := s.path(profile)
	read := State{}
	f, err := os.Open(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return err
	default:
		defer f.Close()
		if err := gob.NewDecoder(f).Decode(&read); err != nil {
			return fmt.Errorf("read state %s: %w", f.Name(), err)
		}
	}
	s.Backend, s.profile = read.Backend, profile
	return nil
}

// Removes the state of a deleted profile
func (s *State) Delete(profile string) error {
	if err := os.Remove(s.path(profile)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
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
	s.mu.Lock()
	defer s.mu.Unlock()
	previous := s.Backend
	s.Backend = backend
	if err := s.save(); err != nil {
		s.Backend = previous
		return err
	}
	return nil
}

// Called with the lock held
func (s *State) save() error {
	if s.profile == "" {
		return errors.New("state: no profile is in use")
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(s.dir, s.profile+suffix+".*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := gob.NewEncoder(f).Encode(struct{ Backend string }{s.Backend}); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), s.path(s.profile))
}
