package gateway

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Writes a config.yaml body under dir
func writeRoomConfig(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRoomLinkEndpointReadsSupportedConfig(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".icebath")
	writeRoomConfig(t, dir, "gateway:\n  room_link_url: https://configured.example.test/icebath\n")
	t.Setenv("ICEBATH_HOME", dir)
	endpoint, err := localRoomLinkEndpoint(context.Background(), nil)
	want := jsonutil.ObjectOf("available", true, "url", "https://configured.example.test/icebath", "transport_security", "tls")
	if err != nil || !anyutil.Equal(endpoint, want) {
		t.Fatalf("endpoint %v %v", endpoint, err)
	}
}

func TestNamedProfileInheritsGatewayRoomLinkEndpoint(t *testing.T) {
	root := filepath.Join(t.TempDir(), "icebath")
	profile := filepath.Join(root, "profiles", "reviewer")
	writeRoomConfig(t, root, "gateway:\n  room_link_url: https://gateway.example.test/icebath\n")
	writeRoomConfig(t, profile, "gateway: {}\n")
	t.Setenv("ICEBATH_HOME", root)
	endpoint, err := localRoomLinkEndpoint(home.WithOverride(context.Background(), profile), nil)
	want := jsonutil.ObjectOf("available", true, "url", "https://gateway.example.test/icebath", "transport_security", "tls")
	if err != nil || !anyutil.Equal(endpoint, want) {
		t.Fatalf("endpoint %v %v", endpoint, err)
	}
}

func TestNamedProfileRoomLinkOverrideWinsOverGatewayRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "icebath")
	profile := filepath.Join(root, "profiles", "reviewer")
	writeRoomConfig(t, root, "gateway:\n  room_link_url: https://gateway.example.test/icebath\n")
	writeRoomConfig(t, profile, "gateway:\n  room_link_url: https://profile.example.test/icebath\n")
	t.Setenv("ICEBATH_HOME", root)
	endpoint, err := localRoomLinkEndpoint(home.WithOverride(context.Background(), profile), nil)
	if url, _ := endpoint.Get("url"); err != nil || url != "https://profile.example.test/icebath" {
		t.Fatalf("endpoint %v %v", endpoint, err)
	}
}

func TestCatalogDigestIsCanonicalAndTamperEvident(t *testing.T) {
	value, err := CatalogMapping(context.Background(), CatalogOptions{TargetProfile: "default", InstallationID: "install-peer",
		ProtocolVersions: []any{int64(2)}, LinkModes: []string{"direct", "pull"}, PersistentProcess: true})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := GatewayRoomCatalogFromMapping(value)
	if err != nil || !catalog.Text || catalog.Attachments {
		t.Fatalf("catalog %+v %v", catalog, err)
	}
	value.Set("attachments", true)
	_, err = GatewayRoomCatalogFromMapping(value)
	if _, isPeer := err.(*HostedRoomPeerError); !isPeer || !strings.Contains(err.Error(), "catalog_digest") {
		t.Fatalf("tampered %v", err)
	}
}

func TestLocalCatalogAdvertisesAPersistentProcess(t *testing.T) {
	catalog, err := LocalCatalogMapping(context.Background(), CatalogOptions{TargetProfile: "default", InstallationID: "install-local"})
	if err != nil {
		t.Fatal(err)
	}
	persistent, _ := catalog.Get("persistent_process")
	links, _ := catalog.Get("link_modes")
	if persistent != true || !anyutil.Equal(links, []any{"direct"}) {
		t.Fatalf("persistent %v links %v", persistent, links)
	}
}

func TestSelfAdvertisedEndpointIsExplicitAndValidated(t *testing.T) {
	for _, tc := range []struct {
		configured       string
		available        bool
		reason, security string
	}{
		{"", false, "not_configured", ""},
		{"http://peer.example.test:8000", false, "invalid_configuration", ""},
		{"http://127.0.0.1:8000", true, "", "loopback"},
		{"https://peer.example.test", true, "", "tls"},
	} {
		dir := t.TempDir()
		body := "gateway: {}\n"
		if tc.configured != "" {
			body = "gateway:\n  room_link_url: " + tc.configured + "\n"
		}
		writeRoomConfig(t, dir, body)
		t.Setenv("ICEBATH_HOME", dir)
		catalog, err := LocalCatalogMapping(context.Background(), CatalogOptions{TargetProfile: "default", InstallationID: "install-peer"})
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := catalog.Get("endpoint")
		endpoint := raw.(*jsonutil.Object)
		if available, _ := endpoint.Get("available"); available != tc.available {
			t.Fatalf("%v available %v", tc.configured, available)
		}
		if tc.reason != "" {
			if !anyutil.Equal(endpoint, jsonutil.ObjectOf("available", false, "reason", tc.reason)) {
				t.Fatalf("%v endpoint %v", tc.configured, endpoint)
			}
		} else if security, _ := endpoint.Get("transport_security"); security != tc.security {
			t.Fatalf("%v security %v", tc.configured, security)
		}
	}
}
