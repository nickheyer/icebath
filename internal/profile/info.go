package profile

import (
	"os"
	"path/filepath"

	"github.com/nickheyer/icebath/pkg/fsx"
)

// One profile as its listing and details show it
type Info struct {
	Name string
	Path string
	// The profile that is always there
	Default bool
	// The profile in use, and the one icebath starts with
	Active   bool
	Sticky   bool
	Model    string
	Provider string
	Skills   int
	HasEnv   bool
	HasSoul  bool
	// Distribution it was installed from, empty for one made here
	Distribution        string
	DistributionVersion string
	DistributionSource  string
	// It gets every skill that comes with icebath, and keeps pulling what another profile imports from other agents
	Bundled, SyncImports bool
	Meta
}

// Path with the user's home folder written as ~
func Tilde(path string) string {
	if home, err := os.UserHomeDir(); err == nil {
		if rel, err := filepath.Rel(home, path); err == nil && filepath.IsLocal(rel) {
			return "~/" + filepath.ToSlash(rel)
		}
	}
	return filepath.Clean(path)
}

// What a profile is: where, what it runs and what tells about it; an error for one that is not there
func (p *Profiles) Info(name string) (Info, error) {
	canon, err := Canon(name)
	if err != nil {
		return Info{}, err
	}
	if !p.Exists(canon) {
		return Info{}, unknown(canon)
	}
	dir := p.Dir(canon)
	summary := p.Summarize(p.config.In(canon))
	info := Info{Name: canon, Path: dir, Default: canon == Default, Active: canon == p.Active(), Sticky: canon == p.sticky(), Model: summary.Model,
		Provider: summary.Provider, Skills: len(summary.Skills), HasEnv: fsx.Exists(filepath.Join(dir, ".env")),
		HasSoul: fsx.Exists(filepath.Join(dir, "SOUL.md")), Bundled: bundled(dir), SyncImports: syncing(dir), Meta: ReadMeta(dir)}
	if manifest, err := ReadManifest(dir); err == nil && manifest != nil {
		info.Distribution, info.DistributionVersion, info.DistributionSource = manifest.Name, manifest.Version, manifest.Source
	}
	return info, nil
}

// Every profile, the default one first
func (p *Profiles) Infos() ([]Info, error) {
	names, err := p.List()
	if err != nil {
		return nil, err
	}
	out := make([]Info, 0, len(names))
	for _, name := range names {
		info, err := p.Info(name)
		if err != nil {
			return nil, err
		}
		out = append(out, info)
	}
	return out, nil
}
