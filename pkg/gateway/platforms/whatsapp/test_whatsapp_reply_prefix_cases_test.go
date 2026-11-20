package whatsapp

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func TestBridgeEnvironmentReplyPrefixReachesBridgeWithExistingPrecedence(t *testing.T) {
	for _, tc := range []struct {
		configured, explicitEnv, want string
		hasEnv                        bool
	}{{`Custom Bot\n`, "", `Custom Bot\n`, false}, {"", "", "", false}, {`Config Bot\n`, `Env Bot\n`, `Env Bot\n`, true}} {
		hermeticHome(t)
		os.Unsetenv("WHATSAPP_REPLY_PREFIX")
		if tc.hasEnv {
			t.Setenv("WHATSAPP_REPLY_PREFIX", tc.explicitEnv)
		}
		a, err := NewWhatsAppAdapter(context.Background(), &gateway.PlatformConfig{Enabled: true, Extra: map[string]any{"reply_prefix": tc.configured}})
		if err != nil {
			t.Fatal(err)
		}
		env, err := a.bridgeEnv(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if got, ok := env["WHATSAPP_REPLY_PREFIX"]; !ok || got != tc.want {
			t.Fatalf("configured %q env %q: got %q present=%v", tc.configured, tc.explicitEnv, got, ok)
		}
	}
}

func TestReadReceiptPolicyOrderingAcceptedReceiptKeyIsSentToBridge(t *testing.T) {
	hermeticHome(t)
	a, err := NewWhatsAppAdapter(context.Background(), &gateway.PlatformConfig{Enabled: true, Extra: map[string]any{"send_read_receipts": true}})
	if err != nil {
		t.Fatal(err)
	}
	var path string
	var body map[string]any
	fakeBridgeServer(t, a, func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&body)
	})
	a.httpSession = &http.Client{}
	key := map[string]any{"id": "incoming-1", "remoteJid": "120363001234567890@g.us", "participant": "15550001111@s.whatsapp.net", "fromMe": false}
	raw, _ := json.Marshal(map[string]any{"readReceiptKey": key})
	data, _ := pyjson.LoadsDict(string(raw))
	a.sendReadReceipt(context.Background(), data)
	if path != "/read" || !reflect.DeepEqual(body, map[string]any{"key": key}) {
		t.Fatalf("path = %q body = %v", path, body)
	}
}
