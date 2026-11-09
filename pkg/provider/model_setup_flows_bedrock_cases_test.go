package provider

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"
)

func TestRegionGeoPrefixKnownGeographies(t *testing.T) {
	for region, want := range map[string]string{"us-east-1": "us.", "eu-central-2": "eu.", "ap-southeast-1": "ap.", "ca-central-1": "ca.",
		"sa-east-1": "sa.", "me-south-1": "me.", "af-south-1": "af."} {
		if got := BedrockRegionGeoPrefix(region); got != want {
			t.Fatalf("%s -> %q want %q", region, got, want)
		}
	}
}

func TestRegionGeoPrefixUnknownRegionIsEmpty(t *testing.T) {
	if BedrockRegionGeoPrefix("") != "" || BedrockRegionGeoPrefix("moon-base-1") != "" {
		t.Fatal("unknown regions must map to the empty geo prefix")
	}
}

func TestRoutableFromRegionUsProfileNotOfferedInEu(t *testing.T) {
	if BedrockModelRoutableFromRegion("us.anthropic.claude-sonnet-4-6", "eu-central-2") {
		t.Fatal("a us. profile routed from eu-central-2")
	}
}

func TestBedrockFlowSeesPoolKeyWhenNoEnv(t *testing.T) {
	h := newTestHome(t)
	t.Setenv("AWS_BEARER_TOKEN_BEDROCK", "")
	os.Unsetenv("AWS_BEARER_TOKEN_BEDROCK")
	r, out := newFlowRun(t, h)
	if _, err := r.store().WriteCredentialPool("bedrock", []map[string]any{{"id": "pool-1", "label": "pool", "auth_type": "api_key", "priority": 0,
		"source": "manual", "access_token": "pool-secret"}}, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	swapFlowSeam(t, &flowGetEnvValueFn, func(context.Context, string, string) (string, bool, error) { return "", false, nil })
	swapFlowSeam(t, &flowRawInputFn, func(io.Writer, string) (string, error) { return "k", nil })
	swapFlowSeam(t, &pickModelOrPromptFn, func(PoolEnv, io.Writer, []string, string, ModelSelectionPrompt) (string, bool) { return "", false })
	if err := modelFlowBedrockAPIKey(r, "us-east-1"); err != nil {
		t.Fatal(err)
	}
	head := out.String()
	if len(head) > 200 {
		head = head[:200]
	}
	if !strings.Contains(head, "pool-secret") && !strings.Contains(head, "pool-sec") {
		t.Fatalf("output %q", out.String())
	}
}
