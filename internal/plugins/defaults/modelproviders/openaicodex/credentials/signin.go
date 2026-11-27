package credentials

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/credentials"
	openaicodexconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/openaicodex/config"
	"github.com/nickheyer/icebath/pkg/strs"
)

const (
	browserScope = "openid profile email offline_access"
	// OpenAI redirects the browser sign-in only to this port
	callbackPort = 1455
	callbackPath = "/auth/callback"
	callbackWait = 300 * time.Second
	portBusy     = "codex_browser_port_busy"
	// Longest a device code sign-in waits for its approval
	deviceWait = 15 * time.Minute
	// Transport errors in a row a device code sign-in rides out while it waits
	blips = 6
)

// Sign-in with a ChatGPT account, by device code or in the browser of this computer as the provider's login_flow or --browser say
var SignIn = credentials.SignIn{Spec: openaicodexconfig.Name, Title: "ChatGPT account", Type: credentials.OAuth, Run: signIn}

func signIn(p *credentials.Prompter) (credentials.Signed, error) {
	var tokens Tokens
	var err error
	if p.Option("browser") == "true" || p.Settings[openaicodexconfig.LoginFlow] == openaicodexconfig.Browser {
		tokens, err = browser(p)
		if failure, ok := errors.AsType[*credentials.AuthError](err); ok && failure.Code == portBusy {
			p.Say("Port %d is already in use (a Codex CLI sign-in may be running). OpenAI only redirects to that port, so falling back to the device-code login.\n", callbackPort)
			tokens, err = device(p)
		}
	} else {
		p.Say("Signing in to OpenAI Codex...")
		p.Say("(Icebath creates its own session — won't affect Codex CLI or VS Code)\n")
		tokens, err = device(p)
	}
	if err != nil {
		return credentials.Signed{}, err
	}
	return credentials.Signed{Identity: Identity(tokens.Access), Fields: map[string]any{openaicodexconfig.AccessToken: tokens.Access,
		openaicodexconfig.RefreshToken: tokens.Refresh, openaicodexconfig.LastRefresh: tokens.Refreshed}}, nil
}

// Device code sign-in: a code asked for, typed at OpenAI's page, polled until approved, then swapped for tokens
func device(p *credentials.Prompter) (Tokens, error) {
	ctx := p.Ctx
	code, err := deviceCode(p)
	if err != nil {
		return Tokens{}, err
	}
	link := Issuer + "/codex/device"
	p.Say("To continue, follow these steps:\n")
	p.Say("  1. Open this URL in your browser:\n     \033[94m%s\033[0m\n", link)
	p.Say("  2. Enter this code:\n     \033[94m%s\033[0m\n", code.user)
	if p.Show != nil {
		p.Show(link, code.user)
	}
	p.OpenBrowser(link, "Browser opened for OpenAI authorization.", "Could not open the browser automatically; use the URL above.")
	p.Say("Waiting for sign-in... (press Ctrl+C to cancel)")
	granted, err := poll(p, code)
	if err != nil {
		return Tokens{}, err
	}
	verifier := credentials.Text(granted["code_verifier"])
	authorization := credentials.Text(granted["authorization_code"])
	if authorization == "" || verifier == "" {
		return Tokens{}, failed("Device auth response missing authorization_code or code_verifier.", "device_code_incomplete_exchange", false)
	}
	return exchange(ctx, url.Values{"grant_type": {"authorization_code"}, "code": {authorization}, "redirect_uri": {Issuer + "/deviceauth/callback"},
		"client_id": {ClientID}, "code_verifier": {verifier}})
}

// Device code OpenAI handed out: the one to type, the one that names the sign-in, and how often to ask
type issued struct {
	user, id string
	interval time.Duration
}

// Asks for a device code, waiting out OpenAI's rate limit up to four times
func deviceCode(p *credentials.Prompter) (issued, error) {
	var reply *credentials.Reply
	var err error
	for attempt := 1; attempt <= 4; attempt++ {
		if reply, err = post(p.Ctx, Issuer+"/api/accounts/deviceauth/usercode", map[string]string{"client_id": ClientID}, "Failed to request device code", "device_code_request_failed"); err != nil {
			return issued{}, err
		}
		if reply.Status != 429 || attempt == 4 {
			break
		}
		delay := time.Duration(1<<attempt) * time.Second
		if wait, ok := reply.RetryAfter(); ok {
			delay = wait
		}
		delay = min(max(delay, time.Second), time.Minute)
		p.Say("OpenAI is rate-limiting login requests (429); retrying in %ds...", int(delay.Seconds()))
		if err := credentials.Sleep(p.Ctx, delay); err != nil {
			return issued{}, err
		}
	}
	switch {
	case reply.Status == 429:
		return issued{}, throttled(reply, "")
	case reply.Status != 200:
		return issued{}, failed(fmt.Sprintf("Device code request returned status %d.", reply.Status), "device_code_request_error", false)
	}
	payload, err := reply.JSON()
	if err != nil {
		return issued{}, err
	}
	interval, ok := config.Whole(payload["interval"])
	if !ok {
		interval = 5
	}
	code := issued{user: credentials.Text(payload["user_code"]), id: credentials.Text(payload["device_auth_id"]), interval: time.Duration(max(interval, 3)) * time.Second}
	if code.user == "" || code.id == "" {
		return issued{}, failed("Device code response missing required fields.", "device_code_incomplete", false)
	}
	return code, nil
}

// Polls until the sign-in is approved, a 403 or 404 meaning not yet, riding out six transport errors in a row
func poll(p *credentials.Prompter, code issued) (map[string]any, error) {
	start := time.Now()
	client := credentials.Client(15 * time.Second)
	body := map[string]string{"device_auth_id": code.id, "user_code": code.user}
	failures := 0
	for time.Since(start) < deviceWait {
		if err := credentials.Sleep(p.Ctx, code.interval); err != nil {
			p.Say("\nLogin cancelled.")
			return nil, err
		}
		reply, err := credentials.PostJSON(p.Ctx, client, Issuer+"/api/accounts/deviceauth/token", body, nil)
		if err != nil {
			if p.Ctx.Err() != nil {
				p.Say("\nLogin cancelled.")
				return nil, errors.Join(credentials.ErrCancelled, p.Ctx.Err())
			}
			if failures++; !transient(err) || failures >= blips {
				prefix := "Device auth polling request failed"
				if failures >= blips {
					prefix = fmt.Sprintf("Device auth polling request failed after %d consecutive transport errors", failures)
				}
				return nil, credentials.Failed(openaicodexconfig.Name, "device_code_poll_error", fmt.Sprintf("%s: %v%s", prefix, err, hint(err)), false, err)
			}
			p.Say("Transient network error while waiting for sign-in; retrying...")
			continue
		}
		failures = 0
		switch reply.Status {
		case 200:
			return reply.JSON()
		case 403, 404:
			continue
		}
		return nil, failed(fmt.Sprintf("Device auth polling returned status %d.", reply.Status), "device_code_poll_error", false)
	}
	return nil, failed("Login timed out after 15 minutes.", credentials.Expired, false)
}

// Swaps an authorization code for tokens at the endpoint both sign-ins share
func exchange(ctx context.Context, form url.Values) (Tokens, error) {
	reply, err := postForm(ctx, form)
	if err != nil {
		return Tokens{}, err
	}
	switch {
	case reply.Status == 429:
		return Tokens{}, throttled(reply, " during token exchange")
	case reply.Status != 200:
		return Tokens{}, failed(fmt.Sprintf("Token exchange returned status %d.", reply.Status), "token_exchange_error", false)
	}
	payload, err := reply.JSON()
	if err != nil {
		return Tokens{}, err
	}
	tokens := Tokens{Access: credentials.Text(payload["access_token"]), Refresh: credentials.Text(payload["refresh_token"]),
		Refreshed: time.Now().UTC().Format("2006-01-02T15:04:05.000000Z")}
	if tokens.Access == "" {
		return Tokens{}, failed("Token exchange did not return an access_token.", "token_exchange_no_access_token", false)
	}
	return tokens, nil
}

// Browser sign-in: OpenAI's authorize page opened, its redirect to the fixed loopback port caught, the code swapped with PKCE
func browser(p *credentials.Prompter) (Tokens, error) {
	verifier, challenge := credentials.PKCE(64)
	state := strs.Token(32)
	callback, err := credentials.Listen("127.0.0.1", callbackPort, callbackPath, "OpenAI Codex", portBusy)
	if err != nil {
		return Tokens{}, err
	}
	defer callback.Close()
	query := url.Values{"response_type": {"code"}, "client_id": {ClientID}, "redirect_uri": {callback.Redirect}, "scope": {browserScope},
		"code_challenge": {challenge}, "code_challenge_method": {"S256"}, "id_token_add_organizations": {"true"}, "state": {state}}
	link := Authorize + "?" + query.Encode()
	p.Say("\nSigning in to OpenAI Codex (browser authorization)...")
	p.Say("(Icebath creates its own session — won't affect Codex CLI or VS Code)\n")
	p.Say("Open this URL to authorize Icebath:\n  %s\n", link)
	if p.Show != nil {
		p.Show(link, "")
	}
	p.Tunnel(callback.Redirect)
	p.OpenBrowser(link, "Browser opened for OpenAI authorization.", "Could not open the browser automatically; use the URL above.")
	wait := p.Wait(callbackWait)
	p.Say("Waiting for the OpenAI callback on %s (timeout %ds, Ctrl+C to cancel)...", callback.Redirect, int(wait.Seconds()))
	code, err := callback.Wait(p.Ctx, wait, state, openaicodexconfig.Name)
	if err != nil {
		if p.Ctx.Err() != nil {
			p.Say("\nLogin cancelled.")
		}
		return Tokens{}, err
	}
	p.Say("Exchanging the authorization code for Codex tokens...")
	return exchange(p.Ctx, url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {callback.Redirect}, "client_id": {ClientID},
		"code_verifier": {verifier}})
}

// One sign-in POST of JSON, tried three times over a transport error that may pass
func post(ctx context.Context, endpoint string, body any, what, code string) (*credentials.Reply, error) {
	return retried(ctx, what, code, func() (*credentials.Reply, error) {
		return credentials.PostJSON(ctx, credentials.Client(15*time.Second), endpoint, body, nil)
	})
}

func postForm(ctx context.Context, form url.Values) (*credentials.Reply, error) {
	return retried(ctx, "Token exchange failed", "token_exchange_failed", func() (*credentials.Reply, error) {
		return credentials.PostForm(ctx, credentials.Client(15*time.Second), TokenURL, form, nil)
	})
}

func retried(ctx context.Context, what, code string, send func() (*credentials.Reply, error)) (*credentials.Reply, error) {
	for attempt := 1; ; attempt++ {
		reply, err := send()
		if err == nil {
			return reply, nil
		}
		if ctx.Err() != nil {
			return nil, errors.Join(credentials.ErrCancelled, ctx.Err())
		}
		if attempt == 3 || !transient(err) {
			return nil, credentials.Failed(openaicodexconfig.Name, code, fmt.Sprintf("%s: %v%s", what, err, hint(err)), false, err)
		}
		if err := credentials.Sleep(ctx, time.Duration(attempt)*time.Second); err != nil {
			return nil, err
		}
	}
}

// Failure of a sign-in OpenAI throttled, which is no credential problem
func throttled(reply *credentials.Reply, during string) error {
	wait := " Wait a minute and run the login again."
	if after, ok := reply.RetryAfter(); ok {
		wait = fmt.Sprintf(" Try again in about %ds.", int(after.Seconds()))
	}
	return failed(fmt.Sprintf("OpenAI is rate-limiting Codex login requests (HTTP 429)%s. This is a temporary throttle on OpenAI's side, not a credential problem.%s",
		during, wait), RateLimited, false)
}

// Whether a failure is of the connection and may pass, unlike an error of decoding or of the code
func transient(err error) bool {
	if _, ok := errors.AsType[*credentials.AuthError](err); ok {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	if _, ok := errors.AsType[net.Error](err); ok {
		return true
	}
	_, errno := errors.AsType[syscall.Errno](err)
	_, record := errors.AsType[tls.RecordHeaderError](err)
	_, alert := errors.AsType[tls.AlertError](err)
	return errno || record || alert
}

// What to do about a sign-in that keeps failing at TLS, which some networks cause by refusing the larger ClientHello Go sends
func hint(err error) string {
	trouble := errors.Is(err, io.ErrUnexpectedEOF)
	if _, ok := errors.AsType[*tls.CertificateVerificationError](err); ok {
		trouble = true
	}
	if _, ok := errors.AsType[x509.UnknownAuthorityError](err); ok {
		trouble = true
	}
	if _, ok := errors.AsType[tls.RecordHeaderError](err); ok {
		trouble = true
	}
	for _, marker := range []string{"tls:", "x509:", "handshake failure"} {
		trouble = trouble || strings.Contains(err.Error(), marker)
	}
	if !trouble {
		return ""
	}
	return " This looks like a TLS handshake failure rather than a Codex outage: some networks reject the larger TLS 1.3 ClientHello that Go sends " +
		"by default (post-quantum hybrid groups). Workaround: run with GODEBUG=tlsmlkem=0 to restrict key exchange to classic curves " +
		"(x25519:secp256r1:secp384r1:x448), or test with TLS 1.2. Sign in already rides out a one-off dropped connection (the device code request " +
		"and token exchange are retried twice and polling survives up to six transport errors in a row), so this error means the network keeps " +
		"failing. Set the workaround, then run `icebath auth add openai-codex` again"
}
