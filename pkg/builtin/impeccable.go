package builtin

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/commands"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/skills"
)

// Surface a slash command hands a built skill turn to, implemented by the CLI, TUI and gateway hosts
type TurnSubmitter interface {
	SubmitUserTurn(ctx context.Context, message string) error
}

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
	def, err := impeccableCommandDef(ctx.PluginID(), func(c context.Context, cmdKey, instruction string) (string, bool, error) {
		return skills.NewScope(home.Dir(c), nil).BuildSkillInvocationMessage(c, cmdKey, instruction, "", "")
	})
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
func impeccableCommandDef(plugin string, build func(ctx context.Context, cmdKey, instruction string) (string, bool, error)) (commands.Def, error) {
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
	cmdKey := "/impeccable:impeccable"
	return commands.Def{
		Name: "impeccable", Description: description, Category: "Plugins", ArgsHint: argsHint,
		Subcommands: names, ArgumentMode: "text", Plugin: plugin,
		Handler: func(ctx context.Context, inv *commands.Invocation) error {
			host, ok := inv.Host.(TurnSubmitter)
			if !ok {
				return errors.New("impeccable: this surface cannot start a skill turn")
			}
			msg, found, err := build(ctx, cmdKey, impeccableRouteAlias(inv.Args))
			if err != nil {
				return err
			}
			if !found {
				return fmt.Errorf("impeccable: skill %s is not available", cmdKey[1:])
			}
			return host.SubmitUserTurn(ctx, msg)
		},
	}, nil
}

// Command aliases the SKILL.md routing rules name, teach running init
var impeccableAliases = map[string]string{"teach": "init"}

// Rewrites a leading aliased command word to the command it aliases, keeping the rest of the request
func impeccableRouteAlias(args string) string {
	trimmed := strings.TrimLeft(args, " \t\n")
	word := trimmed
	if i := strings.IndexAny(trimmed, " \t\n"); i >= 0 {
		word = trimmed[:i]
	}
	target, ok := impeccableAliases[word]
	if !ok {
		return args
	}
	return target + trimmed[len(word):]
}

func replaceFirst(s, old, repl string) string {
	for i := 0; i+len(old) <= len(s); i++ {
		if s[i:i+len(old)] == old {
			return s[:i] + repl + s[i+len(old):]
		}
	}
	return s
}
