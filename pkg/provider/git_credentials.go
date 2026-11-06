package provider

import (
	"context"

	"github.com/nickheyer/icebath/pkg/gitcred"
)

// Runs a git network verb anonymously, then with each credential the user owns for the remote host
var RunGitWithCredentialFallback = gitcred.RunGitWithCredentialFallback

func init() {
	gitcred.SetSecretReader(func(ctx context.Context, name string) (string, error) {
		v, _, err := GetSecret(ctx, name)
		return v, err
	})
}
