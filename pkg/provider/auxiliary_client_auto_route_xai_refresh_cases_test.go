package provider

import "testing"

func TestAutoRouteOnXaiHostRefreshesXaiOauthUnlessApiKeyBackend(t *testing.T) {
	ctx := auxRecoveryHome(t, "")
	for _, c := range []struct{ effective, want string }{{"", "xai-oauth"}, {"auto", "xai-oauth"}, {"xai-oauth", "xai-oauth"}, {"xai", "auto"}} {
		if got := auxAuthRefreshProviderForRoute(ctx, "auto", "https://api.x.ai/v1", c.effective); got != c.want {
			t.Fatalf("effective %q: got %q want %q", c.effective, got, c.want)
		}
	}
}

func TestUnknownHostOnAutoRouteStaysAuto(t *testing.T) {
	if got := auxAuthRefreshProviderForRoute(auxRecoveryHome(t, ""), "auto", "https://unknown.example.com/v1", ""); got != "auto" {
		t.Fatalf("got %q", got)
	}
}
