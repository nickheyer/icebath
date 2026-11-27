package credentials

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/credentials"
	openrouterconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/openrouter/config"
	providerconfig "github.com/nickheyer/icebath/internal/provider/config"
	"github.com/nickheyer/icebath/pkg/strs"
)

const (
	authorize = "https://openrouter.ai/auth"
	keys      = "https://openrouter.ai/api/v1/auth/keys"
	docs      = "https://openrouter.ai/docs/guides/overview/auth/oauth"
	// Most of a refusal shown
	errorLimit = 2048
)

// Sign-in at OpenRouter that gives the account an API key: the browser sent back to this computer, or the code it shows pasted on a
// remote session
var SignIn = credentials.SignIn{Spec: openrouterconfig.Name, Title: "OpenRouter account", Type: credentials.OAuth, Run: signIn}

func failed(message, code string, relogin bool) *credentials.AuthError {
	return credentials.Failed(openrouterconfig.Name, code, message, relogin, nil)
}

func signIn(p *credentials.Prompter) (credentials.Signed, error) {
	verifier, challenge := credentials.PKCE(64)
	params := url.Values{"code_challenge": {challenge}, "code_challenge_method": {"S256"}}
	var code string
	if credentials.Remote() {
		params.Set("key_label", "icebath-agent")
		link := authorize + "?" + params.Encode()
		p.Say("Remote session detected — using OpenRouter's headless flow.\nOpen this URL in a browser on any machine, authorize, then paste the code shown:\n  %s\n", link)
		if p.Show != nil {
			p.Show(link, "")
		}
		typed, ok := p.Ask("Authorization code: ", true)
		if !ok {
			return credentials.Signed{}, credentials.ErrCancelled
		}
		if code = strings.TrimSpace(typed); code == "" {
			return credentials.Signed{}, failed("No authorization code entered.", "openrouter_auth_no_code", false)
		}
	} else {
		callback, err := credentials.Listen("127.0.0.1", 0, "/callback/"+strs.Token(16), "OpenRouter", "openrouter_callback_bind_failed")
		if err != nil {
			return credentials.Signed{}, err
		}
		defer callback.Close()
		params.Set("callback_url", callback.Redirect)
		link := authorize + "?" + params.Encode()
		p.Say("Open this URL to authorize Icebath with OpenRouter:\n  %s\n\nDocs: %s", link, docs)
		if p.Show != nil {
			p.Show(link, "")
		}
		p.OpenBrowser(link, "Browser opened for OpenRouter authorization.", "Could not open the browser automatically; use the URL above.")
		p.Say("Waiting for the OpenRouter callback...")
		if code, err = callback.Wait(p.Ctx, p.Wait(300*time.Second), "", openrouterconfig.Name); err != nil {
			return credentials.Signed{}, err
		}
	}
	p.Say("Exchanging the authorization code for an OpenRouter API key...")
	reply, err := credentials.PostJSON(p.Ctx, credentials.Client(20*time.Second), keys, map[string]string{"code": code, "code_verifier": verifier,
		"code_challenge_method": "S256"}, nil)
	if err != nil {
		failure := failed(fmt.Sprintf("OpenRouter code exchange failed: %s", err), "openrouter_token_exchange_failed", false)
		failure.Err = err
		return credentials.Signed{}, failure
	}
	switch {
	case reply.Status == 403:
		return credentials.Signed{}, failed("OpenRouter rejected the authorization code (invalid, already used, or older than 10 minutes). Run the login again.",
			"openrouter_token_exchange_denied", true)
	case reply.Status >= 400:
		suffix := ""
		if detail := reply.Text(errorLimit); detail != "" {
			suffix = " Response: " + detail
		}
		return credentials.Signed{}, failed(fmt.Sprintf("OpenRouter code exchange failed (HTTP %d).%s", reply.Status, suffix), "openrouter_token_exchange_failed", false)
	}
	payload, err := reply.JSON()
	if err != nil {
		return credentials.Signed{}, failed("OpenRouter code exchange returned a non-JSON body.", "openrouter_token_exchange_invalid", false)
	}
	key := credentials.Text(payload["key"])
	if key == "" {
		return credentials.Signed{}, failed("OpenRouter code exchange response did not include a 'key'.", "openrouter_token_exchange_invalid", false)
	}
	return credentials.Signed{Fields: map[string]any{providerconfig.APIKeyField: key}}, nil
}
