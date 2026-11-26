package sessions

import (
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"

	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/internal/config"
	sessionsconfig "github.com/nickheyer/icebath/internal/sessions/config"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
)

var ErrNotFound = connect.NewError(connect.CodeNotFound, "Session not found")

type Store interface {
	// Makes a session in the directory, run on the profile
	Create(directory, profile string) (*sessionsv1.Session, error)
	List() ([]*sessionsv1.Session, error)
	Get(id string) (*sessionsv1.Session, error)
	Rename(id, title string) error
	Delete(id string) error
	// Runs the session on another profile from now on
	Move(id, profile string) error
	// Deletes every session run on the profile
	Clear(profile string) error
	Messages(id string) ([]*sessionsv1.Message, error)
	Append(id string, message *sessionsv1.Message) error
	Truncate(id string, keep int) error
}

type Registrant interface {
	Stores() map[string]Store
}

type Registry struct {
	registrants []Registrant
}

func (r *Registry) Register(registrant Registrant) {
	r.registrants = append(r.registrants, registrant)
}

func (r *Registry) Stores() map[string]Store {
	stores := map[string]Store{}
	for _, registrant := range r.registrants {
		maps.Copy(stores, registrant.Stores())
	}
	return stores
}

func (r *Registry) Store(name string) (Store, error) {
	store, ok := r.Stores()[name]
	if !ok {
		return nil, fmt.Errorf("No session store named %s.", name)
	}
	return store, nil
}

func (r *Registry) Close() error {
	var errs []error
	for _, registrant := range slices.Backward(r.registrants) {
		if closer, ok := registrant.(io.Closer); ok {
			errs = append(errs, closer.Close())
		}
	}
	return errors.Join(errs...)
}

type Sessions struct {
	Registry
	config *config.Config
}

func New(cfg *config.Config) *Sessions {
	s := &Sessions{config: cfg}
	sessionsconfig.Register(cfg, func() []config.Option {
		var options []config.Option
		for _, name := range slices.Sorted(maps.Keys(s.Stores())) {
			options = append(options, config.Option{Value: name, Title: name})
		}
		return options
	})
	return s
}

// Store sessions are kept in
func (s *Sessions) Current() (Store, error) {
	return s.Store(s.config.GetString(sessionsconfig.Store))
}

// Deletes the sessions of a deleted profile
func (s *Sessions) Delete(profile string) error {
	store, err := s.Current()
	if err != nil {
		return err
	}
	return store.Clear(profile)
}
