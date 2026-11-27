package profile

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/fsx"
)

// What a delete, rename or identity retry did, line by line, and whether every feature settled what it keeps under the name
type Done struct {
	Path string
	// Lines said before the profile's alias is dealt with, and those said after it
	Lines, Later []string
	Settled      bool
	// Why a delete failed once nothing could stop it, said after its lines
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
		return Done{}, &ValueError{fmt.Sprintf("Cannot delete the default profile (%s).\nTo remove everything, delete %s", p.Dir(Default),
			p.config.Dir())}
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
	done := Done{Path: dir, Settled: true}
	if canon == p.Active() {
		if err := p.use(Default); err != nil {
			return Done{}, err
		}
		done.Lines = append(done.Lines, "✓ Switched to the default profile")
	}
	for _, err := range failures(p.Registry.Delete(canon)) {
		done.Settled = false
		done.Lines = append(done.Lines, failed("deleted", "identity purge", err))
	}
	p.config.Forget(canon)
	removeErr := removeTree(dir)
	if removeErr == nil {
		done.Later = append(done.Later, "✓ Removed "+dir)
	} else {
		done.Later = append(done.Later, fmt.Sprintf("⚠ Could not remove %s: %s", dir, failure(removeErr)))
		done.Failure = fmt.Sprintf("Could not remove profile directory %s: %s", dir, failure(removeErr))
	}
	if p.sticky() == canon && p.stick(Default) == nil {
		done.Later = append(done.Later, "✓ Active profile reset to default")
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
		return Done{}, &ValueError{"Cannot rename to 'default' — it is reserved."}
	}
	if backend, err := p.config.Backend(); err != nil {
		return Done{}, err
	} else if backend != "file" {
		return Done{}, &ValueError{fmt.Sprintf("Cannot rename profile '%s': its config lives in the %q config backend under that name, and renaming "+
			"it there is not supported. Nothing was changed.", oldCanon, backend)}
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
	done := Done{Path: newDir, Settled: true}
	if err := p.Registry.Stop(oldCanon); err != nil {
		done.Lines = append(done.Lines, fmt.Sprintf("⚠ Could not stop everything running for %s: %v", oldCanon, err))
	}
	p.config.Forget(oldCanon)
	if err := os.Rename(oldDir, newDir); err != nil {
		return Done{}, err
	}
	done.Lines = append(done.Lines, fmt.Sprintf("✓ Renamed %s → %s", oldCanon, newCanon))
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
		done.Later = append(done.Later, "✓ Active profile updated: "+newCanon)
	}
	moved, settled := p.migrate(oldCanon, newCanon)
	done.Later, done.Settled = append(done.Later, moved...), settled
	return done, nil
}

// Has every feature move what it keeps under the old name to the new one: the lines saying what moved and what did not, and whether all did
func (p *Profiles) migrate(old, renamed string) ([]string, bool) {
	lines, err := p.Registry.Move(old, renamed)
	broken := failures(err)
	for _, err := range broken {
		lines = append(lines, failed("renamed", "identity migration", err))
	}
	return lines, len(broken) == 0
}

// Sets or clears the name the default profile shows, its name staying default
func (p *Profiles) displayName(name string) (Done, error) {
	if strings.TrimSpace(name) == "" {
		return Done{}, &ValueError{"Display name cannot be empty."}
	}
	cleaned := strings.TrimSpace(name)
	if n := utf8.RuneCountInString(cleaned); n > 64 {
		return Done{}, &ValueError{fmt.Sprintf("Display name too long (%d chars, max 64).", n)}
	}
	dir := p.Dir(Default)
	if err := WriteMeta(dir, MetaUpdate{DisplayName: &cleaned}); err != nil {
		return Done{}, err
	}
	return Done{Path: dir, Settled: true, Lines: []string{fmt.Sprintf("✓ Display name set: %s (canonical id remains 'default')", cleaned)}}, nil
}

// Removes again what the features keep of a deleted profile, for a delete whose removal of it failed: a line for each that failed again
func (p *Profiles) PurgeIdentity(name string) (Done, error) {
	canon, err := Canon(name)
	if err != nil {
		return Done{}, err
	}
	if canon == Default {
		return Done{}, &ValueError{"Identity purge applies to named profiles only."}
	}
	if p.config.Exists(canon) {
		return Done{}, &ValueError{fmt.Sprintf("Profile '%s' exists; purge-identity only settles the identity of a delete that has already completed.",
			canon)}
	}
	done := Done{Settled: true}
	for _, err := range failures(p.Registry.Delete(canon)) {
		done.Settled = false
		done.Lines = append(done.Lines, failed("deleted", "identity purge", err))
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
		return Done{}, &ValueError{"Identity migration applies to named profiles only."}
	}
	if !p.config.Exists(newCanon) {
		return Done{}, unknown(newCanon)
	}
	lines, settled := p.migrate(oldCanon, newCanon)
	return Done{Path: p.Dir(newCanon), Lines: lines, Settled: settled}, nil
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
		return &NotFoundError{fmt.Sprintf("profile directory does not exist: %s", p.Dir(canon))}
	}
	auto := false
	return WriteMeta(p.Dir(canon), MetaUpdate{Description: &description, DescriptionAuto: &auto})
}
