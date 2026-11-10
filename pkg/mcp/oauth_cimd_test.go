package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Published CIMD document of the upstream site
func cimdDocument(t *testing.T) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(pkgDir, "..", "..", "references", "projects", "hermes-agent", "website", "static", "oauth", "client-metadata.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

// Swaps the pinned port range for a private one and clears OAuth state under an isolated home
func privateCIMDPorts(t *testing.T) []int {
	t.Helper()
	oauthHome(t)
	prev := cimdPorts
	cimdPorts = []int{28890, 28891, 28892}
	t.Cleanup(func() { cimdPorts = prev })
	return cimdPorts
}

// Reports whether a port is in a list
func portIn(port int, ports []int) bool {
	for _, p := range ports {
		if p == port {
			return true
		}
	}
	return false
}

// Strings of a JSON array member
func docStrings(v any) []string {
	var out []string
	for _, s := range v.([]any) {
		out = append(out, s.(string))
	}
	return out
}

func TestCIMDDocumentClientIDIsTheURLWeSend(t *testing.T) {
	if got := cimdDocument(t)["client_id"]; got != cimdClientMetadataURL {
		t.Fatalf("client_id %v", got)
	}
}

func TestCIMDDocumentDeclaresEveryCallbackWeCanBuild(t *testing.T) {
	oauthHome(t)
	declared := strings.Join(docStrings(cimdDocument(t)["redirect_uris"]), "\n") + "\n"
	for host := range cimdRedirectHosts {
		for _, port := range cimdPorts {
			md, err := buildClientMetadata(map[string]any{"redirect_host": host, "_resolved_port": port})
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(declared, md.RedirectURIs[0]+"\n") {
				t.Fatalf("%s is not registered in the document", md.RedirectURIs[0])
			}
		}
	}
}

func TestCIMDDocumentURLPassesTheSDKValidator(t *testing.T) {
	if !isValidClientMetadataURL(cimdClientMetadataURL) {
		t.Fatal("document URL rejected")
	}
}

func TestCIMDDocumentAdvertisesAPublicNativeClient(t *testing.T) {
	doc := cimdDocument(t)
	grants := strings.Join(docStrings(doc["grant_types"]), " ")
	if doc["application_type"] != "native" || doc["token_endpoint_auth_method"] != "none" || !strings.Contains(grants, "authorization_code") ||
		!strings.Contains(grants, "refresh_token") {
		t.Fatalf("document %v", doc)
	}
}

func TestCIMDDocumentCarriesNoSharedSecret(t *testing.T) {
	doc := cimdDocument(t)
	if _, ok := doc["client_secret"]; ok {
		t.Fatal("client_secret present")
	}
	if _, ok := doc["client_secret_expires_at"]; ok {
		t.Fatal("client_secret_expires_at present")
	}
}

func TestCIMDDefaultDocumentURLIsAValidClientIdentifier(t *testing.T) {
	if !isValidCIMDURL(cimdClientMetadataURL) {
		t.Fatal("default URL rejected")
	}
}

func TestCIMDClientIdentifierURLRequirementsAreEnforced(t *testing.T) {
	for _, raw := range []string{"http://example.com/cimd.json", "https://example.com/", "https://example.com/cimd.json#frag",
		"https://user:pw@example.com/cimd.json", "https://example.com/./cimd.json", "https://example.com/../cimd.json"} {
		if isValidCIMDURL(raw) {
			t.Fatalf("%s accepted", raw)
		}
	}
}

func TestCIMDGeneratedRedirectURIIsRegisteredInTheDocument(t *testing.T) {
	oauthHome(t)
	cfg := map[string]any{}
	if _, err := configureCallbackPort(context.Background(), cfg, NewTokenStorage(context.Background(), "srv", "")); err != nil {
		t.Fatal(err)
	}
	md, err := buildClientMetadata(cfg)
	if err != nil {
		t.Fatal(err)
	}
	port, _ := cfg["_resolved_port"].(int)
	declared := strings.Join(docStrings(cimdDocument(t)["redirect_uris"]), "\n") + "\n"
	if cfg["_cimd_url"] != cimdClientMetadataURL || !portIn(port, cimdPorts) || !strings.Contains(declared, md.RedirectURIs[0]+"\n") {
		t.Fatalf("cfg %v redirect %v", cfg, md.RedirectURIs)
	}
}

func TestCIMDEligibleFlowGetsAPinnedPort(t *testing.T) {
	ports := privateCIMDPorts(t)
	url, port, ok := maybeUseCIMD(context.Background(), map[string]any{}, NewTokenStorage(context.Background(), "srv", ""))
	if !ok || url != cimdClientMetadataURL || !portIn(port, ports) {
		t.Fatalf("%q %d %v", url, port, ok)
	}
}

func TestCIMDPinnedPortIsHeldUntilTheCallbackAdoptsIt(t *testing.T) {
	privateCIMDPorts(t)
	_, port, ok := maybeUseCIMD(context.Background(), map[string]any{}, NewTokenStorage(context.Background(), "srv", ""))
	if !ok || !isReserved(port) {
		t.Fatalf("port %d ok %v reserved %v", port, ok, isReserved(port))
	}
	if ln, err := netListen4(port); err == nil {
		ln.Close()
		t.Fatal("pinned port was stolen")
	}
}

func TestCIMDPinnedSocketSurvivesTheEphemeralEvictionCap(t *testing.T) {
	privateCIMDPorts(t)
	_, pinned, ok := maybeUseCIMD(context.Background(), map[string]any{}, NewTokenStorage(context.Background(), "srv", ""))
	if !ok {
		t.Fatal("no pinned port")
	}
	for i := 0; i < maxReservedSockets+4; i++ {
		if _, err := reserveCallbackPort(); err != nil {
			t.Fatal(err)
		}
	}
	if !isReserved(pinned) {
		t.Fatal("pinned socket evicted")
	}
	if ln, err := netListen4(pinned); err == nil {
		ln.Close()
		t.Fatal("pinned port was stolen after eviction")
	}
}

func TestCIMDConcurrentServersGetDifferentPinnedPorts(t *testing.T) {
	ports := privateCIMDPorts(t)
	got := map[int]bool{}
	for i := range ports {
		_, port, ok := maybeUseCIMD(context.Background(), map[string]any{}, NewTokenStorage(context.Background(), "srv-"+string(rune('0'+i)), ""))
		if !ok {
			t.Fatal("no pinned port")
		}
		got[port] = true
	}
	for _, p := range ports {
		if !got[p] {
			t.Fatalf("ports %v", got)
		}
	}
}

func TestCIMDOccupiedPortMovesToTheNextInTheRange(t *testing.T) {
	ports := privateCIMDPorts(t)
	squatter, err := netListen4(ports[0])
	if err != nil {
		t.Fatalf("could not occupy port %d: %v", ports[0], err)
	}
	_, port, ok := maybeUseCIMD(context.Background(), map[string]any{}, NewTokenStorage(context.Background(), "srv", ""))
	squatter.Close()
	if !ok || !portIn(port, ports[1:]) {
		t.Fatalf("port %d ok %v", port, ok)
	}
}

func TestCIMDSelfHostedDocumentURLOverridesTheDefault(t *testing.T) {
	privateCIMDPorts(t)
	url, _, ok := maybeUseCIMD(context.Background(), map[string]any{"client_metadata_url": "https://example.com/my-cimd.json"}, NewTokenStorage(context.Background(), "srv", ""))
	if !ok || url != "https://example.com/my-cimd.json" {
		t.Fatalf("%q %v", url, ok)
	}
}

func TestCIMDConfigThatConflictsWithTheDocumentFallsBackToDCR(t *testing.T) {
	for _, cfg := range []map[string]any{{"cimd": false}, {"client_id": "preregistered"}, {"client_secret": "shh"},
		{"client_name": "Claude Code"}, {"token_endpoint_auth_method": "client_secret_post"}, {"redirect_port": 49399},
		{"redirect_uri": "https://proxy.example/callback"}, {"redirect_host": "example.test"},
		{"client_metadata_url": "http://insecure.example/cimd.json"}, {"client_metadata_url": "https://example.com/"}} {
		privateCIMDPorts(t)
		if url, port, ok := maybeUseCIMD(context.Background(), cfg, NewTokenStorage(context.Background(), "srv", "")); ok {
			t.Fatalf("%v gave %q %d", cfg, url, port)
		}
	}
}

func TestCIMDDashboardFlowFallsBackToDCR(t *testing.T) {
	privateCIMDPorts(t)
	flow := &DashboardOAuthFlow{RedirectURI: "https://agent.example/api/mcp/oauth/callback/srv"}
	cfg := map[string]any{}
	if _, err := configureCallbackPort(withDashboardOAuthFlow(context.Background(), flow), cfg, NewTokenStorage(context.Background(), "srv", "")); err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg["_cimd_url"]; ok || cfg["redirect_uri"] != flow.RedirectURI {
		t.Fatalf("cfg %v", cfg)
	}
}

func TestCIMDExistingRegistrationFallsBackToDCR(t *testing.T) {
	privateCIMDPorts(t)
	s := NewTokenStorage(context.Background(), "srv", "")
	writeTokenFile(t, s.HomeDir(), "srv.client.json", `{"client_id": "dcr-issued"}`)
	if _, _, ok := maybeUseCIMD(context.Background(), map[string]any{}, s); ok {
		t.Fatal("existing registration still got CIMD")
	}
}

func TestCIMDExhaustedPortRangeFallsBackToDCR(t *testing.T) {
	ports := privateCIMDPorts(t)
	for _, p := range ports {
		ln, err := netListen4(p)
		if err != nil {
			t.Fatalf("could not occupy port %d: %v", p, err)
		}
		defer ln.Close()
	}
	cfg := map[string]any{}
	port, err := configureCallbackPort(context.Background(), cfg, NewTokenStorage(context.Background(), "srv", ""))
	if _, ok := cfg["_cimd_url"]; err != nil || ok || portIn(port, ports) || cfg["_resolved_port"] != port {
		t.Fatalf("port %d cfg %v err %v", port, cfg, err)
	}
}

func TestCIMDMoreServersThanPinnedPortsAllGetCIMD(t *testing.T) {
	ports := privateCIMDPorts(t)
	for i := 0; i < len(ports)+3; i++ {
		url, _, ok := maybeUseCIMD(context.Background(), map[string]any{}, NewTokenStorage(context.Background(), "srv-"+string(rune('a'+i)), ""))
		if !ok || url != cimdClientMetadataURL {
			t.Fatalf("server %d gave %q %v", i, url, ok)
		}
	}
}

// Caches authorization server metadata whose CIMD support flag is supports
func cacheServerMetadata(t *testing.T, s *TokenStorage, supports bool) {
	t.Helper()
	md, err := validateOAuthMetadata(map[string]any{"issuer": "https://idp.example.com", "authorization_endpoint": "https://idp.example.com/authorize",
		"token_endpoint": "https://idp.example.com/token", "response_types_supported": []any{"code"},
		"client_id_metadata_document_supported": supports}, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveOAuthMetadata(context.Background(), md); err != nil {
		t.Fatal(err)
	}
}

func TestCIMDCachedMetadataDecidesWhetherToPin(t *testing.T) {
	for _, supports := range []bool{true, false} {
		ports := privateCIMDPorts(t)
		s := NewTokenStorage(context.Background(), "srv", "")
		cacheServerMetadata(t, s, supports)
		cfg := map[string]any{}
		port, err := configureCallbackPort(context.Background(), cfg, s)
		_, hasCIMD := cfg["_cimd_url"]
		if err != nil || hasCIMD != supports || portIn(port, ports) != supports {
			t.Fatalf("supports=%v port %d cfg %v err %v", supports, port, cfg, err)
		}
	}
}

func TestCIMDUnknownServerStillGetsADocument(t *testing.T) {
	privateCIMDPorts(t)
	cfg := map[string]any{}
	if _, err := configureCallbackPort(context.Background(), cfg, NewTokenStorage(context.Background(), "srv", "")); err != nil || cfg["_cimd_url"] != cimdClientMetadataURL {
		t.Fatalf("cfg %v err %v", cfg, err)
	}
}

func TestCIMDCachedPinnedPortIsNotHandedToASiblingServer(t *testing.T) {
	ports := privateCIMDPorts(t)
	settled := NewTokenStorage(context.Background(), "settled", "")
	writeTokenFile(t, settled.HomeDir(), "settled.client.json", map[string]any{"client_id": cimdClientMetadataURL,
		"redirect_uris": []any{"http://127.0.0.1:28890/callback"}})
	restored, err := configureCallbackPort(context.Background(), map[string]any{}, settled)
	if err != nil {
		t.Fatal(err)
	}
	_, fresh, ok := maybeUseCIMD(context.Background(), map[string]any{}, NewTokenStorage(context.Background(), "fresh", ""))
	if restored != ports[0] || !ok || fresh == ports[0] {
		t.Fatalf("restored %d fresh %d ok %v", restored, fresh, ok)
	}
}

func TestCIMDBuildOAuthAuthForwardsTheDocumentURL(t *testing.T) {
	privateCIMDPorts(t)
	setConsole(t, true)
	p, err := BuildOAuthAuth(context.Background(), "srv", "https://mcp.example.com/mcp", map[string]any{}, HTTPOptions{})
	if err != nil || p.clientMetadataURL != cimdClientMetadataURL {
		t.Fatalf("provider %+v %v", p, err)
	}
}

func TestCIMDBuildOAuthAuthOmitsTheURLWhenDisabled(t *testing.T) {
	oauthHome(t)
	setConsole(t, true)
	p, err := BuildOAuthAuth(context.Background(), "srv", "https://mcp.example.com/mcp", map[string]any{"cimd": false}, HTTPOptions{})
	if err != nil || p.clientMetadataURL != "" {
		t.Fatalf("provider %+v %v", p, err)
	}
}

func TestCIMDDCRFlowPassesNoDocumentURL(t *testing.T) {
	oauthHome(t)
	s := NewTokenStorage(context.Background(), "srv", "")
	dcr, err := buildProviderOptions(context.Background(), map[string]any{"cimd": false}, s, true)
	if err != nil || dcr.ClientMetadataURL != "" {
		t.Fatalf("dcr options %+v %v", dcr, err)
	}
	cimd, err := buildProviderOptions(context.Background(), map[string]any{"_cimd_url": "https://x.example/c.json", "cimd": false}, s, true)
	if err != nil || cimd.ClientMetadataURL != "https://x.example/c.json" {
		t.Fatalf("cimd options %+v %v", cimd, err)
	}
}

func TestCIMDSDKChoosesCIMDOnlyWhenTheServerAdvertisesIt(t *testing.T) {
	for _, c := range []struct {
		advertised *bool
		want       bool
	}{{boolPtr(true), true}, {boolPtr(false), false}, {nil, false}} {
		privateCIMDPorts(t)
		setConsole(t, true)
		p, err := BuildOAuthAuth(context.Background(), "srv", "https://mcp.example.com/mcp", map[string]any{}, HTTPOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if got := shouldUseClientMetadataURL(&OAuthMetadata{ClientIDMetadataDocumentSupported: c.advertised}, p.clientMetadataURL); got != c.want {
			t.Fatalf("advertised %v chose %v", c.advertised, got)
		}
	}
}

func TestCIMDRejectionMarkerOnDiskStopsTheDocumentAfterARestart(t *testing.T) {
	privateCIMDPorts(t)
	NewTokenStorage(context.Background(), "srv", "").MarkCIMDRejected(context.Background())
	s := NewTokenStorage(context.Background(), "srv", "")
	if !s.CIMDRejected() {
		t.Fatal("marker not visible to a fresh storage")
	}
	if _, _, ok := maybeUseCIMD(context.Background(), map[string]any{}, s); ok {
		t.Fatal("rejected document presented again")
	}
}

func TestCIMDReauthorizingClearsTheRejection(t *testing.T) {
	privateCIMDPorts(t)
	s := NewTokenStorage(context.Background(), "srv", "")
	s.MarkCIMDRejected(context.Background())
	if !s.CIMDRejected() {
		t.Fatal("rejection not recorded")
	}
	if err := s.Remove(false); err != nil || s.CIMDRejected() {
		t.Fatalf("rejection survived removal %v", err)
	}
	if _, _, ok := maybeUseCIMD(context.Background(), map[string]any{}, s); !ok {
		t.Fatal("document not retried after removal")
	}
}

func TestCIMDTimeoutNamesTheDocumentAndTheEscapeHatch(t *testing.T) {
	resetOAuthState(t)
	blockingStdin(t)
	port, err := reserveCallbackPort()
	if err != nil {
		t.Fatal(err)
	}
	_, err = makeCallbackWaiter(port, cimdClientMetadataURL, 10*time.Millisecond)(ForceInteractiveOAuth(context.Background()))
	var ne *OAuthNonInteractiveError
	if !errors.As(err, &ne) || !strings.Contains(err.Error(), cimdClientMetadataURL) || !strings.Contains(err.Error(), "cimd: false") {
		t.Fatalf("error %v", err)
	}
}
