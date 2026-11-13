package backend

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"

	"github.com/google/uuid"

	"github.com/nickheyer/icebath/pkg/desktop/hardening"
	"github.com/nickheyer/icebath/pkg/desktop/jsjson"
)

var bootPreferenceProfile = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// Whether a boot profile is null or a valid profile name
func validBootProfile(profile *string) bool {
	return profile == nil || bootPreferenceProfile.MatchString(*profile)
}

// Boot preference on disk, nil for a missing file and an error for unreadable or invalid state
func ReadDesktopBootPreference(file string) (*jsjson.OrderedObject, error) {
	raw, err := os.ReadFile(file)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, hardening.NodeErr(err)
	}
	return parseDesktopBootPreference(utf8Lossy(raw))
}

// Parses the preference JSON, keeping fields other owners wrote and checking profile and migration marker
func parseDesktopBootPreference(text string) (*jsjson.OrderedObject, error) {
	parsed, err := jsjson.ParseOrdered(text)
	if err != nil {
		return nil, err
	}
	obj, ok := parsed.(*jsjson.OrderedObject)
	if !ok || !obj.Has("profile") {
		return nil, errors.New("Invalid desktop boot preference")
	}
	profile := obj.Get("profile")
	if _, isString := profile.(string); profile != nil && (!isString || !bootPreferenceProfile.MatchString(profile.(string))) {
		return nil, errors.New("Invalid profile in desktop boot preference")
	}
	if obj.Has("_migrated") {
		if _, ok := obj.Get("_migrated").(bool); !ok {
			return nil, errors.New("Invalid migration marker in desktop boot preference")
		}
	}
	return obj, nil
}

// Rewrites the preference through a synced temporary file renamed over it, refusing unreadable state
func updateBootPreference(file string, update func(current *jsjson.OrderedObject) *jsjson.OrderedObject) (*jsjson.OrderedObject, error) {
	if err := os.MkdirAll(filepath.Dir(file), 0o777); err != nil {
		return nil, hardening.NodeErr(err)
	}
	temporary := file + "." + uuid.NewString() + ".tmp"
	defer os.Remove(temporary)
	current, err := ReadDesktopBootPreference(file)
	if err != nil {
		return nil, fmt.Errorf("Cannot update an unreadable desktop boot preference: %w", err)
	}
	next := update(current)
	if err := os.WriteFile(temporary, []byte(jsjson.Stringify(next, "  ", "")+"\n"), 0o600); err != nil {
		return nil, hardening.NodeErr(err)
	}
	if err := syncPath(temporary, os.O_RDWR); err != nil {
		return nil, err
	}
	if err := os.Rename(temporary, file); err != nil {
		return nil, hardening.NodeErr(err)
	}
	if runtime.GOOS != "windows" {
		if err := syncPath(filepath.Dir(file), os.O_RDONLY); err != nil {
			return nil, err
		}
	}
	return next, nil
}

// Opens a path and fsyncs it, closing it either way
func syncPath(path string, flag int) error {
	f, err := os.OpenFile(path, flag, 0)
	if err != nil {
		return hardening.NodeErr(err)
	}
	syncErr := f.Sync()
	closeErr := f.Close()
	if syncErr != nil {
		return hardening.NodeErr(syncErr)
	}
	return hardening.NodeErr(closeErr)
}

// Stores the boot profile, null for the default, dropping the migration marker and keeping every other field
func WriteDesktopProfile(file string, profile *string) (*string, error) {
	if !validBootProfile(profile) {
		return nil, fmt.Errorf("Invalid profile name: %s", *profile)
	}
	next, err := updateBootPreference(file, func(current *jsjson.OrderedObject) *jsjson.OrderedObject {
		next := jsjson.NewOrderedObject()
		if current != nil {
			next = current.Clone()
		}
		if profile == nil {
			next.SetRaw("profile", nil)
		} else {
			next.SetRaw("profile", *profile)
		}
		next.Delete("_migrated")
		return next
	})
	if err != nil {
		return nil, err
	}
	if name, ok := next.Get("profile").(string); ok {
		return &name, nil
	}
	return nil, nil
}
