package profile

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/provider"
	"github.com/nickheyer/icebath/pkg/fsx"
)

// Folders every new profile has from the start
var layout = []string{"memories", "sessions", "skills", "skins", "logs", "plans", "workspace", "cron", "home"}

// Files a clone of a profile's settings copies when the source has them
var cloneFiles = []string{"config.yaml", ".env", "SOUL.md", "memories/MEMORY.md", "memories/USER.md"}

// Entries at the top of a profile a full clone never copies: its history and what it keeps of its runs
var history = map[string]bool{"state.db": true, "state.db-wal": true, "state.db-shm": true, "sessions": true, "backups": true, "state-snapshots": true,
	"checkpoints": true, "cron": true, "webapp": true}

// Files a full clone drops so the copy never attaches to what the source runs
var runtimeFiles = []string{"gateway.pid", "gateway_state.json", "processes.json", "bot-desktop/launcher.pid", "bot-desktop/env", "bot-desktop/rfb.sock",
	"bot-desktop/lease.json", "bot-desktop/lease.lock", "bot-desktop/browser-profile/DevToolsActivePort", "bot-desktop/browser-profile/SingletonLock",
	"bot-desktop/browser-profile/SingletonCookie", "bot-desktop/browser-profile/SingletonSocket"}

// File in a profile's folder that keeps bundled skills out of it
const noSkillsMarker = ".no-bundled-skills"

// What a new profile's .env starts as
const placeholderEnv = "# Per-profile secrets for this icebath profile.\n# API keys and tokens set here override the shell environment.\n" +
	"# Behavioral settings belong in config.yaml, not here.\n"

// Everything a create can be asked for, the zero value a fresh profile taking the keys, logins and model of the profile it is made from
type Request struct {
	Name string
	// Copies the settings, keys and skills of a profile: the one named by From, else the one in use
	Clone bool
	From  string
	// Copies every file of the source but its history, not its settings alone
	All bool
	// Copies the messaging channels of the source too
	Channels bool
	// Carries over where agent imports sync from
	SyncImports bool
	// Seeds no bundled skills
	NoSkills    bool
	Description string
	Soul        string
	// Takes no keys, logins or model from the profile it is made from
	NoCredentials bool
	// Profile the request came from, the one in use when empty
	Launch string
	// Model to pin, with its provider
	Model, Provider string
}

// What a create did
type Made struct {
	Name string
	Path string
	// Profile it was cloned from, or the one it took keys from
	Source string
	// Profile the request came from
	Launch string
	// What features put into it, and what they carried over into it from the profile it came from
	Seeded, Notes []string
	// Steps that failed, the profile made all the same
	Warnings []string
	// Keys of the launch profile's .env were copied into it
	Keys bool
}

// Folder of the profile's files to copy from: the one named, else the one the request came from
func (p *Profiles) source(r Request) (string, string, error) {
	label := r.Launch
	if r.From != "" {
		canon, err := Canon(r.From)
		if err != nil {
			return "", "", err
		}
		label = canon
	}
	dir := p.Dir(label)
	if !fsx.IsDir(dir) {
		return "", "", &NotFoundError{fmt.Sprintf("Source profile '%s' does not exist at %s", label, dir)}
	}
	return label, dir, nil
}

// Refuses what no create takes
func refuseRequest(r Request) error {
	cloning := r.Clone || r.From != "" || r.All
	switch {
	case cloning && r.NoCredentials:
		return &ValueError{"--no-credentials cannot be combined with --clone / --clone-from / --clone-all (a clone copies the source's keys); " +
			"create without cloning instead."}
	case r.NoSkills && cloning:
		return &ValueError{"--no-skills is mutually exclusive with --clone / --clone-from / --clone-all (cloning explicitly copies skills from the " +
			"source profile)."}
	case r.SyncImports && !cloning:
		return &ValueError{"--sync-imports requires --clone or --clone-from (there is no import manifest to carry over without a source profile)."}
	case r.Channels && !cloning:
		return &ValueError{"--clone-channels only applies to a clone (--clone, --clone-from or --clone-all)."}
	}
	return nil
}

// Lock of the creates of one name, held while its staging folder is in use
func (p *Profiles) creating(name string) func() {
	p.mu.Lock()
	lock := p.making[name]
	if lock == nil {
		lock = &sync.Mutex{}
		p.making[name] = lock
	}
	p.mu.Unlock()
	lock.Lock()
	return lock.Unlock
}

// Makes a profile in a hidden staging folder published with one rename, then has every feature do its part for it; only the make itself
// fails it, what a feature could not do coming back as a warning
func (p *Profiles) Create(r Request) (Made, error) {
	if err := refuseRequest(r); err != nil {
		return Made{}, err
	}
	canon, err := Canon(r.Name)
	if err != nil {
		return Made{}, err
	}
	if canon == Default {
		return Made{}, &ValueError{fmt.Sprintf("Cannot create a profile named 'default' — it is the built-in profile (%s).", p.Dir(Default))}
	}
	defer p.creating(canon)()
	dir := p.Dir(canon)
	if fsx.Lexists(dir) {
		return Made{}, taken(canon)
	}
	if r.Launch == "" {
		r.Launch = p.Active()
	}
	cloning := r.Clone || r.From != "" || r.All
	label, sourceDir := r.Launch, ""
	if cloning {
		if err := p.fileTooling("Profile clone"); err != nil {
			return Made{}, err
		}
		if label, sourceDir, err = p.source(r); err != nil {
			return Made{}, err
		}
	}
	staging := filepath.Join(p.root(), fmt.Sprintf(".%s.staging-%d", canon, os.Getpid()))
	if err := os.MkdirAll(p.root(), 0o700); err != nil {
		return Made{}, err
	}
	os.RemoveAll(staging)
	published := false
	defer func() {
		if !published {
			os.RemoveAll(staging)
		}
	}()
	if r.All {
		err = cloneAll(sourceDir, staging)
	} else {
		err = bootstrap(sourceDir, staging, r.SyncImports)
	}
	if err != nil {
		return Made{}, err
	}
	if err := finish(staging, r); err != nil {
		return Made{}, err
	}
	if err := os.Rename(staging, dir); err != nil {
		return Made{}, err
	}
	published = true
	made := Made{Name: canon, Path: dir, Source: label, Launch: r.Launch}
	making := Making{Profile: canon, All: r.All, Launch: r.Launch, Bare: r.NoCredentials, Channels: r.Channels, Model: r.Model, Provider: r.Provider}
	if cloning {
		making.Source = label
	}
	seeded, err := p.Seed(making)
	made.Seeded = seeded
	made.Warnings = append(made.Warnings, warnings(err)...)
	if !r.NoCredentials {
		copied, err := mirrorKeys(p.Dir(r.Launch), dir)
		if err != nil {
			made.Warnings = append(made.Warnings, ".env copy: "+failure(err))
		}
		made.Keys = copied
	}
	notes, err := p.Made(making)
	made.Notes = notes
	made.Warnings = append(made.Warnings, warnings(err)...)
	return made, nil
}

// Warning lines of what features could not do for a profile just made
func warnings(err error) []string {
	var out []string
	for _, one := range failures(err) {
		out = append(out, failure(one))
	}
	return out
}

// Copies every file of the source but its history and the files of its runs
func cloneAll(source, dest string) error {
	if err := copyTree(source, dest, func(rel string, entry fs.DirEntry) bool {
		return !strings.Contains(rel, "/") && history[rel] || unexportable(entry)
	}); err != nil {
		return err
	}
	// Files a profile writes in place are its own, so a linked one in the source is never written through from the copy
	for _, name := range []string{".env", "config.yaml", "auth.json", "SOUL.md"} {
		at := filepath.Join(dest, name)
		if info, err := os.Lstat(at); err != nil || info.Mode()&fs.ModeSymlink == 0 {
			continue
		}
		target := resolveLoose(at)
		if err := os.Remove(at); err != nil {
			return err
		}
		if fsx.IsFile(target) {
			if err := fsx.Copy(target, at); err != nil {
				return err
			}
		}
	}
	for _, dir := range layout {
		if err := os.MkdirAll(filepath.Join(dest, dir), 0o755); err != nil {
			return err
		}
	}
	for _, stale := range runtimeFiles {
		if err := os.Remove(filepath.Join(dest, filepath.FromSlash(stale))); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

// Fresh layout, with the settings, keys, skills, memory files and plugins of the source when there is one
func bootstrap(source, dest string, syncImports bool) error {
	for _, dir := range layout {
		if err := os.MkdirAll(filepath.Join(dest, dir), 0o755); err != nil {
			return err
		}
	}
	if source == "" {
		return nil
	}
	files := cloneFiles
	if syncImports {
		files = append(files, "import-sync.json")
	}
	for _, rel := range files {
		if err := copyFile(source, dest, rel); err != nil {
			return err
		}
	}
	for _, tree := range []string{"skills", "plugins"} {
		if fsx.IsDir(filepath.Join(source, tree)) {
			if err := copyTree(filepath.Join(source, tree), filepath.Join(dest, tree), func(rel string, entry fs.DirEntry) bool {
				return unexportable(entry) || tree == "plugins" && !strings.Contains(rel, "/") && entry.IsDir() &&
					(strings.HasPrefix(rel, ".install-") || strings.HasPrefix(rel, ".update-"))
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

// Copies one file of the source's folder when it has it, a .env kept to its owner
func copyFile(source, dest, rel string) error {
	from := filepath.Join(source, filepath.FromSlash(rel))
	if !fsx.IsFile(from) {
		return nil
	}
	to := filepath.Join(dest, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return err
	}
	if err := fsx.Copy(from, to); err != nil {
		return err
	}
	if rel == ".env" {
		return os.Chmod(to, 0o600)
	}
	return nil
}

// Writes a file unless something is already there
func seedFile(path, text string, mode os.FileMode) error {
	if fsx.Lexists(path) {
		return nil
	}
	return os.WriteFile(path, []byte(text), mode)
}

// Files a new profile has from the start: its .env, its SOUL.md, the opt out of bundled skills and its description
func finish(dir string, r Request) error {
	if r.Soul != "" {
		if err := os.WriteFile(filepath.Join(dir, "SOUL.md"), []byte(r.Soul), 0o644); err != nil {
			return err
		}
	}
	if err := seedFile(filepath.Join(dir, ".env"), placeholderEnv, 0o600); err != nil {
		return err
	}
	if fsx.IsFile(filepath.Join(dir, "config.yaml")) {
		os.Chmod(filepath.Join(dir, "config.yaml"), 0o600)
	}
	if err := seedFile(filepath.Join(dir, "SOUL.md"), provider.Identity+"\n", 0o644); err != nil {
		return err
	}
	if r.NoSkills {
		if err := seedFile(filepath.Join(dir, noSkillsMarker), "This profile opted out of bundled-skill seeding (`icebath profile create --no-skills`).\n"+
			"Delete this file to re-enable sync on the next `icebath update`.\n", 0o644); err != nil {
			return err
		}
	}
	if description := strings.TrimSpace(r.Description); description != "" {
		auto := false
		return WriteMeta(dir, MetaUpdate{Description: &description, DescriptionAuto: &auto})
	}
	return nil
}

// Copies the launch profile's .env into a new profile whose .env is still the stub it starts with, never over keys of its own
func mirrorKeys(launch, dir string) (bool, error) {
	from, to := filepath.Join(launch, ".env"), filepath.Join(dir, ".env")
	held, err := config.ReadDotenv(from)
	if err != nil || len(held) == 0 {
		return false, err
	}
	own, err := config.ReadDotenv(to)
	if err != nil || len(own) > 0 {
		return false, err
	}
	if err := fsx.Copy(from, to); err != nil {
		return false, err
	}
	return true, os.Chmod(to, 0o600)
}
