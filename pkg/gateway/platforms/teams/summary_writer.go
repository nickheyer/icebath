package teams

import (
	"context"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/gateway/messagetools"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/netpref"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	httputil "github.com/nickheyer/icebath/pkg/utils/http"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	urlutil "github.com/nickheyer/icebath/pkg/utils/url"
)

// Python ValueError of a delivery the config cannot route
type summaryValueError struct{ msg string }

func (e *summaryValueError) Error() string     { return e.msg }
func (e *summaryValueError) ErrorKind() string { return "ValueError" }

// Status error of an incoming webhook that answered outside the 2xx range, kept with its status and URL
type summaryHTTPStatusError struct{ *httputil.StatusError }

func (e *summaryHTTPStatusError) ErrorKind() string { return "HTTPStatusError" }
func (e *summaryHTTPStatusError) Unwrap() error     { return e.StatusError }

// Boolean of a config value, the usual on and off words read as such, else the default
func parseBool(value any, def bool) bool {
	switch x := value.(type) {
	case bool:
		return x
	case string:
		switch strings.ToLower(strings.TrimSpace(x)) {
		case "1", "true", "yes", "on":
			return true
		case "0", "false", "no", "off":
			return false
		}
	}
	return def
}

// Catalog key of each list heading with the payload list it renders
var summaryListSections = []struct {
	headingKey string
	items      func(SummaryPayload) []any
}{
	{"platform.teams.summary.key_decisions", func(p SummaryPayload) []any { return p.KeyDecisions }},
	{"platform.teams.summary.action_items", func(p SummaryPayload) []any { return p.ActionItems }},
	{"platform.teams.summary.risks", func(p SummaryPayload) []any { return p.Risks }},
}

// Env fallback of each delivery config key, applied only where nothing else set the key
var summaryEnvKeys = [][2]string{
	{"delivery_mode", "TEAMS_DELIVERY_MODE"}, {"incoming_webhook_url", "TEAMS_INCOMING_WEBHOOK_URL"},
	{"access_token", "TEAMS_GRAPH_ACCESS_TOKEN"}, {"team_id", "TEAMS_TEAM_ID"}, {"channel_id", "TEAMS_CHANNEL_ID"}, {"chat_id", "TEAMS_CHAT_ID"},
}

// Token source handing out one fixed access token so Graph delivery reuses the shared client
type staticAccessTokenProvider struct{ accessToken string }

func newStaticAccessTokenProvider(accessToken string) *staticAccessTokenProvider {
	return &staticAccessTokenProvider{accessToken: strings.TrimSpace(accessToken)}
}

func (p *staticAccessTokenProvider) GetAccessToken(ctx context.Context, forceRefresh bool) (string, error) {
	if p.accessToken == "" {
		return "", &summaryValueError{"TEAMS_GRAPH_ACCESS_TOKEN is required for graph delivery mode."}
	}
	return p.accessToken, nil
}

func (p *staticAccessTokenProvider) ClearCache() {}

// Fields of a meeting summary the writer renders, nil standing for an attribute the payload lacks
type SummaryPayload struct {
	Title, Summary, MeetingID        any
	KeyDecisions, ActionItems, Risks []any
}

// Graph client surface the writer posts a message through
type GraphPoster interface {
	PostJSON(ctx context.Context, path string, jsonBody any, headers map[string]string) (any, error)
}

// Delivers a meeting summary to Teams through an incoming webhook or Microsoft Graph
type TeamsSummaryWriter struct {
	platformConfig *gateway.PlatformConfig
	graphClient    GraphPoster
	transport      http.RoundTripper
}

// Writer over the Teams platform config, an optional Graph client and an optional HTTP transport
func NewTeamsSummaryWriter(platformConfig *gateway.PlatformConfig, graphClient GraphPoster, transport http.RoundTripper) *TeamsSummaryWriter {
	return &TeamsSummaryWriter{platformConfig: platformConfig, graphClient: graphClient, transport: transport}
}

// Sends the summary by the configured delivery mode and returns the delivery record, an existing record short circuiting it
func (w *TeamsSummaryWriter) WriteSummary(ctx context.Context, payload SummaryPayload, config, existingRecord *jsonutil.Object) (*jsonutil.Object, error) {
	merged, err := w.resolveDeliveryConfig(ctx, config)
	if err != nil {
		return nil, err
	}
	if existingRecord != nil && existingRecord.Len() > 0 && !parseBool(merged["force_resend"], false) {
		return existingRecord.Clone(), nil
	}
	mode := ""
	for _, key := range []string{"delivery_mode", "mode"} {
		if value := merged[key]; anyutil.IsTrue(value) {
			mode = anyutil.String(value)
			break
		}
	}
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode == "" && anyutil.IsTrue(merged["incoming_webhook_url"]) {
		mode = "incoming_webhook"
	} else if mode == "" && (anyutil.IsTrue(merged["chat_id"]) || (anyutil.IsTrue(merged["team_id"]) && anyutil.IsTrue(merged["channel_id"]))) {
		mode = "graph"
	}
	switch mode {
	case "incoming_webhook":
		return w.writeSummaryViaIncomingWebhook(ctx, payload, merged)
	case "graph":
		return w.writeSummaryViaGraph(ctx, payload, merged)
	}
	return nil, &summaryValueError{"Teams delivery_mode must be 'incoming_webhook' or 'graph'."}
}

// Delivery config merged from the platform extras, token and home channel, the call config, then the env fallbacks
func (w *TeamsSummaryWriter) resolveDeliveryConfig(ctx context.Context, config *jsonutil.Object) (map[string]any, error) {
	merged := map[string]any{}
	if cfg := w.platformConfig; cfg != nil {
		for key, value := range cfg.Extra {
			merged[key] = value
		}
		if _, has := merged["access_token"]; cfg.Token != "" && !has {
			merged["access_token"] = cfg.Token
		}
		if cfg.HomeChannel != nil {
			if _, has := merged["channel_id"]; !has {
				merged["channel_id"] = cfg.HomeChannel.ChatID
			}
		}
	}
	if config != nil {
		for _, key := range config.Keys() {
			merged[key] = config.Value(key)
		}
	}
	for _, pair := range summaryEnvKeys {
		key, env := pair[0], pair[1]
		value := os.Getenv(env)
		if key == "access_token" {
			scoped, err := gateway.GetScopedSecret(ctx, env, "", false)
			if err != nil {
				return nil, err
			}
			value = scoped
		}
		if value != "" && !anyutil.IsTrue(merged[key]) {
			merged[key] = value
		}
	}
	return merged, nil
}

// Stripped text of a config value, empty for a falsy one
func configText(value any) string {
	if !anyutil.IsTrue(value) {
		return ""
	}
	return strings.TrimSpace(anyutil.String(value))
}

// Posts the markdown summary to the incoming webhook URL on a fresh client with a twenty second timeout
func (w *TeamsSummaryWriter) writeSummaryViaIncomingWebhook(ctx context.Context, payload SummaryPayload, config map[string]any) (*jsonutil.Object, error) {
	webhookURL := configText(config["incoming_webhook_url"])
	if webhookURL == "" {
		return nil, &summaryValueError{"TEAMS_INCOMING_WEBHOOK_URL is required for incoming_webhook mode."}
	}
	text, err := jsonutil.Encode(jsonutil.ObjectOf("text", w.renderSummaryMarkdown(ctx, payload)))

	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhookURL, strings.NewReader(text))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	client, closeClient := &http.Client{Transport: w.transport, CheckRedirect: httputil.RedirectPolicy(false)}, func() {}
	if w.transport == nil {
		t := netpref.Follow(http.DefaultTransport.(*http.Transport).Clone())
		client.Transport, closeClient = t, t.CloseIdleConnections
	}
	resp, err := httputil.Do(client, req, httputil.EachPhase(20*time.Second))
	closeClient()
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, &summaryHTTPStatusError{&httputil.StatusError{StatusCode: resp.StatusCode, Status: resp.Status, URL: webhookURL, Location: resp.Header.Get("Location")}}
	}
	return jsonutil.ObjectOf("delivery_mode", "incoming_webhook", "webhook_url", webhookURL, "status_code", resp.StatusCode, "delivered", true), nil
}

// Posts the HTML summary as a chat message, or as a channel message of a team, through Microsoft Graph
func (w *TeamsSummaryWriter) writeSummaryViaGraph(ctx context.Context, payload SummaryPayload, config map[string]any) (*jsonutil.Object, error) {
	graphClient, err := w.buildGraphClient(config)
	if err != nil {
		return nil, err
	}
	record := jsonutil.ObjectOf("delivery_mode", "graph")
	path := ""
	if chatID := configText(config["chat_id"]); chatID != "" {
		path = "/chats/" + urlutil.Escape(chatID, "") + "/messages"
		record.Set("target_type", "chat")
		record.Set("chat_id", chatID)
	} else {
		teamID, channelID := configText(config["team_id"]), configText(config["channel_id"])
		if teamID == "" || channelID == "" {
			return nil, &summaryValueError{"Graph delivery mode requires chat_id, or both team_id and channel_id."}
		}
		path = "/teams/" + urlutil.Escape(teamID, "") + "/channels/" + urlutil.Escape(channelID, "") + "/messages"
		record.Set("target_type", "channel")
		record.Set("team_id", teamID)
		record.Set("channel_id", channelID)
	}
	body := jsonutil.ObjectOf("body", jsonutil.ObjectOf("contentType", "html", "content", w.renderSummaryHTML(ctx, payload)))
	response, err := graphClient.PostJSON(ctx, path, body, nil)
	if err != nil {
		return nil, err
	}
	answer, _ := response.(*jsonutil.Object)
	if answer == nil || answer.Len() == 0 {
		answer = jsonutil.NewObject()
	}
	record.Set("message_id", answer.ValueOr("id", nil))
	record.Set("web_url", answer.ValueOr("webUrl", nil))
	return record, nil
}

// Graph client given at construction, else one over the configured access token or the MSGRAPH credentials of the env
func (w *TeamsSummaryWriter) buildGraphClient(config map[string]any) (GraphPoster, error) {
	if w.graphClient != nil {
		return w.graphClient, nil
	}
	options := messagetools.MicrosoftGraphClientOptions{Transport: w.transport}
	if accessToken := configText(config["access_token"]); accessToken != "" {
		return messagetools.NewMicrosoftGraphClientFor(newStaticAccessTokenProvider(accessToken), options), nil
	}
	provider, err := messagetools.MicrosoftGraphTokenProviderFromEnv(nil)
	if err != nil {
		return nil, err
	}
	return messagetools.NewMicrosoftGraphClient(provider, options), nil
}

func summaryT(ctx context.Context, key string, kwargs map[string]any) string {
	return i18n.T(ctx, key, "", kwargs)
}

// Summary as Teams markdown, the title, the summary line and one bullet list per section
func (w *TeamsSummaryWriter) renderSummaryMarkdown(ctx context.Context, payload SummaryPayload) string {
	summary := summaryText(payload.Summary, summaryT(ctx, "platform.teams.summary.empty", nil))
	lines := []string{"**" + summaryTitle(ctx, payload) + "**", "", summaryT(ctx, "platform.teams.summary.summary_line", map[string]any{"summary": summary})}
	for _, section := range summaryListSections {
		lines = append(lines, "", summaryT(ctx, section.headingKey, nil)+":")
		lines = append(lines, summaryBulletLines(section.items(payload))...)
	}
	return strings.Join(lines, "\n")
}

// Python html.escape with quotes escaped
func htmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&#x27;").Replace(s)
}

// Summary as HTML, a heading per section with its list or the none line
func (w *TeamsSummaryWriter) renderSummaryHTML(ctx context.Context, payload SummaryPayload) string {
	summary := htmlEscape(summaryText(payload.Summary, summaryT(ctx, "platform.teams.summary.empty", nil)))
	var blocks strings.Builder
	blocks.WriteString("<h2>" + htmlEscape(summaryTitle(ctx, payload)) + "</h2>")
	blocks.WriteString("<h3>" + htmlEscape(summaryT(ctx, "platform.teams.summary.heading", nil)) + "</h3>")
	blocks.WriteString("<p>" + summary + "</p>")
	for _, section := range summaryListSections {
		var rendered strings.Builder
		for _, item := range section.items(payload) {
			if text := anyutil.String(item); strings.TrimSpace(text) != "" {
				rendered.WriteString("<li>" + htmlEscape(text) + "</li>")
			}
		}
		blocks.WriteString("<h3>" + htmlEscape(summaryT(ctx, section.headingKey, nil)) + "</h3>")
		if rendered.Len() > 0 {
			blocks.WriteString("<ul>" + rendered.String() + "</ul>")
		} else {
			blocks.WriteString("<p>" + htmlEscape(summaryT(ctx, "platform.teams.summary.none_item", nil)) + "</p>")
		}
	}
	return blocks.String()
}

// Title of the payload, else the meeting summary title naming its meeting id
func summaryTitle(ctx context.Context, payload SummaryPayload) string {
	if anyutil.IsTrue(payload.Title) {
		return anyutil.String(payload.Title)
	}
	meetingID := payload.MeetingID
	if !anyutil.IsTrue(meetingID) {
		meetingID = summaryT(ctx, "platform.teams.summary.title_fallback", nil)
	}
	return summaryT(ctx, "platform.teams.summary.title", map[string]any{"meeting_id": meetingID})
}

// Stripped text of a value, the default when it is falsy or blank
func summaryText(value any, def string) string {
	if text := configText(value); text != "" {
		return text
	}
	return def
}

// One markdown bullet per item that is not blank, a single None bullet when no item is left
func summaryBulletLines(values []any) []string {
	lines := []string{}
	for _, item := range values {
		if text := strings.TrimSpace(anyutil.String(item)); text != "" {
			lines = append(lines, "- "+text)
		}
	}
	if len(lines) == 0 {
		return []string{"- None"}
	}
	return lines
}
