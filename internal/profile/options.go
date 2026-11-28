package profile

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/nickheyer/icebath/pkg/fsx"
)

// Record of what a profile imports from other agents, such as Claude Code and Codex, which a profile that keeps syncing them holds a copy of
const importSync = "import-sync.json"

// Folder of a profile that is there, by its canonical name
func (p *Profiles) folder(name string) (string, string, error) {
	canon, err := Normalize(name)
	if err != nil {
		return "", "", err
	}
	if canon != Default {
		if canon, err = Canon(canon); err != nil {
			return "", "", err
		}
	}
	if !p.Exists(canon) {
		return "", "", missing(canon)
	}
	return canon, p.Dir(canon), nil
}

// Whether the profile gets every skill that comes with icebath, not only the essential ones
func bundled(dir string) bool {
	return !fsx.Exists(filepath.Join(dir, noSkillsMarker))
}

// Gives the profile every skill that comes with icebath from its next start on, or only the essential ones: the note of what changed
func (p *Profiles) SetBundled(name string, on bool) (string, error) {
	canon, dir, err := p.folder(name)
	if err != nil {
		return "", err
	}
	marker := filepath.Join(dir, noSkillsMarker)
	if on {
		if err := os.Remove(marker); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		return fmt.Sprintf("Bundled skills is now On. %s gets every skill that comes with icebath the next time icebath starts.", canon), nil
	}
	if err := seedFile(marker, "Only the essential bundled skills are copied into this profile.\nDelete this file, and the next time icebath "+
		"starts it copies every bundled skill into this profile.\n", 0o644); err != nil {
		return "", err
	}
	return fmt.Sprintf("Bundled skills is now Off. %s keeps the skills it has and gets only the essential ones from now on.", canon), nil
}

// Whether the profile keeps pulling what another profile imports from other agents
func syncing(dir string) bool {
	return fsx.IsFile(filepath.Join(dir, importSync))
}

// Has the profile keep pulling what the profile in use imports from other agents, by a copy of its record, or stop: the note of what changed
func (p *Profiles) SetSyncImports(name string, on bool) (string, error) {
	canon, dir, err := p.folder(name)
	if err != nil {
		return "", err
	}
	if !on {
		if err := os.Remove(filepath.Join(dir, importSync)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		return "Keep syncing imports is now Off for " + canon + ".", nil
	}
	from := p.Active()
	if from == canon {
		return "", &ValueError{canon + " is the profile in use, so it has no other profile to sync imports from. Switch to another first."}
	}
	if !fsx.IsFile(filepath.Join(p.Dir(from), importSync)) {
		return "", &ValueError{from + " imports nothing from other agents such as Claude Code or Codex, so there is nothing to keep syncing."}
	}
	if err := copyFile(p.Dir(from), dir, importSync); err != nil {
		return "", err
	}
	return "Keep syncing imports is now On. " + canon + " pulls what " + from + " imports from other agents.", nil
}
