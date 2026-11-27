package profile

import (
	"context"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/profile"
	"github.com/nickheyer/icebath/internal/skills"
)

// Longest seeding the bundled skills of a new profile may take
const seedTimeout = 60 * time.Second

// Skills as profiles come and go: the bundled ones seeded into a new profile and into the profile in use, and those a profile has told for
// its listing
type seeder struct {
	config   *config.Config
	registry *skills.Registry
}

func Register(registry *profile.Registry, cfg *config.Config, skillset *skills.Registry) {
	registry.Register(seeder{config: cfg, registry: skillset})
}

// Skills live in the profile's folder, deleted with it
func (seeder) Delete(string) error {
	return nil
}

func (s seeder) Use(name string) error {
	return s.registry.Scope(s.config.In(name), "").Seed(context.Background())
}

// Seeds the bundled skills of a fresh profile, a clone having copied those of its source; a seeding that failed is said in place, since the
// bundled skills are seeded again the next time the profile is used
func (s seeder) Seed(making profile.Making) ([]string, error) {
	if making.Source != "" {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), seedTimeout)
	defer cancel()
	synced, err := s.registry.Scope(s.config.In(making.Profile), "").Sync(ctx)
	retry := fmt.Sprintf("⚠ Skills could not be seeded. They are seeded again the next time %s is used.", making.Profile)
	switch {
	case ctx.Err() != nil:
		return []string{"⚠ Skill seeding timed out (60s)", retry}, nil
	case err != nil:
		return []string{fmt.Sprintf("⚠ Skill seeding failed: %v", err), retry}, nil
	case synced.OptedOut:
		return []string{"No bundled skills seeded (--no-skills). Delete .no-bundled-skills in the profile to opt back in."}, nil
	}
	return []string{fmt.Sprintf("%d bundled skills synced.", len(synced.Copied))}, nil
}

// Skills the profile's folder holds, by category and name, or name for one at the top
func (s seeder) Summarize(cfg *config.Config) profile.Summary {
	dir := s.registry.Scope(cfg, "").Dir()
	var names []string
	filepath.WalkDir(dir, func(at string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if entry.IsDir() && at != dir && strings.HasPrefix(entry.Name(), ".") {
			return filepath.SkipDir
		}
		if entry.IsDir() || entry.Name() != "SKILL.md" {
			return nil
		}
		rel, err := filepath.Rel(dir, filepath.Dir(at))
		if err != nil || rel == "." {
			return nil
		}
		parts := strings.Split(filepath.ToSlash(rel), "/")
		if len(parts) == 1 {
			names = append(names, parts[0])
		} else {
			names = append(names, parts[0]+"/"+parts[len(parts)-1])
		}
		return nil
	})
	return profile.Summary{Skills: names}
}
