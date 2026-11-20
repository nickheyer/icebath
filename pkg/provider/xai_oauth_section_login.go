package provider

import (
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// XAIOAuthSectionLogin runs the xAI Grok OAuth device login printing to out, saving the tokens without making xAI the active provider
func XAIOAuthSectionLogin(env PoolEnv, out io.Writer) bool {
	fmt.Fprintln(out, "Signing in to xAI Grok OAuth (SuperGrok / Premium+)...")
	err := func() error {
		creds, err := xaiOAuthDeviceCodeLoginFn(env, out, 20*time.Second, !isRemoteSession())
		if err != nil {
			return err
		}
		tokens, held := creds["tokens"]
		if !held {
			return errors.New("'tokens'")
		}
		if err := saveXAIOAuthTokens(env, anyMap(tokens), xaiSaveOptions{
			Discovery: anyMap(creds["discovery"]), RedirectURI: anyString(creds["redirect_uri"]),
			LastRefresh: anyString(creds["last_refresh"]), AuthMode: "oauth_device_code", SetActive: false,
		}); err != nil {
			return err
		}
		_, err = env.Store.UnsuppressCredentialSource("xai-oauth", "device_code")
		return err
	}()
	if err != nil {
		fmt.Fprintln(out, "xAI Grok OAuth login failed: "+pyrepr.ExceptionMessage(err))
		return false
	}
	return true
}
