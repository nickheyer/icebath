package messagetools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyre"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

var slackUserIDRE = func() interface {
	MatchString(string) (bool, error)
} {
	re, err := pyre.CompileFullmatch(`^\s*(U[A-Z0-9]{8,})\s*$`)
	if err != nil {
		panic(err)
	}
	return re
}()

// Seams the hermes tests patch, the gateway config loader, the platform send and the session mirror
var (
	loadGatewayConfig = gateway.LoadGatewayConfig
	sendToPlatformFn  = SendToPlatform
	mirrorToSession   = gateway.MirrorToSession
)

// JSON text of a tool result with ASCII escapes as json.dumps writes it
func dumpsResult(v any) string {
	text, err := pyjson.Dumps(v)
	if err != nil {
		return tools.ErrorResult(err)
	}
	return text
}

// Handles a send, list, react or unreact call and returns its JSON result
func SendMessageTool(ctx context.Context, args map[string]any) (string, error) {
	action := any("send")
	if v, ok := args["action"]; ok {
		action = v
	}
	switch action {
	case "list":
		return handleList(ctx), nil
	case "react", "unreact":
		return handleReact(ctx, args, action == "unreact")
	}
	return handleSend(ctx, args)
}

// Platform, chat, thread and resolution error of a tool target, empty chat when no ref was given
func resolveToolTarget(ctx context.Context, target string, passUnresolved bool) (platformName, chatID string, threadID *string, resErr string) {
	name, ref, _ := strings.Cut(target, ":")
	platformName, ref = pystr.Lower(pystr.Strip(name)), pystr.Strip(ref)
	discoverPlugins(ctx)
	if ref == "" {
		return platformName, "", nil, ""
	}
	chatID, threadID, resErr = gateway.ResolveSendTarget(ctx, platformName, ref, passUnresolved)
	return platformName, chatID, threadID, resErr
}

// Channel directory targets as JSON, or the load failure
func handleList(ctx context.Context) string {
	text, err := gateway.FormatDirectoryForDisplay(ctx, nil)
	if err != nil {
		return dumpsResult(sendError(fmt.Sprintf("Failed to load channel directory: %v", err)))
	}
	return dumpsResult(map[string]any{"targets": text})
}

// Refusal text of the relay egress guard, a refusal as well when the guard itself fails
func authorizeRelayTarget(ctx context.Context, platformName string, chatID any, threadID *string, native gateway.NativeToken) (denial string) {
	defer func() {
		if r := recover(); r != nil {
			denial = relayGuardFailed(platformName, fmt.Sprint(r))
		}
	}()
	var thread any
	if threadID != nil {
		thread = *threadID
	}
	denial, err := gateway.AuthorizeRelayTarget(ctx, platformName, chatID, thread, native)
	if err != nil {
		return relayGuardFailed(platformName, err.Error())
	}
	return denial
}

func relayGuardFailed(platformName, detail string) string {
	sendLog().Error(fmt.Sprintf("relay target authorization FAILED for %s — refusing the send", platformName), "error", detail)
	return fmt.Sprintf("Refusing to send to relay target '%s': the egress authorization check failed, so this destination "+
		"could not be verified. This is a bug — the send was blocked rather than allowed through unchecked.", platformName)
}

// String argument stripped, empty for a missing or falsy value, an AttributeError for other types
func strippedArg(args map[string]any, key string) (string, error) {
	v := args[key]
	if !pyrepr.Truthy(v) {
		return "", nil
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("'%s' object has no attribute 'strip'", pyrepr.TypeName(v))
	}
	return pystr.Strip(s), nil
}

// Argument value or its default, the args.get idiom
func argOr(args map[string]any, key string, def any) any {
	if v, ok := args[key]; ok {
		return v
	}
	return def
}

type addReactionResult interface {
	AddReaction(ctx context.Context, chatID, messageID, emoji string) (any, error)
}

type removeReactionResult interface {
	RemoveReaction(ctx context.Context, chatID, messageID string) (any, error)
}

type addReactionErr interface {
	AddReaction(ctx context.Context, chatID, messageID, emoji string) error
}

type removeReactionErr interface {
	RemoveReaction(ctx context.Context, chatID, messageID string) error
}

// Adds or removes an emoji reaction through the live gateway adapter
func handleReact(ctx context.Context, args map[string]any, remove bool) (string, error) {
	target := argOr(args, "target", "")
	emoji, err := strippedArg(args, "emoji")
	if err != nil {
		return "", err
	}
	messageID, err := strippedArg(args, "message_id")
	if err != nil {
		return "", err
	}
	targetStr, _ := target.(string)
	if !pyrepr.Truthy(target) || (!remove && emoji == "") {
		if remove {
			return toolError("'target' is required when action='unreact'"), nil
		}
		return toolError("Both 'target' and 'emoji' are required when action='react'"), nil
	}
	platformName, chatID, threadID, resErr := resolveToolTarget(ctx, targetStr, true)
	if resErr != "" {
		return toolError(resErr), nil
	}
	platform, perr := platformEnum(ctx, platformName)
	if perr != "" {
		return toolError(perr), nil
	}
	if chatID == "" {
		cfg, err := loadGatewayConfig(ctx)
		var home *gateway.HomeChannel
		if err == nil {
			home = cfg.GetHomeChannel(platform)
		}
		if home == nil {
			return toolError(fmt.Sprintf("No chat specified and no home channel set for %s. Use '%s:chat_id'.", platformName, platformName)), nil
		}
		chatID = home.ChatID
	}
	if denial := authorizeRelayTarget(ctx, platformName, chatID, threadID, gateway.NativeToken{}); denial != "" {
		return toolError(denial), nil
	}
	_, adapter := liveAdapter(ctx, platform, "")
	if adapter == nil {
		return toolError(fmt.Sprintf("Reactions require a live %s adapter in the running gateway (not available from cron/standalone contexts).", platformName)), nil
	}
	var call func() (any, error)
	if remove {
		switch a := adapter.(type) {
		case removeReactionResult:
			call = func() (any, error) { return a.RemoveReaction(ctx, chatID, messageID) }
		case removeReactionErr:
			call = func() (any, error) { return true, a.RemoveReaction(ctx, chatID, messageID) }
		}
	} else {
		switch a := adapter.(type) {
		case addReactionResult:
			call = func() (any, error) { return a.AddReaction(ctx, chatID, messageID, emoji) }
		case addReactionErr:
			call = func() (any, error) { return true, a.AddReaction(ctx, chatID, messageID, emoji) }
		}
	}
	if call == nil {
		return toolError(fmt.Sprintf("Platform '%s' does not support message reactions.", platformName)), nil
	}
	result, err := guardedCall(call)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return "", err
		}
		return dumpsResult(sendError(fmt.Sprintf("Reaction failed: %v", err))), nil
	}
	if d, ok := dictGet(result); ok {
		return dumpsResult(d), nil
	}
	return dumpsResult(map[string]any{"success": pyrepr.Truthy(result)}), nil
}

// Python dict form of a result held as a Go map or a decoded JSON object
func dictGet(v any) (any, bool) {
	switch d := v.(type) {
	case map[string]any:
		return d, d != nil
	case *pyjson.Object:
		return d, d != nil
	}
	return nil, false
}

// Runs the call with a panic returned as its error
func guardedCall(call func() (any, error)) (out any, err error) {
	defer func() {
		if r := recover(); r != nil {
			if e, ok := r.(error); ok {
				err = e
			} else {
				err = fmt.Errorf("%v", r)
			}
		}
	}()
	return call()
}

// Sends a message with MEDIA tags to the target, the home channel when the target names only a platform
func handleSend(ctx context.Context, args map[string]any) (string, error) {
	targetV, messageV := argOr(args, "target", ""), argOr(args, "message", "")
	if !pyrepr.Truthy(targetV) || !pyrepr.Truthy(messageV) {
		return toolError("Both 'target' and 'message' are required when action='send'"), nil
	}
	target, _ := targetV.(string)
	message, _ := messageV.(string)
	message = state.SanitizeSurrogates(message)
	platformName, chatID, threadID, resErr := resolveToolTarget(ctx, target, false)
	if resErr != "" {
		return toolError(resErr), nil
	}
	if tools.IsInterrupted(ctx) {
		return toolError("Interrupted"), nil
	}
	cfg, err := loadGatewayConfig(ctx)
	if err != nil {
		return dumpsResult(sendError(fmt.Sprintf("Failed to load gateway config: %v", err))), nil
	}
	platform, pconfig, entry, cerr, err := resolvePlatformConfig(ctx, platformName, cfg)
	if err != nil {
		return "", err
	}
	if cerr != "" {
		return toolError(cerr), nil
	}
	forceDocument := strings.Contains(message, "[[as_document]]")
	mediaFiles, cleaned, err := gateway.ExtractMedia(ctx, message)
	if err != nil {
		return "", err
	}
	dropped := []map[string]any{}
	mediaFiles, err = gateway.FilterMediaDeliveryPathsFor(ctx, mediaFiles, "", &dropped)
	if err != nil {
		return "", err
	}
	mirrorText := pystr.Strip(cleaned)
	if mirrorText == "" {
		mirrorText = describeMediaForMirror(mediaFiles)
	}
	usedHome := chatID == ""
	if usedHome {
		id, herr, err := homeChatID(ctx, cfg, platform, platformName)
		if err != nil {
			return "", err
		}
		if herr != "" {
			return toolError(herr), nil
		}
		chatID = id
	}
	if skip := maybeSkipCronDuplicateSend(ctx, platformName, chatID, threadID); skip != nil {
		return dumpsResult(skip), nil
	}
	if platformName == "slack" && chatID != "" {
		resolved, rerr := slackDMChatID(ctx, pconfig, chatID)
		if rerr != nil {
			return dumpsResult(rerr), nil
		}
		chatID = resolved
	}
	if denial := authorizeRelayTarget(ctx, platformName, chatID, threadID, gateway.NativeToken{Set: true, Token: pconfig.Token}); denial != "" {
		return toolError(denial), nil
	}
	opts := SendOptions{ThreadID: threadID, MediaFiles: mediaFiles, ForceDocument: forceDocument}
	if entry != nil && entry.SendMessageHandler != nil {
		opts.Args = args
	}
	if mentions := args["mentions"]; pyrepr.Truthy(mentions) && entry != nil && entry.StandaloneSend != nil && entry.StandaloneSend.Mentions {
		opts.Mentions = mentionList(mentions)
	}
	result, err := guardedSend(func() (map[string]any, error) {
		return sendToPlatformFn(ctx, platform, pconfig, chatID, cleaned, opts)
	})
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return "", err
		}
		return dumpsResult(sendError(fmt.Sprintf("Send failed: %v", err))), nil
	}
	if result == nil {
		return "null", nil
	}
	if pyrepr.Truthy(result["success"]) {
		if usedHome {
			result["note"] = fmt.Sprintf("Sent to %s home channel (chat_id: %s)", platformName, chatID)
		}
		if mirrorText != "" && mirrorSentMessage(ctx, platformName, chatID, mirrorText, threadID) {
			result["mirrored"] = true
		}
		if len(dropped) > 0 {
			result["success"] = false
			result["partial_success"] = true
			result["error"] = fmt.Sprintf("Delivery incomplete: %d requested MEDIA attachment(s) dropped before delivery (see media_dropped)", len(dropped))
		}
	}
	if len(dropped) > 0 {
		result["media_dropped"] = dropped
	}
	if e, ok := result["error"]; ok {
		result["error"] = sanitizeErrorText(e)
	}
	return dumpsResult(result), nil
}

// Mentions argument as a list, a lone string wrapped
func mentionList(v any) []string {
	switch m := v.(type) {
	case string:
		return []string{m}
	case []string:
		return append([]string(nil), m...)
	case []any:
		out := make([]string, 0, len(m))
		for _, x := range m {
			out = append(out, pyrepr.Str(x))
		}
		return out
	}
	return []string{pyrepr.Str(v)}
}

// Runs the send with a panic returned as its error
func guardedSend(send func() (map[string]any, error)) (out map[string]any, err error) {
	defer func() {
		if r := recover(); r != nil {
			if e, ok := r.(error); ok {
				err = e
			} else {
				err = fmt.Errorf("%v", r)
			}
		}
	}()
	return send()
}

// Platform member of a name, or the unknown platform error
func platformEnum(ctx context.Context, platformName string) (gateway.Platform, string) {
	p, err := gateway.ParsePlatform(ctx, platformName)
	if err != nil {
		return "", "Unknown platform: " + platformName
	}
	return p, ""
}

// Platform, its config and registry entry, or the error naming why the platform cannot send
func resolvePlatformConfig(ctx context.Context, platformName string, cfg *gateway.GatewayConfig) (gateway.Platform, *gateway.PlatformConfig, *gateway.PlatformEntry, string, error) {
	entry := gateway.Registry.Get(ctx, platformName)
	if entry == nil && !gateway.BuiltinPlatformValues[platformName] {
		return "", nil, nil, gateway.UnknownPlatformError(ctx, platformName), nil
	}
	platform, perr := platformEnum(ctx, platformName)
	if perr != "" {
		return "", nil, nil, perr, nil
	}
	pconfig := cfg.Platforms[platform]
	if pconfig == nil || !pconfig.Enabled {
		pconfig = nil
		if platformName == "weixin" {
			wx, err := weixinEnvPConfig(ctx)
			if err != nil {
				return "", nil, nil, "", err
			}
			pconfig = wx
		}
	}
	if pconfig == nil {
		msg, err := notConfiguredError(ctx, platformName, platform, entry)
		return "", nil, nil, msg, err
	}
	return platform, pconfig, entry, "", nil
}

// Not configured message naming the resolved home and the state of each credential source it consulted
func notConfiguredError(ctx context.Context, platformName string, platform gateway.Platform, entry *gateway.PlatformEntry) (string, error) {
	h := home.Dir(ctx)
	var envNames []string
	for _, row := range gateway.EnvEnableCredentials {
		if row.Platform == platform {
			envNames = append([]string(nil), row.Names...)
		}
	}
	if len(envNames) == 0 && entry != nil {
		envNames = append([]string(nil), entry.RequiredEnv...)
	}
	names := strings.Join(envNames, "/")
	if names == "" {
		names = "credentials"
	}
	envPath, configPath := filepath.Join(h, ".env"), filepath.Join(h, "config.yaml")
	dotenvKeys := config.LoadEnvFile(envPath)
	dotenvState := "missing"
	if _, err := os.Stat(envPath); err == nil {
		dotenvState = "no " + names
		for _, n := range envNames {
			if _, ok := dotenvKeys[n]; ok {
				dotenvState = names + " present"
				break
			}
		}
	}
	userConfig, block := map[string]any{}, any(nil)
	if uc, err := config.LoadUserEffective(configPath, os.LookupEnv); err == nil {
		if uc == nil {
			uc = map[string]any{}
		}
		platforms, has := uc["platforms"]
		if !has {
			platforms = map[string]any{}
		}
		if pm, ok := platforms.(map[string]any); ok {
			userConfig, block = uc, pm[platformName]
		}
	}
	exists, err := config.ConfigExists(ctx, configPath)
	if err != nil {
		return "", err
	}
	blockMap, isMap := block.(map[string]any)
	var configState string
	switch {
	case !exists:
		configState = "missing"
	case !isMap:
		configState = "no platforms." + platformName + " block"
	case blockMap["enabled"] == false:
		configState = "platforms." + platformName + ".enabled: false"
	default:
		configState = "platforms." + platformName + " has no token"
	}
	envState := names + " unset"
	for _, n := range envNames {
		if gateway.Getenv(ctx, n, "") != "" {
			envState = names + " set"
			break
		}
	}
	msg := fmt.Sprintf("Platform '%s' is not configured. Looked in: %s (%s), %s (%s), environment (%s), external secret sources (%s).",
		platformName, envPath, dotenvState, configPath, configState, envState, secretSourcesState(userConfig))
	return msg + liveGatewayHint(platformName, h, names, envPath, dotenvState), nil
}

// Hint about a running gateway holding the platform connected with credentials this shell cannot see
func liveGatewayHint(platformName, h, names, envPath, dotenvState string) (hint string) {
	defer func() {
		if r := recover(); r != nil {
			hint = ""
		}
	}()
	type gw struct {
		home   string
		record *pyjson.Object
		own    bool
	}
	root := home.Root()
	gateways := []gw{{h, gateway.ReadRuntimeStatus(""), true}}
	if home.Key(root) != home.Key(h) {
		gateways = append(gateways, gw{root, gateway.ReadRuntimeStatus(filepath.Join(root, "gateway_state.json")), false})
	}
	var b strings.Builder
	for _, g := range gateways {
		st, err := recordPlatformState(g.record, platformName)
		if err != nil {
			return b.String()
		}
		if st != "connected" || strings.Contains(dotenvState, "present") || !gateway.RuntimeStatusPidIsLive(g.record) {
			continue
		}
		pid, _ := g.record.Get("pid")
		fmt.Fprintf(&b, " A gateway (pid %s) running from %s has %s connected", pyrepr.Str(pid), g.home, platformName)
		if g.own {
			fmt.Fprintf(&b, ", so its credentials live only in that process's environment; add %s to %s.", names, envPath)
		} else {
			fmt.Fprintf(&b, "; this shell is scoped to profile home %s whose .env has no %s.", h, names)
		}
	}
	return b.String()
}

// State field of the platform in a runtime record, an error where the Python lookups raise
func recordPlatformState(record *pyjson.Object, platformName string) (any, error) {
	if record == nil {
		return nil, nil
	}
	platforms, _ := record.Get("platforms")
	if !pyrepr.Truthy(platforms) {
		return nil, nil
	}
	pobj, ok := platforms.(*pyjson.Object)
	if !ok {
		return nil, fmt.Errorf("'%s' object has no attribute 'get'", pyrepr.TypeName(platforms))
	}
	entry, has := pobj.Get(platformName)
	if !has {
		return nil, nil
	}
	eobj, ok := entry.(*pyjson.Object)
	if !ok || eobj == nil {
		return nil, fmt.Errorf("'%s' object has no attribute 'get'", pyrepr.TypeName(entry))
	}
	st, _ := eobj.Get("state")
	return st, nil
}

// Enabled or disabled state of each registered secret source with a section, names only
func secretSourcesState(userConfig map[string]any) string {
	secrets, _ := userConfig["secrets"].(map[string]any)
	var states []string
	for _, s := range provider.ListSecretSources("") {
		name := s.Spec().Name
		section, ok := secrets[name].(map[string]any)
		if !ok {
			continue
		}
		state := "disabled"
		if provider.SecretSourceEnabled(section) {
			state = "enabled"
		}
		states = append(states, name+": "+state)
	}
	if len(states) == 0 {
		return "none configured"
	}
	return strings.Join(states, ", ")
}

// Home chat id of the platform, Weixin also honouring WEIXIN_HOME_CHANNEL, or the actionable error
func homeChatID(ctx context.Context, cfg *gateway.GatewayConfig, platform gateway.Platform, platformName string) (string, string, error) {
	if h := cfg.GetHomeChannel(platform); h != nil {
		return h.ChatID, "", nil
	}
	if platformName == "weixin" {
		wx, _, err := provider.GetSecret(ctx, "WEIXIN_HOME_CHANNEL")
		if err != nil {
			return "", "", err
		}
		if wx = pystr.Strip(wx); wx != "" {
			return wx, "", nil
		}
	}
	homeEnv, ok := gateway.HomeChannelEnvOverrides[platformName]
	if !ok {
		homeEnv = pystr.Upper(platformName) + "_HOME_CHANNEL"
	}
	return "", fmt.Sprintf("No home channel set for %s to determine where to send the message. "+
		"Either specify a channel directly with '%s:CHANNEL_NAME', "+
		"or set a home channel via: icebath config set %s <channel_id>", platformName, platformName, homeEnv), nil
}

// Slack user targets opened as DM conversations, other ids passed through
func slackDMChatID(ctx context.Context, pconfig *gateway.PlatformConfig, chatID string) (string, map[string]any) {
	dmTarget := chatID
	if strings.HasPrefix(chatID, "U") {
		if ok, _ := slackUserIDRE.MatchString(chatID); ok {
			dmTarget = "user:" + chatID
		}
	}
	if !strings.HasPrefix(dmTarget, "user:") && !strings.HasPrefix(dmTarget, "user_name:") {
		return chatID, nil
	}
	return resolveSlackUserTarget(ctx, pconfig.Token, dmTarget)
}

// Mirrors the sent text into the target's gateway session, false on any failure
func mirrorSentMessage(ctx context.Context, platformName, chatID, mirrorText string, threadID *string) (ok bool) {
	defer func() {
		if r := recover(); r != nil {
			ok = false
		}
	}()
	env := tools.ToolEnvFrom(ctx)
	opts := gateway.MirrorOptions{ThreadID: threadID, SourceLabel: tools.GetSessionEnv(env, "ICEBATH_SESSION_PLATFORM", "cli")}
	if uid := tools.GetSessionEnv(env, "ICEBATH_SESSION_USER_ID", ""); uid != "" {
		opts.UserID = &uid
	}
	return mirrorToSession(ctx, platformName, chatID, mirrorText, opts)
}

// Weixin config synthesized from env secrets, nil without both token and account
func weixinEnvPConfig(ctx context.Context) (*gateway.PlatformConfig, error) {
	secret := func(name string) (string, error) {
		v, _, err := provider.GetSecret(ctx, name)
		return pystr.Strip(v), err
	}
	token, err := secret("WEIXIN_TOKEN")
	if err != nil {
		return nil, err
	}
	account, err := secret("WEIXIN_ACCOUNT_ID")
	if err != nil {
		return nil, err
	}
	if token == "" || account == "" {
		return nil, nil
	}
	base, err := secret("WEIXIN_BASE_URL")
	if err != nil {
		return nil, err
	}
	cdn, err := secret("WEIXIN_CDN_BASE_URL")
	if err != nil {
		return nil, err
	}
	pc := gateway.NewPlatformConfig()
	pc.Enabled, pc.Token = true, token
	pc.Extra = map[string]any{"account_id": account, "base_url": base, "cdn_base_url": cdn}
	return pc, nil
}

// Mirror summary of a media only message
func describeMediaForMirror(media []gateway.MediaRef) string {
	if len(media) == 0 {
		return ""
	}
	if len(media) != 1 {
		return fmt.Sprintf("[Sent %d media attachments]", len(media))
	}
	ext := lowerExt(media[0].Path)
	if media[0].IsVoice && voiceExts[ext] {
		return "[Sent voice message]"
	}
	kind := "document"
	switch {
	case imageExts[ext]:
		kind = "image"
	case videoExts[ext]:
		kind = "video"
	case audioExts[ext]:
		kind = "audio"
	}
	return "[Sent " + kind + " attachment]"
}

// Skip payload when the cron job already auto delivers its final response to the same target
func maybeSkipCronDuplicateSend(ctx context.Context, platformName, chatID string, threadID *string) map[string]any {
	env := tools.ToolEnvFrom(ctx)
	autoPlatform := pystr.Lower(pystr.Strip(tools.GetSessionEnv(env, "ICEBATH_CRON_AUTO_DELIVER_PLATFORM", "")))
	autoChat := pystr.Strip(tools.GetSessionEnv(env, "ICEBATH_CRON_AUTO_DELIVER_CHAT_ID", ""))
	autoThread := pystr.Strip(tools.GetSessionEnv(env, "ICEBATH_CRON_AUTO_DELIVER_THREAD_ID", ""))
	sameThread := (autoThread == "" && threadID == nil) || (threadID != nil && autoThread != "" && *threadID == autoThread)
	if autoPlatform == "" || autoChat == "" || autoPlatform != platformName || autoChat != chatID || !sameThread {
		return nil
	}
	label := platformName + ":" + chatID
	if threadID != nil {
		label += ":" + *threadID
	}
	return map[string]any{"success": true, "skipped": true, "reason": "cron_auto_delivery_duplicate_target", "target": label,
		"note": fmt.Sprintf("Skipped send_message to %s. This cron job will already auto-deliver "+
			"its final response to that same target. Put the intended user-facing content in "+
			"your final response instead, or use a different target if you want an additional message.", label)}
}

// Prints a send result as the send command does and returns its exit code
func EmitSendResult(stdout, stderr io.Writer, resultJSON string, jsonMode, quiet bool) (int, error) {
	var payload any = pyjson.NewObject()
	if resultJSON != "" {
		v, err := pyjson.Loads(resultJSON)
		if err != nil {
			v = pyjson.ObjectOf("error", "invalid JSON from send_message_tool", "raw", resultJSON)
		}
		payload = v
	}
	obj, ok := payload.(*pyjson.Object)
	if jsonMode {
		text, err := pyjson.Dumps(payload, pyjson.Indent(2))
		if err != nil {
			return 1, err
		}
		fmt.Fprintln(stdout, text)
	}
	if !ok || obj == nil {
		return 1, fmt.Errorf("'%s' object has no attribute 'get'", pyrepr.TypeName(payload))
	}
	get := func(k string) any { v, _ := obj.Get(k); return v }
	if !jsonMode && !quiet {
		switch {
		case pyrepr.Truthy(get("error")):
			fmt.Fprintf(stderr, "icebath send: %s\n", pyrepr.Str(get("error")))
		case pyrepr.Truthy(get("success")):
			note := get("note")
			if !pyrepr.Truthy(note) {
				note = "sent"
			}
			fmt.Fprintln(stdout, pyrepr.Str(note))
		default:
			text, err := pyjson.Dumps(payload, pyjson.Indent(2))
			if err != nil {
				return 1, err
			}
			fmt.Fprintln(stdout, text)
		}
	}
	if !pyrepr.Truthy(get("error")) && (pyrepr.Truthy(get("skipped")) || pyrepr.Truthy(get("success"))) {
		return 0, nil
	}
	return 1, nil
}
