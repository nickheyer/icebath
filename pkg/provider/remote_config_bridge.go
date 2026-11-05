package provider

import (
	"context"
	"errors"

	"github.com/nickheyer/icebath/pkg/config/remote"
)

func init() {
	remote.RegisterNousAccessToken(remoteConfigNousToken)
}

// Nous access token from the auth.json of home, a missing login or revoked session marked fatal
func remoteConfigNousToken(ctx context.Context, h string) (string, error) {
	token, err := ResolveNousAccessToken(PoolEnv{Store: NewStore(h), Ctx: ctx}, NousAccessTokenOptions{})
	if err != nil {
		var ae *AuthError
		fatal := errors.As(err, &ae) && (ae.ReloginRequired || ae.Code == "nous_auth_missing")
		return "", &remote.NousTokenError{Err: err, Fatal: fatal}
	}
	return token, nil
}
