package agent

import (
	"testing"

	"github.com/nickheyer/icebath/pkg/provider"
)

func TestAckDetectorTransitionCueNamesOnlyAcpAgents(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Cleanup(provider.SnapshotProviderRegistryForTests())
	provider.RegisterProvider(provider.Profile{Name: "acme-acp", Aliases: []string{"acme"}, AuthType: provider.AuthTypeExternalProcess, ProcessCommand: "acmecli"})
	a := &AIAgent{}
	ack := func(text string) bool { return a.looksLikeCodexIntermediateAck("go", text, nil, false) }
	for text, want := range map[string]bool{
		"Launching it on acme-acp via acpx.":    true,
		"Relaunching the session on acme.":      true,
		"Launching it on acmecli via acpx.":     true,
		"Starting the job on monday.":           false,
		"Launching it on Nous.":                 false,
		"Relaunching the session on plain-key.": false,
	} {
		if got := ack(text); got != want {
			t.Fatalf("%q gave %v", text, got)
		}
	}
}
