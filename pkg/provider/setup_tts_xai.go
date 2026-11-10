package provider

import (
	"errors"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Signs in to xAI Grok OAuth from setup saving the tokens only, never the active provider, false on any failure
func runXAIOAuthLoginFromSetup(env PoolEnv) bool {
	SetupInfo(nil, "Signing in to xAI Grok OAuth (SuperGrok / Premium+)...")
	err := func() error {
		creds, err := xaiOAuthDeviceCodeLoginFn(env, setupUI().Out, 20*time.Second, !isRemoteSession())
		if err != nil {
			return err
		}
		tokens, ok := creds["tokens"]
		if !ok {
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
		setupUI().PrintWarning("xAI Grok OAuth login failed: " + pyrepr.ExceptionMessage(err))
		return false
	}
	return true
}

// Exported runXAIOAuthLoginFromSetup for the text to speech setup in pkg/media
func RunXAIOAuthLoginFromSetup(env PoolEnv) bool { return runXAIOAuthLoginFromSetup(env) }
