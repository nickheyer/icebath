package gateway

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGivingUpOnARefusedHandshakeNamesTheStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("remote body"))
	}))
	defer srv.Close()
	t.Setenv("ICEBATH_TUI_GATEWAY_URL", "ws"+strings.TrimPrefix(srv.URL, "http")+"/ws")
	_, err := ConnectGatewayWith(context.Background(), ConnectGatewayOptions{})
	var refused *InvalidStatusError
	if !errors.As(err, &refused) || refused.StatusCode != 403 {
		t.Fatalf("err=%#v", err)
	}
	if !kanbanWorkerLoopError(err) || !kanbanRestartable(err) {
		t.Fatal("a refused handshake must stay a restartable worker loop error")
	}
	if detail := kanbanSilentDetail(err); detail != "InvalidStatus (HTTP 403)" || strings.Contains(detail, "remote body") {
		t.Fatalf("detail %q", detail)
	}
	if detail := kanbanSilentDetail(&GatewayClientError{Msg: "Gateway connection failed; no local fallback"}); detail != "Gateway connection failed; no local fallback" {
		t.Fatalf("client error detail %q", detail)
	}
}
