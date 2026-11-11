package gatewayrun

import (
	"context"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/skills"
)

// Platform value of a source, empty when the source names none
func sourcePlatformValue(source *gateway.SessionSource) string {
	if source == nil {
		return ""
	}
	return string(source.Platform)
}

// Rewrites a bundle command to the bundle invocation message, true when the command named a bundle
func (r *GatewayRunner) hmBundleSlashRewrite(ctx context.Context, event *gateway.MessageEvent, source *gateway.SessionSource, quickKey, command string) bool {
	scope := skills.NewScope(home.Dir(ctx), nil)
	bundleKey, ok, err := scope.ResolveBundleCommandKey(command)
	if err != nil {
		runLog().Warn(fmt.Sprintf("Bundle dispatch failed: %s", err))
		return false
	}
	if !ok {
		return false
	}
	text, _, missing, built, err := scope.BuildBundleInvocationMessage(ctx, bundleKey, pystr.Strip(event.GetCommandArgs()), quickKey, sourcePlatformValue(source))
	if err != nil {
		runLog().Warn(fmt.Sprintf("Bundle dispatch failed: %s", err))
		return false
	}
	if !built {
		return false
	}
	event.Text = text
	if len(missing) > 0 {
		runLog().Info(fmt.Sprintf("Bundle %s skipped missing skills: %s", bundleKey, strings.Join(missing, ", ")))
	}
	return true
}

// Reply for a slash command that is not builtin, plugin or skill, nil when the command is known
func hmUnknownSlashReply(ctx context.Context, command string, source *gateway.SessionSource) *string {
	if GatewayKnownCommand(strings.ReplaceAll(command, "_", "-")) {
		return nil
	}
	if hint := checkUnavailableSkill(ctx, command); hint != "" {
		return &hint
	}
	platform := sourcePlatformValue(source)
	if platform == "" {
		platform = "?"
	}
	runLog().Warn(fmt.Sprintf("Unrecognized slash command /%s from %s — replying with unknown-command notice", command, platform))
	reply := i18n.T(ctx, "gateway.unknown_command", "", map[string]any{"command": command})
	return &reply
}

// Rewrites a bundle or skill command into the skill prompt on the event text, a reply when disabled, unknown or failed
func (r *GatewayRunner) hmSkillSlashRewrite(ctx context.Context, event *gateway.MessageEvent, source *gateway.SessionSource, quickKey, command string) *string {
	if command == "" || r.hmBundleSlashRewrite(ctx, event, source, quickKey, command) {
		return nil
	}
	reply, err := r.skillSlashRewrite(ctx, event, source, quickKey, command)
	if err != nil {
		runLog().Debug(fmt.Sprintf("Skill command check failed (non-fatal): %s", err))
		return nil
	}
	return reply
}

// Skill command rewrite whose failures the caller logs as non fatal
func (r *GatewayRunner) skillSlashRewrite(ctx context.Context, event *gateway.MessageEvent, source *gateway.SessionSource, quickKey, command string) (*string, error) {
	scope := skills.NewScope(home.Dir(ctx), nil)
	skillCmds := scope.GetSkillCommands()
	cmdKey, found, err := scope.ResolveSkillCommandKey(ctx, command, false)
	if err != nil {
		return nil, err
	}
	if !found {
		return hmUnknownSlashReply(ctx, command, source), nil
	}
	platform := sourcePlatformValue(source)
	userInstruction := pystr.Strip(event.GetCommandArgs())
	extraKeys, stackedInstruction, err := scope.SplitStackedSkillCommands(ctx, userInstruction, false)
	stacked := err == nil
	if !stacked {
		extraKeys, stackedInstruction = nil, userInstruction
	}
	entry, ok := skillCmds[cmdKey]
	if !ok {
		return nil, fmt.Errorf("KeyError: %s", pyrepr.Repr(cmdKey))
	}
	nameOf := func(info map[string]any) string { return pyrepr.Str(pyrepr.Or(info["name"], "")) }
	skillName := ""
	if v, has := entry["name"]; has {
		skillName = pyrepr.Str(v)
	}
	if platform != "" && (skillName != "" || len(extraKeys) > 0) {
		platDisabled := scope.DisabledSkillNames(platform)
		if skillName != "" && platDisabled[skillName] {
			reply := i18n.T(ctx, "gateway.skills.disabled_for_platform", "", map[string]any{"name": skillName, "platform": platform})
			return &reply, nil
		}
		var disabledExtra []string
		for _, k := range extraKeys {
			if name := nameOf(skillCmds[k]); platDisabled[name] {
				disabledExtra = append(disabledExtra, name)
			}
		}
		if len(disabledExtra) > 0 {
			reply := i18n.T(ctx, "gateway.skills.stacked_disabled", "", map[string]any{"names": strings.Join(disabledExtra, ", "), "platform": platform})
			return &reply, nil
		}
	}
	if len(extraKeys) > 0 && stacked {
		text, _, _, built, err := scope.BuildStackedSkillInvocationMessage(ctx, append([]string{cmdKey}, extraKeys...), stackedInstruction, quickKey)
		if err != nil {
			return nil, err
		}
		if !built {
			reply := i18n.T(ctx, "gateway.skills.stacked_load_failed", "", map[string]any{"command": command})
			return &reply, nil
		}
		event.Text = text
		return nil, nil
	}
	msg, built, err := scope.BuildSkillInvocationMessage(ctx, cmdKey, userInstruction, quickKey, "")
	if err != nil {
		return nil, err
	}
	if built && msg != "" {
		event.Text = msg
	}
	return nil, nil
}
