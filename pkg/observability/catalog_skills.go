package observability

import (
	"context"
	"io/fs"
	"os"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// BundledSkillTreesHook returns the embedded bundled and optional skill trees, set by pkg/skills
var BundledSkillTreesHook func() (bundled, optional fs.FS)

func init() { catalogSources["bundled_skill_names"] = bundledSkillNames }

// Lowercased names of every directory holding a SKILL.md anywhere under a tree
func skillDirNames(tree fs.FS) []string {
	var names []string
	_ = fs.WalkDir(tree, ".", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && d.Name() == "SKILL.md" {
			parent := "."
			if i := lastSlash(p); i >= 0 {
				parent = p[:i]
			}
			names = append(names, pystr.Lower(baseName(parent)))
		}
		return nil
	})
	return names
}

func lastSlash(p string) int {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			return i
		}
	}
	return -1
}

func baseName(p string) string { return p[lastSlash(p)+1:] }

// Bundled and optional skill names, a packaged directory override replacing the embedded tree
func bundledSkillNames() ([]string, error) {
	if BundledSkillTreesHook == nil {
		return nil, errHookUnavailable
	}
	bundled, optional := BundledSkillTreesHook()
	ctx := context.Background()
	if pystr.Strip(os.Getenv("ICEBATH_BUNDLED_SKILLS")) != "" {
		bundled = os.DirFS(home.BundledSkillsDir(ctx, ""))
	}
	if pystr.Strip(os.Getenv("ICEBATH_OPTIONAL_SKILLS")) != "" {
		optional = os.DirFS(home.OptionalSkillsDir(ctx, ""))
	}
	return append(skillDirNames(bundled), skillDirNames(optional)...), nil
}
