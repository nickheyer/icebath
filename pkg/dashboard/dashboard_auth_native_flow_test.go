package dashboard

import (
	"errors"
	"fmt"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Kind of a native flow failure, empty when err is none
func nativeFlowKind(err error) string {
	var nf *NativeFlowError
	if errors.As(err, &nf) {
		return nf.PyTypeName()
	}
	return ""
}

func TestNativeLoginCodeRedeemsOnceThroughTheTokenRoute(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	resetNativeFlow()
	t.Cleanup(resetNativeFlow)
	verifier := "desktop-verifier-0123456789abcdefghijklmnopqrstuvwxyz"
	session := &gateway.DashboardSession{UserID: "u1", Provider: "local", ExpiresAt: 1999999999, AccessToken: "at", RefreshToken: "rt"}
	login := func() string {
		t.Helper()
		broker, err := RegisterNativePending(s256(verifier), "http://127.0.0.1:5000/cb", "client-state", "127.0.0.1", nil)
		if err != nil {
			t.Fatal(err)
		}
		for range 2 {
			if pending, err := GetNativePending(broker, nil); err != nil || pending.ClientState != "client-state" {
				t.Fatalf("peek = %v, %v", pending, err)
			}
		}
		code, err := CompleteNativePending(broker, session, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := CompleteNativePending(broker, session, nil); nativeFlowKind(err) != "PendingNotFound" {
			t.Fatalf("second completion = %v", err)
		}
		if _, err := GetNativePending(broker, nil); nativeFlowKind(err) != "PendingNotFound" {
			t.Fatalf("peek after completion = %v", err)
		}
		return code
	}
	app := NewFastAPI("t", "1", nil, nil)
	app.IncludeRouter(dashboardAuthRouter(), "")
	c := newTestClient(t, app)
	redeem := func(code, verifier string) *testResponse {
		body := fmt.Sprintf(`{"code":%q,"code_verifier":%q}`, code, verifier)
		return c.request("POST", "/auth/native/token", []byte(body), "content-type", "application/json")
	}
	invalid := `{"detail":"Invalid or expired authorization code."}`
	code := login()
	redeem(code, verifier).expect(t, 200,
		`{"access_token":"at","refresh_token":"rt","token_type":"Bearer","expires_at":1999999999,"provider":"local","user_id":"u1"}`)
	redeem(code, verifier).expect(t, 400, invalid)
	code = login()
	redeem(code, "wrong-verifier").expect(t, 400, invalid)
	redeem(code, verifier).expect(t, 400, invalid)
	limit := 8
	for i := range limit {
		if _, err := RegisterNativePending("cc", "http://127.0.0.1:1/cb", fmt.Sprint(i), "10.0.0.9", nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := RegisterNativePending("cc", "http://127.0.0.1:1/cb", "over", "10.0.0.9", nil); nativeFlowKind(err) != "NativeFlowError" {
		t.Fatalf("per address cap = %v", err)
	}
}
