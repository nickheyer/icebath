package media

import (
	"testing"

	"github.com/nickheyer/icebath/pkg/provider"
)

func TestFasterWhisperImportableHook(t *testing.T) {
	for _, present := range []bool{true, false} {
		var asked []string
		savedProbe, savedFlag := safeFindSpec, hasFasterWhisper
		safeFindSpec = func(module string) bool { asked = append(asked, module); return present }
		hasFasterWhisper = newFindSpecFlag("faster_whisper")
		if provider.FasterWhisperImportable() != present || provider.FasterWhisperImportable() != present {
			t.Fatalf("FasterWhisperImportable differs from the probe answer %v", present)
		}
		if len(asked) != 1 || asked[0] != "faster_whisper" {
			t.Fatalf("probe asked %v, want faster_whisper once", asked)
		}
		safeFindSpec, hasFasterWhisper = savedProbe, savedFlag
	}
}
