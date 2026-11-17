package builtin

import (
	"github.com/nickheyer/icebath/pkg/commands"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/skills"
)

// Compiled-in impeccable design plugin
type impeccablePlugin struct{}

// Manifest of the impeccable plugin from its Claude build manifest with the design hooks declared
func (impeccablePlugin) Manifest() plugins.Manifest {
	m, err := manifestFromJSON("impeccable/plugin/.claude-plugin/plugin.json")
	if err != nil {
		panic(err)
	}
	m.ProvidesHooks = []string{"transform_tool_result", "pre_verify"}
	return m
}

// Registers the hermes rendered skill, the impeccable slash command and the design hooks
func (impeccablePlugin) Register(ctx *plugins.Context) error {
	t, err := Materialize()
	if err != nil {
		return err
	}
	skillMD := t.Host("impeccable/skills/impeccable/SKILL.md")
	rendered, err := renderImpeccableSkill(t.Host("impeccable/skills/impeccable/scripts"))
	if err != nil {
		return err
	}
	fm, _ := skills.ParseFrontmatter(string(rendered["SKILL.md"].Data))
	name, _ := fm["name"].(string)
	desc, _ := fm["description"].(string)
	if _, err := ctx.RegisterSkill(name, skillMD, desc, fm); err != nil {
		return err
	}
	def, err := impeccableCommandDef(ctx.PluginID())
	if err != nil {
		return err
	}
	commands.Register(def)
	hook := newDesignHook(pluginSessionCwd(ctx))
	ctx.RegisterHook("transform_tool_result", hook.transformToolResult, false)
	ctx.RegisterHook("pre_verify", hook.preVerify, false)
	return nil
}

// The /impeccable command with the 24 metadata commands as subcommands and the rendered argument hint
func impeccableCommandDef(plugin string) (commands.Def, error) {
	names, err := impeccableCommandNames()
	if err != nil {
		return commands.Def{}, err
	}
	hint, err := impeccableCommandHint()
	if err != nil {
		return commands.Def{}, err
	}
	src, err := payloadFile(impeccableSkillRoot + "/SKILL.src.md")
	if err != nil {
		return commands.Def{}, err
	}
	fm, _ := parseImpeccableFrontmatter(string(src))
	argsHint := fmString(fm, "argument-hint")
	argsHint = replaceFirst(argsHint, "{{command_hint}}", hint)
	description := fmString(fm, "description")
	return commands.Def{
		Name: "impeccable", Description: description, Category: "Plugins", ArgsHint: argsHint,
		Subcommands: names, Plugin: plugin,
	}, nil
}

func replaceFirst(s, old, repl string) string {
	for i := 0; i+len(old) <= len(s); i++ {
		if s[i:i+len(old)] == old {
			return s[:i] + repl + s[i+len(old):]
		}
	}
	return s
}
