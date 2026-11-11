package dashboard

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/observability"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/provider"
)

const (
	codexIssuer    = "https://auth.openai.com"
	codexExpiresIn = 15 * 60
)

// Client, single send and sleep of the Codex device login, the scripted transport hermes tests inject
var (
	codexLoginClient = provider.CodexLoginClient
	codexLoginSend   = provider.CodexLoginSend
	codexLoginSleep  = time.Sleep
)

// Short error detail of a provider response, from its JSON error fields or the head of its text
func httpResponseErrorDetail(resp *provider.OAuthHTTPResponse) string {
	if parsed, err := resp.JSON(); err == nil {
		if payload, ok := parsed.(map[string]any); ok {
			errValue := payload["error"]
			if errObj, isDict := errValue.(map[string]any); isDict {
				var parts []string
				for _, key := range []string{"message", "error_description", "code", "type"} {
					text := ""
					if v, present := errObj[key]; present {
						text = pystr.Strip(pyrepr.Str(v))
					}
					if text != "" {
						parts = append(parts, text)
					}
				}
				if len(parts) > 0 {
					return strings.Join(parts, ": ")
				}
			}
			for _, value := range []any{errValue, payload["detail"], payload["message"], payload["error_description"]} {
				if text, isText := value.(string); isText && pystr.Strip(text) != "" {
					return pystr.Strip(text)
				}
			}
		}
	}
	runes := []rune(pystr.Strip(string(resp.Body)))
	if len(runes) > 500 {
		runes = runes[:500]
	}
	return string(runes)
}

// Dashboard message of a refused Codex device code request, naming device code authorization when the detail does
func codexDeviceCodeStartError(resp *provider.OAuthHTTPResponse) string {
	detail := httpResponseErrorDetail(resp)
	lower := pystr.Lower(detail)
	message := "OpenAI rejected the device-code login request. Please try Login " +
		"again from the dashboard after checking your OpenAI account settings."
	if strings.Contains(lower, "device") && (strings.Contains(lower, "authori") || strings.Contains(lower, "enable")) {
		message = "OpenAI rejected the device-code login request. Your OpenAI " +
			"account may need device-code authorization enabled before Icebath " +
			"can start this dashboard login. Enable device-code authorization " +
			"in OpenAI, then return here and click Login again."
	}
	if detail != "" {
		return fmt.Sprintf("%s (HTTP %d: %s)", message, resp.Status, detail)
	}
	return fmt.Sprintf("%s (HTTP %d)", message, resp.Status)
}

// Whether the session was cancelled, logged with the stage the worker had reached
func codexCancelled(sess map[string]any, sessionID, stage string) bool {
	oauthSessionsMu.Lock()
	cancelled := pyrepr.Truthy(sess["cancelled"])
	oauthSessionsMu.Unlock()
	if !cancelled {
		return false
	}
	routersLog().Info(fmt.Sprintf("oauth/device: openai-codex login cancelled%s (session=%s)", stage, sessionID))
	return true
}

// One POST of the device login on a fresh client, a transient transport failure retried twice with a growing wait
func codexPostRetry(ctx context.Context, endpoint string, body []byte, headers map[string]string) (*provider.OAuthHTTPResponse, error) {
	const attempts = 3
	for attempt := 1; ; attempt++ {
		resp, err := codexLoginSend(ctx, codexLoginClient(), endpoint, body, headers)
		if err == nil {
			return resp, nil
		}
		if attempt == attempts || !provider.CodexIsTransientTransportError(err) {
			return nil, err
		}
		codexLoginSleep(time.Duration(attempt) * time.Second)
	}
}

// Error of a non 200 Codex reply, an auth refusal for 401 and 403, a connection failure for 408, 429 and 5xx
func codexHTTPError(status int, message string) error {
	if status == 401 || status == 403 {
		return provider.CodexAuthError(message)
	}
	if status == 408 || status == 429 || status >= 500 {
		return &pm.PyError{Class: "ConnectionError", Message: message}
	}
	return &pm.PyError{Class: "RuntimeError", Message: message}
}

var codexJSONHeaders = map[string]string{"Content-Type": "application/json"}

// Compact JSON body of a device login request
func codexJSONBody(payload *pyjson.Object) ([]byte, error) {
	text, err := pyjson.Dumps(payload, pyjson.Separators(",", ":"), pyjson.EnsureASCII(false))
	return []byte(text), err
}

// Requests the Codex device code, its poll interval floored at three seconds
func codexRequestUserCode(ctx context.Context) (map[string]any, error) {
	body, err := codexJSONBody(pyjson.ObjectOf("client_id", provider.CodexOAuthClientID))
	if err != nil {
		return nil, err
	}
	resp, err := codexPostRetry(ctx, codexIssuer+"/api/accounts/deviceauth/usercode", body, codexJSONHeaders)
	if err != nil {
		return nil, err
	}
	if resp.Status != 200 {
		return nil, codexHTTPError(resp.Status, codexDeviceCodeStartError(resp))
	}
	deviceData, err := resp.JSONObject()
	if err != nil {
		return nil, err
	}
	interval := int64(5)
	if _, present := deviceData["interval"]; present {
		if interval, err = oauthInt(deviceData, "interval"); err != nil {
			return nil, err
		}
	}
	deviceData["interval"] = max(int64(3), interval)
	if !pyrepr.Truthy(deviceData["user_code"]) || !pyrepr.Truthy(deviceData["device_auth_id"]) {
		return nil, &pm.PyError{Class: "RuntimeError", Message: "device-code response missing user_code or device_auth_id"}
	}
	return deviceData, nil
}

// Polls until the device code is authorized, nil when it expired and cancelled true when the user cancelled
func codexPollAuthorization(ctx context.Context, sess map[string]any, sessionID string) (map[string]any, bool, error) {
	oauthSessionsMu.Lock()
	expiresIn, _ := statusNumber(sess["expires_in"])
	interval, _ := statusNumber(sess["interval"])
	payload := pyjson.ObjectOf("device_auth_id", sess["device_auth_id"], "user_code", sess["user_code"])
	oauthSessionsMu.Unlock()
	body, err := codexJSONBody(payload)
	if err != nil {
		return nil, false, err
	}
	deadline := time.Now().Add(time.Duration(expiresIn * float64(time.Second)))
	const maxConsecutiveBlips = 6
	blips := 0
	client := codexLoginClient()
	for time.Now().Before(deadline) {
		if codexCancelled(sess, sessionID, "") {
			return nil, true, nil
		}
		codexLoginSleep(time.Duration(interval * float64(time.Second)))
		if codexCancelled(sess, sessionID, "") {
			return nil, true, nil
		}
		poll, err := codexLoginSend(ctx, client, codexIssuer+"/api/accounts/deviceauth/token", body, codexJSONHeaders)
		if err != nil {
			if !provider.CodexIsTransientTransportError(err) {
				return nil, false, err
			}
			blips++
			if blips >= maxConsecutiveBlips {
				return nil, false, &pm.PyError{Class: "RuntimeError",
					Message: fmt.Sprintf("deviceauth/token poll failed after %d consecutive transport errors: %s", blips, err)}
			}
			routersLog().Info(fmt.Sprintf("oauth/device: openai-codex poll transport blip, retrying (session=%s)", sessionID))
			continue
		}
		blips = 0
		if poll.Status == 200 {
			codeResp, err := poll.JSONObject()
			return codeResp, false, err
		}
		if poll.Status == 403 || poll.Status == 404 {
			continue
		}
		return nil, false, &pm.PyError{Class: "RuntimeError", Message: fmt.Sprintf("deviceauth/token poll returned %d", poll.Status)}
	}
	return nil, false, nil
}

// Exchanges the authorization code of an approved device login for its tokens
func codexExchangeTokens(ctx context.Context, codeResp map[string]any) (map[string]any, error) {
	authorizationCode, _ := codeResp["authorization_code"].(string)
	codeVerifier, _ := codeResp["code_verifier"].(string)
	if authorizationCode == "" || codeVerifier == "" {
		return nil, &pm.PyError{Class: "RuntimeError", Message: "device-auth response missing authorization_code/code_verifier"}
	}
	form := [][2]string{{"grant_type", "authorization_code"}, {"code", authorizationCode},
		{"redirect_uri", codexIssuer + "/deviceauth/callback"}, {"client_id", provider.CodexOAuthClientID}, {"code_verifier", codeVerifier}}
	pairs := make([]string, 0, len(form))
	for _, kv := range form {
		pairs = append(pairs, url.QueryEscape(kv[0])+"="+url.QueryEscape(kv[1]))
	}
	resp, err := codexPostRetry(ctx, provider.CodexOAuthTokenURL, []byte(strings.Join(pairs, "&")),
		map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
	if err != nil {
		return nil, err
	}
	if resp.Status != 200 {
		return nil, codexHTTPError(resp.Status, fmt.Sprintf("token exchange returned %d", resp.Status))
	}
	tokens, err := resp.JSONObject()
	if err != nil {
		return nil, err
	}
	if !pyrepr.Truthy(tokens["access_token"]) {
		return nil, &pm.PyError{Class: "RuntimeError", Message: "token exchange did not return access_token"}
	}
	out := map[string]any{"access_token": "", "refresh_token": ""}
	for key := range out {
		if v, present := tokens[key]; present {
			out[key] = v
		}
	}
	return out, nil
}

// Runs the whole Codex device code login of a session, the cancel check and the token save in one critical section
func codexFullLoginWorker(ctx context.Context, sessionID string) {
	err := func() error {
		deviceData, err := codexRequestUserCode(ctx)
		if err != nil {
			return err
		}
		oauthSessionsMu.Lock()
		sess := oauthSessions[sessionID]
		if len(sess) == 0 {
			oauthSessionsMu.Unlock()
			return nil
		}
		sess["user_code"], sess["verification_url"] = deviceData["user_code"], codexIssuer+"/codex/device"
		sess["device_auth_id"], sess["interval"] = deviceData["device_auth_id"], deviceData["interval"]
		sess["expires_in"], sess["expires_at"] = int64(codexExpiresIn), float64(time.Now().UnixNano())/1e9+codexExpiresIn
		sessionProfile := oauthSessionProfile(sess)
		oauthSessionsMu.Unlock()
		codeResp, cancelled, err := codexPollAuthorization(ctx, sess, sessionID)
		if err != nil {
			return err
		}
		if cancelled {
			return nil
		}
		if codeResp == nil {
			oauthSessionsMu.Lock()
			sess["status"], sess["error_message"] = "expired", "Device code expired before approval"
			oauthSessionsMu.Unlock()
			return nil
		}
		if codexCancelled(sess, sessionID, " before token exchange") {
			return nil
		}
		tokens, err := codexExchangeTokens(ctx, codeResp)
		if err != nil {
			return err
		}
		sctx, _, release, err := profileScope(ctx, sessionProfile)
		if err != nil {
			return err
		}
		defer release()
		oauthSessionsMu.Lock()
		defer oauthSessionsMu.Unlock()
		if pyrepr.Truthy(sess["cancelled"]) {
			routersLog().Info(fmt.Sprintf("oauth/device: openai-codex login cancelled before token save (session=%s)", sessionID))
			return nil
		}
		if err := provider.SaveCodexTokens(bootstrapPoolEnv(sctx), tokens, "", "", true, false); err != nil {
			return err
		}
		sess["status"] = "approved"
		routersLog().Info(fmt.Sprintf("oauth/device: openai-codex login completed (session=%s)", sessionID))
		return nil
	}()
	if err != nil {
		routersLog().Warn(fmt.Sprintf("codex device-code worker failed (session=%s): %s", sessionID, err))
		oauthSessionsMu.Lock()
		sess := oauthSessions[sessionID]
		observability.NoteOAuthFailure(sess, err)
		if len(sess) > 0 {
			sess["status"], sess["error_message"] = "error", err.Error()
		}
		oauthSessionsMu.Unlock()
	}
}
