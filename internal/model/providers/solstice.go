package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/model/auth"
	"github.com/nickheyer/icebath/internal/model/portal"
	"github.com/nickheyer/icebath/internal/model/wire"
)

const (
	solsticeInferenceURL = "https://generativelanguage.googleapis.com/v1alpha"
	solsticeConfigPath   = "/api/oauth/gemini-auth/config"
	solsticeTimeout      = 30 * time.Second
)

var solsticeTokenPaths = map[string]string{"authorization_code": "/api/oauth/gemini-auth/exchange", "refresh_token": "/api/oauth/gemini-auth/refresh"}

type solsticeProfile struct {
	base
	d Deps
}

func newSolstice(d Deps) Profile {
	return &solsticeProfile{d: d, base: base{spec: Spec{
		Name: "solstice", Aliases: []string{"solstice-oauth"}, DisplayName: "Solstice",
		Description: "Solstice (your own per-user quota, OAuth sign-in)", AuthType: authTypeOAuthExternal,
		BaseURL: solsticeInferenceURL, Hidden: true, SkipHealthCheck: true, SkipModelListing: true, Vision: true,
		FallbackModels:  []string{"gemini-3.1-pro", "gemini-4-argon", "gemini-3.8-flash", "gemini-3.5-flash-lite"},
		DefaultAuxModel: "gemini-flash-lite-latest",
		Gemini: &GeminiClient{
			Options:           wire.GeminiOptions{GenerateMethod: "generateContentPerUserQuota", StreamMethod: "streamGenerateContentPerUserQuota", Bearer: true},
			DefaultBaseURL:    solsticeInferenceURL,
			MissingKeyMessage: "Solstice needs a signed-in account. Run `icebath auth add solstice`.",
		},
	}}}
}

func solsticeErr(message, code string, relogin bool, cause error) error {
	return &auth.Error{Message: "solstice: " + message, Provider: "solstice", Code: code, Relogin: relogin, Cause: cause}
}

func (p *solsticeProfile) brokerRequest(ctx context.Context, method, path string, body json.RawMessage) (wire.Value, error) {
	token, err := p.d.Portal.AccessToken(ctx, portal.AccessTokenOptions{})
	if err != nil {
		return wire.Value{}, solsticeErr("sign in to Icebath Portal first (`icebath auth add portal`).", "solstice_portal_login_required", true, err)
	}
	var payload io.Reader
	if body != nil {
		payload = bytes.NewReader(body)
	}
	ctx, cancel := context.WithTimeout(ctx, solsticeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, p.d.Portal.ResolvedPortalBaseURL(ctx)+path, payload)
	if err != nil {
		return wire.Value{}, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+token.Reveal())
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := p.d.Net.Client.Do(req)
	if err != nil {
		return wire.Value{}, solsticeErr(fmt.Sprintf("Icebath Portal request failed: %T", err), "solstice_broker_unreachable", false, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, listBodyLimit))
	if err != nil {
		return wire.Value{}, solsticeErr(fmt.Sprintf("Icebath Portal request failed: %T", err), "solstice_broker_unreachable", false, err)
	}
	if resp.StatusCode == http.StatusNotFound {
		return wire.Value{}, solsticeErr("not available on this Icebath Portal account yet.", "solstice_not_configured", false, nil)
	}
	if resp.StatusCode >= 400 {
		return wire.Value{}, solsticeErr(fmt.Sprintf("Icebath Portal returned HTTP %d.", resp.StatusCode), "solstice_broker_failed", false, nil)
	}
	return wire.ParseValue(raw)
}

func (p *solsticeProfile) discoverClient(ctx context.Context) (map[string]string, error) {
	payload, err := p.brokerRequest(ctx, http.MethodGet, solsticeConfigPath, nil)
	if err != nil {
		return nil, err
	}
	missing := []string{}
	out := map[string]string{}
	for _, k := range []string{"client_id", "authorize_url", "scope"} {
		v := strings.TrimSpace(payload.Get(k).Display())
		if !payload.Get(k).Truthy() || v == "" {
			missing = append(missing, k)
		}
		out[k] = v
	}
	if len(missing) > 0 {
		return nil, solsticeErr("Icebath Portal config is missing "+strings.Join(missing, ", ")+".", "solstice_config_invalid", false, nil)
	}
	return out, nil
}

func (p *solsticeProfile) tokenRequest(ctx context.Context, pairs [][2]string) (json.RawMessage, error) {
	grant := map[string]string{}
	body := &wire.Dict{}
	for _, kv := range pairs {
		grant[kv[0]] = kv[1]
		switch kv[0] {
		case "code", "code_verifier", "redirect_uri", "refresh_token":
			body.SetString(kv[0], kv[1])
		}
	}
	path, known := solsticeTokenPaths[grant["grant_type"]]
	if !known {
		return nil, solsticeErr(fmt.Sprintf("unsupported grant type %q.", grant["grant_type"]), "solstice_grant_unsupported", false, nil)
	}
	payload, err := p.brokerRequest(ctx, http.MethodPost, path, body.JSON())
	if err != nil {
		var ae *auth.Error
		if grant["grant_type"] == "refresh_token" && errors.As(err, &ae) && ae.Code == "solstice_broker_failed" {
			return nil, solsticeErr("the stored sign-in was rejected; run `icebath auth add solstice` again.", "invalid_grant", true, err)
		}
		return nil, err
	}
	return payload.Raw(), nil
}

func (p *solsticeProfile) baseConfig() auth.PKCEConfig {
	return auth.PKCEConfig{Label: "Solstice", RedirectPath: "/gemini-auth/callback", TokenRequest: p.tokenRequest,
		ExtraAuthorizeParams: [][2]string{{"access_type", "offline"}, {"prompt", "consent"}}}
}

func (p *solsticeProfile) HandleAuth(ctx context.Context, action string, args AuthArgs) (bool, error) {
	cfg := p.baseConfig()
	if action == "add" {
		client, err := p.discoverClient(ctx)
		if err != nil {
			return false, err
		}
		cfg.ClientID, cfg.AuthorizeURL, cfg.Scopes = client["client_id"], client["authorize_url"], strings.Fields(client["scope"])
	}
	return p.d.Store.PKCEAuth(ctx, args.Console, p.spec.Name, cfg, action, args.NoBrowser)
}

func (p *solsticeProfile) CredentialRefresher() auth.Refresher {
	return p.d.Store.PKCERefresher(p.baseConfig())
}

func (p *solsticeProfile) MaxTokens(model string) (int, bool) { return 0, false }
