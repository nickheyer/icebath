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
		p.Say("Your browser is on another computer, so OpenRouter shows a code instead of coming back here.")
		p.Say("Open this address in a browser on any computer, allow icebath, then paste the code it shows: %s\n", link)
		if p.Show != nil {
			p.Show(link, "")
		}
		typed, ok := p.Ask("Code: ", true)
		if !ok {
			return credentials.Signed{}, credentials.ErrCancelled
		}
		if code = strings.TrimSpace(typed); code == "" {
			return credentials.Signed{}, failed("No code was entered, so the sign-in stopped.", "openrouter_auth_no_code", false)
		}
	} else {
		callback, err := credentials.Listen("127.0.0.1", 0, "/callback/"+strs.Token(16), "OpenRouter", "openrouter_callback_bind_failed")
		if err != nil {
			return credentials.Signed{}, err
		}
		defer callback.Close()
		params.Set("callback_url", callback.Redirect)
		link := authorize + "?" + params.Encode()
		p.Say("Open this address in your browser to allow icebath in OpenRouter: %s\n", link)
		p.Say("How this works: %s", docs)
		if p.Show != nil {
			p.Show(link, "")
		}
		p.OpenBrowser(link, "The address is open in your browser.", "Your browser could not be opened, so open the address yourself.")
		p.Pending("Waiting for your browser to come back from OpenRouter.")
		if code, err = callback.Wait(p.Ctx, p.Wait(300*time.Second), "", openrouterconfig.Name); err != nil {
			return credentials.Signed{}, err
		}
	}
	p.Say("Getting your API key from OpenRouter.")
	reply, err := credentials.PostJSON(p.Ctx, credentials.Client(20*time.Second), keys, map[string]string{"code": code, "code_verifier": verifier,
		"code_challenge_method": "S256"}, nil)
	if err != nil {
		failure := failed(fmt.Sprintf("icebath could not reach OpenRouter to finish the sign-in: %s.", err), "openrouter_token_exchange_failed", false)
		failure.Err = err
		return credentials.Signed{}, failure
	}
	switch {
	case reply.Status == 403:
		return credentials.Signed{}, failed("OpenRouter refused the code, because it is wrong, used already or more than 10 minutes old. Start the sign-in again.",
			"openrouter_token_exchange_denied", true)
	case reply.Status >= 400:
		suffix := ""
		if detail := reply.Text(errorLimit); detail != "" {
			suffix = " OpenRouter said: " + strings.TrimSuffix(detail, ".") + "."
		}
		return credentials.Signed{}, failed(fmt.Sprintf("OpenRouter refused to finish the sign-in, with HTTP status %d.%s", reply.Status, suffix),
			"openrouter_token_exchange_failed", false)
	}
	payload, err := reply.JSON()
	if err != nil {
		return credentials.Signed{}, failed("OpenRouter's answer to the sign-in can't be read. Start the sign-in again.", "openrouter_token_exchange_invalid", false)
	}
	key := credentials.Text(payload["key"])
	if key == "" {
		return credentials.Signed{}, failed("OpenRouter finished the sign-in without sending an API key. Start the sign-in again.", "openrouter_token_exchange_invalid", false)
	}
	return credentials.Signed{Fields: map[string]any{providerconfig.APIKeyField: key}}, nil
}
