package googlechat

import (
	"context"
	"errors"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Runs the in chat /setup-files OAuth flow for the sender, reporting whether the message was consumed
func (a *GoogleChatAdapter) handleSetupFilesCommand(ctx context.Context, chatID, threadID, rawText, senderEmail string) bool {
	senderKey := strings.ToLower(pystr.Strip(senderEmail))
	arg := ""
	if fields := strings.Fields(rawText); len(fields) > 1 {
		arg = pystr.Strip(strings.TrimSpace(rawText)[len(fields[0]):])
	}
	reply := func(text string) {
		if _, err := a.createMessage(ctx, chatID, threadBody(text, threadID)); err != nil {
			pkgLog().Debug("[GoogleChat] /setup-files reply send failed", "error", err)
		}
	}
	helperFailed := func(step string, err error, failureKey string) {
		var failure *oauthFailure
		if errors.As(err, &failure) && failureKey != "" {
			reply(tr(ctx, "setup_files."+failureKey, nil))
			return
		}
		pkgLog().Warn("[GoogleChat] /setup-files "+step+" failed", "error", err)
		key := "setup_files.helper_error"
		if step == "revoke" {
			key = "setup_files.revoke_error"
		}
		reply(tr(ctx, key, map[string]any{"error": err.Error()}))
	}
	setUserCreds := func(creds *userCreds) {
		a.mu.Lock()
		defer a.mu.Unlock()
		var api *restClient
		if creds != nil {
			api = newRESTClient(creds.creds)
		}
		switch {
		case senderKey == "":
			a.userCreds, a.userAPI = creds, api
		case creds == nil:
			delete(a.userCredsByEmail, senderKey)
			delete(a.userAPIByEmail, senderKey)
		default:
			a.userCredsByEmail[senderKey], a.userAPIByEmail[senderKey] = creds, api
		}
	}
	switch arg {
	case "":
		path := tokenPath(ctx, senderKey)
		var creds *userCreds
		if pathExists(path) {
			creds = loadUserCredentials(ctx, senderKey)
		}
		switch {
		case creds != nil:
			who := senderKey
			if who == "" {
				who = tr(ctx, "setup_files.who_shared", nil)
			}
			reply(tr(ctx, "setup_files.active", map[string]any{"who": who, "token_path": path}))
		case !pathExists(clientSecretPath(ctx)):
			reply(tr(ctx, "setup_files.not_configured", nil))
		default:
			reply(tr(ctx, "setup_files.not_authorized_yet", nil))
		}
		return true
	case "start":
		if !pathExists(clientSecretPath(ctx)) {
			reply(tr(ctx, "setup_files.no_client_credentials", nil))
			return true
		}
		authURL, err := getAuthURL(ctx, senderKey)
		if err != nil {
			helperFailed("start", err, "start_failed")
			return true
		}
		reply(tr(ctx, "setup_files.start_instructions", map[string]any{"auth_url": authURL}))
		return true
	case "revoke":
		output, err := revokeUserToken(ctx, senderKey)
		if err != nil {
			var failure *oauthFailure
			if !errors.As(err, &failure) {
				helperFailed("revoke", err, "")
				return true
			}
			output = tr(ctx, "setup_files.revoke_completed", nil)
		} else if pystr.Strip(output) == "" {
			output = tr(ctx, "setup_files.revoked", nil)
		}
		setUserCreds(nil)
		reply(tr(ctx, "setup_files.done_output", map[string]any{"output": pystr.Strip(output)}))
		return true
	}
	output, err := exchangeAuthCode(ctx, arg, senderKey)
	if err != nil {
		helperFailed("exchange", err, "exchange_failed")
		return true
	}
	if creds := loadUserCredentials(ctx, senderKey); creds != nil {
		setUserCreds(creds)
		reply(tr(ctx, "setup_files.authorized", nil))
		return true
	}
	pkgLog().Warn("[GoogleChat] post-exchange creds load failed")
	reply(tr(ctx, "setup_files.exchanged_not_loaded", map[string]any{"token_path": tokenPath(ctx, senderKey), "output": pystr.Strip(output)}))
	return true
}
