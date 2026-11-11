package gatewayrun

import (
	"context"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Runs a type exec quick command as a host shell of the gateway with a sanitized env and redacted output
var RunGatewayExecQuickCommand func(ctx context.Context, env *tools.Env, execCmd any, cwd string) (string, error)

// Fires the pre_command observers and the command interceptors, a rewrite returning the new command name
func (r *GatewayRunner) hmCommandHooks(ctx context.Context, event *gateway.MessageEvent, source *gateway.SessionSource, quickKey, command, canonical string) (bool, *string, *string) {
	rawArgs := pystr.Strip(event.GetCommandArgs())
	platform := string(source.Platform)
	func() {
		defer func() {
			if rec := recover(); rec != nil {
				runLog().Debug(fmt.Sprintf("pre_command hook dispatch failed (non-fatal): %s", pyrepr.Str(rec)))
			}
		}()
		plugins.FirePreCommandHook(ctx, "gateway", canonical, command, rawArgs, &quickKey, &platform)
	}()
	var userID any
	if source.UserID != nil {
		userID = *source.UserID
	}
	hookCtx := map[string]any{"platform": platform, "user_id": userID, "command": canonical,
		"raw_command": command, "args": rawArgs, "raw_args": rawArgs}
	results, err := r.hooks.emitCollect(ctx, "command:"+canonical, hookCtx)
	if err != nil {
		runLog().Debug(fmt.Sprintf("command:%s hook dispatch failed (non-fatal): %s", canonical, err))
		results = nil
	}
	for _, result := range results {
		hookResult, ok := result.(map[string]any)
		if !ok {
			continue
		}
		decision := pystr.Lower(pystr.Strip(hookPyStr(hookResult, "decision")))
		var message *string
		if text, isText := hookResult["message"].(string); isText && text != "" {
			message = &text
		}
		switch decision {
		case "deny":
			if message == nil {
				blocked := i18n.T(ctx, "gateway.hooks.command_blocked", "", map[string]any{"command": command})
				message = &blocked
			}
			return true, message, nil
		case "handled":
			return true, message, nil
		case "rewrite":
			newCommand := pystr.LstripChars(pystr.Strip(hookPyStr(hookResult, "command_name")), "/")
			if newCommand != "" {
				event.Text = pystr.Strip("/" + newCommand + " " + pystr.Strip(hookPyStr(hookResult, "raw_args")))
				rewritten, _ := event.GetCommand()
				return false, nil, &rewritten
			}
		}
	}
	return false, nil, nil
}

// Python str of a hook result field, empty when absent
func hookPyStr(hookResult map[string]any, key string) string {
	value, ok := hookResult[key]
	if !ok {
		return ""
	}
	return pyrepr.Str(value)
}

// Drain gate, operator quick commands and plugin slash commands, an alias quick command rewriting the command
func (r *GatewayRunner) hmDispatchQuickAndPluginCommands(ctx context.Context, event *gateway.MessageEvent, source *gateway.SessionSource, command string) (bool, *string, string, error) {
	if r.draining && !event.AdmittedBeforeDrain() {
		reply := i18n.T(ctx, "gateway.busy.drain_rejected_new_work", "", map[string]any{"action": r.statusActionGerund(ctx)})
		return true, &reply, command, nil
	}
	if command != "" {
		if raw, ok := r.hmQuickCommands()[command]; ok && raw != nil {
			if denied := r.checkSlashAccess(ctx, source, command); denied != "" {
				return true, &denied, command, nil
			}
			qcmd, _ := raw.(map[string]any)
			qtype := qcmd["type"]
			if qtype == "exec" {
				execCmd := pyrepr.Or(qcmd["command"], "")
				if !pyrepr.Truthy(execCmd) {
					reply := i18n.T(ctx, "gateway.quick_command.no_command", "", map[string]any{"command": command})
					return true, &reply, command, nil
				}
				reply, err := RunGatewayExecQuickCommand(ctx, tools.ToolEnvFrom(ctx), execCmd, "")
				if err != nil {
					return false, nil, command, err
				}
				return true, &reply, command, nil
			}
			if qtype != "alias" {
				reply := i18n.T(ctx, "gateway.quick_command.unsupported_type", "", map[string]any{"command": command})
				return true, &reply, command, nil
			}
			newCommand, expanded := hmExpandAliasQuickCommand(event, qcmd)
			if !expanded {
				reply := i18n.T(ctx, "gateway.quick_command.no_target", "", map[string]any{"command": command})
				return true, &reply, command, nil
			}
			command = newCommand
		}
	}
	if command == "" {
		return false, nil, command, nil
	}
	return r.hmDispatchPluginCommand(ctx, event, source, command)
}

// Runs the plugin registered slash command of that name, unhandled when no plugin registers it
func (r *GatewayRunner) hmDispatchPluginCommand(ctx context.Context, event *gateway.MessageEvent, source *gateway.SessionSource, command string) (bool, *string, string, error) {
	if command == "" {
		return false, nil, command, nil
	}
	handler, ok := plugins.GetPluginCommandHandler(ctx, strings.ReplaceAll(command, "_", "-"))
	if !ok || handler == nil {
		return false, nil, command, nil
	}
	reply, err := r.hmRunPluginHandler(ctx, handler, event, source, nil, nil)
	if err != nil {
		runLog().Warn(fmt.Sprintf("Plugin command dispatch failed: %s", err))
		return false, nil, command, nil
	}
	return true, reply, command, nil
}

// Runs a resolved plugin command handler under the session env of the source, its reply text or nil
func (r *GatewayRunner) hmRunPluginHandler(ctx context.Context, handler plugins.PluginCommandHandler, event *gateway.MessageEvent, source *gateway.SessionSource, sessionEntry *gateway.SessionEntry, args *string) (*string, error) {
	sessionKey, err := r.sessionKeyForSource(ctx, source)
	if err != nil {
		return nil, err
	}
	if sessionEntry == nil && r.sessionStore != nil {
		sessionEntry = r.sessionStore.LookupBySessionKey(ctx, sessionKey)
	}
	pluginContext := gateway.BuildSessionContext(ctx, source, r.config, sessionEntry)
	if sessionEntry == nil {
		pluginContext.SessionKey = sessionKey
	}
	userArgs := pystr.Strip(event.GetCommandArgs())
	if args != nil {
		userArgs = *args
	}
	var result any
	err = r.sessionEnvScope(ctx, pluginContext, func(env *tools.Env) error {
		scoped := tools.WithToolEnv(ctx, env)
		scoped = gateway.WithLiveSource(scoped, source)
		scoped = r.chatChoices(scoped, source, sessionKey)
		var runErr error
		result, runErr = r.runInExecutorWithContext(scoped, func(ctx context.Context) (any, error) {
			return plugins.InvokePluginCommand(ctx, handler, userArgs, map[string]any{
				"source": source, "session_id": pluginContext.SessionID, "session_key": pluginContext.SessionKey,
				"platform": string(source.Platform)})
		})
		return runErr
	})
	if err != nil {
		return nil, err
	}
	if !pyrepr.Truthy(result) {
		return nil, nil
	}
	text := pyrepr.Str(result)
	return &text, nil
}
