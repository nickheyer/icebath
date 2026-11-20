package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/plugins"
)

const (
	hallmarkHookGlobalKey   = "lint_hook"
	hallmarkHookProjectsKey = "lint_hook_projects"
	hallmarkHookAdvice      = "it lints Hallmark .html and .css files, and .jsx, .tsx, .vue, .svelte and .astro files inside a project with a .hallmark folder, on write; it advises and never blocks. Remove with --remove."
	hallmarkInstallUsage    = `/hallmark-install [--dry-run] [--force] [--remove]
  Enables Hallmark for icebath, after checking every theme and reference.
  --dry-run  print what would change and change nothing
  --force    enable even if the checks find a problem
  --remove   disable Hallmark`
)

// Operations the hallmark installer commands drive, swapped by tests
type hallmarkInstaller struct {
	command func(ctx context.Context, script string, args ...string) (*exec.Cmd, error)
	enable  func(ctx context.Context, name string, out io.Writer) error
	disable func(ctx context.Context, name string, out io.Writer) error
	cwd     func(ctx context.Context) string
	get     func(ctx context.Context, key string, def any) (any, error)
	set     func(ctx context.Context, key string, value any) error
}

// Installer bound to the plugin context, the plugins.enabled selection and the managed node scripts
func newHallmarkInstaller(pctx *plugins.Context) *hallmarkInstaller {
	return &hallmarkInstaller{command: HallmarkCommand, cwd: pluginSessionCwd(pctx), get: pctx.GetConfig, set: pctx.SetConfig,
		enable: func(ctx context.Context, name string, out io.Writer) error {
			return plugins.CmdEnable(ctx, name, nil, out)
		},
		disable: plugins.CmdDisable}
}

// Runs a hallmark script and reports whether it exited zero with its stdout
func (h *hallmarkInstaller) gate(ctx context.Context, script string, args ...string) (bool, string, error) {
	cmd, err := h.command(ctx, script, args...)
	if err != nil {
		return false, "", err
	}
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	runErr := cmd.Run()
	return runErr == nil, stdout.String(), nil
}

// Enables Hallmark through plugins.enabled after the refcheck and theme gates install.mjs runs
func (h *hallmarkInstaller) install(ctx context.Context, rawArgs string) (any, error) {
	args := strings.Fields(rawArgs)
	has := func(f string) bool {
		for _, a := range args {
			if a == f {
				return true
			}
		}
		return false
	}
	var unknown []string
	for _, a := range args {
		if a != "--dry-run" && a != "--force" && a != "--remove" && a != "--help" && a != "-h" {
			unknown = append(unknown, a)
		}
	}
	var out strings.Builder
	if has("--help") || has("-h") || len(unknown) > 0 {
		if len(unknown) > 0 {
			fmt.Fprintf(&out, "Unknown option: %s\n\n", strings.Join(unknown, " "))
		}
		return out.String() + hallmarkInstallUsage, nil
	}
	dry := has("--dry-run")
	if has("--remove") {
		out.WriteString("Removing Hallmark from icebath:\n")
		if dry {
			out.WriteString("  would disable the hallmark plugin\n")
			return out.String(), nil
		}
		if err := h.disable(ctx, "hallmark", &out); err != nil {
			return out.String(), err
		}
		return out.String(), nil
	}
	if !dry {
		if ok, _, err := h.gate(ctx, "theme", "index"); err != nil || !ok {
			out.WriteString("  (theme index not rebuilt; run node scripts/theme.mjs index)\n")
		}
	}
	t, err := Materialize()
	if err != nil {
		return nil, err
	}
	themes := payloadUnder(hallmarkSkill + "/references/themes")
	themeFiles := []string{"validate"}
	for _, rel := range themes {
		if strings.HasSuffix(rel, ".md") {
			themeFiles = append(themeFiles, t.Payload(rel))
		}
	}
	clean := true
	for _, g := range []struct {
		label, script string
		args          []string
	}{{"refcheck", "refcheck", []string{"--errors"}}, {"theme validate", "theme", themeFiles}} {
		ok, stdout, err := h.gate(ctx, g.script, g.args...)
		if err != nil {
			return nil, err
		}
		if !ok {
			fmt.Fprintf(&out, "\n%s failed:\n%s\n", g.label, strings.TrimSpace(stdout))
			clean = false
		}
	}
	if !clean && !has("--force") {
		out.WriteString("\nNothing installed. Fix the findings above, or pass --force to install anyway.\n")
		return out.String(), nil
	}
	suffix := ""
	if dry {
		suffix = " (dry run)"
	}
	fmt.Fprintf(&out, "Hallmark installer%s · payload: %s\n\nicebath:\n", suffix, t.Payload(hallmarkSkill))
	if dry {
		fmt.Fprintf(&out, "  would enable hallmark in plugins.enabled of %s\n", filepath.Join(home.Dir(ctx), "config.yaml"))
	} else {
		if err := h.enable(ctx, "hallmark", &out); err != nil {
			return out.String(), err
		}
		fmt.Fprintf(&out, "  installed -> plugins.enabled of %s\n", filepath.Join(home.Dir(ctx), "config.yaml"))
	}
	out.WriteString("\nDone. Invoke as: /hallmark:hallmark (icebath).\n")
	return out.String(), nil
}

// Project roots whose sessions run the lint hook, from the plugin settings
func (h *hallmarkInstaller) hookProjects(ctx context.Context) ([]string, error) {
	raw, err := h.get(ctx, hallmarkHookProjectsKey, nil)
	if err != nil {
		return nil, err
	}
	list, ok := raw.([]any)
	if raw != nil && !ok {
		return nil, fmt.Errorf("refusing to rewrite %s: plugins.entries.hallmark.settings.%s is not a list", filepath.Join(home.Dir(ctx), "config.yaml"), hallmarkHookProjectsKey)
	}
	var out []string
	for _, v := range list {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out, nil
}

// Reports whether the lint hook is installed globally or for a project holding the session cwd
func (h *hallmarkInstaller) hookInstalled(ctx context.Context, cwd string) bool {
	if v, err := h.get(ctx, hallmarkHookGlobalKey, false); err == nil && pyrepr.Truthy(v) {
		return true
	}
	projects, err := h.hookProjects(ctx)
	if err != nil {
		return false
	}
	for _, p := range projects {
		if rel, err := filepath.Rel(p, cwd); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// Installs, prints or removes the edit time lint hook in the plugin settings as install-hook.mjs does in Claude settings
func (h *hallmarkInstaller) installHook(ctx context.Context, rawArgs string) (any, error) {
	args := strings.Fields(rawArgs)
	has := func(f string) bool {
		for _, a := range args {
			if a == f {
				return true
			}
		}
		return false
	}
	configPath := filepath.Join(home.Dir(ctx), "config.yaml")
	global := has("--global")
	project, _ := filepath.Abs(h.cwd(ctx))
	scope := configPath
	if !global {
		scope = configPath + " for " + project
	}
	projects, err := h.hookProjects(ctx)
	if err != nil {
		return nil, err
	}
	without := make([]any, 0, len(projects))
	listed := false
	for _, p := range projects {
		if p == project {
			listed = true
			continue
		}
		without = append(without, p)
	}
	if has("--print") {
		settings := map[string]any{hallmarkHookGlobalKey: true}
		if !global {
			settings = map[string]any{hallmarkHookProjectsKey: append(append([]any{}, without...), project)}
		}
		snippet, _ := json.MarshalIndent(map[string]any{"plugins": map[string]any{"entries": map[string]any{"hallmark": map[string]any{"settings": settings}}}}, "", "  ")
		return fmt.Sprintf("# add to %s :\n%s\n", scope, snippet), nil
	}
	if has("--remove") {
		on, err := h.get(ctx, hallmarkHookGlobalKey, false)
		if err != nil {
			return nil, err
		}
		if (global && !pyrepr.Truthy(on)) || (!global && !listed) {
			return fmt.Sprintf("no Hallmark lint hook in %s; nothing to remove\n", scope), nil
		}
		if global {
			err = h.set(ctx, hallmarkHookGlobalKey, false)
		} else {
			err = h.set(ctx, hallmarkHookProjectsKey, without)
		}
		if err != nil {
			return nil, err
		}
		return fmt.Sprintf("removed the Hallmark lint hook from %s\n", scope), nil
	}
	if global {
		err = h.set(ctx, hallmarkHookGlobalKey, true)
	} else {
		next := append(without, project)
		sort.Slice(next, func(i, j int) bool { return next[i].(string) < next[j].(string) })
		err = h.set(ctx, hallmarkHookProjectsKey, next)
	}
	if err != nil {
		return nil, err
	}
	return fmt.Sprintf("installed the Hallmark edit-time lint hook into %s\n%s\n", scope, hallmarkHookAdvice), nil
}
