package skills

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/fsx"
)

// What the hub installed into a skills folder, kept in its .hub/lock.json
type Lock struct {
	Version int `json:"version"`
	// By the name the skill was installed as
	Installed map[string]*Installed `json:"installed"`
}

// One skill the hub installed
type Installed struct {
	// Registry it came from, such as official
	Source string `json:"source"`
	// What it was installed by, such as official/devops/docker
	Identifier string `json:"identifier"`
	// builtin, trusted or community
	Trust string `json:"trust_level"`
	// What the scan before install found
	Verdict string `json:"scan_verdict"`
	// ContentHash of the folder as installed
	ContentHash string `json:"content_hash"`
	// Its folder relative to the skills folder, with slashes
	InstallPath string         `json:"install_path"`
	Files       []string       `json:"files"`
	Metadata    map[string]any `json:"metadata"`
	InstalledAt string         `json:"installed_at"`
	UpdatedAt   string         `json:"updated_at"`

	// What the scan before install attested: the bundle's hash, the scanner's version, the verdict and its findings
	ScanProvenance json.RawMessage `json:"scan_provenance,omitempty"`
}

func lockPath(skillsDir string) string {
	return filepath.Join(skillsDir, ".hub", "lock.json")
}

// The hub's lock of a skills folder, empty when there is none
func ReadLock(skillsDir string) (*Lock, error) {
	lock := &Lock{Version: 1, Installed: map[string]*Installed{}}
	data, err := os.ReadFile(lockPath(skillsDir))
	if errors.Is(err, fs.ErrNotExist) {
		return lock, nil
	}
	if err != nil {
		return nil, err
	}
	data = []byte(strings.TrimPrefix(string(data), "\uFEFF"))
	if err := json.Unmarshal(data, lock); err != nil {
		return nil, fmt.Errorf("The skills hub lock %s can't be read: %w", lockPath(skillsDir), err)
	}
	if lock.Installed == nil {
		lock.Installed = map[string]*Installed{}
	}
	return lock, nil
}

// Writes the hub's lock of a skills folder at once
func WriteLock(skillsDir string, lock *Lock) error {
	data, err := json.MarshalIndent(lock, "", "  ")
	if err != nil {
		return err
	}
	return fsx.Replace(lockPath(skillsDir), append(data, '\n'), 0o644)
}

// Names the hub installed into the profile's folder: each installed name and the name the skill at its install path declares
func (s *Scope) hubNames() map[string]bool {
	out := map[string]bool{}
	lock, err := ReadLock(s.Dir())
	if err != nil {
		return out
	}
	root := resolve(s.Dir())
	for name, entry := range lock.Installed {
		out[name] = true
		if entry == nil || strings.TrimSpace(entry.InstallPath) == "" {
			continue
		}
		dir := entry.InstallPath
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(s.Dir(), filepath.FromSlash(dir))
		}
		if dir = resolve(dir); within(dir, root) && fsx.Exists(filepath.Join(dir, "SKILL.md")) {
			out[readName(filepath.Join(dir, "SKILL.md"), filepath.Base(dir))] = true
		}
	}
	return out
}

// Whether the hub installed the skill
func (s *Scope) HubInstalled(name string) bool {
	return s.hubNames()[name]
}

// Short digest of a skill folder's files and their paths, as the hub records an install
func ContentHash(dir string) string {
	h := sha256.New()
	if !fsx.IsDir(dir) {
		data, err := os.ReadFile(dir)
		if err != nil {
			return "sha256:"
		}
		h.Write(data)
	} else {
		for _, rel := range files(dir) {
			data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
			if err != nil {
				return "sha256:"
			}
			h.Write([]byte(rel))
			h.Write([]byte{0})
			h.Write(data)
		}
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))[:16]
}

// Every file under a folder by its path relative to it, with slashes, sorted
func files(dir string) []string {
	var out []string
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if rel, err := filepath.Rel(dir, p); err == nil {
				out = append(out, filepath.ToSlash(rel))
			}
		}
		return nil
	})
	slices.Sort(out)
	return out
}

// Time as the hub's records write it
func now() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000000+00:00")
}
