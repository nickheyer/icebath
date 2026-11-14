package gateway

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

func TestGatewayAPIServerAnswers503(t *testing.T) {
	app := NewWebApplication(0)
	app.Middlewares = []WebMiddleware{APIConfigUnavailableMiddleware}
	if err := app.AddGet("/v1/x", func(context.Context, *WebRequest) (*WebResponse, error) {
		return nil, &config.ConfigBackendUnavailable{Message: "Remote Config: the config plane is unreachable"}
	}); err != nil {
		t.Fatal(err)
	}
	resp, err := app.Handle(context.Background(), NewWebRequest(httptest.NewRequest("GET", "/v1/x", nil), 0))
	status, body := webhookResponseJSON(t, resp, err)
	if status != 503 || body.GetOr("error", nil) == nil {
		t.Fatalf("%d %s", status, resp.Text())
	}
	if code := body.GetOr("error", nil).(interface{ GetOr(string, any) any }).GetOr("code", nil); code != "config_backend_unavailable" {
		t.Fatalf("code %v", code)
	}
}
