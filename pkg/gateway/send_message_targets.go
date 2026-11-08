package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyre"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/provider"
)

var phonePlatforms = map[string]bool{"photon": true, "signal": true, "sms": true, "whatsapp": true}

var e164TargetRe = func() interface {
	MatchString(string) (bool, error)
} {
	re, err := pyre.CompileFullmatch(`^\s*\+(\d{7,15})\s*$`)
	if err != nil {
		panic(err)
	}
	return re
}()

// Platforms whose home channel hint names a variable other than the PLATFORM_HOME_CHANNEL default
var HomeChannelEnvOverrides = map[string]string{"email": "EMAIL_HOME_ADDRESS"}

// Channel directory lookup, a variable so tests can answer it
var resolveChannelNameFn = resolveChannelName

// Replaces the channel directory lookup and returns the one it replaced
func SetResolveChannelNameFn(fn func(ctx context.Context, platformName, name string) (string, error)) func(context.Context, string, string) (string, error) {
	prev := resolveChannelNameFn
	resolveChannelNameFn = fn
	return prev
}

// Declared native target syntax parser of a platform, discovering plugins first when the name is unknown
func targetSyntaxFn(ctx context.Context, platformName string) func(ref string) any {
	if !Registry.IsRegistered(ctx, platformName) {
		plugins.DiscoverPlugins(ctx, false)
	}
	if entry := Registry.Get(ctx, platformName); entry != nil {
		return entry.TargetRefSyntaxFn
	}
	return nil
}

// Chat id, thread id and explicitness from the plugin parser then phone, numeric, Matrix and XMPP rules
func parseTargetRef(ctx context.Context, platformName, targetRef string) (chatID, threadID *string, explicit bool) {
	if parser := targetSyntaxFn(ctx, platformName); parser != nil {
		parsed := parser(targetRef)
		if parsed == NotExplicit {
			return nil, nil, false
		}
		if p, ok := parsed.(*TargetRefParse); ok && p != nil {
			return p.ChatID, p.ThreadID, true
		}
	}
	if phonePlatforms[platformName] {
		if ok, _ := e164TargetRe.MatchString(targetRef); ok {
			stripped := pystr.Strip(targetRef)
			return &stripped, nil, true
		}
	}
	if pystr.StrIsDigit(strings.TrimLeft(targetRef, "-")) ||
		(platformName == "matrix" && (strings.HasPrefix(targetRef, "!") || strings.HasPrefix(targetRef, "@"))) ||
		(platformName == "xmpp" && strings.Contains(targetRef, "@")) {
		ref := targetRef
		return &ref, nil, true
	}
	return nil, nil, false
}

// Chat id, thread id and explicitness of a target ref under the platform rules
func ParseTargetRef(ctx context.Context, platformName, targetRef string) (chatID, threadID *string, explicit bool) {
	return parseTargetRef(ctx, platformName, targetRef)
}

// Error naming an unregistered plugin platform with its install command when it left core
func UnknownPlatformError(ctx context.Context, platformName string) string {
	return "Unknown or unregistered plugin platform: " + platformName + provider.PlatformInstallHint(ctx, platformName)
}

// Result of a plugin callback, with a panic reported as failed like a raised exception
func guardPluginCall[T any](call func() T) (out T, failed bool) {
	defer func() {
		if r := recover(); r != nil {
			failed = true
		}
	}()
	return call(), false
}

// Chat and thread ids for one send target the same way for the model tool, the CLI and cron
func ResolveSendTarget(ctx context.Context, platform, ref string, passUnresolvedReferences bool) (chatID string, threadID *string, resolutionError string) {
	entry := Registry.Get(ctx, platform)
	validated := func(chatID string, threadID *string) (string, *string, string) {
		if entry == nil || entry.ValidateTargetRefFn == nil {
			return chatID, threadID, ""
		}
		verdict, failed := guardPluginCall(func() any { return entry.ValidateTargetRefFn(chatID) })
		if failed {
			slog.Debug("Plugin target validator failed for " + platform)
			return "", nil, fmt.Sprintf("Target validator failed for platform '%s'", platform)
		}
		if verdict == true {
			return chatID, threadID, ""
		}
		detail := ""
		if s, ok := verdict.(string); ok && s != "" {
			detail = ": " + s
		}
		return "", nil, fmt.Sprintf("Invalid target '%s' on %s%s", ref, platform, detail)
	}
	if entry != nil && entry.ParseTargetRefFn != nil {
		type parsed struct {
			chatID   string
			threadID *string
			ok       bool
		}
		got, failed := guardPluginCall(func() parsed {
			c, t, ok := entry.ParseTargetRefFn(ref)
			return parsed{c, t, ok}
		})
		if failed {
			slog.Debug("Plugin target parser failed for " + platform)
			return "", nil, fmt.Sprintf("Target parser failed for platform '%s'", platform)
		}
		if got.ok {
			if got.chatID == "" {
				return "", nil, fmt.Sprintf("Target parser for platform '%s' returned an invalid result", platform)
			}
			return validated(got.chatID, got.threadID)
		}
	}
	parsedChatID, parsedThreadID, explicit := parseTargetRef(ctx, platform, ref)
	if explicit && parsedChatID != nil {
		return validated(*parsedChatID, parsedThreadID)
	}
	resolutionFailed := false
	resolved, err := resolveChannelNameFn(ctx, platform, ref)
	if err != nil {
		resolved, resolutionFailed = "", true
	}
	if resolved != "" {
		parsedChatID, parsedThreadID, _ = parseTargetRef(ctx, platform, resolved)
		if parsedChatID != nil && *parsedChatID != "" {
			return validated(*parsedChatID, parsedThreadID)
		}
		return validated(resolved, parsedThreadID)
	}
	isBuiltin := BuiltinPlatformValues[platform]
	if entry == nil && !isBuiltin {
		return "", nil, UnknownPlatformError(ctx, platform)
	}
	isPlugin := entry != nil && entry.Source == "plugin" && !isBuiltin
	if passUnresolvedReferences && (!isPlugin || entry.ParseTargetRefFn == nil) {
		chatID, threadID, errText := validated(ref, nil)
		if errText == "" {
			slog.Debug(fmt.Sprintf("Handing unresolved target '%s' to the %s adapter unchanged (the adapter validates it)", ref, platform))
		}
		return chatID, threadID, errText
	}
	hint := "Use send_message(action='list') to see available targets."
	if isPlugin {
		hint = "The plugin parser did not recognize it and no channel-directory entry matched."
	} else if resolutionFailed {
		hint = "Try using a numeric channel ID instead."
	}
	return "", nil, fmt.Sprintf("Could not resolve '%s' on %s. %s", ref, platform, hint)
}

// Loads the enabled standalone platform plugins before tool schemas and cache keys are built
func PrepareSendMessagePlatforms(ctx context.Context) {
	plugins.DiscoverPlugins(ctx, false)
}
