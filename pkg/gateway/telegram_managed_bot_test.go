package gateway

import (
	"bytes"
	"context"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyhttpx"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

const (
	validTelegramToken       = "123456789:ABCDEFGHIJKLMNOPQRSTUVWXYZabcdef"
	secondValidTelegramToken = "987654321:abcdefghijklmnopqrstuvwxyzABCDEF"
)

// Captures what the onboarding client prints
func telegramOutput(t *testing.T) *bytes.Buffer {
	t.Helper()
	var out bytes.Buffer
	swapSetup(t, &telegramOut, func() io.Writer { return &out })
	return &out
}

func TestQRCodePrintQrCodeWithUrl(t *testing.T) {
	out := telegramOutput(t)
	if err := printQRCode("https://t.me/newbot/Bot/test_bot", true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "https://t.me/newbot/Bot/test_bot") {
		t.Fatalf("output %q", out.String())
	}
}

func TestQRCodePrintQrCodePrintsTheRenderedCodeAndLink(t *testing.T) {
	out := telegramOutput(t)
	swapSetup(t, &telegramRenderQR, func(string) (string, error) { return "QR-BLOCK", nil })
	if err := printQRCode("https://t.me/newbot/Bot/test_bot", true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "QR-BLOCK\n  Link: https://t.me/newbot/Bot/test_bot") || strings.Contains(out.String(), "pip install") ||
		strings.Contains(out.String(), "pm install") {
		t.Fatalf("output %q", out.String())
	}
}

// One recorded post of the pairing client
type telegramPost struct {
	url     string
	body    any
	timeout time.Duration
}

// Replaces the pairing POST with a response of the status and JSON body, recording every call
func fakeTelegramPost(t *testing.T, status int, body string) *[]telegramPost {
	t.Helper()
	posts := &[]telegramPost{}
	swapSetup(t, &telegramHTTPPost, func(_ context.Context, url string, raw []byte, timeout time.Duration) (*pyhttpx.Response, error) {
		decoded, err := pyjson.Loads(string(raw))
		if err != nil {
			t.Fatal(err)
		}
		*posts = append(*posts, telegramPost{url, pyjson.Plain(decoded), timeout})
		return &pyhttpx.Response{StatusCode: status, Content: []byte(body)}, nil
	})
	return posts
}

func TestCreatePairingSuccess(t *testing.T) {
	posts := fakeTelegramPost(t, 201, `{"pairing_id": "abcdefghijklmnop", "poll_token": "secret-token",
		"suggested_username": "hermes_abcdefghijklmnop_bot",
		"deep_link": "https://t.me/newbot/HermesSetupBot/hermes_abcdefghijklmnop_bot?name=Hermes+Agent",
		"qr_payload": "https://t.me/newbot/HermesSetupBot/hermes_abcdefghijklmnop_bot?name=Hermes+Agent",
		"expires_at": "2026-05-18T00:00:00.000Z"}`)
	pairing, err := createTelegramPairing(context.Background(), "https://api.example.com", "Hermes Agent", 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	expires := "2026-05-18T00:00:00.000Z"
	want := &TelegramPairing{PairingID: "abcdefghijklmnop", PollToken: "secret-token", SuggestedUsername: "hermes_abcdefghijklmnop_bot",
		DeepLink:  "https://t.me/newbot/HermesSetupBot/hermes_abcdefghijklmnop_bot?name=Hermes+Agent",
		QRPayload: "https://t.me/newbot/HermesSetupBot/hermes_abcdefghijklmnop_bot?name=Hermes+Agent", ExpiresAt: &expires}
	if !reflect.DeepEqual(pairing, want) {
		t.Fatalf("pairing %+v", pairing)
	}
	wantPosts := []telegramPost{{"https://api.example.com/v1/telegram/pairings", map[string]any{"bot_name": "Hermes Agent"}, 10 * time.Second}}
	if !reflect.DeepEqual(*posts, wantPosts) {
		t.Fatalf("posts %+v", *posts)
	}
}

func TestCreatePairingFailureStatus(t *testing.T) {
	fakeTelegramPost(t, 500, "")
	pairing, err := createTelegramPairing(context.Background(), "https://api.example.com", telegramDefaultBotName, 10*time.Second)
	if err != nil || pairing != nil {
		t.Fatalf("pairing %+v %v", pairing, err)
	}
}

func TestCreatePairingUsesEnvOverride(t *testing.T) {
	t.Setenv(TelegramOnboardingURLEnv, "https://worker.example")
	posts := fakeTelegramPost(t, 500, "")
	if _, err := createTelegramPairing(context.Background(), "", telegramDefaultBotName, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	if len(*posts) != 1 || (*posts)[0].url != "https://worker.example/v1/telegram/pairings" {
		t.Fatalf("posts %+v", *posts)
	}
}

// Pairing the poll cases start from
func pollPairing() *TelegramPairing {
	return &TelegramPairing{PairingID: "abcdefghijklmnop", PollToken: "secret-token", SuggestedUsername: "hermes_abcdefghijklmnop_bot",
		DeepLink: "https://t.me/newbot/HermesSetupBot/hermes_abcdefghijklmnop_bot", QRPayload: "https://t.me/newbot/HermesSetupBot/hermes_abcdefghijklmnop_bot"}
}

// One recorded GET of the poll client
type telegramGet struct {
	url     string
	headers map[string]string
}

func TestPollForTokenImmediateSuccess(t *testing.T) {
	var gets []telegramGet
	swapSetup(t, &telegramHTTPGet, func(_ context.Context, url string, headers map[string]string, _ time.Duration) (*pyhttpx.Response, error) {
		gets = append(gets, telegramGet{url, headers})
		return &pyhttpx.Response{StatusCode: 200, Content: []byte(`{"bot_username": "hermes_abcdefghijklmnop_bot", "owner_user_id": 42,
			"status": "ready", "token": "` + validTelegramToken + `"}`)}, nil
	})
	swapSetup(t, &telegramSleep, func(float64) {})
	result, err := pollForTelegramSetupResult(context.Background(), "https://api.example.com", pollPairing(), 5, telegramPollInterval, nil)
	if err != nil || result == nil || result.Token != validTelegramToken {
		t.Fatalf("result %+v %v", result, err)
	}
	last := gets[len(gets)-1]
	if last.url != "https://api.example.com/v1/telegram/pairings/abcdefghijklmnop" ||
		!reflect.DeepEqual(last.headers, map[string]string{"Authorization": "Bearer secret-token"}) {
		t.Fatalf("get %+v", last)
	}
}

func TestPollForTokenEventualSuccess(t *testing.T) {
	callCount := 0
	swapSetup(t, &telegramHTTPGet, func(context.Context, string, map[string]string, time.Duration) (*pyhttpx.Response, error) {
		callCount++
		if callCount < 3 {
			return &pyhttpx.Response{StatusCode: 200, Content: []byte(`{"status": "waiting"}`)}, nil
		}
		return &pyhttpx.Response{StatusCode: 200, Content: []byte(`{"status": "ready", "token": "` + secondValidTelegramToken + `"}`)}, nil
	})
	swapSetup(t, &telegramSleep, func(float64) {})
	result, err := pollForTelegramSetupResult(context.Background(), "https://api.example.com", pollPairing(), 30, telegramPollInterval, nil)
	if err != nil || result == nil || result.Token != secondValidTelegramToken {
		t.Fatalf("result %+v %v", result, err)
	}
}

func TestSetupTelegramAutoSetupResultPassesProfileNameForProfileHome(t *testing.T) {
	profileHome := t.TempDir() + "/.hermes/profiles/oracle"
	saved := WizardSetup
	t.Cleanup(func() { WizardSetup = saved })
	WizardSetup.HermesHome = func(context.Context) string { return profileHome }
	seen := map[string]any{}
	swapSetup(t, &telegramAutoSetupFn, func(_ context.Context, profileName *string) (*TelegramBotSetupResult, error) {
		seen["profile_name"] = *profileName
		return nil, nil
	})
	result, err := setupTelegramAutoResult(context.Background())
	if err != nil || result != nil || seen["profile_name"] != "oracle" {
		t.Fatalf("result %+v %v seen %v", result, err, seen)
	}
}

func TestSetupTelegramAutoProfileNameFromHomePathHandlesWindowsSeparators(t *testing.T) {
	if name, ok := profileNameFromHermesHomeFlavor(`C:\Users\test\AppData\Local\hermes\profiles\oracle`, true); !ok || name != "oracle" {
		t.Fatalf("name %q %v", name, ok)
	}
}
