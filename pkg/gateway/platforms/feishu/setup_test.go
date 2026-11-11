package feishu

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Swaps a package seam for one test
func swap[T any](t *testing.T, p *T, v T) {
	t.Helper()
	old := *p
	*p = v
	t.Cleanup(func() { *p = old })
}

// One urlopen call a fake saw, its request and form body
type openedRequest struct {
	req  *http.Request
	body string
}

// Fake urlopen answering the JSON bodies in order and recording each request
func fakeURLOpen(t *testing.T, bodies ...string) *[]openedRequest {
	t.Helper()
	seen := &[]openedRequest{}
	swap(t, &urlopenFn, func(_ context.Context, req *http.Request, _ time.Duration) ([]byte, error) {
		raw, _ := io.ReadAll(req.Body)
		*seen = append(*seen, openedRequest{req, string(raw)})
		body := bodies[0]
		if len(bodies) > 1 {
			bodies = bodies[1:]
		}
		return []byte(body), nil
	})
	return seen
}

func TestPostRegistrationPostRegistrationSendsFormEncodedBody(t *testing.T) {
	seen := fakeURLOpen(t, `{}`)
	if _, err := postRegistration(context.Background(), "https://accounts.feishu.cn", [][2]string{{"action", "init"}, {"key", "val"}}); err != nil {
		t.Fatal(err)
	}
	r := (*seen)[0]
	if !strings.Contains(r.body, "action=init") || !strings.Contains(r.body, "key=val") || r.req.Header.Get("Content-type") != "application/x-www-form-urlencoded" {
		t.Fatalf("request %q %v", r.body, r.req.Header)
	}
}

func TestInitRegistrationInitSucceedsWhenClientSecretSupported(t *testing.T) {
	fakeURLOpen(t, `{"nonce": "abc", "supported_auth_methods": ["client_secret"]}`)
	if err := initRegistration(context.Background(), "feishu"); err != nil {
		t.Fatal(err)
	}
}

func TestInitRegistrationInitRaisesWhenClientSecretNotSupported(t *testing.T) {
	fakeURLOpen(t, `{"nonce": "abc", "supported_auth_methods": ["other_method"]}`)
	err := initRegistration(context.Background(), "feishu")
	if _, ok := err.(*RuntimeError); !ok || !strings.Contains(err.Error(), "client_secret") {
		t.Fatalf("error %v", err)
	}
}

func TestBeginRegistrationBeginReturnsDeviceCodeAndQrUrl(t *testing.T) {
	fakeURLOpen(t, `{"device_code": "dc_123", "verification_uri_complete": "https://accounts.feishu.cn/qr/abc", "user_code": "ABCD-1234", "interval": 5, "expire_in": 600}`)
	res, err := beginRegistration(context.Background(), "feishu")
	if err != nil {
		t.Fatal(err)
	}
	if res.DeviceCode != "dc_123" || !strings.Contains(res.QRURL, "accounts.feishu.cn") || res.UserCode != "ABCD-1234" ||
		res.Interval != int64(5) || res.ExpireIn != int64(600) {
		t.Fatalf("begin %+v", res)
	}
}

// Monotonic clock answering the values in order like a patched time.monotonic side effect
func scriptedClock(t *testing.T, values ...float64) {
	t.Helper()
	swap(t, &pollMonotonic, func() float64 {
		v := values[0]
		values = values[1:]
		return v
	})
	swap(t, &pollSleep, func(*gateway.InterruptWatch, float64) error { return nil })
}

// Captured setup console of one test
func captureOutput(t *testing.T) *bytes.Buffer {
	t.Helper()
	var out bytes.Buffer
	t.Cleanup(provider.WithSetupOutput(&out))
	return &out
}

func TestPollRegistrationPollReturnsCredentialsOnSuccess(t *testing.T) {
	captureOutput(t)
	scriptedClock(t, 0, 1)
	fakeURLOpen(t, `{"client_id": "cli_app123", "client_secret": "secret456", "user_info": {"open_id": "ou_owner", "tenant_brand": "feishu"}}`)
	w := gateway.WatchInterrupt()
	defer w.Stop()
	res, err := pollRegistration(context.Background(), w, "dc_123", 1, 60, "feishu")
	if err != nil || res == nil || res.AppID != "cli_app123" || res.AppSecret != "secret456" || res.Domain != "feishu" || res.OpenID != "ou_owner" {
		t.Fatalf("result %+v %v", res, err)
	}
}

func TestPollRegistrationPollSwitchesDomainOnLarkTenantBrand(t *testing.T) {
	captureOutput(t)
	scriptedClock(t, 0, 1, 2)
	fakeURLOpen(t, `{"error": "authorization_pending", "user_info": {"tenant_brand": "lark"}}`,
		`{"client_id": "cli_lark", "client_secret": "secret_lark", "user_info": {"open_id": "ou_lark", "tenant_brand": "lark"}}`)
	w := gateway.WatchInterrupt()
	defer w.Stop()
	res, err := pollRegistration(context.Background(), w, "dc_123", 0, 60, "feishu")
	if err != nil || res == nil || res.Domain != "lark" {
		t.Fatalf("result %+v %v", res, err)
	}
}

// Begin result the qr_register cases start from
func scriptedBegin() *beginResult {
	return &beginResult{DeviceCode: "dc_123", QRURL: "https://example.com/qr", UserCode: "ABCD", Interval: int64(1), ExpireIn: int64(60)}
}

// Replaces init, begin and render with recording fakes, the poll answering result
func stubQRSteps(t *testing.T, result *Registration, rendered bool) (*int, *int) {
	t.Helper()
	inits, renders := 0, 0
	swap(t, &initRegistrationFn, func(context.Context, string) error { inits++; return nil })
	swap(t, &beginRegistrationFn, func(context.Context, string) (*beginResult, error) { return scriptedBegin(), nil })
	swap(t, &pollRegistrationFn, func(context.Context, *gateway.InterruptWatch, any, float64, float64, string) (*Registration, error) {
		return result, nil
	})
	swap(t, &renderQRFn, func(string) bool { renders++; return rendered })
	return &inits, &renders
}

func TestQrRegisterQrRegisterSuccessFlow(t *testing.T) {
	captureOutput(t)
	inits, renders := stubQRSteps(t, &Registration{AppID: "cli_app", AppSecret: "secret", Domain: "feishu", OpenID: "ou_owner"}, true)
	swap(t, &probeBotFn, func(context.Context, any, any, string) (map[string]any, error) {
		return map[string]any{"bot_name": "MyBot", "bot_open_id": "ou_bot"}, nil
	})
	res, err := QRRegister(context.Background(), "feishu", 600)
	if err != nil || res == nil || res.AppID != "cli_app" || res.AppSecret != "secret" || res.BotName != "MyBot" || *inits != 1 || *renders != 1 {
		t.Fatalf("result %+v %v inits %d renders %d", res, err, *inits, *renders)
	}
}

func TestQrRegisterQrRegisterReturnsNoneOnPollFailure(t *testing.T) {
	captureOutput(t)
	stubQRSteps(t, nil, true)
	if res, err := QRRegister(context.Background(), "feishu", 600); res != nil || err != nil {
		t.Fatalf("result %+v %v", res, err)
	}
}

func TestQrRegisterQrFallbackTipTargetsActiveInterpreter(t *testing.T) {
	out := captureOutput(t)
	stubQRSteps(t, nil, false)
	res, err := qrRegisterInner(context.Background(), "feishu", 60)
	if res != nil || err != nil {
		t.Fatalf("result %+v %v", res, err)
	}
	text := out.String()
	if !strings.Contains(text, "https://example.com/qr") || !strings.Contains(text, pm.InstallHint("messaging")) || strings.Contains(text, "pip install") {
		t.Fatalf("output %q", text)
	}
}

func TestQrRegisterQrRegisterSucceedsEvenWhenProbeFails(t *testing.T) {
	captureOutput(t)
	stubQRSteps(t, &Registration{AppID: "cli_app", AppSecret: "secret", Domain: "feishu", OpenID: "ou_owner"}, true)
	swap(t, &probeBotFn, func(context.Context, any, any, string) (map[string]any, error) { return nil, nil })
	res, err := QRRegister(context.Background(), "feishu", 600)
	if err != nil || res == nil || res.AppID != "cli_app" || res.BotName != nil || res.BotOpenID != nil {
		t.Fatalf("result %+v %v", res, err)
	}
}

// Runs the wizard with scripted choices and prompts and a fixed QR result, returning the env saves and removals
func runSetupFeishu(t *testing.T, qr *Registration, choices []int, prompts []string, existing map[string]string) (map[string]string, []string) {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	captureOutput(t)
	savedCLI, savedConfig, savedSetup := gateway.WizardCLI, gateway.WizardConfig, gateway.WizardSetup
	t.Cleanup(func() {
		gateway.WizardCLI, gateway.WizardConfig, gateway.WizardSetup = savedCLI, savedConfig, savedSetup
	})
	saved, removed := map[string]string{}, []string{}
	gateway.WizardConfig.SaveEnvValue = func(_ context.Context, k, v string) error { saved[k] = v; return nil }
	gateway.WizardConfig.GetEnvValue = func(_ context.Context, k string) (string, error) { return existing[k], nil }
	gateway.WizardConfig.RemoveEnvValue = func(_ context.Context, k string) (bool, error) {
		removed = append(removed, k)
		if _, ok := existing[k]; ok {
			delete(existing, k)
			return true, nil
		}
		if _, ok := saved[k]; ok {
			delete(saved, k)
			return true, nil
		}
		return false, nil
	}
	gateway.WizardSetup.PromptChoice = func(string, []string, int) int {
		v := choices[0]
		choices = choices[1:]
		return v
	}
	gateway.WizardCLI.Prompt = func(string, string, bool) string {
		v := prompts[0]
		prompts = prompts[1:]
		return v
	}
	for _, p := range []*func(string){&gateway.WizardCLI.PrintHeader, &gateway.WizardCLI.PrintInfo, &gateway.WizardCLI.PrintSuccess,
		&gateway.WizardCLI.PrintWarning, &gateway.WizardCLI.PrintError} {
		*p = func(string) {}
	}
	swap(t, &qrRegisterFn, func(context.Context) (*Registration, error) { return qr, nil })
	if err := InteractiveSetup(context.Background()); err != nil {
		t.Fatal(err)
	}
	return saved, removed
}

func TestSetupFeishuConnectionModeManualPathWebsocket(t *testing.T) {
	swap(t, &probeBotFn, func(context.Context, any, any, string) (map[string]any, error) { return nil, nil })
	env, _ := runSetupFeishu(t, nil, []int{1, 0, 0, 0, 0}, []string{"cli_manual", "secret_manual", ""}, map[string]string{})
	if env["FEISHU_CONNECTION_MODE"] != "websocket" {
		t.Fatalf("env %v", env)
	}
}

func TestSetupFeishuDmPolicyAllowlistSetsFeishuAllowAllFalseWithList(t *testing.T) {
	qr := &Registration{AppID: "cli_test", AppSecret: "s", Domain: "feishu", OpenID: "ou_owner"}
	env, _ := runSetupFeishu(t, qr, []int{0, 2, 0}, []string{"ou_user1,ou_user2", ""}, map[string]string{})
	if env["FEISHU_ALLOW_ALL_USERS"] != "false" || env["FEISHU_ALLOWED_USERS"] != "ou_user1,ou_user2" {
		t.Fatalf("env %v", env)
	}
	if _, ok := env["GATEWAY_ALLOW_ALL_USERS"]; ok {
		t.Fatalf("env %v", env)
	}
}

func TestSetupFeishuGroupPolicyOpenWithMention(t *testing.T) {
	env, _ := runSetupFeishu(t, &Registration{AppID: "cli_test", AppSecret: "s", Domain: "feishu"}, []int{0, 0, 0}, []string{""}, map[string]string{})
	if env["FEISHU_GROUP_POLICY"] != "open" {
		t.Fatalf("env %v", env)
	}
}

func TestSetupFeishuHomeChannelBlankRemovesExistingHomeChannel(t *testing.T) {
	env, removed := runSetupFeishu(t, &Registration{AppID: "cli_test", AppSecret: "s", Domain: "feishu"}, []int{0, 0, 0}, []string{""},
		map[string]string{"FEISHU_HOME_CHANNEL": "chat_old"})
	found := false
	for _, k := range removed {
		found = found || k == "FEISHU_HOME_CHANNEL"
	}
	if _, ok := env["FEISHU_HOME_CHANNEL"]; !found || ok {
		t.Fatalf("removed %v env %v", removed, env)
	}
}
