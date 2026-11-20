package agent

import "testing"

func TestBedrockPreserveDotsFlagBedrockProviderPreservesDots(t *testing.T) {
	if !(&AIAgent{provider: "bedrock", baseURL: ""}).anthropicPreserveDots() {
		t.Fatal("bedrock keeps inference profile dots")
	}
}

func TestBedrockPreserveDotsFlagNonBedrockAwsUrlDoesNotPreserveDots(t *testing.T) {
	if (&AIAgent{provider: "custom", baseURL: "https://s3.us-east-1.amazonaws.com"}).anthropicPreserveDots() {
		t.Fatal("an unrelated AWS endpoint must not preserve dots")
	}
}
