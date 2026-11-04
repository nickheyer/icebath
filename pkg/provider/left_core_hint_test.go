package provider

import "testing"

func TestHostInstallHintOnlyNamesLeftCoreHosts(t *testing.T) {
	if got := HostInstallHint("https://api.openai.com/v1"); got != "" {
		t.Fatalf("hosted provider has no left core hint, got %q", got)
	}
	if got := HostInstallHint("https://[bad"); got != "" {
		t.Fatalf("unparsable url is at no host, got %q", got)
	}
	if ProviderLeftCore("openai") {
		t.Fatal("openai never left core")
	}
	if !leftCoreAtHost("https://api.githubcopilot.com/chat", "githubcopilot.com") {
		t.Fatal("subdomain is at the host")
	}
	if leftCoreAtHost("https://notgithubcopilot.com/", "githubcopilot.com") {
		t.Fatal("suffix without a dot is not the host")
	}
	for _, name := range copilotAPIProviders {
		if _, ok := leftCoreProviderFeature(name); !ok {
			t.Fatalf("%s belongs to the copilot feature", name)
		}
	}
	if _, ok := leftCoreProviderFeature("  "); ok {
		t.Fatal("blank name matches nothing")
	}
}
