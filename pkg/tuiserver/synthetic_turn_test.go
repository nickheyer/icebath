package tuiserver

import (
	"os"
	"testing"
)

func TestSynthSeamDeadWhenEnvUnset(t *testing.T) {
	t.Setenv("ICEBATH_ISO_CERTIFY_SYNTH_TURN", "")
	os.Unsetenv("ICEBATH_ISO_CERTIFY_SYNTH_TURN")
	if synthTurnArmed() {
		t.Fatal("seam armed with the env unset")
	}
	if agent := maybeBuildSyntheticAgent("sid", nil); agent != nil {
		t.Fatalf("seam built %v", agent)
	}
}
