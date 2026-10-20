package profiles_test

import (
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/protocol"

	_ "github.com/nickheyer/icebath/pkg/profiles/anthropic"
	_ "github.com/nickheyer/icebath/pkg/profiles/bedrock"
	_ "github.com/nickheyer/icebath/pkg/profiles/google"
	_ "github.com/nickheyer/icebath/pkg/profiles/mistral"
	_ "github.com/nickheyer/icebath/pkg/profiles/ollama"
	_ "github.com/nickheyer/icebath/pkg/profiles/openai"
	_ "github.com/nickheyer/icebath/pkg/profiles/openrouter"
)

var known = []string{"anthropic", "bedrock", "google", "mistral", "ollama", "openai", "openai-chat", "openrouter"}

// Every vendor registers a complete profile whose protocol and vendor are resolvable
func TestEveryVendorRegistersACompleteProfile(t *testing.T) {
	for _, name := range known {
		p, ok := profiles.Lookup(name)
		if !ok {
			t.Fatalf("%s is not registered", name)
		}
		if err := p.Validate(); err != nil {
			t.Fatal(err)
		}
		if _, ok := protocol.Lookup(p.Protocol); !ok {
			t.Fatalf("%s: protocol %s not registered", p.Name, p.Protocol)
		}
		if _, ok := profiles.LookupVendor(p.Vendor); !ok {
			t.Fatalf("%s: vendor %q not registered", p.Name, p.Vendor)
		}
		if _, err := p.Build("AK:SK"); err != nil {
			t.Fatalf("%s: %v", p.Name, err)
		}
		if (p.Auth == profiles.AuthBearer || p.Auth == profiles.AuthHeader || p.Auth == profiles.AuthQuery) && p.Key.Kind != profiles.SourceEnv {
			t.Fatalf("%s needs a key but names no environment variable", p.Name)
		}
	}
	registered := strings.Join(profiles.Vendors(), ",")
	for _, v := range []string{"anthropic", "bedrock", "google", "mistral", "ollama", "openai", "openrouter"} {
		if !strings.Contains(","+registered+",", ","+v+",") {
			t.Fatalf("vendor %s missing from %s", v, registered)
		}
	}
	if len(profiles.Defaults()) < len(known) {
		t.Fatalf("%d defaults", len(profiles.Defaults()))
	}
	if p, ok := profiles.Lookup("anthropic"); !ok || p.Protocol != protocol.AnthropicMessages {
		t.Fatalf("%+v %v", p, ok)
	}
}
