package provider

import (
	"strings"
	"testing"
)

func TestModelSearchTextKeepsOrdinaryIDs(t *testing.T) {
	if ModelSearchText("kimi-k2.6") != "kimi-k2.6" || ModelSearchText("glm-5.2") != "glm-5.2" || ModelSearchText("  ") != "  " {
		t.Fatal("ordinary ids pass through")
	}
}

func TestModelSearchTextSurfacesAliases(t *testing.T) {
	if !strings.Contains(ModelSearchText("k3"), "kimi") {
		t.Fatalf("k3 is findable by kimi: %q", ModelSearchText("k3"))
	}
	hay := ModelSearchText("x-preview-f-free")
	for _, q := range []string{"ox", "ox-alpha"} {
		if !strings.Contains(hay, q) {
			t.Fatalf("%s must surface the preview slug: %q", q, hay)
		}
	}
	if ModelSearchText("K3") != "K3 kimi-k3 kimi" {
		t.Fatalf("the wire id keeps its casing: %q", ModelSearchText("K3"))
	}
}

func TestModelAliasCanonical(t *testing.T) {
	if ModelAliasCanonical("k3") != "kimi-k3" || ModelAliasCanonical("K3") != "kimi-k3" || ModelAliasCanonical(" Kimi-K3 ") != "kimi-k3" {
		t.Fatal("bare k3 folds to the public slug")
	}
}
