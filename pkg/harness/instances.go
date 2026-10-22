package harness

import (
	"errors"
	"fmt"
	"os"

	"github.com/nickheyer/icebath/pkg/profiles"
)

// One profile a profile file holds, with its template applied and as written
type Instance struct {
	Profile profiles.Profile
	Written profiles.Profile
	Path    string
}

// Profile files in read order, the configured lists and then the user file
func profileFiles(cfg Config) ([]string, error) {
	user, err := profiles.UserPath()
	if err != nil {
		return nil, err
	}
	return append(append([]string(nil), cfg.ProfileFiles...), user), nil
}

// Profiles of every profile file in order, a later namesake replacing in place, with the last default named
func loadInstances(cfg Config) ([]Instance, string, error) {
	files, err := profileFiles(cfg)
	if err != nil {
		return nil, "", err
	}
	var out []Instance
	var def string
	for i, path := range files {
		doc, err := profiles.ReadFile(path)
		if i == len(files)-1 && errors.Is(err, os.ErrNotExist) {
			break
		}
		if err != nil {
			return nil, "", err
		}
		if doc.Default != "" {
			def = doc.Default
		}
		for _, w := range doc.Profiles {
			p, err := w.Expand()
			if err != nil {
				return nil, "", err
			}
			in := Instance{Profile: p, Written: w, Path: path}
			if j := indexOf(out, p.Name); j >= 0 {
				out[j] = in
				continue
			}
			out = append(out, in)
		}
	}
	return out, def, nil
}

func indexOf(list []Instance, name string) int {
	for i, in := range list {
		if in.Profile.Name == name {
			return i
		}
	}
	return -1
}

// Profile by name, a written one before the built in one it may shadow
func (a *App) FindProfile(name string) (profiles.Profile, bool) {
	if i := indexOf(a.Profiles, name); i >= 0 {
		return a.Profiles[i].Profile, true
	}
	return profiles.Lookup(name)
}

// Written profiles in file order
func (a *App) Instances() []Instance { return append([]Instance(nil), a.Profiles...) }

// Reports whether a written profile carries the name
func (a *App) Shadowed(name string) bool { return indexOf(a.Profiles, name) >= 0 }

// Reads every profile file again
func (a *App) ReloadProfiles() error {
	list, _, err := loadInstances(a.Config)
	if err != nil {
		return err
	}
	a.Profiles = list
	return nil
}

// File holding the profile, the user file for a new one
func (a *App) profilePath(name string) (string, error) {
	if i := indexOf(a.Profiles, name); i >= 0 {
		return a.Profiles[i].Path, nil
	}
	return profiles.UserPath()
}

// Reads a profile file, empty when it does not exist yet
func readDocument(path string) (profiles.Document, error) {
	doc, err := profiles.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return profiles.Document{}, nil
	}
	return doc, err
}

// Applies a change to the file holding the profile, writes it and reads every file again
func (a *App) editProfiles(name string, change func(*profiles.Document) error) error {
	path, err := a.profilePath(name)
	if err != nil {
		return err
	}
	doc, err := readDocument(path)
	if err != nil {
		return err
	}
	if err := change(&doc); err != nil {
		return err
	}
	if err := profiles.WriteFile(path, doc); err != nil {
		return err
	}
	return a.ReloadProfiles()
}

// Writes a profile over the one named old, or adds it to the user file when old is empty
func (a *App) SaveProfile(old string, p profiles.Profile) error {
	if p.Name != old {
		if a.Shadowed(p.Name) {
			return fmt.Errorf("a profile named %s already exists", p.Name)
		}
		if _, builtin := profiles.Lookup(p.Name); builtin {
			return fmt.Errorf("%s names a built in provider, pick another name", p.Name)
		}
	}
	if old != "" && p.Disabled && a.Primary.Profile.Name == old {
		return fmt.Errorf("switch away from %s before turning it off", old)
	}
	e, err := p.Expand()
	if err != nil {
		return err
	}
	if err := e.Validate(); err != nil {
		return err
	}
	return a.editProfiles(old, func(d *profiles.Document) error { return d.Put(old, p) })
}

// Deletes a written profile, refusing the one in use
func (a *App) RemoveProfile(name string) error {
	if a.Primary.Profile.Name == name {
		return fmt.Errorf("switch away from %s before deleting it", name)
	}
	if !a.Shadowed(name) {
		return fmt.Errorf("no written profile %s", name)
	}
	return a.editProfiles(name, func(d *profiles.Document) error { return d.Remove(name) })
}

// Moves a written profile delta places through its file
func (a *App) MoveProfile(name string, delta int) error {
	if !a.Shadowed(name) {
		return fmt.Errorf("no written profile %s", name)
	}
	return a.editProfiles(name, func(d *profiles.Document) error { return d.Move(name, delta) })
}

// Turns a written profile on or off, refusing to turn off the one in use
func (a *App) SetProfileEnabled(name string, on bool) error {
	i := indexOf(a.Profiles, name)
	if i < 0 {
		return fmt.Errorf("no written profile %s", name)
	}
	if !on && a.Primary.Profile.Name == name {
		return fmt.Errorf("switch away from %s before turning it off", name)
	}
	w := a.Profiles[i].Written
	w.Disabled = !on
	return a.editProfiles(name, func(d *profiles.Document) error { return d.Put(name, w) })
}
