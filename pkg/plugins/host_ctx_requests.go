package plugins

import (
	"context"
	"errors"
	"io/fs"
	"time"

	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
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
	hostCtxDispatch["credentialed_provider_request"] = hostMethod("credentialed_provider_request", []paramValue{req("provider"), req("method"), req("url"),
		kwo("json", nil), kwo("headers", nil), kwo("timeout", 0.0)},
		func(ctx context.Context, _ *PluginHost, c *Context, a, _ map[string]any) (any, error) {
			headers := map[string]string{}
			for k, v := range argMap(a["headers"]) {
				headers[k] = anyutil.String(v)
			}
			timeout := guestDuration(guestNumber(a["timeout"], 0))
			answer, err := c.CredentialedProviderRequest(ctx, argStr(a["provider"]), argStr(a["method"]), argStr(a["url"]), a["json"], headers, timeout)
			if errors.Is(err, fs.ErrPermission) {
				return nil, &HostBuiltinError{Type: "PermissionError", Message: err.Error()}
			}
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
