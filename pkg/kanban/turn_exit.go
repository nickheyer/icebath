package kanban

import (
	"errors"

	"github.com/nickheyer/icebath/pkg/provider"
)

// CredentialFailureFlags reads the exit code credential flags from an error raised before any turn ran
func CredentialFailureFlags(err error) (rateLimited, terminal, found bool) {
	for e := err; e != nil; e = errors.Unwrap(e) {
		var authErr *provider.AuthError
		if errors.As(e, &authErr) {
			return provider.IsRateLimitedAuthError(authErr), authErr.ReloginRequired, true
		}
	}
	return false, false, false
}
