package agent

import (
	"context"
	"testing"

	"github.com/nickheyer/icebath/pkg/provider"
)

func TestExternalProcessCanOptIntoStreaming(t *testing.T) {
	yes, no := true, false
	for i, tc := range []struct {
		auth                string
		capability          *bool
		url                 string
		disabled, consumers bool
		expected            bool
	}{
		{"external_process", &yes, "process://fixture", false, true, true},
		{"external_process", &yes, "process://fixture", false, false, true},
		{"external_process", nil, "process://fixture", false, true, false},
		{"external_process", nil, "https://proxy.invalid/v1", false, true, false},
		{"external_process", &no, "process://fixture", false, true, false},
		{"external_process", &yes, "process://fixture", true, true, false},
		{"external_process", &yes, "acp://fixture", false, true, false},
		{"external_process", &yes, "acp+tcp://127.0.0.1:9000", false, true, false},
		{"api_key", nil, "https://provider.invalid/v1", false, true, true},
		{"api_key", &no, "https://provider.invalid/v1", false, true, false},
	} {
		t.Run(tc.auth+" "+tc.url, func(t *testing.T) {
			caseHome(t)
			t.Cleanup(provider.SnapshotProviderRegistryForTests())
			provider.RegisterProvider(provider.Profile{Name: "streaming-process", AuthType: tc.auth, SupportsStreaming: tc.capability})
			a := spAgent(t)
			a.provider, a.baseURL = "streaming-process", tc.url
			if tc.disabled {
				a.DisableStreaming()
			}
			if tc.consumers {
				a.streamDeltaCallback = func(string) {}
			}
			if got := shouldStream(context.Background(), a); got != tc.expected {
				t.Fatalf("row %d stream %v want %v", i, got, tc.expected)
			}
		})
	}
}
