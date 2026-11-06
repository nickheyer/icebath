package skills

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Adds or removes a project root in skills.trusted_project_dirs, the cwd checkout when no path is given
func SkillsTrustCommand(ctx context.Context, h, action, rawPath string, out io.Writer) error {
	cfg, err := config.Load(h, config.NewSecrets(h).Lookup())
	if err != nil {
		return err
	}
	scope := NewScope(h, cfg)
	var root string
	if rawPath != "" {
		root = resolvePath(pypath.Expanduser(rawPath))
		if st, err := os.Stat(root); err != nil || !st.IsDir() {
			fmt.Fprintf(out, "Not a directory: %s\n", root)
			return nil
		}
	} else if root = scope.FindProjectRoot(); root == "" {
		fmt.Fprintln(out, "Not inside a git checkout. Run from a project directory or pass the project root path explicitly.")
		return nil
	}
	if _, present := cfg["skills"]; !present {
		cfg["skills"] = map[string]any{}
	}
	skillsCfg, ok := cfg["skills"].(map[string]any)
	if !ok {
		return fmt.Errorf("'%s' object has no attribute 'get'", pyrepr.TypeName(cfg["skills"]))
	}
	var trusted []string
	switch raw := skillsCfg["trusted_project_dirs"].(type) {
	case []any:
		for _, t := range raw {
			trusted = append(trusted, pyrepr.Str(t))
		}
	default:
		if pyrepr.Truthy(raw) {
			trusted = append(trusted, pyrepr.Str(raw))
		}
	}
	same := func(t string) bool { return resolvePath(pypath.Expanduser(t)) == root }
	toList := func(items []string) []any {
		list := make([]any, 0, len(items))
		for _, t := range items {
			list = append(list, t)
		}
		return list
	}
	if action == "untrust" {
		var kept []string
		for _, t := range trusted {
			if !same(t) {
				kept = append(kept, t)
			}
		}
		if len(kept) == len(trusted) {
			fmt.Fprintf(out, "%s was not trusted.\n", root)
			return nil
		}
		skillsCfg["trusted_project_dirs"] = toList(kept)
		if err := config.SaveConfig(ctx, h, cfg, config.SaveOptions{}); err != nil {
			return err
		}
		fmt.Fprintf(out, "Untrusted: %s\nProject skills from this repo will no longer load.\n", root)
		return nil
	}
	already := false
	for _, t := range trusted {
		if same(t) {
			already = true
			break
		}
	}
	if already {
		fmt.Fprintf(out, "Already trusted: %s\n", root)
	} else {
		skillsCfg["trusted_project_dirs"] = toList(append(trusted, root))
		if err := config.SaveConfig(ctx, h, cfg, config.SaveOptions{}); err != nil {
			return err
		}
		fmt.Fprintf(out, "Trusted: %s\n", root)
	}
	count := 0
	for _, d := range scope.candidateProjectSkillsDirs(root) {
		count += len(iterSkillIndexFiles(d, "SKILL.md"))
	}
	if count > 0 {
		fmt.Fprintf(out, "%d project skill(s) will load in sessions started inside this repo (they take precedence over same-named profile skills).\n", count)
	} else {
		fmt.Fprintf(out, "No project skills found yet — add them under %s.\n", strings.Join(projectSkillsSubdirs, " or "))
	}
	return nil
}
