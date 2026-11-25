package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
)

// Device authorization endpoint and scope of the Qwen account sign in the Qwen CLI uses
const (
	QwenOAuthDeviceCodeURL = "https://chat.qwen.ai/api/v1/oauth2/device/code"
	QwenOAuthScope         = "openid profile email model.completion"
)

// Endpoint the Qwen device login asks for a code at, swappable so tests aim it at a local server
var qwenOAuthDeviceCodeURL = QwenOAuthDeviceCodeURL

// Signs in to a Qwen account by device code and writes the Qwen CLI credentials file
func qwenDeviceCodeLogin(env PoolEnv, out io.Writer, openBrowser bool) error {
	ctx := env.ctx()
	client := qwenHTTPClientFn(20 * time.Second)
	verifier := pkceCodeVerifier(32)
	headers := map[string]string{"x-request-id": uuid.NewString()}
	for k, v := range formJSONHeaders {
		headers[k] = v
	}
	resp, err := oauthPostForm(ctx, client, qwenOAuthDeviceCodeURL, url.Values{"client_id": {QwenOAuthClientID}, "scope": {QwenOAuthScope},
		"code_challenge": {pkceCodeChallenge(verifier)}, "code_challenge_method": {"S256"}}, headers)
	if err != nil {
		return qwenErr(fmt.Sprintf("Qwen device authorization failed: %s", err), "qwen_device_code_failed", false)
	}
	raw, err := resp.JSON()
	device, isObject := raw.(map[string]any)
	if resp.Status != 200 || err != nil || !isObject || anyutil.StringOr(device["device_code"], "") == "" || anyutil.StringOr(device["user_code"], "") == "" {
		return qwenErr(fmt.Sprintf("Qwen device authorization returned status %d.", resp.Status), "qwen_device_code_failed", false)
	}
	link := anyutil.StringOr(device["verification_uri_complete"], "")
	if link == "" {
		link = anyutil.StringOr(device["verification_uri"], "")
	}
	if err := printDeviceCodeInstructions(out, link, anyutil.StringOr(device["user_code"], ""), openBrowser && !isRemoteSession(), "—", true); err != nil {
		return err
	}
	expiresIn, ok := wholeNumber(device["expires_in"])
	if !ok || expiresIn <= 0 {
		expiresIn = 600
	}
	fmt.Fprintln(out, "Waiting for approval...")
	deadline := time.Now().Add(time.Duration(expiresIn) * time.Second)
	interval := 2 * time.Second
	cancelled := func() bool { return ctx.Err() != nil }
	for time.Now().Before(deadline) {
		if err := cancellableSleep(interval, cancelled); err != nil {
			return &SignInCancelledError{ExitCode: 130, HasCode: true}
		}
		poll, err := oauthPostForm(ctx, client, qwenOAuthTokenURL, url.Values{"grant_type": {DeviceCodeGrantType}, "client_id": {QwenOAuthClientID},
			"device_code": {anyutil.StringOr(device["device_code"], "")}, "code_verifier": {verifier}}, formJSONHeaders)
		if err != nil {
			if cancelled() {
				return &SignInCancelledError{ExitCode: 130, HasCode: true}
			}
			return qwenErr(fmt.Sprintf("Qwen token polling failed: %s", err), "qwen_device_token_failed", false)
		}
		payload, _ := poll.JSON()
		body, _ := payload.(map[string]any)
		switch code := anyutil.StringOr(body["error"], ""); {
		case poll.Status == 400 && code == "authorization_pending":
			continue
		case poll.Status == 429 && code == "slow_down":
			interval = min(interval*3/2, 10*time.Second)
			continue
		case poll.Status != 200 || strings.TrimSpace(anyutil.StringOr(body["access_token"], "")) == "":
			reason := anyutil.StringOr(body["error_description"], "")
			if reason == "" {
				reason = code
			}
			return qwenErr("Qwen sign in did not complete: "+reason, "qwen_device_token_failed", false)
		}
		creds := map[string]any{"access_token": strings.TrimSpace(anyutil.StringOr(body["access_token"], "")), "refresh_token": anyutil.StringOr(body["refresh_token"], ""),
			"token_type": anyutil.StringOr(body["token_type"], ""), "resource_url": anyutil.StringOr(body["resource_url"], "")}
		if seconds, ok := wholeNumber(body["expires_in"]); ok && seconds > 0 {
			creds["expiry_date"] = time.Now().UnixMilli() + int64(seconds)*1000
		}
		path, err := saveQwenCLITokens(creds)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "Login successful!\n  Qwen credentials: %s\n", path)
		return nil
	}
	return qwenErr("Timed out waiting for Qwen device authorization.", "device_code_timeout", false)
}

// Sign in ended by a cancel or a stopped context, which the caller prints no failure copy for
func isSignInCancelled(err error) bool {
	var cancelled *SignInCancelledError
	return errors.As(err, &cancelled) || errors.Is(err, context.Canceled)
}
