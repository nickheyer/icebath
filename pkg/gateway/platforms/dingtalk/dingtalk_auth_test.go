package dingtalk

import (
	"bytes"
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/hermetic"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrequests"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/provider"
)

func TestMain(m *testing.M) {
	hermetic.BlockExternalHTTP()
	hermetic.BlockExternalDNS()
	os.Exit(m.Run())
}

// Swaps a package seam for one test
func swap[T any](t *testing.T, p *T, v T) {
	t.Helper()
	old := *p
	*p = v
	t.Cleanup(func() { *p = old })
}

// Registration error text of err, failing when err is another kind
func registrationText(t *testing.T, err error) string {
	t.Helper()
	var reg *RegistrationError
	if !errors.As(err, &reg) {
		t.Fatalf("error %v is not a RegistrationError", err)
	}
	return reg.Msg
}

func TestApiPostRaisesOnNetworkError(t *testing.T) {
	swap(t, &registrationPost, func(context.Context, string, any) (*pyrequests.Response, error) {
		return nil, &pyrequests.Error{Class: "ConnectionError", Text: "nope"}
	})
	_, err := apiPost(context.Background(), "/app/registration/init", pyjson.ObjectOf("source", "icebath"))
	if !strings.Contains(registrationText(t, err), "Network error") {
		t.Fatalf("error %v", err)
	}
}

func TestApiPostRaisesOnNonzeroErrcode(t *testing.T) {
	swap(t, &registrationPost, func(context.Context, string, any) (*pyrequests.Response, error) {
		return &pyrequests.Response{Status: 200, Body: []byte(`{"errcode": 42, "errmsg": "boom"}`)}, nil
	})
	_, err := apiPost(context.Background(), "/app/registration/init", pyjson.ObjectOf("source", "icebath"))
	if !strings.Contains(registrationText(t, err), "boom (errcode=42)") {
		t.Fatalf("error %v", err)
	}
}

// Fake of the registration API answering the decoded responses in order
func scriptedAPIPost(t *testing.T, responses ...string) {
	t.Helper()
	swap(t, &apiPostFn, func(context.Context, string, *pyjson.Object) (*pyjson.Object, error) {
		decoded, err := pyjson.Loads(responses[0])
		responses = responses[1:]
		if err != nil {
			t.Fatal(err)
		}
		return decoded.(*pyjson.Object), nil
	})
}

func TestBeginRegistrationChainsInitThenBegin(t *testing.T) {
	scriptedAPIPost(t, `{"errcode": 0, "nonce": "nonce123"}`, `{"errcode": 0, "device_code": "dev-xyz",
		"verification_uri_complete": "https://open-dev.dingtalk.com/openapp/registration/openClaw?user_code=ABCD", "expires_in": 7200, "interval": 2}`)
	reg, err := beginRegistration(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if reg.DeviceCode != "dev-xyz" || reg.VerificationURIComplete == "" || reg.Interval != 2 || reg.ExpiresIn != 7200 {
		t.Fatalf("registration %+v", reg)
	}
}

func TestBeginRegistrationMissingNonceRaises(t *testing.T) {
	swap(t, &apiPostFn, func(context.Context, string, *pyjson.Object) (*pyjson.Object, error) {
		return pyjson.ObjectOf("errcode", int64(0), "nonce", ""), nil
	})
	_, err := beginRegistration(context.Background())
	if !strings.Contains(registrationText(t, err), "missing nonce") {
		t.Fatalf("error %v", err)
	}
}

// Fake poll answering the results in order and a no op sleep
func scriptedPolls(t *testing.T, results ...pollResult) {
	t.Helper()
	swap(t, &pollFn, func(context.Context, string) (*pollResult, error) {
		r := results[0]
		if len(results) > 1 {
			results = results[1:]
		}
		return &r, nil
	})
	swap(t, &authSleep, func(float64) {})
}

func TestWaitForSuccessReturnsCredentialsOnSuccess(t *testing.T) {
	scriptedPolls(t, pollResult{Status: "WAITING"}, pollResult{Status: "WAITING"}, pollResult{Status: "SUCCESS", ClientID: "cid-1", ClientSecret: "sec-1"})
	cid, secret, err := waitForRegistrationSuccess(context.Background(), "dev", 0, 60, nil)
	if err != nil || cid != "cid-1" || secret != "sec-1" {
		t.Fatalf("got %q %q %v", cid, secret, err)
	}
}

func TestWaitForSuccessSuccessWithoutCredentialsRaises(t *testing.T) {
	scriptedPolls(t, pollResult{Status: "SUCCESS"})
	_, _, err := waitForRegistrationSuccess(context.Background(), "dev", 0, 60, nil)
	if !strings.Contains(registrationText(t, err), "credentials are missing") {
		t.Fatalf("error %v", err)
	}
}

func TestWaitForSuccessInvokesWaitingCallback(t *testing.T) {
	scriptedPolls(t, pollResult{Status: "WAITING"}, pollResult{Status: "WAITING"}, pollResult{Status: "SUCCESS", ClientID: "cid", ClientSecret: "sec"})
	calls := 0
	if _, _, err := waitForRegistrationSuccess(context.Background(), "dev", 0, 60, func() { calls++ }); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("callback ran %d times", calls)
	}
}

func TestRenderQRReturnsFalseWhenQrcodeMissing(t *testing.T) {
	swap(t, &qrcodeAvailable, func() bool { return false })
	if ok, err := renderQRToTerminal("https://example.com"); ok || err != nil {
		t.Fatalf("rendered %v %v", ok, err)
	}
}

func TestRenderQRPrintsWhenQrcodeAvailable(t *testing.T) {
	var out bytes.Buffer
	t.Cleanup(provider.WithSetupOutput(&out))
	ok, err := renderQRToTerminal("https://example.com/test")
	if !ok || err != nil || len(out.String()) <= 100 {
		t.Fatalf("rendered %v %v output %d bytes", ok, err, len(out.String()))
	}
}

func TestMissingQrcodeEnablesDingtalkWithoutSwitchingTheLiveProcess(t *testing.T) {
	for _, succeeds := range []bool{true, false} {
		t.Run(map[bool]string{true: "succeeds", false: "fails"}[succeeds], func(t *testing.T) {
			swap(t, &qrcodeAvailable, func() bool { return false })
			var calls [][]any
			swap(t, &authSyncVenv, func(_ context.Context, o pm.SyncVenvOptions) error {
				calls = append(calls, []any{append([]string(nil), o.Extras...), o.Explicit})
				if !succeeds {
					return pm.NewInstallError("venv", "offline", "")
				}
				return nil
			})
			installed, err := ensureQRCodeInstalled(context.Background())
			if err != nil || installed {
				t.Fatalf("installed %v %v", installed, err)
			}
			if !reflect.DeepEqual(calls, [][]any{{[]string{"dingtalk"}, true}}) {
				t.Fatalf("calls %v", calls)
			}
			if ok, _ := renderQRToTerminal("https://example.com"); ok || qrcodeAvailable() {
				t.Fatal("the live process switched to rendering QR codes")
			}
		})
	}
}

func TestAvailableQrcodeDoesNotInstall(t *testing.T) {
	swap(t, &authSyncVenv, func(context.Context, pm.SyncVenvOptions) error {
		t.Fatal("installed dependency must not trigger a transaction")
		return nil
	})
	if installed, err := ensureQRCodeInstalled(context.Background()); !installed || err != nil {
		t.Fatalf("installed %v %v", installed, err)
	}
}
