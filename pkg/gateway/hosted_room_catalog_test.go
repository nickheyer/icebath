package gateway

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
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

func TestRoomLinkEndpointReadsSupportedConfigWithEnvOverride(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".icebath")
	writeRoomConfig(t, dir, "gateway:\n  room_link_url: https://configured.example.test/hermes\n")
	t.Setenv("ICEBATH_HOME", dir)
	t.Setenv("ICEBATH_ROOM_LINK_URL", "")
	os.Unsetenv("ICEBATH_ROOM_LINK_URL")
	endpoint, err := localRoomLinkEndpoint(context.Background(), nil)
	want := pyjson.ObjectOf("available", true, "url", "https://configured.example.test/hermes", "transport_security", "tls")
	if err != nil || !pyjson.Equal(endpoint, want) {
		t.Fatalf("endpoint %v %v", endpoint, err)
	}
	t.Setenv("ICEBATH_ROOM_LINK_URL", "https://override.example.test/hermes")
	endpoint, err = localRoomLinkEndpoint(context.Background(), nil)
	if url, _ := endpoint.Get("url"); err != nil || url != "https://override.example.test/hermes" {
		t.Fatalf("override %v %v", endpoint, err)
	}
}

func TestNamedProfileInheritsGatewayRoomLinkEndpoint(t *testing.T) {
	root := filepath.Join(t.TempDir(), "hermes")
	profile := filepath.Join(root, "profiles", "reviewer")
	writeRoomConfig(t, root, "gateway:\n  room_link_url: https://gateway.example.test/hermes\n")
	writeRoomConfig(t, profile, "gateway: {}\n")
	t.Setenv("ICEBATH_HOME", root)
	t.Setenv("ICEBATH_ROOM_LINK_URL", "")
	os.Unsetenv("ICEBATH_ROOM_LINK_URL")
	endpoint, err := localRoomLinkEndpoint(home.WithOverride(context.Background(), profile), nil)
	want := pyjson.ObjectOf("available", true, "url", "https://gateway.example.test/hermes", "transport_security", "tls")
	if err != nil || !pyjson.Equal(endpoint, want) {
		t.Fatalf("endpoint %v %v", endpoint, err)
	}
}

func TestNamedProfileRoomLinkOverrideWinsOverGatewayRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "hermes")
	profile := filepath.Join(root, "profiles", "reviewer")
	writeRoomConfig(t, root, "gateway:\n  room_link_url: https://gateway.example.test/hermes\n")
	writeRoomConfig(t, profile, "gateway:\n  room_link_url: https://profile.example.test/hermes\n")
	t.Setenv("ICEBATH_HOME", root)
	t.Setenv("ICEBATH_ROOM_LINK_URL", "")
	os.Unsetenv("ICEBATH_ROOM_LINK_URL")
	endpoint, err := localRoomLinkEndpoint(home.WithOverride(context.Background(), profile), nil)
	if url, _ := endpoint.Get("url"); err != nil || url != "https://profile.example.test/hermes" {
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

func TestLocalCatalogIsHonestForAppManagedProcess(t *testing.T) {
	t.Setenv("ICEBATH_DESKTOP", "1")
	catalog, err := LocalCatalogMapping(context.Background(), CatalogOptions{TargetProfile: "default", InstallationID: "install-desktop"})
	if err != nil {
		t.Fatal(err)
	}
	persistent, _ := catalog.Get("persistent_process")
	links, _ := catalog.Get("link_modes")
	if persistent != false || !pyjson.Equal(links, []any{"direct"}) {
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
		if tc.configured == "" {
			t.Setenv("ICEBATH_ROOM_LINK_URL", "")
			os.Unsetenv("ICEBATH_ROOM_LINK_URL")
		} else {
			t.Setenv("ICEBATH_ROOM_LINK_URL", tc.configured)
		}
		catalog, err := LocalCatalogMapping(context.Background(), CatalogOptions{TargetProfile: "default", InstallationID: "install-peer"})
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := catalog.Get("endpoint")
		endpoint := raw.(*pyjson.Object)
		if available, _ := endpoint.Get("available"); available != tc.available {
			t.Fatalf("%v available %v", tc.configured, available)
		}
		if tc.reason != "" {
			if !pyjson.Equal(endpoint, pyjson.ObjectOf("available", false, "reason", tc.reason)) {
				t.Fatalf("%v endpoint %v", tc.configured, endpoint)
			}
		} else if security, _ := endpoint.Get("transport_security"); security != tc.security {
			t.Fatalf("%v security %v", tc.configured, security)
		}
	}
}
