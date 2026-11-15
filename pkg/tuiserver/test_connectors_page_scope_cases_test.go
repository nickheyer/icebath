package tuiserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/provider"
)

// Bearer of each connectors listing the Connectors page sends for the secondary profile's account under multiplexing
func secondaryPageListBearers(t *testing.T, beeEnv string) []string {
	t.Helper()
	launch, bee := wsLegacyHomes(t)
	var mu sync.Mutex
	var bearers []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/v1/connectors") {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		bearers = append(bearers, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{}})
	}))
	t.Cleanup(srv.Close)
	t.Setenv("ICEBATH_SHARED_AUTH_DIR", t.TempDir())
	t.Setenv("CONNECTOR_GATEWAY_URL", srv.URL)
	t.Setenv("TOOL_GATEWAY_USER_TOKEN", "launch-override")
	if err := os.WriteFile(filepath.Join(bee, ".env"), []byte("CONNECTOR_GATEWAY_URL="+srv.URL+"\n"+beeEnv), 0o600); err != nil {
		t.Fatal(err)
	}
	ticketGuestHome(t, launch, "launch-token", srv.URL)
	ticketGuestHome(t, bee, "bee-token", srv.URL)
	provider.SetMultiplexActive(true)
	t.Cleanup(func() { provider.SetMultiplexActive(false) })
	wsLegacyCall(t, context.Background(), wsLegacyActor(bee), "connectors.list", map[string]any{"owner": map[string]any{"type": "account"}})
	mu.Lock()
	defer mu.Unlock()
	return append([]string(nil), bearers...)
}

func TestConnectorsPageOfAServedProfileUsesItsOwnGatewayTokenOverride(t *testing.T) {
	if bearers := secondaryPageListBearers(t, "TOOL_GATEWAY_USER_TOKEN=bee-override\n"); len(bearers) != 1 || bearers[0] != "Bearer bee-override" {
		t.Fatalf("connectors listing bearers %v, want the served profile's override", bearers)
	}
}

func TestConnectorsPageOfAServedProfileNeverBorrowsTheLaunchTokenOverride(t *testing.T) {
	if bearers := secondaryPageListBearers(t, ""); len(bearers) != 1 || bearers[0] != "Bearer bee-token" {
		t.Fatalf("connectors listing bearers %v, want the served profile's own login token", bearers)
	}
}

func TestConnectorsPageOfAServedProfileWithNoIdentityIgnoresTheLaunchOverrideForAvailability(t *testing.T) {
	connectorsPageFixture(t, true)
	_, bee := wsLegacyHomes(t)
	t.Setenv("TOOL_GATEWAY_USER_TOKEN", "launch-override")
	provider.SetMultiplexActive(true)
	t.Cleanup(func() { provider.SetMultiplexActive(false) })
	reply := wsLegacyCall(t, context.Background(), wsLegacyActor(bee), "connectors.list", map[string]any{"owner": map[string]any{"type": "account"}})
	result, _ := reply["result"].(map[string]any)
	if available, _ := result["available"].(bool); !available {
		t.Fatalf("connectors page of a served profile with no identity answered %v, want it available for a guest", reply)
	}
}
