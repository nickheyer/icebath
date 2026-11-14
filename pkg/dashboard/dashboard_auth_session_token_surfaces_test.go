package dashboard

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/builtin/dashboardauth"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Interactive subclass of the session token provider with its own name and password flag
type interactiveTokenSubclass struct {
	*dashboardauth.SessionTokenProvider
	name, display string
	password      bool
}

func (s interactiveTokenSubclass) Name() string           { return s.name }
func (s interactiveTokenSubclass) DisplayName() string    { return s.display }
func (s interactiveTokenSubclass) SupportsPassword() bool { return s.password }
func (s interactiveTokenSubclass) InteractiveLogin() bool { return true }

func surfaceToken(t *testing.T) *dashboardauth.SessionTokenProvider {
	t.Helper()
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	p, err := dashboardauth.NewSessionTokenProvider(base64.RawURLEncoding.EncodeToString(b))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func surfacesCase(t *testing.T, providers ...gateway.DashboardAuthProvider) {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	gateway.ClearDashboardAuthProviders()
	t.Cleanup(gateway.ClearDashboardAuthProviders)
	for _, p := range providers {
		if err := gateway.RegisterGlobalDashboardAuthProvider(p); err != nil {
			t.Fatal(err)
		}
	}
}

// test_session_token_provider.py TestInteractiveSurfaces test_login_page_shows_token_mode_notice_not_a_button
func TestInteractiveSurfacesLoginPageShowsTokenModeNoticeNotAButton(t *testing.T) {
	surfacesCase(t, surfaceToken(t))
	html, err := renderLoginHTML(context.Background(), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(html, "Token-gated dashboard") || !strings.Contains(html, "Deployment Session Token") ||
		strings.Contains(html, "provider-btn") || strings.Contains(html, "/auth/login?provider=") {
		t.Fatal(html)
	}
}

// test_session_token_provider.py TestInteractiveSurfaces test_login_page_keeps_interactive_providers_clickable
func TestInteractiveSurfacesLoginPageKeepsInteractiveProvidersClickable(t *testing.T) {
	surfacesCase(t, surfaceToken(t), interactiveTokenSubclass{surfaceToken(t), "basic", "Username & Password", true})
	html, err := renderLoginHTML(context.Background(), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(html, "Token-gated dashboard") || !strings.Contains(html, `data-provider="basic"`) {
		t.Fatal(html)
	}
}

func providerNames(t *testing.T, got any) []string {
	t.Helper()
	obj, ok := got.(*pyjson.Object)
	if !ok {
		t.Fatalf("%T %v", got, got)
	}
	names := []string{}
	for _, p := range obj.Vals["providers"].([]any) {
		names = append(names, p.(*pyjson.Object).Vals["name"].(string))
	}
	return names
}

// test_session_token_provider.py TestInteractiveSurfaces test_api_auth_providers_filters_non_interactive
func TestInteractiveSurfacesAPIAuthProvidersFiltersNonInteractive(t *testing.T) {
	surfacesCase(t, surfaceToken(t))
	got, err := apiAuthProviders(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if names := providerNames(t, got); len(names) != 0 {
		t.Fatal(names)
	}
	if err := gateway.RegisterGlobalDashboardAuthProvider(interactiveTokenSubclass{surfaceToken(t), "nous", "Nous Research", false}); err != nil {
		t.Fatal(err)
	}
	got, err = apiAuthProviders(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if names := providerNames(t, got); !reflect.DeepEqual(names, []string{"nous"}) {
		t.Fatal(names)
	}
}
