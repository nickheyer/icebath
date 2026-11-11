package gatewayrun_test

import (
	"context"

	"github.com/nickheyer/icebath/pkg/app"
	"github.com/nickheyer/icebath/pkg/commands"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/gatewayrun"
	"github.com/nickheyer/icebath/pkg/selfupdate"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Installs the runner hooks cmd/icebath wiring assigns at startup with the same production functions
func init() {
	gatewayrun.ResolveEphemeralSystemPromptFromConfig = commands.ResolveEphemeralSystemPrompt
	gatewayrun.ParseServiceTierConfig = app.ParseServiceTierConfig
	gatewayrun.ActivePersonalityName = commands.ActivePersonalityName
	gatewayrun.AvailablePersonalities = func(cfg map[string]any) map[string]any { return commands.AvailablePersonalities(config.Tree(cfg)) }
	gatewayrun.DescribePersonality = commands.DescribePersonality
	gatewayrun.PersistPersonality = commands.PersistPersonality
	gatewayrun.ResolvePersonality = func(value any, cfg map[string]any) (string, string, error) {
		return commands.ResolvePersonality(value, config.Tree(cfg))
	}
	gatewayrun.ResolveCommand = func(name string) (string, bool) {
		if d := commands.Resolve(name); d != nil {
			return d.Name, true
		}
		return "", false
	}
	gatewayrun.SlashConfirmPending = func(sessionKey string) bool {
		_, ok := commands.Confirms.Pending(sessionKey)
		return ok
	}
	gatewayrun.SlashConfirmClear = commands.Confirms.Clear
	gatewayrun.SlashConfirmGetPending = func(sessionKey string) (string, bool) {
		p, ok := commands.Confirms.Pending(sessionKey)
		return p.ConfirmID, ok
	}
	gatewayrun.SlashConfirmResolve = func(ctx context.Context, sessionKey, confirmID, choice string) string {
		reply, _ := commands.Confirms.Resolve(ctx, sessionKey, confirmID, choice, commands.ConfirmTimeout)
		return reply
	}
	gateway.SlashConfirmResolve = gatewayrun.SlashConfirmResolve
	gatewayrun.SlashConfirmClearIfStale = func(sessionKey string) bool {
		return commands.Confirms.ClearIfStale(sessionKey, commands.ConfirmTimeout)
	}
	gatewayrun.RunGatewayExecQuickCommand = commands.RunGatewayExecQuickCommand
	gatewayrun.SlashConfirmRegister = func(sessionKey, confirmID, command string, handler func(ctx context.Context, choice string) (string, error)) {
		commands.Confirms.Register(sessionKey, confirmID, command, handler)
	}
	gatewayrun.SaveConfigValue = app.SaveConfigValue
	gatewayrun.SanitizeTelegramName = commands.SanitizeTelegramName
	gatewayrun.KanbanRunSlash = app.RunKanbanSlash
	gatewayrun.RunCuratorSlash = app.RunCuratorSlash
	gatewayrun.RandomTip = app.RandomTip
	gatewayrun.GatewayDebugLocalReport = selfupdate.GatewayDebugLocalReport
	gatewayrun.GatewayDebugUploadReply = selfupdate.GatewayDebugUploadReply
	gatewayrun.RunApprovalModeCommand = func(ctx context.Context, h string, env *tools.Env, requestedMode string) (string, error) {
		result, err := commands.RunApprovalModeCommand(ctx, h, env, requestedMode)
		return result.Message, err
	}
	gatewayrun.BuildInitPromptForCwd = app.BuildInitPromptForCwd
	gatewayrun.ResolveCommandBusy = func(name string) (string, string, string, bool) {
		if d := commands.Resolve(name); d != nil {
			return d.Name, string(d.BusyPolicy), d.BusyHandler, true
		}
		return "", "", "", false
	}
	gatewayrun.IsGatewayKnownCommand = func(name string) bool { return commands.IsGatewayKnownCommand(commands.Default, name) }
	gatewayrun.GatewayKnownCommand = func(name string) bool { return commands.GatewayKnownCommands()[name] }
	gatewayrun.ExecuteSlashCommand = func(ctx context.Context, name, surface, args string, options map[string]any) (gatewayrun.SlashCommandReply, error) {
		reply, err := commands.ExecuteCommand(ctx, name, commands.CommandContext{Surface: surface, Args: args, Options: options})
		return gatewayrun.SlashCommandReply{Text: reply.Text, Data: reply.Data, Format: reply.Format}, err
	}
}
