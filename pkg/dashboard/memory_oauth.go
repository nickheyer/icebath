package dashboard

import (
	"context"
	"errors"
	"fmt"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/profiles"
)

// OAuth flow of the named memory provider, 404 for a non identifier or a provider without one
func resolveMemoryOAuthFlow(ctx context.Context, provider string) (memory.OAuthFlow, error) {
	if !pystr.IsIdentifier(provider) {
		return nil, NewHTTPException(404, "unknown memory provider "+pyrepr.Repr(provider))
	}
	flow, err := memory.ProviderOAuthFlow(ctx, nil, provider)
	if err != nil {
		return nil, NewHTTPException(404, provider+" does not support OAuth connect")
	}
	return flow, nil
}

// Context resolving config in the requested profile home, unchanged for none, empty or current
func scopeMemoryOAuthProfile(ctx context.Context, profile *string) (context.Context, error) {
	requested := ""
	if profile != nil {
		requested = pystr.Strip(*profile)
	}
	if requested == "" || pystr.Lower(requested) == "current" {
		return ctx, nil
	}
	if err := profiles.ValidateProfileName(requested); err != nil {
		return ctx, NewHTTPException(400, err.Error())
	}
	if !profiles.ProfileExists(requested) {
		return ctx, NewHTTPException(404, fmt.Sprintf("Profile '%s' does not exist.", requested))
	}
	dir, err := profiles.GetProfileDir(requested)
	if err != nil {
		return ctx, err
	}
	return home.WithOverride(ctx, dir), nil
}

// Runs one flow call in the profile scope, an HTTP error passing through and any other failure a 500
func runMemoryOAuth(ctx context.Context, c *Call, failure string, call func(context.Context, memory.OAuthFlow) (any, error)) (any, error) {
	provider := c.Str("provider")
	profile, _ := c.Value("profile").(string)
	var profilePtr *string
	if c.Value("profile") != nil {
		profilePtr = &profile
	}
	result, err := func() (any, error) {
		sctx, err := scopeMemoryOAuthProfile(ctx, profilePtr)
		if err != nil {
			return nil, err
		}
		flow, err := resolveMemoryOAuthFlow(sctx, provider)
		if err != nil {
			return nil, err
		}
		return call(sctx, flow)
	}()
	if err != nil {
		var he *HTTPException
		if errors.As(err, &he) {
			return nil, err
		}
		return nil, NewHTTPException(500, fmt.Sprintf(failure, provider, err.Error()))
	}
	return result, nil
}

// Memory provider OAuth connect routes the web server mounts first
func memoryOAuthRouter() *APIRouter {
	r := NewAPIRouter("/api/memory/providers")
	params := []Param{PathParam("provider", Str), QueryParam("profile", Optional(Str), nil)}
	r.Post("/{provider}/oauth/start", &Endpoint{Name: "start_memory_oauth",
		Doc:    "Begin a provider's zero-CLI OAuth flow (browser + loopback listener); returns immediately, poll status.",
		Params: params,
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return runMemoryOAuth(ctx, c, "Failed to start %s OAuth: %s", func(ctx context.Context, flow memory.OAuthFlow) (any, error) {
				return flow.StartLoopbackFlowBackground(ctx)
			})
		}})
	r.Get("/{provider}/oauth/status", &Endpoint{Name: "memory_oauth_status",
		Doc:    "Poll a provider's OAuth flow: idle | pending | connected | error.",
		Params: params,
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return runMemoryOAuth(ctx, c, "Failed to read %s OAuth status: %s", func(ctx context.Context, flow memory.OAuthFlow) (any, error) {
				return flow.FlowStatus(ctx)
			})
		}})
	return r
}
