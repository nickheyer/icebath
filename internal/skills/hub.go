package skills

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/internal/config"
	skillsconfig "github.com/nickheyer/icebath/internal/skills/config"
	"github.com/nickheyer/icebath/pkg/fsx"
)

// What schedules jobs, the cron: offers the blueprint of a skill the hub installed as a job the user may schedule. A registrant that
// schedules says so by having this too
type Scheduler interface {
	// Adds the blueprint to the suggestions the user reviews, false when it was not added: offered or dismissed before, or the list full
	Suggest(ctx context.Context, cfg *config.Config, blueprint *Blueprint) (bool, error)
}

var ErrNoScheduler = errors.New("Nothing schedules jobs here, so the blueprint was not offered.")

// Offers the blueprint of an installed skill as a job to schedule, false when it was not added
func (r *Registry) Suggest(ctx context.Context, cfg *config.Config, blueprint *Blueprint) (bool, error) {
	scheduler, ok := capability[Scheduler](r)
	if !ok {
		return false, ErrNoScheduler
	}
	return scheduler.Suggest(ctx, cfg, blueprint)
}

// One skill an offered source carries for the hub to install
type Offer struct {
	// Declared name, its folder's name when it has none
	Name string
	// Its folder in the source's files, with slashes, which is also the folder it installs to
	Dir    string
	Files  fs.FS
	Source Source
}

// Skills of the offered sources in path order
func (s *Scope) Offers() []Offer {
	var out []Offer
	for _, o := range s.offers() {
		out = append(out, Offer{Name: o.name, Dir: o.dir, Files: o.tree, Source: o.source})
	}
	return out
}

// Holds an exclusive lock on the path across processes, waiting for it, until release is called
func Hold(ctx context.Context, path string) (func(), error) {
	_, release, err := hold(ctx, path)
	return release, err
}

// Changes the hub's lock of a skills folder while holding it, written when change says it changed
func UpdateLock(ctx context.Context, skillsDir string, change func(lock *Lock) (bool, error)) error {
	release, err := Hold(ctx, lockPath(skillsDir)+".lock")
	if err != nil {
		return err
	}
	defer release()
	lock, err := ReadLock(skillsDir)
	if err != nil {
		return err
	}
	dirty, err := change(lock)
	if err != nil || !dirty {
		return err
	}
	return WriteLock(skillsDir, lock)
}

// Display form of a path, the user's home folder as ~
func Display(p string) string {
	return display(p)
}

// Folders directly in a folder that hold a skill somewhere below, hidden folders left out, sorted
func Categories(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, entry := range entries {
		path := filepath.Join(dir, entry.Name())
		if !strings.HasPrefix(entry.Name(), ".") && fsx.IsDir(path) && len(skillFiles(path)) > 0 {
			out = append(out, entry.Name())
		}
	}
	slices.Sort(out)
	return out
}

// What trusting a project with its skills, or no longer trusting it, did
type Trust struct {
	// Project folder, resolved
	Root string
	// Whether the trusted folders changed; when not, the project was already trusted, or was not trusted
	Changed bool
	// Skills the project holds, which load in sessions working in it while it is trusted
	Skills int
}

// Folder of this computer a trusted project names, resolved: a relative one taken from the scope's folder; an error when it is no folder
func (s *Scope) folder(entry string) (string, error) {
	path := fsx.ExpandHome(s.expandVars(strings.TrimSpace(entry)))
	if path == "" {
		return "", errors.New("A trusted project is a folder, such as ~/code/project.")
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(s.Cwd, path)
	}
	root := resolve(path)
	if !fsx.IsDir(root) {
		return root, errors.New("There is no folder at " + root + ".")
	}
	return root, nil
}

// Trusted projects as the settings keep them
func (s *Scope) TrustedDirs() []string {
	return list(s.Config.Get(skillsconfig.TrustedProjectDirs))
}

// Folders the entries name, each once by its whole path: one trusted already as it is, though it may be gone since, and any other only when
// it is a folder of this computer
func (s *Scope) trustable(entries []string) ([]string, error) {
	trusted := map[string]bool{}
	for _, entry := range s.TrustedDirs() {
		trusted[s.path(entry)] = true
	}
	kept := []string{}
	for _, entry := range entries {
		root := s.path(entry)
		if !trusted[root] {
			var err error
			if root, err = s.folder(entry); err != nil {
				return nil, err
			}
		}
		if !slices.Contains(kept, root) {
			kept = append(kept, root)
		}
	}
	return kept, nil
}

// Keeps the trusted projects, each a folder kept once by its whole path, a new one only when it is there: what trusting a project, no
// longer trusting it and every write of the setting a person asks for end in
func (s *Scope) KeepTrusted(entries []string) error {
	kept, err := s.trustable(entries)
	if err != nil {
		return err
	}
	var value any = kept
	if len(kept) == 0 {
		value = nil
	}
	if err := s.Config.Set(skillsconfig.TrustedProjectDirs, value); err != nil {
		return err
	}
	s.registry.Forget()
	return nil
}

// Trusts a project with its own skills, or stops trusting it: the folder given, a relative one taken from the scope's folder, else the
// git checkout the scope's folder is in
func (s *Scope) SetTrusted(path string, trusted bool) (Trust, error) {
	var root string
	if strings.TrimSpace(path) != "" {
		var err error
		// A project that is gone since it was trusted is still one a person stops trusting
		if root, err = s.folder(path); err != nil && (trusted || root == "" || !s.Trusted(root)) {
			return Trust{Root: root}, err
		}
	} else if root = s.projectRoot(); root == "" {
		return Trust{}, errors.New("This folder is not inside a git repository. Run the command inside a project, or give the folder of the project.")
	}
	entries := s.TrustedDirs()
	kept := slices.DeleteFunc(slices.Clone(entries), func(entry string) bool { return s.path(entry) == root })
	out := Trust{Root: root}
	switch {
	case !trusted && len(kept) < len(entries):
		out.Changed = true
		entries = kept
	case trusted && len(kept) == len(entries):
		out.Changed = true
		entries = append(entries, root)
	}
	for _, dir := range s.projectDirs(root) {
		out.Skills += len(skillFiles(dir))
	}
	if !out.Changed {
		return out, nil
	}
	return out, s.KeepTrusted(entries)
}

// Whether a path, with slashes, has a folder in it no scan of skills enters, such as .git or node_modules
func Excluded(p string) bool {
	return slices.ContainsFunc(strings.Split(p, "/"), func(part string) bool { return excluded[part] })
}

// Project folders where a repository keeps its own skills, as the user reads them
func ProjectSkillDirs() []string {
	return slices.Clone(projectSkillDirs)
}
