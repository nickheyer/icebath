package plugins

import (
	"context"
	"fmt"
	"math/big"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const (
	ActionsContractVersion   = 1
	PlatformActionCapability = "gateway.platform_actions"
)

// Runner whose platform values gateway.config.Platform accepts, answering the enum value or an error
type PlatformActionRunner interface {
	ParsePlatform(value string) (string, error)
}

// Profile aware fail closed adapter lookup of a multiplex runner, the runner _authorization_adapter
type authorizationAdapterRunner interface {
	AuthorizationAdapter(platform, profile string) any
}

// Live adapter a gateway runner's default profile registry holds for a platform value, installed by pkg/gateway
var RunnerAdapterByValue func(runner any, value string) (adapter any, hasRegistry bool)

// Live adapter surface every verb needs, its platform value and connection state
type PlatformActionAdapter interface {
	PlatformValue() string
	IsConnected() bool
}

type telegramReactionAdapter interface {
	SetReaction(ctx context.Context, chatID, messageID, emoji string) (bool, error)
}

type telegramTopicAdapter interface {
	RenameDMTopic(ctx context.Context, chatID string, threadID *big.Int, title string) error
}

type discordThreadAdapter interface {
	RenameThread(ctx context.Context, threadID, title string) (bool, error)
}

type discordClientAdapter interface {
	DiscordClient() DiscordActionClient
}

// Discord client calls the reaction verb makes, a nil channel from GetChannel meaning not cached
type DiscordActionClient interface {
	GetChannel(id *big.Int) DiscordActionChannel
	FetchChannel(ctx context.Context, id *big.Int) (DiscordActionChannel, error)
}

type DiscordActionChannel interface {
	FetchMessage(ctx context.Context, id *big.Int) (DiscordActionMessage, error)
}

type DiscordActionMessage interface {
	AddReaction(ctx context.Context, emoji string) error
}

// Success result with the verb fields
func actionOK(action string) map[string]any { return map[string]any{"ok": true, "action": action} }

// AttributeError text a verb raises when the adapter lacks the method it calls
func missingAdapterMethod(adapter any, method string) error {
	return fmt.Errorf("'%s' object has no attribute '%s'", pyrepr.TypeName(adapter), method)
}

func telegramAddReaction(ctx context.Context, adapter any, args []string) (map[string]any, error) {
	a, ok := adapter.(telegramReactionAdapter)
	if !ok {
		return nil, missingAdapterMethod(adapter, "_set_reaction")
	}
	done, err := a.SetReaction(ctx, args[0], args[1], args[2])
	if err != nil {
		return nil, err
	}
	if done {
		return actionOK("add_reaction"), nil
	}
	return messageErr("action_failed", "telegram set_message_reaction failed"), nil
}

func discordAddReaction(ctx context.Context, adapter any, args []string) (map[string]any, error) {
	var client DiscordActionClient
	if a, ok := adapter.(discordClientAdapter); ok {
		client = a.DiscordClient()
	}
	if client == nil {
		return messageErr("adapter_disconnected", "discord client unavailable"), nil
	}
	channelID, okChannel := pyint.Parse(args[0])
	messageID, okMessage := pyint.Parse(args[1])
	if !okChannel || !okMessage {
		return messageErr("invalid_argument", "discord ids must be numeric"), nil
	}
	channel := client.GetChannel(channelID)
	if channel == nil {
		var err error
		if channel, err = client.FetchChannel(ctx, channelID); err != nil {
			return nil, err
		}
	}
	message, err := channel.FetchMessage(ctx, messageID)
	if err != nil {
		return nil, err
	}
	if err := message.AddReaction(ctx, args[2]); err != nil {
		return nil, err
	}
	return actionOK("add_reaction"), nil
}

func telegramSetThreadTitle(ctx context.Context, adapter any, args []string) (map[string]any, error) {
	a, ok := adapter.(telegramTopicAdapter)
	if !ok {
		return nil, missingAdapterMethod(adapter, "rename_dm_topic")
	}
	threadID, err := pyint.Of(args[1])
	if err != nil {
		return nil, err
	}
	if err := a.RenameDMTopic(ctx, args[0], threadID, args[2]); err != nil {
		return nil, err
	}
	return actionOK("set_thread_title"), nil
}

func discordSetThreadTitle(ctx context.Context, adapter any, args []string) (map[string]any, error) {
	a, ok := adapter.(discordThreadAdapter)
	if !ok {
		return nil, missingAdapterMethod(adapter, "rename_thread")
	}
	done, err := a.RenameThread(ctx, args[1], args[2])
	if err != nil {
		return nil, err
	}
	if done {
		return actionOK("set_thread_title"), nil
	}
	return messageErr("action_failed", "discord thread rename failed"), nil
}

var platformVerbs = map[string]map[string]func(context.Context, any, []string) (map[string]any, error){
	"add_reaction":     {"telegram": telegramAddReaction, "discord": discordAddReaction},
	"set_thread_title": {"telegram": telegramSetThreadTitle, "discord": discordSetThreadTitle},
}

// Per plugin facade over the live gateway adapters, resolved at call time and gated by the capability
type PlatformActions struct{ pluginID string }

// Whether the plugin holds the platform action capability, unreadable consent state denying
func (p *PlatformActions) capabilityGranted(ctx context.Context) bool {
	cfg, err := loadConfig(home.Dir(ctx))
	if err != nil {
		logDebug(fmt.Sprintf("platform_actions capability check failed for %s", p.pluginID), "error", err)
		return false
	}
	return CapabilityGranted(cfg, p.pluginID, PlatformActionCapability)
}

// Connected adapter of the active profile for a platform, or the refusal naming why none answers
func (p *PlatformActions) resolveAdapter(ctx context.Context, platform any) (any, map[string]any) {
	var runner PlatformActionRunner
	if s := installedServices(); s != nil && s.GatewayRunnerRef != nil {
		runner = s.GatewayRunnerRef()
	}
	if runner == nil {
		return nil, messageErr("gateway_unavailable", "no gateway runner is active in this process")
	}
	value, err := runner.ParsePlatform(pystr.Lower(pystr.Strip(pyrepr.Str(platform))))
	if err != nil {
		return nil, messageErr("unknown_platform", fmt.Sprintf("unknown platform %s", pyrepr.Repr(platform)))
	}
	var adapter any
	if resolver, ok := runner.(authorizationAdapterRunner); ok {
		adapter = resolver.AuthorizationAdapter(value, profileName(home.Dir(ctx)))
	} else if RunnerAdapterByValue != nil {
		adapter, _ = RunnerAdapterByValue(runner, value)
	}
	if adapter == nil {
		return nil, messageErr("adapter_not_registered", fmt.Sprintf("no %s adapter is registered", value))
	}
	connected := false
	if live, ok := adapter.(PlatformActionAdapter); ok {
		_ = callGuarded(func() error {
			connected = live.IsConnected()
			return nil
		})
	}
	if !connected {
		return nil, messageErr("adapter_disconnected", fmt.Sprintf("the %s adapter is not connected", value))
	}
	return adapter, nil
}

// Capability, argument and adapter gate chain shared by every verb
func (p *PlatformActions) gate(ctx context.Context, platform any, names []string, values []any) (any, map[string]any) {
	if !p.capabilityGranted(ctx) {
		return nil, messageErr("capability_not_granted", fmt.Sprintf("plugin %s lacks the %s capability (grant via consent flow or plugins.entries.%s.allow_platform_actions)",
			pyrepr.Repr(p.pluginID), pyrepr.Repr(PlatformActionCapability), p.pluginID))
	}
	for i, name := range names {
		if s, ok := values[i].(string); !ok || pystr.Strip(s) == "" {
			return nil, messageErr("invalid_argument", name+" must be a non-empty string")
		}
	}
	return p.resolveAdapter(ctx, platform)
}

// Gates, dispatches the verb to the platform implementation of the adapter, audits and returns
func (p *PlatformActions) run(ctx context.Context, verb string, platform any, names []string, values []any) map[string]any {
	adapter, refusal := p.gate(ctx, platform, names, values)
	var result map[string]any
	if refusal == nil && adapter != nil {
		value := ""
		if live, ok := adapter.(PlatformActionAdapter); ok {
			value = live.PlatformValue()
		}
		impl := platformVerbs[verb][value]
		if impl == nil {
			result = messageErr("unsupported_platform_action", fmt.Sprintf("%s is not implemented for %s", verb, pyrepr.Str(platform)))
		} else {
			args := make([]string, len(values))
			for i, v := range values {
				args[i] = v.(string)
			}
			v, err := guardedValue(func() (any, error) { return impl(ctx, adapter, args) })
			if err != nil {
				result = messageErr("action_failed", pystr.Slice(err.Error(), 0, 512))
			} else {
				result = v.(map[string]any)
			}
		}
	} else {
		result = refusal
		if len(result) == 0 {
			result = messageErr("gateway_unavailable", "")
		}
	}
	p.audit(verb, platform, result)
	return result
}

// Adds or sets an emoji reaction on a platform message
func (p *PlatformActions) AddReaction(ctx context.Context, platform, chatID, messageID, emoji any) map[string]any {
	return p.run(ctx, "add_reaction", platform, []string{"chat_id", "message_id", "emoji"}, []any{chatID, messageID, emoji})
}

// Renames a thread or forum topic
func (p *PlatformActions) SetThreadTitle(ctx context.Context, platform, chatID, threadID, title any) map[string]any {
	return p.run(ctx, "set_thread_title", platform, []string{"chat_id", "thread_id", "title"}, []any{chatID, threadID, title})
}

// Logs every platform action with its outcome
func (p *PlatformActions) audit(verb string, platform any, result map[string]any) {
	tail := ""
	if !pyrepr.Truthy(result["ok"]) {
		tail = " error=" + pyrepr.Str(result["error"])
	}
	logInfo(fmt.Sprintf("platform_action plugin=%s verb=%s platform=%s ok=%s%s", p.pluginID, verb, pyrepr.Str(platform), pyrepr.Str(result["ok"]), tail))
}

// Python attribute of the facade a hosted plugin reads, both verbs answering as coroutine methods
func (p *PlatformActions) HostAttr(_ context.Context, name string) (any, bool, bool, error) {
	if _, ok := platformVerbs[name]; ok {
		return nil, true, true, nil
	}
	return nil, false, false, &HostBuiltinError{Type: "AttributeError", Message: fmt.Sprintf("'PlatformActions' object has no attribute %s", pyrepr.Repr(name))}
}

// Runs add_reaction or set_thread_title for a hosted plugin with the Python argument binding
func (p *PlatformActions) HostCall(ctx context.Context, method string, args []any, kwargs map[string]any) (any, error) {
	third := map[string]string{"add_reaction": "message_id", "set_thread_title": "thread_id"}[method]
	last := map[string]string{"add_reaction": "emoji", "set_thread_title": "title"}[method]
	if third == "" {
		return nil, &HostBuiltinError{Type: "AttributeError", Message: fmt.Sprintf("'PlatformActions' object has no attribute %s", pyrepr.Repr(method))}
	}
	a, _, err := bindPyOn("PlatformActions", method, []pyParam{req("platform"), req("chat_id"), req(third), req(last)}, args, kwargs)
	if err != nil {
		return nil, err
	}
	if method == "add_reaction" {
		return p.AddReaction(ctx, a["platform"], a["chat_id"], a[third], a[last]), nil
	}
	return p.SetThreadTitle(ctx, a["platform"], a["chat_id"], a[third], a[last]), nil
}
