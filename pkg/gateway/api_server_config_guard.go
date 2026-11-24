package gateway

import (
	"context"
	"errors"
	"net/http"

	"github.com/nickheyer/icebath/pkg/config"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Answers a config backend outage with that request's 503 instead of ending the gateway
func APIConfigUnavailableMiddleware(ctx context.Context, request *WebRequest, handler WebHandler) (*WebResponse, error) {
	resp, err := handler(ctx, request)
	var unavailable *config.ConfigBackendUnavailable
	if errors.As(err, &unavailable) {
		return WebJSONResponse(jsonutil.ObjectOf("error", jsonutil.ObjectOf("message", unavailable.SystemExitCode(),
			"type", "server_error", "code", "config_backend_unavailable")), http.StatusServiceUnavailable, nil)
	}
	return resp, err
}
