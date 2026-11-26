package profile

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/internal/config"
	profileconfig "github.com/nickheyer/icebath/internal/profile/config"
)

// Profile that is always there and can't be deleted, the one settings are kept in until another is made and used
const Default = "default"

// What a feature keeps for each profile, removed with it when it is deleted
type Registrant interface {
	Delete(profile string) error
}

// What a feature does once the profile in use changes, for one that keeps something of the profile in use
type User interface {
	Use(profile string) error
}

type Registry struct {
	registrants []Registrant
}

func (r *Registry) Register(registrant Registrant) {
	r.registrants = append(r.registrants, registrant)
}

func (r *Registry) Delete(profile string) error {
	var errs []error
	for _, registrant := range r.registrants {
		errs = append(errs, registrant.Delete(profile))
	}
	return errors.Join(errs...)
}

func (r *Registry) Use(profile string) error {
	var errs []error
	for _, registrant := range r.registrants {
		if user, ok := registrant.(User); ok {
			errs = append(errs, user.Use(profile))
		}
	}
	return errors.Join(errs...)
}

// Profiles of the config folder: one folder each under profiles, holding the settings that aren't shared by all of them
type Profiles struct {
	Registry
	config *config.Config
	mu     sync.Mutex
	active string
}

func New(cfg *config.Config) *Profiles {
	p := &Profiles{config: cfg}
	profileconfig.Register(cfg, p.options)
	return p
}

func (p *Profiles) options() []config.Option {
	names, _ := p.List()
	options := make([]config.Option, len(names))
	for i, name := range names {
		options[i] = config.Option{Value: name, Title: name}
	}
	return options
}

// Reads the settings, makes the default profile when it isn't there and uses the profile picked last, or the default one when that is gone
func (p *Profiles) Load() error {
	if err := p.config.Load(); err != nil {
		return err
	}
	if err := os.MkdirAll(p.config.ProfileDir(Default), 0o700); err != nil {
		return err
	}
	name := p.config.GetString(profileconfig.Active)
	if !p.exists(name) {
		name = Default
	}
	return p.use(name)
}

// Profile in use
func (p *Profiles) Active() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.active
}

// Every profile by name, the default one first
func (p *Profiles) List() ([]string, error) {
	entries, err := os.ReadDir(filepath.Dir(p.config.ProfileDir(Default)))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	names := []string{Default}
	for _, entry := range entries {
		if entry.IsDir() && config.Named(entry.Name()) && entry.Name() != Default {
			names = append(names, entry.Name())
		}
	}
	slices.Sort(names[1:])
	return names, nil
}

func (p *Profiles) exists(name string) bool {
	return p.config.Exists(name)
}

// Makes a profile, with the settings of the one named from when it is given, else with none
func (p *Profiles) Create(name, from string) error {
	if !config.Named(name) {
		return fmt.Errorf("%q can't name a profile. Use lower case letters, digits, dashes or underscores.", name)
	}
	if p.exists(name) {
		return fmt.Errorf("There is already a profile named %s.", name)
	}
	var settings []byte
	if from != "" {
		if !p.exists(from) {
			return fmt.Errorf("No profile named %s.", from)
		}
		var err error
		if settings, err = os.ReadFile(filepath.Join(p.config.ProfileDir(from), "config.yaml")); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	dir := p.config.ProfileDir(name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if settings == nil {
		return nil
	}
	return os.WriteFile(filepath.Join(dir, "config.yaml"), settings, 0o600)
}

// Deletes a profile and everything kept for it. The default profile and the one in use can't be
func (p *Profiles) Delete(name string) error {
	switch {
	case name == Default:
		return errors.New("The default profile can't be deleted.")
	case !p.exists(name):
		return fmt.Errorf("No profile named %s.", name)
	case name == p.Active():
		return fmt.Errorf("%s is in use. Switch to another profile first.", name)
	}
	if err := p.Registry.Delete(name); err != nil {
		return err
	}
	p.config.Forget(name)
	return os.RemoveAll(p.config.ProfileDir(name))
}

// Uses the profile from now on, and the next time icebath starts
func (p *Profiles) Use(name string) error {
	if !p.exists(name) {
		return fmt.Errorf("No profile named %s.", name)
	}
	if err := p.use(name); err != nil {
		return err
	}
	return p.config.Set(profileconfig.Active, name)
}

func (p *Profiles) use(name string) error {
	if err := p.config.Use(name); err != nil {
		return err
	}
	p.mu.Lock()
	p.active = name
	p.mu.Unlock()
	return p.Registry.Use(name)
}

// Profile a screen is opened on and the rest of its argument: a leading @name names the profile, the one in use when there is none
func Split(argument string) (profile, rest string) {
	argument = strings.TrimSpace(argument)
	if !strings.HasPrefix(argument, "@") {
		return "", argument
	}
	profile, rest, _ = strings.Cut(argument[1:], " ")
	return profile, strings.TrimSpace(rest)
}
