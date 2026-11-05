package memory

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

func TestBuildLearnPromptEmbedsTheUserRequestVerbatim(t *testing.T) {
	req := "the REST client in ~/projects/acme-sdk, focus on auth"
	if !strings.Contains(BuildLearnPrompt(req), req) {
		t.Fatal("request missing from the prompt")
	}
}

func TestBuildLearnPromptMatchesPythonText(t *testing.T) {
	digest := func(s string) string {
		sum := sha256.Sum256([]byte(s))
		return hex.EncodeToString(sum[:])
	}
	cases := map[string]string{
		"the REST client in ~/projects/acme-sdk, focus on auth": "7feb42b631b072407eefd303ed21a3c3f74c7cbff94ac8413dae1f0d68e3afc5",
		"  \n ": "5080b0f9cc898e79e42f534ad47399c6d887c1f3b692cb3a9bd5649bbb8f8361",
	}
	for req, want := range cases {
		if got := digest(BuildLearnPrompt(req)); got != want {
			t.Errorf("BuildLearnPrompt(%q) digest %s, want %s", req, got, want)
		}
	}
	if !strings.Contains(BuildLearnPrompt(""), "THE REQUEST:\n"+learnDefaultRequest+"\n\n") {
		t.Fatal("empty request must fall back to the conversation workflow")
	}
}
