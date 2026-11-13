package teams

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/plugins"
)

// Whether the config resolves the client id, client secret and tenant id
func validateConfig(ctx context.Context, config *gateway.PlatformConfig) (bool, error) {
	id, secret, tenant, err := credentials(ctx, config)
	if err != nil {
		return false, err
	}
	return id != "" && secret != "" && tenant != "", nil
}

// Python int of an env string, the ValueError int raises for a bad literal
func intConv(raw string) (any, error) {
	n, err := pyint.Int64(raw)
	if pe, ok := err.(*pyint.Error); ok {
		return nil, &gateway.PyError{Kind: pe.Kind, Msg: pe.Msg}
	}
	return n, err
}

// Env var, extra key and conversion of every Teams value seeded from an env only setup
var envSeedKeys = []gateway.EnvSeedRow{
	{Env: "TEAMS_CLIENT_ID", Key: "client_id"}, {Env: "TEAMS_CLIENT_SECRET", Key: "client_secret"},
	{Env: "TEAMS_TENANT_ID", Key: "tenant_id"}, {Env: "TEAMS_PORT", Key: "port", Conv: intConv},
	{Env: "TEAMS_SERVICE_URL", Key: "service_url"},
}

// Extra seeded from the profile env, nil unless identity and secret are set, a scope read failure raising
func envEnablement(ctx context.Context) map[string]any {
	seed, err := gateway.SeedExtraFromEnv(ctx, envSeedKeys, "TEAMS_HOME_CHANNEL", "")
	if err != nil {
		panic(err)
	}
	for _, k := range []string{"client_id", "client_secret", "tenant_id"} {
		if !pyrepr.Truthy(seed[k]) {
			return nil
		}
	}
	return seed
}

// Out of process text delivery for cron, a Bot Framework token then one message activity post
func standaloneSend(ctx context.Context, kwargs map[string]any) (map[string]any, error) {
	pconfig, _ := kwargs["pconfig"].(*gateway.PlatformConfig)
	chatID, _ := kwargs["chat_id"].(string)
	message, _ := kwargs["message"].(string)
	var extra map[string]any
	if pconfig != nil {
		extra = pconfig.Extra
	}
	clientID, clientSecret, tenantID, err := credentials(ctx, pconfig)
	if err != nil {
		return gateway.SendError("Teams standalone send failed: " + err.Error()), nil
	}
	if clientID == "" || clientSecret == "" || tenantID == "" {
		return gateway.SendError("Teams standalone send: TEAMS_CLIENT_ID, TEAMS_CLIENT_SECRET, and TEAMS_TENANT_ID are all required"), nil
	}
	rawServiceURL := ""
	if v := extra["service_url"]; pyrepr.Truthy(v) {
		rawServiceURL = pyrepr.Str(v)
	} else if rawServiceURL, err = gateway.GetScopedSecret(ctx, "TEAMS_SERVICE_URL", "", false); err != nil {
		return gateway.SendError("Teams standalone send failed: " + err.Error()), nil
	}
	if rawServiceURL == "" {
		rawServiceURL = defaultTeamsServiceURL
	}
	serviceURL := validateTeamsServiceURL(rawServiceURL)
	for _, check := range []struct {
		failed bool
		msg    string
	}{{serviceURL == "", "TEAMS_SERVICE_URL host is not on the Bot Framework allowlist; expected one of " + pyrepr.Repr(allowedTeamsServiceHosts)},
		{chatID == "", "chat_id (conversation ID) is required"},
		{!teamsConvIDRe(chatID), "chat_id contains characters outside the Bot Framework conversation ID set"},
		{!teamsConvIDRe(tenantID), "TEAMS_TENANT_ID contains characters outside the expected set"}} {
		if check.failed {
			return gateway.SendError("Teams standalone send: " + check.msg), nil
		}
	}
	result, err := standalonePost(ctx, tenantID, clientID, clientSecret, serviceURL+"v3/conversations/"+chatID+"/activities", message)
	if err != nil {
		slog.Debug(fmt.Sprintf("Teams standalone send raised: %v", err))
		return gateway.SendError("Teams standalone send failed: " + err.Error()), nil
	}
	return result, nil
}

// Token request and activity post of a standalone send, failures past the transport as send errors
func standalonePost(ctx context.Context, tenantID, clientID, clientSecret, activitiesURL, message string) (map[string]any, error) {
	client := gateway.TrustEnvHTTPClient(ctx, 15*time.Second)
	tokenURL, form := bfTokenRequest(tenantID, clientID, clientSecret)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	status, body, err := doRead(client, req)
	if err != nil {
		return nil, err
	}
	if status >= 400 {
		return gateway.SendError(fmt.Sprintf("Teams standalone send: token request failed (%d): %s", status, pystr.Slice(body, 0, 300))), nil
	}
	var tokenPayload map[string]any
	if err := json.Unmarshal([]byte(body), &tokenPayload); err != nil {
		return nil, err
	}
	accessToken, _ := tokenPayload["access_token"].(string)
	if accessToken == "" {
		return gateway.SendError("Teams standalone send: token response missing access_token"), nil
	}
	payload, err := pyjson.Dumps(pyjson.ObjectOf("type", "message", "text", message, "textFormat", "markdown"))
	if err != nil {
		return nil, err
	}
	req, err = http.NewRequestWithContext(ctx, http.MethodPost, activitiesURL, strings.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	status, body, err = doRead(client, req)
	if err != nil {
		return nil, err
	}
	if status >= 400 {
		return gateway.SendError(fmt.Sprintf("Teams standalone send: activity post failed (%d): %s", status, pystr.Slice(body, 0, 300))), nil
	}
	var sendPayload map[string]any
	if err := json.Unmarshal([]byte(body), &sendPayload); err != nil {
		return nil, err
	}
	return map[string]any{"success": true, "message_id": sendPayload["id"]}, nil
}

// Status and body text of one request
func doRead(client *http.Client, req *http.Request) (int, string, error) {
	resp, err := client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, "", err
	}
	return resp.StatusCode, pystr.DecodeUTF8Replace(raw), nil
}

// Passive dependency probe, satisfied since the Bot Framework client is compiled into the binary
func checkRequirements() bool { return true }

// Active dependency check, satisfied since nothing is installed at runtime
func checkTeamsRequirements(context.Context) (bool, error) { return true, nil }

// Setup flow pointer shown when the Teams support is reported missing
func installHint() string {
	return "Teams SDK missing — run `icebath setup`, configure Teams, then restart the gateway"
}

// Model facing description of Teams markdown support
const platformHint = "You are chatting via Microsoft Teams. Teams renders a subset of " +
	"markdown — bold (**text**), italic (*text*), and inline code " +
	"(`code`) work, but complex tables or raw HTML do not. Keep " +
	"responses clear and professional."

// Teams platform plugin compiled into the binary under the bundled platforms/teams key
type teamsPlatformPlugin struct{}

func (teamsPlatformPlugin) Manifest() plugins.Manifest {
	return plugins.Manifest{Name: "teams-platform", Version: "1.0.0", Kind: "platform", Source: "bundled", Key: "platforms/teams",
		Author: "Aamir Jawaid",
		Description: "Microsoft Teams gateway adapter for Icebath Agent. Connects to Microsoft Teams via the Bot Framework and " +
			"relays messages between Teams chats (personal DMs, group chats, channel posts) and the Icebath agent. Supports " +
			"Adaptive Card approval prompts.\n",
		RequiresEnv: []plugins.EnvSpec{
			{Name: "TEAMS_CLIENT_ID", Description: "Azure AD application (Bot Framework) client ID", URL: "https://portal.azure.com/"},
			{Name: "TEAMS_CLIENT_SECRET", Description: "Azure AD application client secret", URL: "https://portal.azure.com/", Secret: true},
			{Name: "TEAMS_TENANT_ID", Description: "Azure AD tenant ID hosting the bot application"}}}
}

// Adapter class the gateway migration code reads the profile prefix trait from
var teamsAdapterClass = &gateway.AdapterClass{Name: "TeamsAdapter", Traits: teamsAdapterTraits(),
	New: func(ctx context.Context, config *gateway.PlatformConfig) (gateway.PlatformAdapter, error) {
		return NewTeamsAdapter(ctx, config)
	}}

// Registers the Teams platform entry with its factory, gates, setup, standalone sender and hint
func (teamsPlatformPlugin) Register(ctx *plugins.Context) error {
	_, err := ctx.RegisterPlatform(plugins.PlatformSpec{
		Name: "teams", Label: "Microsoft Teams",
		AdapterFactory: gateway.AdapterFactory(func(fctx context.Context, cfg *gateway.PlatformConfig) (any, error) {
			return NewTeamsAdapter(fctx, cfg)
		}),
		Check:       checkRequirements,
		RequiredEnv: []string{"TEAMS_CLIENT_ID", "TEAMS_CLIENT_SECRET", "TEAMS_TENANT_ID"},
		InstallHint: installHint(),
		Extra: map[string]any{
			"dashboard_description":  "Connect Icebath to Microsoft Teams chats via the Bot Framework.",
			"docs_url":               "https://hermes-agent.nousresearch.com/docs/user-guide/messaging/teams",
			"ensure_deps_fn":         checkTeamsRequirements,
			"scoped_validate_config": validateConfig,
			"is_connected":           validateConfig,
			"setup_fn":               InteractiveSetup,
			"env_enablement_fn":      envEnablement,
			"cron_deliver_env_var":   "TEAMS_HOME_CHANNEL",
			"standalone_sender_fn":   standaloneSend,
			"allowed_users_env":      "TEAMS_ALLOWED_USERS",
			"allow_all_env":          "TEAMS_ALLOW_ALL_USERS",
			"max_message_length":     maxMessage,
			"emoji":                  "💼",
			"allow_update_command":   true,
			"platform_hint":          platformHint,
		},
	})
	return err
}

func init() {
	gateway.RegisterAdapterModule(teamsAdapterClass)
	plugins.RegisterBundled(teamsPlatformPlugin{})
}
