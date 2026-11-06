package builtin

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/skills"
)

const hallmarkSkill = "hallmark/skills/hallmark"

// Node scripts hallmark ships beside its skill
var hallmarkScripts = []string{"capture", "imagegen", "install", "install-hook", "lint-hook", "picture", "refcheck", "sloplint", "sloplint-render", "theme"}

// Compiled-in hallmark design skill plugin
type hallmarkPlugin struct{}

// Manifest of the hallmark plugin from its Claude plugin manifest with the lint hook declared
func (hallmarkPlugin) Manifest() plugins.Manifest {
	m, err := manifestFromJSON("hallmark/.claude-plugin/plugin.json")
	if err != nil {
		panic(err)
	}
	m.ProvidesHooks = []string{"transform_tool_result"}
	return m
}

// Registers the hallmark skill from the materialized payload and its edit time lint hook
func (hallmarkPlugin) Register(ctx *plugins.Context) error {
	t, err := Materialize()
	if err != nil {
		return err
	}
	content, err := payloadFile(hallmarkSkill + "/SKILL.md")
	if err != nil {
		return err
	}
	fm, _ := skills.ParseFrontmatter(string(content))
	name, _ := fm["name"].(string)
	desc, _ := fm["description"].(string)
	if _, err := ctx.RegisterSkill(name, t.Payload(hallmarkSkill+"/SKILL.md"), desc, fm); err != nil {
		return err
	}
	installer := newHallmarkInstaller(ctx)
	hook := &hallmarkHook{sessionCwd: pluginSessionCwd(ctx), command: HallmarkCommand, installed: installer.hookInstalled}
	ctx.RegisterHook("transform_tool_result", hook.transformToolResult, false)
	ctx.RegisterCommand("hallmark-install", installer.install, "Enable Hallmark for icebath after checking every theme and reference", "[--dry-run] [--force] [--remove]", "")
	ctx.RegisterCommand("hallmark-install-hook", installer.installHook, "Install or remove the Hallmark edit-time lint hook", "[--global] [--print] [--remove]", "")
	return nil
}

// Environment of the current process as a map
func environMap() map[string]string {
	env := map[string]string{}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}
	return env
}

// Command running a hallmark node script from the materialized tree on the pm managed node runtime
func HallmarkCommand(ctx context.Context, script string, args ...string) (*exec.Cmd, error) {
	known := false
	for _, s := range hallmarkScripts {
		known = known || s == script
	}
	if !known {
		return nil, fmt.Errorf("builtin: unknown hallmark script %s", script)
	}
	t, err := Materialize()
	if err != nil {
		return nil, err
	}
	return nodeCommand(ctx, t.Payload(hallmarkSkill+"/scripts/"+script+".mjs"), args...)
}

// Command running a node script through pm with the managed node path composed over the process env
func nodeCommand(ctx context.Context, scriptPath string, args ...string) (*exec.Cmd, error) {
	runner, err := pm.Ensure(ctx, "node", pm.EnsureOptions{BaseEnv: pm.WithIcebathNodePath(environMap())})
	if err != nil {
		return nil, err
	}
	return runner.Command(ctx, "node", append([]string{scriptPath}, args...)...), nil
}
