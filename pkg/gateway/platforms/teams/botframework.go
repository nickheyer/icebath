package teams

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"

	httputil "github.com/nickheyer/icebath/pkg/utils/http"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	jwtutil "github.com/nickheyer/icebath/pkg/utils/jwt"
)

// Bot Framework and Entra endpoints of the public cloud the connector and token checks use
var (
	botFrameworkJWKSURL  = "https://login.botframework.com/v1/.well-known/keys"
	entraLoginEndpoint   = "https://login.microsoftonline.com"
	defaultAPIServiceURL = "https://smba.trafficmanager.net/teams"
)

const (
	botFrameworkIssuer    = "https://api.botframework.com"
	entraV1IssuerPrefix   = "https://sts.windows.net/"
	agent365PlatformAppID = "5a807f24-c9de-44ee-a3a7-329e88a00ffc"
	jwtLeeway             = 300 * time.Second
	maxEntraValidators    = 100
	missingActivityID     = "DO_NOT_USE_PLACEHOLDER_ID"
	connectorUserAgent    = "Icebath"
	invokeMessageType     = "application/vnd.microsoft.activity.message"
	invokeCardType        = "application/vnd.microsoft.card.adaptive"
)

// Inbound activity token check, Bot Framework connector tokens and Agent 365 Entra tokens
type inboundTokenValidator struct {
	appID   string
	service *jwtutil.JWKClient

	mu         sync.Mutex
	entra      map[string]*jwtutil.JWKClient
	entraOrder []string
}

// Validator for the bot's app id against the public cloud key sets
func newInboundTokenValidator(appID string) *inboundTokenValidator {
	return &inboundTokenValidator{appID: appID, service: jwtutil.NewJWKClient(botFrameworkJWKSURL),
		entra: map[string]*jwtutil.JWKClient{}}
}

// Audiences a token for this app may carry
func (v *inboundTokenValidator) audiences() []string {
	return []string{v.appID, "api://" + v.appID, "api://botid-" + v.appID}
}

// Verifies a bearer token and returns its serviceurl claim, Entra tokens taking the tenant key set and caller check
func (v *inboundTokenValidator) validate(ctx context.Context, raw, serviceURL string) (string, error) {
	if raw == "" {
		return "", errors.New("No token provided")
	}
	unverified := jwt.MapClaims{}
	if _, _, err := jwt.NewParser().ParseUnverified(raw, unverified); err != nil {
		return "", err
	}
	var err error
	iss, _ := unverified["iss"].(string)
	var claims jwt.MapClaims
	if strings.HasPrefix(iss, entraLoginEndpoint) || strings.HasPrefix(iss, entraV1IssuerPrefix) {
		tid, _ := unverified["tid"].(string)
		if tid == "" {
			return "", errors.New("Entra inbound token is missing tid")
		}
		if claims, err = v.verify(ctx, raw, v.entraKeys(tid), []string{entraLoginEndpoint + "/" + tid + "/v2.0", entraV1IssuerPrefix + tid + "/"}, ""); err != nil {
			return "", err
		}
		caller, hasAzp := claims["azp"]
		if !hasAzp {
			caller = claims["appid"]
		}
		if id, ok := caller.(string); !ok || strings.ToLower(id) != agent365PlatformAppID {
			pkgLog().Warn(fmt.Sprintf("Entra inbound token caller app is not allowed: %v", caller))
			return "", errors.New("Entra inbound token caller app is not allowed")
		}
	} else if claims, err = v.verify(ctx, raw, v.service, []string{botFrameworkIssuer}, serviceURL); err != nil {
		return "", err
	}
	tokenServiceURL, _ := claims["serviceurl"].(string)
	return tokenServiceURL, nil
}

// Key set of an Entra tenant, the oldest tenant evicted past one hundred
func (v *inboundTokenValidator) entraKeys(tenantID string) *jwtutil.JWKClient {
	v.mu.Lock()
	defer v.mu.Unlock()
	if c := v.entra[tenantID]; c != nil {
		return c
	}
	c := jwtutil.NewJWKClient(entraLoginEndpoint + "/" + tenantID + "/discovery/v2.0/keys")
	v.entra[tenantID] = c
	v.entraOrder = append(v.entraOrder, tenantID)
	if len(v.entraOrder) > maxEntraValidators {
		delete(v.entra, v.entraOrder[0])
		v.entraOrder = v.entraOrder[1:]
	}
	return c
}

// RS256 signature, audience, issuer, expiry and issued at checks with the service URL claim when given
func (v *inboundTokenValidator) verify(ctx context.Context, raw string, keys *jwtutil.JWKClient, issuers []string, serviceURL string) (jwt.MapClaims, error) {
	parser := jwt.NewParser(jwt.WithValidMethods([]string{"RS256"}), jwt.WithLeeway(jwtLeeway), jwt.WithIssuedAt(),
		jwt.WithAudience(v.audiences()...))
	claims := jwt.MapClaims{}
	if _, err := parser.ParseWithClaims(raw, claims, keys.Keyfunc(ctx)); err != nil {
		return nil, err
	}
	if len(issuers) > 0 {
		if iss, _ := claims["iss"].(string); !slices.Contains(issuers, iss) {
			return nil, errors.New("Invalid issuer")
		}
	}
	if serviceURL != "" {
		tokenURL, _ := claims["serviceurl"].(string)
		if tokenURL == "" {
			return nil, errors.New("Token missing serviceurl claim")
		}
		got, want := strings.TrimRight(tokenURL, "/"), strings.TrimRight(serviceURL, "/")
		if got != want {
			return nil, fmt.Errorf("Service URL mismatch. Token: %s, Expected: %s", got, want)
		}
	}
	return claims, nil
}

// Conversation reference of an inbound activity, its service URL, bot account and conversation as received
type conversationRef struct {
	serviceURL   string
	bot          map[string]any
	conversation map[string]any
}

// Service URL App.send posts to, the SERVICE_URL env var or the public Teams endpoint
func appServiceURL() string {
	if v := os.Getenv("SERVICE_URL"); v != "" {
		return v
	}
	return defaultAPIServiceURL
}

// Activity id the connector returned for a sent activity, the SDK fallback id when its answer had none
type sentActivity struct{ id string }

// Posts an activity into a conversation with the bot token, the httpx status error text on a non 2xx answer
func (a *TeamsAdapter) createActivity(ctx context.Context, ref conversationRef, activity map[string]any) (*sentActivity, error) {
	token, err := a.getBotFrameworkToken(ctx)
	if err != nil {
		return nil, err
	}
	activity["from"] = ref.bot
	activity["conversation"] = ref.conversation
	if _, ok := activity["channelId"]; !ok {
		activity["channelId"] = "msteams"
	}
	convID, _ := ref.conversation["id"].(string)
	target := strings.TrimRight(ref.serviceURL, "/") + "/v3/conversations/" + convID + "/activities"
	body, err := json.Marshal(activity)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", connectorUserAgent)
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &httputil.StatusError{StatusCode: resp.StatusCode, URL: target, Location: resp.Header.Get("Location")}
	}
	var payload map[string]any
	_ = json.Unmarshal(raw, &payload)
	id, _ := payload["id"].(string)
	if _, present := payload["id"]; !present {
		id = missingActivityID
	}
	return &sentActivity{id: id}, nil
}

// Proactive send to a bare conversation id the way App.send builds its reference
func (a *TeamsAdapter) appSend(ctx context.Context, conversationID string, activity map[string]any) (*sentActivity, error) {
	a.mu.Lock()
	app := a.app
	a.mu.Unlock()
	if app == nil {
		return nil, errors.New("app not initialized - call app.initialize() or app.start() first")
	}
	ref := conversationRef{serviceURL: appServiceURL(), bot: map[string]any{"id": app.id},
		conversation: map[string]any{"id": conversationID}}
	return a.createActivity(ctx, ref, activity)
}

// Thread conversation id of a channel post, the ValueError App.reply raises for a bad root id
func toThreadedConversationID(conversationID, messageID string) (string, error) {
	if conversationID == "" {
		return "", errors.New("conversation_id must be a non-empty string")
	}
	if messageID == "" || !strIsDigit(messageID) || messageID == "0" {
		return "", fmt.Errorf("Invalid message_id \"%s\": must be a non-zero numeric value", messageID)
	}
	return strings.Split(conversationID, ";")[0] + ";messageid=" + messageID, nil
}

// Message activity carrying text
func textActivity(text string) map[string]any { return map[string]any{"type": "message", "text": text} }

// Message activity carrying one adaptive card attachment
func cardActivity(card *jsonutil.Object) map[string]any {
	return map[string]any{"type": "message", "attachments": []any{
		jsonutil.ObjectOf("contentType", invokeCardType, "content", card)}}
}

// Adaptive card of version 1.4 with its body blocks and optional actions
func adaptiveCard(body []any, actions []any) *jsonutil.Object {
	card := jsonutil.ObjectOf("type", "AdaptiveCard", "version", "1.4", "body", body)
	if actions != nil {
		card.Set("actions", actions)
	}
	return card
}

// TextBlock element with wrap on and the optional weight and subtle flags
func textBlock(text, weight string, subtle bool) *jsonutil.Object {
	block := jsonutil.ObjectOf("type", "TextBlock", "text", text, "wrap", true)
	if weight != "" {
		block.Set("weight", weight)
	}
	if subtle {
		block.Set("isSubtle", true)
	}
	return block
}

// Invoke response of a card action that shows a message
func invokeMessage(text string) (int, any) {
	return 200, jsonutil.ObjectOf("statusCode", 200, "type", invokeMessageType, "value", text)
}

// Invoke response of a card action that replaces the card
func invokeCard(body []any) (int, any) {
	return 200, jsonutil.ObjectOf("statusCode", 200, "type", invokeCardType, "value", adaptiveCard(body, nil))
}

// Bot Framework token URL and client credentials form
func bfTokenRequest(tenantID, clientID, clientSecret string) (string, url.Values) {
	return "https://login.microsoftonline.com/" + tenantID + "/oauth2/v2.0/token", url.Values{
		"grant_type": {"client_credentials"}, "client_id": {clientID}, "client_secret": {clientSecret}, "scope": {bfTokenScope}}
}

const bfTokenScope = "https://api.botframework.com/.default"
