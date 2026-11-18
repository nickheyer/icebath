//go:build !icebath_core

package main

// Links the plugins tier, model providers, messaging platforms, backends and catalog plugins
import (
	"context"

	_ "github.com/nickheyer/icebath/pkg/acpclient"
	_ "github.com/nickheyer/icebath/pkg/builtin"
	_ "github.com/nickheyer/icebath/pkg/builtin/camofox"
	_ "github.com/nickheyer/icebath/pkg/builtin/diskcleanup"
	"github.com/nickheyer/icebath/pkg/builtin/langfuse"
	_ "github.com/nickheyer/icebath/pkg/builtin/mcpserveplugin"
	_ "github.com/nickheyer/icebath/pkg/builtin/securityguidance"
	_ "github.com/nickheyer/icebath/pkg/builtin/spotify"
	"github.com/nickheyer/icebath/pkg/builtin/teamspipeline"
	_ "github.com/nickheyer/icebath/pkg/builtin/webbackends"
	"github.com/nickheyer/icebath/pkg/commands"
	"github.com/nickheyer/icebath/pkg/config"
	_ "github.com/nickheyer/icebath/pkg/config/remote"
	"github.com/nickheyer/icebath/pkg/containerinit"
	"github.com/nickheyer/icebath/pkg/containerinit/stage"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/gateway/messagetools"
	_ "github.com/nickheyer/icebath/pkg/gateway/platforms/bundled"
	"github.com/nickheyer/icebath/pkg/gateway/platforms/feishu"
	"github.com/nickheyer/icebath/pkg/gateway/platforms/matrix"
	"github.com/nickheyer/icebath/pkg/gateway/platforms/slack"
	"github.com/nickheyer/icebath/pkg/gateway/platforms/telegram"
	"github.com/nickheyer/icebath/pkg/gateway/platforms/wecom"
	"github.com/nickheyer/icebath/pkg/gateway/platforms/whatsapp"
	"github.com/nickheyer/icebath/pkg/gateway/platforms/whatsappcloud"
	"github.com/nickheyer/icebath/pkg/gatewaycli"
	"github.com/nickheyer/icebath/pkg/gatewayrun"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/linktier"
	_ "github.com/nickheyer/icebath/pkg/media"
	"github.com/nickheyer/icebath/pkg/relayhost"
	"github.com/nickheyer/icebath/pkg/startupfast"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/spf13/cobra"
)

var _ gateway.NativeStreamAdapter = (*wecom.Streamer)(nil)

func init() {
	linktier.Link(linktier.Plugins)
	tierArgvEntries = append(tierArgvEntries, containerinit.Dispatch)
	tierStartup[phaseBeforeStart] = append(tierStartup[phaseBeforeStart], containerinit.DropRootInImage)
	childEntriesAfterPrunePlugins = append(childEntriesAfterPrunePlugins, relayhost.DispatchRelayHostEntry)
	childEntriesAfterStopMark = append(childEntriesAfterStopMark, stage.DispatchStageEntry)
	tierWiring = append(tierWiring, installPluginsWiring)
	tierCLI = append(tierCLI, func(cli *commands.CLI) {
		bindGroups(cli, map[string]groupHandler{"send": sendGroup, "slack": slackGroup, "whatsapp": whatsappGroup, "whatsapp-cloud": whatsappCloudGroup})
	})
	tierFlushAtExit = append(tierFlushAtExit, langfuse.AtExit)
}

// Hooks the plugins tier installs into the core packages
func installPluginsWiring() {
	tools.RegisterNemoRelayBinding(relayhost.Load)
	tools.RegisterNemoRelayReadiness(relayhost.Ready)
	tools.RegisterNemoRelayProvisioner(relayhost.ProvisionRuntime)
	gatewayrun.BindTeamsPipelineRuntime = func(ctx context.Context, r *gatewayrun.GatewayRunner) (bool, error) {
		return teamspipeline.BindGatewayRuntime(ctx, r)
	}
	startupfast.VersionLines = append(startupfast.VersionLines, matrix.E2EEVersionLine)
	gatewaycli.SendMessageTool = messagetools.SendMessageTool
	slack.SubcommandMap = func(ctx context.Context) map[string]string {
		return commands.SlackSubcommandMap(ctx, commands.Default, rawConfigTree(ctx))
	}
	gateway.RelaySlackSubcommandMap = slack.SubcommandMap
	slack.IsGatewayKnownCommand = func(ctx context.Context, name string) bool {
		return commands.IsGatewayKnownCommand(ctx, commands.Default, name)
	}
	matrix.IsGatewayKnownCommand = slack.IsGatewayKnownCommand
	slack.AppManifest = func(ctx context.Context, cfg config.Tree) map[string]any {
		return commands.SlackAppManifest(ctx, commands.Default, cfg, "")
	}
	feishu.CommentAgentRunner = runFeishuCommentAgent
	slack.NativeSlashNames = func(ctx context.Context) []string {
		var names []string
		for _, s := range commands.SlackNativeSlashes(ctx, commands.Default, rawConfigTree(ctx)) {
			names = append(names, s.Name)
		}
		return names
	}
	slack.ResolveCommandBusy = func(name string) (string, string, bool) {
		if d := commands.Resolve(name); d != nil {
			return d.Name, string(d.BusyPolicy), true
		}
		return "", "", false
	}
	telegram.InlineCommands = telegramInlineCommands{}
	telegram.SendTelegramREST = messagetools.SendTelegram
}

// Feishu comment agent turn with the feishu doc and drive toolsets
func runFeishuCommentAgent(ctx context.Context, prompt string, hooks feishu.CommentAgentHooks) (*feishu.CommentAgentRun, error) {
	run, err := gatewayrun.RunCommentAgent(ctx, prompt, []any{"feishu_doc", "feishu_drive"}, func(rt gatewayrun.CommentAgentRuntime) []map[string]any {
		return hooks.OnRuntime(feishu.CommentAgentRuntime(rt))
	}, hooks.OnRun)
	if err != nil {
		return nil, err
	}
	return (*feishu.CommentAgentRun)(run), nil
}

func sendGroup(ctx context.Context, cli *commands.CLI, cmd *cobra.Command, path string, args []string) (int, error) {
	return gatewaycli.SendCommand(ctx, home.Dir(ctx), namespace(cli, cmd, path, args), cmd.OutOrStdout(), cmd.ErrOrStderr())
}

func slackGroup(ctx context.Context, cli *commands.CLI, cmd *cobra.Command, path string, args []string) (int, error) {
	ns := namespace(cli, cmd, path, args)
	ns["slack_command"] = nilWhenEmpty(secondWord(path))
	return slack.CmdSlack(ctx, ns, cmd.OutOrStdout(), cmd.ErrOrStderr())
}

func whatsappGroup(ctx context.Context, cli *commands.CLI, cmd *cobra.Command, path string, args []string) (int, error) {
	return whatsapp.CmdWhatsApp(ctx, home.Dir(ctx), namespace(cli, cmd, path, args), cmd.OutOrStdout(), requireTTY)
}

func whatsappCloudGroup(ctx context.Context, cli *commands.CLI, cmd *cobra.Command, path string, args []string) (int, error) {
	return whatsappcloud.CmdWhatsAppCloud(ctx, home.Dir(ctx), namespace(cli, cmd, path, args), cmd.OutOrStdout(), requireTTY)
}

// The pkg/commands readers behind the Telegram inline picker
type telegramInlineCommands struct{}

func (telegramInlineCommands) TelegramBotCommands(ctx context.Context) ([]telegram.CatalogItem, error) {
	var out []telegram.CatalogItem
	for _, e := range commands.TelegramBotCommands(ctx, commands.Default, rawConfigTree(ctx), true) {
		out = append(out, telegram.CatalogItem{Name: e.Name, Description: e.Description})
	}
	return out, nil
}

func (telegramInlineCommands) GatewaySkillEntries(ctx context.Context, platform string, maxSlots int, reserved map[string]bool,
	descLimit int, sanitize func(string) string) ([]telegram.CatalogItem, error) {
	cfg := rawConfigTree(ctx)
	entries, _ := commands.GatewaySkillEntries(ctx, commands.Default, platform, commands.HomeSkillSource(home.Dir(ctx), cfg), maxSlots, reserved, descLimit, sanitize)
	var out []telegram.CatalogItem
	for _, e := range entries {
		out = append(out, telegram.CatalogItem{Name: e.Name, Description: e.Description})
	}
	return out, nil
}

func (telegramInlineCommands) TelegramMenuMaxCommands(ctx context.Context) int {
	return commands.TelegramMenuMaxCommands(rawConfigTree(ctx))
}

func (telegramInlineCommands) TelegramMenuCommands(ctx context.Context, maxCommands int) ([]telegram.CatalogItem, int, error) {
	cfg := rawConfigTree(ctx)
	menu, hidden := commands.TelegramMenuCommands(ctx, commands.Default, cfg, commands.HomeSkillSource(home.Dir(ctx), cfg), maxCommands)
	out := make([]telegram.CatalogItem, 0, len(menu))
	for _, e := range menu {
		out = append(out, telegram.CatalogItem{Name: e.Name, Description: e.Description})
	}
	return out, hidden, nil
}

func (telegramInlineCommands) SanitizeTelegramName(raw string) string {
	return commands.SanitizeTelegramName(raw)
}
