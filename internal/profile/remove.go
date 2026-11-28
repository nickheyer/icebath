package profile

import (
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/fsx"
	"github.com/nickheyer/icebath/pkg/strs"
)

// What a delete, rename or identity retry did, and whether every feature settled what it keeps under the name
type Done struct {
	Path string
	// Name the profile has now, or the name the default profile shows
	Name string
	// What was done besides the act itself, and what failed, each a sentence; the act stands all the same
	Done, Failed []string
	Settled      bool
	// Why a delete failed once nothing could stop it
	Failure string
}

// Removes a tree, giving the owner rights on what refuses once before trying again, never through a link at its root
func removeTree(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return os.Remove(dir)
	}
	if err := os.RemoveAll(dir); err == nil || !errors.Is(err, fs.ErrPermission) {
		return err
	}
	filepath.WalkDir(dir, func(at string, entry fs.DirEntry, err error) error {
		if err == nil && entry.IsDir() {
			os.Chmod(at, 0o700)
		}
		return nil
	})
	return os.RemoveAll(dir)
}

// Deletes a profile: every feature stops what it runs for it and removes what it keeps of it, then its folder goes. Deleting the profile in
// use switches to the default one first, and the profile icebath starts with goes back to the default when it was this one, even when the
// folder would not go
func (p *Profiles) Delete(name string) (Done, error) {
	canon, err := Normalize(name)
	if err != nil {
		return Done{}, err
	}
	if canon == Default {
		return Done{}, &ValueError{fmt.Sprintf("The default profile in %s can't be deleted. To remove everything icebath keeps, delete %s.",
			p.Dir(Default), p.config.Dir())}
	}
	if canon, err = Canon(canon); err != nil {
		return Done{}, err
	}
	if !p.config.Exists(canon) {
		return Done{}, unknown(canon)
	}
	if why := p.Registry.Holds(canon); why != "" {
		return Done{}, &ValueError{why}
	}
	dir := p.Dir(canon)
	done := Done{Path: dir, Name: canon, Settled: true}
	if canon == p.Active() {
		if err := p.use(Default); err != nil {
			return Done{}, err
		}
		done.Done = append(done.Done, "icebath uses the default profile now.")
	}
	for _, err := range failures(p.Registry.Delete(canon)) {
		done.Settled = false
		done.Failed = append(done.Failed, failed(true, err))
	}
	p.config.Forget(canon)
	if removeErr := removeTree(dir); removeErr == nil {
		done.Done = append(done.Done, "Removed the folder "+Tilde(dir)+".")
	} else {
		done.Failure = fmt.Sprintf("The folder %s could not be removed: %s.", dir, failure(removeErr))
	}
	if p.sticky() == canon && p.stick(Default) == nil {
		done.Done = append(done.Done, "icebath starts with the default profile from now on.")
	}
	return done, nil
}

// Renames a profile: what runs for it stops, its folder moves, every feature moves what it keeps under the old name, and the profile in use
// and the one icebath starts with follow. Renaming the default profile only sets the name it shows
func (p *Profiles) Rename(old, renamed string) (Done, error) {
	oldCanon, err := Canon(old)
	if err != nil {
		return Done{}, err
	}
	if oldCanon == Default {
		return p.displayName(renamed)
	}
	newCanon, err := Canon(renamed)
	if err != nil {
		return Done{}, err
	}
	if newCanon == Default {
		return Done{}, &ValueError{"No profile can be renamed to default, because that is the name of the built-in profile."}
	}
	if backend, err := p.config.Backend(); err != nil {
		return Done{}, err
	} else if backend != "file" {
		return Done{}, &ValueError{fmt.Sprintf("%s can't be renamed, because its settings are kept in the %q settings store under that name, "+
			"which can't rename them. Nothing changed.", oldCanon, backend)}
	}
	oldDir, newDir := p.Dir(oldCanon), p.Dir(newCanon)
	if !p.config.Exists(oldCanon) {
		return Done{}, unknown(oldCanon)
	}
	if fsx.Lexists(newDir) {
		return Done{}, taken(newCanon)
	}
	if why := p.Registry.Holds(oldCanon); why != "" {
		return Done{}, &ValueError{why}
	}
	done := Done{Path: newDir, Name: newCanon, Settled: true}
	if err := p.Registry.Stop(oldCanon); err != nil {
		done.Failed = append(done.Failed, fmt.Sprintf("Some of what runs for %s could not be stopped: %s", oldCanon, strs.Sentence(err.Error())))
	}
	p.config.Forget(oldCanon)
	if err := os.Rename(oldDir, newDir); err != nil {
		return Done{}, err
	}
	history := ReadMeta(newDir).PreviousNames
	if !slices.Contains(history, oldCanon) {
		history = append(history, oldCanon)
	}
	// The history only tells; a rename it could not record is no failure
	WriteMeta(newDir, MetaUpdate{PreviousNames: &history})
	if p.Active() == oldCanon {
		if err := p.use(newCanon); err != nil {
			return done, err
		}
	}
	if p.sticky() == oldCanon && p.stick(newCanon) == nil {
		done.Done = append(done.Done, "icebath starts with "+newCanon+" from now on.")
	}
	moved, broken := p.migrate(oldCanon, newCanon)
	done.Done, done.Failed, done.Settled = append(done.Done, moved...), append(done.Failed, broken...), len(broken) == 0
	return done, nil
}

// Has every feature move what it keeps under the old name to the new one: what moved and what did not, each a sentence
func (p *Profiles) migrate(old, renamed string) (moved, broken []string) {
	lines, err := p.Registry.Move(old, renamed)
	for _, line := range lines {
		moved = append(moved, unmarked(line))
	}
	for _, err := range failures(err) {
		broken = append(broken, failed(false, err))
	}
	return moved, broken
}

// A line a feature reports without the mark it may start with
func unmarked(line string) string {
	return strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line), "✓⚠✗"))
}

// Sets the name the default profile shows, or clears it for no name so it shows as default again, its name staying default: the name
// it shows now
func (p *Profiles) displayName(name string) (Done, error) {
	cleaned := strings.TrimSpace(name)
	if strings.EqualFold(cleaned, Default) {
		cleaned = ""
	}
	if n := utf8.RuneCountInString(cleaned); n > 64 {
		return Done{}, &ValueError{fmt.Sprintf("The name is %s long, and the most it can be is 64.", strs.Counted(float64(n), "characters"))}
	}
	dir := p.Dir(Default)
	if err := WriteMeta(dir, MetaUpdate{DisplayName: &cleaned}); err != nil {
		return Done{}, err
	}
	return Done{Path: dir, Name: cmp.Or(cleaned, Default), Settled: true}, nil
}

// Removes again what the features keep of a deleted profile, for a delete whose removal of it failed: a line for each that failed again
func (p *Profiles) PurgeIdentity(name string) (Done, error) {
	canon, err := Canon(name)
	if err != nil {
		return Done{}, err
	}
	if canon == Default {
		return Done{}, &ValueError{"purge-identity finishes the delete of a profile, and the default profile can't be deleted."}
	}
	if p.config.Exists(canon) {
		return Done{}, &ValueError{fmt.Sprintf("The profile %s still exists. purge-identity only finishes a delete that already happened.", canon)}
	}
	done := Done{Name: canon, Settled: true}
	for _, err := range failures(p.Registry.Delete(canon)) {
		done.Settled = false
		done.Failed = append(done.Failed, failed(true, err))
	}
	return done, nil
}

// Moves again what the features keep under a renamed profile's old name, for a rename whose move of it failed: what moved and what did not
func (p *Profiles) MigrateIdentity(old, renamed string) (Done, error) {
	oldCanon, err := Canon(old)
	if err != nil {
		return Done{}, err
	}
	newCanon, err := Canon(renamed)
	if err != nil {
		return Done{}, err
	}
	if oldCanon == Default || newCanon == Default {
		return Done{}, &ValueError{"migrate-identity finishes the rename of a profile, and the default profile keeps its name."}
	}
	if !p.config.Exists(newCanon) {
		return Done{}, unknown(newCanon)
	}
	moved, broken := p.migrate(oldCanon, newCanon)
	return Done{Path: p.Dir(newCanon), Name: newCanon, Done: moved, Failed: broken, Settled: len(broken) == 0}, nil
}

// Sets a profile's description, the default profile's included
func (p *Profiles) SetDescription(name, description string) error {
	canon, err := Normalize(name)
	if err != nil {
		return err
	}
	if canon != Default {
		if canon, err = Canon(canon); err != nil {
			return err
		}
	}
	if !p.Exists(canon) {
		return missing(canon)
	}
	auto := false
	return WriteMeta(p.Dir(canon), MetaUpdate{Description: &description, DescriptionAuto: &auto})
}
