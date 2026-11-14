package weixin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/plugins"
)

// Sends the formatted text then each media file through an adapter and wraps the outcome
func deliverDirect(ctx context.Context, adapter *WeixinAdapter, chatID, message string, mediaFiles []gateway.MediaRef, contextToken string) (map[string]any, error) {
	var lastResult *gateway.SendResult
	if cleaned := adapter.FormatMessage(message); cleaned != "" {
		result, err := adapter.Send(ctx, chatID, cleaned, "", nil)
		if err != nil {
			return nil, err
		}
		if !result.Success {
			return map[string]any{"error": "Weixin send failed: " + pyrepr.Str(result.Error)}, nil
		}
		lastResult = &result
	}
	for _, media := range mediaFiles {
		var result gateway.SendResult
		var err error
		if directImageExts[pystr.Lower(pypath.Suffix(media.Path))] {
			result, err = adapter.SendImageFile(ctx, chatID, media.Path, "", "", nil, nil)
		} else {
			result, err = adapter.SendDocument(ctx, chatID, media.Path, "", "", "", nil, nil)
		}
		if err != nil {
			return nil, err
		}
		if !result.Success {
			return map[string]any{"error": "Weixin media send failed: " + pyrepr.Str(result.Error)}, nil
		}
		lastResult = &result
	}
	var messageID any
	if lastResult != nil {
		messageID = lastResult.MessageID
	}
	return map[string]any{"success": true, "platform": "weixin", "chat_id": chatID, "message_id": messageID, "context_token_used": contextToken != ""}, nil
}

// One shot send for send_message and cron, through the live adapter's session when one is open and a throwaway adapter otherwise
func sendWeixinDirect(ctx context.Context, extra map[string]any, token any, chatID, message string, mediaFiles []gateway.MediaRef) (map[string]any, error) {
	accountID, err := extraOrSecret(ctx, extra, "account_id", "")
	if err != nil {
		return nil, err
	}
	baseURL, err := extraOrSecret(ctx, extra, "base_url", ilinkBaseURL)
	if err != nil {
		return nil, err
	}
	cdnBaseURL, err := extraOrSecret(ctx, extra, "cdn_base_url", weixinCDNBaseURL)
	if err != nil {
		return nil, err
	}
	baseURL, cdnBaseURL = pystr.RstripChars(baseURL, "/"), pystr.RstripChars(cdnBaseURL, "/")
	tokenRaw := pyrepr.Or(token, extra["token"])
	if !pyrepr.Truthy(tokenRaw) {
		if tokenRaw, err = gateway.GetScopedSecret(ctx, "WEIXIN_TOKEN", "", false); err != nil {
			return nil, err
		}
	}
	resolvedToken := pystr.Strip(pyrepr.Str(tokenRaw))
	if resolvedToken == "" {
		return map[string]any{"error": "Weixin token missing. Configure WEIXIN_TOKEN or platforms.weixin.token."}, nil
	}
	if accountID == "" {
		return map[string]any{"error": "Weixin account ID missing. Configure WEIXIN_ACCOUNT_ID or platforms.weixin.extra.account_id."}, nil
	}
	tokenStore, err := newContextTokenStore(home.Dir(ctx))
	if err != nil {
		return nil, err
	}
	if err := tokenStore.restore(accountID); err != nil {
		return nil, err
	}
	contextToken := tokenStore.get(accountID, chatID)
	liveAdapters.Lock()
	live := liveAdapters.byToken[resolvedToken]
	liveAdapters.Unlock()
	if live != nil {
		if _, send := live.sessions(); send != nil && !send.isClosed() {
			return deliverDirect(ctx, live, chatID, message, mediaFiles, contextToken)
		}
	}
	session := newSession(ctx)
	defer session.close()
	merged := map[string]any{}
	for k, v := range extra {
		merged[k] = v
	}
	merged["account_id"], merged["base_url"], merged["cdn_base_url"] = accountID, baseURL, cdnBaseURL
	cfg := gateway.NewPlatformConfig()
	cfg.Enabled, cfg.Token, cfg.Extra = true, resolvedToken, merged
	adapter, err := NewWeixinAdapter(ctx, cfg)
	if err != nil {
		return nil, err
	}
	adapter.mu.Lock()
	adapter.sendSession = session
	adapter.mu.Unlock()
	adapter.tokenStore = tokenStore
	return deliverDirect(ctx, adapter, chatID, message, mediaFiles, contextToken)
}

const installHint = "Weixin: aiohttp/cryptography not installed"

// Account id and token both present in the home env file
var envConfigured = gateway.EnvIsConnected("WEIXIN_ACCOUNT_ID", "WEIXIN_TOKEN")

// Account id and token both present in the merged platform config, a token alone not counting
func configConnected(cfg *gateway.PlatformConfig) bool {
	return pyrepr.Truthy(cfg.Extra["account_id"]) && (pyrepr.Truthy(cfg.Token) || pyrepr.Truthy(cfg.Extra["token"]))
}

// Account id and token from the config or the scoped WEIXIN env, else from the home env file
func isConnected(ctx context.Context, cfg *gateway.PlatformConfig) (bool, error) {
	extra := map[string]any{}
	var token any
	if cfg != nil {
		token = cfg.Token
		if cfg.Extra != nil {
			extra = cfg.Extra
		}
	}
	account, err := extraOrSecret(ctx, extra, "account_id", "")
	if err != nil {
		return false, err
	}
	tokenRaw := pyrepr.Or(token, extra["token"])
	if !pyrepr.Truthy(tokenRaw) {
		if tokenRaw, err = gateway.GetScopedSecret(ctx, "WEIXIN_TOKEN", "", false); err != nil {
			return false, err
		}
	}
	if account != "" && pystr.Strip(pyrepr.Str(tokenRaw)) != "" {
		return true, nil
	}
	return envConfigured(ctx, cfg)
}

// Out of process sender of send_message and cron with native text and media
func standaloneSend(ctx context.Context, kwargs map[string]any) (map[string]any, error) {
	pconfig, _ := kwargs["pconfig"].(*gateway.PlatformConfig)
	chatID, _ := kwargs["chat_id"].(string)
	message, _ := kwargs["message"].(string)
	mediaFiles, _ := kwargs["media_files"].([]gateway.MediaRef)
	extra := map[string]any{}
	var token any
	if pconfig != nil {
		token = pconfig.Token
		if pconfig.Extra != nil {
			extra = pconfig.Extra
		}
	}
	result, err := sendWeixinDirect(ctx, extra, token, chatID, message, mediaFiles)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		slog.Debug("Weixin standalone send failed", "error", err)
		return gateway.SendError("Weixin send failed: " + errText(err)), nil
	}
	return result, nil
}

// Policy, allow all flag, print level and message per DM menu index, index two prompting an allowlist instead
var weixinDMPolicies = map[int][4]string{
	0: {"pairing", "false", "success", "  DM pairing enabled."},
	1: {"open", "true", "warning", "  Open DM access enabled for Weixin."},
	3: {"disabled", "false", "warning", "  Direct messages disabled."},
}

var weixinGroupNote = []string{
	"  Note: QR login connects an iLink bot identity (e.g. ...@im.bot), not a",
	"  scriptable personal WeChat account. Ordinary WeChat groups typically cannot",
	"  invite an @im.bot identity, and iLink does not deliver ordinary-group events",
	"  to most bot accounts. The settings below only apply when iLink actually",
	"  delivers group events for your account type — otherwise DM remains the only",
	"  working channel regardless of this choice.",
}

// QR login the wizard runs, a seam tests replace
var qrLoginFn = qrLogin

// Prints each line through the wizard info printer
func printInfoLines(lines ...string) {
	for _, line := range lines {
		gateway.WizardSetup.PrintInfo(line)
	}
}

// Weixin flow of icebath gateway setup, the iLink QR login then the DM policy, group policy and home channel
func InteractiveSetup(ctx context.Context) error {
	w, wizard := gateway.WizardSetup, gateway.WizardGateway
	wizard.PrintSetupHeader("💬 Weixin / WeChat")
	gateway.WizardPrintln("")
	printInfoLines("  1. Icebath will open Tencent iLink QR login in this terminal.",
		"  2. Use WeChat to scan and confirm the QR code.",
		"  3. Icebath will store the returned account_id/token in ~/.icebath/.env.",
		"  4. This adapter supports native text, image, video, and document delivery.")
	proceed, err := wizard.ConfirmReconfigure(ctx, "Weixin", "WEIXIN_ACCOUNT_ID", "WEIXIN_TOKEN")
	if err != nil || !proceed {
		return err
	}
	gateway.WizardPrintln("")
	if !w.PromptYesNo("  Start QR login now?", true) {
		w.PrintInfo("  Cancelled.")
		return nil
	}
	watch := gateway.WatchInterrupt()
	loginCtx, cancel := context.WithCancel(ctx)
	finished := make(chan struct{})
	go func() {
		for {
			if watch.Sleep(0.2) != nil {
				cancel()
				return
			}
			select {
			case <-finished:
				return
			default:
			}
		}
	}()
	credentials, err := qrLoginFn(loginCtx, w.HermesHome(ctx), "3", 480)
	wasInterrupted := loginCtx.Err() != nil && ctx.Err() == nil
	close(finished)
	cancel()
	watch.Stop()
	switch {
	case wasInterrupted || errors.Is(err, gateway.ErrKeyboardInterrupt):
		gateway.WizardPrintln("")
		w.PrintWarning("  Weixin setup cancelled.")
		return nil
	case err != nil && ctx.Err() != nil:
		return ctx.Err()
	case err != nil:
		w.PrintError("  QR login failed: " + errText(err))
		return nil
	case credentials == nil:
		w.PrintWarning("  QR login did not complete.")
		return nil
	}
	accountID, userID := credentials.accountID, credentials.userID
	if err := w.SaveEnvValue(ctx, "WEIXIN_ACCOUNT_ID", accountID); err != nil {
		return err
	}
	if err := w.SaveEnvValue(ctx, "WEIXIN_TOKEN", credentials.token); err != nil {
		return err
	}
	if credentials.baseURL != "" {
		if err := w.SaveEnvValue(ctx, "WEIXIN_BASE_URL", credentials.baseURL); err != nil {
			return err
		}
	}
	cdnBaseURL, err := w.GetEnvValue(ctx, "WEIXIN_CDN_BASE_URL")
	if err != nil {
		return err
	}
	if cdnBaseURL == "" {
		cdnBaseURL = weixinCDNBaseURL
	}
	if err := w.SaveEnvValue(ctx, "WEIXIN_CDN_BASE_URL", cdnBaseURL); err != nil {
		return err
	}
	gateway.WizardPrintln("")
	accessIdx := w.PromptChoice("  How should direct messages be authorized?", []string{"Use DM pairing approval (recommended)",
		"Allow all direct messages", "Only allow listed user IDs", "Disable direct messages"}, 0)
	if accessIdx == 2 {
		allowlist := wizard.PromptCSV("  Allowed Weixin user IDs (comma-separated)", userID)
		if err := wizard.SaveEnvValues(ctx, gateway.EnvAssignment{Name: "WEIXIN_DM_POLICY", Value: "allowlist"},
			gateway.EnvAssignment{Name: "WEIXIN_ALLOW_ALL_USERS", Value: "false"}, gateway.EnvAssignment{Name: "WEIXIN_ALLOWED_USERS", Value: allowlist}); err != nil {
			return err
		}
		w.PrintSuccess("  Weixin allowlist saved.")
	} else {
		policy, known := weixinDMPolicies[accessIdx]
		if !known {
			policy = weixinDMPolicies[3]
		}
		if err := wizard.SaveEnvValues(ctx, gateway.EnvAssignment{Name: "WEIXIN_DM_POLICY", Value: policy[0]},
			gateway.EnvAssignment{Name: "WEIXIN_ALLOW_ALL_USERS", Value: policy[1]}, gateway.EnvAssignment{Name: "WEIXIN_ALLOWED_USERS", Value: ""}); err != nil {
			return err
		}
		if policy[2] == "success" {
			w.PrintSuccess(policy[3])
		} else {
			w.PrintWarning(policy[3])
		}
		if accessIdx == 0 {
			w.PrintInfo("  Unknown DM users can request access and you approve them with `icebath pairing approve`.")
		}
	}
	gateway.WizardPrintln("")
	printInfoLines(weixinGroupNote...)
	groupIdx := w.PromptChoice("  How should group chats be handled?", []string{"Disable group chats (recommended)", "Allow all group chats",
		"Only allow listed group chat IDs"}, 0)
	groupPolicy, groupAllow := "allowlist", ""
	switch groupIdx {
	case 0:
		groupPolicy = "disabled"
	case 1:
		groupPolicy = "open"
	default:
		groupAllow = wizard.PromptCSV("  Allowed group chat IDs (comma-separated, not member user IDs)", "")
	}
	if err := wizard.SaveEnvValues(ctx, gateway.EnvAssignment{Name: "WEIXIN_GROUP_POLICY", Value: groupPolicy},
		gateway.EnvAssignment{Name: "WEIXIN_GROUP_ALLOWED_USERS", Value: groupAllow}); err != nil {
		return err
	}
	switch groupIdx {
	case 0:
		w.PrintInfo("  Group chats disabled.")
	case 1:
		w.PrintWarning("  All group chats enabled (only takes effect if iLink delivers group events).")
	default:
		w.PrintSuccess("  Group allowlist saved (only takes effect if iLink delivers group events).")
	}
	if userID != "" {
		gateway.WizardPrintln("")
		if err := wizard.OfferHomeChannel(ctx, "WEIXIN_HOME_CHANNEL", userID, "your Weixin user ID"); err != nil {
			return err
		}
	}
	gateway.WizardPrintln("")
	w.PrintSuccess("Weixin configured!")
	w.PrintInfo("  Account ID: " + accountID)
	if userID != "" {
		w.PrintInfo("  User ID: " + userID)
	}
	return nil
}

// System prompt platform hint, cached prompt bytes kept stable
const platformHint = "You are on Weixin/WeChat. Markdown formatting is supported, so you may use it when it improves readability, " +
	"but keep the message compact and chat-friendly. You can send media files natively: include " +
	"MEDIA:/absolute/path/to/file in your response. Images are sent as native photos, videos play inline when " +
	"supported, and other files arrive as downloadable documents. You can also include image URLs in markdown " +
	"format ![alt](url) and they will be downloaded and sent as native media when possible."

// Builds the adapter for a platform config, the factory the registry and the adapter class share
func newAdapter(ctx context.Context, cfg *gateway.PlatformConfig) (gateway.PlatformAdapter, error) {
	adapter, err := NewWeixinAdapter(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return adapter, nil
}

// Weixin platform plugin compiled into the binary under the bundled platforms/weixin key
type weixinPlatformPlugin struct{}

func (weixinPlatformPlugin) Manifest() plugins.Manifest {
	return plugins.Manifest{Name: "weixin-platform", Version: "1.0.0", Kind: "platform", Source: "bundled", Key: "platforms/weixin",
		Author: "NousResearch",
		Description: "Weixin (personal WeChat) gateway adapter for Icebath Agent over Tencent's iLink Bot API. Long-polls getupdates for inbound " +
			"messages, echoes each peer's context_token on replies, and moves media through the AES-128-ECB encrypted CDN. " +
			"`icebath gateway setup` performs the iLink QR login.\n",
		RequiresEnv: []plugins.EnvSpec{
			{Name: "WEIXIN_ACCOUNT_ID", Description: "iLink Bot account ID obtained through QR login in icebath gateway setup"},
			{Name: "WEIXIN_TOKEN", Description: "iLink Bot token obtained through QR login in icebath gateway setup", Secret: true}}}
}

// Registers the Weixin platform entry with its factory, checks, setup, target syntax and standalone sender
func (weixinPlatformPlugin) Register(ctx *plugins.Context) error {
	extra := map[string]any{
		"target_ref_syntax_fn":   gateway.RegexGroupsParser(sendTargetRE, false),
		"standalone_send":        &gateway.StandaloneSendPolicy{Label: "Weixin", Media: true},
		"dashboard_label":        "Weixin / WeChat (Personal)",
		"dashboard_description":  "Connect a personal WeChat account through Tencent's iLink Bot API.",
		"docs_url":               "https://hermes-agent.nousresearch.com/docs/user-guide/messaging/weixin/",
		"dashboard_env_vars":     []string{"WEIXIN_ACCOUNT_ID", "WEIXIN_TOKEN", "WEIXIN_BASE_URL"},
		"is_connected":           isConnected,
		"config_connected_fn":    configConnected,
		"setup_fn":               InteractiveSetup,
		"cron_deliver_env_var":   "WEIXIN_HOME_CHANNEL",
		"standalone_sender_fn":   standaloneSend,
		"max_message_length":     0,
		"emoji":                  "💬",
		"allow_update_command":   true,
		"platform_hint":          platformHint,
		"display_tier":           "low",
		"env_overrides":          envOverrides,
		"env_enable_credentials": envEnableCredentials,
	}
	for k, v := range authz {
		extra[k] = v
	}
	_, err := ctx.RegisterPlatform(plugins.PlatformSpec{
		Name: "weixin", Label: "Weixin / WeChat",
		AdapterFactory: gateway.AdapterFactory(func(fctx context.Context, cfg *gateway.PlatformConfig) (any, error) { return newAdapter(fctx, cfg) }),
		Check:          checkWeixinRequirements,
		RequiredEnv:    []string{"WEIXIN_ACCOUNT_ID", "WEIXIN_TOKEN"},
		InstallHint:    installHint,
		Extra:          extra,
	})
	if err != nil {
		return fmt.Errorf("weixin: %w", err)
	}
	return nil
}

var _ gateway.PlatformAdapter = (*WeixinAdapter)(nil)

func init() {
	gateway.RegisterAdapterModule(&gateway.AdapterClass{Name: "WeixinAdapter", New: newAdapter, Traits: adapterTraits()})
	plugins.RegisterBundled(weixinPlatformPlugin{})
}
