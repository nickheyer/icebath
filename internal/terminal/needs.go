package terminal

import (
	"io/fs"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/skills"
)

// Generated media goes to remote sandboxes while it is this recent
const generatedRecent = 24 * time.Hour

// Folders a sandbox's copy of a skill folder leaves out: version control, environments, caches and icebath's own bookkeeping
var uncopied = map[string]bool{
	".git": true, ".github": true, ".hub": true, ".archive": true, ".curator_backups": true, ".locks": true, ".icebath-dist": true,
	".venv": true, "venv": true, "node_modules": true, "site-packages": true, "__pycache__": true, ".tox": true, ".nox": true,
	".pytest_cache": true, ".mypy_cache": true, ".ruff_cache": true, ".restore-backups": true,
}

// Sessions the environment serves: those it was held for, and the one it was made for
func (s *Spec) sessions() []string {
	sessions := s.Terminal.Sessions(s.Key)
	if s.Session != "" && !slices.Contains(sessions, s.Session) {
		sessions = append(sessions, s.Session)
	}
	return sessions
}

// What the skills of the sessions the environment serves need of it: the skill folders of the profile and of the project the session
// works in, and the credential files and the values of the variables the skills those sessions loaded need, credentials icebath itself
// manages refused
func (s *Spec) Skills(skillset *skills.Registry) (Skills, map[string]string) {
	scope := skillset.Scope(s.Config, s.Folder)
	needs := Skills{Own: scope.Dir(), External: scope.External(), Project: scope.ProjectDirs()}
	for _, session := range s.sessions() {
		_, files := skillset.Needs(s.Config, session)
		needs.Files = append(needs.Files, files...)
	}
	return needs, s.variables(skillset)
}

// Values of the variables the skills the environment's sessions loaded need, credentials icebath itself manages refused
func (s *Spec) variables(skillset *skills.Registry) map[string]string {
	values := map[string]string{}
	for _, session := range s.sessions() {
		variables, _ := skillset.Needs(s.Config, session)
		maps.Copy(values, variables)
	}
	return s.Terminal.skillValues(s.Config, values)
}

// Environment of a command the environment runs on this machine: the host's child environment from the profile's own with the toolchain
// of the client of the session it was made for in place of its own, the variables the skills of its sessions need passed through
func (s *Spec) ChildEnv(skillset *skills.Registry) map[string]string {
	return s.Terminal.childEnv(s.Config, s.Terminal.Client(s.Config, s.Session).Toolchain(s.Config.Environ()), nil, s.variables(skillset))
}

// Every file a remote backend keeps a copy of in its sandbox under base, its profile folder there: the credential files Handed names,
// which go up but never come back, then the files of the folders it names one by one, a skill folder's without the folders a copy leaves
// out, generated media only while recent, links inside them never
func SyncFiles(cfg *config.Config, base string, needs Skills) []SyncFile {
	base = strings.TrimRight(base, "/")
	files, folders := Handed(cfg, base, needs)
	var out []SyncFile
	for _, f := range files {
		out = append(out, SyncFile{Host: f.Host, Remote: f.Sandbox, UploadOnly: true})
	}
	cutoff := time.Now().Add(-generatedRecent)
	for _, folder := range folders {
		cache := strings.TrimPrefix(folder.Sandbox, base+"/")
		cached := slices.Contains(CacheDirs, cache)
		root := folder.Host
		if real, err := filepath.EvalSymlinks(root); err == nil {
			root = real
		}
		filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				if !cached && p != root && uncopied[d.Name()] {
					return filepath.SkipDir
				}
				return nil
			}
			info, err := d.Info()
			if err != nil || !info.Mode().IsRegular() || cache == "cache/generated" && info.ModTime().Before(cutoff) {
				return nil
			}
			if rel, err := filepath.Rel(root, p); err == nil {
				out = append(out, SyncFile{Host: p, Remote: folder.Sandbox + "/" + filepath.ToSlash(rel)})
			}
			return nil
		})
	}
	return out
}

// Environment that keeps copies of the profile's files in its sandbox. One that does says so by having this too
type Syncer interface {
	// Pushes the host files that changed into the sandbox now, however recently it last did
	SyncFiles()
}

// Brings the copies every kept environment of the profile holds up to date, before a path into them is handed out
func (t *Terminal) push(cfg *config.Config) {
	t.mu.Lock()
	var syncers []Syncer
	for _, k := range t.kept {
		if syncer, ok := k.env.(Syncer); ok && k.config.Profile() == cfg.Profile() {
			syncers = append(syncers, syncer)
		}
	}
	t.mu.Unlock()
	for _, syncer := range syncers {
		syncer.SyncFiles()
	}
}
