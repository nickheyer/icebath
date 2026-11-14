package provider

import "testing"

func TestValidateExportsRejectMalformedURLs(t *testing.T) {
	if err := ValidateBaseURL("http://host:notaport/v1"); err == nil {
		t.Fatal("malformed port accepted")
	}
	if err := ValidateBaseURL("acp://vendor"); err != nil {
		t.Fatalf("acp url rejected: %v", err)
	}
	for _, k := range auxProxyEnvKeys {
		t.Setenv(k, "")
	}
	if err := ValidateProxyEnvURLs(); err != nil {
		t.Fatalf("empty proxy env rejected: %v", err)
	}
	t.Setenv("HTTPS_PROXY", "http://proxy:bad")
	if err := ValidateProxyEnvURLs(); err == nil {
		t.Fatal("malformed proxy env accepted")
	}
}
