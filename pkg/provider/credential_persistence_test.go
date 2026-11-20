package provider

import "testing"

func TestIsSecretPayloadKeyExport(t *testing.T) {
	if !IsSecretPayloadKey("api_key") || IsSecretPayloadKey("") || IsSecretPayloadKey("base_url") {
		t.Fatal("secret payload key classification")
	}
}
