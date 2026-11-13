package plugins

import (
	"context"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
)

// Answer of a credentialed provider request the wiring sends for a plugin
type ProviderRequestAnswer struct {
	Status  int
	Headers map[string]string
	Body    []byte
}

// Sends one provider request with the sign in of the calling plugin attached, the plugin named by its directory
func (c *Context) CredentialedProviderRequest(ctx context.Context, provider, method, url string, json any, headers map[string]string, timeout time.Duration) (*ProviderRequestAnswer, error) {
	send, err := needService("CredentialedProviderRequest", func(s *Services) func(context.Context, string, string, string, string, any, map[string]string, time.Duration) (*ProviderRequestAnswer, error) {
		return s.CredentialedProviderRequest
	})
	if err != nil {
		return nil, err
	}
	return send(ctx, c.manifest.Path, provider, method, url, json, headers, timeout)
}

func init() {
	hostCtxDispatch["credentialed_provider_request"] = hostMethod("credentialed_provider_request", []pyParam{req("provider"), req("method"), req("url"),
		kwo("json", nil), kwo("headers", nil), kwo("timeout", 0.0)},
		func(ctx context.Context, _ *PluginHost, c *Context, a, _ map[string]any) (any, error) {
			headers := map[string]string{}
			for k, v := range argMap(a["headers"]) {
				headers[k] = config.PyStr(v)
			}
			timeout := time.Duration(guestNumber(a["timeout"], 0) * float64(time.Second))
			answer, err := c.CredentialedProviderRequest(ctx, argStr(a["provider"]), argStr(a["method"]), argStr(a["url"]), a["json"], headers, timeout)
			if err != nil {
				return nil, err
			}
			out := map[string]any{}
			for k, v := range answer.Headers {
				out[k] = v
			}
			body := answer.Body
			if body == nil {
				body = []byte{}
			}
			return map[string]any{"status": int64(answer.Status), "headers": out, "body": body}, nil
		})
}
