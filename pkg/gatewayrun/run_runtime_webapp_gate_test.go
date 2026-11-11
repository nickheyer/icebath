package gatewayrun

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Mounted dashboard behind the sign in gate, optionally declaring its own public URL
type gatedDashboardWeb struct {
	publicURL string
	minted    int
}

func (w *gatedDashboardWeb) WebDist() (string, bool)     { return "", false }
func (w *gatedDashboardWeb) PrivateLaunch() bool         { return false }
func (w *gatedDashboardWeb) ListenerPublicURL() string   { return w.publicURL }
func (w *gatedDashboardWeb) AuthRequired() bool          { return true }
func (w *gatedDashboardWeb) LaunchTicketTTLSeconds() int { return 60 }
func (w *gatedDashboardWeb) MintLaunchTicket() (string, error) {
	w.minted++
	return "ticket", nil
}
func (w *gatedDashboardWeb) MintPrintedLaunchTicket() (string, error) {
	w.minted++
	return "ticket", nil
}
func (w *gatedDashboardWeb) RotateBrowserCredential(string) map[string]int { return map[string]int{} }

type gatedDashboardMount struct{ web *gatedDashboardWeb }

func (m *gatedDashboardMount) Ensure(context.Context) (gateway.GatewayDashboardWeb, error) {
	return m.web, nil
}
func (m *gatedDashboardMount) Web() gateway.GatewayDashboardWeb { return m.web }
func (m *gatedDashboardMount) Closed() bool                     { return false }
func (m *gatedDashboardMount) Close(context.Context) error      { return nil }

const lanTestAddress = "192.168.1.20"

// Gateway serving the Webapp on every interface behind the sign in gate, registered for a callback when one is given
func lanWebapp(t *testing.T, registered, address, publicURL string) (*GatewayRunner, *gatedDashboardWeb) {
	t.Helper()
	r, _, h, err := startStubbedRuntimeAPI(t, "webapp:\n  enabled: true\n  host: 0.0.0.0\n  port: 9555\n", false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if registered != "" {
		record := filepath.Join(h, "webapp", "sign-in-registration.json")
		if err := os.MkdirAll(filepath.Dir(record), 0o700); err != nil {
			t.Fatal(err)
		}
		body := `{"portal": "https://portal.example", "client_id": "agent:x", "callback": "` + registered + `"}` + "\n"
		if err := os.WriteFile(record, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(gateway.SwapLANAddressProbe(func() string { return address }))
	web := &gatedDashboardWeb{publicURL: publicURL}
	r.sessionAPI.app = &gatedDashboardMount{web: web}
	return r, web
}

// Result of the launch verb asked by a same user peer
func launchResult(t *testing.T, r *GatewayRunner, params map[string]any) map[string]any {
	t.Helper()
	answer := askVerb(t, r.sessionControlServer, "webapp-launch", "same-user-peer", params)
	result, isDict := answer["result"].(map[string]any)
	if !isDict {
		t.Fatalf("webapp-launch answered %v", answer)
	}
	return result
}

func TestBehindTheDashboardSignInTheLauncherGetsTheWebappAddress(t *testing.T) {
	r, web := lanWebapp(t, "", "", "")
	result := launchResult(t, r, map[string]any{"profile": "work"})
	want := map[string]any{"url": "http://127.0.0.1:9555/?profile=work", "private": false, "expires_in_seconds": nil, "sign_in": true}
	if !reflect.DeepEqual(result, want) {
		t.Fatalf("launch %v", result)
	}
	if web.minted != 0 {
		t.Fatal("a launch ticket was minted behind the sign in gate")
	}
}

func TestOnTheLanTheLauncherOpensTheAddressWebAccessRegisteredForSignIn(t *testing.T) {
	r, _ := lanWebapp(t, "http://"+lanTestAddress+":9555/auth/callback", lanTestAddress, "")
	result := launchResult(t, r, map[string]any{"profile": "work"})
	want := map[string]any{"url": "http://" + lanTestAddress + ":9555/?profile=work", "private": false, "expires_in_seconds": nil, "sign_in": true}
	if !reflect.DeepEqual(result, want) {
		t.Fatalf("launch %v", result)
	}
}

func TestOnTheLanWithoutACurrentRegistrationTheLauncherOpensTheLoopbackAddress(t *testing.T) {
	for name, row := range map[string][2]string{
		"set_up_by_hand":            {"", lanTestAddress},
		"registered_before_port":    {"http://" + lanTestAddress + ":9120/auth/callback", lanTestAddress},
		"lan_address_changed":       {"http://" + lanTestAddress + ":9555/auth/callback", "192.168.1.77"},
		"registered_for_public_url": {"https://webapp.example.net/auth/callback", lanTestAddress},
	} {
		t.Run(name, func(t *testing.T) {
			r, _ := lanWebapp(t, row[0], row[1], "")
			if got := launchResult(t, r, nil)["url"]; got != "http://127.0.0.1:9555/" {
				t.Fatalf("launcher opens %v", got)
			}
		})
	}
}

func TestTheWebappsOwnPublicUrlWinsOverALanRegistration(t *testing.T) {
	r, _ := lanWebapp(t, "http://"+lanTestAddress+":9555/auth/callback", lanTestAddress, "https://webapp.example.net")
	if got := launchResult(t, r, nil)["url"]; got != "https://webapp.example.net/" {
		t.Fatalf("launcher opens %v", got)
	}
}

func TestBehindItsOwnPublicUrlTheLauncherOpensThatUrlToSignIn(t *testing.T) {
	for _, row := range []struct {
		publicURL string
		params    map[string]any
		opened    string
	}{
		{"https://webapp.example.net", nil, "https://webapp.example.net/"},
		{"https://example.net:8443/hermes", map[string]any{"profile": "work"}, "https://example.net:8443/hermes/?profile=work"},
	} {
		t.Run(row.opened, func(t *testing.T) {
			r, _ := lanWebapp(t, "", lanTestAddress, row.publicURL)
			result := launchResult(t, r, row.params)
			want := map[string]any{"url": row.opened, "private": false, "expires_in_seconds": nil, "sign_in": true}
			if !reflect.DeepEqual(result, want) {
				t.Fatalf("launch %v", result)
			}
		})
	}
}
