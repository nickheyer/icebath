package transport

import (
	"cmp"
	"context"
	"errors"

	"github.com/nickheyer/icebath/internal/provider"
)

// Most times one request is sent again with a renewed credential or another account
const recoveries = 8

// Client whose route's credential is kept: each request counted on it, and a refused one sent again with the credential renewed or with
// another account, as long as nothing of the answer streamed
type kept struct {
	registry *Registry
	client   provider.Client
}

func (k *kept) Route() provider.Route {
	return k.client.Route()
}

func (k *kept) Stream(ctx context.Context, request *provider.Request, fn provider.StreamFunc) (*provider.Response, error) {
	for attempt := 0; ; attempt++ {
		route := k.client.Route()
		if route.Keeper == nil {
			return k.client.Stream(ctx, request, fn)
		}
		route.Keeper.Sent()
		streamed := false
		response, err := k.client.Stream(ctx, request, func(event provider.StreamEvent) error {
			streamed = streamed || event.Delivers()
			if fn == nil {
				return nil
			}
			return fn(event)
		})
		failure, refused := errors.AsType[*provider.APIError](err)
		if err == nil || !refused || streamed || ctx.Err() != nil || attempt >= recoveries {
			return response, err
		}
		next, ok := route.Keeper.Recover(ctx, failure)
		if !ok {
			return response, err
		}
		client, made := k.registry.make(next)
		if made != nil {
			return response, err
		}
		if next.Account == route.Account && fn != nil {
			_ = fn(provider.StreamEvent{Kind: provider.EventStatus, Text: k.renewed(route, failure)})
		}
		if next.Model != route.Model {
			copied := *request
			copied.Model = next.Model
			request = &copied
		}
		k.client = client
	}
}

// Line once the route's credential was renewed after the refusal: the kind's own, else the generic one for a 401 or a stale credential
func (k *kept) renewed(route provider.Route, failure *provider.APIError) string {
	reason := "stale"
	if failure.StatusCode == 401 || failure.Kind == provider.ErrAuth {
		reason = "unauthorized"
	}
	var spec provider.Spec
	if k.registry.host != nil && k.registry.host.providers != nil {
		spec, _ = k.registry.host.providers.Spec(route.Spec)
	}
	if spec.RefreshNotice != nil {
		if notice := spec.RefreshNotice(reason); notice != "" {
			return notice
		}
	}
	name := cmp.Or(spec.DisplayName, spec.Name, route.Provider)
	if reason == "unauthorized" {
		return name + " refused the sign-in, so icebath renewed it. Trying again."
	}
	return name + " refused a sign-in that had gone stale, so icebath renewed it. Trying again."
}
