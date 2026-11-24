package googlechat

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"time"

	"github.com/spf13/cobra"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/plugins"
	textutil "github.com/nickheyer/icebath/pkg/utils/text"
)

// Whether the config carries an HTTP events URL or a project and subscription
func validateConfig(ctx context.Context, cfg *gateway.PlatformConfig) (bool, error) {
	if cfg == nil || cfg.Extra == nil {
		return false, nil
	}
	extra := cfg.Extra
	return pyrepr.Truthy(extra["http_events_url"]) || (pyrepr.Truthy(extra["project_id"]) && pyrepr.Truthy(extra["subscription_name"])), nil
}

// Whether the platform is enabled and its config is complete
func isConnected(ctx context.Context, cfg *gateway.PlatformConfig) (bool, error) {
	if cfg == nil || !cfg.Enabled {
		return false, nil
	}
	return validateConfig(ctx, cfg)
}

// First non empty scoped secret of the names
func scopedFirst(ctx context.Context, names ...string) string {
	for _, name := range names {
		if v, err := gateway.GetScopedSecret(ctx, name, "", false); err == nil && v != "" {
			return v
		}
	}
	return ""
}

// Project, subscription and HTTP events URL from the scoped env with their alternate names
func envInboundSettings(ctx context.Context) (string, string, string) {
	return scopedFirst(ctx, "GOOGLE_CHAT_PROJECT_ID", "GOOGLE_CLOUD_PROJECT"),
		scopedFirst(ctx, "GOOGLE_CHAT_SUBSCRIPTION_NAME", "GOOGLE_CHAT_SUBSCRIPTION"),
		scopedFirst(ctx, "GOOGLE_CHAT_HTTP_EVENTS_URL")
}

// Whether the env carries the minimum inbound settings so an unconfigured profile never auto enables it
func checkRequirements(ctx context.Context) bool {
	project, subscription, httpURL := envInboundSettings(ctx)
	return httpURL != "" || (project != "" && subscription != "")
}

// Extra seeded from the profile env so env only setups show in gateway status, nil without inbound settings
func envEnablement(ctx context.Context) map[string]any {
	if !checkRequirements(ctx) {
		return nil
	}
	project, subscription, httpURL := envInboundSettings(ctx)
	seed := map[string]any{}
	for _, kv := range [][2]string{{"project_id", project}, {"subscription_name", subscription}, {"http_events_url", httpURL},
		{"service_account_json", scopedFirst(ctx, "GOOGLE_CHAT_SERVICE_ACCOUNT_JSON", "GOOGLE_APPLICATION_CREDENTIALS")}} {
		if kv[1] != "" {
			seed[kv[0]] = kv[1]
		}
	}
	extra, err := gateway.SeedExtraFromEnv(ctx, []gateway.EnvSeedRow{
		{Env: "GOOGLE_CHAT_HTTP_EVENTS_AUDIENCE", Key: "http_events_audience"},
		{Env: "GOOGLE_CHAT_HTTP_EVENTS_SERVICE_ACCOUNT_EMAIL", Key: "http_events_service_account_email"},
	}, "GOOGLE_CHAT_HOME_CHANNEL", "")
	if err != nil {
		panic(err)
	}
	for k, v := range extra {
		seed[k] = v
	}
	return seed
}

// Resource names Chat documents, anything else being a tampered id breaking out of the REST path
var (
	chatIDRE   = regexp.MustCompile(`^(?:spaces|users)/[A-Za-z0-9_-]+$`)
	threadIDRE = regexp.MustCompile(`^spaces/[A-Za-z0-9_-]+/threads/[A-Za-z0-9_-]+$`)
)

// Standalone send failure with its detail
func standaloneError(detail string) map[string]any {
	return gateway.SendError("Google Chat standalone send: " + detail)
}

// Posts one message through the REST API without a live adapter, for cron and the send message tool
func standaloneSend(ctx context.Context, kwargs map[string]any) (map[string]any, error) {
	pconfig, _ := kwargs["pconfig"].(*gateway.PlatformConfig)
	chatID, _ := kwargs["chat_id"].(string)
	message, _ := kwargs["message"].(string)
	threadID, hasThread := "", false
	if v := kwargs["thread_id"]; v != nil {
		threadID, hasThread = pyrepr.Str(v), true
	}
	if chatID == "" {
		return standaloneError("chat_id (space resource) is required"), nil
	}
	if !chatIDRE.MatchString(chatID) {
		return standaloneError(fmt.Sprintf("chat_id %s must match 'spaces/<id>' or 'users/<id>' with only [A-Za-z0-9_-] in the id", pyrepr.Repr(chatID))), nil
	}
	if hasThread && !threadIDRE.MatchString(threadID) {
		return standaloneError(fmt.Sprintf("thread_id %s must match 'spaces/<id>/threads/<id>'", pyrepr.Repr(threadID))), nil
	}
	saValue := ""
	if pconfig != nil && pyrepr.Truthy(pconfig.Extra["service_account_json"]) {
		saValue = pyrepr.Str(pconfig.Extra["service_account_json"])
	} else {
		saValue = scopedFirst(ctx, "GOOGLE_CHAT_SERVICE_ACCOUNT_JSON", "GOOGLE_APPLICATION_CREDENTIALS")
	}
	creds, err := loadSACredentialsFrom(ctx, saValue)
	var classified *saCredentialError
	if errors.As(err, &classified) {
		return standaloneError(saErrorText(standaloneSAErrors, classified, saValue)), nil
	}
	if err != nil {
		return standaloneError(fmt.Sprintf("credential load failed: %v", err)), nil
	}
	refreshCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	token, err := creds.Token(refreshCtx)
	cancel()
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return standaloneError("token refresh timed out"), nil
		}
		return standaloneError("token refresh failed: " + excText(err)), nil
	}
	if token == "" {
		return standaloneError("refreshed credentials have no token"), nil
	}
	api := newRESTClient(creds)
	api.http.Timeout = 30 * time.Second
	if transport, ok := api.http.Transport.(*http.Transport); ok && !gateway.GatewayTrustEnv(ctx) {
		transport.Proxy = nil
	}
	resp, err := api.createMessage(ctx, chatID, threadBody(message, threadID))
	if err != nil {
		var h *httpError
		if errors.As(err, &h) {
			return standaloneError(fmt.Sprintf("API returned %d: %s", h.status, textutil.First(h.reason, 300))), nil
		}
		return gateway.SendError("Google Chat standalone send failed: " + gateway.ProxySafeErrorText(err, gateway.TrustedEnvProxyForRedaction(ctx, "https://chat.googleapis.com"))), nil
	}
	return map[string]any{"success": true, "message_id": resp["name"]}, nil
}

// Model facing description of Google Chat formatting and limits
const platformHint = "You are on Google Chat. Limited markdown subset is rendered: *bold*, _italic_, ~strike~, `code`. " +
	"No headings or lists. Message size limit: 4000 characters; longer responses are split across multiple " +
	"messages. You are in a space (DM or group). Images render inline; audio, video, and document attachments " +
	"render as download cards (no native voice/video UI). To send files, include MEDIA:/absolute/path/to/file " +
	"in your response. Native file attachments require the user to run /setup-files once in their own DM — " +
	"until they do, file requests fall back to a text notice with the host path. Command approvals and clarify " +
	"questions with choices reach the user as Card v2 buttons they click; your own replies are plain text, so do " +
	"not write card markup or button syntax. While you are generating a response, an 'Icebath is thinking…' marker message " +
	"appears in the space and is replaced by your response once it is ready. You do NOT have access to Google " +
	"Chat-specific APIs — you cannot search space history, list space members, or manage spaces. Do not " +
	"promise to perform these actions; explain that you can only read messages sent directly to you and " +
	"respond in the same space/thread."

// Binds the user OAuth helper flags onto the google_chat command
func setupOAuthCLI(parser any) {
	cmd, ok := parser.(*cobra.Command)
	if !ok {
		panic(fmt.Sprintf("the CLI parser is %T, expected an argument parser", parser))
	}
	oauth := &cobra.Command{Use: "oauth", Short: "User OAuth for native attachment delivery (the /setup-files host side)",
		Args: cobra.NoArgs, SilenceUsage: true}
	var check, authURL, revoke bool
	var clientSecret, authCode, email string
	f := oauth.Flags()
	f.BoolVar(&check, "check", false, "Check if auth is valid (exit 0=yes, 1=no)")
	f.StringVar(&clientSecret, "client-secret", "", "Store OAuth client_secret.json")
	f.BoolVar(&authURL, "auth-url", false, "Print OAuth URL for user to visit")
	f.StringVar(&authCode, "auth-code", "", "Exchange auth code for token")
	f.BoolVar(&revoke, "revoke", false, "Revoke and delete stored token")
	f.StringVar(&email, "email", "", "Scope operation to a specific user's token (default: legacy single-user path)")
	oauth.MarkFlagsMutuallyExclusive("check", "client-secret", "auth-url", "auth-code", "revoke")
	oauth.MarkFlagsOneRequired("check", "client-secret", "auth-url", "auth-code", "revoke")
	oauth.RunE = func(c *cobra.Command, args []string) error {
		ctx := c.Context()
		if ctx == nil {
			ctx = context.Background()
		}
		out := c.OutOrStdout()
		var text string
		var err error
		switch {
		case check:
			line, ok := checkAuth(ctx, email)
			fmt.Fprintln(out, line)
			if !ok {
				return silentFailure{}
			}
			return nil
		case clientSecret != "":
			text, err = storeClientSecret(ctx, clientSecret)
		case authURL:
			text, err = getAuthURL(ctx, email)
		case authCode != "":
			text, err = exchangeAuthCode(ctx, authCode, email)
		case revoke:
			text, err = revokeUserToken(ctx, email)
		}
		return printOAuthResult(out, text, err)
	}
	cmd.AddCommand(oauth)
	cmd.RunE = func(c *cobra.Command, args []string) error { return c.Help() }
}

// Failure exit whose lines were already printed, so the CLI exits 1 without a stderr message
type silentFailure struct{}

func (silentFailure) Error() string { return "" }

// Prints a helper result, the failure lines with exit status 1 when it failed
func printOAuthResult(out io.Writer, text string, err error) error {
	var failure *oauthFailure
	if errors.As(err, &failure) {
		for _, line := range failure.lines {
			fmt.Fprintln(out, line)
		}
		return silentFailure{}
	}
	if err != nil {
		return err
	}
	fmt.Fprintln(out, text)
	return nil
}

// Google Chat platform plugin compiled into the binary under the bundled platforms/google_chat key
type googleChatPlatformPlugin struct{}

func (googleChatPlatformPlugin) Manifest() plugins.Manifest {
	return plugins.Manifest{Name: "google_chat-platform", Version: "1.0.0", Kind: "platform", Source: "bundled", Key: "platforms/google_chat",
		Description: "Google Chat gateway adapter for Icebath Agent: Cloud Pub/Sub pull or authenticated HTTP callbacks in, the Chat REST API out.\n",
		RequiresEnv: []plugins.EnvSpec{{Name: "GOOGLE_CHAT_SERVICE_ACCOUNT_JSON", Description: "Path to the Service Account JSON (or inline JSON)", Secret: true}}}
}

// Adapter class the gateway constructs Google Chat adapters through
var googleChatAdapterClass = &gateway.AdapterClass{Name: "GoogleChatAdapter", Traits: googleChatAdapterTraits(),
	New: func(ctx context.Context, cfg *gateway.PlatformConfig) (gateway.PlatformAdapter, error) {
		return NewGoogleChatAdapter(ctx, cfg)
	}}

// Registers the Google Chat platform entry and the user OAuth helper command
func (googleChatPlatformPlugin) Register(ctx *plugins.Context) error {
	_, err := ctx.RegisterPlatform(plugins.PlatformSpec{
		Name: "google_chat", Label: "Google Chat",
		AdapterFactory: gateway.AdapterFactory(func(fctx context.Context, cfg *gateway.PlatformConfig) (any, error) {
			return NewGoogleChatAdapter(fctx, cfg)
		}),
		Check:       func() bool { return checkRequirements(context.Background()) },
		RequiredEnv: []string{"GOOGLE_CHAT_SERVICE_ACCOUNT_JSON"},
		InstallHint: "No extra packages needed; set GOOGLE_CHAT_SUBSCRIPTION_NAME (or GOOGLE_CHAT_HTTP_EVENTS_URL) and GOOGLE_CHAT_SERVICE_ACCOUNT_JSON",
		Extra: map[string]any{
			"scoped_check_fn":        checkRequirements,
			"scoped_validate_config": validateConfig,
			"is_connected":           isConnected,
			"setup_fn":               InteractiveSetup,
			"env_enablement_fn":      envEnablement,
			"cron_deliver_env_var":   "GOOGLE_CHAT_HOME_CHANNEL",
			"standalone_sender_fn":   standaloneSend,
			"allowed_users_env":      "GOOGLE_CHAT_ALLOWED_USERS",
			"allow_all_env":          "GOOGLE_CHAT_ALLOW_ALL_USERS",
			"max_message_length":     maxTextLength,
			"emoji":                  "💬",
			"allow_update_command":   true,
			"platform_hint":          platformHint,
		},
	})
	if err != nil {
		return err
	}
	ctx.RegisterCLICommand("google_chat", "Google Chat helpers (user OAuth for native attachments)", setupOAuthCLI, nil,
		"Google Chat host side setup: store the OAuth client secret and manage per user tokens for /setup-files.")
	return nil
}

func init() {
	gateway.RegisterAdapterModule(googleChatAdapterClass)
	plugins.RegisterBundled(googleChatPlatformPlugin{})
}
