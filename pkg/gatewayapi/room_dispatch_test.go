package gatewayapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func TestNonRoomRunBodyPassesThroughUnchanged(t *testing.T) {
	adapter := &APIServerAdapter{}
	request := gateway.NewWebRequest(httptest.NewRequest("POST", "/v1/runs", nil), 0)
	body := pyjson.ObjectOf("input", "ordinary run")
	normalized, errResp, err := adapter.normalizeRoomDispatch(context.Background(), request, body)
	if err != nil || errResp != nil {
		t.Fatalf("err = %v, response = %v", err, errResp)
	}
	if normalized != any(body) {
		t.Fatalf("normalized = %v, want the body itself", normalized)
	}
}

func TestRoomDispatchRejectsExtraFieldsBeforeGrantVerification(t *testing.T) {
	adapter := &APIServerAdapter{}
	wire := httptest.NewRequest("POST", "/v1/runs", nil)
	wire.Header.Set("Authorization", "HermesRoom room-grant")
	request := gateway.NewWebRequest(wire, 0)
	body := pyjson.ObjectOf("input", "room prompt", "hosted_room_dispatch", pyjson.NewObject(), "unexpected", true)
	normalized, errResp, err := adapter.normalizeRoomDispatch(context.Background(), request, body)
	if err != nil {
		t.Fatal(err)
	}
	if normalized != any(body) || errResp == nil || errResp.Status != http.StatusBadRequest {
		t.Fatalf("normalized = %v, response = %v", normalized, errResp)
	}
	envelope, _ := jsonBody(t, errResp.Body)["error"].(map[string]any)
	if envelope["code"] != "invalid_room_dispatch" {
		t.Fatalf("error = %v", envelope)
	}
}
