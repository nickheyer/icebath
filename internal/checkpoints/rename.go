package checkpoints

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/nickheyer/icebath/pkg/fsx"
)

// Moves the projects that lay in the old folder of a renamed profile, now in its new one, to the keys of where they are now: their snapshots,
// metadata and agent write ledgers. A project whose new key already holds snapshots that do not continue its own is left and told; how many
// moved
func (s *Store) Rekey(oldRoot, newRoot string) (int, error) {
	if !s.made() {
		return 0, nil
	}
	if _, err := exec.LookPath("git"); err != nil {
		return 0, errors.New("The checkpoints could not move with the renamed profile, because git is not installed or not on your PATH.")
	}
	release, err := lock(s.base)
	if err != nil {
		return 0, err
	}
	defer release()
	from, to := normalize(oldRoot), normalize(newRoot)
	migrated := 0
	var errs []error
	for _, meta := range s.projects() {
		if meta.Workdir == "" {
			continue
		}
		rel, err := filepath.Rel(from, normalize(meta.Workdir))
		if err != nil || !filepath.IsLocal(rel) {
			continue
		}
		workdir := filepath.Join(to, rel)
		if !fsx.IsDir(workdir) {
			continue
		}
		if err := s.rekeyProject(meta, normalize(meta.Workdir), normalize(workdir)); err != nil {
			errs = append(errs, fmt.Errorf("The checkpoints of %s could not move to %s with the renamed profile: %w", meta.Workdir, workdir, err))
			continue
		}
		migrated++
	}
	return migrated, errors.Join(errs...)
}

// Moves one project to the key of its new folder: its snapshots taken over unless the new key holds others, its ledger folded in, the old
// key's files removed
func (s *Store) rekeyProject(meta project, old, workdir string) error {
	newHash := hash(workdir)
	oldRef, newRef := ref(meta.Hash), ref(newHash)
	oldTip, newTip := s.tip(workdir, oldRef), s.tip(workdir, newRef)
	if oldTip != "" && newTip != "" {
		if ok, _, _ := s.git(workdir, step{allowed: []int{1}}, "merge-base", "--is-ancestor", oldTip, newTip); !ok {
			return fmt.Errorf("%s already exists and does not come from %s.", newRef, oldRef)
		}
	}
	if !fsx.Exists(s.meta(newHash)) {
		moved := meta
		moved.Workdir = workdir
		if dev, ino, err := identity(filepath.Dir(workdir)); err == nil && dev != 0 && ino != 0 {
			moved.ParentDev, moved.ParentIno = dev, ino
		}
		data, err := json.Marshal(moved)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(s.meta(newHash)), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(s.meta(newHash), data, 0o600); err != nil {
			return err
		}
	}
	if fsx.Exists(s.ledgerPath(meta.Hash)) {
		ledger := map[string]write{}
		for path, entry := range s.ledger(meta.Hash) {
			if rel, err := filepath.Rel(old, path); err == nil && filepath.IsLocal(rel) {
				path = filepath.Join(workdir, rel)
			}
			ledger[path] = entry
		}
		maps.Copy(ledger, s.ledger(newHash))
		s.keep(newHash, ledger)
	}
	if oldTip != "" {
		if newTip == "" {
			if ok, _, errs := s.git(workdir, step{}, "update-ref", newRef, oldTip); !ok {
				return fmt.Errorf("%s could not be made: %s", newRef, errs)
			}
		}
		if ok, _, _ := s.git(workdir, step{}, "update-ref", "-d", oldRef); !ok {
			return fmt.Errorf("%s could not be deleted.", oldRef)
		}
	}
	for _, path := range []string{s.meta(meta.Hash), s.ledgerPath(meta.Hash), s.index(meta.Hash)} {
		os.Remove(path)
	}
	return nil
}
